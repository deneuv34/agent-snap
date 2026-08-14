package snapshot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCreateRejectsSymbolicSnapshotRefs(t *testing.T) {
	dir := testRepo(t)
	headBefore := git(t, dir, "rev-parse", "HEAD")

	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// An attacker (or accident) points our latest ref at a real branch.
	git(t, dir, "symbolic-ref", store.latestRef(), "refs/heads/main")

	if _, err := store.Create(context.Background(), "must-refuse"); err == nil || !strings.Contains(err.Error(), "symbolic") {
		t.Fatalf("create must refuse a symbolic snapshot ref, got: %v", err)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD moved: got %s want %s", got, headBefore)
	}
	if got := git(t, dir, "rev-parse", "refs/heads/main"); got != headBefore {
		t.Fatalf("branch moved: got %s want %s", got, headBefore)
	}

	// After removing the symref, creates work again.
	git(t, dir, "update-ref", "--no-deref", "-d", store.latestRef())
	if _, err := store.Create(context.Background(), "after-cleanup"); err != nil {
		t.Fatalf("create after removing symref: %v", err)
	}
	if got := git(t, dir, "rev-parse", "refs/heads/main"); got != headBefore {
		t.Fatalf("branch moved during recovery: got %s want %s", got, headBefore)
	}
}

func TestScratchIndexNeverInsideRepo(t *testing.T) {
	dir := testRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Every temp override points INSIDE the repository: the scratch index and
	// any git child temp files must never end up in the worktree (or they
	// would be captured into the snapshot itself).
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)

	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create(context.Background(), "no-self-capture")
	if err != nil {
		t.Fatal(err)
	}

	out := git(t, dir, "ls-tree", "-r", "--name-only", snap.Commit)
	if strings.Contains(out, "agent-snap-index-") {
		t.Fatalf("scratch index captured into the snapshot: %s", out)
	}
	for _, name := range strings.Split(out, "\n") {
		if strings.Contains(name, "agent-snap-index-") {
			t.Fatalf("scratch artifact in tree: %q", name)
		}
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

func TestSha256Repositories(t *testing.T) {
	dir := t.TempDir()
	init := exec.Command("git", "init", "--object-format=sha256", "-b", "main")
	init.Dir = dir
	init.Env = sanitizedEnv()
	if out, err := init.CombinedOutput(); err != nil {
		t.Skipf("git without sha256 support: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "file.txt")
	git(t, dir, "commit", "-m", "initial")

	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(context.Background(), "sha256-one")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Commit) != 64 {
		t.Fatalf("expected 64-char sha256 commit, got %d chars: %s", len(first.Commit), first.Commit)
	}
	second, err := store.Create(context.Background(), "sha256-two")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused || second.Commit != first.Commit {
		t.Fatalf("dedup must work on sha256: %+v", second)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := store.Create(context.Background(), "sha256-three")
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
	if items[0].Commit != third.Commit {
		t.Fatalf("unexpected newest snapshot: %+v", items[0])
	}
}

func TestRootSnapshotListsAllFiles(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.ChangedFiles) != 2 {
		t.Fatalf("root snapshot must list all files, got: %+v", snap.ChangedFiles)
	}
	found := map[string]bool{}
	for _, name := range snap.ChangedFiles {
		found[name] = true
	}
	if !found["a.txt"] || !found["b.txt"] {
		t.Fatalf("missing files in root snapshot: %+v", snap.ChangedFiles)
	}
}

func TestChangedFilesPreserveLeadingWhitespaceAndReasonTerminalNewlines(t *testing.T) {
	dir := testRepo(t)
	spaced := "  spaced.txt"
	if err := os.WriteFile(filepath.Join(dir, spaced), []byte("odd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reason := "term\n"
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create(context.Background(), reason)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range snap.ChangedFiles {
		if name == spaced {
			found = true
		}
	}
	if !found {
		t.Fatalf("leading-whitespace filename corrupted: %+v", snap.ChangedFiles)
	}
	items, err := store.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Reason != reason {
		t.Fatalf("terminal-newline reason not preserved: got %q want %q", items[0].Reason, reason)
	}
}

func TestRepoPathWithTrailingWhitespace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows strips trailing spaces from directory names")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "repo-with-trailing-space ")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "file.txt")
	git(t, dir, "commit", "-m", "initial")

	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	gotRepo, err := filepath.EvalSymlinks(store.Repo())
	if err != nil {
		t.Fatal(err)
	}
	wantRepo, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if gotRepo != wantRepo {
		t.Fatalf("repo path mangled: got %q want %q", store.Repo(), dir)
	}
	snap, err := store.Create(context.Background(), "trailing-space-repo")
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, dir, "show", fmt.Sprintf("%s:file.txt", snap.Commit)); got != "content" {
		t.Fatalf("content mismatch: %q", got)
	}
}
