package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(sanitizedEnv(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func testRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "tracked.txt")
	git(t, dir, "commit", "-m", "initial")
	return dir
}

func TestCreateCapturesWorkingTreeWithoutChangingHeadOrIndex(t *testing.T) {
	dir := testRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("working\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "staged.txt")

	headBefore := git(t, dir, "rev-parse", "HEAD")
	indexBefore := git(t, dir, "write-tree")

	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create(context.Background(), "turn-end", Origin{})
	if err != nil {
		t.Fatal(err)
	}

	if snap.ID == "" || snap.Commit == "" {
		t.Fatalf("missing snapshot identity: %+v", snap)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD changed: got %s want %s", got, headBefore)
	}
	if got := git(t, dir, "write-tree"); got != indexBefore {
		t.Fatalf("index changed: got %s want %s", got, indexBefore)
	}
	if got := git(t, dir, "show", snap.Commit+":tracked.txt"); got != "working" {
		t.Fatalf("tracked content not captured: %q", got)
	}
	if got := git(t, dir, "show", snap.Commit+":untracked.txt"); got != "new" {
		t.Fatalf("untracked content not captured: %q", got)
	}
}

func TestCreateDeduplicatesUnchangedTreeAndListReturnsNewestFirst(t *testing.T) {
	dir := testRepo(t)
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(context.Background(), "baseline", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(context.Background(), "idle", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused || second.Commit != first.Commit {
		t.Fatalf("expected unchanged tree reuse; first=%+v second=%+v", first, second)
	}

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := store.Create(context.Background(), "after-edit", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d snapshots, want 2: %+v", len(items), items)
	}
	if items[0].Commit != third.Commit || items[1].Commit != first.Commit {
		t.Fatalf("unexpected order: %+v", items)
	}
}

func TestCreateRecordsANewSnapshotWhenLatestsItemIsGone(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			store := newStore(t, dir)
			first := create(t, store, "baseline")
			git(t, dir, "update-ref", "-d", store.itemsRef()+first.ID)

			again, err := store.Create(context.Background(), "idle", Origin{})
			if err != nil {
				t.Fatalf("an unchanged create after the latest item ref was deleted: %v", err)
			}
			if again.Reused || again.ID == first.ID {
				t.Fatalf("the create must record a new snapshot: %+v", again)
			}
			if got := git(t, dir, "rev-parse", store.latestRef()); got != again.Commit {
				t.Fatalf("latest is %s, want the new snapshot %s", got, again.Commit)
			}
			if got := get(t, store, again.ID); got.Tree != git(t, dir, "rev-parse", first.Commit+"^{tree}") || got.Parent != first.Commit || got.ParentID != "" {
				t.Fatalf("the new snapshot must capture the same tree on top of the old latest: %+v", got)
			}
			items, err := store.List(context.Background(), 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].ID != again.ID {
				t.Fatalf("list must show the new snapshot: %+v", items)
			}
			if git(t, dir, "for-each-ref", store.itemsRef()+first.ID) != "" {
				t.Fatalf("the deleted item ref must stay deleted")
			}

			if third := create(t, store, "idle"); !third.Reused || third.ID != again.ID {
				t.Fatalf("the next unchanged create must reuse the new snapshot: %+v", third)
			}
		})
	}
}

func TestCreateReusesOnlyAnItemThatLookupAccepts(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			store := newStore(t, dir)
			first := create(t, store, "baseline")
			alias := "20000101T000000.000000000Z-000000000000"
			git(t, dir, "update-ref", store.itemsRef()+alias, first.Commit)

			if again := create(t, store, "idle"); !again.Reused || again.ID != first.ID {
				t.Fatalf("a reuse must report the item that Get accepts, not %s: %+v", alias, again)
			}

			git(t, dir, "update-ref", "-d", store.itemsRef()+first.ID)
			again := create(t, store, "idle")
			if again.Reused || again.ID == alias || again.ID == first.ID {
				t.Fatalf("an item that Get refuses must not be reused: %+v", again)
			}
			if got := get(t, store, again.ID); got.Parent != first.Commit {
				t.Fatalf("the new snapshot must sit on top of the old latest: %+v", got)
			}
			if got := git(t, dir, "rev-parse", store.itemsRef()+alias); got != first.Commit {
				t.Fatalf("the refused item ref must stay as it was: %s", got)
			}
		})
	}
}

func TestCappedBufferKeepsTheLastBytesAndCountsTheRest(t *testing.T) {
	cases := []struct {
		name   string
		chunks []int
		want   int
	}{
		{"under the cap", []int{10, 20}, 0},
		{"at the cap", []int{stderrLimit}, 0},
		{"one past the cap", []int{stderrLimit, 1}, 1},
		{"past twice the cap in small writes", slices.Repeat([]int{1000}, 30), 30*1000 - stderrLimit},
		{"one large write", []int{5 * stderrLimit}, 4 * stderrLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b cappedBuffer
			var all []byte
			for i, n := range tc.chunks {
				chunk := bytes.Repeat([]byte{byte('a' + i%26)}, n)
				all = append(all, chunk...)
				if got, err := b.Write(chunk); got != n || err != nil {
					t.Fatalf("Write = %d, %v", got, err)
				}
			}
			want := string(all[tc.want:])
			if tc.want > 0 {
				want = fmt.Sprintf("[%d earlier bytes of standard error omitted] %s", tc.want, want)
			}
			if got := b.String(); got != want {
				t.Fatalf("String() is %d bytes, want %d", len(got), len(want))
			}
		})
	}
}
