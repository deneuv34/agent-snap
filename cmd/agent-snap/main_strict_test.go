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

func TestCLIGetRejectsBadArguments(t *testing.T) {
	const id = "20260101T000000.000000000Z-0123456789ab"
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"get"}, "exactly one snapshot id, got 0"},
		{[]string{"get", id, id}, "exactly one snapshot id, got 2"},
		{[]string{"get", id, "--limit", "5"}, "unknown flag"},
		{[]string{"get", id, "--cwd"}, "requires a value"},
		{[]string{"get", "../latest", "--cwd", "/not/a/repo"}, "invalid snapshot id"},
	} {
		var out bytes.Buffer
		if err := run(tc.args, &out); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: expected an error containing %q, got: %v", tc.args, tc.want, err)
		}
	}
}

func TestCLIDiffRejectsBadArgumentsBeforeResolvingTheRepository(t *testing.T) {
	const id = "20260101T000000.000000000Z-0123456789ab"
	const other = "20260101T000000.000000000Z-ba9876543210"
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"diff", id}, "expected two snapshot ids, got 1"},
		{[]string{"diff", id, other, id}, "expected two snapshot ids, got 3"},
		{[]string{"diff", id, other, "--limit", "5"}, "unknown flag"},
		{[]string{"diff", id, other, "--offset"}, "requires a value"},
		{[]string{"diff", id, other, "--max-patch-bytes"}, "requires a value"},
		{[]string{"diff", id, other, "--offset", "-1"}, "invalid value for --offset"},
		{[]string{"diff", id, other, "--offset", "2147483648"}, "invalid value for --offset"},
		{[]string{"diff", id, other, "--offset", "1.5"}, "invalid value for --offset"},
		{[]string{"diff", id, other, "--max-patch-bytes", "1023"}, "invalid value for --max-patch-bytes"},
		{[]string{"diff", id, other, "--max-patch-bytes", "1048577"}, "invalid value for --max-patch-bytes"},
		{[]string{"diff", "HEAD", other}, "invalid snapshot id"},
		{[]string{"diff", id, "0123456789abcdef0123456789abcdef01234567"}, "invalid snapshot id"},
	} {
		var out bytes.Buffer
		if err := run(append([]string{"diff", "--cwd", "/not/a/repo"}, tc.args[1:]...), &out); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: expected an error containing %q, got: %v", tc.args, tc.want, err)
		}
	}
	var out bytes.Buffer
	if err := run([]string{"diff", id, other, "--offset", "2147483647", "--max-patch-bytes", "1048576", "--cwd", "/not/a/repo"}, &out); err == nil || !strings.Contains(err.Error(), "find git repository") {
		t.Errorf("in-range values reach the repository lookup, got: %v", err)
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
