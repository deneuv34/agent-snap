package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	refsPrefix        = "refs/agent-snapshots/"
	itemsSuffix       = "/items/"
	latestSuffix      = "/latest"
	messagePrefix     = "agent-snap: "
	identityEmail     = "agent-snap@localhost"
	addAttempts       = 5
	changedFilesLimit = 1000
	originLimit       = 64
	gitStopGrace      = 500 * time.Millisecond
	refLockTimeout    = time.Second
	refDirAttempts    = 5
	refDirDelay       = 10 * time.Millisecond
	MaxPageOffset     = 1<<31 - 1
	pageSize          = 200
)

var (
	ErrInvalidID     = errors.New("invalid snapshot id")
	ErrInvalidReason = errors.New("invalid snapshot reason")
	ErrInvalidOrigin = errors.New("invalid snapshot origin")
	ErrNotFound      = errors.New("snapshot not found")
	errNoSnapshot    = errors.New("no snapshot item ref points at the commit")
	idPattern        = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[0-9a-f]{12}$`)
	originPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	originLine       = regexp.MustCompile(`^Agent-Snap-([A-Za-z0-9]+(?:-[A-Za-z0-9]+)*): ([!-~](?:[ -~]{0,253}[!-~])?)$`)
	lookupEnv        = []string{"GIT_NO_REPLACE_OBJECTS=1"}
)

type Origin struct {
	Source string `json:"source,omitempty"`
	Host   string `json:"host,omitempty"`
}

type Snapshot struct {
	ID        string    `json:"id"`
	Commit    string    `json:"commit"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	Origin
	ChangedFiles []string `json:"changed_files,omitempty"`
	Reused       bool     `json:"reused,omitempty"`
}

type Detail struct {
	ID        string    `json:"id"`
	Commit    string    `json:"commit"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	Origin
	Tree                  string   `json:"tree"`
	Parent                string   `json:"parent,omitempty"`
	ParentID              string   `json:"parent_id,omitempty"`
	ChangedFiles          []string `json:"changed_files"`
	ChangedFilesTruncated bool     `json:"changed_files_truncated,omitempty"`
}

// ValidID reports whether id has the shape Create mints. Get checks it before
// building a ref path from id, so an id can never name a ref outside this
// worktree's items namespace or smuggle a for-each-ref pattern.
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// ValidOrigin reports whether value may be stored as an origin source or host.
func ValidOrigin(value string) bool {
	return len(value) <= originLimit && originPattern.MatchString(value)
}

// ValidReason reports whether reason reads back unchanged. Git refuses a NUL
// in a commit message, and a reason whose last paragraph parses as an origin
// paragraph would be read as origin, so Create refuses both.
func ValidReason(reason string) bool {
	_, _, ok := splitOrigin(reason)
	return !ok && !strings.Contains(reason, "\x00")
}

func checkOrigin(origin Origin) error {
	for _, value := range []string{origin.Source, origin.Host} {
		if value != "" && !ValidOrigin(value) {
			return fmt.Errorf("%w: %q", ErrInvalidOrigin, value)
		}
	}
	return nil
}

func commitMessage(reason string, origin Origin) string {
	message := messagePrefix + reason
	if origin == (Origin{}) {
		return message
	}
	message += "\n"
	if origin.Source != "" {
		message += "\nAgent-Snap-Source: " + origin.Source
	}
	if origin.Host != "" {
		message += "\nAgent-Snap-Host: " + origin.Host
	}
	return message
}

func parseMessage(message string) (string, Origin) {
	reason, origin, _ := splitOrigin(strings.TrimPrefix(message, messagePrefix))
	return reason, origin
}

// splitOrigin separates a trailing origin paragraph from the reason. Blink
// parses the paragraph itself because Git's trailer parsing follows the
// user's trailer.* configuration. The line grammar is deliberately wider than
// an origin value and must never be widened again: a reason stored today that
// a wider reader took as origin would be cut short. Unknown keys are skipped,
// and Source or Host is reported only when its value is a valid origin value.
func splitOrigin(body string) (string, Origin, bool) {
	cut := strings.LastIndex(body, "\n\n")
	if cut < 0 {
		return body, Origin{}, false
	}
	var origin Origin
	seen := map[string]bool{}
	for _, line := range strings.Split(body[cut+2:], "\n") {
		m := originLine.FindStringSubmatch(line)
		if m == nil || seen[m[1]] {
			return body, Origin{}, false
		}
		seen[m[1]] = true
		if !ValidOrigin(m[2]) {
			continue
		}
		switch m[1] {
		case "Source":
			origin.Source = m[2]
		case "Host":
			origin.Host = m[2]
		}
	}
	return body[:cut], origin, true
}

type Store struct {
	repo    string
	ns      string
	zeroOID string
	now     func() time.Time
	pins    []string
}

func New(path string) (*Store, error) {
	out, err := revParse(path, "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("find git repository: %w", err)
	}
	repo := strings.TrimSuffix(strings.TrimSuffix(out, "\n"), "\r")

	format, err := revParse(repo, "--show-object-format")
	if err != nil {
		return nil, fmt.Errorf("resolve object format: %w", err)
	}
	zeroOID := strings.Repeat("0", 40)
	if strings.TrimSpace(format) == "sha256" {
		zeroOID = strings.Repeat("0", 64)
	}

	gitDir, err := revParse(repo, "--absolute-git-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve git dir: %w", err)
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(gitDir)))
	ns := hex.EncodeToString(sum[:8])

	// Every Store pins core.fsmonitor=, so no fsmonitor program decides which
	// files a read or a capture looks at.
	return &Store{repo: repo, ns: ns, zeroOID: zeroOID, now: time.Now, pins: []string{"-c", "core.fsmonitor="}}, nil
}

func revParse(dir, option string) (string, error) {
	cmd := exec.Command("git", "rev-parse", option)
	cmd.Dir = dir
	cmd.Env = sanitizedEnv()
	return output(cmd, []string{"rev-parse", option})
}

func (s *Store) itemsRef() string {
	return refsPrefix + s.ns + itemsSuffix
}

func (s *Store) latestRef() string {
	return refsPrefix + s.ns + latestSuffix
}

func (s *Store) Create(ctx context.Context, reason string, origin Origin) (Snapshot, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "manual"
	}
	if !ValidReason(reason) {
		return Snapshot{}, fmt.Errorf("%w: it contains a NUL byte or ends with an origin paragraph", ErrInvalidReason)
	}
	if err := checkOrigin(origin); err != nil {
		return Snapshot{}, err
	}

	tmpDir, cleanup, err := createScratchIndex(s.repo)
	if err != nil {
		return Snapshot{}, err
	}
	defer cleanup()
	op, err := s.withoutHooks(ctx, tmpDir)
	if err != nil {
		return Snapshot{}, err
	}
	env := scratchEnv(tmpDir)

	tree, head, err := op.captureTree(ctx, env)
	if err != nil {
		return Snapshot{}, err
	}
	return op.recordTree(ctx, env, tree, head, reason, origin)
}

// withoutHooks returns a copy of s whose Git commands start no repository
// hook, because a hook would receive the scratch index. Configured hooks are
// listed once, so one added during the operation can still run.
func (s *Store) withoutHooks(ctx context.Context, scratchDir string) (*Store, error) {
	hooks := filepath.Join(scratchDir, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		return nil, fmt.Errorf("create empty hooks directory: %w", err)
	}
	out, err := s.gitRaw(ctx, nil, "config", "-z", "--get-regexp", `^hook\..*\.(command|event)$`)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		out, err = "", nil
	}
	if err != nil {
		return nil, fmt.Errorf("list configured hooks: %w", err)
	}
	pins := slices.Concat(s.pins, []string{"-c", "core.hooksPath=" + hooks})
	seen := map[string]bool{}
	for _, record := range records(out) {
		key, _, _ := strings.Cut(record, "\n")
		name, ok := strings.CutPrefix(key, "hook.")
		dot := strings.LastIndexByte(name, '.')
		if !ok || dot < 0 {
			return nil, fmt.Errorf("list configured hooks: Git listed %q, which is not hook.<name>.<key>", key)
		}
		name = name[:dot]
		if strings.Contains(name, "=") {
			return nil, fmt.Errorf("list configured hooks: hook name %q contains '=', so it cannot be disabled on the Git command line", name)
		}
		if !seen[name] {
			seen[name] = true
			pins = append(pins, "-c", "hook."+name+".enabled=false")
		}
	}
	op := *s
	op.pins = pins
	return &op, nil
}

func (s *Store) recordTree(ctx context.Context, env []string, tree, head, reason string, origin Origin) (Snapshot, error) {
	for {
		if symref, err := s.isSymref(ctx, s.latestRef()); err != nil {
			return Snapshot{}, err
		} else if symref {
			return Snapshot{}, fmt.Errorf("snapshot ref %s is a symbolic ref; refusing to update it", s.latestRef())
		}

		latest, latestErr := s.git(ctx, env, "rev-parse", "--verify", s.latestRef())
		if latestErr == nil {
			latestTree, err := s.git(ctx, env, "rev-parse", latest+"^{tree}")
			if err != nil {
				return Snapshot{}, err
			}
			if tree == latestTree {
				snap, err := s.snapshotAt(ctx, latest)
				if err == nil {
					snap.Reused = true
					return snap, nil
				}
				if !errors.Is(err, errNoSnapshot) {
					return Snapshot{}, err
				}
			}
		}

		parent := head
		if latestErr == nil {
			parent = latest
		}

		args := []string{"commit-tree", tree}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		args = append(args, "-F", "-")
		commit, err := s.gitStdin(ctx, append(env, authorEnv()...), commitMessage(reason, origin), args...)
		if err != nil {
			return Snapshot{}, err
		}

		now := s.now().UTC()
		id := now.Format("20060102T150405.000000000Z") + "-" + commit[:12]
		latestHash := ""
		if latestErr == nil {
			latestHash = latest
		}
		committed, err := s.commitRefs(ctx, id, commit, latestHash, env)
		if err != nil {
			return Snapshot{}, err
		}
		if committed {
			changed, err := s.changedFiles(ctx, nil, parent, commit)
			if err != nil {
				return Snapshot{}, err
			}
			return Snapshot{ID: id, Commit: commit, Reason: reason, CreatedAt: now, Origin: origin, ChangedFiles: changed}, nil
		}
	}
}

// captureTree is the one definition of the captured state: the tree that
// git add -A -- . stages into the scratch index named in env, seeded from
// HEAD. head is empty on an unborn branch.
func (s *Store) captureTree(ctx context.Context, env []string) (tree, head string, err error) {
	head, headErr := s.git(ctx, env, "rev-parse", "--verify", "HEAD")
	if headErr != nil {
		head = ""
		if _, err := s.git(ctx, env, scratchIndexArgs("read-tree", "--empty")...); err != nil {
			return "", "", err
		}
	} else if _, err := s.git(ctx, env, scratchIndexArgs("read-tree", head)...); err != nil {
		return "", "", err
	}
	if err := s.addAll(ctx, env); err != nil {
		return "", "", err
	}
	tree, err = s.git(ctx, env, scratchIndexArgs("write-tree")...)
	if err != nil {
		return "", "", err
	}
	return tree, head, nil
}

// Under core.splitIndex=true, Git writes a shared index into the real Git
// directory whatever GIT_INDEX_FILE names, and deletes expired shared indexes
// there, including the one the real index still needs.
func scratchIndexArgs(args ...string) []string {
	return append([]string{"-c", "core.splitIndex=false"}, args...)
}

// addAll stages the working tree into the scratch index. Concurrent creates in
// one repository write the same loose objects, and Git for Windows can lose
// that race with "Permission denied" while another process finalizes the same
// object. Only the scratch index is touched, and a retry finds the finished
// object instead of writing it, so the object-write failure is retried with
// backoff. Every other failure returns at once.
func (s *Store) addAll(ctx context.Context, env []string) error {
	delay := 25 * time.Millisecond
	for attempt := 1; ; attempt++ {
		_, err := s.git(ctx, env, scratchIndexArgs("add", "-A", "--", ".")...)
		if err == nil {
			return nil
		}
		if attempt == addAttempts || !strings.Contains(stderrOf(err), "failed to insert into database") {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("git add -A -- .: %w", ctx.Err())
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// createScratchIndex allocates the private scratch directory that holds the
// temporary index for one snapshot. The directory is guaranteed to live
// outside the repository (so a snapshot can never capture it), is uniquely
// named per allocation, and is removed by the returned cleanup function.
// Create uses exactly this helper; the safety tests exercise it directly.
func createScratchIndex(repo string) (scratchDir string, cleanup func(), err error) {
	tmpRoot, err := scratchRootOutside(repo)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(tmpRoot, "agent-snap-index-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func scratchEnv(dir string) []string {
	return []string{
		"GIT_INDEX_FILE=" + filepath.Join(dir, "index"),
		"TMPDIR=" + dir,
		"TMP=" + dir,
		"TEMP=" + dir,
	}
}

// scratchRootOutside returns a temp directory candidate that is not inside
// the repository, so scratch files (index, git children temp files) can never
// be captured into a snapshot.
func scratchRootOutside(repo string) (string, error) {
	realRepo := repo
	if resolved, err := filepath.EvalSymlinks(repo); err == nil {
		realRepo = resolved
	}
	candidates := []string{os.TempDir()}
	if cacheDir, err := os.UserCacheDir(); err == nil {
		candidates = append(candidates, cacheDir)
	}
	candidates = append(candidates, "/tmp")
	for _, candidate := range candidates {
		realCandidate := candidate
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			realCandidate = resolved
		}
		rel, err := filepath.Rel(realRepo, realCandidate)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no scratch directory found outside repository %s (TMPDIR resolves inside the worktree)", repo)
}

// isSymref reports whether ref is a symbolic reference.
func (s *Store) isSymref(ctx context.Context, ref string) (bool, error) {
	err := s.command(ctx, nil, "symbolic-ref", "-q", ref).Run()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect ref %s: %w", ref, err)
}

// commitRefs atomically updates the item ref and moves latest using a
// compare-and-swap transaction. It returns false when another process moved
// latest concurrently (the caller retries). An empty expectedLatest is
// encoded as the object format's all-zero OID, meaning "latest must not
// exist yet". update-ref runs with --no-deref so a symbolic snapshot ref can
// never redirect the write onto a real branch; symbolic refs are rejected
// outright. A ref lock held past refLockTimeout fails the call and is never
// removed, because a stale lock and one that a live Git holds look the same.
// A ref directory that a concurrent pack-refs --prune removed is attempted at
// most refDirAttempts times, because Git prints the same message for a
// permission failure on that directory.
func (s *Store) commitRefs(ctx context.Context, id, commit, expectedLatest string, extraEnv []string) (bool, error) {
	itemsRef := s.itemsRef() + id
	if symref, err := s.isSymref(ctx, itemsRef); err != nil {
		return false, err
	} else if symref {
		return false, fmt.Errorf("snapshot ref %s is a symbolic ref; refusing to update it", itemsRef)
	}
	if symref, err := s.isSymref(ctx, s.latestRef()); err != nil {
		return false, err
	} else if symref {
		return false, fmt.Errorf("snapshot ref %s is a symbolic ref; refusing to update it", s.latestRef())
	}

	expected := expectedLatest
	if expected == "" {
		expected = s.zeroOID
	}
	var b strings.Builder
	b.WriteString("start\n")
	fmt.Fprintf(&b, "update %s %s\n", itemsRef, commit)
	fmt.Fprintf(&b, "update %s %s %s\n", s.latestRef(), commit, expected)
	b.WriteString("prepare\ncommit\n")

	delay := refDirDelay
	for attempt := 1; ; attempt++ {
		_, err := s.gitStdin(ctx, extraEnv, b.String(), "-c", fmt.Sprintf("core.filesRefLockTimeout=%d", refLockTimeout.Milliseconds()), "update-ref", "--no-deref", "--stdin")
		if err == nil {
			return true, nil
		}
		out := stderrOf(err)
		if strings.Contains(out, "but expected") || strings.Contains(out, "reference already exists") {
			return false, nil
		}
		dirUncreatable := false
		for _, ref := range []string{itemsRef, s.latestRef()} {
			if strings.Contains(out, "cannot lock ref '"+ref+"': Unable to create '") && strings.Contains(out, "': File exists") {
				return false, fmt.Errorf("snapshot ref lock %s.lock in the Git directory stayed held for %s; remove it only when no agent-snap or Git process is running in this repository", ref, refLockTimeout)
			}
			dirUncreatable = dirUncreatable || strings.Contains(out, "cannot lock ref '"+ref+"': unable to create directory for ")
		}
		if attempt == refDirAttempts || !dirUncreatable {
			return false, fmt.Errorf("update refs: %w", err)
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("update refs: %w", ctx.Err())
		case <-time.After(delay):
		}
		delay *= 2
	}
}

func (s *Store) List(ctx context.Context, limit int) ([]Snapshot, error) {
	if limit <= 0 {
		limit = 20
	}
	format := "%(refname:strip=4)%00%(objectname)%00%(contents)%00%(creatordate:iso-strict)%00"
	out, err := s.gitRaw(ctx, nil, "for-each-ref", "--format="+format, s.itemsRef())
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []Snapshot{}, nil
	}
	var items []Snapshot
	for _, record := range strings.Split(out, "\x00\n") {
		if record == "" {
			continue
		}
		fields := strings.Split(record, "\x00")
		if len(fields) > 0 && fields[len(fields)-1] == "" {
			fields = fields[:len(fields)-1]
		}
		if len(fields) != 4 || !ValidID(fields[0]) {
			continue
		}
		created, _ := time.Parse(time.RFC3339, fields[3])
		reason, origin := parseMessage(fields[2])
		items = append(items, Snapshot{ID: fields[0], Commit: fields[1], Reason: reason, CreatedAt: created, Origin: origin})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// Get resolves id inside this worktree's namespace only, and returns it only
// when checkItem accepts the item ref.
func (s *Store) Get(ctx context.Context, id string) (Detail, error) {
	found, err := s.lookup(ctx, id)
	if err != nil {
		return Detail{}, err
	}

	created, _ := time.Parse(time.RFC3339, found.created)
	reason, origin := parseMessage(found.message)
	detail := Detail{ID: id, Commit: found.commit, Reason: reason, CreatedAt: created, Origin: origin, Tree: found.tree}
	if parents := strings.Fields(found.parents); len(parents) == 1 {
		detail.Parent = parents[0]
		parent, err := s.snapshotAt(ctx, detail.Parent)
		if err != nil && !errors.Is(err, errNoSnapshot) {
			return Detail{}, err
		}
		detail.ParentID = parent.ID
	}

	changed, err := s.changedFiles(ctx, lookupEnv, detail.Parent, detail.Commit)
	if err != nil {
		return Detail{}, err
	}
	if len(changed) > changedFilesLimit {
		changed = changed[:changedFilesLimit]
		detail.ChangedFilesTruncated = true
	}
	detail.ChangedFiles = changed
	return detail, nil
}

func (s *Store) lookup(ctx context.Context, id string) (item, error) {
	if !ValidID(id) {
		return item{}, fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	ref := s.itemsRef() + id
	records, err := s.items(ctx, ref)
	if err != nil {
		return item{}, err
	}
	for _, record := range records {
		if record.ref == ref {
			if err := checkItem(record, id); err != nil {
				return item{}, err
			}
			return record, nil
		}
	}
	return item{}, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// snapshotAt returns the snapshot whose item ref points at commit, applying
// checkItem so a reused id and a reported parent_id always resolve through Get.
func (s *Store) snapshotAt(ctx context.Context, commit string) (Snapshot, error) {
	records, err := s.items(ctx, "--points-at", commit, s.itemsRef())
	if err != nil {
		return Snapshot{}, err
	}
	for _, record := range records {
		id := strings.TrimPrefix(record.ref, s.itemsRef())
		if ValidID(id) && checkItem(record, id) == nil {
			created, _ := time.Parse(time.RFC3339, record.created)
			reason, origin := parseMessage(record.message)
			return Snapshot{ID: id, Commit: record.commit, Reason: reason, CreatedAt: created, Origin: origin}, nil
		}
	}
	return Snapshot{}, errNoSnapshot
}

const itemFormat = "%(refname)%00%(symref)%00%(objecttype)%00%(objectname)%00%(tree)%00%(parent)%00" +
	"%(authoremail)%00%(committeremail)%00%(creatordate:iso-strict)%00%(contents)%00"

type item struct {
	ref, symref, objectType, commit, tree, parents, authorEmail, committerEmail, created, message string
}

// items reads item refs for lookup. Replace objects are disabled so a
// refs/replace entry cannot substitute a snapshot's tree, parents, or message,
// and the symbolic-ref status comes from the same read as the object it
// names. A record that does not parse is an error, never "not found".
func (s *Store) items(ctx context.Context, args ...string) ([]item, error) {
	out, err := s.gitRaw(ctx, lookupEnv, append([]string{"for-each-ref", "--format=" + itemFormat}, args...)...)
	if err != nil {
		return nil, err
	}
	var records []item
	for _, record := range strings.Split(out, "\x00\n") {
		if record == "" {
			continue
		}
		f := strings.Split(record, "\x00")
		if len(f) != 10 {
			return nil, fmt.Errorf("read snapshot refs: unexpected for-each-ref record with %d fields", len(f))
		}
		records = append(records, item{f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7], f[8], f[9]})
	}
	return records, nil
}

// checkItem accepts only a ref Create could have written under id: a direct
// ref to a single-parent commit whose ID begins with the id's suffix and that
// carries the agent-snap message prefix and identity. Hand-edited refs to
// branch commits, tags, or foreign objects are refused rather than reported.
func checkItem(r item, id string) error {
	email := "<" + identityEmail + ">"
	switch {
	case r.symref != "":
		return fmt.Errorf("snapshot ref %s is a symbolic ref; refusing to read it", r.ref)
	case r.objectType != "commit":
		return fmt.Errorf("snapshot ref %s points at a %s, not a commit", r.ref, r.objectType)
	case !strings.HasPrefix(r.commit, id[len(id)-12:]):
		return fmt.Errorf("snapshot ref %s points at commit %s, which does not match its id", r.ref, r.commit)
	case len(strings.Fields(r.parents)) > 1:
		return fmt.Errorf("snapshot commit %s has more than one parent", r.commit)
	case !strings.HasPrefix(r.message, messagePrefix) || r.authorEmail != email || r.committerEmail != email:
		return fmt.Errorf("snapshot ref %s points at commit %s, which agent-snap did not write", r.ref, r.commit)
	}
	return nil
}

func (s *Store) changedFiles(ctx context.Context, env []string, parent, commit string) ([]string, error) {
	args := []string{"diff-tree", "-z", "--no-commit-id", "--name-only", "-r"}
	if parent == "" {
		args = append(args, "--root", commit)
	} else {
		args = append(args, parent, commit)
	}
	out, err := s.gitRaw(ctx, env, args...)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []string{}, nil
	}
	var names []string
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

func authorEnv() []string {
	now := strconv.FormatInt(time.Now().Unix(), 10) + " +0000"
	return []string{
		"GIT_AUTHOR_NAME=agent-snap", "GIT_AUTHOR_EMAIL=" + identityEmail,
		"GIT_COMMITTER_NAME=agent-snap", "GIT_COMMITTER_EMAIL=" + identityEmail,
		"GIT_AUTHOR_DATE=" + now, "GIT_COMMITTER_DATE=" + now,
	}
}

// sanitizedEnv sets LANGUAGE=C because commitRefs and addAll match Git's
// English messages, and Git translates some of them. LANGUAGE outranks LC_ALL
// for messages alone, so LC_ALL and LC_CTYPE, which set the encoding that a
// filter sees, stay the caller's.
func sanitizedEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		key := kv
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			key = kv[:idx]
		}
		if strings.HasPrefix(key, "GIT_") || key == "LANGUAGE" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "LANGUAGE=C")
}

// gitCmd stops Git with SIGTERM when ctx ends, because a SIGKILL can land
// while Git removes its ref lock files and leave them in the repository. A
// Git still running gitStopGrace later is killed, and the pipes are closed
// even when a filter still holds them, so cleanup finishes inside the client's
// kill grace. The filter itself is not killed. Windows has no SIGTERM, so
// cancellation kills Git there.
func gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	}
	cmd.WaitDelay = gitStopGrace
	return cmd
}

func (s *Store) command(ctx context.Context, extraEnv []string, args ...string) *exec.Cmd {
	cmd := gitCmd(ctx, slices.Concat(s.pins, args)...)
	cmd.Dir = s.repo
	cmd.Env = append(sanitizedEnv(), extraEnv...)
	return cmd
}

func (s *Store) git(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	out, err := s.gitRaw(ctx, extraEnv, args...)
	return strings.TrimSpace(out), err
}

func (s *Store) gitStdin(ctx context.Context, extraEnv []string, stdin string, args ...string) (string, error) {
	cmd := s.command(ctx, extraEnv, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := output(cmd, args)
	return strings.TrimSpace(out), err
}

// gitRaw keeps the output untrimmed, so NUL-separated records keep their
// leading and trailing whitespace.
func (s *Store) gitRaw(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	return output(s.command(ctx, extraEnv, args...), args)
}

// A clean filter, a process filter such as Git LFS, or Git itself can print
// to standard error while a command succeeds, so that text must never reach
// an object ID, a ref record, or a path list.
func output(cmd *exec.Cmd, args []string) (string, error) {
	stderr := &cappedBuffer{}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return "", &gitError{args: args, err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return string(out), nil
}

// stderrLimit bounds the standard error kept from one Git command. Git prints
// the messages that addAll and commitRefs match last, after any filter output,
// and from the matched text to the end each holds one path and a few hundred
// bytes, so keeping the last stderrLimit bytes keeps them whole for a path up
// to Linux's 4096-byte PATH_MAX.
const stderrLimit = 8 << 10

type cappedBuffer struct {
	buf     []byte
	omitted int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if extra := len(b.buf) - stderrLimit; extra > stderrLimit {
		b.omitted += extra
		b.buf = append(b.buf[:0], b.buf[extra:]...)
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	extra := max(len(b.buf)-stderrLimit, 0)
	if b.omitted+extra == 0 {
		return string(b.buf)
	}
	return fmt.Sprintf("[%d earlier bytes of standard error omitted] %s", b.omitted+extra, b.buf[extra:])
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *gitError) Unwrap() error { return e.err }

func stderrOf(err error) string {
	var failed *gitError
	if errors.As(err, &failed) {
		return failed.stderr
	}
	return ""
}

func (s *Store) Repo() string { return filepath.Clean(s.repo) }
