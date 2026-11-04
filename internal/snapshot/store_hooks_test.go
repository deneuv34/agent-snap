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
	"time"
)

func logScript(t *testing.T, path, marker, log, reply string) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\necho %s >> '%s'\nprintf '%s'\n", marker, filepath.ToSlash(log), reply)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runs(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var markers []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		markers = append(markers, strings.Fields(line)[0])
	}
	return markers
}

func plainCapture(t *testing.T, dir string) string {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(sanitizedEnv(), "GIT_INDEX_FILE="+index)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	run("read-tree", "HEAD")
	run("add", "-A", "--", ".")
	return run("write-tree")
}

func TestCaptureRunsNoRepositoryHookOrFsmonitor(t *testing.T) {
	cases := []struct {
		marker string
		set    func(t *testing.T, dir, log string)
	}{
		{"post-index-change", func(t *testing.T, dir, log string) {
			logScript(t, filepath.Join(dir, ".git", "hooks", "post-index-change"), "post-index-change", log, "")
		}},
		{"reference-transaction", func(t *testing.T, dir, log string) {
			logScript(t, filepath.Join(dir, ".git", "hooks", "reference-transaction"), "reference-transaction", log, "")
		}},
		{"config-hook", func(t *testing.T, dir, log string) {
			command := fmt.Sprintf("echo config-hook >> '%s'", filepath.ToSlash(log))
			git(t, dir, "config", "hook.my hook.command", command)
			git(t, dir, "config", "hook.my hook.event", "post-index-change")
			git(t, dir, "config", "--add", "hook.my hook.event", "reference-transaction")
		}},
		{"global-config-hook", func(t *testing.T, dir, log string) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			command := fmt.Sprintf("echo global-config-hook >> '%s'", filepath.ToSlash(log))
			git(t, dir, "config", "--global", "hook.a.b.command", command)
			git(t, dir, "config", "--global", "hook.a.b.event", "post-index-change")
		}},
		{"fsmonitor", func(t *testing.T, dir, log string) {
			hook := filepath.Join(t.TempDir(), "fsmonitor")
			logScript(t, hook, "fsmonitor", log, `token\0/\0`)
			git(t, dir, "config", "core.fsmonitor", filepath.ToSlash(hook))
			git(t, dir, "config", "core.fsmonitorHookVersion", "2")
			git(t, dir, "update-index", "--fsmonitor")
			git(t, dir, "status", "--porcelain")
			if index, err := os.ReadFile(filepath.Join(dir, ".git", "index")); err != nil || !strings.Contains(string(index), "FSMN") {
				t.Fatalf("the real index has no fsmonitor extension: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.marker, func(t *testing.T) {
			dir := testRepo(t)
			writeFile(t, dir, "tracked.txt", "changed\n")
			writeFile(t, dir, "untracked.txt", "new\n")
			log := filepath.Join(t.TempDir(), "runs")
			tc.set(t, dir, log)
			store := newStore(t, dir)

			want := plainCapture(t, dir)
			git(t, dir, "update-ref", "refs/control", "HEAD")
			if got := runs(t, log); !slices.Contains(got, tc.marker) {
				t.Fatalf("the %s control never ran under plain Git: %v", tc.marker, got)
			}
			if err := os.Remove(log); err != nil {
				t.Fatal(err)
			}

			snap := create(t, store, "hooks")
			if got := runs(t, log); len(got) > 0 {
				t.Errorf("create ran %d repository programs: %v", len(got), got)
			}
			if tree := git(t, dir, "rev-parse", snap.Commit+"^{tree}"); tree != want {
				t.Errorf("create recorded tree %s, plain Git captures %s", tree, want)
			}

			writeFile(t, dir, "tracked.txt", "later\n")
			if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if _, err := store.PlanRestore(context.Background(), snap.ID, PlanQuery{Scope: WholeTree()}); err != nil {
				t.Fatal(err)
			}
			if got := runs(t, log); len(got) > 0 {
				t.Errorf("the restore plan ran %d repository programs: %v", len(got), got)
			}

			if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			get(t, store, snap.ID)
			if got := runs(t, log); len(got) > 0 {
				t.Errorf("get ran %d repository programs: %v", len(got), got)
			}
		})
	}
}

func TestCaptureRefusesAConfigHookItCannotDisable(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "before")
	log := filepath.Join(t.TempDir(), "runs")
	git(t, dir, "config", "hook.a=b.command", fmt.Sprintf("echo a=b >> '%s'", filepath.ToSlash(log)))
	git(t, dir, "config", "hook.a=b.event", "reference-transaction")
	writeFile(t, dir, "tracked.txt", "changed\n")
	root := scratchRoot(t)
	refs := git(t, dir, "for-each-ref", refsPrefix)

	if _, err := store.Create(context.Background(), "after", Origin{}); err == nil || !strings.Contains(err.Error(), `hook name "a=b"`) {
		t.Fatalf("create error = %v, want a refusal that names the hook", err)
	}
	if _, err := store.PlanRestore(context.Background(), snap.ID, PlanQuery{Scope: WholeTree()}); err == nil || !strings.Contains(err.Error(), `hook name "a=b"`) {
		t.Fatalf("plan error = %v, want a refusal that names the hook", err)
	}
	if got := git(t, dir, "for-each-ref", refsPrefix); got != refs {
		t.Fatalf("the refused create changed refs from %q to %q", refs, got)
	}
	if got := runs(t, log); len(got) > 0 {
		t.Fatalf("the refused operations ran the hook: %v", got)
	}
	assertEmpty(t, root, "a refused create and plan")
}

func TestCaptureReadsValuesFromStandardOutputOnly(t *testing.T) {
	filters := []struct{ name, clean string }{
		{"quiet", "cat"},
		{"noisy", "echo noisy filter ran >&2; cat"},
	}
	for _, format := range objectFormats {
		for _, filter := range filters {
			t.Run(format+"/"+filter.name, func(t *testing.T) {
				dir := testRepoIn(t, format)
				writeFile(t, dir, ".gitattributes", "*.txt filter=probe\n")
				git(t, dir, "config", "filter.probe.clean", filter.clean)
				writeFile(t, dir, "tracked.txt", "changed\n")
				future := time.Now().Add(24 * time.Hour)
				if err := os.Chtimes(filepath.Join(dir, "tracked.txt"), future, future); err != nil {
					t.Fatal(err)
				}
				store := newStore(t, dir)

				snap := create(t, store, "filtered")
				if tree, want := git(t, dir, "rev-parse", snap.Commit+"^{tree}"), plainCapture(t, dir); tree != want {
					t.Fatalf("create recorded tree %s, plain Git captures %s", tree, want)
				}
				if _, err := store.PlanRestore(context.Background(), snap.ID, PlanQuery{Scope: WholeTree()}); err != nil {
					t.Fatalf("plan restore: %v", err)
				}
			})
		}
	}
}

func TestCreateRefusesAMalformedHookListing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the git wrapper is a POSIX shell script")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, listing := range []string{"x", "hook.", "hook.a", "hook.command\nv", "\nfoo"} {
		t.Run(fmt.Sprintf("%q", listing), func(t *testing.T) {
			dir := testRepo(t)
			bin := t.TempDir()
			reply := filepath.Join(bin, "listing")
			if err := os.WriteFile(reply, []byte(listing+"\x00"), 0o644); err != nil {
				t.Fatal(err)
			}
			script := fmt.Sprintf("#!/bin/sh\nfor a; do [ \"$a\" = --get-regexp ] && exec cat %q; done\nexec %q \"$@\"\n", reply, realGit)
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			store := newStore(t, dir)

			if _, err := store.Create(context.Background(), "malformed", Origin{}); err == nil || !strings.Contains(err.Error(), "list configured hooks") {
				t.Fatalf("create error = %v, want the malformed hook listing refused", err)
			}
			if refs := git(t, dir, "for-each-ref", refsPrefix); refs != "" {
				t.Fatalf("the refused create wrote refs %q", refs)
			}
		})
	}
}
