package snapshot

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

var objectFormats = []string{"sha1", "sha256"}

// testRepoIn is testRepo in the given object format. It skips when Git
// cannot create a repository in that format.
func testRepoIn(t *testing.T, format string) string {
	t.Helper()
	if format == "sha1" {
		return testRepo(t)
	}
	dir := t.TempDir()
	init := exec.Command("git", "init", "--object-format="+format, "-b", "main")
	init.Dir = dir
	init.Env = sanitizedEnv()
	if out, err := init.CombinedOutput(); err != nil {
		t.Skipf("git without %s support: %v: %s", format, err, out)
	}
	writeFile(t, dir, "tracked.txt", "initial\n")
	git(t, dir, "add", "tracked.txt")
	git(t, dir, "commit", "-m", "initial")
	return dir
}

func mktrees(t *testing.T, dir string, trees [][]string) []string {
	t.Helper()
	var in strings.Builder
	for _, entries := range trees {
		for _, entry := range entries {
			in.WriteString(entry + "\x00")
		}
		in.WriteString("\x00")
	}
	cmd := exec.Command("git", "mktree", "-z", "--missing", "--batch")
	cmd.Dir = dir
	cmd.Env = sanitizedEnv()
	cmd.Stdin = strings.NewReader(in.String())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("mktree --batch: %v", err)
	}
	ids := strings.FieldsFunc(string(out), func(r rune) bool { return r == '\n' || r == 0 })
	if len(ids) != len(trees) {
		t.Fatalf("mktree --batch wrote %d trees for %d", len(ids), len(trees))
	}
	return ids
}

func TestParseScopeAppliesThePathRulesInOrder(t *testing.T) {
	numbered := func(n int) []string {
		paths := make([]string, n)
		for i := range paths {
			paths[i] = fmt.Sprintf("p%d", i)
		}
		return paths
	}
	longest := strings.Repeat("a/", ScopeMaxPathBytes/2-1) + "ab"
	cases := []struct {
		name  string
		paths []string
		want  error
	}{
		{"no paths", []string{}, ErrScopeSize},
		{"nil", nil, ErrScopeSize},
		{"101 paths", numbered(101), ErrScopeSize},
		{"101 paths with a bad one", append(numbered(100), ".."), ErrScopeSize},
		{"empty path", []string{""}, ErrScopePath},
		{"leading slash", []string{"/a"}, ErrScopePath},
		{"trailing slash", []string{"a/"}, ErrScopePath},
		{"empty segment", []string{"a//b"}, ErrScopePath},
		{"dot", []string{"."}, ErrScopePath},
		{"dot segment", []string{"a/./b"}, ErrScopePath},
		{"dot-dot segment", []string{"a/../b"}, ErrScopePath},
		{"NUL", []string{"a\x00b"}, ErrScopePath},
		{"invalid UTF-8", []string{"a\xffb"}, ErrScopePath},
		{"4097 bytes", []string{longest + "c"}, ErrScopePath},
		{"a bad path after an overlap", []string{"src", "src", "a//b"}, ErrScopePath},
		{"equal paths", []string{"src", "src"}, ErrScopeOverlap},
		{"child before parent", []string{"src/a", "src"}, ErrScopeOverlap},
		{"grandparent", []string{"a", "x", "a/b/c"}, ErrScopeOverlap},
	}
	for _, tc := range cases {
		if _, err := ParseScope(tc.paths); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}

	for _, paths := range [][]string{
		{".git"},
		{"a b"},
		{"src", "srcx", "sr"},
		{"src", "docs/guide.md"},
		{"docs/guide.md", "src", "docs/other"},
		{longest},
		{"Ünï/✓"},
		numbered(100),
	} {
		scope, err := ParseScope(paths)
		if err != nil {
			t.Errorf("%q: %v", paths, err)
			continue
		}
		if got := scope.Paths(); !slices.Equal(got, paths) {
			t.Errorf("Paths() = %q, want %q in request order", got, paths)
		}
	}
	if (Scope{}).Paths() != nil {
		t.Error("the whole-tree scope must report no paths")
	}
}

func TestScopeCoversWholeSegments(t *testing.T) {
	scope, err := ParseScope([]string{"src", "docs/guide.md"})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"src":                 "src",
		"src/a.go":            "src",
		"src/a/b":             "src",
		"docs/guide.md":       "docs/guide.md",
		"docs/guide.md/x":     "docs/guide.md",
		"srcx":                "",
		"sr":                  "",
		"src.go":              "",
		"docs":                "",
		"docs/guide.mdx":      "",
		"other/src":           "",
		"other/docs/guide.md": "",
	} {
		got, ok := scope.covering(path)
		if got != want || ok != (want != "") {
			t.Errorf("covering(%q) = %q, %v; want %q", path, got, ok, want)
		}
	}
	if got, ok := (Scope{}).covering("any/path"); !ok || got != "" {
		t.Errorf("the whole-tree scope must cover every path: %q %v", got, ok)
	}
}

func TestValidObjectIDAcceptsOnlyFullLowercaseIDs(t *testing.T) {
	sha1ID, sha256ID := strings.Repeat("0123456789abcdef", 3)[:40], strings.Repeat("0123456789abcdef", 4)
	for id, want := range map[string]bool{
		sha1ID:                         true,
		sha256ID:                       true,
		strings.ToUpper(sha1ID):        false,
		sha1ID[:39]:                    false,
		sha1ID + "0":                   false,
		sha256ID[:63]:                  false,
		sha256ID + "0":                 false,
		"":                             false,
		strings.Repeat("g", 40):        false,
		sha1ID[:39] + " ":              false,
		strings.Repeat("0", 40) + "\n": false,
	} {
		if got := ValidObjectID(id); got != want {
			t.Errorf("ValidObjectID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestFoldKeyMatchesUnicodeSimpleCaseFolding(t *testing.T) {
	for _, pair := range [][2]string{
		{"README", "readme"},
		{"Docs/a", "docs/A"},
		{"K", "\u212a"},
		{"k", "\u212a"},
		{"\u03a3", "\u03c3"},
		{"\u03c3", "\u03c2"},
		{"\u00df", "\u1e9e"},
		{"a\xffB", "A\xffb"},
	} {
		if foldKey(pair[0]) != foldKey(pair[1]) {
			t.Errorf("%q and %q must share a fold key", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{
		{"\u00df", "ss"},
		{"\u0130", "i"},
		{"a", "b"},
		{"a\xff", "a\xfe"},
		{"\xff", "\ufffd"},
		{"a/b", "a\\b"},
	} {
		if foldKey(pair[0]) == foldKey(pair[1]) {
			t.Errorf("%q and %q must not share a fold key", pair[0], pair[1])
		}
	}
}

func TestTreeIDMatchesGitWriteTree(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			dir := testRepoIn(t, format)
			head := git(t, dir, "rev-parse", "HEAD")
			blob := func(path, mode string) leaf {
				return leaf{path: path, mode: mode, oid: blobOf(t, dir, "content of "+path+"\n")}
			}
			ordering := listing{blob("a b", modeFile), blob("a-b", modeFile), blob("a.b", modeFile), blob("a/x", modeFile), blob("a0", modeFile)}
			kinds := listing{blob("exe", modeExecutable), blob("link", modeSymlink), {path: "sub", mode: modeGitlink, oid: head}, blob("x.txt", modeFile), blob("x/y", modeFile)}
			names := listing{blob("d/e/f/g", modeFile), blob("d/e/h", modeFile), blob("n\xffm", modeFile), blob("Ünï/✓", modeFile)}
			trees := []listing{{}, {blob("one", modeFile)}, ordering, kinds, names, slices.Concat(ordering, kinds, names)}

			newHash := map[string]func() hash.Hash{"sha1": sha1.New, "sha256": sha256.New}[format]
			for _, leaves := range trees {
				slices.SortFunc(leaves, func(a, b leaf) int { return strings.Compare(a.path, b.path) })
				if got, want := treeID(newHash, leaves), writeTreeOf(t, dir, leaves); got != want {
					t.Errorf("treeID of %d leaves = %s, git write-tree = %s", len(leaves), got, want)
				}
			}
		})
	}
}

func writeTreeOf(t *testing.T, dir string, leaves listing) string {
	t.Helper()
	env := append(sanitizedEnv(), "GIT_INDEX_FILE="+filepath.Join(t.TempDir(), "index"))
	run := func(stdin string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	var info strings.Builder
	for _, l := range leaves {
		fmt.Fprintf(&info, "%s %s\t%s\x00", l.mode, l.oid, l.path)
	}
	run(info.String(), "update-index", "-z", "--index-info")
	if listed := strings.Count(run("", "ls-files", "-z"), "\x00"); listed != len(leaves) {
		t.Fatalf("the index holds %d of %d leaves", listed, len(leaves))
	}
	return strings.TrimSpace(run("", "write-tree"))
}

func TestUnsafePathMatchesGitOracle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git for Windows also refuses names Windows cannot create, so it is no oracle for the protect flags alone")
	}
	dir := testRepo(t)
	blob := blobOf(t, dir, "x\n")
	type sample struct{ path, mode string }
	var samples []sample
	for _, path := range unsafePathCorpus() {
		samples = append(samples, sample{path, modeFile}, sample{path, modeSymlink})
	}

	trees := make([]string, len(samples))
	pending := make([]string, len(samples))
	segments := make([][]string, len(samples))
	for i, s := range samples {
		segments[i] = strings.Split(s.path, "/")
		pending[i] = s.mode + " blob " + blob + "\t" + segments[i][len(segments[i])-1]
	}
	for depth := 1; ; depth++ {
		var batch []int
		for i := range samples {
			if len(segments[i]) >= depth {
				batch = append(batch, i)
			}
		}
		if len(batch) == 0 {
			break
		}
		entries := make([][]string, len(batch))
		for j, i := range batch {
			entries[j] = []string{pending[i]}
		}
		for j, id := range mktrees(t, dir, entries) {
			i := batch[j]
			if parent := len(segments[i]) - depth - 1; parent >= 0 {
				pending[i] = "040000 tree " + id + "\t" + segments[i][parent]
			} else {
				trees[i] = id
			}
		}
	}

	refused := make([]bool, len(samples))
	var failures []string
	var mu sync.Mutex
	next := make(chan int)
	var wg sync.WaitGroup
	scratch := t.TempDir()
	for worker := range runtime.NumCPU() {
		index := filepath.Join(scratch, fmt.Sprintf("index-%d", worker))
		wg.Go(func() {
			for i := range next {
				cmd := exec.Command("git", "-c", "core.protectNTFS=true", "-c", "core.protectHFS=true", "read-tree", trees[i])
				cmd.Dir = dir
				cmd.Env = append(sanitizedEnv(), "GIT_INDEX_FILE="+index)
				out, err := cmd.CombinedOutput()
				refused[i] = err != nil
				if err != nil && !bytes.Contains(out, []byte("invalid path")) && !bytes.Contains(out, []byte("empty filename in tree entry")) {
					mu.Lock()
					failures = append(failures, fmt.Sprintf("%q: %v: %s", samples[i].path, err, out))
					mu.Unlock()
				}
			}
		})
	}
	for i := range samples {
		next <- i
	}
	close(next)
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("read-tree failed for a reason other than the path:\n%s", strings.Join(failures, "\n"))
	}

	var disagreements []string
	for i, s := range samples {
		if got := unsafePath(s.path, s.mode); got != refused[i] {
			disagreements = append(disagreements, fmt.Sprintf("%s %q: unsafePath %v, git refuses %v", s.mode, s.path, got, refused[i]))
		}
	}
	if len(disagreements) > 0 {
		t.Fatalf("%d of %d samples disagree with git read-tree:\n%s", len(disagreements), len(samples), strings.Join(disagreements, "\n"))
	}
	t.Logf("%d samples agree with git read-tree", len(samples))
}

func unsafePathCorpus() []string {
	bases := []string{
		".git", "git~1", "git~2", "git~0", "git~10", "git~01", "git", ".gitx", "x.git", "..git", ".git.git",
		".gitmodules", "gitmod~1", "gitmod~2", "gitmod~4", "gitmod~5", "gitmod~0", "gitmod~10",
		"gi7eba~1", "gi7eba~5", "gi7eba~9", "gi7eba~0", "gi7ebb~1", ".gitmodule", ".gitmodulesx", "gitmodules",
		".gitattributes", "gitatt~1", ".gitignore", ".mailmap", "ok",
	}
	suffixes := []string{".", " ", "..", ". ", " .", ":", ":x", "::$INDEX_ALLOCATION", ".:x", " :x", "\\", "\\x", ":x\\y", "x"}
	prefixes := []string{" ", ".", "\\", "\\\\", "a\\", "a\\\\", ".\\", "x:y\\", "\\ "}
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, base := range bases {
		add(alternateCase(base))
		for _, spelling := range []string{base, strings.ToUpper(base)} {
			add(spelling)
			for _, suffix := range suffixes {
				add(spelling + suffix)
			}
			for _, prefix := range prefixes {
				add(prefix + spelling)
			}
		}
	}
	ignorable := []rune{0x200c, 0x200d, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x206a, 0x206b, 0x206c, 0x206d, 0x206e, 0x206f, 0xfeff}
	lookalikes := []rune{0x200b, 0x2060, 0x00ad, 0x034f, 0x180e, 0x2061, 0x200a}
	for _, base := range []string{".git", "git~1", ".gitmodules", "gitmod~1", "gi7eba~1", ".gitx"} {
		for _, r := range slices.Concat(ignorable, lookalikes) {
			for _, at := range []int{0, 1, len(base) - 1, len(base)} {
				add(base[:at] + string(r) + base[at:])
			}
		}
		add(base + "\u200c\u200d")
		add(base + "\u200c.")
		add(base + ".\u200c")
	}
	for _, name := range []string{
		"", ".", "..", "...", " ", "\\", ":",
		".G\u0130T", ".g\u0131t", ".\uff47it", ".gitmodule\u017f", ".GITMODULE\u017f", "\u212agit",
		".git\xff", "\xff.git", ".g\xe2\x80it", ".git\xe2\x80", "\xe2\x80\x8c\xff.git", ".git\u200c\xff", "\xff",
		".gitmodules\xff", "\xc0\xaegit", ".git\xed\xa0\x80", ".gi\xc1\xb4", ".gitmodules.\\x",
		"a\\.gitmodules\\x", "a\\.gitmodules:x\\y", "a\\b\\.git", "a\\\\.git", ".git\\\\",
		"a/.git", ".git/a", "a/b/GIT~1/c", ".Git./hooks/x", "a/.git:x/b",
		".gitmodules/x", "a/.gitmodules", "a/.GITMODULES.", "a/gitmod~1", "a/b/gi7eba~3 ",
		"a/.gitmodules/b", ".GITMODULES./x", "gitmod~1/x", ".gitmodules\\x/y", "a/gi7eba~1:x/b",
		"a/\\.gitmodules", "gitmod~1:x/y", "gitmod~1 /x", "x/gitmod~1:y/z", ".gitmodules.:x/y", "a\\gitmod~1/x",
		"a\\gitmod~1:z/x", "\\gitmod~1:z/x", "a/b\\c/\\.gitmodules", ".gitmodules\u200c/x", "x/.gitmodules/y/z",
		"a/x\\.gitmodules", "x/.gitmodules./y", ".gitmodules /x", "a/b\\gi7eba~1/c/d", "\\\\.gitmodules/b",
		".gitmodules\\/b", "a/\\.git", "\\.git/x", "a/\\git~1", ".git./x", "git~1:y/z",
		"a/b", "a//b", "a/./b", "a/../b", "./a", "a/.", "a/..", "a/", "/a", "x\\y/.git", "a\\b/c",
	} {
		add(name)
	}
	return names
}

func alternateCase(s string) string {
	b := []byte(s)
	upper := true
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			if upper {
				b[i] = c - 'a' + 'A'
			}
			upper = !upper
		}
	}
	return string(b)
}
