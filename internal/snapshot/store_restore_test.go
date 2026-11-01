package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func planAndCheck(t *testing.T, dir string, store *Store, id string, q PlanQuery) RestorePlan {
	t.Helper()
	gitDir, worktree := gitDirFiles(t, dir), worktreeFiles(t, dir)
	plan, err := store.PlanRestore(context.Background(), id, q)
	if err != nil {
		t.Fatalf("plan %s: %v", id, err)
	}
	if changes := gitDirChanges(gitDir, gitDirFiles(t, dir)); len(changes) > 0 {
		t.Fatalf("the plan changed the Git directory:\n%s", strings.Join(changes, "\n"))
	}
	if !maps.Equal(worktree, worktreeFiles(t, dir)) {
		t.Fatal("the plan changed the working tree")
	}
	if tree := get(t, store, create(t, store, "after the plan").ID).Tree; tree != plan.CurrentTree {
		t.Fatalf("a create after the plan recorded %s, the plan's current tree is %s", tree, plan.CurrentTree)
	}
	return plan
}

func worktreeFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == ".git" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			files[rel] = "directory"
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			files[rel] = "link to " + target
		default:
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[rel] = fmt.Sprintf("%v %x", info.Mode(), sha256.Sum256(content))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func scopeOf(t *testing.T, paths ...string) Scope {
	t.Helper()
	scope, err := ParseScope(paths)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func gitCommand(dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd
}

func side(mode, oid string) *PlanSide { return &PlanSide{Mode: mode, OID: oid} }

func assertEntries(t *testing.T, got, want []PlanEntry) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries:\n got %s\nwant %s", entriesJSON(got), entriesJSON(want))
	}
}

func entriesJSON(entries []PlanEntry) string {
	var b strings.Builder
	for _, e := range entries {
		out, _ := json.Marshal(e)
		b.WriteString("\n  " + string(out))
	}
	return b.String()
}

func TestPlanRestoreOfAnUnchangedTreeIsEmptyAndApplicable(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			writeFile(t, dir, "untracked.txt", "kept\n")
			store := newStore(t, dir)
			snap := create(t, store, "snap")
			tree := get(t, store, snap.ID).Tree

			plan := planAndCheck(t, dir, store, snap.ID, PlanQuery{})
			want := RestorePlan{
				Snapshot:    PlanSnapshot{ID: snap.ID, Commit: snap.Commit, Tree: tree},
				CurrentTree: tree, ResultTree: tree, Applicable: true,
				Blockers: []RepoBlocker{}, Unmatched: []string{}, Entries: []PlanEntry{},
			}
			if !reflect.DeepEqual(plan, want) {
				t.Fatalf("plan:\n got %+v\nwant %+v", plan, want)
			}
			out, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{`"blockers":[]`, `"unmatched":[]`, `"entries":[]`, `"applicable":true`} {
				if !strings.Contains(string(out), field) {
					t.Errorf("JSON lacks %s: %s", field, out)
				}
			}
			for _, field := range []string{`"paths"`, `"next_offset"`} {
				if strings.Contains(string(out), field) {
					t.Errorf("JSON of a full-tree last page holds %s: %s", field, out)
				}
			}
		})
	}
}

func TestPlanRestoreReportsEachActionWithItsSides(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			store := newStore(t, dir)
			snapshot := snapshotOfTree(t, dir, store, treeOf(t, dir,
				fileEntry(t, dir, modeFile, "tracked.txt", "initial\n"),
				fileEntry(t, dir, modeExecutable, "exe.sh", "run\n"),
				fileEntry(t, dir, modeFile, "mod.txt", "old\n"),
				fileEntry(t, dir, modeFile, "gone.txt", "gone\n"),
				fileEntry(t, dir, modeSymlink, "link", "target"),
				fileEntry(t, dir, modeFile, "d", "a file\n"),
			))
			for name, content := range map[string]string{"exe.sh": "run\n", "mod.txt": "new\n", "new.txt": "new\n", "link": "plain\n", "d/x": "below\n"} {
				writeFile(t, dir, name, content)
			}

			plan := planAndCheck(t, dir, store, snapshot, PlanQuery{})
			oid := func(content string) string { return blobOf(t, dir, content) }
			assertEntries(t, plan.Entries, []PlanEntry{
				{Path: "d", Action: ActionCreate, Target: side(modeFile, oid("a file\n"))},
				{Path: "d/x", Action: ActionDelete, Current: side(modeFile, oid("below\n")), Untracked: true},
				{Path: "exe.sh", Action: ActionModify, Current: side(modeFile, oid("run\n")), Target: side(modeExecutable, oid("run\n")), Untracked: true},
				{Path: "gone.txt", Action: ActionCreate, Target: side(modeFile, oid("gone\n"))},
				{Path: "link", Action: ActionTypeChange, Current: side(modeFile, oid("plain\n")), Target: side(modeSymlink, oid("target")), Untracked: true},
				{Path: "mod.txt", Action: ActionModify, Current: side(modeFile, oid("new\n")), Target: side(modeFile, oid("old\n")), Untracked: true},
				{Path: "new.txt", Action: ActionDelete, Current: side(modeFile, oid("new\n")), Untracked: true},
			})
			want := PlanTotals{Entries: 7, Create: 2, Modify: 2, TypeChange: 1, Delete: 2, Untracked: 5}
			if plan.Totals != want || !plan.Applicable {
				t.Fatalf("totals %+v applicable %v, want %+v and applicable", plan.Totals, plan.Applicable, want)
			}
		})
	}
}

func TestPlanRestoreSetsTheFlagsFromHeadTheIndexAndTheWorkingTree(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			for _, name := range []string{"staged", "unstaged", "both", "removed", "deleted"} {
				writeFile(t, dir, name+".txt", "v1\n")
			}
			git(t, dir, "add", "-A")
			git(t, dir, "commit", "-m", "flags")
			store := newStore(t, dir)
			snap := create(t, store, "before")

			writeFile(t, dir, "staged.txt", "v2\n")
			git(t, dir, "add", "staged.txt")
			writeFile(t, dir, "unstaged.txt", "v2\n")
			writeFile(t, dir, "both.txt", "v2\n")
			git(t, dir, "add", "both.txt")
			writeFile(t, dir, "both.txt", "v3\n")
			git(t, dir, "rm", "--cached", "-q", "removed.txt")
			writeFile(t, dir, "removed.txt", "v2\n")
			if err := os.Remove(filepath.Join(dir, "deleted.txt")); err != nil {
				t.Fatal(err)
			}
			writeFile(t, dir, "ita.txt", "v1\n")
			git(t, dir, "add", "-N", "ita.txt")
			writeFile(t, dir, "untracked.txt", "v1\n")

			plan := planAndCheck(t, dir, store, snap.ID, PlanQuery{})
			type flags struct{ staged, unstaged, untracked bool }
			want := map[string]flags{
				"both.txt":      {staged: true, unstaged: true},
				"deleted.txt":   {unstaged: true},
				"ita.txt":       {staged: true, unstaged: true},
				"removed.txt":   {staged: true, untracked: true},
				"staged.txt":    {staged: true},
				"unstaged.txt":  {unstaged: true},
				"untracked.txt": {untracked: true},
			}
			got := map[string]flags{}
			for _, e := range plan.Entries {
				got[e.Path] = flags{e.Staged, e.Unstaged, e.Untracked}
				if e.Unstaged && e.Untracked {
					t.Errorf("%s is both unstaged and untracked", e.Path)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("flags:\n got %+v\nwant %+v", got, want)
			}
			if plan.Totals.Staged != 4 || plan.Totals.Unstaged != 4 || plan.Totals.Untracked != 2 {
				t.Fatalf("totals %+v", plan.Totals)
			}
		})
	}
}

func dirEntry(name, tree string) string { return "040000 tree " + tree + "\t" + name }

func entryPaths(entries []PlanEntry) []string {
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
	}
	return paths
}

func entryAt(t *testing.T, plan RestorePlan, path string) PlanEntry {
	t.Helper()
	for _, e := range plan.Entries {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no entry for %q in %v", path, entryPaths(plan.Entries))
	return PlanEntry{}
}

func TestPlanRestoreListsEntriesInByteOrder(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snapshot := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, modeFile, "tracked.txt", "initial\n"),
		fileEntry(t, dir, modeFile, "B", "1\n"),
		fileEntry(t, dir, modeFile, "a b", "2\n"),
		fileEntry(t, dir, modeFile, "a-b", "3\n"),
		fileEntry(t, dir, modeFile, "a.b", "4\n"),
		dirEntry("a", treeOf(t, dir, fileEntry(t, dir, modeFile, "x", "5\n"))),
		fileEntry(t, dir, modeFile, "a0", "6\n"),
	))
	plan := planAndCheck(t, dir, store, snapshot, PlanQuery{})
	if got, want := entryPaths(plan.Entries), []string{"B", "a b", "a-b", "a.b", "a/x", "a0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("entries in %q, want byte order %q", got, want)
	}
}

func TestPlanRestoreReportsARenameAsADeleteAndACreate(t *testing.T) {
	dir := testRepo(t)
	writeFile(t, dir, "old.txt", "moved\n")
	store := newStore(t, dir)
	snap := create(t, store, "before the move")
	if err := os.Rename(filepath.Join(dir, "old.txt"), filepath.Join(dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	plan := planAndCheck(t, dir, store, snap.ID, PlanQuery{})
	blob := blobOf(t, dir, "moved\n")
	assertEntries(t, plan.Entries, []PlanEntry{
		{Path: "new.txt", Action: ActionDelete, Current: side(modeFile, blob), Untracked: true},
		{Path: "old.txt", Action: ActionCreate, Target: side(modeFile, blob)},
	})
}

func TestPlanRestoreOnAnUnbornBranchComparesTheIndexWithAnEmptyHead(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := emptyRepoIn(t, format)
			store := newStore(t, dir)
			snapshot := snapshotOfTree(t, dir, store, treeOf(t, dir))
			writeFile(t, dir, "added.txt", "added\n")
			writeFile(t, dir, "loose.txt", "loose\n")
			git(t, dir, "add", "added.txt")

			plan := planAndCheck(t, dir, store, snapshot, PlanQuery{})
			assertEntries(t, plan.Entries, []PlanEntry{
				{Path: "added.txt", Action: ActionDelete, Current: side(modeFile, blobOf(t, dir, "added\n")), Staged: true},
				{Path: "loose.txt", Action: ActionDelete, Current: side(modeFile, blobOf(t, dir, "loose\n")), Untracked: true},
			})
		})
	}
}

func scopedTree(t *testing.T, dir, current, snapshot string, paths ...string) string {
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
		return string(out)
	}
	run("", "read-tree", current)
	run("", append([]string{"rm", "-r", "-q", "--cached", "--ignore-unmatch", "--"}, paths...)...)
	var info strings.Builder
	for _, record := range records(run("", append([]string{"ls-tree", "-r", "-z", snapshot, "--"}, paths...)...)) {
		meta, path, _ := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		fmt.Fprintf(&info, "%s %s\t%s\x00", fields[0], fields[2], path)
	}
	run(info.String(), "update-index", "-z", "--index-info")
	return strings.TrimSpace(run("", "write-tree"))
}

func TestPlanRestoreScopedResultTreeTakesTheSnapshotOnlyInsideTheScope(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			for _, name := range []string{"src/a.go", "src/b.go", "src/deep/c.go", "docs/x.md", "other.txt", "srcx"} {
				writeFile(t, dir, name, "v1\n")
			}
			store := newStore(t, dir)
			snap := create(t, store, "before")
			for _, name := range []string{"src/a.go", "src/deep/c.go", "docs/x.md", "other.txt", "srcx", "src/new.go"} {
				writeFile(t, dir, name, "v2\n")
			}
			if err := os.Remove(filepath.Join(dir, "src", "b.go")); err != nil {
				t.Fatal(err)
			}

			for _, paths := range [][]string{{"src"}, {"other.txt", "src/deep"}} {
				plan := planAndCheck(t, dir, store, snap.ID, PlanQuery{Scope: scopeOf(t, paths...)})
				if want := scopedTree(t, dir, plan.CurrentTree, snap.Commit, paths...); plan.ResultTree != want {
					t.Errorf("scope %q: result tree %s, git builds %s", paths, plan.ResultTree, want)
				}
				if !reflect.DeepEqual(plan.Paths, paths) || len(plan.Unmatched) != 0 || !plan.Applicable {
					t.Errorf("scope %q: paths %q unmatched %q applicable %v", paths, plan.Paths, plan.Unmatched, plan.Applicable)
				}
				for _, e := range plan.Entries {
					if _, covered := scopeOf(t, paths...).covering(e.Path); !covered {
						t.Errorf("scope %q lists %s, which it does not cover", paths, e.Path)
					}
				}
			}
			plan := planAndCheck(t, dir, store, snap.ID, PlanQuery{Scope: scopeOf(t, "src")})
			if got, want := entryPaths(plan.Entries), []string{"src/a.go", "src/b.go", "src/deep/c.go", "src/new.go"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("entries %q, want %q", got, want)
			}
		})
	}
}

func TestPlanRestoreListsScopePathsThatCoverNothing(t *testing.T) {
	dir := testRepo(t)
	writeFile(t, dir, "src/a.go", "v1\n")
	writeFile(t, dir, "gone.txt", "v1\n")
	store := newStore(t, dir)
	snap := create(t, store, "before")
	writeFile(t, dir, "src/a.go", "v2\n")
	writeFile(t, dir, "fresh.txt", "v1\n")
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	plan := planAndCheck(t, dir, store, snap.ID, PlanQuery{Scope: scopeOf(t, "nowhere", "src", "tracked.txt", "gone.txt", "fresh.txt", "a/b/c")})
	if want := []string{"nowhere", "a/b/c"}; !reflect.DeepEqual(plan.Unmatched, want) {
		t.Fatalf("unmatched %q, want %q in request order", plan.Unmatched, want)
	}
	if plan.Applicable || plan.Totals.Blocked != 0 || len(plan.Blockers) != 0 {
		t.Fatalf("a plan with unmatched paths must not be applicable: %+v", plan)
	}
	if plan.ResultTree == "" {
		t.Fatal("unmatched paths do not stop the result tree")
	}
}

func TestPlanRestoreBlocksAWriteBelowAFileOutsideTheScope(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			writeFile(t, dir, "a", "a file\n")
			store := newStore(t, dir)
			head := git(t, dir, "rev-parse", "HEAD")
			tracked := fileEntry(t, dir, modeFile, "tracked.txt", "initial\n")
			below := snapshotOfTree(t, dir, store, treeOf(t, dir, tracked, dirEntry("a", treeOf(t, dir, fileEntry(t, dir, modeFile, "b", "below\n")))))
			gitlink := snapshotOfTree(t, dir, store, treeOf(t, dir, tracked, dirEntry("a", treeOf(t, dir, modeGitlink+" commit "+head+"\tb"))))

			plan := planAndCheck(t, dir, store, below, PlanQuery{Scope: scopeOf(t, "a/b")})
			if e := entryAt(t, plan, "a/b"); e.Blocker != BlockParentConflict || plan.ResultTree != "" || plan.Applicable {
				t.Fatalf("a/b below the file a: blocker %q, result tree %q, applicable %v", e.Blocker, plan.ResultTree, plan.Applicable)
			}
			masked := planAndCheck(t, dir, store, gitlink, PlanQuery{Scope: scopeOf(t, "a/b")})
			if e := entryAt(t, masked, "a/b"); e.Blocker != BlockSubmodule || masked.ResultTree != "" {
				t.Fatalf("a submodule below the file a: blocker %q, result tree %q", e.Blocker, masked.ResultTree)
			}
			for _, q := range []PlanQuery{{}, {Scope: scopeOf(t, "a")}} {
				covered := planAndCheck(t, dir, store, below, q)
				if covered.ResultTree == "" || covered.Totals.Blocked != 0 {
					t.Fatalf("scope %q covers a: result tree %q, blocked %d", q.Scope.Paths(), covered.ResultTree, covered.Totals.Blocked)
				}
			}
		})
	}
}

func TestPlanRestoreReportsRepositoryBlockersOnceInOrder(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			store := newStore(t, dir)
			snap := create(t, store, "snap")
			plan := func() RestorePlan { return planAndCheck(t, dir, store, snap.ID, PlanQuery{}) }

			for _, name := range operationFiles {
				path := git(t, dir, "rev-parse", "--git-path", name)
				if !filepath.IsAbs(path) {
					path = filepath.Join(dir, path)
				}
				if strings.Contains(name, "-") || name == "sequencer" {
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if got := plan().Blockers; !reflect.DeepEqual(got, []RepoBlocker{BlockOperationInProgress}) {
					t.Errorf("%s: blockers %q", name, got)
				}
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
			}

			blob := blobOf(t, dir, "conflict\n")
			var stages strings.Builder
			for stage := 1; stage <= 3; stage++ {
				fmt.Fprintf(&stages, "100644 %s %d\tconflict.txt\x00", blob, stage)
			}
			cmd := gitCommand(dir, sanitizedEnv(), "update-index", "-z", "--index-info")
			cmd.Stdin = strings.NewReader(stages.String())
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("update-index: %v: %s", err, out)
			}
			writeFile(t, dir, "conflict.txt", "conflict\n")
			unmerged := plan()
			if !reflect.DeepEqual(unmerged.Blockers, []RepoBlocker{BlockUnmergedIndex}) {
				t.Errorf("unmerged index: blockers %q", unmerged.Blockers)
			}
			assertEntries(t, unmerged.Entries, []PlanEntry{
				{Path: "conflict.txt", Action: ActionDelete, Current: side(modeFile, blob)},
			})
			git(t, dir, "config", "core.sparseCheckout", "true")
			writeFile(t, dir, ".git/MERGE_HEAD", git(t, dir, "rev-parse", "HEAD")+"\n")
			all := plan()
			if want := []RepoBlocker{BlockOperationInProgress, BlockUnmergedIndex, BlockSparseCheckout}; !reflect.DeepEqual(all.Blockers, want) || all.Applicable {
				t.Fatalf("blockers %q applicable %v, want %q", all.Blockers, all.Applicable, want)
			}
		})
	}
}

func TestPlanRestoreCountsADanglingOperationLinkAsAnOperation(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "snap")
	if err := os.Symlink("missing", filepath.Join(dir, ".git", "MERGE_HEAD")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if got := planAndCheck(t, dir, store, snap.ID, PlanQuery{}).Blockers; !reflect.DeepEqual(got, []RepoBlocker{BlockOperationInProgress}) {
		t.Fatalf("blockers %q", got)
	}
}

func TestPlanRestoreBlocksPathsTheIndexFlags(t *testing.T) {
	dir := testRepo(t)
	for _, name := range []string{"assumed", "skipped", "both", "plain"} {
		writeFile(t, dir, name+".txt", "v1\n")
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "flags")
	store := newStore(t, dir)
	snap := create(t, store, "before")
	for _, name := range []string{"assumed", "skipped", "both", "plain"} {
		writeFile(t, dir, name+".txt", "v2\n")
	}
	git(t, dir, "update-index", "--assume-unchanged", "assumed.txt", "both.txt")
	git(t, dir, "update-index", "--skip-worktree", "skipped.txt", "both.txt")

	plan := planAndCheck(t, dir, store, snap.ID, PlanQuery{})
	for path, want := range map[string]PathBlocker{"assumed.txt": BlockIndexFlag, "skipped.txt": BlockIndexFlag, "both.txt": BlockIndexFlag, "plain.txt": ""} {
		if got := entryAt(t, plan, path).Blocker; got != want {
			t.Errorf("%s: blocker %q, want %q", path, got, want)
		}
	}
}

func TestPlanRestorePagesTheWholePlan(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "before")
	for i := range 450 {
		writeFile(t, dir, fmt.Sprintf("f%03d", i), "x\n")
	}

	first := planAndCheck(t, dir, store, snap.ID, PlanQuery{})
	pages := []RestorePlan{first}
	for page := first; page.NextOffset != 0 && len(pages) <= 3; {
		page = planAndCheck(t, dir, store, snap.ID, PlanQuery{Offset: page.NextOffset, CurrentTree: first.CurrentTree})
		pages = append(pages, page)
	}
	var got []string
	for i, want := range []struct{ offset, entries, next int }{{0, 200, 200}, {200, 200, 400}, {400, 50, 0}} {
		page := pages[i]
		if page.Offset != want.offset || len(page.Entries) != want.entries || page.NextOffset != want.next || page.Totals != first.Totals {
			t.Fatalf("page %d: offset %d, %d entries, next %d, totals %+v", i, page.Offset, len(page.Entries), page.NextOffset, page.Totals)
		}
		got = append(got, entryPaths(page.Entries)...)
	}
	if len(pages) != 3 || len(got) != 450 || got[0] != "f000" || got[449] != "f449" || first.Totals.Entries != 450 || first.Totals.Delete != 450 {
		t.Fatalf("%d pages, %d entries from %q to %q, totals %+v", len(pages), len(got), got[0], got[len(got)-1], first.Totals)
	}
	for _, offset := range []int{450, MaxPageOffset} {
		past := planAndCheck(t, dir, store, snap.ID, PlanQuery{Offset: offset, CurrentTree: first.CurrentTree})
		if len(past.Entries) != 0 || past.Entries == nil || past.NextOffset != 0 || past.Totals != first.Totals || past.Offset != offset {
			t.Fatalf("offset %d past the end: %+v", offset, past)
		}
	}
}

func TestPlanRestoreFailsWhenTheWorkingTreeNoLongerCapturesToCurrentTree(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			store := newStore(t, dir)
			snap := create(t, store, "snap")
			writeFile(t, dir, "new.txt", "new\n")
			first := planAndCheck(t, dir, store, snap.ID, PlanQuery{})
			writeFile(t, dir, "newer.txt", "newer\n")

			other := strings.Repeat("a", 104-len(first.CurrentTree))
			for _, q := range []PlanQuery{{CurrentTree: first.CurrentTree}, {Offset: 1, CurrentTree: first.CurrentTree}, {CurrentTree: other}} {
				plan, err := store.PlanRestore(context.Background(), snap.ID, q)
				if err != ErrStale || err.Error() != "working tree changed since the plan" || !reflect.DeepEqual(plan, RestorePlan{}) {
					t.Fatalf("%+v: got %v and %+v, want ErrStale and no plan", q, err, plan)
				}
			}
		})
	}
}

func TestPlanRestoreChecksTheQueryBeforeTheLookup(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	unknown := "20260101T000000.000000000Z-000000000000"
	tree := strings.Repeat("a", 40)
	for _, q := range []PlanQuery{
		{Offset: -1},
		{Offset: MaxPageOffset + 1, CurrentTree: tree},
		{Offset: 1},
		{CurrentTree: "abc"},
		{CurrentTree: strings.ToUpper(tree)},
		{CurrentTree: tree + "0"},
	} {
		if _, err := store.PlanRestore(context.Background(), unknown, q); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%+v: got %v, want a query error before the lookup", q, err)
		}
	}
	if _, err := store.PlanRestore(context.Background(), unknown, PlanQuery{Offset: MaxPageOffset, CurrentTree: tree}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a valid query for an unknown snapshot: %v", err)
	}
}

func TestPlanRestoreResolvesTheSnapshotAsGetDoes(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "snap")
	for id, want := range map[string]error{"not-an-id": ErrInvalidID, "20260101T000000.000000000Z-000000000000": ErrNotFound} {
		if _, err := store.PlanRestore(context.Background(), id, PlanQuery{}); !errors.Is(err, want) {
			t.Errorf("%q: got %v, want %v", id, err, want)
		}
	}
	linked := filepath.Join(t.TempDir(), "linked")
	git(t, dir, "worktree", "add", "-q", "--detach", linked)
	if _, err := newStore(t, linked).PlanRestore(context.Background(), snap.ID, PlanQuery{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another worktree's snapshot: %v", err)
	}
	symbolic := "20260101T000000.000000000Z-" + snap.Commit[:12]
	git(t, dir, "symbolic-ref", store.itemsRef()+symbolic, store.itemsRef()+snap.ID)
	if _, err := store.PlanRestore(context.Background(), symbolic, PlanQuery{}); err == nil || !strings.Contains(err.Error(), "symbolic ref") {
		t.Fatalf("a symbolic item ref: %v", err)
	}
}

func foldsCase(t *testing.T, dir string) bool {
	t.Helper()
	writeFile(t, dir, ".fold-probe", "")
	defer os.Remove(filepath.Join(dir, ".fold-probe"))
	_, err := os.Lstat(filepath.Join(dir, ".FOLD-PROBE"))
	return err == nil
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
}

func TestPlanRestoreBlocksEachWriteSomethingUntrackedStandsIn(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, dir string)
		write   string
		blocked bool
	}{
		{"an ignored file", func(t *testing.T, dir string) { writeFile(t, dir, "x.log", "ignored\n") }, "x.log", true},
		{"an ignored symbolic link", func(t *testing.T, dir string) {
			symlinkOrSkip(t, "tracked.txt", filepath.Join(dir, "link.log"))
		}, "link.log", true},
		{"an empty directory", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "empty", true},
		{"a directory that holds an ignored file", func(t *testing.T, dir string) { writeFile(t, dir, "dir/x.log", "ignored\n") }, "dir", true},
		{"a directory that holds only an empty directory once the plan's deletes run", func(t *testing.T, dir string) {
			writeFile(t, dir, "nest/x", "current\n")
			if err := os.Mkdir(filepath.Join(dir, "nest", "inner"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "nest", true},
		{"an ignored file at a parent", func(t *testing.T, dir string) { writeFile(t, dir, "p.log", "ignored\n") }, "p.log/child", true},
		{"nothing", func(t *testing.T, dir string) {}, "fresh", false},
		{"a current file the plan modifies", func(t *testing.T, dir string) { writeFile(t, dir, "mod", "current\n") }, "mod", false},
		{"a directory the plan's deletes empty", func(t *testing.T, dir string) { writeFile(t, dir, "full/a/b", "current\n") }, "full", false},
		{"a current file at a parent that the plan deletes", func(t *testing.T, dir string) { writeFile(t, dir, "f", "current\n") }, "f/g", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := testRepo(t)
			writeFile(t, dir, ".gitignore", "*.log\n")
			git(t, dir, "add", ".gitignore")
			git(t, dir, "commit", "-q", "-m", "ignore logs")
			c.setup(t, dir)
			store := newStore(t, dir)
			snapshot := snapshotOfTree(t, dir, store, treeOf(t, dir,
				fileEntry(t, dir, modeFile, "tracked.txt", "initial\n"),
				fileEntry(t, dir, modeFile, ".gitignore", "*.log\n"),
				nestedEntry(t, dir, c.write, fileEntry(t, dir, modeFile, filepath.Base(c.write), "snapshot\n")),
			))

			plan := planAndCheck(t, dir, store, snapshot, PlanQuery{})
			want := PathBlocker("")
			if c.blocked {
				want = BlockUntrackedInTheWay
			}
			if got := entryAt(t, plan, c.write).Blocker; got != want {
				t.Fatalf("%s at %s: blocker %q, want %q", c.name, c.write, got, want)
			}
			for _, e := range plan.Entries {
				if e.Path != c.write && e.Blocker != "" {
					t.Errorf("%s: blocker %q", e.Path, e.Blocker)
				}
			}
		})
	}
}

func nestedEntry(t *testing.T, dir, path, entry string) string {
	t.Helper()
	segments := strings.Split(path, "/")
	for i := len(segments) - 2; i >= 0; i-- {
		entry = dirEntry(segments[i], treeOf(t, dir, entry))
	}
	return entry
}

func TestPlanRestoreNeverFollowsASymbolicLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git for Windows records a symbolic link as one only under core.symlinks")
	}
	dir := testRepo(t)
	writeFile(t, dir, ".gitignore", "*.log\n")
	outside := t.TempDir()
	writeFile(t, outside, "x", "outside\n")
	empty := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(dir, "link"))
	symlinkOrSkip(t, empty, filepath.Join(dir, "out.log"))
	store := newStore(t, dir)
	snapshot := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, modeFile, "tracked.txt", "initial\n"),
		fileEntry(t, dir, modeFile, ".gitignore", "*.log\n"),
		nestedEntry(t, dir, "link/x", fileEntry(t, dir, modeFile, "x", "snapshot\n")),
		nestedEntry(t, dir, "out.log/x", fileEntry(t, dir, modeFile, "x", "snapshot\n")),
	))

	plan := planAndCheck(t, dir, store, snapshot, PlanQuery{})
	if e := entryAt(t, plan, "link"); e.Action != ActionDelete || e.Current.Mode != modeSymlink || e.Blocker != "" {
		t.Fatalf("the current link: %+v", e)
	}
	if got := entryAt(t, plan, "link/x").Blocker; got != "" {
		t.Errorf("link/x below a link the plan deletes: blocker %q, want none, so the probe did not read %s through the link", got, outside)
	}
	if got := entryAt(t, plan, "out.log/x").Blocker; got != BlockUntrackedInTheWay {
		t.Errorf("out.log/x below an ignored link to an empty directory: blocker %q, want %q", got, BlockUntrackedInTheWay)
	}
}

func TestPlanRestoreBlocksNamesThatFoldTogether(t *testing.T) {
	dir := testRepo(t)
	git(t, dir, "config", "core.ignorecase", "true")
	writeFile(t, dir, "Docs/c", "current\n")
	writeFile(t, dir, "readme.md", "current\n")
	store := newStore(t, dir)
	tracked := fileEntry(t, dir, modeFile, "tracked.txt", "initial\n")
	pair := snapshotOfTree(t, dir, store, treeOf(t, dir, tracked,
		fileEntry(t, dir, modeFile, "README", "upper\n"),
		fileEntry(t, dir, modeFile, "readme", "lower\n"),
		dirEntry("Docs", treeOf(t, dir, fileEntry(t, dir, modeFile, "a", "a\n"))),
		dirEntry("docs", treeOf(t, dir, fileEntry(t, dir, modeFile, "b", "b\n"))),
		fileEntry(t, dir, modeFile, "README.md", "current\n"),
	))

	plan := planAndCheck(t, dir, store, pair, PlanQuery{})
	want := map[string]PathBlocker{
		"Docs/a":    BlockNameCollision,
		"Docs/c":    BlockNameCollision,
		"README":    BlockNameCollision,
		"README.md": "",
		"docs/b":    BlockNameCollision,
		"readme":    BlockNameCollision,
		"readme.md": "",
	}
	got := map[string]PathBlocker{}
	for _, e := range plan.Entries {
		got[e.Path] = e.Blocker
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blockers:\n got %v\nwant %v", got, want)
	}

	if !foldsCase(t, dir) {
		git(t, dir, "config", "core.ignorecase", "false")
		for _, e := range planAndCheck(t, dir, store, pair, PlanQuery{}).Entries {
			if e.Blocker != "" {
				t.Errorf("a tree that does not fold: %s blocked as %q", e.Path, e.Blocker)
			}
		}
	}
}

func TestPlanRestoreProbesForFoldingWhenTheConfigurationIsSilent(t *testing.T) {
	dir := testRepo(t)
	if !foldsCase(t, dir) {
		t.Skip("the volume that holds the test repository does not fold case")
	}
	git(t, dir, "config", "core.ignorecase", "false")
	store := newStore(t, dir)
	snapshot := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, modeFile, "tracked.txt", "initial\n"),
		fileEntry(t, dir, modeFile, "README", "upper\n"),
		fileEntry(t, dir, modeFile, "readme", "lower\n"),
	))
	plan := planAndCheck(t, dir, store, snapshot, PlanQuery{})
	for _, path := range []string{"README", "readme"} {
		if got := entryAt(t, plan, path).Blocker; got != BlockNameCollision {
			t.Errorf("%s: blocker %q, want %q", path, got, BlockNameCollision)
		}
	}
}

func TestPlanRestoreDoesNotCountACaseOnlyRenameAsInTheWay(t *testing.T) {
	dir := testRepo(t)
	if !foldsCase(t, dir) {
		t.Skip("the volume that holds the test repository does not fold case")
	}
	writeFile(t, dir, "readme", "current\n")
	writeFile(t, dir, "Dir/x", "current\n")
	store := newStore(t, dir)
	snapshot := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, modeFile, "tracked.txt", "initial\n"),
		fileEntry(t, dir, modeFile, "README", "snapshot\n"),
		fileEntry(t, dir, modeFile, "DIR", "snapshot\n"),
	))
	plan := planAndCheck(t, dir, store, snapshot, PlanQuery{})
	want := map[string]PathBlocker{"DIR": "", "Dir/x": BlockNameCollision, "README": "", "readme": ""}
	got := map[string]PathBlocker{}
	for _, e := range plan.Entries {
		got[e.Path] = e.Blocker
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blockers:\n got %v\nwant %v", got, want)
	}
}

func TestPlanRestoreBlocksTheDeleteOfACaseOnlyDirectoryRename(t *testing.T) {
	dir := testRepo(t)
	git(t, dir, "config", "core.ignorecase", "true")
	writeFile(t, dir, "Dir/a", "a\n")
	store := newStore(t, dir)
	snapshot := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, modeFile, "tracked.txt", "initial\n"),
		dirEntry("dir", treeOf(t, dir, fileEntry(t, dir, modeFile, "a", "a\n"))),
	))
	plan := planAndCheck(t, dir, store, snapshot, PlanQuery{})
	want := map[string]PathBlocker{"Dir/a": BlockNameCollision, "dir/a": ""}
	got := map[string]PathBlocker{}
	for _, e := range plan.Entries {
		got[e.Path] = e.Blocker
	}
	if !reflect.DeepEqual(got, want) || plan.Applicable {
		t.Fatalf("blockers:\n got %v, applicable %v\nwant %v, not applicable", got, plan.Applicable, want)
	}
}

func TestPlanRestoreBlocksNamesGitRefusesWithoutTouchingThem(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			store := newStore(t, dir)
			x := fileEntry(t, dir, modeFile, "x", "planted\n")
			snapshotTree := treeOf(t, dir,
				fileEntry(t, dir, modeFile, "tracked.txt", "initial\n"),
				dirEntry(".git", treeOf(t, dir, dirEntry("hooks", treeOf(t, dir, x)))),
				dirEntry("GIT~1", treeOf(t, dir, x)),
				fileEntry(t, dir, modeSymlink, ".gitmodules", "elsewhere"),
				fileEntry(t, dir, modeFile, "bad\xffname", "bytes\n"),
			)
			snapshot := snapshotOfTree(t, dir, store, snapshotTree)

			plan := planAndCheck(t, dir, store, snapshot, PlanQuery{})
			want := map[string]PathBlocker{
				".git/hooks/x":  BlockUnsafePath,
				".gitmodules":   BlockUnsafePath,
				"GIT~1/x":       BlockUnsafePath,
				"bad\ufffdname": BlockUnaddressablePath,
			}
			got := map[string]PathBlocker{}
			for _, e := range plan.Entries {
				got[e.Path] = e.Blocker
			}
			if !reflect.DeepEqual(got, want) || plan.Applicable || plan.ResultTree != snapshotTree {
				t.Fatalf("blockers %v applicable %v result tree %s, want %v, not applicable, and %s", got, plan.Applicable, plan.ResultTree, want, snapshotTree)
			}

			scoped := planAndCheck(t, dir, store, snapshot, PlanQuery{Scope: scopeOf(t, ".git", "GIT~1")})
			root := git(t, dir, "ls-tree", "-z", plan.CurrentTree)
			kept := strings.Split(strings.TrimSuffix(root, "\x00"), "\x00")
			for _, name := range []string{".git", "GIT~1"} {
				kept = append(kept, strings.TrimSuffix(git(t, dir, "ls-tree", "-z", snapshotTree, name), "\x00"))
			}
			if want := treeOf(t, dir, kept...); scoped.ResultTree != want {
				t.Fatalf("scope .git and GIT~1: result tree %q, want %s", scoped.ResultTree, want)
			}
		})
	}
}

func literalTree(t *testing.T, dir string, entries ...[3]string) string {
	t.Helper()
	var body bytes.Buffer
	for _, e := range entries {
		oid, err := hex.DecodeString(e[2])
		if err != nil {
			t.Fatal(err)
		}
		body.WriteString(e[0] + " " + e[1] + "\x00")
		body.Write(oid)
	}
	cmd := gitCommand(dir, sanitizedEnv(), "hash-object", "-t", "tree", "-w", "--literally", "--stdin")
	cmd.Stdin = &body
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object --literally: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestPlanRestoreFailsOnASnapshotTreeNoRestoreCouldProduce(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	blob := blobOf(t, dir, "x\n")
	below := treeOf(t, dir, fileEntry(t, dir, modeFile, "b", "below\n"))
	for name, want := range map[string]string{
		literalTree(t, dir, [3]string{modeFile, "a", blob}, [3]string{modeFile, "a", blob}): `names "a" twice`,
		literalTree(t, dir, [3]string{modeFile, "a", blob}, [3]string{"40000", "a", below}): `holds "a" and a path below it`,
	} {
		snapshot := snapshotOfTree(t, dir, store, name)
		plan, err := store.PlanRestore(context.Background(), snapshot, PlanQuery{})
		if err == nil || !strings.Contains(err.Error(), want) || !reflect.DeepEqual(plan, RestorePlan{}) {
			t.Fatalf("tree %s: got %v and %+v, want an error that %s", name, err, plan, want)
		}
	}
}

func TestPlanRestoreIgnoresReplaceRefs(t *testing.T) {
	dir := testRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	store := newStore(t, dir)
	snap := create(t, store, "snap")
	writeFile(t, dir, "a.txt", "changed\n")
	before := planAndCheck(t, dir, store, snap.ID, PlanQuery{})

	tree := get(t, store, snap.ID).Tree
	decoy := treeOf(t, dir, fileEntry(t, dir, modeFile, "decoy.txt", "decoy\n"))
	git(t, dir, "replace", "-f", tree, decoy)
	git(t, dir, "replace", "-f", blobOf(t, dir, "a\n"), blobOf(t, dir, "decoy\n"))
	after := planAndCheck(t, dir, store, snap.ID, PlanQuery{})
	if !reflect.DeepEqual(after, before) || after.Snapshot.Tree != tree {
		t.Fatalf("a replace ref changed the plan:\nbefore %+v\nafter  %+v", before, after)
	}
}

func nestedRepo(t *testing.T, dir, name string) string {
	t.Helper()
	inner := filepath.Join(dir, name)
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, inner, "init", "-q", "-b", "main")
	writeFile(t, inner, "inner.txt", "inner\n")
	git(t, inner, "add", "inner.txt")
	git(t, inner, "commit", "-q", "-m", "inner")
	return git(t, inner, "rev-parse", "HEAD")
}

func TestPlanRestoreBlocksSubmodulesAndNestedRepositories(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	empty := create(t, store, "before the nested repository")
	nested := nestedRepo(t, dir, "nested")
	head := git(t, dir, "rev-parse", "HEAD")
	gitlink := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, modeFile, "tracked.txt", "initial\n"),
		modeGitlink+" commit "+nested+"\tnested",
		modeGitlink+" commit "+head+"\tsub",
	))

	plan := planAndCheck(t, dir, store, empty.ID, PlanQuery{})
	assertEntries(t, plan.Entries, []PlanEntry{
		{Path: "nested", Action: ActionDelete, Current: side(modeGitlink, nested), Untracked: true, Blocker: BlockSubmodule},
	})
	plan = planAndCheck(t, dir, store, gitlink, PlanQuery{})
	assertEntries(t, plan.Entries, []PlanEntry{
		{Path: "sub", Action: ActionCreate, Target: side(modeGitlink, head), Blocker: BlockSubmodule},
	})
}

func TestPlanRestoreSeesGitlinkChangesDespiteSubmoduleIgnore(t *testing.T) {
	dir := testRepo(t)
	first := nestedRepo(t, dir, "sub")
	writeFile(t, dir, ".gitmodules", "[submodule \"sub\"]\n\tpath = sub\n\turl = ./sub\n")
	git(t, dir, "add", ".gitmodules", "sub")
	git(t, dir, "commit", "-q", "-m", "submodule")
	git(t, dir, "config", "submodule.sub.ignore", "all")
	git(t, dir, "config", "diff.ignoreSubmodules", "all")
	store := newStore(t, dir)
	snap := create(t, store, "at the first commit")
	inner := filepath.Join(dir, "sub")
	writeFile(t, inner, "inner.txt", "second\n")
	git(t, inner, "commit", "-q", "-am", "second")
	second := git(t, inner, "rev-parse", "HEAD")

	plan := planAndCheck(t, dir, store, snap.ID, PlanQuery{})
	assertEntries(t, plan.Entries, []PlanEntry{
		{Path: "sub", Action: ActionModify, Current: side(modeGitlink, second), Target: side(modeGitlink, first), Unstaged: true, Blocker: BlockSubmodule},
	})
}

func TestPlanRestoreInALinkedWorktreeReadsThatWorktreesState(t *testing.T) {
	dir := testRepo(t)
	main := newStore(t, dir)
	for _, linked := range []string{filepath.Join(t.TempDir(), "linked"), filepath.Join(dir, "nested")} {
		t.Run(filepath.Base(linked), func(t *testing.T) {
			git(t, dir, "worktree", "add", "-q", "--detach", linked)
			store := newStore(t, linked)
			snap := create(t, store, "linked")
			writeFile(t, linked, "work.txt", "linked\n")
			if got := planAndCheck(t, linked, store, snap.ID, PlanQuery{}); !got.Applicable || len(got.Entries) != 1 {
				t.Fatalf("linked plan %+v", got)
			}

			mergeHead := git(t, linked, "rev-parse", "--path-format=absolute", "--git-path", "MERGE_HEAD")
			if err := os.WriteFile(mergeHead, []byte(git(t, dir, "rev-parse", "HEAD")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(mergeHead)
			if got := planAndCheck(t, linked, store, snap.ID, PlanQuery{}).Blockers; !reflect.DeepEqual(got, []RepoBlocker{BlockOperationInProgress}) {
				t.Fatalf("linked worktree with MERGE_HEAD: blockers %q", got)
			}
			mainSnap := create(t, main, "main")
			if got := planAndCheck(t, dir, main, mainSnap.ID, PlanQuery{}).Blockers; len(got) != 0 {
				t.Fatalf("the main worktree read the linked worktree's MERGE_HEAD: blockers %q", got)
			}
		})
	}
}

func objectExists(t *testing.T, dir, oid string) bool {
	t.Helper()
	return gitCommand(dir, sanitizedEnv(), "cat-file", "-e", oid).Run() == nil
}

func TestPlanRestoreCapturesTheTreeCreateWouldRecord(t *testing.T) {
	cases := []struct {
		name  string
		posix bool
		setup func(t *testing.T, dir string)
		dirty func(t *testing.T, dir string)
	}{
		{"a CRLF HEAD under core.autocrlf", false, func(t *testing.T, dir string) {
			writeFile(t, dir, "crlf.txt", "a\r\nb\r\n")
			git(t, dir, "add", "crlf.txt")
			git(t, dir, "commit", "-q", "-m", "crlf")
			git(t, dir, "config", "core.autocrlf", "true")
		}, func(t *testing.T, dir string) {
			writeFile(t, dir, "crlf.txt", "a\r\nc\r\n")
			writeFile(t, dir, "fresh.txt", "x\r\ny\r\n")
		}},
		{"a clean filter", true, func(t *testing.T, dir string) {
			writeFile(t, dir, ".gitattributes", "*.up filter=up\n")
			git(t, dir, "config", "filter.up.clean", "tr a-z A-Z")
			git(t, dir, "config", "filter.up.required", "true")
		}, func(t *testing.T, dir string) { writeFile(t, dir, "fresh.up", "made upper by the filter\n") }},
		{"sparse checkout", false, func(t *testing.T, dir string) {
			writeFile(t, dir, "in/a", "in\n")
			writeFile(t, dir, "out/b", "out\n")
			git(t, dir, "add", "-A")
			git(t, dir, "commit", "-q", "-m", "cone")
			git(t, dir, "sparse-checkout", "set", "in")
		}, func(t *testing.T, dir string) { writeFile(t, dir, "in/a", "changed in the cone\n") }},
		{"a packed repository", false, func(t *testing.T, dir string) {
			git(t, dir, "gc", "-q")
		}, func(t *testing.T, dir string) { writeFile(t, dir, "tracked.txt", "after gc\n") }},
		{"a split index", false, func(t *testing.T, dir string) {
			git(t, dir, "config", "core.splitIndex", "true")
			git(t, dir, "update-index", "--split-index")
		}, func(t *testing.T, dir string) { writeFile(t, dir, "fresh.txt", "split\n") }},
	}
	for _, format := range objectFormats {
		for _, c := range cases {
			t.Run(format+"/"+c.name, func(t *testing.T) {
				if c.posix && runtime.GOOS == "windows" {
					t.Skip("the filter is a POSIX command")
				}
				dir := testRepoIn(t, format)
				c.setup(t, dir)
				store := newStore(t, dir)
				snap := create(t, store, "before")
				c.dirty(t, dir)

				before := gitDirFiles(t, dir)
				plan, err := store.PlanRestore(context.Background(), snap.ID, PlanQuery{})
				if err != nil {
					t.Fatal(err)
				}
				if changes := gitDirChanges(before, gitDirFiles(t, dir)); len(changes) > 0 {
					t.Fatalf("the plan changed the Git directory:\n%s", strings.Join(changes, "\n"))
				}
				if objectExists(t, dir, plan.CurrentTree) {
					t.Fatalf("the plan wrote its current tree %s into the repository's object store", plan.CurrentTree)
				}
				if again := planAndCheck(t, dir, store, snap.ID, PlanQuery{}); again.CurrentTree != plan.CurrentTree || len(plan.Entries) == 0 {
					t.Fatalf("current tree %s then %s, %d entries", plan.CurrentTree, again.CurrentTree, len(plan.Entries))
				}
			})
		}
	}
}

func TestPlanRestoreReadsTheIndexWhileItIsLocked(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "snap")
	writeFile(t, dir, "tracked.txt", "changed\n")
	git(t, dir, "add", "tracked.txt")
	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := planAndCheck(t, dir, store, snap.ID, PlanQuery{})
	if e := entryAt(t, plan, "tracked.txt"); !e.Staged || e.Unstaged {
		t.Fatalf("tracked.txt staged under a held lock: %+v", e)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the plan touched another process's index lock: %v", err)
	}
}

func scratchRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, root)
	}
	return root
}

func assertEmpty(t *testing.T, dir, when string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Fatalf("%s left %d entries in the scratch root, the first %s", when, len(entries), entries[0].Name())
	}
}

func TestPlanRestoreRemovesItsScratchDirectoryOnEveryReturn(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "snap")
	blob := blobOf(t, dir, "x\n")
	tampered := snapshotOfTree(t, dir, store, literalTree(t, dir, [3]string{modeFile, "a", blob}, [3]string{modeFile, "a", blob}))
	writeFile(t, dir, "tracked.txt", "changed\n")
	root := scratchRoot(t)

	if _, err := store.PlanRestore(context.Background(), snap.ID, PlanQuery{}); err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, root, "a plan")
	if _, err := store.PlanRestore(context.Background(), snap.ID, PlanQuery{CurrentTree: strings.Repeat("0", 40)}); err != ErrStale {
		t.Fatalf("stale page: %v", err)
	}
	assertEmpty(t, root, "a stale page")
	if _, err := store.PlanRestore(context.Background(), tampered, PlanQuery{}); err == nil {
		t.Fatal("a tampered snapshot tree must fail the plan")
	}
	assertEmpty(t, root, "a failed plan")
}

func TestPlanRestoreCancelledWhileAFilterBlocksReturnsNoPlan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the filter is a POSIX command")
	}
	dir := testRepo(t)
	signals := t.TempDir()
	started, release := filepath.Join(signals, "started"), filepath.Join(signals, "release")
	writeFile(t, dir, ".gitattributes", "*.slow filter=slow\n")
	git(t, dir, "config", "filter.slow.clean", fmt.Sprintf("touch %q; while [ ! -e %q ]; do sleep 0.05; done; cat", started, release))
	store := newStore(t, dir)
	snap := create(t, store, "snap")
	writeFile(t, dir, "x.slow", "blocks\n")
	root := scratchRoot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		plan RestorePlan
		err  error
	}
	done := make(chan result, 1)
	go func() {
		plan, err := store.PlanRestore(ctx, snap.ID, PlanQuery{})
		done <- result{plan, err}
	}()
	defer func() { _ = os.WriteFile(release, nil, 0o644) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the clean filter never started")
		}
	}
	cancel()
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || !reflect.DeepEqual(got.plan, RestorePlan{}) {
			t.Fatalf("got %v and %+v, want context.Canceled and no plan", got.err, got.plan)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled plan did not return")
	}
	assertEmpty(t, root, "a cancelled plan")
}

func TestPlanRestoreIsApplicableOnlyWithoutBlockersOrUnmatchedPaths(t *testing.T) {
	dir := testRepo(t)
	writeFile(t, dir, "flagged.txt", "v1\n")
	git(t, dir, "add", "flagged.txt")
	git(t, dir, "commit", "-q", "-m", "flagged")
	store := newStore(t, dir)
	snap := create(t, store, "snap")
	mergeHead := filepath.Join(dir, ".git", "MERGE_HEAD")
	cases := []struct {
		name       string
		setup      func()
		query      PlanQuery
		applicable bool
	}{
		{"no entries", func() {}, PlanQuery{}, true},
		{"an entry with no blocker", func() { writeFile(t, dir, "tracked.txt", "changed\n") }, PlanQuery{}, true},
		{"an unmatched scope path", func() {}, PlanQuery{Scope: scopeOf(t, "tracked.txt", "nowhere")}, false},
		{"a blocked entry", func() {
			writeFile(t, dir, "flagged.txt", "v2\n")
			git(t, dir, "update-index", "--skip-worktree", "flagged.txt")
		}, PlanQuery{}, false},
		{"a repository blocker", func() {
			git(t, dir, "update-index", "--no-skip-worktree", "flagged.txt")
			writeFile(t, dir, "flagged.txt", "v1\n")
			writeFile(t, dir, ".git/MERGE_HEAD", git(t, dir, "rev-parse", "HEAD")+"\n")
		}, PlanQuery{}, false},
		{"the repository blocker gone", func() {
			if err := os.Remove(mergeHead); err != nil {
				t.Fatal(err)
			}
		}, PlanQuery{}, true},
	}
	for _, c := range cases {
		c.setup()
		plan := planAndCheck(t, dir, store, snap.ID, c.query)
		if plan.Applicable != c.applicable {
			t.Errorf("%s: applicable %v, want %v (blockers %q, unmatched %q, blocked %d)", c.name, plan.Applicable, c.applicable, plan.Blockers, plan.Unmatched, plan.Totals.Blocked)
		}
	}
}
