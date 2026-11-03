package snapshot

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCreateCancelledWhileGitHoldsRefLocksLeavesNoLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM, so cancellation kills Git")
	}
	dir := testRepo(t)
	writeFile(t, dir, "file.txt", "changed\n")
	signals := t.TempDir()
	started, release := filepath.Join(signals, "started"), filepath.Join(signals, "release")
	hook := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = prepared ]; then touch %q; while [ ! -e %q ]; do sleep 0.05; done; fi\n", started, release)
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "reference-transaction"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
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
