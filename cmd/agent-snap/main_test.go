package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func cliRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "file.txt")
	gitRun(t, dir, "commit", "-m", "initial")
	return dir
}

func TestCLIPingAndVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"ping"}, &out); err != nil {
		t.Fatal(err)
	}
	var ping struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out.Bytes(), &ping); err != nil {
		t.Fatal(err)
	}
	if ping.Status != "ok" || ping.Version == "" {
		t.Fatalf("unexpected ping: %+v", ping)
	}

	out.Reset()
	if err := run([]string{"version"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != ping.Version {
		t.Fatalf("version mismatch: got %q want %q", got, ping.Version)
	}
}

func TestCLICreateAndList(t *testing.T) {
	dir := cliRepo(t)
	var out bytes.Buffer
	if err := run([]string{"create", "cli-test", "--cwd", dir}, &out); err != nil {
		t.Fatal(err)
	}
	var snap struct {
		ID     string  `json:"id"`
		Commit string  `json:"commit"`
		Reason string  `json:"reason"`
		Source string  `json:"source"`
		Host   *string `json:"host"`
	}
	if err := json.Unmarshal(out.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.ID == "" || snap.Commit == "" || snap.Reason != "cli-test" || snap.Source != "cli" || snap.Host != nil {
		t.Fatalf("unexpected snapshot: %s", out.String())
	}

	out.Reset()
	if err := run([]string{"list", "--cwd", dir, "--limit", "5"}, &out); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Snapshots []struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Snapshots) != 1 || listed.Snapshots[0].ID != snap.ID || listed.Snapshots[0].Source != "cli" {
		t.Fatalf("unexpected list: %+v", listed)
	}
}

func TestCLICreateRejectsAReasonThatReadsAsOrigin(t *testing.T) {
	dir := cliRepo(t)
	var out bytes.Buffer
	for _, cwd := range []string{dir, t.TempDir()} {
		err := run([]string{"create", "x\n\nAgent-Snap-Host: pi", "--cwd", cwd}, &out)
		if err == nil || !strings.Contains(err.Error(), "invalid snapshot reason") {
			t.Fatalf("cwd %s: want an invalid reason error before the repository is resolved, got %v", cwd, err)
		}
	}
	out.Reset()
	if err := run([]string{"list", "--cwd", dir}, &out); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Snapshots []json.RawMessage `json:"snapshots"`
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil || len(listed.Snapshots) != 0 {
		t.Fatalf("a rejected create must not write a snapshot: %v %s", err, out.String())
	}
}

func TestCLIGet(t *testing.T) {
	dir := cliRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"create", "--cwd", dir}, &out); err != nil {
		t.Fatal(err)
	}
	var snap struct {
		ID     string `json:"id"`
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(out.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := run([]string{"get", snap.ID, "--cwd", dir}, &out); err != nil {
		t.Fatal(err)
	}
	var detail struct {
		ID           string   `json:"id"`
		Commit       string   `json:"commit"`
		Tree         string   `json:"tree"`
		Parent       string   `json:"parent"`
		ChangedFiles []string `json:"changed_files"`
	}
	if err := json.Unmarshal(out.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.ID != snap.ID || detail.Commit != snap.Commit || detail.Tree != gitRun(t, dir, "rev-parse", snap.Commit+"^{tree}") ||
		detail.Parent != gitRun(t, dir, "rev-parse", "HEAD") || len(detail.ChangedFiles) != 1 || detail.ChangedFiles[0] != "file.txt" {
		t.Fatalf("unexpected detail: %+v", detail)
	}

	missing := snap.ID[:len(snap.ID)-12] + "000000000000"
	if err := run([]string{"get", missing, "--cwd", dir}, &out); err == nil || err.Error() != "snapshot not found: "+missing {
		t.Fatalf("expected not found for %s, got: %v", missing, err)
	}
}

func TestCLIUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"frobnicate"}, &out); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("expected unknown command error, got: %v", err)
	}
}
