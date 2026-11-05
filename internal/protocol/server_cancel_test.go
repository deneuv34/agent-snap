package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	b := newBlocker(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.slow filter=slow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.slow"), []byte("blocks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := exec.Command("git", "config", "filter.slow.clean", b.wait+"; cat")
	config.Dir = dir
	if out, err := config.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	scratch := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, scratch)
	}

	cancel, done := serveInBackground(strings.NewReader(requestLine(1, "snapshot.create", map[string]any{"cwd": dir}) + "\n"))
	defer cancel()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(b.started); err == nil {
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
