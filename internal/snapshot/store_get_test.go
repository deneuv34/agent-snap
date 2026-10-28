package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newStore(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func create(t *testing.T, store *Store, reason string) Snapshot {
	t.Helper()
	snap, err := store.Create(context.Background(), reason)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func get(t *testing.T, store *Store, id string) Detail {
	t.Helper()
	detail, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return detail
}

// repoState fingerprints everything lookup must leave alone: HEAD, the real
// index, the working tree, every ref, and the object store.
func repoState(t *testing.T, dir string) string {
	t.Helper()
	return strings.Join([]string{
		git(t, dir, "rev-parse", "HEAD"),
		git(t, dir, "ls-files", "--stage"),
		git(t, dir, "--no-optional-locks", "status", "--porcelain", "--untracked-files=all"),
		git(t, dir, "for-each-ref"),
		git(t, dir, "count-objects", "-v"),
	}, "\n")
}

func assertDetail(t *testing.T, got, want Detail) {
	t.Helper()
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("created_at: got %v want %v", got.CreatedAt, want.CreatedAt)
	}
	got.CreatedAt, want.CreatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("detail mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestGetDescribesEachSnapshotInTheChainWithoutWriting(t *testing.T) {
	dir := testRepo(t)
	head := git(t, dir, "rev-parse", "HEAD")
	writeFile(t, dir, "tracked.txt", "working\n")
	writeFile(t, dir, "untracked.txt", "new\n")
	store := newStore(t, dir)
	first := create(t, store, "baseline")
	writeFile(t, dir, "tracked.txt", "edited\n")
	second := create(t, store, "edit")
	listed, err := store.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}

	before := repoState(t, dir)
	gotFirst := get(t, store, first.ID)
	gotSecond := get(t, store, second.ID)
	if after := repoState(t, dir); after != before {
		t.Fatalf("lookup changed repository state:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	assertDetail(t, gotFirst, Detail{
		ID: first.ID, Commit: first.Commit, Reason: "baseline", CreatedAt: listed[1].CreatedAt,
		Tree:         git(t, dir, "rev-parse", first.Commit+"^{tree}"),
		Parent:       head,
		ChangedFiles: []string{"tracked.txt", "untracked.txt"},
	})
	assertDetail(t, gotSecond, Detail{
		ID: second.ID, Commit: second.Commit, Reason: "edit", CreatedAt: listed[0].CreatedAt,
		Tree:         git(t, dir, "rev-parse", second.Commit+"^{tree}"),
		Parent:       first.Commit,
		ParentID:     first.ID,
		ChangedFiles: []string{"tracked.txt"},
	})
}

func TestGetReturnsTheReasonExactlyAsStored(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	reason := "checkpoint | after pipe\nsecond line & more\n"
	snap := create(t, store, reason)
	if got := get(t, store, snap.ID).Reason; got != reason {
		t.Fatalf("reason: got %q want %q", got, reason)
	}
}

func TestGetRootSnapshotHasNoParentAndListsEveryPath(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	writeFile(t, dir, "a.txt", "a\n")
	writeFile(t, dir, "sub/b.txt", "b\n")
	store := newStore(t, dir)
	snap := create(t, store, "root")

	got := get(t, store, snap.ID)
	if got.Parent != "" || got.ParentID != "" {
		t.Fatalf("root snapshot must have no parent: %+v", got)
	}
	if !reflect.DeepEqual(got.ChangedFiles, []string{"a.txt", "sub/b.txt"}) {
		t.Fatalf("root snapshot must list every path: %+v", got.ChangedFiles)
	}
}

func TestGetOfAnUnchangedTreeReportsAnEmptyChangedFilesList(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "clean")

	got := get(t, store, snap.ID)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"changed_files":[]`) || strings.Contains(string(encoded), "changed_files_truncated") {
		t.Fatalf("changed_files must always be present and truncation omitted: %s", encoded)
	}
}

func TestGetRejectsMalformedIDsBeforeBuildingARef(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "real")
	if !ValidID(snap.ID) {
		t.Fatalf("Create minted an id that ValidID rejects: %q", snap.ID)
	}
	for _, id := range []string{
		"",
		"latest",
		"../latest",
		"../../../heads/main",
		snap.ID + "/child",
		snap.ID + "\n",
		" " + snap.ID,
		snap.ID[:len(snap.ID)-1],
		snap.ID + "0",
		"20260101T000000.000000000Z-0123456789AB",
		"20260101T000000.000000000Z-0123456789a*",
		"20260101T000000Z-0123456789ab",
	} {
		if _, err := store.Get(context.Background(), id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Get(%q): got %v, want ErrInvalidID", id, err)
		}
	}
}

func TestGetOfAWellFormedUnknownIDIsNotFound(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	const id = "20260101T000000.000000000Z-0123456789ab"
	for _, history := range []string{"empty", "non-empty"} {
		if history == "non-empty" {
			create(t, store, "one")
		}
		_, err := store.Get(context.Background(), id)
		if !errors.Is(err, ErrNotFound) || err.Error() != "snapshot not found: "+id {
			t.Fatalf("%s history: got %v, want ErrNotFound naming the id", history, err)
		}
	}
}

func TestGetDoesNotResolveAnotherWorktreesSnapshot(t *testing.T) {
	dir := testRepo(t)
	linked := t.TempDir()
	git(t, dir, "worktree", "add", "-b", "feature", linked)
	storeMain := newStore(t, dir)
	storeLinked := newStore(t, linked)
	snap := create(t, storeLinked, "linked")

	if _, err := storeMain.Get(context.Background(), snap.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("main worktree must not resolve a linked worktree snapshot: %v", err)
	}
	if got := get(t, storeLinked, snap.ID); got.Commit != snap.Commit {
		t.Fatalf("linked worktree must resolve its own snapshot: %+v", got)
	}
}

func TestGetFailsClosedOnTamperedItemRefs(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "real")
	head := git(t, dir, "rev-parse", "HEAD")
	blob := git(t, dir, "rev-parse", "HEAD:tracked.txt")
	merge := git(t, dir, "commit-tree", git(t, dir, "rev-parse", "HEAD^{tree}"), "-p", head, "-p", snap.Commit, "-m", "agent-snap: merge")
	idFor := func(commit string) string { return "20260101T000000.000000000Z-" + commit[:12] }
	mismatched := "20260101T000000.000000000Z-000000000000"
	nested := "20260101T000000.000000000Z-111111111111"

	git(t, dir, "symbolic-ref", store.itemsRef()+idFor(head), "refs/heads/main")
	git(t, dir, "update-ref", store.itemsRef()+idFor(blob), blob)
	git(t, dir, "update-ref", store.itemsRef()+idFor(merge), merge)
	git(t, dir, "update-ref", store.itemsRef()+mismatched, snap.Commit)
	git(t, dir, "update-ref", store.itemsRef()+nested+"/"+snap.Commit[:12], snap.Commit)
	writeFile(t, dir, "tracked.txt", "edited\n")
	child := create(t, store, "child")

	before := repoState(t, dir)
	if got := get(t, store, snap.ID); got.Parent != head || got.ParentID != "" {
		t.Errorf("a symbolic alias of HEAD must not be reported as the parent snapshot: %+v", got)
	}
	if got := get(t, store, child.ID); got.ParentID != snap.ID {
		t.Errorf("parent_id must skip aliases Get refuses: got %q want %q", got.ParentID, snap.ID)
	}
	for _, tc := range []struct{ name, id, want string }{
		{"symbolic ref onto a branch", idFor(head), "symbolic ref"},
		{"ref to a blob", idFor(blob), "not a commit"},
		{"ref to a merge commit", idFor(merge), "more than one parent"},
		{"id suffix that does not match the commit", mismatched, "does not match its id"},
	} {
		_, err := store.Get(context.Background(), tc.id)
		if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
	if _, err := store.Get(context.Background(), nested); !errors.Is(err, ErrNotFound) {
		t.Errorf("a ref nested below an id must not resolve that id: %v", err)
	}
	if after := repoState(t, dir); after != before {
		t.Fatalf("lookup changed repository state:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestGetCapsChangedFiles(t *testing.T) {
	for _, n := range []int{changedFilesLimit, changedFilesLimit + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			dir := testRepo(t)
			for i := 0; i < n; i++ {
				writeFile(t, dir, fmt.Sprintf("f%04d.txt", i), "x\n")
			}
			store := newStore(t, dir)
			got := get(t, store, create(t, store, "many").ID)

			if len(got.ChangedFiles) != changedFilesLimit {
				t.Fatalf("got %d changed files, want %d", len(got.ChangedFiles), changedFilesLimit)
			}
			if got.ChangedFilesTruncated != (n > changedFilesLimit) {
				t.Fatalf("%d changed paths: truncated=%v", n, got.ChangedFilesTruncated)
			}
			if last := got.ChangedFiles[changedFilesLimit-1]; last != fmt.Sprintf("f%04d.txt", changedFilesLimit-1) {
				t.Fatalf("truncation must keep the first paths in order, last kept: %q", last)
			}
		})
	}
}

func TestGetInSha256Repositories(t *testing.T) {
	dir := t.TempDir()
	init := exec.Command("git", "init", "--object-format=sha256", "-b", "main")
	init.Dir = dir
	init.Env = sanitizedEnv()
	if out, err := init.CombinedOutput(); err != nil {
		t.Skipf("git without sha256 support: %v: %s", err, out)
	}
	writeFile(t, dir, "file.txt", "initial\n")
	git(t, dir, "add", "file.txt")
	git(t, dir, "commit", "-m", "initial")
	writeFile(t, dir, "file.txt", "changed\n")
	store := newStore(t, dir)
	snap := create(t, store, "sha256")

	got := get(t, store, snap.ID)
	if len(got.Commit) != 64 || len(got.Tree) != 64 || got.Parent != git(t, dir, "rev-parse", "HEAD") {
		t.Fatalf("sha256 detail must carry full object ids: %+v", got)
	}
	if !reflect.DeepEqual(got.ChangedFiles, []string{"file.txt"}) {
		t.Fatalf("changed files: %+v", got.ChangedFiles)
	}
}
