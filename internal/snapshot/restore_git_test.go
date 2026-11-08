//go:build darwin || linux

package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	printSessionArg = "agent-snap-test-print-session"
	restoreCoreArg  = "agent-snap-test-restore-core"
	coreGroupEnv    = "AGENT_SNAP_TEST_CORE_GROUP"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		var err error
		switch os.Args[1] {
		case printSessionArg:
			err = printSession()
		case restoreCoreArg:
			err = runRestoreCore(os.Args[2], os.Args[3], os.Args[4])
		default:
			os.Exit(m.Run())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func printSession() error {
	session, _, errno := syscall.RawSyscall(syscall.SYS_GETSID, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	fmt.Println(session)
	return nil
}

type restoreSteps struct {
	r        *restoreGit
	c        capture
	computed computedPlan
}

func throughStepEight(ctx context.Context, store *Store, scratch, snapshotTree string) (restoreSteps, error) {
	r, _, err := store.newRestoreGit(ctx, scratch)
	if err != nil {
		return restoreSteps{}, err
	}
	c, err := r.captureCurrent(ctx)
	if err != nil {
		return restoreSteps{}, err
	}
	computed, err := c.computePlan(ctx, WholeTree(), snapshotTree)
	if err != nil {
		return restoreSteps{}, err
	}
	return restoreSteps{r: r, c: c, computed: computed}, nil
}

func runRestoreCore(dir, id, log string) error {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM)
	if err := os.Setenv(coreGroupEnv, strconv.Itoa(os.Getpid())); err != nil {
		return err
	}
	note := func(line string) error {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintln(f, line)
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := New(dir)
	if err != nil {
		return err
	}
	snap, err := store.lookup(ctx, id)
	if err != nil {
		return err
	}
	lock, err := store.lockRestore()
	if err != nil {
		return err
	}
	defer lock.release()
	scratch, cleanup, err := createScratchIndex(store.repo)
	if err != nil {
		return err
	}
	defer cleanup()
	steps, err := throughStepEight(ctx, store, scratch, snap.tree)
	if err != nil {
		return err
	}
	if _, err := steps.r.listRecordedStat(ctx); err != nil {
		return err
	}
	if _, err := steps.r.readConfig(ctx); err != nil {
		return err
	}

	cancel()
	if err := note("step write"); err != nil {
		return err
	}
	write, err := steps.r.startWrite(ctx, lock, steps.c.current, steps.computed.resultTree)
	if err != nil {
		return err
	}
	end := write.wait()
	if err := note(fmt.Sprintf("write-group %d", write.pgid())); err != nil {
		return err
	}
	if err := note("step check"); err != nil {
		return err
	}
	var noteErr error
	after, err := steps.r.captureAfterWrite(ctx, func(pgid int) { noteErr = errors.Join(noteErr, note(fmt.Sprintf("check-group %d", pgid))) })
	if noteErr != nil {
		return noteErr
	}
	if outcome := decideOutcome(end.status, err, after, steps.computed.resultTree); outcome != outcomeApplied {
		return fmt.Errorf("outcome %s, capture error %v, git error %q", outcome, err, end.errorText)
	}
	return nil
}

type recordedRun struct {
	args []string
	env  map[string]string
}

func (r recordedRun) settings() []string {
	var values []string
	for i := 0; i+1 < len(r.args) && strings.HasPrefix(r.args[i], "-"); i++ {
		if r.args[i] == "-c" {
			values = append(values, r.args[i+1])
			i++
		}
	}
	return values
}

func (r recordedRun) subcommand() string {
	for i := 0; i < len(r.args); i++ {
		switch {
		case r.args[i] == "-c":
			i++
		case !strings.HasPrefix(r.args[i], "-"):
			return strings.Join(r.args[i:], " ")
		}
	}
	return ""
}

func (r recordedRun) inEvidenceRepo() bool {
	return strings.HasPrefix(r.subcommand(), "init ") || slices.ContainsFunc(r.args, func(arg string) bool {
		return strings.HasPrefix(arg, "--git-dir=") && strings.HasSuffix(arg, "evidence.git")
	})
}

func recordGit(t *testing.T) (log string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log = filepath.Join(t.TempDir(), "runs")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\0' ARGS \"$@\" ENV >> %[1]q\nenv | grep '^GIT_' | tr '\\n' '\\0' >> %[1]q\nprintf 'END\\0' >> %[1]q\nexec %[2]q \"$@\"\n", log, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func recordedRuns(t *testing.T, log string) []recordedRun {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var runs []recordedRun
	var run recordedRun
	inEnv := false
	for _, field := range records(string(data)) {
		switch {
		case field == "ARGS":
			run, inEnv = recordedRun{env: map[string]string{}}, false
		case field == "ENV":
			inEnv = true
		case field == "END":
			runs = append(runs, run)
		case inEnv:
			key, value, _ := strings.Cut(field, "=")
			run.env[key] = value
		default:
			run.args = append(run.args, field)
		}
	}
	return runs
}

var eightSettings = []string{
	"core.splitIndex=false", "core.trustctime=true", "core.checkStat=default", "core.ignoreStat=false",
	"core.fsmonitor=", "core.sparseCheckout=false", "checkout.workers=1",
}

var pinsOnlyTheCapturesTake = []string{"core.trustctime=true", "core.checkStat=default", "core.ignoreStat=false", "core.sparseCheckout=false", "checkout.workers=1"}

func holdsSetting(run recordedRun, prefix string) bool {
	return slices.ContainsFunc(run.settings(), func(value string) bool { return strings.HasPrefix(value, prefix) })
}

func wantRealRepo(t *testing.T, step string, run recordedRun) {
	t.Helper()
	if !slices.ContainsFunc(run.args, func(arg string) bool { return strings.HasPrefix(arg, "--git-dir=") }) ||
		!slices.ContainsFunc(run.args, func(arg string) bool { return strings.HasPrefix(arg, "--work-tree=") }) {
		t.Errorf("%s: %q does not name the resolved repository", step, run.subcommand())
	}
	if run.env["GIT_NO_REPLACE_OBJECTS"] != "1" {
		t.Errorf("%s: %q does not set GIT_NO_REPLACE_OBJECTS=1", step, run.subcommand())
	}
}

func wantPinned(t *testing.T, step string, run recordedRun) {
	t.Helper()
	wantRealRepo(t, step, run)
	for _, setting := range eightSettings {
		if !slices.Contains(run.settings(), setting) {
			t.Errorf("%s: %q lacks %s: %v", step, run.subcommand(), setting, run.settings())
		}
	}
	if !holdsSetting(run, "core.hooksPath=") || !slices.Contains(run.settings(), "hook.post-index-change.enabled=false") {
		t.Errorf("%s: %q lacks the hook settings: %v", step, run.subcommand(), run.settings())
	}
}

func wantReadRole(t *testing.T, step string, run recordedRun) {
	t.Helper()
	wantRealRepo(t, step, run)
	if !slices.Contains(run.settings(), "core.fsmonitor=") || !holdsSetting(run, "core.hooksPath=") ||
		!slices.Contains(run.settings(), "hook.post-index-change.enabled=false") {
		t.Errorf("%s: %q lacks the read role's settings: %v", step, run.subcommand(), run.settings())
	}
	for _, setting := range pinsOnlyTheCapturesTake {
		if slices.Contains(run.settings(), setting) {
			t.Errorf("%s: %q must not take %s", step, run.subcommand(), setting)
		}
	}
	if _, set := run.env["GIT_INDEX_FILE"]; set && !slices.Contains(run.settings(), "core.splitIndex=false") {
		t.Errorf("%s: %q sets GIT_INDEX_FILE without core.splitIndex=false", step, run.subcommand())
	}
}

func wantNoSettings(t *testing.T, step string, run recordedRun) {
	t.Helper()
	wantRealRepo(t, step, run)
	if slices.Contains(run.args, "-c") {
		t.Errorf("%s: %q must pass no -c: %v", step, run.subcommand(), run.args)
	}
}

func TestRestoreGitPinsEverySetting(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			setup := newStore(t, dir)
			snap := create(t, setup, "before")
			writeFile(t, dir, "tracked.txt", "edited\n")
			writeFile(t, dir, "new.txt", "new\n")
			backup := create(t, setup, "restore backup")
			for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG_PARAMETERS"} {
				t.Setenv(name, filepath.Join(t.TempDir(), "elsewhere"))
			}

			log := recordGit(t)
			during := func(work func()) []recordedRun {
				before := len(recordedRuns(t, log))
				work()
				return recordedRuns(t, log)[before:]
			}
			ctx := context.Background()

			var store *Store
			var snapTree string
			early := during(func() {
				var err error
				if _, err = gitVersion(ctx); err != nil {
					t.Fatal(err)
				}
				if store, err = New(dir); err != nil {
					t.Fatal(err)
				}
				found, err := store.lookup(ctx, snap.ID)
				if err != nil {
					t.Fatal(err)
				}
				snapTree = found.tree
				if _, err = store.lookup(ctx, backup.ID); err != nil {
					t.Fatal(err)
				}
			})
			if len(early) != 6 {
				t.Fatalf("before step 5: %d commands, want git version, 3 resolution reads, and 2 lookups: %v", len(early), early)
			}
			for _, run := range early {
				command := run.subcommand()
				if command != "version" && !strings.HasPrefix(command, "rev-parse --") && !strings.HasPrefix(command, "for-each-ref ") {
					t.Errorf("before step 5: unexpected command %q", command)
				}
				if run.env["GIT_NO_REPLACE_OBJECTS"] != "1" {
					t.Errorf("before step 5: %q does not set GIT_NO_REPLACE_OBJECTS=1", command)
				}
			}

			lock, err := store.lockRestore()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lock.release() })

			scratch := t.TempDir()
			var r *restoreGit
			var c capture
			stepFive := during(func() {
				var err error
				if r, _, err = store.newRestoreGit(ctx, scratch); err != nil {
					t.Fatal(err)
				}
				if c, err = r.captureCurrent(ctx); err != nil {
					t.Fatal(err)
				}
			})
			if len(stepFive) == 0 || !strings.HasPrefix(stepFive[0].subcommand(), "config --list -z --show-scope --show-origin") {
				t.Fatalf("step 5 must start with the configuration read: %v", stepFive)
			}
			wantNoSettings(t, "step 5 configuration read", stepFive[0])
			captures := 0
			for _, run := range stepFive[1:] {
				switch {
				case run.inEvidenceRepo():
				case run.env["GIT_INDEX_FILE"] != "":
					captures++
					wantPinned(t, "step 5 capture", run)
					if run.env["GIT_INDEX_FILE"] != filepath.Join(scratch, captureIndexName) {
						t.Errorf("step 5 capture: %q uses index %q", run.subcommand(), run.env["GIT_INDEX_FILE"])
					}
					if run.env["GIT_OBJECT_DIRECTORY"] != filepath.Join(scratch, "evidence.git", "objects") {
						t.Errorf("step 5 capture: %q uses object directory %q", run.subcommand(), run.env["GIT_OBJECT_DIRECTORY"])
					}
				default:
					wantReadRole(t, "step 5", run)
				}
			}
			if captures != 4 {
				t.Errorf("step 5: %d capture commands, want rev-parse, read-tree, add, and write-tree", captures)
			}

			var computed computedPlan
			stepEight := during(func() {
				var err error
				if computed, err = c.computePlan(ctx, WholeTree(), snapTree); err != nil {
					t.Fatal(err)
				}
				if _, err = r.listRecordedStat(ctx); err != nil {
					t.Fatal(err)
				}
			})
			var sawIndexRead, sawDebug bool
			for _, run := range stepEight {
				if run.inEvidenceRepo() {
					continue
				}
				wantReadRole(t, "step 8", run)
				switch command := run.subcommand(); {
				case strings.HasPrefix(command, "ls-files -z -s -v"):
					sawIndexRead = true
					if _, set := run.env["GIT_INDEX_FILE"]; set {
						t.Errorf("step 8: the read of the real index sets GIT_INDEX_FILE=%q", run.env["GIT_INDEX_FILE"])
					}
					if run.env["GIT_OPTIONAL_LOCKS"] != "0" {
						t.Error("step 8: the read of the real index does not set GIT_OPTIONAL_LOCKS=0")
					}
				case strings.HasPrefix(command, "ls-files --debug -z"):
					sawDebug = true
					if run.env["GIT_INDEX_FILE"] != filepath.Join(scratch, captureIndexName) {
						t.Errorf("step 8: ls-files --debug reads index %q", run.env["GIT_INDEX_FILE"])
					}
					if run.env["GIT_OPTIONAL_LOCKS"] != "0" {
						t.Error("step 8: ls-files --debug does not set GIT_OPTIONAL_LOCKS=0")
					}
				}
			}
			if !sawIndexRead || !sawDebug {
				t.Errorf("step 8: real index read %v, ls-files --debug %v", sawIndexRead, sawDebug)
			}

			var written string
			stepNine := during(func() {
				var err error
				if written, err = r.writeResultTree(ctx, computed.plan.result); err != nil {
					t.Fatal(err)
				}
			})
			if written != computed.resultTree {
				t.Errorf("step 9 wrote tree %s, the plan hashed %s", written, computed.resultTree)
			}
			if len(stepNine) != 2 {
				t.Fatalf("step 9: %d commands, want update-index and write-tree", len(stepNine))
			}
			for _, run := range stepNine {
				wantReadRole(t, "step 9", run)
				if run.env["GIT_INDEX_FILE"] != filepath.Join(scratch, resultIndexName) {
					t.Errorf("step 9: %q uses index %q", run.subcommand(), run.env["GIT_INDEX_FILE"])
				}
			}

			stepTen := during(func() {
				if _, err := r.readConfig(ctx); err != nil {
					t.Fatal(err)
				}
			})
			if len(stepTen) != 1 {
				t.Fatalf("step 10: %d configuration reads, want 1", len(stepTen))
			}
			wantNoSettings(t, "step 10 configuration read", stepTen[0])

			var write *restoreWrite
			theWrite := during(func() {
				var err error
				if write, err = r.startWrite(ctx, lock, c.current, computed.resultTree); err != nil {
					t.Fatal(err)
				}
				if end := write.wait(); end.status != writeExitedZero {
					t.Fatalf("the write ended %+v", end)
				}
			})
			if len(theWrite) != 1 || theWrite[0].subcommand() != "read-tree -m -u "+c.current+" "+computed.resultTree {
				t.Fatalf("the write ran %v", theWrite)
			}
			wantPinned(t, "the write", theWrite[0])
			if theWrite[0].env["GIT_INDEX_FILE"] != filepath.Join(scratch, captureIndexName) {
				t.Errorf("the write used index %q", theWrite[0].env["GIT_INDEX_FILE"])
			}
			if write.cmd.Stdin != nil || write.cmd.Stdout != nil {
				t.Errorf("the write must have the null device on standard input and output: %v, %v", write.cmd.Stdin, write.cmd.Stdout)
			}
			if _, isFile := write.cmd.Stderr.(*os.File); !isFile {
				t.Errorf("the write's standard error is %T, want a file", write.cmd.Stderr)
			}
			if !slices.Equal(write.cmd.ExtraFiles, []*os.File{lock.file}) {
				t.Errorf("the write inherits %v, want only the lock", write.cmd.ExtraFiles)
			}

			stepEleven := during(func() {
				after, err := r.captureAfterWrite(ctx, func(int) {})
				if err != nil {
					t.Fatal(err)
				}
				if after != computed.resultTree {
					t.Errorf("step 11 captured %s, want %s", after, computed.resultTree)
				}
			})
			if len(stepEleven) != 4 {
				t.Fatalf("step 11: %d commands, want rev-parse, read-tree, add, and write-tree", len(stepEleven))
			}
			for _, run := range stepEleven {
				wantPinned(t, "step 11", run)
				if run.env["GIT_INDEX_FILE"] != filepath.Join(scratch, afterIndexName) {
					t.Errorf("step 11: %q uses index %q", run.subcommand(), run.env["GIT_INDEX_FILE"])
				}
			}

			for _, run := range recordedRuns(t, log) {
				for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG_PARAMETERS"} {
					if value, set := run.env[name]; set {
						t.Errorf("%q inherited %s=%s", run.subcommand(), name, value)
					}
				}
			}
		})
	}
}

func TestCreateAndPlanNeverTakeTheRestoreSettings(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "before")
	writeFile(t, dir, "tracked.txt", "edited\n")
	log := recordGit(t)

	if _, err := store.Create(context.Background(), "after", Origin{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PlanRestore(context.Background(), snap.ID, PlanQuery{Scope: WholeTree()}); err != nil {
		t.Fatal(err)
	}

	runs := recordedRuns(t, log)
	if len(runs) == 0 {
		t.Fatal("recorded no Git command")
	}
	for _, run := range runs {
		if run.inEvidenceRepo() {
			continue
		}
		wantRealRepo(t, "create or plan", run)
		if !slices.Contains(run.settings(), "core.fsmonitor=") {
			t.Errorf("%q lost core.fsmonitor=", run.subcommand())
		}
		beforeScratch := strings.HasPrefix(run.subcommand(), "config -z --get-regexp") || strings.HasPrefix(run.subcommand(), "for-each-ref ")
		if !beforeScratch && !holdsSetting(run, "core.hooksPath=") {
			t.Errorf("%q lost core.hooksPath", run.subcommand())
		}
		for _, setting := range pinsOnlyTheCapturesTake {
			if slices.Contains(run.settings(), setting) {
				t.Errorf("%q must not take %s", run.subcommand(), setting)
			}
		}
		if slices.Contains(run.settings(), "hook.post-index-change.enabled=false") {
			t.Errorf("%q must not take the restore's event switch", run.subcommand())
		}
	}
}

func TestRestoreGitListsConfigHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	marker := filepath.Join(t.TempDir(), "ran")
	command := fmt.Sprintf("echo ran >> '%s'", filepath.ToSlash(marker))

	dir := testRepo(t)
	git(t, dir, "config", "hook.a.b.command", command)
	git(t, dir, "config", "hook.a.b.event", "post-index-change")
	git(t, dir, "config", "--global", "hook.my hook.event", "post-index-change")
	git(t, dir, "config", "--global", "hook.my hook.command", command)
	git(t, dir, "config", "hook.only-enabled.enabled", "true")
	git(t, dir, "config", "custom.value", "line\nhook.forged.command")
	store := newStore(t, dir)

	r, fingerprint, err := store.newRestoreGit(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fingerprint, "hook.a.b.command\n") {
		t.Fatalf("the fingerprint misses the repository's hook: %q", fingerprint)
	}
	for role, op := range map[string]*Store{"reads": r.reads, "pinned": r.pinned} {
		args := op.command(context.Background(), nil, "status").Args
		for _, want := range []string{"hook.post-index-change.enabled=false", "hook.a.b.enabled=false", "hook.my hook.enabled=false"} {
			if !slices.Contains(args, want) {
				t.Errorf("the %s role lacks %s: %v", role, want, args)
			}
		}
		for _, unwanted := range []string{"hook.only-enabled.enabled=false", "hook.forged.enabled=false"} {
			if slices.Contains(args, unwanted) {
				t.Errorf("the %s role must not name %s: %v", role, unwanted, args)
			}
		}
	}
	if args := r.config.command(context.Background(), nil, "config", "--list").Args; slices.Contains(args, "-c") {
		t.Errorf("the configuration role passes -c: %v", args)
	}

	writeFile(t, dir, "tracked.txt", "edited\n")
	if _, err := r.captureCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a config hook ran during the capture: %v", err)
	}

	t.Run("a name that holds = refuses", func(t *testing.T) {
		git(t, dir, "config", "hook.a=b.command", command)
		git(t, dir, "config", "hook.a=b.event", "post-index-change")
		_, _, err := store.newRestoreGit(context.Background(), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), `hook name "a=b"`) {
			t.Fatalf("error = %v, want a refusal that names the hook", err)
		}
		for _, sentinel := range []error{ErrStale, ErrNotFound, ErrRestoreBusy} {
			if errors.Is(err, sentinel) {
				t.Fatalf("error = %v maps to a code other than -32603", err)
			}
		}
	})

	t.Run("a malformed listing refuses", func(t *testing.T) {
		if _, err := configKeys("local\x00file:.git/config\x00"); err == nil {
			t.Fatal("a listing without a whole record must refuse")
		}
	})
}

func TestRestoreGitStartsInANewSession(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := testRepo(t)
	snap := create(t, newStore(t, dir), "before")
	writeFile(t, dir, "tracked.txt", "edited\n")

	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "log")
	script := fmt.Sprintf(`#!/bin/sh
trap 'echo "term $$" >> %[1]q' TERM
session=$(%[2]q %[3]q)
if [ -e /dev/fd/3 ]; then lock=lock; else lock=nolock; fi
echo "run $$ $session $lock $*" >> %[1]q
kill -s TERM -- "-$%[5]s" && echo "sent $$" >> %[1]q
exec %[4]q "$@"
`, log, self, printSessionArg, realGit, coreGroupEnv)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	core := exec.Command(self, restoreCoreArg, dir, snap.ID, log)
	core.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	core.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stderr bytes.Buffer
	core.Stderr = &stderr
	if err := core.Run(); err != nil {
		t.Fatalf("the core failed: %v\n%s", err, stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(dir, "tracked.txt")); err != nil || string(got) != "initial\n" {
		t.Fatalf("the write left %q, %v", got, err)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	type run struct {
		pid, session string
		lock         bool
		command      string
		phase        string
	}
	var runs []*run
	var writeGroup, checkGroups []string
	sent, termed := map[string]bool{}, map[string]bool{}
	phase := "before"
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.SplitN(line, " ", 5)
		switch fields[0] {
		case "step":
			phase = fields[1]
		case "write-group":
			writeGroup = append(writeGroup, fields[1])
		case "check-group":
			checkGroups = append(checkGroups, fields[1])
		case "sent":
			sent[fields[1]] = true
		case "term":
			termed[fields[1]] = true
		case "run":
			runs = append(runs, &run{pid: fields[1], session: fields[2], lock: fields[3] == "lock", command: fields[4], phase: phase})
		default:
			t.Fatalf("unexpected log line %q", line)
		}
	}

	coreSession := strconv.Itoa(core.Process.Pid)
	counts := map[string]int{}
	var checkPids []string
	for _, r := range runs {
		counts[r.phase]++
		if !sent[r.pid] {
			t.Fatalf("%q never signalled the core's group", r.command)
		}
		if r.phase == "before" {
			if r.session != coreSession || !termed[r.pid] || r.lock {
				t.Errorf("%q: session %s, core's %s, reached by SIGTERM %v, inherited the lock %v", r.command, r.session, coreSession, termed[r.pid], r.lock)
			}
			continue
		}
		if r.session == coreSession || r.session != r.pid {
			t.Errorf("%q: session %s, core's %s, its own pid %s", r.command, r.session, coreSession, r.pid)
		}
		if termed[r.pid] {
			t.Errorf("%q was reached by the SIGTERM to the core's group", r.command)
		}
		if r.lock != (r.phase == "write") {
			t.Errorf("%q in phase %s inherited the lock: %v", r.command, r.phase, r.lock)
		}
		if r.phase == "write" && !strings.Contains(r.command, " read-tree -m -u ") {
			t.Errorf("the write ran %q", r.command)
		}
		if r.phase == "write" && !slices.Equal(writeGroup, []string{r.pid}) {
			t.Errorf("the write's group was reported as %v, its pid is %s", writeGroup, r.pid)
		}
		if r.phase == "check" {
			checkPids = append(checkPids, r.pid)
		}
	}
	if counts["before"] == 0 || counts["write"] != 1 || counts["check"] != 4 {
		t.Fatalf("commands per phase = %v, want some before, 1 write, and 4 checks", counts)
	}
	if !slices.Equal(checkGroups, checkPids) {
		t.Errorf("step 11 reported groups %v, its commands were %v", checkGroups, checkPids)
	}
}

func TestPlanReadsSparseCheckoutWithoutThePin(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := get(t, store, create(t, store, "before").ID)
	writeFile(t, dir, "tracked.txt", "edited\n")
	git(t, dir, "config", "core.sparseCheckout", "true")

	steps, err := throughStepEight(context.Background(), store, t.TempDir(), snap.Tree)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(steps.computed.blockers, BlockSparseCheckout) {
		t.Fatalf("the step-8 plan blockers = %v, want sparse_checkout", steps.computed.blockers)
	}

	t.Run("control: the sparse pin hides the blocker", func(t *testing.T) {
		pinned := steps.c
		pinned.op = steps.r.pinned
		computed, err := pinned.computePlan(context.Background(), WholeTree(), snap.Tree)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(computed.blockers, BlockSparseCheckout) {
			t.Fatalf("with the pin the plan still read sparse checkout as on: %v", computed.blockers)
		}
	})
}

func TestRestoreStderrReadIsBounded(t *testing.T) {
	status := "git exited with status 128"

	t.Run("one 10 MiB line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stderr")
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 10<<20), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, want := writeErrorText(status, path), status+"\n"+strings.Repeat("x", 4096); got != want {
			t.Fatalf("error holds %d bytes, want %d", len(got), len(want))
		}
	})

	t.Run("a read stops at the limit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stderr")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		defer close(release)
		go func() {
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				return
			}
			defer f.Close()
			_, _ = f.Write(bytes.Repeat([]byte("y"), writeStderrLimit))
			<-release
		}()
		done := make(chan string, 1)
		go func() { done <- writeErrorText(status, path) }()
		select {
		case got := <-done:
			if want := status + "\n" + strings.Repeat("y", writeStderrLimit); got != want {
				t.Fatalf("error = %q", got)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the read kept waiting after the first 4096 bytes")
		}
	})

	t.Run("an empty file leaves only the status line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stderr")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := writeErrorText(status, path); got != status {
			t.Fatalf("error = %q, want only the status line", got)
		}
	})

	t.Run("a cut in a character is repaired", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stderr")
		if err := os.WriteFile(path, append(bytes.Repeat([]byte("z"), writeStderrLimit-1), "é"...), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, want := writeErrorText(status, path), status+"\n"+strings.Repeat("z", writeStderrLimit-1)+"�"; got != want {
			t.Fatalf("error ends in %q", got[len(got)-8:])
		}
	})

	t.Run("an unreadable file names the failure", func(t *testing.T) {
		for name, path := range map[string]string{
			"missing":     filepath.Join(t.TempDir(), "missing"),
			"a directory": t.TempDir(),
		} {
			if got := writeErrorText(status, path); !strings.HasPrefix(got, status+"\nstandard error unreadable: ") {
				t.Errorf("%s: error = %q", name, got)
			}
		}
	})
}

func restoreWriteCase(t *testing.T, beforeWrite func(dir string)) (writeEnd, restoreOutcome) {
	t.Helper()
	ctx := context.Background()
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := get(t, store, create(t, store, "before").ID)
	writeFile(t, dir, "tracked.txt", "edited\n")
	lock, err := store.lockRestore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.release() })
	steps, err := throughStepEight(ctx, store, t.TempDir(), snap.Tree)
	if err != nil {
		t.Fatal(err)
	}
	beforeWrite(dir)
	write, err := steps.r.startWrite(ctx, lock, steps.c.current, steps.computed.resultTree)
	if err != nil {
		t.Fatal(err)
	}
	end := write.wait()
	after, err := steps.r.captureAfterWrite(ctx, func(int) {})
	return end, decideOutcome(end.status, err, after, steps.computed.resultTree)
}

func noEdit(string) {}

func editPlannedPath(t *testing.T) func(string) {
	return func(dir string) { writeFile(t, dir, "tracked.txt", "edited again, a longer file\n") }
}

func TestRestoreWriteStderrOnAFullFileSystem(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux has /dev/full")
	}
	original := openWriteStderr
	t.Cleanup(func() { openWriteStderr = original })
	openWriteStderr = func(string) (*os.File, error) { return os.OpenFile("/dev/full", os.O_WRONLY, 0) }

	t.Run("git exits 0 and the capture equals the result", func(t *testing.T) {
		end, outcome := restoreWriteCase(t, noEdit)
		if outcome != outcomeApplied || end.errorText != "" {
			t.Fatalf("outcome %s, error %q", outcome, end.errorText)
		}
	})

	t.Run("git refuses and the capture differs", func(t *testing.T) {
		end, outcome := restoreWriteCase(t, editPlannedPath(t))
		if outcome != outcomeStopped || end.errorText != "git exited with status 128" {
			t.Fatalf("outcome %s, error %q", outcome, end.errorText)
		}
	})
}

func TestRestoreWriteReportsGitsStandardError(t *testing.T) {
	end, outcome := restoreWriteCase(t, editPlannedPath(t))
	if outcome != outcomeStopped {
		t.Fatalf("outcome = %s, want stopped", outcome)
	}
	if !strings.HasPrefix(end.errorText, "git exited with status 128\n") || !strings.Contains(end.errorText, "tracked.txt") {
		t.Fatalf("error = %q", end.errorText)
	}
}

func TestRestoreOutcomeFollowsGitsExitAndTheCapture(t *testing.T) {
	const result, other = "result", "other"
	failed := errors.New("capture failed")
	cases := []struct {
		name       string
		status     writeStatus
		captureErr error
		after      string
		want       restoreOutcome
	}{
		{"git 0, at the result", writeExitedZero, nil, result, outcomeApplied},
		{"git 0, elsewhere", writeExitedZero, nil, other, outcomeUnknown},
		{"git failed, elsewhere", writeFailed, nil, other, outcomeStopped},
		{"git failed, at the result", writeFailed, nil, result, outcomeUnknown},
		{"git 0, capture failed", writeExitedZero, failed, "", outcomeUnknown},
		{"git failed, capture failed", writeFailed, failed, "", outcomeUnknown},
		{"wait failed, at the result", writeUnobserved, nil, result, outcomeUnknown},
		{"wait failed, elsewhere", writeUnobserved, nil, other, outcomeUnknown},
	}
	for _, tc := range cases {
		if got := decideOutcome(tc.status, tc.captureErr, tc.after, result); got != tc.want {
			t.Errorf("%s: outcome = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestRestoreWriteReportsADeathBySignal(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nfor a; do [ \"$a\" = -m ] && kill -s KILL $$; done\nexec %q \"$@\"\n", realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	end, _ := restoreWriteCase(t, func(string) {
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	})
	if end.status != writeFailed || end.errorText != "git died from signal killed" {
		t.Fatalf("end = %+v", end)
	}
}
