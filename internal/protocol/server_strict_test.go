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
	response := serveRequest(t, `{"id":1,"method":"snapshot.create","params":{"cwd":42}}`)
	if got := errCode(t, response); got != CodeInvalidParams {
		t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, response)
	}
}

func TestServerRejectsUnknownParams(t *testing.T) {
	response := serveRequest(t, `{"id":1,"method":"snapshot.create","params":{"cwd":"/tmp","reason":"x","bogus":1}}`)
	if got := errCode(t, response); got != CodeInvalidParams {
		t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, response)
	}
}

func TestServerRejectsBadLimit(t *testing.T) {
	response := serveRequest(t, `{"id":1,"method":"snapshot.list","params":{"cwd":"/tmp","limit":"many"}}`)
	if got := errCode(t, response); got != CodeInvalidParams {
		t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, response)
	}
}

func TestServerRejectsParamsForPing(t *testing.T) {
	response := serveRequest(t, `{"id":1,"method":"ping","params":{"cwd":"/tmp"}}`)
	if got := errCode(t, response); got != CodeInvalidParams {
		t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, response)
	}
}

func TestServerRejectsMissingCwd(t *testing.T) {
	response := serveRequest(t, `{"id":1,"method":"snapshot.list","params":{"limit":5}}`)
	if got := errCode(t, response); got != CodeInvalidParams {
		t.Fatalf("want %d, got %v: %v", CodeInvalidParams, got, response)
	}
}

func TestServerRejectsOutOfRangeLimits(t *testing.T) {
	for _, limit := range []string{"0", "-1", "101", "1.5"} {
		response := serveRequest(t, `{"id":1,"method":"snapshot.list","params":{"cwd":"/tmp","limit":`+limit+`}}`)
		if got := errCode(t, response); got != CodeInvalidParams {
			t.Fatalf("limit %s: want %d, got %v: %v", limit, CodeInvalidParams, got, response)
		}
	}
}

func TestServerAcceptsBoundedLimit(t *testing.T) {
	dir := protocolRepo(t)
	line := `{"id":1,"method":"snapshot.list","params":{"cwd":"` + dir + `","limit":100}}`
	response := serveRequest(t, line)
	if _, hasErr := response["error"]; hasErr {
		t.Fatalf("limit 100 must be accepted: %v", response)
	}
}
