package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func protocolRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	commands := [][]string{{"init", "-b", "main"}, {"add", "file.txt"}, {"commit", "-m", "initial"}}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range commands {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestServerHandlesPingCreateAndList(t *testing.T) {
	dir := protocolRepo(t)
	input := strings.Join([]string{
		`{"id":1,"method":"ping"}`,
		`{"id":2,"method":"snapshot.create","params":{"cwd":"` + dir + `","reason":"test"}}`,
		`{"id":3,"method":"snapshot.list","params":{"cwd":"` + dir + `","limit":5}}`,
	}, "\n") + "\n"
	var output bytes.Buffer

	if err := Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d responses: %s", len(lines), output.String())
	}
	var ping struct {
		ID     int            `json:"id"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &ping); err != nil {
		t.Fatal(err)
	}
	if ping.ID != 1 || ping.Result["status"] != "ok" {
		t.Fatalf("unexpected ping response: %+v", ping)
	}
	var listed struct {
		ID     int `json:"id"`
		Result struct {
			Snapshots []json.RawMessage `json:"snapshots"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.ID != 3 || len(listed.Result.Snapshots) != 1 {
		t.Fatalf("unexpected list response: %+v", listed)
	}
}

func TestServerReturnsStructuredErrorAndContinues(t *testing.T) {
	input := "{not-json}\n{\"id\":2,\"method\":\"unknown\"}\n"
	var output bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"error"`) || !strings.Contains(lines[1], `"method not found"`) {
		t.Fatalf("unexpected responses: %s", output.String())
	}
}
