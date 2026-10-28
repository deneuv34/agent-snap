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
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	refsPrefix   = "refs/agent-snapshots/"
	itemsSuffix  = "/items/"
	latestSuffix = "/latest"
	addAttempts  = 5
)

type Snapshot struct {
	ID           string    `json:"id"`
	Commit       string    `json:"commit"`
	Reason       string    `json:"reason"`
	CreatedAt    time.Time `json:"created_at"`
	ChangedFiles []string  `json:"changed_files,omitempty"`
	Reused       bool      `json:"reused,omitempty"`
}

type Store struct {
	repo    string
	ns      string
	zeroOID string
	now     func() time.Time
}

func New(path string) (*Store, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = path
	cmd.Env = sanitizedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("find git repository: %w: %s", err, strings.TrimSpace(string(out)))
	}
	repo := strings.TrimSuffix(strings.TrimSuffix(string(out), "\n"), "\r")

	formatCmd := exec.Command("git", "rev-parse", "--show-object-format")
	formatCmd.Dir = repo
	formatCmd.Env = sanitizedEnv()
	formatOut, err := formatCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("resolve object format: %w: %s", err, strings.TrimSpace(string(formatOut)))
	}
	zeroOID := strings.Repeat("0", 40)
	if strings.TrimSpace(string(formatOut)) == "sha256" {
		zeroOID = strings.Repeat("0", 64)
	}

	gdirCmd := exec.Command("git", "rev-parse", "--absolute-git-dir")
	gdirCmd.Dir = repo
	gdirCmd.Env = sanitizedEnv()
	gdirOut, err := gdirCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("resolve git dir: %w: %s", err, strings.TrimSpace(string(gdirOut)))
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(gdirOut))))
	ns := hex.EncodeToString(sum[:8])

	return &Store{repo: repo, ns: ns, zeroOID: zeroOID, now: time.Now}, nil
}

func (s *Store) itemsRef() string {
	return refsPrefix + s.ns + itemsSuffix
}

func (s *Store) latestRef() string {
	return refsPrefix + s.ns + latestSuffix
}

func (s *Store) Create(ctx context.Context, reason string) (Snapshot, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "manual"
	}

	tmpDir, cleanup, err := createScratchIndex(s.repo)
	if err != nil {
		return Snapshot{}, err
	}
	defer cleanup()
	indexPath := filepath.Join(tmpDir, "index")
	scratchEnv := []string{
		"GIT_INDEX_FILE=" + indexPath,
		"TMPDIR=" + tmpDir,
		"TMP=" + tmpDir,
		"TEMP=" + tmpDir,
	}

	head, headErr := s.gitMaybe(ctx, scratchEnv, "rev-parse", "--verify", "HEAD")
	if headErr == nil {
		if _, err := s.git(ctx, scratchEnv, "read-tree", head); err != nil {
			return Snapshot{}, err
		}
	} else if _, err := s.git(ctx, scratchEnv, "read-tree", "--empty"); err != nil {
		return Snapshot{}, err
	}
	if err := s.addAll(ctx, scratchEnv); err != nil {
		return Snapshot{}, err
	}
	tree, err := s.git(ctx, scratchEnv, "write-tree")
	if err != nil {
		return Snapshot{}, err
	}

	for {
		if symref, err := s.isSymref(ctx, s.latestRef()); err != nil {
			return Snapshot{}, err
		} else if symref {
			return Snapshot{}, fmt.Errorf("snapshot ref %s is a symbolic ref; refusing to update it", s.latestRef())
		}

		latest, latestErr := s.gitMaybe(ctx, scratchEnv, "rev-parse", "--verify", s.latestRef())
		if latestErr == nil {
			latestTree, err := s.git(ctx, scratchEnv, "rev-parse", latest+"^{tree}")
			if err != nil {
				return Snapshot{}, err
			}
			if tree == latestTree {
				return s.snapshotForCommit(ctx, latest, true)
			}
		}

		parent := ""
		if latestErr == nil {
			parent = latest
		} else if headErr == nil {
			parent = head
		}

		args := []string{"commit-tree", tree}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		args = append(args, "-F", "-")
		commit, err := s.gitStdin(ctx, append(scratchEnv, authorEnv()...), "agent-snap: "+reason, args...)
		if err != nil {
			return Snapshot{}, err
		}

		now := s.now().UTC()
		id := now.Format("20060102T150405.000000000Z") + "-" + commit[:12]
		latestHash := ""
		if latestErr == nil {
			latestHash = latest
		}
		committed, err := s.commitRefs(ctx, id, commit, latestHash, scratchEnv)
		if err != nil {
			return Snapshot{}, err
		}
		if committed {
			changed, err := s.changedFiles(ctx, parent, commit)
			if err != nil {
				return Snapshot{}, err
			}
			return Snapshot{ID: id, Commit: commit, Reason: reason, CreatedAt: now, ChangedFiles: changed}, nil
		}
	}
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
		out, err := s.gitMaybe(ctx, env, "add", "-A", "--", ".")
		if err == nil {
			return nil
		}
		if attempt == addAttempts || !strings.Contains(out, "failed to insert into database") {
			return fmt.Errorf("git add -A -- .: %w: %s", err, out)
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
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", ref)
	cmd.Dir = s.repo
	cmd.Env = sanitizedEnv()
	err := cmd.Run()
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
// outright.
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

	out, err := s.gitStdinMaybe(ctx, extraEnv, b.String(), "update-ref", "--no-deref", "--stdin")
	if err != nil {
		if strings.Contains(out, "but expected") ||
			strings.Contains(out, "cannot lock ref") ||
			strings.Contains(out, "reference already exists") {
			return false, nil
		}
		return false, fmt.Errorf("update refs: %w: %s", err, out)
	}
	return true, nil
}

func (s *Store) List(ctx context.Context, limit int) ([]Snapshot, error) {
	if limit <= 0 {
		limit = 20
	}
	format := "%(refname:strip=4)%00%(objectname)%00%(contents)%00%(creatordate:iso-strict)%00"
	out, err := s.git(ctx, nil, "for-each-ref", "--format="+format, s.itemsRef())
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
		if len(fields) != 4 {
			continue
		}
		created, _ := time.Parse(time.RFC3339, fields[3])
		items = append(items, Snapshot{
			ID: fields[0], Commit: fields[1],
			Reason: strings.TrimPrefix(fields[2], "agent-snap: "), CreatedAt: created,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *Store) snapshotForCommit(ctx context.Context, commit string, reused bool) (Snapshot, error) {
	out, err := s.git(ctx, nil, "for-each-ref", "--points-at", commit, "--format=%(refname:strip=4)%00%(contents)%00%(creatordate:iso-strict)%00", s.itemsRef())
	if err != nil {
		return Snapshot{}, err
	}
	records := strings.Split(out, "\x00\n")
	if len(records) == 0 || records[0] == "" {
		return Snapshot{}, errors.New("snapshot ref not found")
	}
	fields := strings.Split(records[0], "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	if len(fields) != 3 {
		return Snapshot{}, errors.New("snapshot ref not found")
	}
	created, _ := time.Parse(time.RFC3339, fields[2])
	return Snapshot{ID: fields[0], Commit: commit, Reason: strings.TrimPrefix(fields[1], "agent-snap: "), CreatedAt: created, Reused: reused}, nil
}

func (s *Store) changedFiles(ctx context.Context, parent, commit string) ([]string, error) {
	args := []string{"diff-tree", "-z", "--no-commit-id", "--name-only", "-r"}
	if parent == "" {
		args = append(args, "--root", commit)
	} else {
		args = append(args, parent, commit)
	}
	out, err := s.gitRaw(ctx, nil, args...)
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
		"GIT_AUTHOR_NAME=agent-snap", "GIT_AUTHOR_EMAIL=agent-snap@localhost",
		"GIT_COMMITTER_NAME=agent-snap", "GIT_COMMITTER_EMAIL=agent-snap@localhost",
		"GIT_AUTHOR_DATE=" + now, "GIT_COMMITTER_DATE=" + now,
	}
}

func sanitizedEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key := kv
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			key = kv[:idx]
		}
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (s *Store) git(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	out, err := s.gitMaybe(ctx, extraEnv, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

func (s *Store) gitStdin(ctx context.Context, extraEnv []string, stdin string, args ...string) (string, error) {
	out, err := s.gitStdinMaybe(ctx, extraEnv, stdin, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

func (s *Store) gitMaybe(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = s.repo
	cmd.Env = append(sanitizedEnv(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// gitRaw is like git but never trims the output, so NUL-separated values
// with leading/trailing whitespace are preserved verbatim.
func (s *Store) gitRaw(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = s.repo
	cmd.Env = append(sanitizedEnv(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, string(out))
	}
	return string(out), nil
}

func (s *Store) gitStdinMaybe(ctx context.Context, extraEnv []string, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = s.repo
	cmd.Env = append(sanitizedEnv(), extraEnv...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (s *Store) Repo() string { return filepath.Clean(s.repo) }
