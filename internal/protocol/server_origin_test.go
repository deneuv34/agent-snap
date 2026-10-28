package protocol

import (
	"strings"
	"testing"
)

func TestServerCreateStoresOriginAndListAndGetReturnIt(t *testing.T) {
	dir := protocolRepo(t)
	created := serveRequestFor(t, 1, "snapshot.create", map[string]any{"cwd": dir, "reason": "before", "source": "tool", "host": "pi"})
	result, ok := created["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result, got: %v", created)
	}
	if result["reason"] != "before" || result["source"] != "tool" || result["host"] != "pi" {
		t.Fatalf("create result: %v", result)
	}

	listed := serveRequestFor(t, 2, "snapshot.list", map[string]any{"cwd": dir})
	entry := listed["result"].(map[string]any)["snapshots"].([]any)[0].(map[string]any)
	if entry["id"] != result["id"] || entry["reason"] != "before" || entry["source"] != "tool" || entry["host"] != "pi" {
		t.Fatalf("list entry: %v", entry)
	}

	got := serveRequestFor(t, 3, "snapshot.get", map[string]any{"cwd": dir, "id": result["id"]})
	detail := got["result"].(map[string]any)
	if detail["reason"] != "before" || detail["source"] != "tool" || detail["host"] != "pi" {
		t.Fatalf("get result: %v", detail)
	}
}

func TestServerCreateWithoutOriginOmitsSourceAndHost(t *testing.T) {
	dir := protocolRepo(t)
	created := serveRequestFor(t, 1, "snapshot.create", map[string]any{"cwd": dir, "reason": "plain"})
	result := created["result"].(map[string]any)
	for _, key := range []string{"source", "host"} {
		if _, present := result[key]; present {
			t.Errorf("a create without origin must omit %s: %v", key, result)
		}
	}
}

func TestServerCreateValidatesOriginBeforeResolvingTheRepository(t *testing.T) {
	for _, tc := range []struct {
		name    string
		params  map[string]any
		message string
	}{
		{"numeric source", map[string]any{"source": 7}, "param must be a string: source"},
		{"blank source", map[string]any{"source": ""}, "param must be an origin value: source"},
		{"capitalized source", map[string]any{"source": "Tool"}, "param must be an origin value: source"},
		{"source with a space", map[string]any{"source": "to ol"}, "param must be an origin value: source"},
		{"overlong source", map[string]any{"source": strings.Repeat("a", 65)}, "param must be an origin value: source"},
		{"null host", map[string]any{"host": nil}, "param must be a string: host"},
		{"session-like host", map[string]any{"host": "ses_01J9"}, "param must be an origin value: host"},
		{"reason that reads as origin", map[string]any{"reason": "x\n\nAgent-Snap-Source: tool"}, "param must not contain a NUL byte or end with an origin paragraph: reason"},
		{"reason with a wide future key", map[string]any{"reason": "x\n\nAgent-Snap-Ticket: ABC-1"}, "param must not contain a NUL byte or end with an origin paragraph: reason"},
		{"reason with a NUL byte", map[string]any{"reason": "a\x00b"}, "param must not contain a NUL byte or end with an origin paragraph: reason"},
	} {
		params := map[string]any{"cwd": "/not/a/repo"}
		for key, value := range tc.params {
			params[key] = value
		}
		response := serveRequestFor(t, 1, "snapshot.create", params)
		if code := errCode(t, response); code != CodeInvalidParams {
			t.Errorf("%s: want code %d, got %v: %v", tc.name, CodeInvalidParams, code, response)
			continue
		}
		if message := errMessage(t, response); message != tc.message {
			t.Errorf("%s: want message %q, got %q", tc.name, tc.message, message)
		}
	}
}
