package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func redirectPinnedHooksPath(t *testing.T, hooks string) {
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

// A blocker's wait is shell code that touches started and then waits until
// release exists or the test process exits. Both files are in dir.
type blocker struct{ dir, wait, started, release string }

// The wait watches the test process because cancellation stops Git and not
// the hook or filter that Git runs, and a test that times out or is killed
// runs no cleanup. The cleanup writes release before t.TempDir removes dir,
// then fails the test and kills any process that names dir 5 seconds later.
func newBlocker(t *testing.T) blocker {
	t.Helper()
	dir := t.TempDir()
	b := blocker{dir: dir, started: filepath.Join(dir, "started"), release: filepath.Join(dir, "release")}
	b.wait = fmt.Sprintf("touch %q; while [ ! -e %q ] && kill -0 %d 2>/dev/null; do sleep 0.05; done", b.started, b.release, os.Getpid())
	t.Cleanup(func() {
		if err := os.WriteFile(b.release, nil, 0o644); err != nil {
			t.Errorf("release the blocking shell: %v", err)
		}
		pattern := regexp.QuoteMeta(dir)
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			out, err := exec.Command("pgrep", "-f", pattern).Output()
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return
			}
			if err != nil {
				t.Errorf("list the processes naming %s: %v", dir, err)
				return
			}
			if time.Now().After(deadline) {
				_ = exec.Command("pkill", "-KILL", "-f", pattern).Run()
				t.Errorf("processes %v naming %s outlived the test", strings.Fields(string(out)), dir)
				return
			}
		}
	})
	return b
}

func TestCreateCancelledWhileGitHoldsRefLocksLeavesNoLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM, so cancellation kills Git")
	}
	dir := testRepo(t)
	writeFile(t, dir, "file.txt", "changed\n")
	// The hook lives in the blocker's directory, so the process check finds
	// both the hook and the Git that names the directory as its hooks path.
	b := newBlocker(t)
	hook := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = prepared ]; then %s; fi\n", b.wait)
	if err := os.WriteFile(filepath.Join(b.dir, "reference-transaction"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	redirectPinnedHooksPath(t, b.dir)
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
		if _, err := os.Stat(b.started); err == nil {
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
