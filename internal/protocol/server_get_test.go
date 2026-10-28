package protocol

import (
	"reflect"
	"sort"
	"testing"
)

const wellFormedID = "20260101T000000.000000000Z-0123456789ab"

func errMessage(t *testing.T, response map[string]any) string {
	t.Helper()
	rawErr, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error response, got: %v", response)
	}
	message, _ := rawErr["message"].(string)
	return message
}

func TestServerGetReturnsTheSnapshotDetail(t *testing.T) {
	dir := protocolRepo(t)
	created := serveRequestFor(t, 1, "snapshot.create", map[string]any{"cwd": dir, "reason": "before"})
	id := created["result"].(map[string]any)["id"].(string)

	response := serveRequestFor(t, 2, "snapshot.get", map[string]any{"cwd": dir, "id": id})
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result, got: %v", response)
	}
	var keys []string
	for key := range result {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"changed_files", "commit", "created_at", "id", "parent", "reason", "tree"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("result keys: got %v want %v", keys, want)
	}
	if result["id"] != id || result["reason"] != "before" {
		t.Fatalf("unexpected detail: %v", result)
	}
	if files, ok := result["changed_files"].([]any); !ok || len(files) != 0 {
		t.Fatalf("an unchanged tree must report an empty changed_files array: %v", result["changed_files"])
	}
}

func TestServerGetValidatesParamsBeforeResolvingTheRepository(t *testing.T) {
	for _, tc := range []struct {
		name    string
		params  map[string]any
		message string
	}{
		{"missing cwd", map[string]any{"id": wellFormedID}, "missing required param: cwd"},
		{"missing id", map[string]any{"cwd": "/not/a/repo"}, "missing required param: id"},
		{"numeric id", map[string]any{"cwd": "/not/a/repo", "id": 7}, "param must be a non-empty string: id"},
		{"blank id", map[string]any{"cwd": "/not/a/repo", "id": "  "}, "param must be a non-empty string: id"},
		{"path-like id", map[string]any{"cwd": "/not/a/repo", "id": "../../heads/main"}, "param must be a snapshot id: id"},
		{"latest alias", map[string]any{"cwd": "/not/a/repo", "id": "latest"}, "param must be a snapshot id: id"},
		{"unknown key", map[string]any{"cwd": "/not/a/repo", "id": wellFormedID, "limit": 5}, "unknown param: limit"},
	} {
		response := serveRequestFor(t, 1, "snapshot.get", tc.params)
		if code := errCode(t, response); code != CodeInvalidParams {
			t.Errorf("%s: want code %d, got %v: %v", tc.name, CodeInvalidParams, code, response)
			continue
		}
		if message := errMessage(t, response); message != tc.message {
			t.Errorf("%s: want message %q, got %q", tc.name, tc.message, message)
		}
	}
}

func TestServerGetOfAnUnknownSnapshotIsNotFound(t *testing.T) {
	dir := protocolRepo(t)
	response := serveRequestFor(t, 1, "snapshot.get", map[string]any{"cwd": dir, "id": wellFormedID})
	if code := errCode(t, response); code != CodeSnapshotNotFound {
		t.Fatalf("want code %d, got %v: %v", CodeSnapshotNotFound, code, response)
	}
	if message := errMessage(t, response); message != "snapshot not found: "+wellFormedID {
		t.Fatalf("unexpected message: %q", message)
	}
}

func TestServerGetOutsideARepositoryIsAnInternalError(t *testing.T) {
	response := serveRequestFor(t, 1, "snapshot.get", map[string]any{"cwd": t.TempDir(), "id": wellFormedID})
	if code := errCode(t, response); code != CodeInternalError {
		t.Fatalf("want code %d, got %v: %v", CodeInternalError, code, response)
	}
}
