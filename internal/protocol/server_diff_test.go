package protocol

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const otherWellFormedID = "20260101T000000.000000000Z-ba9876543210"

func createdID(t *testing.T, dir string) string {
	t.Helper()
	response := serveRequestFor(t, 1, "snapshot.create", map[string]any{"cwd": dir})
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("create: %v", response)
	}
	return result["id"].(string)
}

func TestServerDiffReturnsOnePageOfTheDiff(t *testing.T) {
	dir := protocolRepo(t)
	from := createdID(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(strings.Repeat("x", 2000)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	to := createdID(t, dir)

	response := serveRequestFor(t, 2, "snapshot.diff", map[string]any{"cwd": dir, "from": from, "to": to})
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result, got: %v", response)
	}
	var keys []string
	for key := range result {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"files", "from", "offset", "to", "totals"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("result keys: got %v want %v", keys, want)
	}
	if side := result["from"].(map[string]any); side["id"] != from || len(side) != 2 {
		t.Fatalf("from side: %v", side)
	}
	files := result["files"].([]any)
	if len(files) != 2 || files[1].(map[string]any)["path"] != "large.txt" || files[1].(map[string]any)["patch"] != "full" {
		t.Fatalf("files: %v", files)
	}

	paged := serveRequestFor(t, 3, "snapshot.diff", map[string]any{"cwd": dir, "from": from, "to": to, "offset": 1, "max_patch_bytes": 1024})
	pagedResult := paged["result"].(map[string]any)
	truncated := pagedResult["files"].([]any)[0].(map[string]any)
	if pagedResult["offset"] != float64(1) || truncated["patch"] != "truncated" || truncated["patch_bytes"] != float64(len("@@ -0,0 +1 @@\n")+2002) || pagedResult["next_offset"] != nil {
		t.Fatalf("max_patch_bytes must reach the store: %v", pagedResult)
	}

	for _, params := range []map[string]any{
		{"cwd": dir, "from": from, "to": to, "offset": 2147483647, "max_patch_bytes": 1048576},
		{"cwd": dir, "from": to, "to": to, "offset": 1.0},
	} {
		past := serveRequestFor(t, 4, "snapshot.diff", params)
		if files, ok := past["result"].(map[string]any)["files"].([]any); !ok || len(files) != 0 {
			t.Fatalf("bounds must be accepted: %v %v", params, past)
		}
	}
}

func TestServerDiffValidatesParamsBeforeResolvingTheRepository(t *testing.T) {
	base := func(extra map[string]any) map[string]any {
		params := map[string]any{"cwd": "/not/a/repo", "from": wellFormedID, "to": otherWellFormedID}
		for key, value := range extra {
			if value == "<delete>" {
				delete(params, key)
			} else {
				params[key] = value
			}
		}
		return params
	}
	offsetMessage := "param must be an integer between 0 and 2147483647: offset"
	budgetMessage := "param must be an integer between 1024 and 1048576: max_patch_bytes"
	for _, tc := range []struct {
		name    string
		params  map[string]any
		message string
	}{
		{"unknown key", base(map[string]any{"limit": 5}), "unknown param: limit"},
		{"unknown id key", base(map[string]any{"id": wellFormedID}), "unknown param: id"},
		{"missing cwd", base(map[string]any{"cwd": "<delete>"}), "missing required param: cwd"},
		{"missing from", base(map[string]any{"from": "<delete>"}), "missing required param: from"},
		{"missing to", base(map[string]any{"to": "<delete>"}), "missing required param: to"},
		{"blank cwd", base(map[string]any{"cwd": " "}), "param must be a non-empty string: cwd"},
		{"numeric from", base(map[string]any{"from": 7}), "param must be a non-empty string: from"},
		{"blank to", base(map[string]any{"to": ""}), "param must be a non-empty string: to"},
		{"null to", base(map[string]any{"to": nil}), "param must be a non-empty string: to"},
		{"commit as from", base(map[string]any{"from": "0123456789abcdef0123456789abcdef01234567"}), "param must be a snapshot id: from"},
		{"revision as to", base(map[string]any{"to": "HEAD~1"}), "param must be a snapshot id: to"},
		{"negative offset", base(map[string]any{"offset": -1}), offsetMessage},
		{"offset too large", base(map[string]any{"offset": 2147483648}), offsetMessage},
		{"fractional offset", base(map[string]any{"offset": 1.5}), offsetMessage},
		{"string offset", base(map[string]any{"offset": "1"}), offsetMessage},
		{"null offset", base(map[string]any{"offset": nil}), offsetMessage},
		{"budget too small", base(map[string]any{"max_patch_bytes": 1023}), budgetMessage},
		{"budget too large", base(map[string]any{"max_patch_bytes": 1048577}), budgetMessage},
		{"zero budget", base(map[string]any{"max_patch_bytes": 0}), budgetMessage},
		{"fractional budget", base(map[string]any{"max_patch_bytes": 2048.5}), budgetMessage},
		{"boolean budget", base(map[string]any{"max_patch_bytes": true}), budgetMessage},
	} {
		response := serveRequestFor(t, 1, "snapshot.diff", tc.params)
		if code := errCode(t, response); code != CodeInvalidParams {
			t.Errorf("%s: want code %d, got %v: %v", tc.name, CodeInvalidParams, code, response)
			continue
		}
		if message := errMessage(t, response); message != tc.message {
			t.Errorf("%s: want message %q, got %q", tc.name, tc.message, message)
		}
	}
}

func TestServerDiffOfAnUnknownSnapshotIsNotFoundNamingFromFirst(t *testing.T) {
	dir := protocolRepo(t)
	known := createdID(t, dir)
	for _, tc := range []struct{ from, to, missing string }{
		{wellFormedID, otherWellFormedID, wellFormedID},
		{wellFormedID, known, wellFormedID},
		{known, otherWellFormedID, otherWellFormedID},
	} {
		response := serveRequestFor(t, 1, "snapshot.diff", map[string]any{"cwd": dir, "from": tc.from, "to": tc.to})
		if code := errCode(t, response); code != CodeSnapshotNotFound {
			t.Errorf("%s %s: want code %d, got %v", tc.from, tc.to, CodeSnapshotNotFound, code)
			continue
		}
		if message := errMessage(t, response); message != "snapshot not found: "+tc.missing {
			t.Errorf("%s %s: unexpected message %q", tc.from, tc.to, message)
		}
	}
}

func TestServerDiffOutsideARepositoryIsAnInternalError(t *testing.T) {
	response := serveRequestFor(t, 1, "snapshot.diff", map[string]any{"cwd": t.TempDir(), "from": wellFormedID, "to": otherWellFormedID})
	if code := errCode(t, response); code != CodeInternalError {
		t.Fatalf("want code %d, got %v: %v", CodeInternalError, code, response)
	}
}

func TestServerListKeepsItsLimitMessage(t *testing.T) {
	for _, limit := range []any{0, 101, 1.5, "5", nil} {
		response := serveRequestFor(t, 1, "snapshot.list", map[string]any{"cwd": "/tmp", "limit": limit})
		if message := errMessage(t, response); message != "param must be an integer between 1 and 100: limit" {
			t.Errorf("limit %v: got %q", limit, message)
		}
	}
}
