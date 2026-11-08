package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func reusedName(t *testing.T, dir string) (store *Store, reused string, gone Snapshot) {
	t.Helper()
	parent := t.TempDir()
	first, second := filepath.Join(parent, "first", "wt"), filepath.Join(parent, "second", "wt")
	git(t, dir, "worktree", "add", "-b", "old-work", first)
	oldStore := newStore(t, first)
	writeFile(t, first, "old-work.txt", "work of the removed worktree\n")
	gone = create(t, oldStore, "old work")
	git(t, dir, "worktree", "remove", "--force", first)

	git(t, dir, "worktree", "add", "-b", "new-work", second)
	store = newStore(t, second)
	if store.gitDir != oldStore.gitDir {
		t.Fatalf("the new worktree must reuse the removed one's Git directory:\n got %s\nwant %s", store.gitDir, oldStore.gitDir)
	}
	return store, second, gone
}

func olderBinary(store *Store) *Store {
	older := *store
	older.born = time.Time{}
	return &older
}

func parentOf(t *testing.T, dir, commit string) string {
	t.Helper()
	return git(t, dir, "rev-parse", commit+"^")
}

func listIDs(t *testing.T, store *Store) []string {
	t.Helper()
	listed, err := store.List(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, snap := range listed {
		ids = append(ids, snap.ID)
	}
	return ids
}

func TestNewWorktreeUnderARemovedNameDoesNotInheritItsSnapshots(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		ctx := context.Background()
		store, reused, gone := reusedName(t, dir)

		listed, err := store.List(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if listed == nil || len(listed) != 0 {
			t.Errorf("list must answer an empty list, not null or a removed worktree's snapshots: %#v", listed)
		}
		if _, err := store.Get(ctx, gone.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("get of a removed worktree's snapshot: got %v, want %v", err, ErrNotFound)
		}
		if _, err := store.Diff(ctx, gone.ID, gone.ID, DiffPage{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("diff of a removed worktree's snapshot: got %v, want %v", err, ErrNotFound)
		}
		if _, err := store.PlanRestore(ctx, gone.ID, PlanQuery{Scope: WholeTree()}); !errors.Is(err, ErrNotFound) {
			t.Errorf("restore plan for a removed worktree's snapshot: got %v, want %v", err, ErrNotFound)
		}

		writeFile(t, reused, "old-work.txt", "work of the removed worktree\n")
		made, err := store.Create(ctx, "new work", Origin{})
		if err != nil {
			t.Fatal(err)
		}
		if made.Reused || made.ID == gone.ID {
			t.Errorf("create returned a removed worktree's snapshot for an identical tree: %+v", made)
		}
		if parent, head := parentOf(t, reused, made.Commit), git(t, reused, "rev-parse", "HEAD"); parent != head {
			t.Errorf("the worktree's first snapshot must start at HEAD: parent %s, HEAD %s", parent, head)
		}
		if latest := git(t, reused, "rev-parse", "--verify", store.latestRef()); latest != made.Commit {
			t.Errorf("latest must move to the new snapshot: got %s, want %s", latest, made.Commit)
		}

		writeFile(t, reused, "next.txt", "next\n")
		next := create(t, store, "next work")
		if parent := parentOf(t, reused, next.Commit); parent != made.Commit {
			t.Errorf("the second snapshot must chain onto the worktree's first: parent %s, want %s", parent, made.Commit)
		}
	})
}

func TestEveryWayOfFreeingANameStartsAnEmptyTimeline(t *testing.T) {
	routes := []struct {
		name string
		free func(t *testing.T, dir, first, second string) string
	}{
		{"remove then add at another path", func(t *testing.T, dir, first, second string) string {
			git(t, dir, "worktree", "remove", "--force", first)
			git(t, dir, "worktree", "add", "-b", "new-work", second)
			return second
		}},
		{"remove then add at the same path", func(t *testing.T, dir, first, _ string) string {
			git(t, dir, "worktree", "remove", "--force", first)
			git(t, dir, "worktree", "add", "-b", "new-work", first)
			return first
		}},
		{"delete and prune then add", func(t *testing.T, dir, first, second string) string {
			if err := os.RemoveAll(first); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "worktree", "prune")
			git(t, dir, "worktree", "add", "-b", "new-work", second)
			return second
		}},
		{"delete then force an add at the same path", func(t *testing.T, dir, first, _ string) string {
			if err := os.RemoveAll(first); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "worktree", "add", "-f", "-b", "new-work", first)
			return first
		}},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			dir := testRepo(t)
			parent := t.TempDir()
			first, second := filepath.Join(parent, "first", "wt"), filepath.Join(parent, "second", "wt")
			git(t, dir, "worktree", "add", "-b", "old-work", first)
			oldStore := newStore(t, first)
			create(t, oldStore, "old work")

			store := newStore(t, route.free(t, dir, first, second))
			if store.gitDir != oldStore.gitDir {
				t.Fatalf("this route must reuse the Git directory:\n got %s\nwant %s", store.gitDir, oldStore.gitDir)
			}
			if ids := listIDs(t, store); len(ids) != 0 {
				t.Fatalf("the new worktree inherited snapshots: %v", ids)
			}
		})
	}
}

func TestReusedNameLeavesTheRemovedWorktreesRefsInPlace(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		store, reused, gone := reusedName(t, dir)
		ns := namespaceOf(store.gitDir)
		ref := refsPrefix + ns + itemsSuffix + gone.ID

		writeFile(t, reused, "new-work.txt", "new\n")
		create(t, store, "new work")
		listIDs(t, store)

		if got := git(t, dir, "rev-parse", "--verify", ref); got != gone.Commit {
			t.Fatalf("the removed worktree's item ref moved: got %s, want %s", got, gone.Commit)
		}
		if entry := entryFor(t, reportOf(t, store), ns); entry.Snapshots != 2 {
			t.Fatalf("the report must count the removed worktree's snapshot and the new one: %+v", entry)
		}
	})
}

func TestWorktreeKeepsItsSnapshotsThroughGitMaintenance(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		ctx := context.Background()
		parent := t.TempDir()
		before, after := filepath.Join(parent, "before", "wt"), filepath.Join(parent, "moved")
		git(t, dir, "worktree", "add", "-b", "kept", before)
		kept := create(t, newStore(t, before), "kept")

		git(t, dir, "worktree", "lock", before)
		git(t, dir, "worktree", "unlock", before)
		git(t, dir, "worktree", "move", before, after)
		git(t, dir, "worktree", "repair", after)
		git(t, dir, "gc", "--prune=now")

		store := newStore(t, after)
		if ids := listIDs(t, store); len(ids) != 1 || ids[0] != kept.ID {
			t.Fatalf("a moved worktree lost its timeline: %v", ids)
		}
		if _, err := store.Get(ctx, kept.ID); err != nil {
			t.Fatalf("get after a move: %v", err)
		}
		again, err := store.Create(ctx, "unchanged", Origin{})
		if err != nil {
			t.Fatal(err)
		}
		if !again.Reused || again.ID != kept.ID {
			t.Fatalf("a create in a moved worktree must still reuse its own latest: %+v", again)
		}
	})
}

func TestBothBinariesShareAReusedNamespace(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		store, reused, gone := reusedName(t, dir)
		older := olderBinary(store)

		writeFile(t, reused, "a.txt", "a\n")
		first := create(t, store, "new binary")
		writeFile(t, reused, "b.txt", "b\n")
		second := create(t, older, "older binary")
		writeFile(t, reused, "c.txt", "c\n")
		third := create(t, store, "new binary again")

		if parent := parentOf(t, reused, second.Commit); parent != first.Commit {
			t.Errorf("the older binary must chain onto the new one's snapshot: parent %s, want %s", parent, first.Commit)
		}
		if parent := parentOf(t, reused, third.Commit); parent != second.Commit {
			t.Errorf("the new binary must chain onto the older one's snapshot: parent %s, want %s", parent, second.Commit)
		}
		if ids := listIDs(t, older); len(ids) != 4 || !strings.Contains(strings.Join(ids, " "), gone.ID) {
			t.Errorf("the older binary must keep listing every snapshot: %v", ids)
		}
		if ids := listIDs(t, store); len(ids) != 3 || strings.Contains(strings.Join(ids, " "), gone.ID) {
			t.Errorf("the new binary must list the worktree's own three snapshots: %v", ids)
		}
	})
}

func TestSnapshotFromBeforeTheFixKeepsItsCommitParentAndNoParentID(t *testing.T) {
	dir := testRepo(t)
	store, reused, gone := reusedName(t, dir)

	writeFile(t, reused, "post.txt", "post\n")
	chained := create(t, olderBinary(store), "chained before the fix")

	detail, err := store.Get(context.Background(), chained.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Parent != gone.Commit || detail.ParentID != "" {
		t.Fatalf("got parent %s and parent id %q, want parent %s and no parent id", detail.Parent, detail.ParentID, gone.Commit)
	}

	again, err := store.Create(context.Background(), "unchanged", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Reused || again.ID != chained.ID {
		t.Fatalf("a create must reuse a snapshot taken in this worktree: %+v", again)
	}
}

func TestConcurrentCreatesUnderAReusedNameStartOneChain(t *testing.T) {
	dir := testRepo(t)
	store, reused, gone := reusedName(t, dir)
	writeFile(t, reused, "same.txt", "same\n")

	const n = 8
	made := make([]Snapshot, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		racer := newStore(t, reused)
		wg.Add(1)
		go func() {
			defer wg.Done()
			made[i], errs[i] = racer.Create(context.Background(), fmt.Sprintf("racer-%d", i), Origin{})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
		if made[i].ID == gone.ID {
			t.Fatalf("racer %d got the removed worktree's snapshot", i)
		}
	}
	ids := listIDs(t, store)
	if len(ids) != 1 {
		t.Fatalf("racers over one tree must leave one snapshot: %v", ids)
	}
	for i := range made {
		if made[i].ID != ids[0] {
			t.Fatalf("racer %d got %s, want %s", i, made[i].ID, ids[0])
		}
	}
}

func TestCommondirTimeMovedForwardHidesSnapshotsAndDeletesNothing(t *testing.T) {
	dir := testRepo(t)
	path := addWorktree(t, dir, "clock")
	store := newStore(t, path)
	kept := create(t, store, "kept")
	ref := store.itemsRef() + kept.ID

	commondir := filepath.Join(store.gitDir, "commondir")
	info, err := os.Lstat(commondir)
	if err != nil {
		t.Fatal(err)
	}
	original := info.ModTime()
	forward := time.Now().Add(time.Hour)
	if err := os.Chtimes(commondir, forward, forward); err != nil {
		t.Fatal(err)
	}

	if ids := listIDs(t, newStore(t, path)); len(ids) != 0 {
		t.Fatalf("a snapshot older than the worktree's birth must be hidden: %v", ids)
	}
	if got := git(t, dir, "rev-parse", "--verify", ref); got != kept.Commit {
		t.Fatalf("the hidden snapshot's ref changed: got %s, want %s", got, kept.Commit)
	}

	if err := os.Chtimes(commondir, original, original); err != nil {
		t.Fatal(err)
	}
	if ids := listIDs(t, newStore(t, path)); len(ids) != 1 || ids[0] != kept.ID {
		t.Fatalf("setting the time back must show the snapshot again: %v", ids)
	}
}

func TestMainWorktreeHasNoBirth(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	if !store.born.IsZero() {
		t.Fatalf("a main worktree must have no birth: %v", store.born)
	}
	store.now = func() time.Time { return time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC) }
	old := create(t, store, "old clock")
	if ids := listIDs(t, store); len(ids) != 1 || ids[0] != old.ID {
		t.Fatalf("a main worktree must list a snapshot of any age: %v", ids)
	}
}

func TestInTimelineComparesAtTheIDsFullPrecision(t *testing.T) {
	born := time.Date(2026, 11, 9, 12, 0, 0, 500, time.UTC)
	const badDate = "20261399T000000.000000000Z-aaaaaaaaaaaa"
	idAt := func(at time.Time) string { return at.Format(idTimeLayout) + "-aaaaaaaaaaaa" }
	cases := []struct {
		name string
		born time.Time
		id   string
		want bool
	}{
		{"taken at the birth", born, idAt(born), true},
		{"one nanosecond after", born, idAt(born.Add(time.Nanosecond)), true},
		{"one nanosecond before", born, idAt(born.Add(-time.Nanosecond)), false},
		{"earlier in the same second", born, idAt(born.Truncate(time.Second)), false},
		{"a date no create mints", born, badDate, false},
		{"no birth, any age", time.Time{}, idAt(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)), true},
		{"no birth, a date no create mints", time.Time{}, badDate, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (&Store{born: c.born}).inTimeline(c.id); got != c.want {
				t.Fatalf("inTimeline(%s) = %v, want %v", c.id, got, c.want)
			}
		})
	}
}
