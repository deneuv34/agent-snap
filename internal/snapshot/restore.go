package snapshot

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

var ErrStale = errors.New("working tree changed since the plan")

type PlanQuery struct {
	Scope       Scope
	Offset      int
	CurrentTree string
}

type Action string

const (
	ActionCreate     Action = "create"
	ActionModify     Action = "modify"
	ActionTypeChange Action = "type_change"
	ActionDelete     Action = "delete"
)

type RepoBlocker string

const (
	BlockOperationInProgress RepoBlocker = "operation_in_progress"
	BlockUnmergedIndex       RepoBlocker = "unmerged_index"
	BlockSparseCheckout      RepoBlocker = "sparse_checkout"
)

type PathBlocker string

const (
	BlockUnaddressablePath PathBlocker = "unaddressable_path"
	BlockUnsafePath        PathBlocker = "unsafe_path"
	BlockSubmodule         PathBlocker = "submodule"
	BlockParentConflict    PathBlocker = "parent_conflict"
	BlockIndexFlag         PathBlocker = "index_flag"
	BlockNameCollision     PathBlocker = "name_collision"
	BlockUntrackedInTheWay PathBlocker = "untracked_in_the_way"
)

type PlanSnapshot struct {
	ID     string `json:"id"`
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
}

type PlanSide struct {
	Mode string `json:"mode"`
	OID  string `json:"oid"`
}

type PlanTotals struct {
	Entries    int `json:"entries"`
	Create     int `json:"create"`
	Modify     int `json:"modify"`
	TypeChange int `json:"type_change"`
	Delete     int `json:"delete"`
	Staged     int `json:"staged"`
	Unstaged   int `json:"unstaged"`
	Untracked  int `json:"untracked"`
	Blocked    int `json:"blocked"`
}

type PlanEntry struct {
	Path      string      `json:"path"`
	Action    Action      `json:"action"`
	Current   *PlanSide   `json:"current,omitempty"`
	Target    *PlanSide   `json:"target,omitempty"`
	Staged    bool        `json:"staged,omitempty"`
	Unstaged  bool        `json:"unstaged,omitempty"`
	Untracked bool        `json:"untracked,omitempty"`
	Blocker   PathBlocker `json:"blocker,omitempty"`
}

type RestorePlan struct {
	Snapshot    PlanSnapshot  `json:"snapshot"`
	Paths       []string      `json:"paths,omitempty"`
	CurrentTree string        `json:"current_tree"`
	ResultTree  string        `json:"result_tree,omitempty"`
	Applicable  bool          `json:"applicable"`
	Blockers    []RepoBlocker `json:"blockers"`
	Unmatched   []string      `json:"unmatched"`
	Offset      int           `json:"offset"`
	NextOffset  int           `json:"next_offset,omitempty"`
	Totals      PlanTotals    `json:"totals"`
	Entries     []PlanEntry   `json:"entries"`
}

func ValidObjectID(id string) bool {
	return (len(id) == 40 || len(id) == 64) && isLowerHex(id)
}

func (s *Store) PlanRestore(ctx context.Context, id string, q PlanQuery) (RestorePlan, error) {
	plan, err := s.planRestore(ctx, id, q)
	if ctx.Err() != nil {
		return RestorePlan{}, fmt.Errorf("plan restore: %w", ctx.Err())
	}
	if err != nil {
		return RestorePlan{}, err
	}
	return plan, nil
}

func (s *Store) planRestore(ctx context.Context, id string, q PlanQuery) (RestorePlan, error) {
	if err := q.check(); err != nil {
		return RestorePlan{}, err
	}
	snap, err := s.lookup(ctx, id)
	if err != nil {
		return RestorePlan{}, err
	}
	dir, cleanup, err := createScratchIndex(s.repo)
	if err != nil {
		return RestorePlan{}, err
	}
	defer cleanup()
	gitDir, env, err := s.evidenceRepo(ctx, dir)
	if err != nil {
		return RestorePlan{}, err
	}
	ev := evidence{dir: dir, gitDir: gitDir, env: env, oidLen: len(s.zeroOID)}

	// Capture writes its new objects into the evidence repository, which the
	// cleanup removes, so the real object store gains nothing.
	current, head, err := s.captureTree(ctx, append(scratchEnv(dir), "GIT_OBJECT_DIRECTORY="+filepath.Join(gitDir, "objects")))
	if err != nil {
		return RestorePlan{}, err
	}
	if q.CurrentTree != "" && q.CurrentTree != current {
		return RestorePlan{}, ErrStale
	}

	state, err := s.readWorktree(ctx)
	if err != nil {
		return RestorePlan{}, err
	}
	cur, err := ev.listTree(ctx, current)
	if err != nil {
		return RestorePlan{}, err
	}
	tgt, err := ev.listTree(ctx, snap.tree)
	if err != nil {
		return RestorePlan{}, err
	}
	heads, err := ev.headView(ctx, head, current, cur)
	if err != nil {
		return RestorePlan{}, err
	}
	p := buildPlan(q.Scope, cur, tgt)
	index, err := s.readIndex(ctx, p.paths())
	if err != nil {
		return RestorePlan{}, err
	}
	w := newWorld(s.repo, p, index, heads, state.ignoreCase || foldingProbe(s.repo, cur))
	if err := p.decide(ctx, w); err != nil {
		return RestorePlan{}, err
	}
	result := snap.tree
	if !q.Scope.whole() {
		result = p.resultTree(s.newHash)
	}
	return p.page(PlanSnapshot{ID: id, Commit: snap.commit, Tree: snap.tree}, q, current, result, state.blockers(index.unmerged)), nil
}

func (q PlanQuery) check() error {
	switch {
	case q.Offset < 0 || q.Offset > MaxPageOffset:
		return fmt.Errorf("invalid restore plan page: offset %d is outside 0 through %d", q.Offset, MaxPageOffset)
	case q.CurrentTree != "" && !ValidObjectID(q.CurrentTree):
		return fmt.Errorf("invalid restore plan page: current tree %q is not an object id", q.CurrentTree)
	case q.Offset > 0 && q.CurrentTree == "":
		return errors.New("invalid restore plan page: a page after the first needs the current tree of the first page")
	}
	return nil
}

func (s *Store) newHash() hash.Hash {
	if len(s.zeroOID) == 64 {
		return sha256.New()
	}
	return sha1.New()
}

type evidence struct {
	dir, gitDir string
	env         []string
	oidLen      int
}

func (e evidence) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + e.gitDir}, args...)...)
	cmd.Dir = e.dir
	cmd.Env = e.env
	stderr := &cappedBuffer{limit: diffStderrLimit}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func (e evidence) listTree(ctx context.Context, tree string) (listing, error) {
	out, err := e.git(ctx, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return nil, err
	}
	var leaves listing
	for _, record := range records(out) {
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Split(meta, " ")
		if !ok || len(fields) != 3 || !leafType(fields[0], fields[1]) || !e.validOID(fields[2]) {
			return nil, fmt.Errorf("read tree %s: malformed ls-tree record %q", tree, record)
		}
		leaves = append(leaves, leaf{path: path, mode: fields[0], oid: fields[2]})
	}
	slices.SortFunc(leaves, func(a, b leaf) int { return strings.Compare(a.path, b.path) })
	if err := leaves.check(); err != nil {
		return nil, fmt.Errorf("read tree %s: %w", tree, err)
	}
	return leaves, nil
}

func (e evidence) headView(ctx context.Context, head, currentTree string, current listing) (headView, error) {
	if head == "" {
		return headView{unborn: true}, nil
	}
	out, err := e.git(ctx, "diff-tree", "-r", "-z", "--raw", "--no-renames", "--no-abbrev", head+"^{tree}", currentTree)
	if err != nil {
		return headView{}, err
	}
	v := headView{delta: map[string]*leaf{}, current: current}
	tokens := records(out)
	if len(tokens)%2 != 0 {
		return headView{}, fmt.Errorf("read HEAD: diff-tree printed %d tokens", len(tokens))
	}
	for i := 0; i < len(tokens); i += 2 {
		fields := strings.Split(tokens[i], " ")
		if len(fields) != 5 || !strings.HasPrefix(fields[0], ":") || !validMode(fields[0][1:]) || !validMode(fields[1]) ||
			!e.validOID(fields[2]) || !e.validOID(fields[3]) {
			return headView{}, fmt.Errorf("read HEAD: malformed diff-tree record %q", tokens[i])
		}
		path := tokens[i+1]
		v.delta[path] = nil
		if mode := fields[0][1:]; mode != "000000" {
			v.delta[path] = &leaf{path: path, mode: mode, oid: fields[2]}
		}
	}
	return v, nil
}

func (e evidence) validOID(oid string) bool { return len(oid) == e.oidLen && isLowerHex(oid) }

func records(out string) []string {
	if out == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
}

func leafType(mode, objectType string) bool {
	switch mode {
	case modeFile, modeExecutable, modeSymlink:
		return objectType == "blob"
	case modeGitlink:
		return objectType == "commit"
	}
	return false
}

type indexEntry struct {
	mode, oid string
	flagged   bool
}

type indexView struct {
	stage0   map[string]indexEntry
	tracked  map[string]bool
	unmerged bool
}

func (s *Store) readIndex(ctx context.Context, paths map[string]bool) (indexView, error) {
	out, err := s.gitRaw(ctx, []string{"GIT_OPTIONAL_LOCKS=0"}, "ls-files", "-z", "-s", "-v")
	if err != nil {
		return indexView{}, err
	}
	v := indexView{stage0: map[string]indexEntry{}, tracked: map[string]bool{}}
	for _, record := range records(out) {
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Split(meta, " ")
		if !ok || len(fields) != 4 || len(fields[0]) != 1 || !validMode(fields[1]) ||
			len(fields[2]) != len(s.zeroOID) || !isLowerHex(fields[2]) || len(fields[3]) != 1 || fields[3][0] < '0' || fields[3][0] > '3' {
			return indexView{}, fmt.Errorf("read index: malformed ls-files record %q", record)
		}
		if fields[3] != "0" {
			v.unmerged = true
		}
		if !paths[path] {
			continue
		}
		v.tracked[path] = true
		if fields[3] == "0" {
			tag := fields[0][0]
			v.stage0[path] = indexEntry{mode: fields[1], oid: fields[2], flagged: tag >= 'a' && tag <= 'z' || tag == 'S'}
		}
	}
	return v, nil
}

var operationFiles = []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "BISECT_LOG", "rebase-merge", "rebase-apply", "sequencer"}

type worktreeState struct {
	operation, sparse, ignoreCase bool
}

func (s *Store) readWorktree(ctx context.Context) (worktreeState, error) {
	args := []string{"rev-parse"}
	for _, name := range operationFiles {
		args = append(args, "--git-path", name)
	}
	out, err := s.gitRaw(ctx, nil, args...)
	if err != nil {
		return worktreeState{}, err
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(operationFiles) {
		return worktreeState{}, fmt.Errorf("read operation files: rev-parse printed %d paths for %d names", len(lines), len(operationFiles))
	}
	var state worktreeState
	for _, line := range lines {
		path := strings.TrimSuffix(line, "\r")
		if path == "" || strings.Contains(path, "\r") {
			return worktreeState{}, fmt.Errorf("read operation files: unexpected path %q", line)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.repo, path)
		}
		_, err := os.Lstat(path)
		switch {
		case err == nil:
			state.operation = true
		case !errors.Is(err, fs.ErrNotExist):
			return worktreeState{}, fmt.Errorf("read operation files: %w", err)
		}
	}

	out, err = s.gitRaw(ctx, nil, "config", "--type=bool", "--get-regexp", `^core\.(sparsecheckout|ignorecase)$`)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		out, err = "", nil
	}
	if err != nil {
		return worktreeState{}, err
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		key, value, _ := strings.Cut(strings.TrimSuffix(line, "\r"), " ")
		switch key {
		case "core.sparsecheckout":
			state.sparse = value == "true"
		case "core.ignorecase":
			state.ignoreCase = value == "true"
		}
	}
	return state, nil
}

func (r worktreeState) blockers(unmerged bool) []RepoBlocker {
	blockers := []RepoBlocker{}
	if r.operation {
		blockers = append(blockers, BlockOperationInProgress)
	}
	if unmerged {
		blockers = append(blockers, BlockUnmergedIndex)
	}
	if r.sparse {
		blockers = append(blockers, BlockSparseCheckout)
	}
	return blockers
}
