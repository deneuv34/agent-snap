package snapshot

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeSymlink    = "120000"
	modeGitlink    = "160000"
)

type leaf struct{ path, mode, oid string }

type listing []leaf

// treeID matches git write-tree. A subdirectory sorts as name+"/", modes
// carry no leading zeros, and object IDs are raw bytes.
func treeID(newHash func() hash.Hash, leaves listing) string {
	return hex.EncodeToString(treeHash(newHash, "", leaves))
}

func treeHash(newHash func() hash.Hash, prefix string, leaves listing) []byte {
	type entry struct {
		name, key, mode string
		id              []byte
	}
	var entries []entry
	for i := 0; i < len(leaves); {
		name, _, inDir := strings.Cut(leaves[i].path[len(prefix):], "/")
		if !inDir {
			id, _ := hex.DecodeString(leaves[i].oid)
			entries = append(entries, entry{name, name, leaves[i].mode, id})
			i++
			continue
		}
		dir := prefix + name + "/"
		end := i
		for end < len(leaves) && strings.HasPrefix(leaves[end].path, dir) {
			end++
		}
		entries = append(entries, entry{name, name + "/", "40000", treeHash(newHash, dir, leaves[i:end])})
		i = end
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.key, b.key) })
	var body bytes.Buffer
	for _, e := range entries {
		body.WriteString(e.mode + " " + e.name + "\x00")
		body.Write(e.id)
	}
	h := newHash()
	fmt.Fprintf(h, "tree %d\x00", body.Len())
	h.Write(body.Bytes())
	return h.Sum(nil)
}

func (l listing) find(p string) (int, bool) {
	return slices.BinarySearchFunc(l, p, func(e leaf, p string) int { return strings.Compare(e.path, p) })
}

func (l listing) at(p string) (leaf, bool) {
	if i, ok := l.find(p); ok {
		return l[i], true
	}
	return leaf{}, false
}

func (l listing) hasDir(d string) bool {
	i, _ := l.find(d + "/")
	return i < len(l) && strings.HasPrefix(l[i].path, d+"/")
}

func (l listing) check() error {
	for i, lf := range l {
		if i > 0 && l[i-1].path == lf.path {
			return fmt.Errorf("tree names %q twice", lf.path)
		}
		if l.hasDir(lf.path) {
			return fmt.Errorf("tree holds %q and a path below it", lf.path)
		}
	}
	return nil
}

type headView struct {
	unborn  bool
	delta   map[string]*leaf
	current listing
}

func (h headView) at(p string) (leaf, bool) {
	if h.unborn {
		return leaf{}, false
	}
	if l, ok := h.delta[p]; ok {
		if l == nil {
			return leaf{}, false
		}
		return *l, true
	}
	return h.current.at(p)
}

type change struct {
	path                        string
	cur, tgt                    *leaf
	staged, unstaged, untracked bool
	blocker                     PathBlocker
}

func (c *change) action() Action {
	switch {
	case c.cur == nil:
		return ActionCreate
	case c.tgt == nil:
		return ActionDelete
	case kind(c.cur.mode) != kind(c.tgt.mode):
		return ActionTypeChange
	}
	return ActionModify
}

func kind(mode string) string {
	if mode == modeExecutable {
		return modeFile
	}
	return mode
}

func (c *change) writesTarget() bool { return c.tgt != nil }

func (c *change) checkoutMode() string {
	if c.tgt != nil {
		return c.tgt.mode
	}
	return c.cur.mode
}

func (c *change) hasMode(mode string) bool {
	return c.cur != nil && c.cur.mode == mode || c.tgt != nil && c.tgt.mode == mode
}

func (c *change) setFlags(index indexView, head headView) {
	entry, indexed := index.stage0[c.path]
	inHead, headed := head.at(c.path)
	c.staged = indexed != headed || indexed && (entry.mode != inHead.mode || entry.oid != inHead.oid)
	c.unstaged = indexed && (c.cur == nil || entry.mode != c.cur.mode || entry.oid != c.cur.oid)
	c.untracked = c.cur != nil && !index.tracked[c.path]
}

func (c *change) entry() PlanEntry {
	var lossy bool
	e := PlanEntry{
		Path: repairUTF8([]byte(c.path), &lossy), Action: c.action(),
		Staged: c.staged, Unstaged: c.unstaged, Untracked: c.untracked, Blocker: c.blocker,
	}
	if c.cur != nil {
		e.Current = &PlanSide{Mode: c.cur.mode, OID: c.cur.oid}
	}
	if c.tgt != nil {
		e.Target = &PlanSide{Mode: c.tgt.mode, OID: c.tgt.oid}
	}
	return e
}

type plan struct {
	scope     Scope
	current   listing
	changes   []change
	result    listing
	unmatched []string
}

func buildPlan(scope Scope, cur, tgt listing) *plan {
	p := &plan{scope: scope, current: cur, unmatched: []string{}}
	matched := map[string]bool{}
	for i, j := 0, 0; i < len(cur) || j < len(tgt); {
		var c, t *leaf
		switch {
		case j == len(tgt) || i < len(cur) && cur[i].path < tgt[j].path:
			c, i = &cur[i], i+1
		case i == len(cur) || tgt[j].path < cur[i].path:
			t, j = &tgt[j], j+1
		default:
			c, t, i, j = &cur[i], &tgt[j], i+1, j+1
		}
		path := pathOf(c, t)
		by, covered := scope.covering(path)
		if !covered {
			if c != nil {
				p.result = append(p.result, *c)
			}
			continue
		}
		matched[by] = true
		if t != nil {
			p.result = append(p.result, *t)
		}
		if c == nil || t == nil || *c != *t {
			p.changes = append(p.changes, change{path: path, cur: c, tgt: t})
		}
	}
	for _, path := range scope.paths {
		if !matched[path] {
			p.unmatched = append(p.unmatched, path)
		}
	}
	return p
}

func pathOf(c, t *leaf) string {
	if c != nil {
		return c.path
	}
	return t.path
}

func (p *plan) paths() map[string]bool {
	paths := make(map[string]bool, len(p.changes))
	for _, c := range p.changes {
		paths[c.path] = true
	}
	return paths
}

func (p *plan) removed() []string {
	var paths []string
	for _, c := range p.changes {
		if c.cur != nil {
			paths = append(paths, c.path)
		}
	}
	return paths
}

type world struct {
	index indexView
	head  headView
	disk  diskProbe
	nodes map[string][]string
}

func newWorld(root string, p *plan, index indexView, head headView, folds bool) *world {
	w := &world{index: index, head: head, disk: newDiskProbe(root, p.removed(), folds)}
	if folds {
		w.nodes = foldNodes(p.result)
	}
	return w
}

func foldNodes(result listing) map[string][]string {
	nodes := map[string][]string{}
	seen := map[string]bool{}
	add := func(node string) {
		if seen[node] {
			return
		}
		seen[node] = true
		key := foldKey(node)
		nodes[key] = append(nodes[key], node)
	}
	for _, l := range result {
		for _, d := range parents(l.path) {
			add(d)
		}
		add(l.path)
	}
	return nodes
}

// The rules ahead of untracked_in_the_way stop every invalid or unsafe path,
// such as one through .git or "..", before any rule reads the file system.
var pathRules = [...]struct {
	blocker PathBlocker
	applies func(p *plan, w *world, c *change) bool
}{
	{BlockUnaddressablePath, func(_ *plan, _ *world, c *change) bool { return !utf8.ValidString(c.path) }},
	{BlockUnsafePath, func(_ *plan, _ *world, c *change) bool { return unsafePath(c.path, c.checkoutMode()) }},
	{BlockSubmodule, func(_ *plan, _ *world, c *change) bool { return c.hasMode(modeGitlink) }},
	{BlockParentConflict, func(p *plan, _ *world, c *change) bool { return p.parentConflict(c) }},
	{BlockIndexFlag, func(_ *plan, w *world, c *change) bool { return w.index.stage0[c.path].flagged }},
	{BlockNameCollision, func(_ *plan, w *world, c *change) bool { return w.collision(c) }},
	{BlockUntrackedInTheWay, func(_ *plan, w *world, c *change) bool { return c.writesTarget() && w.disk.inTheWay(c.path) }},
}

func (p *plan) decide(ctx context.Context, w *world) error {
	for i := range p.changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		c := &p.changes[i]
		c.setFlags(w.index, w.head)
		for _, rule := range pathRules {
			if rule.applies(p, w, c) {
				c.blocker = rule.blocker
				break
			}
		}
	}
	return nil
}

func (p *plan) parentConflict(c *change) bool {
	if p.scope.whole() || !c.writesTarget() {
		return false
	}
	for _, d := range parents(c.path) {
		if _, covered := p.scope.covering(d); !covered {
			if _, isLeaf := p.current.at(d); isLeaf {
				return true
			}
		}
	}
	return false
}

func (w *world) collision(c *change) bool {
	if w.nodes == nil {
		return false
	}
	if c.writesTarget() && w.foldsOnto(c.path) {
		return true
	}
	return slices.ContainsFunc(parents(c.path), w.foldsOnto)
}

func (w *world) foldsOnto(node string) bool {
	return slices.ContainsFunc(w.nodes[foldKey(node)], func(other string) bool { return other != node })
}

func (p *plan) resultTree(newHash func() hash.Hash) string {
	if p.result.check() != nil {
		return ""
	}
	return treeID(newHash, p.result)
}

func (p *plan) page(snap PlanSnapshot, q PlanQuery, current, result string, blockers []RepoBlocker) RestorePlan {
	out := RestorePlan{
		Snapshot: snap, Paths: q.Scope.Paths(), CurrentTree: current, ResultTree: result,
		Blockers: blockers, Unmatched: p.unmatched, Offset: q.Offset, Entries: []PlanEntry{},
	}
	for i := range p.changes {
		c := &p.changes[i]
		out.Totals.count(c)
		if i >= q.Offset && len(out.Entries) < pageSize {
			out.Entries = append(out.Entries, c.entry())
		}
	}
	if end := q.Offset + len(out.Entries); end < out.Totals.Entries {
		out.NextOffset = end
	}
	out.Applicable = len(blockers) == 0 && len(p.unmatched) == 0 && out.Totals.Blocked == 0
	return out
}

func (t *PlanTotals) count(c *change) {
	t.Entries++
	switch c.action() {
	case ActionCreate:
		t.Create++
	case ActionModify:
		t.Modify++
	case ActionTypeChange:
		t.TypeChange++
	case ActionDelete:
		t.Delete++
	}
	for _, flag := range []struct {
		set   bool
		total *int
	}{{c.staged, &t.Staged}, {c.unstaged, &t.Unstaged}, {c.untracked, &t.Untracked}, {c.blocker != "", &t.Blocked}} {
		if flag.set {
			*flag.total++
		}
	}
}
