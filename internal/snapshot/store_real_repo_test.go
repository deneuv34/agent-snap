package snapshot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func realTree(t *testing.T, dir, commit string) string {
	t.Helper()
	out, err := gitCommand(dir, append(sanitizedEnv(), "GIT_NO_REPLACE_OBJECTS=1"), "rev-parse", commit+"^{tree}").Output()
	if err != nil {
		t.Fatalf("rev-parse %s^{tree}: %v", commit, err)
	}
	return strings.TrimSpace(string(out))
}

func TestCreateIgnoresAReplaceRefOnLatest(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			store := newStore(t, dir)
			first := create(t, store, "first")
			writeFile(t, dir, "tracked.txt", "changed\n")
			changed := plainCapture(t, dir)
			git(t, dir, "replace", first.Commit, git(t, dir, "commit-tree", changed, "-m", "decoy latest"))

			second := create(t, store, "second")
			if second.Reused || second.ID == first.ID {
				t.Fatalf("create reused %s, whose real tree %s is not the working tree %s", first.ID, realTree(t, dir, first.Commit), changed)
			}
			if tree := realTree(t, dir, second.Commit); tree != changed {
				t.Fatalf("create recorded tree %s, the working tree captures to %s", tree, changed)
			}
		})
	}
}

func TestListIgnoresReplaceRefs(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "real")
	git(t, dir, "replace", snap.Commit, git(t, dir, "commit-tree", realTree(t, dir, snap.Commit), "-m", messagePrefix+"forged"))

	items, err := store.List(context.Background(), 10)
	if err != nil || len(items) != 1 || items[0].Commit != snap.Commit || items[0].Reason != "real" {
		t.Fatalf("list must read the snapshot commit, not its replacement: %v %+v", err, items)
	}
}

func TestCreateInASparseRepoKeepsTheOutOfConeTree(t *testing.T) {
	dir := testRepo(t)
	writeFile(t, dir, "in/a", "in\n")
	writeFile(t, dir, "out/b", "out\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "cone")
	git(t, dir, "sparse-checkout", "set", "in")
	if _, err := os.Stat(filepath.Join(dir, "out", "b")); !os.IsNotExist(err) {
		t.Fatalf("sparse checkout left out/b on disk: %v", err)
	}
	writeFile(t, dir, "in/a", "changed in the cone\n")
	store := newStore(t, dir)

	snap := create(t, store, "sparse")
	if got, want := git(t, dir, "rev-parse", snap.Commit+":out/b"), git(t, dir, "rev-parse", "HEAD:out/b"); got != want {
		t.Fatalf("create recorded out/b as %s, HEAD holds %s", got, want)
	}
	if got, want := git(t, dir, "rev-parse", snap.Commit+":in/a"), blobOf(t, dir, "changed in the cone\n"); got != want {
		t.Fatalf("create recorded in/a as %s, the working tree holds %s", got, want)
	}
}

type gitCall struct {
	args                   []string
	noReplace, evidenceEnv bool
}

// logGitCalls puts a git on PATH that records each call's arguments and the
// two environment variables that tell the real repository from the evidence
// repository, and then runs the real Git.
func logGitCalls(t *testing.T) func() []gitCall {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, logs := t.TempDir(), t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
log=$(mktemp %q)
{ echo "${GIT_NO_REPLACE_OBJECTS-unset} ${GIT_CONFIG_NOSYSTEM-unset}"; for a; do echo "$a"; done; } > "$log"
exec %q "$@"
`, filepath.Join(logs, "call.XXXXXX"), realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []gitCall {
		t.Helper()
		entries, err := os.ReadDir(logs)
		if err != nil {
			t.Fatal(err)
		}
		var calls []gitCall
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(logs, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			env := strings.Fields(lines[0])
			calls = append(calls, gitCall{args: lines[1:], noReplace: env[0] == "1", evidenceEnv: env[1] == "1"})
			if err := os.Remove(filepath.Join(logs, entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
		return calls
	}
}

func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return ""
}

func TestCaptureAndPlanPassExplicitPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the git wrapper is a POSIX shell script")
	}
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			repo := git(t, dir, "rev-parse", "--show-toplevel")
			gitDir := git(t, dir, "rev-parse", "--absolute-git-dir")
			calls := logGitCalls(t)

			store := newStore(t, dir)
			for _, call := range calls() {
				if !call.noReplace || len(call.args) != 2 || call.args[0] != "rev-parse" {
					t.Errorf("resolution ran %q with GIT_NO_REPLACE_OBJECTS=1 %v, want one rev-parse option with it set", call.args, call.noReplace)
				}
			}
			first := create(t, store, "first")
			writeFile(t, dir, "tracked.txt", "changed\n")
			create(t, store, "second")
			if _, err := store.PlanRestore(context.Background(), first.ID, PlanQuery{Scope: WholeTree()}); err != nil {
				t.Fatal(err)
			}

			seen := map[string]bool{}
			for _, call := range calls() {
				if call.evidenceEnv {
					if sub := subcommand(call.args); sub != "init" && (len(call.args) == 0 || !strings.HasPrefix(call.args[0], "--git-dir=") || call.args[0] == "--git-dir="+gitDir) {
						t.Errorf("evidence repository call %q does not name its own Git directory", call.args)
					}
					continue
				}
				seen[subcommand(call.args)] = true
				if !call.noReplace || !slices.Contains(call.args, "--git-dir="+gitDir) || !slices.Contains(call.args, "--work-tree="+repo) {
					t.Errorf("real repository call %q: GIT_NO_REPLACE_OBJECTS=1 %v, want it with --git-dir=%s and --work-tree=%s", call.args, call.noReplace, gitDir, repo)
				}
			}
			for _, sub := range []string{"config", "rev-parse", "read-tree", "add", "write-tree", "commit-tree", "symbolic-ref", "update-ref", "diff-tree", "for-each-ref", "ls-files"} {
				if !seen[sub] {
					t.Errorf("no real repository call ran %s, so the check above did not see it", sub)
				}
			}
		})
	}
}

func TestCoreWorktreeKeepsTheCallersRepository(t *testing.T) {
	dir := testRepo(t)
	other := testRepo(t)
	worktree := filepath.Join(other, "w")
	writeFile(t, worktree, "w.txt", "in the other repository\n")
	git(t, dir, "config", "core.worktree", worktree)
	store := newStore(t, dir)

	snap := create(t, store, "elsewhere")
	if refs := git(t, other, "for-each-ref", refsPrefix); refs != "" {
		t.Fatalf("create wrote refs into the repository that holds core.worktree:\n%s", refs)
	}
	if got := git(t, dir, "ls-tree", "-r", "--name-only", snap.Commit); got != "w.txt" {
		t.Fatalf("create recorded %q, want only w.txt, the one file in core.worktree", got)
	}
	plan, err := store.PlanRestore(context.Background(), snap.ID, PlanQuery{Scope: WholeTree()})
	if err != nil || plan.CurrentTree != get(t, store, snap.ID).Tree {
		t.Fatalf("plan: %v, current tree %s, want the tree create recorded", err, plan.CurrentTree)
	}
}
