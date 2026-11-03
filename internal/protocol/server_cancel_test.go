package protocol

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func serveInBackground(in io.Reader) (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, io.Discard) }()
	return cancel, done
}

func serveResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its context ended")
		return nil
	}
}

func TestServeReturnsWhenItsContextEndsWhileIdle(t *testing.T) {
	in, writer := io.Pipe()
	defer writer.Close()
	cancel, done := serveInBackground(in)
	cancel()
	if err := serveResult(t, done); err != nil {
		t.Fatalf("an idle Serve must stop cleanly, got: %v", err)
	}
}

func TestServeCancelledMidCreateRemovesItsScratchDirectoryAndLandsNoRef(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the filter is a POSIX command")
	}
	dir := protocolRepo(t)
	signals := t.TempDir()
	started, release := filepath.Join(signals, "started"), filepath.Join(signals, "release")
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.slow filter=slow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.slow"), []byte("blocks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := exec.Command("git", "config", "filter.slow.clean", fmt.Sprintf("touch %q; while [ ! -e %q ]; do sleep 0.05; done; cat", started, release))
	config.Dir = dir
	if out, err := config.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	scratch := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, scratch)
	}
	defer func() { _ = os.WriteFile(release, nil, 0o644) }()

	cancel, done := serveInBackground(strings.NewReader(requestLine(1, "snapshot.create", map[string]any{"cwd": dir}) + "\n"))
	defer cancel()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the clean filter never started")
		}
	}
	cancel()
	if err := serveResult(t, done); err != nil {
		t.Fatalf("a cancelled Serve must stop cleanly, got: %v", err)
	}
	if entries, err := os.ReadDir(scratch); err != nil || len(entries) > 0 {
		t.Fatalf("the cancelled create left %v in the scratch root (read error %v)", entries, err)
	}
	refs := exec.Command("git", "for-each-ref", "refs/agent-snapshots/")
	refs.Dir = dir
	out, err := refs.Output()
	if err != nil || len(out) > 0 {
		t.Fatalf("the cancelled create landed refs %q (error %v)", out, err)
	}
}
