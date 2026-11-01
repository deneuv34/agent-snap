package snapshot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func blobOf(t *testing.T, dir, content string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "--no-filters", "--stdin")
	cmd.Dir = dir
	cmd.Env = sanitizedEnv()
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// treeOf writes a flat tree from "<mode> <type> <oid>\t<name>" entries. Names
// may hold any bytes but NUL.
func treeOf(t *testing.T, dir string, entries ...string) string {
	t.Helper()
	cmd := exec.Command("git", "mktree", "-z", "--missing")
	cmd.Dir = dir
	cmd.Env = sanitizedEnv()
	cmd.Stdin = strings.NewReader(strings.Join(append(entries, ""), "\x00"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("mktree: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func fileEntry(t *testing.T, dir, mode, name, content string) string {
	t.Helper()
	return mode + " blob " + blobOf(t, dir, content) + "\t" + name
}

var forgedSnapshots atomic.Int64

// snapshotOfTree writes an item ref for a commit of tree that Create could
// have written, so a test can diff trees no working tree can hold portably.
func snapshotOfTree(t *testing.T, dir string, store *Store, tree string) string {
	t.Helper()
	n := forgedSnapshots.Add(1)
	cmd := exec.Command("git", "commit-tree", tree, "-F", "-")
	cmd.Dir = dir
	cmd.Env = append(sanitizedEnv(), authorEnv()...)
	cmd.Stdin = strings.NewReader(fmt.Sprintf("%sforged %d", messagePrefix, n))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("commit-tree: %v", err)
	}
	commit := strings.TrimSpace(string(out))
	id := fmt.Sprintf("20260101T000000.%09dZ-%s", n, commit[:12])
	git(t, dir, "update-ref", store.itemsRef()+id, commit)
	return id
}

func diffOf(t *testing.T, store *Store, from, to string, page DiffPage) Diff {
	t.Helper()
	d, err := store.Diff(context.Background(), from, to, page)
	if err != nil {
		t.Fatalf("diff %s %s %+v: %v", from, to, page, err)
	}
	return d
}

func diffJSON(t *testing.T, store *Store, from, to string, page DiffPage) string {
	t.Helper()
	out, err := json.Marshal(diffOf(t, store, from, to, page))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func assertFiles(t *testing.T, got, want []DiffFile) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d files, want %d:\n got %+v", len(got), len(want), got)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("file %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestDiffOfTheSameSnapshotOrEqualTreesHasNoFiles(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "one")
	twin := snapshotOfTree(t, dir, store, git(t, dir, "rev-parse", snap.Commit+"^{tree}"))
	twinCommit := git(t, dir, "rev-parse", store.itemsRef()+twin)

	for _, tc := range []struct{ from, to, toCommit string }{
		{snap.ID, snap.ID, snap.Commit},
		{snap.ID, twin, twinCommit},
	} {
		got := diffJSON(t, store, tc.from, tc.to, DiffPage{Offset: 3})
		want := fmt.Sprintf(`{"from":{"id":%q,"commit":%q},"to":{"id":%q,"commit":%q},"offset":3,"totals":{"files":0,"additions":0,"deletions":0},"files":[]}`,
			snap.ID, snap.Commit, tc.to, tc.toCommit)
		if got != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	}
}

func TestDiffReportsEveryKindOfChangeInPathOrder(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	ten := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	sub1, sub2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	fromTree := treeOf(t, dir,
		fileEntry(t, dir, "100644", "bin", "x\x00y\n"),
		fileEntry(t, dir, "100644", "crlf.txt", "a\r\nb\r\n"),
		fileEntry(t, dir, "100644", "del.txt", "gone\n"),
		fileEntry(t, dir, "120000", "link", "a"),
		fileEntry(t, dir, "100644", "mode.sh", "echo\n"),
		fileEntry(t, dir, "100644", "nonl.txt", "end"),
		fileEntry(t, dir, "100644", "old-name.txt", ten),
		"160000 commit "+sub1+"\tsub",
		fileEntry(t, dir, "100644", "tracked.txt", "initial\n"),
		fileEntry(t, dir, "100644", "typ", "file\n"),
	)
	toTree := treeOf(t, dir,
		fileEntry(t, dir, "100644", "added.txt", "one\ntwo\n"),
		fileEntry(t, dir, "100644", "bin", "x\x00z\n"),
		fileEntry(t, dir, "100644", "crlf.txt", "a\r\nB\r\n"),
		fileEntry(t, dir, "100644", "empty.txt", ""),
		fileEntry(t, dir, "120000", "link", "b"),
		fileEntry(t, dir, "100755", "mode.sh", "echo\n"),
		fileEntry(t, dir, "100644", "new-name.txt", strings.Replace(ten, "10\n", "ten\n", 1)),
		fileEntry(t, dir, "100644", "nonl.txt", "end\n"),
		"160000 commit "+sub2+"\tsub",
		fileEntry(t, dir, "100644", "tracked.txt", "changed\n"),
		fileEntry(t, dir, "120000", "typ", "tracked.txt"),
		fileEntry(t, dir, "100644", "zz.txt", "z\n"),
	)
	from, to := snapshotOfTree(t, dir, store, fromTree), snapshotOfTree(t, dir, store, toTree)
	b := func(content string) string { return blobOf(t, dir, content) }

	got := diffOf(t, store, from, to, DiffPage{})
	if got.From.ID != from || got.To.ID != to || got.From.Commit != git(t, dir, "rev-parse", store.itemsRef()+from) || got.Offset != 0 || got.NextOffset != 0 {
		t.Fatalf("sides and paging: %+v %+v offset=%d next=%d", got.From, got.To, got.Offset, got.NextOffset)
	}
	if got.Totals != (DiffTotals{Files: 13, Additions: 10, Deletions: 8}) {
		t.Fatalf("totals: %+v", got.Totals)
	}
	for k := range got.Files {
		page := diffOf(t, store, from, to, DiffPage{Offset: k})
		if len(page.Files) != len(got.Files)-k || !reflect.DeepEqual(page.Files[0], got.Files[k]) {
			t.Fatalf("the page at offset %d must start with file %d:\n got %+v\nwant %+v", k, k, page.Files, got.Files[k])
		}
	}
	if len(got.Files) == 13 {
		if s := got.Files[7].Similarity; s < 50 || s >= 100 {
			t.Fatalf("an edited rename has a similarity from 50 through 99: %d", s)
		}
		got.Files[7].Similarity = 0
	}
	noNewline := "\\ No newline at end of file\n"
	assertFiles(t, got.Files, []DiffFile{
		{Status: StatusAdded, Path: "added.txt", NewMode: "100644", NewOID: b("one\ntwo\n"), Additions: 2, Patch: PatchFull,
			Hunks: []Hunk{{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 2, Text: "+one\n+two\n"}}},
		{Status: StatusModified, Path: "bin", OldMode: "100644", NewMode: "100644", OldOID: b("x\x00y\n"), NewOID: b("x\x00z\n"), Patch: PatchBinary, Hunks: []Hunk{}},
		{Status: StatusModified, Path: "crlf.txt", OldMode: "100644", NewMode: "100644", OldOID: b("a\r\nb\r\n"), NewOID: b("a\r\nB\r\n"), Additions: 1, Deletions: 1, Patch: PatchFull,
			Hunks: []Hunk{{OldStart: 1, OldLines: 2, NewStart: 1, NewLines: 2, Text: " a\r\n-b\r\n+B\r\n"}}},
		{Status: StatusDeleted, Path: "del.txt", OldMode: "100644", OldOID: b("gone\n"), Deletions: 1, Patch: PatchFull,
			Hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 0, NewLines: 0, Text: "-gone\n"}}},
		{Status: StatusAdded, Path: "empty.txt", NewMode: "100644", NewOID: b(""), Patch: PatchFull, Hunks: []Hunk{}},
		{Status: StatusModified, Path: "link", OldMode: "120000", NewMode: "120000", OldOID: b("a"), NewOID: b("b"), Additions: 1, Deletions: 1, Patch: PatchFull,
			Hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1, Text: "-a\n" + noNewline + "+b\n" + noNewline}}},
		{Status: StatusModified, Path: "mode.sh", OldMode: "100644", NewMode: "100755", OldOID: b("echo\n"), NewOID: b("echo\n"), Patch: PatchFull, Hunks: []Hunk{}},
		{Status: StatusRenamed, Path: "new-name.txt", OldPath: "old-name.txt", OldMode: "100644", NewMode: "100644", OldOID: b(ten), NewOID: b(strings.Replace(ten, "10\n", "ten\n", 1)),
			Additions: 1, Deletions: 1, Patch: PatchFull,
			Hunks: []Hunk{{OldStart: 7, OldLines: 4, NewStart: 7, NewLines: 4, Text: " 7\n 8\n 9\n-10\n+ten\n"}}},
		{Status: StatusModified, Path: "nonl.txt", OldMode: "100644", NewMode: "100644", OldOID: b("end"), NewOID: b("end\n"), Additions: 1, Deletions: 1, Patch: PatchFull,
			Hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1, Text: "-end\n" + noNewline + "+end\n"}}},
		{Status: StatusModified, Path: "sub", OldMode: "160000", NewMode: "160000", OldOID: sub1, NewOID: sub2, Additions: 1, Deletions: 1, Patch: PatchFull,
			Hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1, Text: "-Subproject commit " + sub1 + "\n+Subproject commit " + sub2 + "\n"}}},
		{Status: StatusModified, Path: "tracked.txt", OldMode: "100644", NewMode: "100644", OldOID: b("initial\n"), NewOID: b("changed\n"), Additions: 1, Deletions: 1, Patch: PatchFull,
			Hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1, Text: "-initial\n+changed\n"}}},
		{Status: StatusTypeChanged, Path: "typ", OldMode: "100644", NewMode: "120000", OldOID: b("file\n"), NewOID: b("tracked.txt"), Additions: 1, Deletions: 1, Patch: PatchFull,
			Hunks: []Hunk{
				{OldStart: 1, OldLines: 1, NewStart: 0, NewLines: 0, Text: "-file\n"},
				{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 1, Text: "+tracked.txt\n" + noNewline},
			}},
		{Status: StatusAdded, Path: "zz.txt", NewMode: "100644", NewOID: b("z\n"), Additions: 1, Patch: PatchFull,
			Hunks: []Hunk{{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 1, Text: "+z\n"}}},
	})

	back := diffOf(t, store, to, from, DiffPage{})
	if back.Totals != (DiffTotals{Files: 13, Additions: 8, Deletions: 10}) || back.Files[0].Status != StatusDeleted || back.Files[0].Path != "added.txt" {
		t.Fatalf("either snapshot may be the older one: %+v %+v", back.Totals, back.Files[0])
	}
}

func TestDiffAddsAGitlinkByCommitID(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	head := git(t, dir, "rev-parse", "HEAD")
	from := snapshotOfTree(t, dir, store, treeOf(t, dir))
	to := snapshotOfTree(t, dir, store, treeOf(t, dir, "160000 commit "+head+"\tsub"))

	assertFiles(t, diffOf(t, store, from, to, DiffPage{}).Files, []DiffFile{{
		Status: StatusAdded, Path: "sub", NewMode: "160000", NewOID: head, Additions: 1, Patch: PatchFull,
		Hunks: []Hunk{{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 1, Text: "+Subproject commit " + head + "\n"}},
	}})
}

func TestDiffRepairsNonUTF8TextAndChargesTheOriginalBytes(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	invalid := strings.Repeat("\xffa", 200)
	aCost := len("@@ -0,0 +1 @@\n") + len("+"+invalid+"\n")
	bLine := strings.Repeat("b", 2048-aCost-len("@@ -0,0 +1 @@\n")-2)
	from := snapshotOfTree(t, dir, store, treeOf(t, dir))
	to := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, "100644", "a-caf\xe9.txt", invalid+"\n"),
		fileEntry(t, dir, "100644", "b.txt", bLine+"\n"),
	))

	fit := diffOf(t, store, from, to, DiffPage{MaxPatchBytes: 2048})
	if len(fit.Files) != 2 || fit.Files[0].Patch != PatchFull || fit.Files[1].Patch != PatchFull || fit.NextOffset != 0 {
		t.Fatalf("hunks costing exactly the budget fit it: %+v", fit)
	}
	a := fit.Files[0]
	if a.Path != "a-caf\uFFFD.txt" || !a.Lossy || a.Hunks[0].Text != "+"+strings.Repeat("\uFFFDa", 200)+"\n" || a.Additions != 1 {
		t.Fatalf("non-UTF-8 path and text: %+v", a)
	}
	if fit.Files[1].Lossy {
		t.Fatalf("a valid file is not lossy: %+v", fit.Files[1])
	}

	over := diffOf(t, store, from, to, DiffPage{MaxPatchBytes: 2047})
	if over.Files[0].Patch != PatchFull || over.Files[1].Patch != PatchDeferred || len(over.Files[1].Hunks) != 0 || over.NextOffset != 1 {
		t.Fatalf("one byte short defers the second file: %+v", over)
	}
}

func TestDiffTruncatesTheFirstFileThatDoesNotFit(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	var oldLines, newLines []string
	for i := 1; i <= 30; i++ {
		oldLines = append(oldLines, fmt.Sprint(i))
	}
	newLines = append(newLines, oldLines...)
	newLines[0] = "one"
	long := strings.Repeat("x", 2000)
	newLines[29] = long
	join := func(lines []string) string { return strings.Join(lines, "\n") + "\n" }
	from := snapshotOfTree(t, dir, store, treeOf(t, dir, fileEntry(t, dir, "100644", "two-hunks.txt", join(oldLines))))
	to := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, "100644", "one-hunk.txt", strings.Repeat("y", 3000)+"\n"),
		fileEntry(t, dir, "100644", "two-hunks.txt", join(newLines)),
		fileEntry(t, dir, "100644", "z.txt", "z\n"),
	))
	firstHunk := Hunk{OldStart: 1, OldLines: 4, NewStart: 1, NewLines: 4, Text: "-1\n+one\n 2\n 3\n 4\n"}
	twoHunksCost := len("@@ -1,4 +1,4 @@\n") + len(firstHunk.Text) +
		len("@@ -27,4 +27,4 @@\n") + len(" 27\n 28\n 29\n-30\n+"+long+"\n")
	oneHunkCost := len("@@ -0,0 +1 @@\n") + 3002

	page := diffOf(t, store, from, to, DiffPage{MaxPatchBytes: 1024})
	one := page.Files[0]
	if one.Patch != PatchTruncated || len(one.Hunks) != 0 || one.PatchBytes != oneHunkCost || one.Additions != 1 {
		t.Fatalf("a first file larger than the budget is truncated with no hunks: %+v", one)
	}
	if page.Files[1].Patch != PatchDeferred || page.Files[2].Patch != PatchDeferred || page.NextOffset != 1 {
		t.Fatalf("text files after the truncated file are deferred: %+v", page)
	}
	if again := diffOf(t, store, from, to, DiffPage{MaxPatchBytes: one.PatchBytes}); again.Files[0].Patch != PatchFull || len(again.Files[0].Hunks) != 1 {
		t.Fatalf("patch_bytes as the budget returns the file full: %+v", again.Files[0])
	}

	next := diffOf(t, store, from, to, DiffPage{Offset: page.NextOffset, MaxPatchBytes: 1024})
	two := next.Files[0]
	if two.Patch != PatchTruncated || !reflect.DeepEqual(two.Hunks, []Hunk{firstHunk}) || two.PatchBytes != twoHunksCost {
		t.Fatalf("a truncated file keeps the longest hunk prefix that fits: %+v", two)
	}
	if next.NextOffset != 2 || next.Files[1].Patch != PatchDeferred {
		t.Fatalf("next_offset names the first deferred file: %+v", next)
	}
	last := diffOf(t, store, from, to, DiffPage{Offset: next.NextOffset, MaxPatchBytes: 1024})
	if len(last.Files) != 1 || last.Files[0].Patch != PatchFull || last.NextOffset != 0 {
		t.Fatalf("the last page finishes the diff: %+v", last)
	}
}

func TestDiffOmitsPatchBytesBeyondTheLargestBudget(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	line := strings.Repeat("w", 59) + "\n"
	from := snapshotOfTree(t, dir, store, treeOf(t, dir))
	to := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, "100644", "huge.txt", strings.Repeat(line, 20000)),
		fileEntry(t, dir, "100644", "tail.txt", "t\n"),
	))

	page := diffOf(t, store, from, to, DiffPage{MaxPatchBytes: DiffMaxPatchBytes})
	huge := page.Files[0]
	if huge.Patch != PatchTruncated || huge.PatchBytes != 0 || len(huge.Hunks) != 0 || huge.Additions != 20000 {
		t.Fatalf("a patch over 1 MiB is truncated without patch_bytes: %+v", huge)
	}
	if page.Files[1].Patch != PatchDeferred || page.NextOffset != 1 || page.Totals.Additions != 20001 {
		t.Fatalf("the rest of the page: %+v", page)
	}
}

func TestDiffBinaryFilesNeverEndThePatches(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	from := snapshotOfTree(t, dir, store, treeOf(t, dir))
	to := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, "100644", "a.txt", "a\n"),
		fileEntry(t, dir, "100644", "b.bin", strings.Repeat("\x00", 70000)),
		fileEntry(t, dir, "100644", "c.txt", strings.Repeat("c", 70000)+"\n"),
		fileEntry(t, dir, "100644", "d.bin", "\x00"),
		fileEntry(t, dir, "100644", "e.txt", "e\n"),
	))

	first := diffOf(t, store, from, to, DiffPage{})
	var states []PatchState
	for _, f := range first.Files {
		states = append(states, f.Patch)
	}
	if want := []PatchState{PatchFull, PatchBinary, PatchDeferred, PatchBinary, PatchDeferred}; !reflect.DeepEqual(states, want) || first.NextOffset != 2 {
		t.Fatalf("states %v next %d, want %v next 2", states, first.NextOffset, want)
	}
	if b := first.Files[1]; b.Additions != 0 || b.Deletions != 0 || len(b.Hunks) != 0 {
		t.Fatalf("a binary file has no hunks and no line counts: %+v", b)
	}
	second := diffOf(t, store, from, to, DiffPage{Offset: 2})
	if second.Files[0].Patch != PatchTruncated || second.Files[2].Patch != PatchDeferred || second.NextOffset != 4 {
		t.Fatalf("second page: %+v", second)
	}
	third := diffOf(t, store, from, to, DiffPage{Offset: 4})
	if len(third.Files) != 1 || third.Files[0].Patch != PatchFull || third.NextOffset != 0 {
		t.Fatalf("third page: %+v", third)
	}
}

func TestDiffPagesAtMost200Files(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	var entries []string
	for i := 0; i < 201; i++ {
		entries = append(entries, fileEntry(t, dir, "100644", fmt.Sprintf("f%03d", i), fmt.Sprintf("%d\n", i)))
	}
	from := snapshotOfTree(t, dir, store, treeOf(t, dir))
	to := snapshotOfTree(t, dir, store, treeOf(t, dir, entries...))

	first := diffOf(t, store, from, to, DiffPage{})
	if len(first.Files) != 200 || first.NextOffset != 200 || first.Totals.Files != 201 || first.Files[199].Path != "f199" {
		t.Fatalf("first page: %d files, next %d, totals %+v", len(first.Files), first.NextOffset, first.Totals)
	}
	second := diffOf(t, store, from, to, DiffPage{Offset: 200})
	if len(second.Files) != 1 || second.Files[0].Path != "f200" || second.Files[0].Patch != PatchFull || second.NextOffset != 0 || second.Offset != 200 {
		t.Fatalf("second page: %+v", second)
	}
	for _, offset := range []int{201, MaxPageOffset} {
		past := diffOf(t, store, from, to, DiffPage{Offset: offset})
		if len(past.Files) != 0 || past.Files == nil || past.NextOffset != 0 || past.Totals.Files != 201 || past.Offset != offset {
			t.Fatalf("offset %d past the end: %+v", offset, past)
		}
	}
}

func TestDiffStopsAGitThatRunsUnderALauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git for Windows already runs git.exe under its cmd launcher")
	}
	dir := testRepo(t)
	store := newStore(t, dir)
	var entries []string
	for i := 0; i < 50; i++ {
		entries = append(entries, fileEntry(t, dir, "100644", fmt.Sprintf("f%03d", i), strings.Repeat(fmt.Sprintf("line %d\n", i), 2000)))
	}
	from := snapshotOfTree(t, dir, store, treeOf(t, dir))
	to := snapshotOfTree(t, dir, store, treeOf(t, dir, entries...))
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	launcher := t.TempDir()
	if err := os.WriteFile(filepath.Join(launcher, "git"), []byte("#!/bin/sh\n\""+real+"\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", launcher+string(os.PathListSeparator)+os.Getenv("PATH"))

	done := make(chan error, 1)
	go func() {
		_, err := store.Diff(context.Background(), from, to, DiffPage{Offset: 50})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a page that ends before Git does must not wait on a Git that the kill does not reach")
	}
}

func TestDiffRejectsPagesOutsideTheirRange(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "one")
	for _, page := range []DiffPage{{Offset: -1}, {Offset: MaxPageOffset + 1}, {MaxPatchBytes: DiffMinPatchBytes - 1}, {MaxPatchBytes: DiffMaxPatchBytes + 1}, {MaxPatchBytes: -1}} {
		if _, err := store.Diff(context.Background(), snap.ID, snap.ID, page); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("page %+v: got %v", page, err)
		}
	}
}

func TestDiffResolvesFromBeforeTo(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	snap := create(t, store, "one")
	const missingFrom = "20260101T000000.000000000Z-0123456789ab"
	const missingTo = "20260101T000000.000000000Z-ba9876543210"
	for _, tc := range []struct{ from, to, want string }{
		{missingFrom, missingTo, missingFrom},
		{missingFrom, snap.ID, missingFrom},
		{snap.ID, missingTo, missingTo},
	} {
		_, err := store.Diff(context.Background(), tc.from, tc.to, DiffPage{})
		if !errors.Is(err, ErrNotFound) || err.Error() != "snapshot not found: "+tc.want {
			t.Errorf("diff %s %s: got %v, want not found naming %s", tc.from, tc.to, err, tc.want)
		}
	}
	if _, err := store.Diff(context.Background(), "HEAD", snap.ID, DiffPage{}); !errors.Is(err, ErrInvalidID) {
		t.Errorf("a revision expression is not a snapshot id: %v", err)
	}
	if _, err := store.Diff(context.Background(), snap.ID, snap.Commit, DiffPage{}); !errors.Is(err, ErrInvalidID) {
		t.Errorf("a commit id is not a snapshot id: %v", err)
	}
}

func TestDiffDoesNotResolveAnotherWorktreesSnapshot(t *testing.T) {
	dir := testRepo(t)
	linked := t.TempDir()
	git(t, dir, "worktree", "add", "-b", "feature", linked)
	storeMain, storeLinked := newStore(t, dir), newStore(t, linked)
	base := create(t, storeLinked, "base")
	writeFile(t, linked, "tracked.txt", "linked\n")
	edited := create(t, storeLinked, "edited")

	if _, err := storeMain.Diff(context.Background(), base.ID, edited.ID, DiffPage{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("main worktree must not resolve a linked worktree snapshot: %v", err)
	}
	got := diffOf(t, storeLinked, base.ID, edited.ID, DiffPage{})
	if len(got.Files) != 1 || got.Files[0].Hunks[0].Text != "-initial\n+linked\n" {
		t.Fatalf("a linked worktree diffs its own snapshots: %+v", got)
	}
}

func TestDiffInSha256Repositories(t *testing.T) {
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
	store := newStore(t, dir)
	from := create(t, store, "one")
	writeFile(t, dir, "file.txt", "changed\n")
	to := create(t, store, "two")

	assertFiles(t, diffOf(t, store, from.ID, to.ID, DiffPage{}).Files, []DiffFile{{
		Status: StatusModified, Path: "file.txt", OldMode: "100644", NewMode: "100644",
		OldOID: git(t, dir, "rev-parse", from.Commit+":file.txt"), NewOID: git(t, dir, "rev-parse", to.Commit+":file.txt"),
		Additions: 1, Deletions: 1, Patch: PatchFull,
		Hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1, Text: "-initial\n+changed\n"}},
	}})
}

func TestDiffIgnoresAttributesConfigurationAndReplaceRefs(t *testing.T) {
	dir := testRepo(t)
	writeFile(t, dir, "script.txt", "heading line\n1\n2\n3\n4\n5\n6\n")
	store := newStore(t, dir)
	from := create(t, store, "from")
	writeFile(t, dir, "script.txt", "heading line\n1\n2\n3\n4\n5\nsix\n")
	writeFile(t, dir, "tracked.txt", "changed\n")
	to := create(t, store, "to")
	gitDir := git(t, dir, "rev-parse", "--absolute-git-dir")
	clean := diffJSON(t, store, from.ID, to.ID, DiffPage{})
	if want := `"section":"heading line","text":" 3\n 4\n 5\n-6\n+six\n"`; !strings.Contains(clean, want) {
		t.Fatalf("clean diff lacks %s:\n%s", want, clean)
	}

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"worktree -diff attribute", func(t *testing.T) { writeFile(t, dir, ".gitattributes", "* -diff\n") }},
		{"worktree funcname driver", func(t *testing.T) { writeFile(t, dir, ".gitattributes", "* diff=bash\n") }},
		{"info attributes", func(t *testing.T) { writeFile(t, gitDir, "info/attributes", "* binary\n") }},
		{"global configuration and attributes", func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, home, ".gitconfig", "[core]\n\tabbrev = 4\n\tattributesFile = "+filepath.ToSlash(filepath.Join(home, "attrs"))+"\n[diff]\n\tcontext = 10\n")
			writeFile(t, home, "attrs", "* -diff\n")
			writeFile(t, home, "xdg/git/attributes", "* binary\n")
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
		}},
		{"inherited git environment", func(t *testing.T) {
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "core.abbrev")
			t.Setenv("GIT_CONFIG_VALUE_0", "4")
			t.Setenv("GIT_DIR", t.TempDir())
		}},
		{"repository diff configuration", func(t *testing.T) {
			for _, kv := range [][2]string{{"diff.algorithm", "patience"}, {"diff.context", "10"}, {"diff.renames", "false"}, {"core.abbrev", "4"}, {"diff.noprefix", "true"}} {
				git(t, dir, "config", kv[0], kv[1])
			}
		}},
		{"replace ref", func(t *testing.T) {
			git(t, dir, "replace", git(t, dir, "rev-parse", to.Commit+":script.txt"), blobOf(t, dir, "replaced\n"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() {
				_ = os.Remove(filepath.Join(dir, ".gitattributes"))
				_ = os.Remove(filepath.Join(gitDir, "info", "attributes"))
			})
			tc.setup(t)
			if got := diffJSON(t, store, from.ID, to.ID, DiffPage{}); got != clean {
				t.Fatalf("diff changed:\n got %s\nwant %s", got, clean)
			}
		})
	}

	withAttrs := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, "100644", ".gitattributes", "* -diff\n"),
		"100644 blob "+git(t, dir, "rev-parse", to.Commit+":script.txt")+"\tscript.txt",
	))
	fromScript := snapshotOfTree(t, dir, store, treeOf(t, dir,
		fileEntry(t, dir, "100644", ".gitattributes", "* -diff\n"),
		"100644 blob "+git(t, dir, "rev-parse", from.Commit+":script.txt")+"\tscript.txt",
	))
	inTree := diffOf(t, store, fromScript, withAttrs, DiffPage{})
	if len(inTree.Files) != 1 || inTree.Files[0].Patch != PatchFull || inTree.Files[0].Hunks[0].Section != "heading line" {
		t.Fatalf("an attribute file inside the snapshot trees must not apply: %+v", inTree.Files)
	}
}

func TestDiffFailsOnAMissingObjectWithoutFetchingIt(t *testing.T) {
	dir := testRepo(t)
	store := newStore(t, dir)
	from := create(t, store, "from")
	writeFile(t, dir, "tracked.txt", "only in the snapshot\n")
	to := create(t, store, "to")
	blob := git(t, dir, "rev-parse", to.Commit+":tracked.txt")
	remote := filepath.Join(t.TempDir(), "remote.git")
	git(t, dir, "clone", "--quiet", "--mirror", dir, remote)
	git(t, dir, "config", "core.repositoryformatversion", "1")
	git(t, dir, "config", "extensions.partialClone", "origin")
	git(t, dir, "remote", "add", "origin", remote)
	git(t, dir, "config", "remote.origin.promisor", "true")
	gitDir := git(t, dir, "rev-parse", "--absolute-git-dir")
	if err := os.Remove(filepath.Join(gitDir, "objects", blob[:2], blob[2:])); err != nil {
		t.Skipf("blob is not a loose object: %v", err)
	}

	for _, page := range []DiffPage{{}, {Offset: 5}} {
		_, err := store.Diff(context.Background(), from.ID, to.ID, page)
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("a missing object must fail the diff, even past the page: %+v %v", page, err)
		}
	}
	check := exec.Command("git", "cat-file", "-e", blob)
	check.Dir = dir
	check.Env = append(sanitizedEnv(), "GIT_NO_LAZY_FETCH=1")
	if check.Run() == nil {
		t.Fatalf("the diff fetched the missing object %s", blob)
	}
}

func TestDiffWritesNothingOutsideItsScratchDirectory(t *testing.T) {
	dir := testRepo(t)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	t.Setenv("TMP", scratch)
	t.Setenv("TEMP", scratch)
	store := newStore(t, dir)
	from := create(t, store, "from")
	writeFile(t, dir, "tracked.txt", "changed\n")
	writeFile(t, dir, "untracked.txt", "new\n")
	to := create(t, store, "to")
	gitDir := git(t, dir, "rev-parse", "--absolute-git-dir")
	readConfig := func() string {
		out, err := os.ReadFile(filepath.Join(gitDir, "config"))
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	before, config := repoState(t, dir), readConfig()
	for _, page := range []DiffPage{{}, {Offset: 1}, {Offset: 5}} {
		diffOf(t, store, from.ID, to.ID, page)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Diff(ctx, from.ID, to.ID, DiffPage{}); err == nil {
		t.Fatal("a cancelled diff must fail")
	}
	if after := repoState(t, dir); after != before {
		t.Fatalf("diff changed repository state:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if readConfig() != config {
		t.Fatal("diff changed the repository configuration")
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "agent-snap") {
			t.Fatalf("diff left scratch directory %s", entry.Name())
		}
	}
}

func readDiffString(stream string, offset, budget int) (diffPage, bool, error) {
	return readDiff(bufio.NewReader(strings.NewReader(stream)), offset, budget)
}

func TestReadDiffFailsClosedOnMalformedStreams(t *testing.T) {
	oid := func(c string) string { return strings.Repeat(c, 40) }
	raw := ":100644 100644 " + oid("a") + " " + oid("b") + " M\x00f.txt\x00"
	numstat := "1\t1\tf.txt\x00"
	section := "diff --git a/f.txt b/f.txt\nindex " + oid("a") + ".." + oid("b") + " 100644\n--- a/f.txt\n+++ b/f.txt\n"
	hunk := "@@ -1,2 +1,2 @@\n x\n-y\n+z\n"
	valid := raw + numstat + "\x00" + section + hunk
	if page, complete, err := readDiffString(valid, 0, DiffDefaultPatchBytes); err != nil || !complete || page.files[0].Patch != PatchFull {
		t.Fatalf("the valid fixture must parse: %+v %v %v", page, complete, err)
	}

	typeRaw := ":100644 120000 " + oid("a") + " " + oid("b") + " T\x00f.txt\x00"
	for _, tc := range []struct{ name, stream string }{
		{"hunk body shorter than its header", raw + numstat + "\x00" + section + "@@ -1,3 +1,2 @@\n x\n-y\n+z\n"},
		{"hunk body longer than its header", raw + numstat + "\x00" + section + "@@ -1,1 +1,1 @@\n x\n-y\n+z\n"},
		{"hunk body line with an unknown prefix", raw + numstat + "\x00" + section + "@@ -1,2 +1,2 @@\n x\n*y\n+z\n"},
		{"hunk body without a final newline", raw + numstat + "\x00" + section + "@@ -1,2 +1,2 @@\n x\n-y\n+z"},
		{"malformed hunk header", raw + numstat + "\x00" + section + "@@ -1,2 +1,x @@\n x\n-y\n+z\n"},
		{"missing NUL separator", raw + numstat},
		{"missing patch", raw + numstat + "\x00"},
		{"extra section", valid + section + hunk},
		{"type change with one section", typeRaw + numstat + "\x00" + section + hunk},
		{"numstat count below raw count", raw + raw + numstat + "\x00" + section + hunk + section + hunk},
		{"numstat path differs from raw path", raw + "1\t1\tg.txt\x00" + "\x00" + section + hunk},
		{"hunk lines differ from numstat", raw + "2\t1\tf.txt\x00" + "\x00" + section + hunk},
		{"unsupported status", ":100644 100644 " + oid("a") + " " + oid("b") + " C075\x00f.txt\x00g.txt\x00" + "1\t1\t\x00f.txt\x00g.txt\x00\x00" + section + hunk},
		{"added entry with an old object", ":100644 100644 " + oid("a") + " " + oid("b") + " A\x00f.txt\x00" + numstat + "\x00" + section + hunk},
		{"unexpected section header line", raw + numstat + "\x00" + section + "+stray\n" + hunk},
		{"section that is not a diff --git line", raw + numstat + "\x00" + "garbage\n" + hunk},
		{"separator without records", "\x00"},
	} {
		if _, _, err := readDiffString(tc.stream, 0, DiffDefaultPatchBytes); err == nil {
			t.Errorf("%s: parsed without error", tc.name)
		}
	}
}
