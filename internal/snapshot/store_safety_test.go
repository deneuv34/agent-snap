package snapshot

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCreateWorksInRepoWithoutCommits(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "fresh.txt"), []byte("unborn\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create(context.Background(), "first-snapshot", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if snap.ID == "" || snap.Commit == "" {
		t.Fatalf("missing snapshot identity: %+v", snap)
	}
	if got := git(t, dir, "show", snap.Commit+":fresh.txt"); got != "unborn" {
		t.Fatalf("content not captured: %q", got)
	}

	second, err := store.Create(context.Background(), "second-snapshot", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused || second.Commit != snap.Commit {
		t.Fatalf("expected unchanged-tree reuse: %+v", second)
	}

	items, err := store.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d snapshots, want 1: %+v", len(items), items)
	}

	if _, err := exec.Command("git", "rev-parse", "--verify", "HEAD").CombinedOutput(); err == nil {
		_ = err
	}
	headCmd := exec.Command("git", "rev-parse", "--verify", "HEAD")
	headCmd.Dir = dir
	headCmd.Env = sanitizedEnv()
	if _, err := headCmd.CombinedOutput(); err == nil {
		t.Fatal("HEAD should still be unborn after snapshots")
	}
}

func TestCreateIgnoresInheritedGitRepositoryEnv(t *testing.T) {
	repoA := testRepo(t)
	repoB := testRepo(t)
	if err := os.WriteFile(filepath.Join(repoB, "tracked.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repoB, "add", "tracked.txt")
	git(t, repoB, "commit", "-m", "other")
	if err := os.WriteFile(filepath.Join(repoA, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GIT_DIR", filepath.Join(repoB, ".git"))
	t.Setenv("GIT_WORK_TREE", repoB)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(repoB, ".git", "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(repoB, ".git", "objects"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(repoB, ".git"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(repoB, ".git", "config"))

	store, err := New(repoA)
	if err != nil {
		t.Fatalf("New must resolve repoA despite inherited git env: %v", err)
	}
	snap, err := store.Create(context.Background(), "env-test", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, repoA, "show", snap.Commit+":tracked.txt"); got != "changed" {
		t.Fatalf("snapshot must reflect repoA, got %q", got)
	}
	if refs := git(t, repoB, "for-each-ref", "refs/agent-snapshots"); refs != "" {
		t.Fatalf("repoB must be untouched, got refs: %q", refs)
	}
}

func TestCreateKeepsIndexInPrivateDir(t *testing.T) {
	dir := testRepo(t)
	indexBefore := git(t, dir, "write-tree")

	// Poison every temp override to point inside the repository: the scratch
	// machinery must refuse to place its index (or git child temp files)
	// anywhere the snapshot could capture them.
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)

	repoReal, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Deterministic part one: the exact production allocation path yields a
	// scratch directory outside the repo.
	scratch, cleanup, err := createScratchIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertOutside(t, repoReal, scratch, "scratch dir")

	info, err := os.Stat(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("scratch path %s is not a directory", scratch)
	}
	if runtime.GOOS == "windows" {
		// Windows reports synthetic 0777 directory modes; Unix mode bits do
		// not exist there. Assert the guarantees the platform provides
		// instead: allocations are unique and the helper owns cleanup.
		second, secondCleanup, err := createScratchIndex(dir)
		if err != nil {
			t.Fatal(err)
		}
		if second == scratch {
			t.Fatalf("scratch dir path is not unique: %s", scratch)
		}
		secondCleanup()
		if _, err := os.Stat(second); !os.IsNotExist(err) {
			t.Fatalf("cleanup must remove the scratch dir, got %v", err)
		}
	} else {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("scratch dir is not private: mode %v", perm)
		}
	}

	// Deterministic part two: cleanup removes the scratch directory.
	cleanup()
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the scratch dir, got %v", err)
	}

	// Deterministic part three: a full create succeeds, leaves the real
	// index untouched, and leaves no scratch artifacts behind.
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create(context.Background(), "private-index", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, dir, "write-tree"); got != indexBefore {
		t.Fatalf("index changed: got %s want %s", got, indexBefore)
	}
	out := git(t, dir, "ls-tree", "-r", "--name-only", snap.Commit)
	if strings.Contains(out, "agent-snap-index-") {
		t.Fatalf("scratch index captured into the snapshot: %s", out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "agent-snap-index-") {
			t.Fatalf("scratch dir left inside the repository: %s", entry.Name())
		}
	}
}

func assertOutside(t *testing.T, repoReal, candidate, label string) {
	t.Helper()
	real := candidate
	if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
		real = resolved
	}
	rel, err := filepath.Rel(repoReal, real)
	if err != nil {
		t.Fatalf("%s %s cannot be compared with repo %s: %v", label, candidate, repoReal, err)
	}
	if !(rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)) {
		t.Fatalf("%s %s is inside the repository %s", label, candidate, repoReal)
	}
}

func TestListPreservesReasonWithSpecialChars(t *testing.T) {
	dir := testRepo(t)
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	reason := "checkpoint | after pipe\nsecond line & more"
	if _, err := store.Create(context.Background(), reason, Origin{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), "second", Origin{}); err != nil {
		t.Fatal(err)
	}
	items, err := store.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d snapshots, want 2: %+v", len(items), items)
	}
	if items[0].Reason != "second" {
		t.Fatalf("unexpected newest reason: %q", items[0].Reason)
	}
	if items[1].Reason != reason {
		t.Fatalf("reason not preserved: got %q want %q", items[1].Reason, reason)
	}
}

func TestChangedFilesPreserveUnusualNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("newlines in filenames are not supported on Windows")
	}
	dir := testRepo(t)
	weird := "we\nird|name.txt"
	if err := os.WriteFile(filepath.Join(dir, weird), []byte("odd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create(context.Background(), "weird-names", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range snap.ChangedFiles {
		if name == weird {
			found = true
		}
	}
	if !found {
		t.Fatalf("changed_files does not preserve unusual name: %+v", snap.ChangedFiles)
	}
}

func TestLinkedWorktreesHaveIndependentSnapshots(t *testing.T) {
	dir := testRepo(t)
	linked := t.TempDir()
	git(t, dir, "worktree", "add", "-b", "feature", linked)
	if err := os.WriteFile(filepath.Join(linked, "linked.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	storeMain, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	storeLinked, err := New(linked)
	if err != nil {
		t.Fatal(err)
	}
	snapMain, err := storeMain.Create(context.Background(), "main-snap", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	snapLinked, err := storeLinked.Create(context.Background(), "linked-snap", Origin{})
	if err != nil {
		t.Fatal(err)
	}

	itemsMain, err := storeMain.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(itemsMain) != 1 || itemsMain[0].Commit != snapMain.Commit {
		t.Fatalf("main worktree must only see its own snapshots: %+v", itemsMain)
	}
	itemsLinked, err := storeLinked.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(itemsLinked) != 1 || itemsLinked[0].Commit != snapLinked.Commit {
		t.Fatalf("linked worktree must only see its own snapshots: %+v", itemsLinked)
	}
}

func TestConcurrentCreatesKeepLinearChain(t *testing.T) {
	dir := testRepo(t)
	const n = 8
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%d.txt", i)), []byte("original\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	stores := make([]*Store, n)
	for i := range stores {
		s, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	snaps := make([]Snapshot, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%d.txt", i)), []byte(fmt.Sprintf("content-%d\n", i)), 0o644); err != nil {
				errs[i] = err
				return
			}
			snap, err := stores[i].Create(context.Background(), fmt.Sprintf("concurrent-%d", i), Origin{})
			if err != nil {
				errs[i] = err
				return
			}
			snaps[i] = snap
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d failed: %v", i, err)
		}
	}

	items, err := stores[0].List(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 || len(items) > n {
		t.Fatalf("got %d snapshots, want 1..%d: %+v", len(items), n, items)
	}

	commits := make(map[string]bool)
	for _, item := range items {
		commits[item.Commit] = true
	}
	for i, snap := range snaps {
		if !commits[snap.Commit] {
			t.Fatalf("create %d result commit %s missing from items (snapshot lost)", i, snap.Commit)
		}
	}

	latest := git(t, dir, "rev-parse", stores[0].latestRef())
	for _, item := range items {
		cmd := exec.Command("git", "merge-base", "--is-ancestor", item.Commit, latest)
		cmd.Dir = dir
		cmd.Env = sanitizedEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("snapshot %s is not an ancestor of latest %s (forked chain): %v: %s", item.Commit, latest, err, out)
		}
	}
}

func TestCommitRefsRejectsStaleLatestValue(t *testing.T) {
	dir := testRepo(t)
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create(context.Background(), "one", Origin{})
	if err != nil {
		t.Fatal(err)
	}

	committed, err := store.commitRefs(context.Background(), "20260101T000000.000000000Z-stale", snap.Commit, store.zeroOID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("CAS with a stale expected value must not commit")
	}
	if got := git(t, dir, "rev-parse", store.latestRef()); got != snap.Commit {
		t.Fatalf("latest must be unchanged after failed CAS: got %s want %s", got, snap.Commit)
	}

	committed, err = store.commitRefs(context.Background(), "20260101T000000.000000000Z-ok", snap.Commit, snap.Commit, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("CAS with the correct expected value must commit")
	}
}

const objectWriteRace = "error: unable to write file .git/objects/55/e687c00476c4aad5715d303a59a01499616bec: Permission denied\nerror: changed.txt: failed to insert into database\nfatal: adding files failed"

// fakeAddFailures puts a git wrapper first on PATH that fails the first
// `failures` invocations of `git add` with stderr, then defers to real git.
// It returns how many times `git add` ran.
func fakeAddFailures(t *testing.T, failures int, stderr string) func() int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the git wrapper is a POSIX shell script")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	counter := filepath.Join(dir, "adds")
	script := fmt.Sprintf("#!/bin/sh\ncommand=$1\nif [ \"$1\" = -c ]; then command=$3; fi\nif [ \"$command\" = add ]; then\n  n=$(( $(cat %q 2>/dev/null || echo 0) + 1 ))\n  echo \"$n\" > %q\n  if [ \"$n\" -le %d ]; then printf '%%s\\n' %q >&2; exit 128; fi\nfi\nexec %q \"$@\"\n", counter, counter, failures, stderr, realGit)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		raw, err := os.ReadFile(counter)
		if err != nil {
			return 0
		}
		n := 0
		fmt.Sscan(string(raw), &n)
		return n
	}
}

func TestCreateRetriesAConcurrentObjectWriteFailure(t *testing.T) {
	dir := testRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "changed.txt"), []byte("after the race\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	adds := fakeAddFailures(t, 2, objectWriteRace)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Create(context.Background(), "race", Origin{})
	if err != nil {
		t.Fatalf("create should survive a transient object write failure: %v", err)
	}
	if got := adds(); got != 3 {
		t.Fatalf("git add ran %d times, want 3", got)
	}
	if got := git(t, dir, "show", snap.Commit+":changed.txt"); got != "after the race" {
		t.Fatalf("snapshot content = %q", got)
	}
}

func TestCreateGivesUpOnAPersistentObjectWriteFailure(t *testing.T) {
	dir := testRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "changed.txt"), []byte("never stored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	adds := fakeAddFailures(t, 1000, objectWriteRace)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), "race", Origin{}); err == nil || !strings.Contains(err.Error(), "failed to insert into database") {
		t.Fatalf("create error = %v, want the object write failure", err)
	}
	if got := adds(); got != addAttempts {
		t.Fatalf("git add ran %d times, want %d", got, addAttempts)
	}
	if items, err := s.List(context.Background(), 10); err != nil || len(items) != 0 {
		t.Fatalf("a failed create must publish no snapshot: %v %+v", err, items)
	}
}

func TestCreateDoesNotRetryOtherAddFailures(t *testing.T) {
	dir := testRepo(t)
	adds := fakeAddFailures(t, 1000, "fatal: Unable to create '.git/index.lock': File exists.")
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), "locked", Origin{}); err == nil {
		t.Fatal("create should fail")
	}
	if got := adds(); got != 1 {
		t.Fatalf("git add ran %d times, want 1", got)
	}
}

// Modification times are left out, because Git refreshes the modification
// time of an object it would otherwise write again.
func gitDirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	root := git(t, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			files[rel] = "directory"
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			files[rel] = "link to " + target
		default:
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[rel] = fmt.Sprintf("%d bytes %x", len(content), sha256.Sum256(content))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func gitDirChanges(before, after map[string]string, allowed ...string) []string {
	var changes []string
	note := func(path, change string) {
		for _, prefix := range allowed {
			if path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(path, prefix) {
				return
			}
		}
		changes = append(changes, change+" "+path)
	}
	for path, was := range before {
		if now, ok := after[path]; !ok {
			note(path, "removed")
		} else if now != was {
			note(path, "changed")
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			note(path, "added")
		}
	}
	sort.Strings(changes)
	return changes
}

func TestCreateUnderSplitIndexLeavesTheGitDirectoryUnchanged(t *testing.T) {
	dir := testRepo(t)
	git(t, dir, "config", "core.splitIndex", "true")
	git(t, dir, "update-index", "--split-index")
	shared, err := filepath.Glob(filepath.Join(git(t, dir, "rev-parse", "--absolute-git-dir"), "sharedindex.*"))
	if err != nil || len(shared) == 0 {
		t.Fatalf("the real index is not split: %v %v", shared, err)
	}
	monthAgo := time.Now().Add(-30 * 24 * time.Hour)
	for _, path := range shared {
		if err := os.Chtimes(path, monthAgo, monthAgo); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, dir, "tracked.txt", "changed\n")
	writeFile(t, dir, "new.txt", "new\n")

	before := gitDirFiles(t, dir)
	create(t, newStore(t, dir), "split index")
	if changes := gitDirChanges(before, gitDirFiles(t, dir), "objects/", "refs/agent-snapshots/"); len(changes) > 0 {
		t.Fatalf("create changed the Git directory outside its objects and snapshot refs:\n%s", strings.Join(changes, "\n"))
	}
	git(t, dir, "ls-files", "--stage")
}

func TestConcurrentRecordsLandEveryDistinctTree(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	head := git(t, dir, "rev-parse", "HEAD")
	const n = 16
	trees := make([]string, n)
	for i := range trees {
		entry := fmt.Sprintf("100644 blob %s\tfile.txt\n", blobOf(t, dir, fmt.Sprintf("content-%d\n", i)))
		tree, err := store.gitStdin(context.Background(), nil, entry, "mktree")
		if err != nil {
			t.Fatal(err)
		}
		trees[i] = tree
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	snaps := make([]Snapshot, n)
	for i := range trees {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snaps[i], errs[i] = store.recordTree(context.Background(), nil, trees[i], head, fmt.Sprintf("record-%d", i), Origin{})
		}(i)
	}
	wg.Wait()

	ids, commits := map[string]bool{}, map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("record %d failed: %v", i, err)
		}
		if snaps[i].Reused {
			t.Fatalf("record %d of a distinct tree was reused", i)
		}
		ids[snaps[i].ID], commits[snaps[i].Commit] = true, true
	}
	if len(ids) != n || len(commits) != n {
		t.Fatalf("got %d ids and %d commits, want %d distinct snapshots", len(ids), len(commits), n)
	}
	chain := strings.Fields(git(t, dir, "rev-list", store.latestRef(), "^"+head))
	if len(chain) != n {
		t.Fatalf("latest chain holds %d commits, want %d", len(chain), n)
	}
	for _, commit := range chain {
		if !commits[commit] {
			t.Fatalf("latest chain holds %s, which no record returned", commit)
		}
	}
}

func TestCreateFailsFastOnAStaleRefLock(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, dir string, store *Store) []string
	}{
		{"latest", func(t *testing.T, dir string, store *Store) []string {
			create(t, store, "first")
			writeFile(t, dir, "tracked.txt", "changed\n")
			return []string{store.latestRef() + ".lock"}
		}},
		{"item", func(t *testing.T, dir string, store *Store) []string {
			now := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
			store.now = func() time.Time { return now }
			tree := git(t, dir, "rev-parse", "HEAD^{tree}")
			head := git(t, dir, "rev-parse", "HEAD")
			var locks []string
			for sec := time.Now().Unix(); sec < time.Now().Unix()+15; sec++ {
				env := authorEnv()
				for i, kv := range env {
					if strings.HasPrefix(kv, "GIT_AUTHOR_DATE=") || strings.HasPrefix(kv, "GIT_COMMITTER_DATE=") {
						env[i] = kv[:strings.IndexByte(kv, '=')+1] + fmt.Sprintf("%d +0000", sec)
					}
				}
				commit, err := store.gitStdin(context.Background(), env, commitMessage("stale", Origin{}), "commit-tree", tree, "-p", head, "-F", "-")
				if err != nil {
					t.Fatal(err)
				}
				locks = append(locks, store.itemsRef()+now.Format("20060102T150405.000000000Z")+"-"+commit[:12]+".lock")
			}
			return locks
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := testRepo(t)
			store := newStore(t, dir)
			locks := tc.plant(t, dir, store)
			for _, lock := range locks {
				writeFile(t, filepath.Join(dir, ".git"), lock, "")
			}
			refsBefore := git(t, dir, "for-each-ref", refsPrefix)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			_, err := store.Create(ctx, "stale", Origin{})
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("a create behind a stale ref lock must fail")
			}
			named := false
			for _, lock := range locks {
				named = named || strings.Contains(err.Error(), "snapshot ref lock "+lock+" in the Git directory")
			}
			if !named || !strings.Contains(err.Error(), "remove it only when no agent-snap or Git process is running") {
				t.Fatalf("after %s the error does not name the lock and the recovery: %v", elapsed, err)
			}
			if elapsed > 2*time.Second {
				t.Fatalf("the create took %s to report the stale lock", elapsed)
			}
			for _, lock := range locks {
				if _, err := os.Stat(filepath.Join(dir, ".git", lock)); err != nil {
					t.Fatalf("the create touched the planted lock %s: %v", lock, err)
				}
			}
			if refs := git(t, dir, "for-each-ref", refsPrefix); refs != refsBefore {
				t.Fatalf("the failed create changed refs from %q to %q", refsBefore, refs)
			}
		})
	}
}

func TestCreateGivesUpOnAnUncreatableRefDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix directory permissions")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := testRepo(t)
	store := newStore(t, dir)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	runs := filepath.Join(shim, "update-ref-runs")
	writeFile(t, shim, "git", fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = update-ref ] && echo >> '%s'; done\nexec '%s' \"$@\"\n", runs, realGit))
	if err := os.Chmod(filepath.Join(shim, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	refs := filepath.Join(dir, ".git", "refs")
	if err := os.Chmod(refs, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(refs, 0o755) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err = store.Create(ctx, "denied", Origin{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a create that cannot make its ref directory must fail")
	}
	if !strings.HasPrefix(err.Error(), "update refs: ") || !strings.Contains(err.Error(), "unable to create directory for") {
		t.Fatalf("after %s the error is not the update refs failure: %v", elapsed, err)
	}
	logged, err := os.ReadFile(runs)
	if err != nil {
		t.Fatal(err)
	}
	if attempts := strings.Count(string(logged), "\n"); attempts != refDirAttempts {
		t.Fatalf("the create ran update-ref %d times, want %d", attempts, refDirAttempts)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the create took %s to give up", elapsed)
	}
	t.Logf("the create gave up after %d attempts in %s", refDirAttempts, elapsed)
	if refs := git(t, dir, "for-each-ref", refsPrefix); refs != "" {
		t.Fatalf("the failed create left refs %q", refs)
	}
}

func TestGitPrintsEnglishMessagesUnderTheCallersLocale(t *testing.T) {
	dir := testRepo(t)
	dump := filepath.ToSlash(filepath.Join(t.TempDir(), "env"))
	writeFile(t, dir, ".gitattributes", "*.env filter=env\n")
	git(t, dir, "config", "filter.env.clean", fmt.Sprintf("env > '%s'; cat", dump))
	writeFile(t, dir, "seen.env", "filtered\n")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LC_CTYPE", "fr_FR.UTF-8")
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")

	create(t, newStore(t, dir), "locale")
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the clean filter did not run: %v", err)
	}
	seen := map[string][]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			seen[key] = append(seen[key], value)
		}
	}
	want := map[string]string{"LANGUAGE": "C", "LC_ALL": "de_DE.UTF-8", "LC_CTYPE": "fr_FR.UTF-8", "LANG": "de_DE.UTF-8"}
	for key, value := range want {
		if got := seen[key]; len(got) != 1 || got[0] != value {
			t.Errorf("Git and its filter saw %s=%q, want %q", key, got, value)
		}
	}
}
