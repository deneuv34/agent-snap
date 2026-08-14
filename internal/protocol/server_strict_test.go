package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func serveRequest(t *testing.T, line string) map[string]any {
	t.Helper()
	var output bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(line+"\n"), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 response, got %d: %s", len(lines), output.String())
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func serveRequestFor(t *testing.T, id int, method string, params map[string]any) map[string]any {
	t.Helper()
	return serveRequest(t, requestLine(id, method, params))
}

func errCode(t *testing.T, response map[string]any) float64 {
	t.Helper()
	rawErr, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error response, got: %v", response)
	}
	code, ok := rawErr["code"].(float64)
	if !ok {
		t.Fatalf("error missing numeric code: %v", rawErr)
	}
	return code
}

func TestServerRejectsNonStringCwd(t *testing.T) {
	response := serveRequest(t, requestLine(1, "snapshot.create", map[string]any{"cwd": 42}))
	if got := errCode(t, response); got != CodeInvalidParams {
		t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, response)
	}
}

func TestServerRejectsUnknownParams(t *testing.T) {
	response := serveRequest(t, requestLine(1, "snapshot.create", map[string]any{"cwd": "/tmp", "reason": "x", "bogus": 1}))
	if got := errCode(t, response); got != CodeInvalidParams {
		t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, response)
	}
}

func TestServerRejectsBadLimit(t *testing.T) {
	response := serveRequest(t, requestLine(1, "snapshot.list", map[string]any{"cwd": "/tmp", "limit": "many"}))
	if got := errCode(t, response); got != CodeInvalidParams {
		t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, response)
	}
}

func TestServerRejectsParamsForPing(t *testing.T) {
	response := serveRequest(t, requestLine(1, "ping", map[string]any{"cwd": "/tmp"}))
	if got := errCode(t, response); got != CodeInvalidParams {
		t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, response)
	}
}

func TestServerRejectsMissingCwd(t *testing.T) {
	response := serveRequest(t, requestLine(1, "snapshot.list", map[string]any{"limit": 5}))
	if got := errCode(t, response); got != CodeInvalidParams {
		t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, response)
	}
}

func TestServerRejectsOutOfRangeLimits(t *testing.T) {
	for _, limit := range []any{0, -1, 101, 1.5} {
		response := serveRequest(t, requestLine(1, "snapshot.list", map[string]any{"cwd": "/tmp", "limit": limit}))
		if got := errCode(t, response); got != CodeInvalidParams {
			t.Fatalf("limit %v: want %d, got %v: %v", limit, CodeInvalidParams, got, response)
		}
	}
}

func TestServerAcceptsBoundedLimit(t *testing.T) {
	dir := protocolRepo(t)
	response := serveRequestFor(t, 1, "snapshot.list", map[string]any{"cwd": dir, "limit": 100})
	if _, hasErr := response["error"]; hasErr {
		t.Fatalf("limit 100 must be accepted: %v", response)
	}
}

// Windows paths contain backslashes, which are invalid in string-concatenated
// JSON. Marshaled requests must round-trip them as ordinary path values.
func TestServerAcceptsBackslashPaths(t *testing.T) {
	winPath := `C:\Users\runner\work\repo`
	response := serveRequest(t, requestLine(1, "snapshot.list", map[string]any{"cwd": winPath, "limit": 100}))
	if code := errCode(t, response); code != CodeInternalError {
		t.Fatalf("backslash path must parse (then fail as a non-repo), got code %v: %v", code, response)
	}
}
