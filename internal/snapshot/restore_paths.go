package snapshot

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	ScopeMaxPaths     = 100
	ScopeMaxPathBytes = 4096
)

var (
	ErrScopeSize    = errors.New("restore scope must hold 1 to 100 paths")
	ErrScopePath    = errors.New("restore scope holds a path that is not a repository path")
	ErrScopeOverlap = errors.New("restore scope holds overlapping paths")
)

type Scope struct {
	paths []string
	set   map[string]bool
}

func ParseScope(paths []string) (Scope, error) {
	if len(paths) < 1 || len(paths) > ScopeMaxPaths {
		return Scope{}, fmt.Errorf("%w: got %d", ErrScopeSize, len(paths))
	}
	for _, p := range paths {
		if !repositoryPath(p) {
			return Scope{}, fmt.Errorf("%w: %q", ErrScopePath, p)
		}
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		if set[p] {
			return Scope{}, fmt.Errorf("%w: %q appears twice", ErrScopeOverlap, p)
		}
		set[p] = true
	}
	for _, p := range paths {
		for _, d := range parents(p) {
			if set[d] {
				return Scope{}, fmt.Errorf("%w: %q covers %q", ErrScopeOverlap, d, p)
			}
		}
	}
	return Scope{paths: slices.Clone(paths), set: set}, nil
}

func (s Scope) Paths() []string { return slices.Clone(s.paths) }

func (s Scope) whole() bool { return s.set == nil }

func (s Scope) covering(p string) (string, bool) {
	if s.whole() {
		return "", true
	}
	if s.set[p] {
		return p, true
	}
	for _, d := range parents(p) {
		if s.set[d] {
			return d, true
		}
	}
	return "", false
}

func repositoryPath(p string) bool {
	if len(p) > ScopeMaxPathBytes || !utf8.ValidString(p) || strings.Contains(p, "\x00") {
		return false
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func parents(p string) []string {
	var dirs []string
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			dirs = append(dirs, p[:i])
		}
	}
	return dirs
}

// unsafePath mirrors Git's checkout refusal under core.protectNTFS=true and
// core.protectHFS=true. TestUnsafePathMatchesGitOracle pins it to Git.
func unsafePath(path, mode string) bool {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || dotGit(segment) {
			return true
		}
		if mode == modeSymlink && dotGitmodules(segment, i == len(segments)-1) {
			return true
		}
	}
	return false
}

func dotGit(segment string) bool {
	return hfsName(segment) == ".git" || slices.ContainsFunc(ntfsNames(segment, `:\`), func(name string) bool {
		return name == ".git" || name == "git~1"
	})
}

// dotGitmodules follows Git, which reads the NTFS name of a segment that is
// not the last one on into the next segment unless a colon ends it, so only a
// colon lets it match.
func dotGitmodules(segment string, last bool) bool {
	ntfs := segment
	if !last {
		ntfs += "/"
	}
	return hfsName(segment) == ".gitmodules" || slices.ContainsFunc(ntfsNames(ntfs, ":"), func(name string) bool {
		return name == ".gitmodules" || shortName(name, "gitmod", '4') || shortName(name, "gi7eba", '9')
	})
}

// hfsName drops the code points that Apple's TN1150 lists as ignorable and,
// as Git does, stops at the first byte that is not valid UTF-8.
func hfsName(segment string) string {
	var b strings.Builder
	for i := 0; i < len(segment); {
		r, size := utf8.DecodeRuneInString(segment[i:])
		if r == utf8.RuneError && size <= 1 {
			break
		}
		if !(r >= 0x200c && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x206a && r <= 0x206f || r == 0xfeff) {
			b.WriteRune(r)
		}
		i += size
	}
	return lowerASCII(b.String())
}

func lowerASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// ntfsNames follows Git in starting a name after each backslash that is not
// the first byte.
func ntfsNames(segment, stop string) []string {
	starts := []int{0}
	for i := 1; i < len(segment); i++ {
		if segment[i] == '\\' {
			starts = append(starts, i+1)
		}
	}
	names := make([]string, len(starts))
	for i, start := range starts {
		name := segment[start:]
		if end := strings.IndexAny(name, stop); end >= 0 {
			name = name[:end]
		}
		names[i] = lowerASCII(strings.TrimRight(name, ". "))
	}
	return names
}

func shortName(name, prefix string, last byte) bool {
	rest, ok := strings.CutPrefix(name, prefix+"~")
	return ok && len(rest) == 1 && rest[0] >= '1' && rest[0] <= last
}

func foldKey(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte(s[i])
		} else {
			b.WriteRune(smallestFold(r))
		}
		i += size
	}
	return b.String()
}

func smallestFold(r rune) rune {
	smallest := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		smallest = min(smallest, f)
	}
	return smallest
}
