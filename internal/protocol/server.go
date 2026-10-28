package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			if err := writeResponse(enc, json.RawMessage("null"), nil, &Error{Code: CodeParseError, Message: "parse error: " + err.Error()}); err != nil {
				return fmt.Errorf("write response: %w", err)
			}
			continue
		}
		result, rerr := dispatch(ctx, &req)
		if err := writeResponse(enc, req.ID, result, rerr); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read requests: %w", err)
	}
	return nil
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

func optionalLimit(params map[string]any) (int, *Error) {
	raw, ok := params["limit"]
	if !ok {
		return 0, nil
	}
	value, ok := raw.(float64)
	if !ok || value != float64(int(value)) || int(value) < 1 || int(value) > 100 {
		return 0, &Error{Code: CodeInvalidParams, Message: "param must be an integer between 1 and 100: limit"}
	}
	return int(value), nil
}

func createSnapshot(ctx context.Context, params map[string]any) (any, *Error) {
	if err := validateParams(params, "cwd", "reason"); err != nil {
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
	store, err := snapshot.New(cwd)
	if err != nil {
		return nil, &Error{Code: CodeInternalError, Message: err.Error()}
	}
	snap, err := store.Create(ctx, reason)
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
	limit, rerr := optionalLimit(params)
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
	id, rerr := requireString(params, "id")
	if rerr != nil {
		return nil, rerr
	}
	if !snapshot.ValidID(id) {
		return nil, &Error{Code: CodeInvalidParams, Message: "param must be a snapshot id: id"}
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

func writeResponse(enc *json.Encoder, id json.RawMessage, result any, rerr *Error) error {
	resp := Response{ID: id, Result: result, Error: rerr}
	return enc.Encode(resp)
}
