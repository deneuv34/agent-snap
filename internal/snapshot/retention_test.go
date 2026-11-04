package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type repoConfig struct{ objectFormat, refFormat string }

func (c repoConfig) name() string { return c.objectFormat + "/" + c.refFormat }

var retentionConfigs = []repoConfig{{"sha1", "files"}, {"sha256", "files"}, {"sha1", "reftable"}, {"sha256", "reftable"}}

func initRepoIn(t *testing.T, dir string, c repoConfig) {
	t.Helper()
	args := []string{"init", "-b", "main"}
	if c.objectFormat != "sha1" {
		args = append(args, "--object-format="+c.objectFormat)
	}
	if c.refFormat != "files" {
		args = append(args, "--ref-format="+c.refFormat)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = sanitizedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git without %s: %v: %s", c.name(), err, out)
	}
	writeFile(t, dir, "tracked.txt", "initial\n")
	git(t, dir, "add", "tracked.txt")
	git(t, dir, "commit", "-m", "initial")
}

func eachConfig(t *testing.T, filesOnly bool, run func(t *testing.T, dir string)) {
	t.Helper()
	for _, c := range retentionConfigs {
		if filesOnly && c.refFormat != "files" {
			continue
		}
		t.Run(c.name(), func(t *testing.T) {
			dir := t.TempDir()
			initRepoIn(t, dir, c)
			run(t, dir)
		})
	}
}

func skipWhereMappingIsUnproven(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the worktree mapping is not proven on Windows: Git and Go spell the Git directory differently there")
	}
}

func commonDirOf(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

func commitOf(t *testing.T, dir, message string) string {
	t.Helper()
	return git(t, dir, "commit-tree", git(t, dir, "rev-parse", "HEAD^{tree}"), "-m", message)
}

func plant(t *testing.T, dir, ref, oid string) {
	t.Helper()
	git(t, dir, "update-ref", "--no-deref", ref, oid)
}

func looseObjectPath(t *testing.T, dir, oid string) string {
	t.Helper()
	return filepath.Join(commonDirOf(t, dir), "objects", oid[:2], oid[2:])
}

func removeLooseObject(t *testing.T, dir, oid string) {
	t.Helper()
	if err := os.Remove(looseObjectPath(t, dir, oid)); err != nil {
		t.Skipf("object is not loose: %v", err)
	}
}

func writeRefFile(t *testing.T, dir, ref, content string) {
	t.Helper()
	path := filepath.Join(commonDirOf(t, dir), filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func isReftable(t *testing.T, dir string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(commonDirOf(t, dir), "reftable"))
	return err == nil
}

func itemName(n int, oid string) string {
	return fmt.Sprintf("2026010%dT120000.000000000Z-%s", n, oid[:12])
}

func itemRef(ns string, n int, oid string) string {
	return refsPrefix + ns + "/items/" + itemName(n, oid)
}

func latestOf(ns string) string { return refsPrefix + ns + "/latest" }

func digestOf(lines ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	return hex.EncodeToString(sum[:])
}

func line(ref, value, target string) string {
	if target == "" {
		target = "-"
	}
	return ref + " " + value + " " + target + "\n"
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func reportOf(t *testing.T, store *Store) RetentionReport {
	t.Helper()
	report, err := store.RetentionReport(context.Background(), 0)
	if err != nil {
		t.Fatalf("retention report: %v", err)
	}
	return report
}

func entries(t *testing.T, report RetentionReport) map[string]NamespaceReport {
	t.Helper()
	out := map[string]NamespaceReport{}
	for _, entry := range report.Namespaces {
		out[entry.Namespace] = entry
	}
	if len(out) != len(report.Namespaces) {
		t.Fatalf("a namespace is listed twice: %+v", report.Namespaces)
	}
	return out
}

func entryFor(t *testing.T, report RetentionReport, ns string) NamespaceReport {
	t.Helper()
	entry, ok := entries(t, report)[ns]
	if !ok {
		t.Fatalf("namespace %s is not in the report: %+v", ns, report.Namespaces)
	}
	return entry
}

func stamp(id string) *SnapshotStamp { return &SnapshotStamp{ID: id, CreatedAt: idTime(id)} }

func addWorktree(t *testing.T, dir, name string, extra ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	git(t, dir, append(append([]string{"worktree", "add"}, extra...), "-b", name, path)...)
	return path
}

func TestRetentionReportListsEveryNamespace(t *testing.T) {
	skipWhereMappingIsUnproven(t)
	eachConfig(t, false, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		writeFile(t, dir, "tracked.txt", "one\n")
		first := create(t, store, "one")
		writeFile(t, dir, "tracked.txt", "two\n")
		second := create(t, store, "two")

		linked := addWorktree(t, dir, "linked")
		linkedStore := newStore(t, linked)
		writeFile(t, linked, "linked.txt", "linked\n")
		onLinked := create(t, linkedStore, "linked")

		const planted = "00000000000000a1"
		c1, c2 := commitOf(t, dir, "planted one"), commitOf(t, dir, "planted two")
		plant(t, dir, itemRef(planted, 1, c1), c1)
		plant(t, dir, itemRef(planted, 2, c2), c2)
		plant(t, dir, latestOf(planted), c2)

		report := reportOf(t, store)
		if want := (RetentionTotals{Namespaces: 3, Refs: 3 + 2 + 3}); report.Totals != want || report.Offset != 0 || report.NextOffset != 0 {
			t.Fatalf("totals %+v offset %d next %d, want %+v", report.Totals, report.Offset, report.NextOffset, want)
		}
		if len(report.Namespaces) != 3 {
			t.Fatalf("got %d namespaces: %+v", len(report.Namespaces), report.Namespaces)
		}
		for i := 1; i < len(report.Namespaces); i++ {
			if report.Namespaces[i-1].Namespace >= report.Namespaces[i].Namespace {
				t.Fatalf("namespaces are not in increasing order: %+v", report.Namespaces)
			}
		}

		main := entryFor(t, report, store.ns)
		want := NamespaceReport{
			Namespace: store.ns, Current: true, Worktree: realPath(t, dir), Refs: 3, Snapshots: 2,
			Oldest: &SnapshotStamp{first.ID, first.CreatedAt.Truncate(time.Second)}, Newest: &SnapshotStamp{second.ID, second.CreatedAt.Truncate(time.Second)},
			Latest: LatestCommit, Digest: main.Digest,
		}
		if !reflect.DeepEqual(main, want) || len(main.Digest) != 64 {
			t.Fatalf("main namespace:\n got %+v\nwant %+v", main, want)
		}

		other := entryFor(t, report, linkedStore.ns)
		want = NamespaceReport{
			Namespace: linkedStore.ns, Worktree: realPath(t, linked), Refs: 2, Snapshots: 1,
			Oldest: &SnapshotStamp{onLinked.ID, onLinked.CreatedAt.Truncate(time.Second)}, Newest: &SnapshotStamp{onLinked.ID, onLinked.CreatedAt.Truncate(time.Second)},
			Latest: LatestCommit, Digest: other.Digest,
		}
		if !reflect.DeepEqual(other, want) {
			t.Fatalf("linked namespace:\n got %+v\nwant %+v", other, want)
		}

		orphan := entryFor(t, report, planted)
		want = NamespaceReport{
			Namespace: planted, Refs: 3, Snapshots: 2, Oldest: stamp(itemName(1, c1)), Newest: stamp(itemName(2, c2)),
			Latest: LatestCommit,
			Digest: digestOf(line(itemRef(planted, 1, c1), c1, ""), line(itemRef(planted, 2, c2), c2, ""), line(latestOf(planted), c2, "")),
		}
		if !reflect.DeepEqual(orphan, want) || orphan.Oldest.CreatedAt != time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) {
			t.Fatalf("planted namespace:\n got %+v\nwant %+v", orphan, want)
		}

		fromLinked := reportOf(t, linkedStore)
		if !entryFor(t, fromLinked, linkedStore.ns).Current || entryFor(t, fromLinked, store.ns).Current {
			t.Fatalf("current must follow the worktree the report runs in: %+v", fromLinked.Namespaces)
		}
		if !reflect.DeepEqual(entryFor(t, fromLinked, store.ns).Worktree, realPath(t, dir)) {
			t.Fatalf("the main worktree's path from a linked worktree: %+v", fromLinked.Namespaces)
		}
	})
}

func TestRetentionReportMapsEveryWorktree(t *testing.T) {
	skipWhereMappingIsUnproven(t)
	for _, c := range retentionConfigs {
		t.Run(c.name(), func(t *testing.T) {
			parent := t.TempDir()
			real := filepath.Join(parent, "real")
			if err := os.Mkdir(real, 0o755); err != nil {
				t.Fatal(err)
			}
			initRepoIn(t, real, c)
			link := filepath.Join(parent, "link")
			if err := os.Symlink(real, link); err != nil {
				t.Skipf("no symbolic links: %v", err)
			}
			writeFile(t, real, "sub/file.txt", "sub\n")

			plain := filepath.Join(parent, "plain")
			git(t, real, "worktree", "add", "-b", "plain", plain)
			relative := filepath.Join(parent, "relative")
			relativeSupported := exec.Command("git", "worktree", "add", "--relative-paths", "-b", "relative", relative)
			relativeSupported.Dir, relativeSupported.Env = real, sanitizedEnv()
			hasRelative := relativeSupported.Run() == nil

			want := map[string]string{}
			stores := map[string]*Store{}
			for name, path := range map[string]string{"root": link, "sub": filepath.Join(link, "sub"), "plain": plain} {
				stores[name] = newStore(t, path)
			}
			create(t, stores["root"], "root")
			writeFile(t, plain, "plain.txt", "plain\n")
			create(t, stores["plain"], "plain")
			want[stores["root"].ns] = realPath(t, real)
			want[stores["plain"].ns] = realPath(t, plain)
			if stores["sub"].ns != stores["root"].ns {
				t.Fatalf("a subdirectory must share the root's namespace")
			}
			if hasRelative {
				stores["relative"] = newStore(t, relative)
				writeFile(t, relative, "relative.txt", "relative\n")
				create(t, stores["relative"], "relative")
				want[stores["relative"].ns] = realPath(t, relative)
				gitdir, err := os.ReadFile(filepath.Join(commonDirOf(t, real), "worktrees", "relative", "gitdir"))
				if err != nil || filepath.IsAbs(strings.TrimSpace(string(gitdir))) {
					t.Fatalf("the --relative-paths worktree must hold a relative gitdir file: %q %v", gitdir, err)
				}
			}

			for name, store := range stores {
				report := reportOf(t, store)
				if len(report.Namespaces) != len(want) {
					t.Fatalf("from %s: got %d namespaces, want %d: %+v", name, len(report.Namespaces), len(want), report.Namespaces)
				}
				for ns, path := range want {
					entry := entryFor(t, report, ns)
					if entry.Worktree != path || entry.Current != (ns == store.ns) {
						t.Fatalf("from %s: namespace %s has worktree %q current %v, want %q current %v", name, ns, entry.Worktree, entry.Current, path, ns == store.ns)
					}
				}
			}
			if !hasRelative {
				t.Log("this git has no worktree add --relative-paths, so that case did not run")
			}
		})
	}
}

func TestRetentionReportMarksARemovedWorktree(t *testing.T) {
	skipWhereMappingIsUnproven(t)
	eachConfig(t, false, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		create(t, store, "main")
		linked := addWorktree(t, dir, "doomed")
		linkedStore := newStore(t, linked)
		writeFile(t, linked, "doomed.txt", "doomed\n")
		create(t, linkedStore, "doomed")

		before := entryFor(t, reportOf(t, store), linkedStore.ns)
		if before.Worktree == "" {
			t.Fatalf("a live worktree must map: %+v", before)
		}
		git(t, dir, "worktree", "remove", "--force", linked)

		report := reportOf(t, store)
		gone := entryFor(t, report, linkedStore.ns)
		if gone.Worktree != "" || gone.Current || gone.Snapshots != 1 || gone.Latest != LatestCommit {
			t.Fatalf("a removed worktree's namespace must stay with no worktree: %+v", gone)
		}
		if entryFor(t, report, store.ns).Worktree == "" {
			t.Fatalf("the main worktree must still map: %+v", report.Namespaces)
		}
	})
}

func TestRetentionReportReadsEachLatestState(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		const (
			nsCommit   = "00000000000000c1"
			nsMissing  = "00000000000000c2"
			nsBlob     = "00000000000000c3"
			nsSymbolic = "00000000000000c4"
			nsDangling = "00000000000000c5"
			nsGone     = "00000000000000c6"
			nsBroken   = "00000000000000c7"
			nsUnseen   = "00000000000000c8"
		)
		zero := strings.Repeat("0", len(store.zeroOID))
		c := commitOf(t, dir, "commit")
		head := git(t, dir, "rev-parse", "HEAD")
		blob := blobOf(t, dir, "blob\n")
		gone := commitOf(t, dir, "gone")

		plant(t, dir, itemRef(nsCommit, 1, c), c)
		plant(t, dir, latestOf(nsCommit), c)
		plant(t, dir, itemRef(nsMissing, 1, c), c)
		plant(t, dir, latestOf(nsBlob), blob)
		plant(t, dir, itemRef(nsSymbolic, 1, c), c)
		git(t, dir, "symbolic-ref", latestOf(nsSymbolic), "refs/heads/main")
		plant(t, dir, itemRef(nsDangling, 1, c), c)
		git(t, dir, "symbolic-ref", latestOf(nsDangling), "refs/heads/ghost")
		git(t, dir, "symbolic-ref", latestOf(nsUnseen), "refs/heads/ghost")
		git(t, dir, "symbolic-ref", store.latestRef(), "refs/heads/ghost")
		plant(t, dir, itemRef(nsGone, 1, gone), gone)
		plant(t, dir, latestOf(nsGone), gone)
		removeLooseObject(t, dir, gone)
		if !isReftable(t, dir) {
			plant(t, dir, itemRef(nsBroken, 1, c), c)
			writeRefFile(t, dir, latestOf(nsBroken), "not an object id\n")
		}

		report := reportOf(t, store)
		type want struct {
			latest                  LatestState
			refs, snapshots, broken int
			digest                  string
		}
		cases := map[string]want{
			nsCommit:   {LatestCommit, 2, 1, 0, digestOf(line(itemRef(nsCommit, 1, c), c, ""), line(latestOf(nsCommit), c, ""))},
			nsMissing:  {LatestMissing, 1, 1, 0, digestOf(line(itemRef(nsMissing, 1, c), c, ""))},
			nsBlob:     {LatestNotCommit, 1, 0, 0, digestOf(line(latestOf(nsBlob), blob, ""))},
			nsSymbolic: {LatestSymbolic, 2, 1, 0, digestOf(line(itemRef(nsSymbolic, 1, c), c, ""), line(latestOf(nsSymbolic), head, "refs/heads/main"))},
			nsDangling: {LatestSymbolic, 2, 1, 0, digestOf(line(itemRef(nsDangling, 1, c), c, ""), line(latestOf(nsDangling), zero, "refs/heads/ghost"))},
			nsGone:     {LatestMissingObject, 2, 1, 0, digestOf(line(itemRef(nsGone, 1, gone), gone, ""), line(latestOf(nsGone), gone, ""))},
			store.ns:   {LatestSymbolic, 1, 0, 0, digestOf(line(store.latestRef(), zero, "refs/heads/ghost"))},
		}
		if !isReftable(t, dir) {
			cases[nsBroken] = want{LatestBroken, 2, 1, 1, digestOf(line(itemRef(nsBroken, 1, c), c, ""), line(latestOf(nsBroken), "broken", ""))}
		}
		if len(report.Namespaces) != len(cases) {
			t.Fatalf("got %d namespaces, want %d: %+v", len(report.Namespaces), len(cases), report.Namespaces)
		}
		for ns, w := range cases {
			got := entryFor(t, report, ns)
			if got.Latest != w.latest || got.Refs != w.refs || got.Snapshots != w.snapshots || got.Broken != w.broken || got.Digest != w.digest {
				t.Errorf("namespace %s:\n got latest %s refs %d snapshots %d broken %d digest %s\nwant latest %s refs %d snapshots %d broken %d digest %s",
					ns, got.Latest, got.Refs, got.Snapshots, got.Broken, got.Digest, w.latest, w.refs, w.snapshots, w.broken, w.digest)
			}
		}
		if !entryFor(t, report, store.ns).Current {
			t.Fatalf("a dangling symbolic latest in the current namespace must list it as current")
		}
		if _, listed := entries(t, report)[nsUnseen]; listed {
			t.Fatalf("a dangling symbolic latest alone, in a namespace no worktree maps, is invisible to every listing")
		}
	})
}

func TestRetentionReportSurvivesARefToAMissingObject(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		const lost, kept = "00000000000000b1", "00000000000000b2"
		gone, c := commitOf(t, dir, "gone"), commitOf(t, dir, "kept")
		plant(t, dir, itemRef(lost, 1, gone), gone)
		plant(t, dir, itemRef(kept, 1, c), c)
		plant(t, dir, latestOf(kept), c)
		removeLooseObject(t, dir, gone)

		report := reportOf(t, store)
		if len(report.Namespaces) != 2 || report.Totals.Refs != 3 {
			t.Fatalf("every namespace must report: %+v", report)
		}
		if got := entryFor(t, report, lost); got.Refs != 1 || got.Snapshots != 1 || got.Latest != LatestMissing || got.Digest != digestOf(line(itemRef(lost, 1, gone), gone, "")) {
			t.Fatalf("the namespace with the lost object: %+v", got)
		}
		if got := entryFor(t, report, kept); got.Latest != LatestCommit || got.Refs != 2 {
			t.Fatalf("the other namespace: %+v", got)
		}
	})
}

func TestRetentionReportCountsBrokenRefs(t *testing.T) {
	eachConfig(t, true, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		const (
			nsMixed    = "00000000000000d1"
			nsOnlyItem = "00000000000000d2"
			nsLatest   = "00000000000000d3"
			nsNames    = "00000000000000d4"
		)
		c := commitOf(t, dir, "commit")

		plant(t, dir, itemRef(nsMixed, 1, c), c)
		plant(t, dir, latestOf(nsMixed), c)
		writeRefFile(t, dir, itemRef(nsMixed, 2, c), "not an object id\n")

		writeRefFile(t, dir, itemRef(nsOnlyItem, 3, c), "")

		plant(t, dir, itemRef(nsLatest, 1, c), c)
		writeRefFile(t, dir, latestOf(nsLatest), "not an object id\n")

		plant(t, dir, itemRef(nsNames, 1, c), c)
		plant(t, dir, latestOf(nsNames), c)
		writeRefFile(t, dir, refsPrefix+nsNames+"/items/a..b", c+"\n")
		writeRefFile(t, dir, refsPrefix+nsNames+"/items/x y", c+"\n")
		writeRefFile(t, dir, refsPrefix+"tmp/a..b", c+"\n")

		report := reportOf(t, store)
		if report.Totals != (RetentionTotals{Namespaces: 4, Refs: 3 + 1 + 2 + 4, Other: 1}) {
			t.Fatalf("totals %+v", report.Totals)
		}

		mixed := entryFor(t, report, nsMixed)
		wantMixed := NamespaceReport{
			Namespace: nsMixed, Refs: 3, Snapshots: 1, Broken: 1, Oldest: stamp(itemName(1, c)), Newest: stamp(itemName(1, c)), Latest: LatestCommit,
			Digest: digestOf(line(itemRef(nsMixed, 1, c), c, ""), line(itemRef(nsMixed, 2, c), "broken", ""), line(latestOf(nsMixed), c, "")),
		}
		if !reflect.DeepEqual(mixed, wantMixed) {
			t.Fatalf("a broken item with a valid identifier is no snapshot:\n got %+v\nwant %+v", mixed, wantMixed)
		}

		only := entryFor(t, report, nsOnlyItem)
		wantOnly := NamespaceReport{
			Namespace: nsOnlyItem, Refs: 1, Snapshots: 0, Broken: 1, Latest: LatestMissing,
			Digest: digestOf(line(itemRef(nsOnlyItem, 3, c), "broken", "")),
		}
		if !reflect.DeepEqual(only, wantOnly) || only.Oldest != nil || only.Newest != nil {
			t.Fatalf("a namespace whose only item is broken has no oldest and no newest:\n got %+v\nwant %+v", only, wantOnly)
		}

		latest := entryFor(t, report, nsLatest)
		if latest.Latest != LatestBroken || latest.Refs != 2 || latest.Broken != 1 || latest.Snapshots != 1 ||
			latest.Digest != digestOf(line(itemRef(nsLatest, 1, c), c, ""), line(latestOf(nsLatest), "broken", "")) {
			t.Fatalf("a broken latest: %+v", latest)
		}

		names := entryFor(t, report, nsNames)
		if names.Refs != 4 || names.Broken != 2 || names.Snapshots != 1 || names.Latest != LatestCommit ||
			names.Digest != digestOf(line(itemRef(nsNames, 1, c), c, ""), line(refsPrefix+nsNames+"/items/a..b", "broken", ""), line(refsPrefix+nsNames+"/items/x y", "broken", ""), line(latestOf(nsNames), c, "")) {
			t.Fatalf("refs with rejected names: %+v", names)
		}
	})
}

func lowerStderrBound(t *testing.T, bound int) {
	t.Helper()
	was := retentionStderrBound
	retentionStderrBound = bound
	t.Cleanup(func() { retentionStderrBound = was })
}

func TestRetentionReportFailsOnTooMuchStandardError(t *testing.T) {
	eachConfig(t, true, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		const ns = "00000000000000e1"
		for i := 0; i < 40; i++ {
			writeRefFile(t, dir, fmt.Sprintf("%s%s/items/20260101T120000.%09dZ-%012x", refsPrefix, ns, i, i), "not an object id\n")
		}
		if got := entryFor(t, reportOf(t, store), ns); got.Broken != 40 {
			t.Fatalf("with the default bound the report counts every broken ref: %+v", got)
		}

		lowerStderrBound(t, 1024)
		report, err := store.RetentionReport(context.Background(), 0)
		if err == nil || !strings.Contains(err.Error(), "standard error is longer than 1024 bytes") || !reflect.DeepEqual(report, RetentionReport{}) {
			t.Fatalf("got %+v and %v, want the bound error and no report", report, err)
		}
	})
}

func TestRetentionReportFailsOnAnUnknownLine(t *testing.T) {
	cases := []struct {
		name, extra string
		fails       bool
		other       int
	}{
		{"another warning", "warning: unable to access '/x/attributes': Permission denied\n", true, 0},
		{"a warning for a ref outside the prefix", "warning: ignoring broken ref refs/heads/main\n", true, 0},
		{"a blank line", "\n", true, 0},
		{"a reworded warning", "warning: skipping broken ref refs/agent-snapshots/00000000000000f1/latest\n", true, 0},
		{"a warning for a ref outside every namespace", "warning: ignoring broken ref refs/agent-snapshots/README\n", false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eachConfig(t, false, func(t *testing.T, dir string) {
				store := newStore(t, dir)
				read := retentionRead
				retentionRead = func(cmd *exec.Cmd, args []string) (string, string, error) {
					stdout, stderr, err := read(cmd, args)
					return stdout, stderr + tc.extra, err
				}
				t.Cleanup(func() { retentionRead = read })

				report, err := store.RetentionReport(context.Background(), 0)
				if tc.fails {
					if err == nil || !reflect.DeepEqual(report, RetentionReport{}) {
						t.Fatalf("got %+v and %v, want an error and no report", report, err)
					}
					return
				}
				if err != nil || report.Totals.Other != tc.other {
					t.Fatalf("got %+v and %v, want other %d", report, err, tc.other)
				}
			})
		})
	}
}

func TestRetentionReportCountsOtherRefs(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		const ns = "00000000000000a9"
		c := commitOf(t, dir, "commit")
		plant(t, dir, itemRef(ns, 1, c), c)
		plant(t, dir, latestOf(ns), c)
		for _, ref := range []string{
			refsPrefix + "README",
			refsPrefix + "tmp/x",
			refsPrefix + "0123456789ABCDEF/latest",
			refsPrefix + "0123456789abcde/latest",
			refsPrefix + "0123456789abcdef0/latest",
			refsPrefix + "fedcba9876543210",
		} {
			plant(t, dir, ref, c)
		}

		report := reportOf(t, store)
		if report.Totals != (RetentionTotals{Namespaces: 1, Refs: 2, Other: 6}) || len(report.Namespaces) != 1 {
			t.Fatalf("got %+v", report)
		}
		if got := report.Namespaces[0]; got.Namespace != ns || got.Refs != 2 || got.Digest != digestOf(line(itemRef(ns, 1, c), c, ""), line(latestOf(ns), c, "")) {
			t.Fatalf("no namespace may include a ref that no namespace owns: %+v", got)
		}
	})
}

func TestRetentionReportDigestChangesOnEveryRefChange(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		const ns = "00000000000000a8"
		c1, c2 := commitOf(t, dir, "one"), commitOf(t, dir, "two")
		plant(t, dir, itemRef(ns, 1, c1), c1)
		plant(t, dir, latestOf(ns), c1)
		digest := func() string { return entryFor(t, reportOf(t, store), ns).Digest }

		start := digest()
		extra := itemRef(ns, 2, c2)
		plant(t, dir, extra, c2)
		created := digest()
		git(t, dir, "update-ref", "--no-deref", "-d", extra)
		deleted := digest()
		plant(t, dir, itemRef(ns, 1, c1), c2)
		repointed := digest()
		plant(t, dir, latestOf(ns), c2)
		moved := digest()

		if deleted != start {
			t.Fatalf("a ref that is created and deleted again leaves the digest as it was")
		}
		seen := map[string]string{"start": start, "created": created, "repointed": repointed, "moved": moved}
		for a, da := range seen {
			for b, db := range seen {
				if a < b && da == db {
					t.Fatalf("the digest did not change between %s and %s", a, b)
				}
			}
		}
	})
}

func TestRetentionReportIgnoresMessageSize(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		const ns = "00000000000000a7"
		message := filepath.Join(t.TempDir(), "message.txt")
		body := "agent-snap: SECRET-REASON\n\n" + strings.Repeat("secret body line\n", 4<<20/len("secret body line\n"))
		if err := os.WriteFile(message, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c := git(t, dir, "commit-tree", git(t, dir, "rev-parse", "HEAD^{tree}"), "-F", message)
		plant(t, dir, itemRef(ns, 1, c), c)
		plant(t, dir, latestOf(ns), c)

		report := reportOf(t, store)
		if got := entryFor(t, report, ns); got.Snapshots != 1 || got.Latest != LatestCommit {
			t.Fatalf("the namespace with the large message: %+v", got)
		}
		out, err := json.Marshal(report)
		if err != nil || strings.Contains(string(out), "SECRET") {
			t.Fatalf("the report must hold no reason text: %v %s", err, out)
		}
	})
}

func TestRetentionReportFetchesNothingInAPartialClone(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			store := newStore(t, dir)
			const ns = "00000000000000a6"
			c := commitOf(t, dir, "lost")
			plant(t, dir, itemRef(ns, 1, c), c)
			plant(t, dir, latestOf(ns), c)
			remote := filepath.Join(t.TempDir(), "remote.git")
			git(t, dir, "clone", "--quiet", "--mirror", dir, remote)
			git(t, dir, "config", "core.repositoryformatversion", "1")
			git(t, dir, "config", "extensions.partialClone", "origin")
			git(t, dir, "remote", "add", "origin", remote)
			git(t, dir, "config", "remote.origin.promisor", "true")
			removeLooseObject(t, dir, c)

			if got := entryFor(t, reportOf(t, store), ns); got.Latest != LatestMissingObject {
				t.Fatalf("latest must be missing_object: %+v", got)
			}
			if gitCommand(dir, append(sanitizedEnv(), "GIT_NO_LAZY_FETCH=1"), "cat-file", "-e", c).Run() == nil {
				t.Fatalf("the report fetched the missing object %s", c)
			}
		})
	}
}

func TestRetentionReportPagesNamespaces(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		c := commitOf(t, dir, "commit")
		var in strings.Builder
		for i := 0; i < 101; i++ {
			fmt.Fprintf(&in, "create %s %s\n", latestOf(fmt.Sprintf("%016x", i+1)), c)
		}
		cmd := gitCommand(dir, sanitizedEnv(), "update-ref", "--stdin")
		cmd.Stdin = strings.NewReader(in.String())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("update-ref --stdin: %v: %s", err, out)
		}

		for _, tc := range []struct{ offset, listed, next int }{{0, 100, 100}, {50, 51, 0}, {100, 1, 0}, {101, 0, 0}, {5000, 0, 0}} {
			page, err := store.RetentionReport(context.Background(), tc.offset)
			if err != nil {
				t.Fatal(err)
			}
			if page.Offset != tc.offset || len(page.Namespaces) != tc.listed || page.NextOffset != tc.next || page.Totals != (RetentionTotals{Namespaces: 101, Refs: 101}) || page.Namespaces == nil {
				t.Fatalf("offset %d: got offset %d, %d namespaces, next %d, totals %+v", tc.offset, page.Offset, len(page.Namespaces), page.NextOffset, page.Totals)
			}
			if tc.listed > 0 && page.Namespaces[0].Namespace != fmt.Sprintf("%016x", tc.offset+1) {
				t.Fatalf("offset %d starts at %s", tc.offset, page.Namespaces[0].Namespace)
			}
		}
		for _, offset := range []int{-1, MaxPageOffset + 1} {
			if page, err := store.RetentionReport(context.Background(), offset); err == nil || !reflect.DeepEqual(page, RetentionReport{}) {
				t.Fatalf("offset %d: got %+v and %v, want an error", offset, page, err)
			}
		}
	})
}

func TestRetentionReportOfARepositoryWithNoSnapshotsIsEmpty(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			for name, dir := range map[string]string{"unborn": emptyRepoIn(t, format), "with history": testRepoIn(t, format)} {
				report := reportOf(t, newStore(t, dir))
				if report.Totals != (RetentionTotals{}) || report.Namespaces == nil || len(report.Namespaces) != 0 || report.NextOffset != 0 {
					t.Fatalf("%s: got %+v", name, report)
				}
				if out, _ := json.Marshal(report); !strings.Contains(string(out), `"namespaces":[]`) {
					t.Fatalf("%s: namespaces must marshal as an empty array: %s", name, out)
				}
			}
		})
	}
}

func worktreeFingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() && rel == ".git" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			files[rel] = fmt.Sprintf("directory %v", info.Mode())
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			files[rel] = "link to " + target
		default:
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[rel] = fmt.Sprintf("%v %x", info.Mode(), sha256.Sum256(content))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func refsFingerprint(t *testing.T, dir string) string {
	t.Helper()
	return strings.Join([]string{
		git(t, dir, "rev-parse", "HEAD"),
		git(t, dir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"),
		git(t, dir, "ls-files", "--stage"),
		git(t, dir, "config", "--list", "--show-origin"),
	}, "\n--\n")
}

func TestRetentionReportWritesNothing(t *testing.T) {
	eachConfig(t, false, func(t *testing.T, dir string) {
		store := newStore(t, dir)
		writeFile(t, dir, ".gitignore", "ignored.txt\n")
		git(t, dir, "add", ".gitignore")
		git(t, dir, "commit", "-m", "ignore")
		writeFile(t, dir, "tracked.txt", "unstaged\n")
		writeFile(t, dir, "staged.txt", "staged\n")
		git(t, dir, "add", "staged.txt")
		writeFile(t, dir, "untracked.txt", "untracked\n")
		writeFile(t, dir, "ignored.txt", "ignored\n")
		create(t, store, "main")
		linked := addWorktree(t, dir, "linked")
		linkedStore := newStore(t, linked)
		writeFile(t, linked, "linked.txt", "linked\n")
		create(t, linkedStore, "linked")

		c, gone := commitOf(t, dir, "planted"), commitOf(t, dir, "gone")
		plant(t, dir, itemRef("00000000000000b1", 1, c), c)
		plant(t, dir, latestOf("00000000000000b1"), c)
		plant(t, dir, itemRef("00000000000000b2", 1, gone), gone)
		git(t, dir, "symbolic-ref", latestOf("00000000000000b3"), "refs/heads/ghost")
		plant(t, dir, refsPrefix+"README", c)
		removeLooseObject(t, dir, gone)
		if !isReftable(t, dir) {
			writeRefFile(t, dir, itemRef("00000000000000b4", 2, c), "not an object id\n")
			writeRefFile(t, dir, refsPrefix+"00000000000000b4/items/x y", c+"\n")
		}

		type state struct {
			gitDir, mainTree, linkedTree map[string]string
			refs, linkedRefs             string
		}
		take := func() state {
			return state{gitDirFiles(t, dir), worktreeFingerprint(t, dir), worktreeFingerprint(t, linked), refsFingerprint(t, dir), refsFingerprint(t, linked)}
		}
		before := take()
		for _, s := range []*Store{store, linkedStore} {
			if _, err := s.RetentionReport(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
		}
		after := take()

		if changes := gitDirChanges(before.gitDir, after.gitDir); len(changes) > 0 {
			t.Fatalf("the report changed the Git directory: %v", changes)
		}
		if !reflect.DeepEqual(before.mainTree, after.mainTree) || !reflect.DeepEqual(before.linkedTree, after.linkedTree) {
			t.Fatalf("the report changed a working tree")
		}
		if before.refs != after.refs || before.linkedRefs != after.linkedRefs {
			t.Fatalf("the report changed HEAD, a ref, the index, or the configuration:\n%s\n--- after\n%s", before.refs, after.refs)
		}
	})
}
