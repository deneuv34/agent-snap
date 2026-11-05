package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	retentionPageSize = 100
	gitdirFileLimit   = 4096
	namespaceLength   = 16
	valueBroken       = "broken"
)

var (
	// Git prints a warning for a ref it ignores, and the report counts it
	// from this text. A longer standard error fails the report instead of
	// being cut, because a cut text undercounts.
	retentionStderrBound = 4 << 20
	retentionRead        = readWithStderr

	warnBrokenRef  = regexp.MustCompile(`^warning: ignoring broken ref (refs/agent-snapshots/.+)$`)
	warnBrokenName = regexp.MustCompile(`^warning: ignoring ref with broken name (refs/agent-snapshots/.+)$`)

	// GIT_NO_LAZY_FETCH keeps cat-file from fetching a missing object into the
	// real object store in a partial clone. A Git older than 2.44 ignores it.
	retentionEnv = slices.Concat(lookupEnv, []string{"GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1"})

	errNotRegular = errors.New("not a regular file")
	errTooLarge   = errors.New("file reaches the read limit")
)

type LatestState string

const (
	LatestCommit        LatestState = "commit"
	LatestNotCommit     LatestState = "not_commit"
	LatestMissingObject LatestState = "missing_object"
	LatestSymbolic      LatestState = "symbolic"
	LatestBroken        LatestState = "broken"
	LatestMissing       LatestState = "missing"
)

type SnapshotStamp struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

type NamespaceReport struct {
	Namespace string         `json:"namespace"`
	Current   bool           `json:"current"`
	Worktree  string         `json:"worktree,omitempty"`
	Refs      int            `json:"refs"`
	Snapshots int            `json:"snapshots"`
	Broken    int            `json:"broken"`
	Oldest    *SnapshotStamp `json:"oldest,omitempty"`
	Newest    *SnapshotStamp `json:"newest,omitempty"`
	Latest    LatestState    `json:"latest"`
	Digest    string         `json:"digest"`
}

type RetentionTotals struct {
	Namespaces int `json:"namespaces"`
	Refs       int `json:"refs"`
	Other      int `json:"other"`
}

type RetentionReport struct {
	Offset     int               `json:"offset"`
	NextOffset int               `json:"next_offset,omitempty"`
	Totals     RetentionTotals   `json:"totals"`
	Namespaces []NamespaceReport `json:"namespaces"`
}

type digestLine struct{ ref, value, target string }

func (l digestLine) String() string {
	target := l.target
	if target == "" {
		target = "-"
	}
	return l.ref + " " + l.value + " " + target + "\n"
}

type namespaceRead struct {
	lines          []digestLine
	broken         int
	snapshots      int
	oldest, newest string
	latest         LatestState
	latestOID      string
}

func (n *namespaceRead) hasLatest() bool { return n.latest != "" }

func (s *Store) RetentionReport(ctx context.Context, offset int) (RetentionReport, error) {
	if offset < 0 || offset > MaxPageOffset {
		return RetentionReport{}, fmt.Errorf("invalid retention page: offset %d is outside 0 through %d", offset, MaxPageOffset)
	}
	mapped, err := s.mapWorktrees(ctx)
	if err != nil {
		return RetentionReport{}, err
	}
	reads, other, err := s.readNamespaces(ctx)
	if err != nil {
		return RetentionReport{}, err
	}
	if err := s.readSymbolicLatests(ctx, reads, mapped); err != nil {
		return RetentionReport{}, err
	}
	if err := s.typeLatests(ctx, reads); err != nil {
		return RetentionReport{}, err
	}
	return s.assemble(reads, mapped, other, offset), nil
}

func (s *Store) inNamespace(ns string) *Store {
	scoped := *s
	scoped.ns = ns
	return &scoped
}

func namespaceOf(gitDir string) string {
	sum := sha256.Sum256([]byte(gitDir))
	return hex.EncodeToString(sum[:8])
}

func isNamespace(name string) bool {
	if len(name) != namespaceLength {
		return false
	}
	for _, c := range []byte(name) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *Store) readNamespaces(ctx context.Context) (map[string]*namespaceRead, int, error) {
	args := []string{"for-each-ref", "--format=%(refname) %(objectname) %(symref)", refsPrefix}
	stdout, stderr, err := retentionRead(s.command(ctx, retentionEnv, args...), args)
	if err != nil {
		return nil, 0, err
	}
	reads := map[string]*namespaceRead{}
	other := 0
	add := func(ref string, line digestLine, broken bool) (*namespaceRead, string) {
		ns, rest := splitRef(ref)
		if ns == "" {
			other++
			return nil, ""
		}
		read := reads[ns]
		if read == nil {
			read = &namespaceRead{}
			reads[ns] = read
		}
		read.lines = append(read.lines, line)
		if broken {
			read.broken++
		}
		return read, rest
	}

	for _, text := range lines(stderr) {
		m := warnBrokenRef.FindStringSubmatch(text)
		if m == nil {
			m = warnBrokenName.FindStringSubmatch(text)
		}
		if m == nil {
			return nil, 0, fmt.Errorf("git %s: unexpected standard error %q", strings.Join(args, " "), text)
		}
		read, rest := add(m[1], digestLine{m[1], valueBroken, ""}, true)
		if read != nil && rest == latestSuffix {
			read.latest = LatestBroken
		}
	}
	for _, text := range lines(stdout) {
		fields := strings.SplitN(text, " ", 3)
		if len(fields) != 3 || !strings.HasPrefix(fields[0], refsPrefix) || !isObjectID(fields[1], len(s.zeroOID)) {
			return nil, 0, fmt.Errorf("git %s: unexpected line %q", strings.Join(args, " "), text)
		}
		ref, oid, target := fields[0], fields[1], fields[2]
		read, rest := add(ref, digestLine{ref, oid, target}, false)
		if read == nil {
			continue
		}
		switch id, isItem := strings.CutPrefix(rest, itemsSuffix); {
		case rest == latestSuffix && target != "":
			read.latest = LatestSymbolic
		case rest == latestSuffix:
			read.latestOID = oid
			read.latest = LatestCommit
		case isItem && ValidID(id):
			read.snapshots++
			if read.oldest == "" || id < read.oldest {
				read.oldest = id
			}
			if id > read.newest {
				read.newest = id
			}
		}
	}
	return reads, other, nil
}

func splitRef(ref string) (ns, rest string) {
	name, tail, ok := strings.Cut(strings.TrimPrefix(ref, refsPrefix), "/")
	if !ok || !isNamespace(name) {
		return "", ""
	}
	return name, "/" + tail
}

func lines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func isObjectID(text string, length int) bool {
	if len(text) != length {
		return false
	}
	_, err := hex.DecodeString(text)
	return err == nil && text == strings.ToLower(text)
}

// readSymbolicLatests finds the latest that for-each-ref did not list and did
// not name as broken, for every namespace it read and every namespace a
// worktree maps to. for-each-ref skips a symbolic ref whose target does not
// exist, without a warning.
func (s *Store) readSymbolicLatests(ctx context.Context, reads map[string]*namespaceRead, mapped map[string]string) error {
	pending := map[string]bool{}
	for ns, read := range reads {
		if !read.hasLatest() {
			pending[ns] = true
		}
	}
	for ns := range mapped {
		if read := reads[ns]; read == nil || !read.hasLatest() {
			pending[ns] = true
		}
	}
	names := make([]string, 0, len(pending))
	for ns := range pending {
		names = append(names, ns)
	}
	sort.Strings(names)
	for _, ns := range names {
		ref := s.inNamespace(ns).latestRef()
		args := []string{"symbolic-ref", "-q", ref}
		out, err := s.gitRaw(ctx, retentionEnv, args...)
		var line digestLine
		var state LatestState
		var exit *exec.ExitError
		switch {
		case err == nil && strings.TrimSpace(out) != "":
			line, state = digestLine{ref, s.zeroOID, strings.TrimSpace(out)}, LatestSymbolic
		case err == nil:
			return fmt.Errorf("git %s: printed no target", strings.Join(args, " "))
		case ctx.Err() != nil:
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			continue
		case errors.As(err, &exit) && exit.ExitCode() > 1:
			line, state = digestLine{ref, valueBroken, ""}, LatestBroken
		default:
			return err
		}
		read := reads[ns]
		if read == nil {
			read = &namespaceRead{}
			reads[ns] = read
		}
		read.lines = append(read.lines, line)
		read.latest = state
		if state == LatestBroken {
			read.broken++
		}
	}
	return nil
}

func (s *Store) typeLatests(ctx context.Context, reads map[string]*namespaceRead) error {
	var names []string
	for ns, read := range reads {
		if read.latest == LatestCommit {
			names = append(names, ns)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	var in strings.Builder
	for _, ns := range names {
		in.WriteString(reads[ns].latestOID + "\n")
	}
	out, err := s.gitStdin(ctx, retentionEnv, in.String(), "cat-file", "--batch-check")
	if err != nil {
		return err
	}
	got := lines(out)
	if len(got) != len(names) {
		return fmt.Errorf("git cat-file --batch-check: answered %d lines for %d objects", len(got), len(names))
	}
	for i, ns := range names {
		read := reads[ns]
		fields := strings.Fields(got[i])
		switch {
		case len(fields) < 2 || fields[0] != read.latestOID:
			return fmt.Errorf("git cat-file --batch-check: unexpected line %q", got[i])
		case fields[1] == "missing" && len(fields) == 2:
			read.latest = LatestMissingObject
		case fields[1] == "commit" && len(fields) == 3:
			read.latest = LatestCommit
		case len(fields) == 3:
			read.latest = LatestNotCommit
		default:
			return fmt.Errorf("git cat-file --batch-check: unexpected line %q", got[i])
		}
	}
	return nil
}

func (s *Store) assemble(reads map[string]*namespaceRead, mapped map[string]string, other, offset int) RetentionReport {
	names := make([]string, 0, len(reads))
	for ns, read := range reads {
		if len(read.lines) > 0 {
			names = append(names, ns)
		}
	}
	sort.Strings(names)
	report := RetentionReport{Offset: offset, Totals: RetentionTotals{Namespaces: len(names), Other: other}, Namespaces: []NamespaceReport{}}
	for i, ns := range names {
		read := reads[ns]
		report.Totals.Refs += len(read.lines)
		if i < offset || i >= offset+retentionPageSize {
			continue
		}
		report.Namespaces = append(report.Namespaces, s.namespaceReport(ns, read, mapped[ns]))
	}
	if end := offset + len(report.Namespaces); len(report.Namespaces) > 0 && end < len(names) {
		report.NextOffset = end
	}
	return report
}

func (s *Store) namespaceReport(ns string, read *namespaceRead, worktree string) NamespaceReport {
	slices.SortFunc(read.lines, func(a, b digestLine) int { return strings.Compare(a.ref, b.ref) })
	h := sha256.New()
	for _, line := range read.lines {
		io.WriteString(h, line.String())
	}
	entry := NamespaceReport{
		Namespace: ns,
		Current:   ns == s.ns,
		Worktree:  worktree,
		Refs:      len(read.lines),
		Snapshots: read.snapshots,
		Broken:    read.broken,
		Latest:    read.latest,
		Digest:    hex.EncodeToString(h.Sum(nil)),
	}
	if !read.hasLatest() {
		entry.Latest = LatestMissing
	}
	if read.snapshots > 0 {
		entry.Oldest, entry.Newest = &SnapshotStamp{read.oldest, idTime(read.oldest)}, &SnapshotStamp{read.newest, idTime(read.newest)}
	}
	return entry
}

// idTime reads the time an identifier names, in whole seconds. An identifier
// whose digits are no date has the zero time.
func idTime(id string) time.Time {
	t, err := time.Parse("20060102T150405.000000000Z", id[:strings.IndexByte(id, '-')])
	if err != nil {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Second)
}

func (s *Store) mapWorktrees(ctx context.Context) (map[string]string, error) {
	gitDir, err := s.git(ctx, retentionEnv, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve git dir: %w", err)
	}
	common, err := commonDir(gitDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read worktree entries: %w", err)
	}
	mapped := map[string]string{}
	listable := true
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(common, "worktrees", entry.Name())
		mapped[namespaceOf(filepath.ToSlash(dir))] = linkedWorktreePath(dir)
		listable = listable && gitdirFileIsSafe(filepath.Join(dir, "gitdir"))
	}
	mainNS := namespaceOf(filepath.ToSlash(common))
	mapped[mainNS] = ""
	if listable {
		if mapped[mainNS], err = s.mainWorktreePath(ctx); err != nil {
			return nil, err
		}
	}
	return mapped, nil
}

// commonDir finds the common Git directory without --path-format, which Git
// 2.31 added, and without --git-common-dir, whose relative output keeps a
// symbolic link that Git resolved before it hashed the Git directory.
func commonDir(gitDir string) (string, error) {
	content, err := readRegular(filepath.Join(gitDir, "commondir"), gitdirFileLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return gitDir, nil
	}
	if err != nil {
		return "", fmt.Errorf("read commondir: %w", err)
	}
	path := strings.TrimRight(string(content), "\r\n")
	if !filepath.IsAbs(path) {
		path = filepath.Join(gitDir, path)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve common git directory: %w", err)
	}
	return resolved, nil
}

// gitdirFileIsSafe reports whether git worktree list can read the entry's
// gitdir file without blocking. Git opens it with a blocking read, so a FIFO
// there stops the command for as long as the caller waits.
func gitdirFileIsSafe(path string) bool {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return err == nil && info.Mode().IsRegular()
}

func linkedWorktreePath(entryDir string) string {
	content, err := readRegular(filepath.Join(entryDir, "gitdir"), gitdirFileLimit)
	if err != nil {
		return ""
	}
	path := strings.TrimRight(string(content), "\r\n")
	if path == "" || strings.ContainsAny(path, "\n\x00") {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(entryDir, path)
	}
	return filepath.ToSlash(filepath.Dir(filepath.Clean(path)))
}

// mainWorktreePath is the first worktree line of git worktree list. A path
// with a line feed splits that line, so the record must continue with a HEAD
// line or a bare line, and any other record has no path.
func (s *Store) mainWorktreePath(ctx context.Context) (string, error) {
	out, err := s.gitRaw(ctx, retentionEnv, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	first, rest, _ := strings.Cut(out, "\n")
	second, _, _ := strings.Cut(rest, "\n")
	path, ok := strings.CutPrefix(first, "worktree ")
	if !ok || (!strings.HasPrefix(second, "HEAD ") && second != "bare") {
		return "", nil
	}
	return path, nil
}

// readRegular reads a regular file of fewer than limit bytes. It opens the
// file without blocking and checks the descriptor, so a FIFO swapped in
// after the check is refused and never waited on.
func readRegular(path string, limit int) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errNotRegular
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errNotRegular
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	if err != nil {
		return nil, err
	}
	if len(data) >= limit {
		return nil, errTooLarge
	}
	return data, nil
}

type stderrBound struct {
	buf   []byte
	limit int
	over  bool
}

func (b *stderrBound) Write(p []byte) (int, error) {
	n := len(p)
	if room := max(b.limit-len(b.buf), 0); n > room {
		b.over = true
		p = p[:room]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

// readWithStderr runs cmd and returns both streams. output keeps only the
// tail of standard error and returns it only when Git fails, which cannot
// feed a count. Standard error over retentionStderrBound is an error.
func readWithStderr(cmd *exec.Cmd, args []string) (stdout, stderr string, err error) {
	var out bytes.Buffer
	errs := &stderrBound{limit: retentionStderrBound}
	cmd.Stdout, cmd.Stderr = &out, errs
	if err := cmd.Run(); err != nil {
		return "", "", &gitError{args: args, err: err, stderr: strings.TrimSpace(string(errs.buf[:min(len(errs.buf), stderrLimit)]))}
	}
	if errs.over {
		return "", "", fmt.Errorf("git %s: standard error is longer than %d bytes", strings.Join(args, " "), errs.limit)
	}
	return out.String(), string(errs.buf), nil
}
