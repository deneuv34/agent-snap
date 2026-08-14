package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCLIRejectsUnknownFlag(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"create", "--cwd", cliRepo(t), "--frobnicate"}, &out); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("expected unknown flag error, got: %v", err)
	}
}

func TestCLIRejectsMissingCwdValue(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"create", "--cwd"}, &out); err == nil || !strings.Contains(err.Error(), "requires a value") {
		t.Fatalf("expected missing value error, got: %v", err)
	}
}

func TestCLIRejectsInvalidLimit(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"list", "--cwd", cliRepo(t), "--limit", "many"}, &out); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("expected invalid limit error, got: %v", err)
	}
}

func TestCLIRejectsMultiplePositionals(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"create", "reason-one", "reason-two"}, &out); err == nil || !strings.Contains(err.Error(), "single") {
		t.Fatalf("expected multiple positionals error, got: %v", err)
	}
}

func TestCLIRejectsConflictingReasonFlagAndPositional(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"create", "--reason", "flagged", "positional"}, &out); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("expected conflicting reason error, got: %v", err)
	}
}

func TestCLIRejectsTrailingArgs(t *testing.T) {
	for _, args := range [][]string{{"ping", "extra"}, {"version", "extra"}, {"serve", "extra"}} {
		var out bytes.Buffer
		if err := run(args, &out); err == nil || !strings.Contains(err.Error(), "no arguments") {
			t.Fatalf("expected trailing-args error for %v, got: %v", args, err)
		}
	}
}
