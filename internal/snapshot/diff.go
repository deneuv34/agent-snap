package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	DiffDefaultPatchBytes = 64 << 10
	DiffMinPatchBytes     = 1 << 10
	DiffMaxPatchBytes     = 1 << 20
	diffTokenLimit        = 1 << 20
	diffHeaderLimit       = 64 << 10
	diffStderrLimit       = 4 << 10
)

// DiffPage selects one page of a diff. A zero MaxPatchBytes means
// DiffDefaultPatchBytes.
type DiffPage struct {
	Offset        int
	MaxPatchBytes int
}

type FileStatus string

const (
	StatusAdded       FileStatus = "added"
	StatusDeleted     FileStatus = "deleted"
	StatusModified    FileStatus = "modified"
	StatusRenamed     FileStatus = "renamed"
	StatusTypeChanged FileStatus = "type_changed"
)

type PatchState string

const (
	PatchFull      PatchState = "full"
	PatchTruncated PatchState = "truncated"
	PatchDeferred  PatchState = "deferred"
	PatchBinary    PatchState = "binary"
)

type DiffSide struct {
	ID     string `json:"id"`
	Commit string `json:"commit"`
}

type DiffTotals struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

type Hunk struct {
	OldStart int    `json:"old_start"`
	OldLines int    `json:"old_lines"`
	NewStart int    `json:"new_start"`
	NewLines int    `json:"new_lines"`
	Section  string `json:"section,omitempty"`
	Text     string `json:"text"`
}

type DiffFile struct {
	Status     FileStatus `json:"status"`
	Path       string     `json:"path"`
	OldPath    string     `json:"old_path,omitempty"`
	Similarity int        `json:"similarity,omitempty"`
	OldMode    string     `json:"old_mode,omitempty"`
	NewMode    string     `json:"new_mode,omitempty"`
	OldOID     string     `json:"old_oid,omitempty"`
	NewOID     string     `json:"new_oid,omitempty"`
	Additions  int        `json:"additions"`
	Deletions  int        `json:"deletions"`
	Patch      PatchState `json:"patch"`
	PatchBytes int        `json:"patch_bytes,omitempty"`
	Hunks      []Hunk     `json:"hunks"`
	Lossy      bool       `json:"lossy,omitempty"`
}

type Diff struct {
	From       DiffSide   `json:"from"`
	To         DiffSide   `json:"to"`
	Offset     int        `json:"offset"`
	NextOffset int        `json:"next_offset,omitempty"`
	Totals     DiffTotals `json:"totals"`
	Files      []DiffFile `json:"files"`
}

// Diff returns one page of the difference from the tree of fromID to the tree
// of toID. Both ids resolve as Get resolves them, from first.
func (s *Store) Diff(ctx context.Context, fromID, toID string, page DiffPage) (Diff, error) {
	budget, err := page.budget()
	if err != nil {
		return Diff{}, err
	}
	from, err := s.lookup(ctx, fromID)
	if err != nil {
		return Diff{}, err
	}
	to, err := s.lookup(ctx, toID)
	if err != nil {
		return Diff{}, err
	}
	result := Diff{
		From:   DiffSide{ID: fromID, Commit: from.commit},
		To:     DiffSide{ID: toID, Commit: to.commit},
		Offset: page.Offset,
		Files:  []DiffFile{},
	}
	if from.tree == to.tree {
		return result, nil
	}

	dir, cleanup, err := createScratchIndex(s.repo)
	if err != nil {
		return Diff{}, err
	}
	defer cleanup()
	gitDir, env, err := s.evidenceRepo(ctx, dir)
	if err != nil {
		return Diff{}, err
	}
	p, err := runDiff(ctx, dir, env, diffArgs(gitDir, from.tree, to.tree), page.Offset, budget)
	if err != nil {
		return Diff{}, err
	}
	result.Totals, result.Files, result.NextOffset = p.totals, p.files, p.next
	return result, nil
}

func (p DiffPage) budget() (int, error) {
	if p.Offset < 0 || p.Offset > MaxPageOffset {
		return 0, fmt.Errorf("invalid diff page: offset %d is outside 0 through %d", p.Offset, MaxPageOffset)
	}
	if p.MaxPatchBytes == 0 {
		return DiffDefaultPatchBytes, nil
	}
	if p.MaxPatchBytes < DiffMinPatchBytes || p.MaxPatchBytes > DiffMaxPatchBytes {
		return 0, fmt.Errorf("invalid diff page: max patch bytes %d is outside %d through %d", p.MaxPatchBytes, DiffMinPatchBytes, DiffMaxPatchBytes)
	}
	return p.MaxPatchBytes, nil
}

// evidenceRepo creates a bare repository in dir whose only link to this
// repository is a read-only alternates entry. With no refs, no attribute
// files, no replace refs, no promisor remote, and every configuration file
// redirected into dir, nothing outside the two trees can shape the diff, and
// a missing object fails instead of being fetched. Git never writes into an
// alternate object store.
func (s *Store) evidenceRepo(ctx context.Context, dir string) (string, []string, error) {
	objects, err := s.gitRaw(ctx, nil, "rev-parse", "--git-path", "objects")
	if err != nil {
		return "", nil, err
	}
	objects = strings.TrimSuffix(strings.TrimSuffix(objects, "\n"), "\r")
	if !filepath.IsAbs(objects) {
		objects = filepath.Join(s.repo, objects)
	}
	if strings.ContainsAny(objects, "\n\r") {
		return "", nil, fmt.Errorf("object directory %q cannot be written to an alternates file", objects)
	}

	env := append(sanitizedEnv(),
		"HOME="+dir, "XDG_CONFIG_HOME="+dir, "USERPROFILE="+dir,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1",
	)
	format := "sha1"
	if len(s.zeroOID) == 64 {
		format = "sha256"
	}
	gitDir := filepath.Join(dir, "evidence.git")
	cmd := exec.CommandContext(ctx, "git", "init", "--quiet", "--bare", "--template=", "--object-format="+format, gitDir)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("create evidence repository: %w: %s", err, strings.TrimSpace(string(out)))
	}
	info := filepath.Join(gitDir, "objects", "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		return "", nil, fmt.Errorf("create evidence repository: %w", err)
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(filepath.ToSlash(objects)+"\n"), 0o600); err != nil {
		return "", nil, fmt.Errorf("create evidence repository: %w", err)
	}
	return gitDir, env, nil
}

// diffArgs names every option that shapes the output, so the result does not
// depend on Git's defaults.
func diffArgs(gitDir, fromTree, toTree string) []string {
	return []string{
		"--git-dir=" + gitDir, "diff-tree", "-r", "-z", "--raw", "--numstat", "--patch",
		"--find-renames=50%", "-l1000", "--full-index", "--no-color", "--no-ext-diff", "--no-textconv",
		"--unified=3", "--inter-hunk-context=0", "--indent-heuristic", "--diff-algorithm=myers",
		fromTree, toTree,
	}
}

// runDiff streams one diff-tree process through readDiff. When the page is
// complete before Git finishes, the pipe is closed and the process is killed:
// everything the page needs has been read, and the exit status is expected.
// Closing the pipe stops a Git that runs under a launcher the kill does not
// reach, such as Git for Windows' cmd\git.exe, which would otherwise block on
// a full pipe while Wait waits for its stderr. Missing objects still fail,
// because numstat reads every blob before the patch.
func runDiff(ctx context.Context, dir string, env, args []string, offset, budget int) (diffPage, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	stderr := &cappedBuffer{limit: diffStderrLimit}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return diffPage{}, fmt.Errorf("git diff-tree: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return diffPage{}, fmt.Errorf("git diff-tree: %w", err)
	}
	page, complete, perr := readDiff(bufio.NewReaderSize(stdout, 64<<10), offset, budget)
	if !complete {
		stdout.Close()
		cancel()
	}
	werr := cmd.Wait()
	if err := ctx.Err(); err != nil {
		return diffPage{}, fmt.Errorf("git diff-tree: %w", err)
	}
	if perr != nil {
		return diffPage{}, fmt.Errorf("git diff-tree: %w: %s", perr, strings.TrimSpace(stderr.String()))
	}
	if complete && werr != nil {
		return diffPage{}, fmt.Errorf("git diff-tree: %w: %s", werr, strings.TrimSpace(stderr.String()))
	}
	return page, nil
}

type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

type diffPage struct {
	totals DiffTotals
	files  []DiffFile
	next   int
}

// readDiff parses `diff-tree -z --raw --numstat --patch` output into the page
// that starts at offset. The stream is the raw records, one numstat record per
// raw record in the same order, one NUL, then the patch. A type change owns two
// patch sections and every other entry owns one. complete reports that the
// whole stream was read; otherwise the caller may stop the producer.
func readDiff(r *bufio.Reader, offset, budget int) (diffPage, bool, error) {
	sum, err := readSummary(r, offset)
	if err != nil {
		return diffPage{}, false, err
	}
	page := diffPage{totals: sum.totals, files: sum.files}
	if offset >= sum.totals.Files {
		page.files = []DiffFile{}
		return page, sum.totals.Files == 0, nil
	}

	p := &patchReader{r: r}
	for i := 0; i < sum.sectionsBefore; i++ {
		if err := p.skipSection(); err != nil {
			return diffPage{}, false, err
		}
	}
	used, ended := 0, false
	for i := range page.files {
		f := &page.files[i]
		if ended {
			if f.Patch != PatchBinary {
				f.Patch = PatchDeferred
			}
			continue
		}
		if f.Patch == PatchBinary {
			if err := p.skipFile(f); err != nil {
				return diffPage{}, false, err
			}
			continue
		}
		cost, err := p.readFile(f, budget-used, used == 0)
		if err != nil {
			return diffPage{}, false, err
		}
		if f.Patch == PatchFull {
			used += cost
		} else {
			ended = true
		}
	}
	end := offset + len(page.files)
	page.next = end
	for i, f := range page.files {
		if f.Patch == PatchDeferred {
			page.next = offset + i
			break
		}
	}
	if page.next == sum.totals.Files {
		page.next = 0
	}
	if ended || end < sum.totals.Files {
		return page, false, nil
	}
	if !atEOF(r) {
		return diffPage{}, false, errors.New("diff patch has more sections than diff entries")
	}
	return page, true, nil
}

func atEOF(r *bufio.Reader) bool {
	_, err := r.Peek(1)
	return err == io.EOF
}

type diffSummary struct {
	totals         DiffTotals
	files          []DiffFile
	sectionsBefore int
}

// readSummary reads the raw and numstat records. Paths of both are hashed in
// order, so every entry, not only the page, must agree between the two.
func readSummary(r *bufio.Reader, offset int) (diffSummary, error) {
	var sum diffSummary
	rawPaths, numPaths := sha256.New(), sha256.New()
	raws, numstats, oidLen := 0, 0, 0
	inWindow := func(i int) bool { return i >= offset && i-offset < pageSize }
	for {
		b, err := r.Peek(1)
		if err == io.EOF && raws == 0 {
			return sum, nil
		}
		if err != nil {
			return diffSummary{}, fmt.Errorf("diff summary ends early: %w", unexpected(err))
		}
		switch {
		case b[0] == ':':
			if numstats > 0 {
				return diffSummary{}, errors.New("diff summary has a raw record after a numstat record")
			}
			f, err := readRaw(r, rawPaths, &oidLen)
			if err != nil {
				return diffSummary{}, err
			}
			if raws < offset && f.Status == StatusTypeChanged {
				sum.sectionsBefore += 2
			} else if raws < offset {
				sum.sectionsBefore++
			}
			if inWindow(raws) {
				sum.files = append(sum.files, f)
			}
			raws++
		case b[0] == 0:
			if _, err := r.ReadByte(); err != nil {
				return diffSummary{}, err
			}
			if raws == 0 {
				return diffSummary{}, errors.New("diff summary has a separator but no records")
			}
			if raws != numstats {
				return diffSummary{}, fmt.Errorf("diff summary has %d raw records and %d numstat records", raws, numstats)
			}
			if !bytes.Equal(rawPaths.Sum(nil), numPaths.Sum(nil)) {
				return diffSummary{}, errors.New("diff summary paths differ between raw and numstat records")
			}
			sum.totals.Files = raws
			if sum.files == nil {
				sum.files = []DiffFile{}
			}
			return sum, nil
		default:
			if numstats >= raws {
				return diffSummary{}, errors.New("diff summary has more numstat records than raw records")
			}
			adds, dels, binary, err := readNumstat(r, numPaths)
			if err != nil {
				return diffSummary{}, err
			}
			sum.totals.Additions += adds
			sum.totals.Deletions += dels
			if inWindow(numstats) {
				f := &sum.files[numstats-offset]
				f.Additions, f.Deletions = adds, dels
				if binary {
					f.Patch = PatchBinary
				}
			}
			numstats++
		}
	}
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func readToken(r *bufio.Reader) ([]byte, error) {
	token, n, err := readUntil(r, 0, diffTokenLimit)
	if err != nil {
		return nil, fmt.Errorf("diff summary ends early: %w", unexpected(err))
	}
	if token == nil {
		return nil, fmt.Errorf("diff summary token of %d bytes exceeds %d bytes", n, diffTokenLimit)
	}
	return token[:len(token)-1], nil
}

func readRaw(r *bufio.Reader, paths hash.Hash, oidLen *int) (DiffFile, error) {
	token, err := readToken(r)
	if err != nil {
		return DiffFile{}, err
	}
	fields := strings.Split(string(token), " ")
	if len(fields) != 5 || !strings.HasPrefix(fields[0], ":") {
		return DiffFile{}, fmt.Errorf("malformed raw diff record %q", token)
	}
	oldMode, newMode, oldOID, newOID, status := fields[0][1:], fields[1], fields[2], fields[3], fields[4]
	for _, mode := range []string{oldMode, newMode} {
		if !validMode(mode) {
			return DiffFile{}, fmt.Errorf("malformed raw diff record %q: mode %q", token, mode)
		}
	}
	if *oidLen == 0 {
		*oidLen = len(oldOID)
	}
	for _, oid := range []string{oldOID, newOID} {
		if len(oid) != *oidLen || (len(oid) != 40 && len(oid) != 64) || !isLowerHex(oid) {
			return DiffFile{}, fmt.Errorf("malformed raw diff record %q: object id %q", token, oid)
		}
	}

	f := DiffFile{OldMode: oldMode, NewMode: newMode, OldOID: oldOID, NewOID: newOID, Hunks: []Hunk{}}
	zero := strings.Repeat("0", *oidLen)
	oldAbsent, newAbsent := oldMode == "000000" && oldOID == zero, newMode == "000000" && newOID == zero
	switch {
	case status == "A" && oldAbsent && !newAbsent:
		f.Status, f.OldMode, f.OldOID = StatusAdded, "", ""
	case status == "D" && newAbsent && !oldAbsent:
		f.Status, f.NewMode, f.NewOID = StatusDeleted, "", ""
	case status == "M" && !oldAbsent && !newAbsent:
		f.Status = StatusModified
	case status == "T" && !oldAbsent && !newAbsent:
		f.Status = StatusTypeChanged
	case len(status) == 4 && status[0] == 'R' && !oldAbsent && !newAbsent:
		score, err := strconv.Atoi(status[1:])
		if err != nil || score < 50 || score > 100 {
			return DiffFile{}, fmt.Errorf("malformed raw diff record %q: rename score", token)
		}
		f.Status, f.Similarity = StatusRenamed, score
	default:
		return DiffFile{}, fmt.Errorf("unsupported raw diff record %q", token)
	}

	path, err := readToken(r)
	if err != nil {
		return DiffFile{}, err
	}
	writePath(paths, path)
	if f.Status == StatusRenamed {
		newPath, err := readToken(r)
		if err != nil {
			return DiffFile{}, err
		}
		writePath(paths, newPath)
		f.OldPath = repairUTF8(path, &f.Lossy)
		path = newPath
	}
	f.Path = repairUTF8(path, &f.Lossy)
	return f, nil
}

func readNumstat(r *bufio.Reader, paths hash.Hash) (adds, dels int, binary bool, err error) {
	token, err := readToken(r)
	if err != nil {
		return 0, 0, false, err
	}
	fields := bytes.SplitN(token, []byte("\t"), 3)
	if len(fields) != 3 {
		return 0, 0, false, fmt.Errorf("malformed numstat record %q", token)
	}
	if string(fields[0]) == "-" && string(fields[1]) == "-" {
		binary = true
	} else if adds, err = strconv.Atoi(string(fields[0])); err != nil || adds < 0 || !isDigits(fields[0]) {
		return 0, 0, false, fmt.Errorf("malformed numstat record %q", token)
	} else if dels, err = strconv.Atoi(string(fields[1])); err != nil || dels < 0 || !isDigits(fields[1]) {
		return 0, 0, false, fmt.Errorf("malformed numstat record %q", token)
	}
	if len(fields[2]) > 0 {
		writePath(paths, fields[2])
		return adds, dels, binary, nil
	}
	for i := 0; i < 2; i++ {
		path, err := readToken(r)
		if err != nil {
			return 0, 0, false, err
		}
		writePath(paths, path)
	}
	return adds, dels, binary, nil
}

func writePath(h hash.Hash, path []byte) {
	h.Write(path)
	h.Write([]byte{0})
}

func validMode(mode string) bool {
	if len(mode) != 6 {
		return false
	}
	for _, c := range mode {
		if c < '0' || c > '7' {
			return false
		}
	}
	return true
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isDigits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(b) > 0
}

// repairUTF8 makes text valid UTF-8 for JSON. Every count and budget charge is
// taken from the original bytes before this runs.
func repairUTF8(b []byte, lossy *bool) string {
	if utf8.Valid(b) {
		return string(b)
	}
	*lossy = true
	return strings.ToValidUTF8(string(b), "�")
}

// readUntil reads through the next delim. It returns the bytes read, delim
// included, when they number at most keep; otherwise it returns nil and still
// consumes through delim, so an oversized line is never buffered.
func readUntil(r *bufio.Reader, delim byte, keep int) ([]byte, int, error) {
	var out []byte
	n := 0
	for {
		chunk, err := r.ReadSlice(delim)
		n += len(chunk)
		if n <= keep {
			out = append(out, chunk...)
		} else {
			out = nil
		}
		switch {
		case err == nil:
			return out, n, nil
		case err == bufio.ErrBufferFull:
			continue
		case err == io.EOF && n == 0:
			return nil, 0, io.EOF
		default:
			return nil, n, unexpected(err)
		}
	}
}

var (
	sectionStart = []byte("diff --git ")
	hunkStart    = []byte("@@ -")
	headerLines  = [][]byte{
		[]byte("old mode "), []byte("new mode "), []byte("deleted file mode "), []byte("new file mode "),
		[]byte("similarity index "), []byte("rename from "), []byte("rename to "), []byte("index "),
		[]byte("--- "), []byte("+++ "), []byte("Binary files "),
	}
)

type patchReader struct {
	r *bufio.Reader
}

func (p *patchReader) startsWith(prefix []byte) bool {
	b, _ := p.r.Peek(len(prefix))
	return bytes.Equal(b, prefix)
}

func (p *patchReader) section() error {
	if !p.startsWith(sectionStart) {
		if atEOF(p.r) {
			return errors.New("diff patch has fewer sections than diff entries")
		}
		return errors.New("diff patch section does not start with a diff --git line")
	}
	if _, _, err := readUntil(p.r, '\n', 0); err != nil {
		return fmt.Errorf("diff patch section header: %w", err)
	}
	for !atEOF(p.r) && !p.startsWith(hunkStart) && !p.startsWith(sectionStart) {
		known := false
		for _, prefix := range headerLines {
			known = known || p.startsWith(prefix)
		}
		if !known {
			return errors.New("diff patch section has an unexpected header line")
		}
		if _, _, err := readUntil(p.r, '\n', 0); err != nil {
			return fmt.Errorf("diff patch section header: %w", err)
		}
	}
	return nil
}

type hunkRead struct {
	hunk       Hunk
	kept       bool
	cost       int
	adds, dels int
}

// hunk reads the next hunk of the current section and keeps its text only
// when its whole cost is at most keep.
func (p *patchReader) hunk(keep int, lossy *bool) (hunkRead, bool, error) {
	if !p.startsWith(hunkStart) {
		return hunkRead{}, false, nil
	}
	header, n, err := readUntil(p.r, '\n', diffHeaderLimit)
	if err != nil {
		return hunkRead{}, false, fmt.Errorf("diff hunk header: %w", err)
	}
	if header == nil {
		return hunkRead{}, false, fmt.Errorf("diff hunk header of %d bytes exceeds %d bytes", n, diffHeaderLimit)
	}
	h, section, err := parseHunkHeader(header[:len(header)-1])
	if err != nil {
		return hunkRead{}, false, err
	}
	out := hunkRead{cost: len(header), kept: len(header) <= keep}
	var text []byte
	oldLeft, newLeft := h.OldLines, h.NewLines
	for prev := byte(0); ; {
		b, err := p.r.Peek(1)
		if oldLeft == 0 && newLeft == 0 && (err != nil || b[0] != '\\' || prev == 0 || prev == '\\') {
			break
		}
		if err != nil {
			return hunkRead{}, false, fmt.Errorf("diff hunk body ends before its header's line counts: %w", unexpected(err))
		}
		switch {
		case b[0] == ' ' && oldLeft > 0 && newLeft > 0:
			oldLeft, newLeft = oldLeft-1, newLeft-1
		case b[0] == '-' && oldLeft > 0:
			oldLeft, out.dels = oldLeft-1, out.dels+1
		case b[0] == '+' && newLeft > 0:
			newLeft, out.adds = newLeft-1, out.adds+1
		case b[0] == '\\' && prev != 0 && prev != '\\':
		default:
			return hunkRead{}, false, fmt.Errorf("diff hunk body does not match its header @@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		}
		prev = b[0]
		limit := 0
		if out.kept {
			limit = keep - out.cost
		}
		line, n, err := readUntil(p.r, '\n', limit)
		if err != nil {
			return hunkRead{}, false, fmt.Errorf("diff hunk body line: %w", err)
		}
		out.cost += n
		if line == nil {
			out.kept, text = false, nil
		} else if out.kept {
			text = append(text, line...)
		}
	}
	if !atEOF(p.r) && !p.startsWith(hunkStart) && !p.startsWith(sectionStart) {
		return hunkRead{}, false, fmt.Errorf("diff hunk body does not match its header @@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
	}
	if out.kept {
		h.Section = repairUTF8(section, lossy)
		h.Text = repairUTF8(text, lossy)
		out.hunk = h
	}
	return out, true, nil
}

// parseHunkHeader parses "@@ -a[,b] +c[,d] @@[ section]". Git omits a count
// of 1.
func parseHunkHeader(line []byte) (Hunk, []byte, error) {
	bad := errors.New("malformed diff hunk header")
	rest, ok := bytes.CutPrefix(line, hunkStart)
	if !ok {
		return Hunk{}, nil, bad
	}
	ranges, section, ok := bytes.Cut(rest, []byte(" @@"))
	if !ok || (len(section) > 0 && section[0] != ' ') {
		return Hunk{}, nil, bad
	}
	if len(section) > 0 {
		section = section[1:]
	}
	oldRange, newRange, ok := bytes.Cut(ranges, []byte(" +"))
	if !ok {
		return Hunk{}, nil, bad
	}
	var h Hunk
	var err1, err2 error
	h.OldStart, h.OldLines, err1 = parseRange(oldRange)
	h.NewStart, h.NewLines, err2 = parseRange(newRange)
	if err1 != nil || err2 != nil || h.OldLines+h.NewLines == 0 {
		return Hunk{}, nil, bad
	}
	return h, section, nil
}

func parseRange(b []byte) (int, int, error) {
	startText, countText, hasCount := bytes.Cut(b, []byte(","))
	if !isDigits(startText) || (hasCount && !isDigits(countText)) {
		return 0, 0, errors.New("malformed range")
	}
	start, err := strconv.Atoi(string(startText))
	if err != nil {
		return 0, 0, err
	}
	if !hasCount {
		return start, 1, nil
	}
	count, err := strconv.Atoi(string(countText))
	return start, count, err
}

func sections(f *DiffFile) int {
	if f.Status == StatusTypeChanged {
		return 2
	}
	return 1
}

func (p *patchReader) skipSection() error {
	if err := p.section(); err != nil {
		return err
	}
	var lossy bool
	for {
		_, ok, err := p.hunk(0, &lossy)
		if err != nil || !ok {
			return err
		}
	}
}

func (p *patchReader) skipFile(f *DiffFile) error {
	for i := 0; i < sections(f); i++ {
		if err := p.skipSection(); err != nil {
			return err
		}
	}
	return nil
}

// readFile sets a text file's patch state and returns the cost it charges to
// the page. A file that does not fit stops the read as soon as the page needs
// nothing more: at once when deferred, and when truncated, once its whole
// cost is known or exceeds DiffMaxPatchBytes.
func (p *patchReader) readFile(f *DiffFile, remaining int, first bool) (int, error) {
	cost, adds, dels, fits, lossy := 0, 0, 0, true, false
	for i := 0; i < sections(f); i++ {
		if err := p.section(); err != nil {
			return 0, err
		}
		for {
			keep := 0
			if fits {
				keep = remaining - cost
			}
			h, ok, err := p.hunk(keep, &lossy)
			if err != nil {
				return 0, err
			}
			if !ok {
				break
			}
			cost += h.cost
			adds, dels = adds+h.adds, dels+h.dels
			if fits && h.kept {
				f.Hunks = append(f.Hunks, h.hunk)
				continue
			}
			if fits && !first {
				f.Patch, f.Hunks = PatchDeferred, []Hunk{}
				return 0, nil
			}
			fits = false
			if cost > DiffMaxPatchBytes {
				f.Patch, f.Lossy = PatchTruncated, f.Lossy || lossy
				return 0, nil
			}
		}
	}
	f.Lossy = f.Lossy || lossy
	if !fits {
		f.Patch, f.PatchBytes = PatchTruncated, cost
		return 0, nil
	}
	if adds != f.Additions || dels != f.Deletions {
		return 0, fmt.Errorf("diff hunks of %s add %d and delete %d lines, but numstat reports %d and %d", f.Path, adds, dels, f.Additions, f.Deletions)
	}
	f.Patch = PatchFull
	return cost, nil
}
