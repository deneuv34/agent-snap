package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func casedRepo(t *testing.T, snapshotPath, diskDir string, ignoredInDisk bool) (string, *Store, string) {
	t.Helper()
	dir := testRepo(t)
	git(t, dir, "config", "core.ignorecase", "true")
	writeFile(t, dir, ".gitignore", "*.log\n")
	git(t, dir, "add", ".gitignore")
	git(t, dir, "commit", "-q", "-m", "ignore logs")
	store := newStore(t, dir)
	writeFile(t, dir, snapshotPath, "snapshot\n")
	id := create(t, store, "casespelled").ID
	if err := os.RemoveAll(filepath.Join(dir, filepath.FromSlash(firstSegment(snapshotPath)))); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(diskDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if ignoredInDisk {
		writeFile(t, dir, diskDir+"/x.log", "ignored\n")
	}
	return dir, store, id
}

func firstSegment(p string) string {
	first, _, _ := strings.Cut(p, "/")
	return first
}

func TestPlanRestoreBlocksAWriteBelowAParentSpelledByCaseOnDisk(t *testing.T) {
	cases := []struct {
		name         string
		snapshotPath string
		diskDir      string
	}{
		{"lower in the snapshot, upper on disk", "dir/y", "Dir"},
		{"upper in the snapshot, lower on disk", "Dir/y", "dir"},
		{"a deeper parent", "a/dir/y", "a/Dir"},
		{"the top parent of a deeper path", "a/dir/y", "A/dir"},
	}
	for _, c := range cases {
		for _, ignored := range []bool{true, false} {
			for _, scoped := range []bool{false, true} {
				name := c.name + map[bool]string{true: " with an ignored file", false: " empty"}[ignored] + map[bool]string{true: " scoped", false: " whole"}[scoped]
				t.Run(name, func(t *testing.T) {
					dir, store, id := casedRepo(t, c.snapshotPath, c.diskDir, ignored)
					scope := WholeTree()
					if scoped {
						scope = scopeOf(t, c.snapshotPath)
					}
					plan := planAndCheck(t, dir, store, id, PlanQuery{Scope: scope})
					if got := entryAt(t, plan, c.snapshotPath).Blocker; got != BlockNameCollision {
						t.Fatalf("%s: blocker %q, want %q", c.snapshotPath, got, BlockNameCollision)
					}
					if plan.Applicable {
						t.Fatal("the plan is applicable")
					}
				})
			}
		}
	}
}

func TestPlanRestoreDoesNotBlockAWriteBelowAParentTheRestoreEmpties(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{true: "scoped", false: "whole"}[scoped], func(t *testing.T) {
			dir := testRepo(t)
			git(t, dir, "config", "core.ignorecase", "true")
			store := newStore(t, dir)
			writeFile(t, dir, "dir/a", "snapshot\n")
			writeFile(t, dir, "dir/sub/b", "snapshot\n")
			id := create(t, store, "before the rename").ID
			if err := os.RemoveAll(filepath.Join(dir, "dir")); err != nil {
				t.Fatal(err)
			}
			writeFile(t, dir, "Dir/a", "current\n")
			writeFile(t, dir, "Dir/sub/b", "current\n")

			scope := WholeTree()
			if scoped {
				scope = scopeOf(t, "dir/a", "dir/sub/b", "Dir/a", "Dir/sub/b")
			}
			plan := planAndCheck(t, dir, store, id, PlanQuery{Scope: scope})
			for _, path := range []string{"dir/a", "dir/sub/b"} {
				if got := entryAt(t, plan, path).Blocker; got != "" {
					t.Errorf("%s: blocker %q, want none", path, got)
				}
			}
		})
	}
}

func TestPlanRestoreBlocksTheDirectoryAParentSpelledByCaseKeepsAnIgnoredFileIn(t *testing.T) {
	dir := testRepo(t)
	git(t, dir, "config", "core.ignorecase", "true")
	writeFile(t, dir, ".gitignore", "*.log\n")
	git(t, dir, "add", ".gitignore")
	git(t, dir, "commit", "-q", "-m", "ignore logs")
	store := newStore(t, dir)
	writeFile(t, dir, "dir/a", "snapshot\n")
	id := create(t, store, "before the rename").ID
	if err := os.RemoveAll(filepath.Join(dir, "dir")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "Dir/a", "current\n")
	writeFile(t, dir, "Dir/x.log", "ignored\n")

	plan := planAndCheck(t, dir, store, id, PlanQuery{Scope: WholeTree()})
	if got := entryAt(t, plan, "dir/a").Blocker; got != BlockNameCollision {
		t.Fatalf("dir/a: blocker %q, want %q", got, BlockNameCollision)
	}
}

func TestPlanRestoreDoesNotBlockAWriteWhenTheTreeDoesNotFoldCase(t *testing.T) {
	dir, store, id := casedRepo(t, "dir/y", "Dir", false)
	if foldsCase(t, dir) {
		t.Skip("the volume that holds the test repository folds case, so core.ignorecase=false cannot hold")
	}
	git(t, dir, "config", "core.ignorecase", "false")
	plan := planAndCheck(t, dir, store, id, PlanQuery{Scope: WholeTree()})
	if got := entryAt(t, plan, "dir/y").Blocker; got != "" {
		t.Fatalf("dir/y: blocker %q, want none", got)
	}
}
