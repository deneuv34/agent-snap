package snapshot

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// hooksPathFor puts a git wrapper first on PATH that points the hooks path
// the Store pins at hooks, so a test can run a hook inside a create.
func hooksPathFor(t *testing.T, hooks string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nfor a; do\n  shift\n  case $a in core.hooksPath=*) a=core.hooksPath=%q ;; esac\n  set -- \"$@\" \"$a\"\ndone\nexec %q \"$@\"\n", hooks, realGit)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCreateCancelledWhileGitHoldsRefLocksLeavesNoLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM, so cancellation kills Git")
	}
	dir := testRepo(t)
	writeFile(t, dir, "file.txt", "changed\n")
	signals := t.TempDir()
	started, release := filepath.Join(signals, "started"), filepath.Join(signals, "release")
	hook := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = prepared ]; then touch %q; while [ ! -e %q ]; do sleep 0.05; done; fi\n", started, release)
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	hooksPathFor(t, hooks)
	defer func() { _ = os.WriteFile(release, nil, 0o644) }()
	store := newStore(t, dir)
	root := scratchRoot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := store.Create(ctx, "locked", Origin{})
		done <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reference-transaction hook never started")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled create must fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled create did not return while the hook still ran")
	}

	var locks []string
	_ = filepath.WalkDir(filepath.Join(dir, ".git"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, ".lock") {
			locks = append(locks, path)
		}
		return nil
	})
	if len(locks) > 0 {
		t.Fatalf("the cancelled create left lock files %v", locks)
	}
	if refs := git(t, dir, "for-each-ref", refsPrefix); refs != "" {
		t.Fatalf("the cancelled create landed refs %q", refs)
	}
	assertEmpty(t, root, "a cancelled create")
}

func TestCreateCancelledWhileWaitingOnARefLockReturnsPromptly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM, so cancellation kills Git and leaves its item lock")
	}
	dir := testRepo(t)
	store := newStore(t, dir)
	create(t, store, "first")
	writeFile(t, dir, "tracked.txt", "changed\n")
	planted := filepath.Join(dir, ".git", store.latestRef()+".lock")
	writeFile(t, dir, filepath.Join(".git", store.latestRef()+".lock"), "")
	items := filepath.Join(dir, ".git", store.itemsRef())
	refsBefore := git(t, dir, "for-each-ref", refsPrefix)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := store.Create(ctx, "waiting", Origin{})
		done <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if held, _ := filepath.Glob(filepath.Join(items, "*.lock")); len(held) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Git never took the item lock while it waited on latest")
		}
	}
	time.Sleep(50 * time.Millisecond)
	cancelled := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled create must fail")
		}
		waited := time.Since(cancelled)
		if waited > 100*time.Millisecond {
			t.Fatalf("the cancelled create returned %s after the cancel", waited)
		}
		t.Logf("the cancelled create returned %s after the cancel", waited)
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled create did not return")
	}
	if held, _ := filepath.Glob(filepath.Join(items, "*.lock")); len(held) > 0 {
		t.Fatalf("the cancelled create left item locks %v", held)
	}
	if _, err := os.Stat(planted); err != nil {
		t.Fatalf("the cancelled create touched the planted lock: %v", err)
	}
	if refs := git(t, dir, "for-each-ref", refsPrefix); refs != refsBefore {
		t.Fatalf("the cancelled create changed refs from %q to %q", refsBefore, refs)
	}
}
