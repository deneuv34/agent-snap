package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/deneuv34/agent-snap/internal/snapshot"
)

// Version is the agent-snap host version. It is a variable so release builds
// can override it with -ldflags "-X github.com/deneuv34/agent-snap/internal/protocol.Version=x.y.z".
var Version = "0.1.0"

type Request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params map[string]any  `json:"params"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Response struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Result any             `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

const (
	CodeParseError       = -32700
	CodeMethodNotFound   = -32601
	CodeInvalidParams    = -32602
	CodeInternalError    = -32603
	CodeSnapshotNotFound = -32001
	CodeStalePlan        = -32002
)

// Serve returns only after the request in flight when ctx ends has seen the
// cancelled ctx and removed its scratch directories.
func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	lines := make(chan string)
	var readErr error
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		readErr = scanner.Err()
	}()
	enc := json.NewEncoder(out)
	for ctx.Err() == nil {
		var text string
		var ok bool
		select {
		case <-ctx.Done():
			return nil
		case text, ok = <-lines:
		}
		if !ok {
			if readErr != nil {
				return fmt.Errorf("read requests: %w", readErr)
			}
			return nil
		}
		line := strings.TrimSpace(text)
		if line == "" {
			continue
		}
		req, rerr := decodeRequest([]byte(line))
		var result any
		if rerr == nil {
			result, rerr = dispatch(ctx, &req)
		}
		if err := writeResponse(enc, req.ID, result, rerr); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	return nil
}

// decodeRequest checks the keys before it fills Request, because struct
// decoding folds case and would read "METHOD" as method and "ID" as id.
func decodeRequest(line []byte) (Request, *Error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(line, &keys); err != nil {
		return Request{ID: json.RawMessage("null")}, parseError(err)
	}
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		if key != "id" && key != "method" && key != "params" {
			return Request{ID: keys["id"]}, &Error{Code: CodeInvalidParams, Message: "unknown request key: " + key}
		}
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return Request{ID: json.RawMessage("null")}, parseError(err)
	}
	return req, nil
}

func parseError(err error) *Error {
	return &Error{Code: CodeParseError, Message: "parse error: " + err.Error()}
}

func dispatch(ctx context.Context, req *Request) (any, *Error) {
	switch req.Method {
	case "ping":
		if err := validateParams(req.Params); err != nil {
			return nil, err
		}
		return map[string]string{"status": "ok", "version": Version}, nil
	case "snapshot.create":
		return createSnapshot(ctx, req.Params)
	case "snapshot.list":
		return listSnapshots(ctx, req.Params)
	case "snapshot.get":
		return getSnapshot(ctx, req.Params)
	case "snapshot.diff":
		return diffSnapshots(ctx, req.Params)
	case "snapshot.restore_plan":
		return planRestore(ctx, req.Params)
	default:
		return nil, &Error{Code: CodeMethodNotFound, Message: "method not found"}
	}
}

// validateParams rejects unknown keys and mistyped values so a caller can
// never silently target a different repository than intended.
func validateParams(params map[string]any, allowed ...string) *Error {
	if params == nil {
		params = map[string]any{}
	}
	for key := range params {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return &Error{Code: CodeInvalidParams, Message: "unknown param: " + key}
		}
	}
	return nil
}

func requireString(params map[string]any, key string) (string, *Error) {
	raw, ok := params[key]
	if !ok {
		return "", &Error{Code: CodeInvalidParams, Message: "missing required param: " + key}
	}
	value, ok := raw.(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", &Error{Code: CodeInvalidParams, Message: "param must be a non-empty string: " + key}
	}
	return value, nil
}

func optionalString(params map[string]any, key string) (string, bool, *Error) {
	raw, ok := params[key]
	if !ok {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", false, &Error{Code: CodeInvalidParams, Message: "param must be a string: " + key}
	}
	return value, true, nil
}

func optionalOrigin(params map[string]any, key string) (string, *Error) {
	value, present, rerr := optionalString(params, key)
	if rerr != nil {
		return "", rerr
	}
	if present && !snapshot.ValidOrigin(value) {
		return "", &Error{Code: CodeInvalidParams, Message: "param must be an origin value: " + key}
	}
	return value, nil
}

func optionalInt(params map[string]any, key string, min, max int) (int, *Error) {
	raw, ok := params[key]
	if !ok {
		return 0, nil
	}
	value, ok := raw.(float64)
	if !ok || value != math.Trunc(value) || value < float64(min) || value > float64(max) {
		return 0, &Error{Code: CodeInvalidParams, Message: fmt.Sprintf("param must be an integer between %d and %d: %s", min, max, key)}
	}
	return int(value), nil
}

func requireSnapshotID(params map[string]any, key string) (string, *Error) {
	id, rerr := requireString(params, key)
	if rerr != nil {
		return "", rerr
	}
	if !snapshot.ValidID(id) {
		return "", &Error{Code: CodeInvalidParams, Message: "param must be a snapshot id: " + key}
	}
	return id, nil
}

func createSnapshot(ctx context.Context, params map[string]any) (any, *Error) {
	if err := validateParams(params, "cwd", "reason", "source", "host"); err != nil {
		return nil, err
	}
	cwd, rerr := requireString(params, "cwd")
	if rerr != nil {
		return nil, rerr
	}
	reason, _, rerr := optionalString(params, "reason")
	if rerr != nil {
		return nil, rerr
	}
	if !snapshot.ValidReason(reason) {
		return nil, &Error{Code: CodeInvalidParams, Message: "param must not contain a NUL byte or end with an origin paragraph: reason"}
	}
	source, rerr := optionalOrigin(params, "source")
	if rerr != nil {
		return nil, rerr
	}
	host, rerr := optionalOrigin(params, "host")
	if rerr != nil {
		return nil, rerr
	}
	store, err := snapshot.New(cwd)
	if err != nil {
		return nil, &Error{Code: CodeInternalError, Message: err.Error()}
	}
	snap, err := store.Create(ctx, reason, snapshot.Origin{Source: source, Host: host})
	if errors.Is(err, snapshot.ErrInvalidReason) || errors.Is(err, snapshot.ErrInvalidOrigin) {
		return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
	}
	if err != nil {
		return nil, &Error{Code: CodeInternalError, Message: err.Error()}
	}
	return snap, nil
}

func listSnapshots(ctx context.Context, params map[string]any) (any, *Error) {
	if err := validateParams(params, "cwd", "limit"); err != nil {
		return nil, err
	}
	cwd, rerr := requireString(params, "cwd")
	if rerr != nil {
		return nil, rerr
	}
	limit, rerr := optionalInt(params, "limit", 1, 100)
	if rerr != nil {
		return nil, rerr
	}
	store, err := snapshot.New(cwd)
	if err != nil {
		return nil, &Error{Code: CodeInternalError, Message: err.Error()}
	}
	if limit <= 0 {
		limit = 0
	}
	items, err := store.List(ctx, limit)
	if err != nil {
		return nil, &Error{Code: CodeInternalError, Message: err.Error()}
	}
	return map[string]any{"snapshots": items}, nil
}

func getSnapshot(ctx context.Context, params map[string]any) (any, *Error) {
	if err := validateParams(params, "cwd", "id"); err != nil {
		return nil, err
	}
	cwd, rerr := requireString(params, "cwd")
	if rerr != nil {
		return nil, rerr
	}
	id, rerr := requireSnapshotID(params, "id")
	if rerr != nil {
		return nil, rerr
	}
	store, err := snapshot.New(cwd)
	if err != nil {
		return nil, &Error{Code: CodeInternalError, Message: err.Error()}
	}
	detail, err := store.Get(ctx, id)
	if errors.Is(err, snapshot.ErrNotFound) {
		return nil, &Error{Code: CodeSnapshotNotFound, Message: err.Error()}
	}
	if err != nil {
		return nil, &Error{Code: CodeInternalError, Message: err.Error()}
	}
	return detail, nil
}

func diffSnapshots(ctx context.Context, params map[string]any) (any, *Error) {
	if err := validateParams(params, "cwd", "from", "to", "offset", "max_patch_bytes"); err != nil {
		return nil, err
	}
	cwd, rerr := requireString(params, "cwd")
	if rerr != nil {
		return nil, rerr
	}
	from, rerr := requireSnapshotID(params, "from")
	if rerr != nil {
		return nil, rerr
	}
	to, rerr := requireSnapshotID(params, "to")
	if rerr != nil {
		return nil, rerr
	}
	offset, rerr := optionalInt(params, "offset", 0, snapshot.MaxPageOffset)
	if rerr != nil {
		return nil, rerr
	}
	maxPatchBytes, rerr := optionalInt(params, "max_patch_bytes", snapshot.DiffMinPatchBytes, snapshot.DiffMaxPatchBytes)
	if rerr != nil {
		return nil, rerr
	}
	store, err := snapshot.New(cwd)
	if err != nil {
		return nil, &Error{Code: CodeInternalError, Message: err.Error()}
	}
	diff, err := store.Diff(ctx, from, to, snapshot.DiffPage{Offset: offset, MaxPatchBytes: maxPatchBytes})
	if errors.Is(err, snapshot.ErrNotFound) {
		return nil, &Error{Code: CodeSnapshotNotFound, Message: err.Error()}
	}
	if err != nil {
		return nil, &Error{Code: CodeInternalError, Message: err.Error()}
	}
	return diff, nil
}

var scopeMessages = []struct {
	err     error
	message string
}{
	{snapshot.ErrScopeSize, "param must hold 1 to 100 paths: paths"},
	{snapshot.ErrScopePath, "param must hold only repository paths: paths"},
	{snapshot.ErrScopeOverlap, "param must not hold overlapping paths: paths"},
}

func optionalScope(params map[string]any) (snapshot.Scope, *Error) {
	raw, ok := params["paths"]
	if !ok {
		return snapshot.WholeTree(), nil
	}
	items, ok := raw.([]any)
	if !ok {
		return snapshot.Scope{}, &Error{Code: CodeInvalidParams, Message: "param must be an array of strings: paths"}
	}
	paths := make([]string, len(items))
	for i, item := range items {
		path, ok := item.(string)
		if !ok {
			return snapshot.Scope{}, &Error{Code: CodeInvalidParams, Message: "param must be an array of strings: paths"}
		}
		paths[i] = path
	}
	scope, err := snapshot.ParseScope(paths)
	if err == nil {
		return scope, nil
	}
	for _, row := range scopeMessages {
		if errors.Is(err, row.err) {
			return snapshot.Scope{}, &Error{Code: CodeInvalidParams, Message: row.message}
		}
	}
	return snapshot.Scope{}, &Error{Code: CodeInvalidParams, Message: err.Error()}
}

func optionalObjectID(params map[string]any, key string) (string, *Error) {
	raw, ok := params[key]
	if !ok {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok || !snapshot.ValidObjectID(value) {
		return "", &Error{Code: CodeInvalidParams, Message: "param must be an object id: " + key}
	}
	return value, nil
}

func planRestore(ctx context.Context, params map[string]any) (any, *Error) {
	if err := validateParams(params, "cwd", "id", "paths", "offset", "current_tree"); err != nil {
		return nil, err
	}
	cwd, rerr := requireString(params, "cwd")
	if rerr != nil {
		return nil, rerr
	}
	id, rerr := requireSnapshotID(params, "id")
	if rerr != nil {
		return nil, rerr
	}
	scope, rerr := optionalScope(params)
	if rerr != nil {
		return nil, rerr
	}
	offset, rerr := optionalInt(params, "offset", 0, snapshot.MaxPageOffset)
	if rerr != nil {
		return nil, rerr
	}
	currentTree, rerr := optionalObjectID(params, "current_tree")
	if rerr != nil {
		return nil, rerr
	}
	if offset > 0 && currentTree == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "missing required param: current_tree"}
	}
	store, err := snapshot.New(cwd)
	if err != nil {
		return nil, &Error{Code: CodeInternalError, Message: err.Error()}
	}
	plan, err := store.PlanRestore(ctx, id, snapshot.PlanQuery{Scope: scope, Offset: offset, CurrentTree: currentTree})
	switch {
	case errors.Is(err, snapshot.ErrNotFound):
		return nil, &Error{Code: CodeSnapshotNotFound, Message: err.Error()}
	case errors.Is(err, snapshot.ErrStale):
		return nil, &Error{Code: CodeStalePlan, Message: err.Error()}
	case err != nil:
		return nil, &Error{Code: CodeInternalError, Message: err.Error()}
	}
	return plan, nil
}

func writeResponse(enc *json.Encoder, id json.RawMessage, result any, rerr *Error) error {
	resp := Response{ID: id, Result: result, Error: rerr}
	return enc.Encode(resp)
}
