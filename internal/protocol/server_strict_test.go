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

func TestServerRejectsUnknownRequestKeys(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		key    string
		wantID any
	}{
		{name: "beside ping", line: `{"id":9,"method":"ping","bogus":1}`, key: "bogus", wantID: float64(9)},
		{name: "before the id", line: `{"bogus":1,"id":9,"method":"ping"}`, key: "bogus", wantID: float64(9)},
		{name: "null value", line: `{"id":9,"method":"ping","bogus":null}`, key: "bogus", wantID: float64(9)},
		{name: "empty name", line: `{"id":9,"method":"ping","":1}`, key: "", wantID: float64(9)},
		{name: "string id", line: `{"id":"req-9","method":"ping","bogus":1}`, key: "bogus", wantID: "req-9"},
		{name: "beside params", line: `{"id":9,"method":"snapshot.list","params":{"cwd":"/tmp"},"bogus":1}`, key: "bogus", wantID: float64(9)},
		{name: "JSON-RPC version", line: `{"jsonrpc":"2.0","id":9,"method":"ping"}`, key: "jsonrpc", wantID: float64(9)},
		{name: "before the method lookup", line: `{"id":9,"method":"snapshot.nope","bogus":1}`, key: "bogus", wantID: float64(9)},
		{name: "mistyped method", line: `{"id":9,"method":5,"bogus":1}`, key: "bogus", wantID: float64(9)},
		{name: "origin name is not the retry message", line: `{"id":9,"method":"snapshot.create","params":{"cwd":"/tmp"},"source":"tool"}`, key: "source", wantID: float64(9)},
		{name: "first of two in byte order", line: `{"id":9,"method":"ping","zeta":1,"alpha":2}`, key: "alpha", wantID: float64(9)},
		{name: "method in another case", line: `{"id":9,"METHOD":"ping"}`, key: "METHOD", wantID: float64(9)},
		{name: "params in another case", line: `{"id":9,"method":"ping","Params":{}}`, key: "Params", wantID: float64(9)},
		{name: "id only in another case", line: `{"ID":9,"method":"ping"}`, key: "ID", wantID: nil},
		{name: "exact id beside another case", line: `{"id":9,"ID":4,"method":"ping"}`, key: "ID", wantID: float64(9)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := serveRequest(t, tc.line)
			if got := errCode(t, response); got != CodeInvalidParams {
				t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, response)
			}
			if got, want := errMessage(t, response), "unknown request key: "+tc.key; got != want {
				t.Fatalf("message = %q, want %q", got, want)
			}
			if response["id"] != tc.wantID {
				t.Fatalf("id = %v, want %v: %v", response["id"], tc.wantID, response)
			}
		})
	}
}

func TestServerAcceptsTheThreeRequestKeys(t *testing.T) {
	for _, line := range []string{
		`{"id":9,"method":"ping"}`,
		`{"id":9,"method":"ping","params":{}}`,
		`{"id":9,"method":"ping","params":null}`,
	} {
		response := serveRequest(t, line)
		if _, hasErr := response["error"]; hasErr {
			t.Fatalf("%s must be accepted: %v", line, response)
		}
		if response["id"] != float64(9) {
			t.Fatalf("%s: id = %v, want 9", line, response["id"])
		}
	}
}

func TestServerRunsNothingForAnUnknownRequestKey(t *testing.T) {
	dir := protocolRepo(t)
	rejected, err := json.Marshal(map[string]any{"id": 1, "method": "snapshot.create", "params": map[string]any{"cwd": dir}, "bogus": true})
	if err != nil {
		t.Fatal(err)
	}
	input := string(rejected) + "\n" + requestLine(2, "snapshot.list", map[string]any{"cwd": dir}) + "\n"
	var output bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 responses, got %d: %s", len(lines), output.String())
	}
	var refused map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &refused); err != nil {
		t.Fatal(err)
	}
	if got := errCode(t, refused); got != CodeInvalidParams {
		t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, refused)
	}
	var listed struct {
		Result struct {
			Snapshots []json.RawMessage `json:"snapshots"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Result.Snapshots) != 0 {
		t.Fatalf("the refused create recorded a snapshot: %s", lines[1])
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
