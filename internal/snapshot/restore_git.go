//go:build darwin || linux

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const (
	captureIndexName = "index"
	resultIndexName  = "result-index"
	afterIndexName   = "after-index"
	writeStderrName  = "write-stderr"
	writeStderrLimit = 4096
)

var openWriteStderr = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
}

// config passes no -c, because a -c value shows up in what the configuration
// reads print.
type restoreGit struct {
	config, reads, pinned *Store

	dir string
	ev  evidence
}

func gitVersion(ctx context.Context) (string, error) {
	cmd := gitCmd(ctx, "version")
	cmd.Env = append(sanitizedEnv(), "GIT_NO_REPLACE_OBJECTS=1")
	return output(cmd, []string{"version"})
}

func (s *Store) newRestoreGit(ctx context.Context, dir string) (*restoreGit, string, error) {
	hooks, err := createHooksDir(dir)
	if err != nil {
		return nil, "", err
	}
	stderr, err := os.OpenFile(filepath.Join(dir, writeStderrName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("create the write's standard error file: %w", err)
	}
	if err := stderr.Close(); err != nil {
		return nil, "", fmt.Errorf("create the write's standard error file: %w", err)
	}

	config := *s
	config.pins = nil
	r := &restoreGit{config: &config, dir: dir}
	fingerprint, err := r.readConfig(ctx)
	if err != nil {
		return nil, "", err
	}
	keys, err := configKeys(fingerprint)
	if err != nil {
		return nil, "", err
	}
	disabled, err := disabledHooks(configuredHookNames(keys))
	if err != nil {
		return nil, "", err
	}

	reads := *s
	reads.pins = slices.Concat(s.pins, []string{"-c", "core.hooksPath=" + hooks}, disabled)
	r.reads = &reads
	pinned := *s
	pinned.pins = slices.Concat([]string{
		"-c", "core.splitIndex=false", "-c", "core.trustctime=true", "-c", "core.checkStat=default",
		"-c", "core.ignoreStat=false", "-c", "core.fsmonitor=", "-c", "core.sparseCheckout=false",
		"-c", "checkout.workers=1", "-c", "core.hooksPath=" + hooks,
	}, disabled)
	r.pinned = &pinned

	gitDir, env, err := r.reads.evidenceRepo(ctx, dir)
	if err != nil {
		return nil, "", err
	}
	r.ev = evidence{dir: dir, gitDir: gitDir, env: env, oidLen: len(s.zeroOID)}
	return r, fingerprint, nil
}

func (r *restoreGit) readConfig(ctx context.Context) (string, error) {
	out, err := r.config.gitRaw(ctx, nil, "config", "--list", "-z", "--show-scope", "--show-origin")
	if err != nil {
		return "", fmt.Errorf("read configuration: %w", err)
	}
	return out, nil
}

// A value can hold a newline, and a key cannot, so the key ends at the first.
func configKeys(listing string) ([]string, error) {
	fields := records(listing)
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("read configuration: Git printed %d fields, which do not form scope, origin, and key records", len(fields))
	}
	keys := make([]string, 0, len(fields)/3)
	for i := 2; i < len(fields); i += 3 {
		key, _, _ := strings.Cut(fields[i], "\n")
		keys = append(keys, key)
	}
	return keys, nil
}

// The event switch also stops a config hook for that event added after the
// read, on the Git versions that document it.
func configuredHookNames(keys []string) []string {
	names := []string{"post-index-change"}
	for _, key := range keys {
		name, ok := hookName(key)
		if ok && (strings.HasSuffix(key, ".command") || strings.HasSuffix(key, ".event")) {
			names = append(names, name)
		}
	}
	return names
}

func (r *restoreGit) env(index string) []string {
	return append(scratchEnvFor(r.dir, index), "GIT_OBJECT_DIRECTORY="+filepath.Join(r.ev.gitDir, "objects"))
}

func (r *restoreGit) captureCurrent(ctx context.Context) (capture, error) {
	current, head, err := r.pinned.captureTree(ctx, r.env(captureIndexName))
	if err != nil {
		return capture{}, err
	}
	return capture{op: r.reads, ev: r.ev, current: current, head: head}, nil
}

func (r *restoreGit) listRecordedStat(ctx context.Context) (string, error) {
	return r.reads.gitRaw(ctx, append(r.env(captureIndexName), "GIT_OPTIONAL_LOCKS=0"), scratchIndexArgs("ls-files", "--debug", "-z")...)
}

// writeResultTree goes through a second scratch index, so the index that the
// write checks against stays byte-identical.
func (r *restoreGit) writeResultTree(ctx context.Context, result listing) (string, error) {
	env := r.env(resultIndexName)
	var entries strings.Builder
	for _, l := range result {
		fmt.Fprintf(&entries, "%s %s\t%s\x00", l.mode, l.oid, l.path)
	}
	if _, err := r.reads.gitStdin(ctx, env, entries.String(), scratchIndexArgs("update-index", "-z", "--index-info")...); err != nil {
		return "", err
	}
	return r.reads.git(ctx, env, scratchIndexArgs("write-tree")...)
}

type restoreWrite struct {
	cmd        *exec.Cmd
	stderrPath string
}

// startWrite starts Git in a new session, so no terminal or guardian group
// signal reaches it or its filters. It has no pipe to the core, so a core that
// dies cannot end it with SIGPIPE. It inherits the lock, which stays held
// until it exits.
func (r *restoreGit) startWrite(ctx context.Context, lock *restoreLock, currentTree, resultTree string) (*restoreWrite, error) {
	path := filepath.Join(r.dir, writeStderrName)
	stderr, err := openWriteStderr(path)
	if err != nil {
		return nil, fmt.Errorf("open the write's standard error file: %w", err)
	}
	defer stderr.Close()
	cmd := r.pinned.command(context.WithoutCancel(ctx), r.env(captureIndexName), "read-tree", "-m", "-u", currentTree, resultTree)
	cmd.Stderr = stderr
	cmd.ExtraFiles = []*os.File{lock.file}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the write: %w", err)
	}
	return &restoreWrite{cmd: cmd, stderrPath: path}, nil
}

func (w *restoreWrite) pgid() int { return w.cmd.Process.Pid }

type writeStatus int

const (
	writeExitedZero writeStatus = iota
	writeFailed
	writeUnobserved
)

type writeEnd struct {
	status    writeStatus
	errorText string
}

func (w *restoreWrite) wait() writeEnd {
	err := w.cmd.Wait()
	if err == nil {
		return writeEnd{status: writeExitedZero}
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return writeEnd{status: writeUnobserved}
	}
	return writeEnd{status: writeFailed, errorText: writeErrorText(statusLine(exit), w.stderrPath)}
}

func statusLine(exit *exec.ExitError) string {
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return "git died from signal " + status.Signal().String()
	}
	return fmt.Sprintf("git exited with status %d", exit.ExitCode())
}

func writeErrorText(status, path string) string {
	first, err := readWriteStderr(path)
	if err != nil {
		return status + "\nstandard error unreadable: " + err.Error()
	}
	var lossy bool
	text := strings.TrimSuffix(repairUTF8(first, &lossy), "\n")
	if text == "" {
		return status
	}
	return status + "\n" + text
}

func readWriteStderr(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	first := make([]byte, writeStderrLimit)
	n, err := io.ReadFull(f, first)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return first[:n], nil
}

func (r *restoreGit) captureAfterWrite(ctx context.Context, started func(pgid int)) (string, error) {
	op := *r.pinned
	op.run = func(cmd *exec.Cmd, args []string) (string, error) {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return outputOnceStarted(cmd, args, started)
	}
	tree, _, err := op.captureTree(context.WithoutCancel(ctx), r.env(afterIndexName))
	return tree, err
}

type restoreOutcome string

const (
	outcomeApplied restoreOutcome = "applied"
	outcomeStopped restoreOutcome = "stopped"
	outcomeUnknown restoreOutcome = "unknown"
)

func decideOutcome(status writeStatus, captureErr error, afterTree, resultTree string) restoreOutcome {
	switch atResult := afterTree == resultTree; {
	case captureErr != nil:
		return outcomeUnknown
	case status == writeExitedZero && atResult:
		return outcomeApplied
	case status == writeFailed && !atResult:
		return outcomeStopped
	}
	return outcomeUnknown
}
