//go:build !windows

package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRetentionReportReadsAtMost4096BytesOfAGitdirFile(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		create(t, store, "main")
		stores := map[string]*Store{}
		paths := map[string]string{}
		for _, name := range []string{"intact", "large", "fifo"} {
			paths[name] = addWorktree(t, dir, name)
			stores[name] = newStore(t, paths[name])
			writeFile(t, paths[name], name+".txt", name+"\n")
			create(t, stores[name], name)
		}
		entryDir := func(name string) string {
			return filepath.Join(commonDirOf(t, dir), "worktrees", name)
		}

		gitdir := filepath.Join(entryDir("large"), "gitdir")
		original, err := os.ReadFile(gitdir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gitdir, []byte(strings.TrimSpace(string(original))+strings.Repeat("a", 5000)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		report := reportOf(t, store)
		if got := entryFor(t, report, stores["large"].ns); got.Worktree != "" || got.Snapshots != 1 {
			t.Fatalf("a gitdir file past 4096 bytes maps its namespace with no path: %+v", got)
		}
		if got := entryFor(t, report, stores["intact"].ns).Worktree; got != realPath(t, paths["intact"]) {
			t.Fatalf("an intact entry must still map: %q", got)
		}
		if got := entryFor(t, report, store.ns).Worktree; got != realPath(t, dir) {
			t.Fatalf("the main worktree's path: %q", got)
		}

		fifo := filepath.Join(entryDir("fifo"), "gitdir")
		if err := os.Remove(fifo); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Skipf("no FIFOs: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		done := make(chan struct{})
		var blocked RetentionReport
		var blockedErr error
		go func() {
			defer close(done)
			blocked, blockedErr = store.RetentionReport(ctx, 0)
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			cancel()
			<-done
			t.Fatalf("the report blocked on a FIFO gitdir file: %v", blockedErr)
		}
		if blockedErr != nil {
			t.Fatalf("report with a FIFO gitdir file: %v", blockedErr)
		}
		if got := entryFor(t, blocked, stores["fifo"].ns); got.Worktree != "" || got.Snapshots != 1 {
			t.Fatalf("a FIFO gitdir file maps its namespace with no path: %+v", got)
		}
		if got := entryFor(t, blocked, stores["intact"].ns).Worktree; got != realPath(t, paths["intact"]) {
			t.Fatalf("an intact entry must still map beside a FIFO entry: %q", got)
		}
		if got := entryFor(t, blocked, store.ns).Worktree; got != "" {
			t.Fatalf("git worktree list blocks on a FIFO entry, so the main worktree has no path then: %q", got)
		}
	})
}
