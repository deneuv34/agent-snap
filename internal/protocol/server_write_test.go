package protocol

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type failingWriter struct {
	writes int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("broken pipe")
	}
	return len(p), nil
}

func TestServePropagatesWriteErrors(t *testing.T) {
	input := "{\"id\":1,\"method\":\"ping\"}\n{\"id\":2,\"method\":\"ping\"}\n"
	var out failingWriter
	err := Serve(context.Background(), strings.NewReader(input), &out)
	if err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("expected write error to propagate, got: %v", err)
	}
}
