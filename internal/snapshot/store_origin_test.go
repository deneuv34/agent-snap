package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func createFrom(t *testing.T, store *Store, reason string, origin Origin) Snapshot {
	t.Helper()
	snap, err := store.Create(context.Background(), reason, origin)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func storedMessage(t *testing.T, dir, commit string) string {
	t.Helper()
	raw := git(t, dir, "cat-file", "commit", commit)
	return raw[strings.Index(raw, "\n\n")+2:]
}

func listed(t *testing.T, store *Store, id string) Snapshot {
	t.Helper()
	items, err := store.List(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("snapshot %s is not listed", id)
	return Snapshot{}
}

// forgeItem writes an item ref to a commit carrying the agent-snap identity
// and an arbitrary message, as a hand-edited or future-version snapshot would.
func forgeItem(t *testing.T, dir string, store *Store, message string) string {
	t.Helper()
	cmd := exec.Command("git", "commit-tree", git(t, dir, "rev-parse", "HEAD^{tree}"), "-p", git(t, dir, "rev-parse", "HEAD"), "-F", "-")
	cmd.Dir = dir
	cmd.Env = append(sanitizedEnv(), authorEnv()...)
	cmd.Stdin = strings.NewReader(message)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(string(out))
	id := "20260101T000000.000000000Z-" + commit[:12]
	git(t, dir, "update-ref", store.itemsRef()+id, commit)
	return id
}

func TestCreateStoresOriginAndEveryReaderReturnsIt(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	origin := Origin{Source: "tool", Host: "pi"}
	snap := createFrom(t, store, "before refactor", origin)

	if want := "agent-snap: before refactor\n\nAgent-Snap-Source: tool\nAgent-Snap-Host: pi"; storedMessage(t, dir, snap.Commit) != want {
		t.Fatalf("commit message:\n%q\nwant\n%q", storedMessage(t, dir, snap.Commit), want)
	}
	if snap.Reason != "before refactor" || snap.Origin != origin {
		t.Fatalf("create result: %+v", snap)
	}
	if got := listed(t, store, snap.ID); got.Reason != "before refactor" || got.Origin != origin {
		t.Fatalf("list result: %+v", got)
	}
	if got := get(t, store, snap.ID); got.Reason != "before refactor" || got.Origin != origin {
		t.Fatalf("get result: %+v", got)
	}
	encoded, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"source":"tool","host":"pi"`) {
		t.Fatalf("origin must be flat source/host fields: %s", encoded)
	}
}

func TestCreateWritesOnlyTheOriginValuesSupplied(t *testing.T) {
	for _, tc := range []struct {
		origin Origin
		want   string
	}{
		{Origin{}, "agent-snap: plain"},
		{Origin{Source: "cli"}, "agent-snap: plain\n\nAgent-Snap-Source: cli"},
		{Origin{Host: "opencode"}, "agent-snap: plain\n\nAgent-Snap-Host: opencode"},
	} {
		dir := testRepo(t)
		store := newStore(t, dir)
		snap := createFrom(t, store, "plain", tc.origin)
		if got := storedMessage(t, dir, snap.Commit); got != tc.want {
			t.Errorf("%+v: commit message %q, want %q", tc.origin, got, tc.want)
		}
		if got := get(t, store, snap.ID); got.Reason != "plain" || got.Origin != tc.origin {
			t.Errorf("%+v: get returned %+v", tc.origin, got)
		}
		encoded, err := json.Marshal(get(t, store, snap.ID))
		if err != nil {
			t.Fatal(err)
		}
		if tc.origin == (Origin{}) && (strings.Contains(string(encoded), `"source"`) || strings.Contains(string(encoded), `"host"`)) {
			t.Errorf("a snapshot without origin must omit source and host: %s", encoded)
		}
	}
}

func TestReusedSnapshotReportsItsStoredOrigin(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	first := createFrom(t, store, "session-start", Origin{Source: "session-start", Host: "pi"})
	again := createFrom(t, store, "manual", Origin{Source: "tool", Host: "opencode"})

	if !again.Reused || again.ID != first.ID {
		t.Fatalf("an unchanged tree must reuse the latest snapshot: %+v", again)
	}
	if again.Reason != "session-start" || again.Origin != first.Origin {
		t.Fatalf("a reused snapshot must report its stored reason and origin: %+v", again)
	}
}

func TestReasonsRoundTripAroundTheOriginParagraph(t *testing.T) {
	reasons := []string{
		"x\n",
		"x\n\n",
		"line one\nline two",
		"x\n\nnot an origin line",
		"x\n\nagent-snap-source: tool",
		"x\n\nAgent-Snap-Source:  leading space",
		"x\n\nAgent-Snap-Source: trailing space ",
		"x\n\nAgent-Snap-Source: tööl",
		"x\n\nAgent-Snap-: tool",
		"x\n\nAgent-Snap-Source:tool",
		"x\r\n\r\nAgent-Snap-Source: tool",
		"x\n\nAgent-Snap-Source: tool\nfree text",
		"x\n\nAgent-Snap-Source: tool\nAgent-Snap-Source: cli",
		"x\n\nAgent-Snap-Source: tool\n",
		"ünïcode reason",
	}
	for _, origin := range []Origin{{}, {Source: "tool", Host: "pi"}} {
		for _, reason := range reasons {
			dir := testRepo(t)
			store := newStore(t, dir)
			snap := createFrom(t, store, reason, origin)
			for name, got := range map[string]Snapshot{"create": snap, "list": listed(t, store, snap.ID)} {
				if got.Reason != reason || got.Origin != origin {
					t.Errorf("%s with origin %+v: reason %q origin %+v, want %q", name, origin, got.Reason, got.Origin, reason)
				}
			}
			if got := get(t, store, snap.ID); got.Reason != reason || got.Origin != origin {
				t.Errorf("get with origin %+v: reason %q origin %+v, want %q", origin, got.Reason, got.Origin, reason)
			}
		}
	}
}

func TestCreateRejectsReasonsThatCannotReadBackUnchanged(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	create(t, store, "existing")
	before := repoState(t, dir)
	for _, reason := range []string{
		"x\n\nAgent-Snap-Source: tool",
		"\n\nAgent-Snap-Host: pi",
		"x\n\nAgent-Snap-Source: tool\nAgent-Snap-Host: pi",
		"x\n\nAgent-Snap-Future-Key: v2",
		"x\n\nAgent-Snap-Source: Not A Token",
		"x\n\nAgent-Snap-Ticket: ABC-1",
		"x\n\nAgent-Snap-URL2: https://example.com/a?b=c",
		"a\x00b",
	} {
		if ValidReason(reason) {
			t.Errorf("ValidReason(%q) = true", reason)
		}
		for _, origin := range []Origin{{}, {Source: "tool"}} {
			if _, err := store.Create(context.Background(), reason, origin); !errors.Is(err, ErrInvalidReason) {
				t.Errorf("reason %q origin %+v: got %v, want ErrInvalidReason", reason, origin, err)
			}
		}
	}
	if after := repoState(t, dir); after != before {
		t.Fatalf("a rejected create on an unchanged tree must not reuse or write anything:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestCreateRejectsInvalidOriginValues(t *testing.T) {
	for _, value := range []string{"a", "tool", "session-start", "a1-b2", strings.Repeat("a", 64)} {
		if !ValidOrigin(value) {
			t.Errorf("ValidOrigin(%q) = false", value)
		}
	}
	dir := testRepo(t)
	store := newStore(t, dir)
	before := repoState(t, dir)
	for _, value := range []string{" ", "Tool", "to ol", "tool-", "-tool", "a--b", "1tool", "ses_123", "tool\n", "tööl", "a/b", strings.Repeat("a", 65)} {
		if ValidOrigin(value) {
			t.Errorf("ValidOrigin(%q) = true", value)
		}
		for _, origin := range []Origin{{Source: value}, {Host: value}} {
			if _, err := store.Create(context.Background(), "x", origin); !errors.Is(err, ErrInvalidOrigin) {
				t.Errorf("origin %+v: got %v, want ErrInvalidOrigin", origin, err)
			}
		}
	}
	if after := repoState(t, dir); after != before {
		t.Fatalf("a rejected create changed repository state:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestReadersParseOnlyAWellFormedOriginParagraph(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	for _, tc := range []struct {
		name, message, reason string
		origin                Origin
	}{
		{"0.1.0 message", "agent-snap: legacy", "legacy", Origin{}},
		{"future key is skipped", "agent-snap: r\n\nAgent-Snap-Source: tool\nAgent-Snap-Model: m1\nAgent-Snap-Host: pi", "r", Origin{Source: "tool", Host: "pi"}},
		{"keys in any order", "agent-snap: r\n\nAgent-Snap-Host: pi\nAgent-Snap-Source: tool", "r", Origin{Source: "tool", Host: "pi"}},
		{"repeated key", "agent-snap: r\n\nAgent-Snap-Source: tool\nAgent-Snap-Source: cli", "r\n\nAgent-Snap-Source: tool\nAgent-Snap-Source: cli", Origin{}},
		{"invalid source value is not reported", "agent-snap: r\n\nAgent-Snap-Source: Tool\nAgent-Snap-Host: pi", "r", Origin{Host: "pi"}},
		{"future key with a wide value", "agent-snap: r\n\nAgent-Snap-Ticket: ABC-1 / 2.1\nAgent-Snap-URL2: x\nAgent-Snap-Source: tool", "r", Origin{Source: "tool"}},
		{"value with a trailing space", "agent-snap: r\n\nAgent-Snap-Source: tool ", "r\n\nAgent-Snap-Source: tool ", Origin{}},
		{"foreign line", "agent-snap: r\n\nAgent-Snap-Source: tool\nSigned-off-by: someone", "r\n\nAgent-Snap-Source: tool\nSigned-off-by: someone", Origin{}},
		{"trailing newline", "agent-snap: r\n\nAgent-Snap-Source: tool\n", "r\n\nAgent-Snap-Source: tool\n", Origin{}},
	} {
		id := forgeItem(t, dir, store, tc.message)
		if got := get(t, store, id); got.Reason != tc.reason || got.Origin != tc.origin {
			t.Errorf("%s: get reason %q origin %+v, want %q %+v", tc.name, got.Reason, got.Origin, tc.reason, tc.origin)
		}
		if got := listed(t, store, id); got.Reason != tc.reason || got.Origin != tc.origin {
			t.Errorf("%s: list reason %q origin %+v, want %q %+v", tc.name, got.Reason, got.Origin, tc.reason, tc.origin)
		}
	}
}

func TestOriginParsingIgnoresGitTrailerConfig(t *testing.T) {
	dir := testRepo(t)
	git(t, dir, "config", "trailer.separators", "#")
	git(t, dir, "config", "trailer.ifexists", "replace")
	store := newStore(t, dir)
	snap := createFrom(t, store, "configured", Origin{Source: "tool", Host: "pi"})
	if got := get(t, store, snap.ID); got.Reason != "configured" || got.Origin != (Origin{Source: "tool", Host: "pi"}) {
		t.Fatalf("trailer config must not change origin parsing: %+v", got)
	}
}
