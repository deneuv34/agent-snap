package snapshot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(sanitizedEnv(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func testRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "tracked.txt")
	git(t, dir, "commit", "-m", "initial")
	return dir
}

func TestCreateCapturesWorkingTreeWithoutChangingHeadOrIndex(t *testing.T) {
	dir := testRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("working\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "staged.txt")

	headBefore := git(t, dir, "rev-parse", "HEAD")
	indexBefore := git(t, dir, "write-tree")

	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create(context.Background(), "turn-end", Origin{})
	if err != nil {
		t.Fatal(err)
	}

	if snap.ID == "" || snap.Commit == "" {
		t.Fatalf("missing snapshot identity: %+v", snap)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD changed: got %s want %s", got, headBefore)
	}
	if got := git(t, dir, "write-tree"); got != indexBefore {
		t.Fatalf("index changed: got %s want %s", got, indexBefore)
	}
	if got := git(t, dir, "show", snap.Commit+":tracked.txt"); got != "working" {
		t.Fatalf("tracked content not captured: %q", got)
	}
	if got := git(t, dir, "show", snap.Commit+":untracked.txt"); got != "new" {
		t.Fatalf("untracked content not captured: %q", got)
	}
}

func TestCreateDeduplicatesUnchangedTreeAndListReturnsNewestFirst(t *testing.T) {
	dir := testRepo(t)
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(context.Background(), "baseline", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(context.Background(), "idle", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused || second.Commit != first.Commit {
		t.Fatalf("expected unchanged tree reuse; first=%+v second=%+v", first, second)
	}

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := store.Create(context.Background(), "after-edit", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d snapshots, want 2: %+v", len(items), items)
	}
	if items[0].Commit != third.Commit || items[1].Commit != first.Commit {
		t.Fatalf("unexpected order: %+v", items)
	}
}
