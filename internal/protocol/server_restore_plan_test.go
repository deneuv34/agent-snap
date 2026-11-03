package protocol

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func planResult(t *testing.T, params map[string]any) map[string]any {
	t.Helper()
	response := serveRequestFor(t, 2, "snapshot.restore_plan", params)
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a plan, got: %v", response)
	}
	return result
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func entrySummary(t *testing.T, result map[string]any) []string {
	t.Helper()
	var summary []string
	for _, raw := range result["entries"].([]any) {
		entry := raw.(map[string]any)
		summary = append(summary, fmt.Sprintf("%s %s", entry["action"], entry["path"]))
	}
	return summary
}

func TestServerRestorePlanDescribesTheMoveBackToTheSnapshot(t *testing.T) {
	dir := protocolRepo(t)
	id := createdID(t, dir)
	writeFiles(t, dir, "file.txt", "new.txt")

	whole := planResult(t, map[string]any{"cwd": dir, "id": id})
	wantKeys := []string{"applicable", "blockers", "current_tree", "entries", "offset", "result_tree", "snapshot", "totals", "unmatched"}
	if got := sortedKeys(whole); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("whole-tree keys: got %v want %v", got, wantKeys)
	}
	if got, want := entrySummary(t, whole), []string{"modify file.txt", "delete new.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("entries: got %v want %v", got, want)
	}
	if whole["applicable"] != true || whole["offset"] != float64(0) || whole["snapshot"].(map[string]any)["id"] != id {
		t.Fatalf("plan header: %v", whole)
	}
	totals := whole["totals"].(map[string]any)
	if totals["entries"] != float64(2) || totals["modify"] != float64(1) || totals["delete"] != float64(1) ||
		totals["create"] != float64(0) || totals["unstaged"] != float64(1) || totals["untracked"] != float64(1) || totals["blocked"] != float64(0) {
		t.Fatalf("totals: %v", totals)
	}
	if blockers := whole["blockers"].([]any); len(blockers) != 0 {
		t.Fatalf("blockers: %v", blockers)
	}

	scoped := planResult(t, map[string]any{"cwd": dir, "id": id, "paths": []string{"new.txt"}})
	wantKeys = []string{"applicable", "blockers", "current_tree", "entries", "offset", "paths", "result_tree", "snapshot", "totals", "unmatched"}
	if got := sortedKeys(scoped); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("scoped keys: got %v want %v", got, wantKeys)
	}
	if got, want := entrySummary(t, scoped), []string{"delete new.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped entries: got %v want %v", got, want)
	}
	if !reflect.DeepEqual(scoped["paths"], []any{"new.txt"}) || scoped["totals"].(map[string]any)["entries"] != float64(1) {
		t.Fatalf("scoped plan: %v", scoped)
	}
	if scoped["current_tree"] != whole["current_tree"] || scoped["result_tree"] == whole["result_tree"] {
		t.Fatalf("scoped trees: %v %v", scoped, whole)
	}

	missing := planResult(t, map[string]any{"cwd": dir, "id": id, "paths": []string{"absent.txt"}})
	if missing["applicable"] != false || !reflect.DeepEqual(missing["unmatched"], []any{"absent.txt"}) {
		t.Fatalf("unmatched scope path: %v", missing)
	}
}

func TestServerRestorePlanPagesAndRejectsAStaleTree(t *testing.T) {
	dir := protocolRepo(t)
	id := createdID(t, dir)
	var names []string
	for i := 0; i < 250; i++ {
		names = append(names, fmt.Sprintf("f%03d.txt", i))
	}
	writeFiles(t, dir, names...)

	first := planResult(t, map[string]any{"cwd": dir, "id": id})
	if len(first["entries"].([]any)) != 200 || first["next_offset"] != float64(200) || first["totals"].(map[string]any)["entries"] != float64(250) {
		t.Fatalf("first page: %d entries, next_offset %v", len(first["entries"].([]any)), first["next_offset"])
	}
	tree := first["current_tree"].(string)

	rest := planResult(t, map[string]any{"cwd": dir, "id": id, "offset": 200, "current_tree": tree})
	entries := rest["entries"].([]any)
	if len(entries) != 50 || rest["offset"] != float64(200) || rest["current_tree"] != tree || entries[0].(map[string]any)["path"] != "f200.txt" {
		t.Fatalf("second page: %v", rest)
	}
	if _, present := rest["next_offset"]; present {
		t.Fatalf("last page must omit next_offset: %v", rest)
	}

	writeFiles(t, dir, "late.txt")
	response := serveRequestFor(t, 3, "snapshot.restore_plan", map[string]any{"cwd": dir, "id": id, "offset": 200, "current_tree": tree})
	if code := errCode(t, response); code != CodeStalePlan {
		t.Fatalf("want code %d, got %v: %v", CodeStalePlan, code, response)
	}
	if message := errMessage(t, response); message != "working tree changed since the plan" {
		t.Fatalf("stale message: %q", message)
	}
	if _, present := response["result"]; present {
		t.Fatalf("a stale page must list nothing: %v", response)
	}
}

func TestServerRestorePlanOfAnUnknownSnapshotIsNotFound(t *testing.T) {
	dir := protocolRepo(t)
	response := serveRequestFor(t, 1, "snapshot.restore_plan", map[string]any{"cwd": dir, "id": wellFormedID})
	if code := errCode(t, response); code != CodeSnapshotNotFound {
		t.Fatalf("want code %d, got %v: %v", CodeSnapshotNotFound, code, response)
	}
	if message := errMessage(t, response); message != "snapshot not found: "+wellFormedID {
		t.Fatalf("message: %q", message)
	}
}

func TestServerRestorePlanOutsideARepositoryIsAnInternalError(t *testing.T) {
	response := serveRequestFor(t, 1, "snapshot.restore_plan", map[string]any{"cwd": t.TempDir(), "id": wellFormedID})
	if code := errCode(t, response); code != CodeInternalError {
		t.Fatalf("want code %d, got %v: %v", CodeInternalError, code, response)
	}
}

var restorePlanInvalidRows = []struct {
	name    string
	extra   map[string]any
	message string
}{
	{"unknown key", map[string]any{"limit": 5}, "unknown param: limit"},
	{"unknown key before bad cwd", map[string]any{"limit": 5, "cwd": 7}, "unknown param: limit"},
	{"missing cwd", map[string]any{"cwd": "<delete>"}, "missing required param: cwd"},
	{"blank cwd", map[string]any{"cwd": " "}, "param must be a non-empty string: cwd"},
	{"numeric cwd", map[string]any{"cwd": 7}, "param must be a non-empty string: cwd"},
	{"bad cwd before bad id", map[string]any{"cwd": " ", "id": "HEAD"}, "param must be a non-empty string: cwd"},
	{"missing id", map[string]any{"id": "<delete>"}, "missing required param: id"},
	{"blank id", map[string]any{"id": ""}, "param must be a non-empty string: id"},
	{"revision as id", map[string]any{"id": "HEAD~1"}, "param must be a snapshot id: id"},
	{"bad id before bad paths", map[string]any{"id": "HEAD", "paths": []any{}}, "param must be a snapshot id: id"},
	{"paths is a string", map[string]any{"paths": "src"}, "param must be an array of strings: paths"},
	{"paths is null", map[string]any{"paths": nil}, "param must be an array of strings: paths"},
	{"paths is an object", map[string]any{"paths": map[string]any{"a": 1}}, "param must be an array of strings: paths"},
	{"paths holds a number", map[string]any{"paths": []any{"a", 1}}, "param must be an array of strings: paths"},
	{"paths holds null", map[string]any{"paths": []any{nil}}, "param must be an array of strings: paths"},
	{"non-string paths before bad path", map[string]any{"paths": []any{"../x", 1}}, "param must be an array of strings: paths"},
	{"empty paths", map[string]any{"paths": []any{}}, "param must hold 1 to 100 paths: paths"},
	{"101 paths", map[string]any{"paths": numberedPaths(101)}, "param must hold 1 to 100 paths: paths"},
	{"empty segment", map[string]any{"paths": []any{"a//b"}}, "param must hold only repository paths: paths"},
	{"blank path", map[string]any{"paths": []any{""}}, "param must hold only repository paths: paths"},
	{"trailing slash", map[string]any{"paths": []any{"a/"}}, "param must hold only repository paths: paths"},
	{"dot segment", map[string]any{"paths": []any{"a/./b"}}, "param must hold only repository paths: paths"},
	{"parent segment", map[string]any{"paths": []any{"a/../b"}}, "param must hold only repository paths: paths"},
	{"NUL byte", map[string]any{"paths": []any{"a\x00b"}}, "param must hold only repository paths: paths"},
	{"bad path before overlap", map[string]any{"paths": []any{"src", "src/a", ".."}}, "param must hold only repository paths: paths"},
	{"parent covers child", map[string]any{"paths": []any{"src", "src/a"}}, "param must not hold overlapping paths: paths"},
	{"duplicate path", map[string]any{"paths": []any{"src", "src"}}, "param must not hold overlapping paths: paths"},
	{"bad paths before bad offset", map[string]any{"paths": []any{}, "offset": -1}, "param must hold 1 to 100 paths: paths"},
	{"negative offset", map[string]any{"offset": -1}, offsetRangeMessage},
	{"fractional offset", map[string]any{"offset": 1.5}, offsetRangeMessage},
	{"offset too large", map[string]any{"offset": 2147483648}, offsetRangeMessage},
	{"string offset", map[string]any{"offset": "1"}, offsetRangeMessage},
	{"null offset", map[string]any{"offset": nil}, offsetRangeMessage},
	{"bad offset before bad current_tree", map[string]any{"offset": -1, "current_tree": "x"}, offsetRangeMessage},
	{"short current_tree", map[string]any{"current_tree": strings.Repeat("a", 39)}, "param must be an object id: current_tree"},
	{"long current_tree", map[string]any{"current_tree": strings.Repeat("a", 41)}, "param must be an object id: current_tree"},
	{"uppercase current_tree", map[string]any{"current_tree": strings.Repeat("A", 40)}, "param must be an object id: current_tree"},
	{"numeric current_tree", map[string]any{"current_tree": 7}, "param must be an object id: current_tree"},
	{"empty current_tree", map[string]any{"current_tree": ""}, "param must be an object id: current_tree"},
	{"null current_tree", map[string]any{"current_tree": nil}, "param must be an object id: current_tree"},
	{"bad current_tree before missing current_tree", map[string]any{"offset": 1, "current_tree": 7}, "param must be an object id: current_tree"},
	{"offset without current_tree", map[string]any{"offset": 1}, "missing required param: current_tree"},
}

const offsetRangeMessage = "param must be an integer between 0 and 2147483647: offset"

func numberedPaths(n int) []any {
	paths := make([]any, n)
	for i := range paths {
		paths[i] = fmt.Sprintf("p%d", i)
	}
	return paths
}

func restorePlanParams(cwd string, extra map[string]any) map[string]any {
	params := map[string]any{"cwd": cwd, "id": wellFormedID}
	for key, value := range extra {
		if value == "<delete>" {
			delete(params, key)
		} else {
			params[key] = value
		}
	}
	return params
}

func TestServerRestorePlanValidatesParamsBeforeResolvingTheRepository(t *testing.T) {
	for _, tc := range restorePlanInvalidRows {
		response := serveRequestFor(t, 1, "snapshot.restore_plan", restorePlanParams("/not/a/repo", tc.extra))
		if code := errCode(t, response); code != CodeInvalidParams {
			t.Errorf("%s: want code %d, got %v: %v", tc.name, CodeInvalidParams, code, response)
			continue
		}
		if message := errMessage(t, response); message != tc.message {
			t.Errorf("%s: want message %q, got %q", tc.name, tc.message, message)
		}
	}
}

func TestServerRestorePlanNeverTurnsABadScopeIntoAWholeTreePlan(t *testing.T) {
	dir := protocolRepo(t)
	id := createdID(t, dir)
	writeFiles(t, dir, "file.txt")
	for _, tc := range restorePlanInvalidRows {
		if _, scoped := tc.extra["paths"]; !scoped || tc.extra["id"] != nil || tc.extra["offset"] != nil {
			continue
		}
		params := restorePlanParams(dir, tc.extra)
		params["id"] = id
		response := serveRequestFor(t, 1, "snapshot.restore_plan", params)
		if code := errCode(t, response); code != CodeInvalidParams {
			t.Errorf("%s: want code %d, got %v: %v", tc.name, CodeInvalidParams, code, response)
		}
		if _, planned := response["result"]; planned {
			t.Errorf("%s: a bad scope produced a plan: %v", tc.name, response)
		}
	}
}
