package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

func neverHeld(string) bool { return false }

func TestCaseSpellingReadsEachDirectoryOnce(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "Dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	probe := newDiskProbe(root, nil, true)
	paths := []string{"a/dir/y", "a/dir/z", "a/dir/sub/w"}
	for _, path := range paths {
		if got := probe.caseSpelling(path, neverHeld); got != spellingByCase {
			t.Fatalf("%s: %v, want spellingByCase", path, got)
		}
	}
	if err := os.RemoveAll(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if got := probe.caseSpelling(path, neverHeld); got != spellingByCase {
			t.Errorf("%s after the directory is gone: %v, want the first reading", path, got)
		}
	}
}

func TestCaseSpellingReadsNothingWhenTheTreeDoesNotFoldCase(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	probe := newDiskProbe(root, nil, false)
	if got := probe.caseSpelling("dir/y", neverHeld); got != spellingClear {
		t.Fatalf("got %v, want spellingClear", got)
	}
	if probe.listed != nil {
		t.Fatal("the probe kept directory listings")
	}
}

func TestCaseSpellingLeavesTheExemptionsToTheirRules(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Held", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "exact"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "File"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(root, "Link"))
	probe := newDiskProbe(root, nil, true)
	held := func(dir string) bool { return dir == "held" }
	cases := []struct {
		path string
		want caseSpelling
	}{
		{"exact/y", spellingClear},
		{"held/y", spellingClear},
		{"held/inner/y", spellingClear},
		{"file/y", spellingClear},
		{"link/target/y", spellingClear},
		{"held/Inner/y", spellingByCase},
		{"held/inner/z/y", spellingClear},
	}
	for _, c := range cases {
		if got := probe.caseSpelling(c.path, held); got != c.want {
			t.Errorf("%s: %v, want %v", c.path, got, c.want)
		}
	}
	if got := probe.caseSpelling("held/inner/y", func(string) bool { return false }); got != spellingByCase {
		t.Errorf("held/inner/y with no held directory: %v, want spellingByCase", got)
	}
}

func TestCaseSpellingReportsAnUnreadableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop this user")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "a"), 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "a"), 0o755) })
	probe := newDiskProbe(root, nil, true)
	if got := probe.caseSpelling("a/dir/y", neverHeld); got != spellingUnreadable {
		t.Fatalf("got %v, want spellingUnreadable", got)
	}
}

func TestPlanRestoreBlocksAWriteBelowACaseSpelledParentUnlessAHEADPathTheRestoreKeepsSpellsIt(t *testing.T) {
	type scoped struct {
		paths []string
		want  PathBlocker
	}
	cases := []struct {
		name               string
		head, snap, onDisk map[string]string
		scopes             []scoped
	}{
		{
			"the restore deletes the only HEAD path below the parent",
			map[string]string{"dir/a": "head\n"},
			map[string]string{"dir/y": "snapshot\n"},
			map[string]string{"Dir/a": "head\n", "Dir/x.log": "ignored\n"},
			[]scoped{{nil, BlockNameCollision}, {[]string{"dir/y", "dir/a"}, BlockNameCollision}, {[]string{"dir/y"}, ""}},
		},
		{
			"an untracked path below the parent stays",
			map[string]string{"dir/a": "head\n"},
			map[string]string{"dir/q": "untracked\n", "dir/y": "snapshot\n"},
			map[string]string{"Dir/a": "head\n", "Dir/q": "untracked\n"},
			[]scoped{{nil, BlockNameCollision}, {[]string{"dir/y", "dir/a"}, BlockNameCollision}, {[]string{"dir/y"}, ""}},
		},
		{
			"the restore modifies the HEAD path below the parent",
			map[string]string{"dir/a": "head\n"},
			map[string]string{"dir/a": "snapshot\n", "dir/y": "snapshot\n"},
			map[string]string{"Dir/a": "current\n"},
			[]scoped{{nil, ""}, {[]string{"dir/y", "dir/a"}, ""}},
		},
		{
			"the restore creates the HEAD path below the parent",
			map[string]string{"dir/z": "head\n"},
			map[string]string{"dir/y": "snapshot\n", "dir/z": "head\n"},
			map[string]string{"Dir/x.log": "ignored\n"},
			[]scoped{{nil, ""}, {[]string{"dir/y", "dir/z"}, ""}, {[]string{"dir/y"}, BlockNameCollision}},
		},
	}
	for _, c := range cases {
		for _, s := range c.scopes {
			t.Run(fmt.Sprintf("%s %q", c.name, s.paths), func(t *testing.T) {
				dir := testRepo(t)
				if !foldsCase(t, dir) {
					t.Skip("the current tree spells dir/a over a Dir/ on disk only when the volume folds case")
				}
				git(t, dir, "config", "core.ignorecase", "true")
				writeFile(t, dir, ".gitignore", "*.log\n")
				writeFiles(t, dir, c.head)
				git(t, dir, "add", ".")
				git(t, dir, "commit", "-q", "-m", "head")
				store := newStore(t, dir)
				removeAll(t, dir, "dir")
				writeFiles(t, dir, c.snap)
				id := create(t, store, "snapshot").ID
				removeAll(t, dir, "dir")
				writeFiles(t, dir, c.onDisk)

				scope := WholeTree()
				if s.paths != nil {
					scope = scopeOf(t, s.paths...)
				}
				plan := planAndCheck(t, dir, store, id, PlanQuery{Scope: scope})
				if got := entryAt(t, plan, "dir/y").Blocker; got != s.want {
					t.Fatalf("dir/y: blocker %q, want %q", got, s.want)
				}
				applyWithGit(t, dir, plan)
				captured := get(t, store, create(t, store, "after").ID).Tree
				if blocked := s.want != ""; blocked == (captured == plan.ResultTree) {
					t.Fatalf("blocked %v, but Git's apply captures %s and the result tree is %s", blocked, captured, plan.ResultTree)
				}
			})
		}
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		writeFile(t, dir, name, content)
	}
}

func removeAll(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
		t.Fatal(err)
	}
}

func applyWithGit(t *testing.T, dir string, plan RestorePlan) {
	t.Helper()
	env := append(sanitizedEnv(), "GIT_INDEX_FILE="+filepath.Join(t.TempDir(), "index"))
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := gitCommand(dir, env, args...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	var info strings.Builder
	for _, e := range plan.Entries {
		if e.Target == nil {
			fmt.Fprintf(&info, "0 %s\t%s\n", strings.Repeat("0", len(plan.CurrentTree)), e.Path)
		} else {
			fmt.Fprintf(&info, "%s %s\t%s\n", e.Target.Mode, e.Target.OID, e.Path)
		}
	}
	run("", "read-tree", plan.CurrentTree)
	run(info.String(), "update-index", "--index-info")
	if tree := run("", "write-tree"); tree != plan.ResultTree {
		t.Fatalf("the entries build %s, the result tree is %s", tree, plan.ResultTree)
	}
	run("", "read-tree", plan.CurrentTree)
	run("", "update-index", "-q", "--refresh")
	run("", "read-tree", "-m", "-u", plan.CurrentTree, plan.ResultTree)
}

func TestPlanRestoreBlocksAWriteBelowACaseSpelledParentHEADHoldsWhenCoreIgnorecaseIsFalse(t *testing.T) {
	cases := []struct {
		name       string
		head, snap map[string]string
		scopes     [][]string
	}{
		{
			"the restore creates the HEAD path below the parent",
			map[string]string{"dir/z": "head\n"},
			map[string]string{"dir/y": "snapshot\n"},
			[][]string{nil, {"dir/y", "dir/z"}},
		},
		{
			"the restore recreates a HEAD path gone from disk",
			map[string]string{"dir/a": "head\n"},
			map[string]string{"dir/y": "snapshot\n"},
			[][]string{nil, {"dir/a"}},
		},
	}
	for _, c := range cases {
		for _, paths := range c.scopes {
			t.Run(fmt.Sprintf("%s %q", c.name, paths), func(t *testing.T) {
				dir := testRepo(t)
				if !foldsCase(t, dir) {
					t.Skip("Git writes into a parent spelled by case on disk only when the volume folds case")
				}
				git(t, dir, "config", "core.ignorecase", "false")
				writeFile(t, dir, ".gitignore", "*.log\n")
				writeFiles(t, dir, c.head)
				git(t, dir, "add", ".")
				git(t, dir, "commit", "-q", "-m", "head")
				store := newStore(t, dir)
				writeFiles(t, dir, c.snap)
				id := create(t, store, "snapshot").ID
				removeAll(t, dir, "dir")
				writeFile(t, dir, "Dir/x.log", "ignored\n")

				scope := WholeTree()
				if paths != nil {
					scope = scopeOf(t, paths...)
				}
				plan := planAndCheck(t, dir, store, id, PlanQuery{Scope: scope})
				if len(plan.Entries) == 0 {
					t.Fatal("the plan has no entries")
				}
				for _, e := range plan.Entries {
					if e.Blocker != BlockNameCollision {
						t.Errorf("%s: blocker %q, want %q", e.Path, e.Blocker, BlockNameCollision)
					}
				}
				applyWithGit(t, dir, plan)
				if captured := get(t, store, create(t, store, "after").ID).Tree; captured == plan.ResultTree {
					t.Fatal("Git's apply captures the result tree, so the block is not needed")
				}
			})
		}
	}
}
