package snapshot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
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
	snap, err := store.Create(context.Background(), "first-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if snap.ID == "" || snap.Commit == "" {
		t.Fatalf("missing snapshot identity: %+v", snap)
	}
	if got := git(t, dir, "show", snap.Commit+":fresh.txt"); got != "unborn" {
		t.Fatalf("content not captured: %q", got)
	}

	second, err := store.Create(context.Background(), "second-snapshot")
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
	snap, err := store.Create(context.Background(), "env-test")
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
	snap, err := store.Create(context.Background(), "private-index")
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
	if _, err := store.Create(context.Background(), reason); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), "second"); err != nil {
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
	snap, err := store.Create(context.Background(), "weird-names")
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
	snapMain, err := storeMain.Create(context.Background(), "main-snap")
	if err != nil {
		t.Fatal(err)
	}
	snapLinked, err := storeLinked.Create(context.Background(), "linked-snap")
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
			snap, err := stores[i].Create(context.Background(), fmt.Sprintf("concurrent-%d", i))
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
	snap, err := store.Create(context.Background(), "one")
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
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = add ]; then\n  n=$(( $(cat %q 2>/dev/null || echo 0) + 1 ))\n  echo \"$n\" > %q\n  if [ \"$n\" -le %d ]; then printf '%%s\\n' %q >&2; exit 128; fi\nfi\nexec %q \"$@\"\n", counter, counter, failures, stderr, realGit)
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
	snap, err := s.Create(context.Background(), "race")
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
	if _, err := s.Create(context.Background(), "race"); err == nil || !strings.Contains(err.Error(), "failed to insert into database") {
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
	if _, err := s.Create(context.Background(), "locked"); err == nil {
		t.Fatal("create should fail")
	}
	if got := adds(); got != 1 {
		t.Fatalf("git add ran %d times, want 1", got)
	}
}
