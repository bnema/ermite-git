// SPDX-License-Identifier: MIT

package git

import (
	"context"
	"fmt"
	"strings"
)

// LastCommits finds, in one walk, the last commit that touched each entry of
// a directory: for every name, the same commit as
//
//	git log -n 1 <start> -- <dir>/<name>
//
// with the same history simplification. The result has one id per name, in
// the order of names; the zero ObjectID means that no commit reachable from
// start has the entry (it never existed). Read the commits you need with
// ReadCommit; many names share the same commit.
//
// dir is relative to the root of the repository ("" or "." for the root
// itself). Each name is one path component, taken literally.
//
// Algorithm. The history of one path is a walk where a merge that is
// TREESAME to a parent follows only the first such parent, so two paths can
// follow different parents at the same merge and then visit commits in
// different orders. A single shared walk would therefore not give the
// per-path answers. Instead the names travel as groups. All names start in
// one group, which owns the queue and the set of seen commits of their (so
// far identical) walks. For each popped commit the group computes, per name,
// the first parent whose entry is identical to the commit's; a name with no
// such parent is changed by the commit: it is resolved and leaves the group.
// The other names follow exactly the parent they matched (the only parent
// for a commit that has one). When the names of a group match different
// parents of a merge, the group splits: the queue and the seen set are
// cloned, each part follows its parent. Every group therefore holds the
// exact state of the individual walk of each of its names, and the answers
// are those of N separate walks, but a commit is loaded and compared once
// per group instead of once per name. The walk ends when every name has an
// answer. A directory that did not change (same tree id, or a Bloom filter
// that says so) settles all the names of a group at once.
func (r *Repo) LastCommits(ctx context.Context, start ObjectID, dir string, names []string) ([]ObjectID, error) {
	res := make([]ObjectID, len(names))
	if len(names) == 0 {
		return res, nil
	}
	spec, err := parsePathSpec(dir)
	if err != nil {
		return nil, err
	}
	spec.dirOnly = false
	// Distinct names; duplicates copy their answer at the end.
	idx := make(map[string]int32, len(names))
	uniq := make([]string, 0, len(names))
	for _, n := range names {
		if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
			return nil, fmt.Errorf("%w: entry name %q", ErrInvalidPath, n)
		}
		if _, ok := idx[n]; !ok {
			idx[n] = int32(len(uniq))
			uniq = append(uniq, n)
		}
	}
	w, err := r.newWalker(ctx, start, WalkOptions{})
	if err != nil {
		return nil, err
	}
	defer w.close()
	s := &lastState{
		w: w, d: newPathDiffer(r, spec), idx: idx, n: len(uniq),
		found:     make([]ObjectID, len(uniq)),
		remaining: len(uniq),
	}
	if spec.bloom {
		s.key = newBloomKey([]byte(spec.String()))
		s.useBloom = s.key.usable
	}
	s.maxTrees = max(64, (16<<20)/(len(uniq)*28+64))
	s.cache = make(map[ObjectID][]pathEntry, 256)
	s.absent = make([]pathEntry, len(uniq))
	if err := s.run(); err != nil {
		return nil, err
	}
	for i, n := range names {
		res[i] = s.found[idx[n]]
	}
	return res, nil
}

// LastCommit returns the first commit of Walk with the given path and Limit
// 1, that is the answer of "git log -n 1 <start> -- <path>". The boolean is
// false when no commit has the path.
func (r *Repo) LastCommit(ctx context.Context, start ObjectID, path string) (ObjectID, bool, error) {
	for c, err := range r.Walk(ctx, start, WalkOptions{Path: path, Limit: 1, IDOnly: true}) {
		if err != nil {
			return ObjectID{}, false, err
		}
		return c.ID, true, nil
	}
	return ObjectID{}, false, nil
}

// lastGroup is a set of names whose individual walks are in the same state.
type lastGroup struct {
	walkState
	active []int32
}

type lastState struct {
	w *walker
	d *pathDiffer // resolves the directory
	// useBloom is set when the directory is spelled the way Git inserts it.
	useBloom bool
	key      bloomKey

	idx       map[string]int32
	n         int
	found     []ObjectID
	remaining int

	cache    map[ObjectID][]pathEntry // entries of the names, by directory tree
	maxTrees int
	absent   []pathEntry // all names missing
	buf      []byte

	// scratch
	ks []int8
}

// dirOf finds the directory in a root tree.
func (s *lastState) dirOf(root ObjectID) (ObjectID, bool, error) {
	if len(s.d.comps) == 0 {
		return root, true, nil
	}
	e, err := s.d.resolve(root)
	if err != nil {
		return ObjectID{}, false, err
	}
	return e.id, e.found && e.mode.IsTree(), nil
}

// entries returns what the directory tree holds for each name; absent
// directories hold nothing. The slice is shared: do not modify it.
func (s *lastState) entries(dir ObjectID, ok bool) ([]pathEntry, error) {
	if !ok || dir == emptyTreeID {
		return s.absent, nil
	}
	if e, hit := s.cache[dir]; hit {
		return e, nil
	}
	typ, data, err := s.w.r.ReadObject(dir, s.buf)
	if err != nil {
		return nil, err
	}
	s.buf = data
	if typ != TypeTree {
		return nil, wrongType(dir, typ, TypeTree)
	}
	ents := make([]pathEntry, s.n)
	if err := ParseTree(data, func(e Entry) bool {
		if i, hit := s.idx[string(e.Name)]; hit {
			ents[i] = pathEntry{id: e.ID, mode: e.Mode, found: true}
		}
		return true
	}); err != nil {
		return nil, wrapObj(dir, err)
	}
	if len(s.cache) >= s.maxTrees {
		clear(s.cache)
	}
	s.cache[dir] = ents
	return ents, nil
}

func (s *lastState) run() error {
	root := &lastGroup{walkState: s.w.walkState, active: make([]int32, s.n)}
	for i := range root.active {
		root.active[i] = int32(i)
	}
	stack := []*lastGroup{root}
	visited := 0
	for len(stack) > 0 && s.remaining > 0 {
		g := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for len(g.q) > 0 && len(g.active) > 0 && s.remaining > 0 {
			if visited++; visited%walkCheckEvery == 0 {
				if err := s.w.ctx.Err(); err != nil {
					return err
				}
			}
			extra, err := s.step(g)
			if err != nil {
				return err
			}
			stack = append(stack, extra...)
		}
	}
	return nil
}

// step processes the next commit of the group. It returns the groups split
// off from g.
func (s *lastState) step(g *lastGroup) ([]*lastGroup, error) {
	w := s.w
	n := g.pop().n
	tree, parents, err := w.load(n, w.parents[:0])
	w.parents = parents
	if err != nil {
		return nil, err
	}
	if len(parents) == 0 {
		dir, ok, err := s.dirOf(tree)
		if err != nil {
			return nil, err
		}
		ents, err := s.entries(dir, ok)
		if err != nil {
			return nil, err
		}
		return nil, s.resolve(g, n, func(i int32) bool { return ents[i].found })
	}

	// Fast path: the directory is unchanged against the first parent.
	if s.useBloom && n.ing {
		if maybe, ok := w.g.maybeChangedKey(n.pos, &s.key); ok && !maybe {
			return nil, s.follow(g, parents[0])
		}
	}
	dir, ok, err := s.dirOf(tree)
	if err != nil {
		return nil, err
	}
	p0tree, err := w.treeOf(parents[0])
	if err != nil {
		return nil, err
	}
	pdir, pok, err := s.dirOf(p0tree)
	if err != nil {
		return nil, err
	}
	if ok == pok && (!ok || dir == pdir) {
		return nil, s.follow(g, parents[0])
	}

	ec, err := s.entries(dir, ok)
	if err != nil {
		return nil, err
	}
	if cap(s.ks) < s.n {
		s.ks = make([]int8, s.n)
	}
	ks := s.ks[:s.n]
	// The first parent each active name is identical in, -1 if none.
	for _, i := range g.active {
		ks[i] = -1
	}
	for pi, p := range parents {
		var pe []pathEntry
		if pi == 0 {
			pe, err = s.entries(pdir, pok)
		} else {
			var pt ObjectID
			if pt, err = w.treeOf(p); err == nil {
				var d2 ObjectID
				var ok2 bool
				if d2, ok2, err = s.dirOf(pt); err == nil {
					pe, err = s.entries(d2, ok2)
				}
			}
		}
		if err != nil {
			return nil, err
		}
		for _, i := range g.active {
			if ks[i] < 0 && pe[i] == ec[i] {
				ks[i] = int8(pi)
			}
		}
	}
	// Names changed against every parent are resolved here.
	live := g.active[:0]
	for _, i := range g.active {
		if ks[i] < 0 {
			id, err := w.idOf(n)
			if err != nil {
				return nil, err
			}
			s.found[i] = id
			s.remaining--
			continue
		}
		live = append(live, i)
	}
	g.active = live
	if len(live) == 0 {
		return nil, nil
	}
	// Split the group by the parent each remaining name follows.
	var extra []*lastGroup
	first := true
	var firstParent node
	for pi := range parents {
		var part []int32
		for _, i := range live {
			if int(ks[i]) == pi {
				part = append(part, i)
			}
		}
		if len(part) == 0 {
			continue
		}
		if first {
			first, firstParent = false, parents[pi]
			g.active = part
			continue
		}
		ng := &lastGroup{walkState: g.clone(), active: part}
		if err := s.follow(ng, parents[pi]); err != nil {
			return nil, err
		}
		extra = append(extra, ng)
	}
	// g is still in the state before inserting the parent; the clones were
	// made before g follows its own.
	return extra, s.follow(g, firstParent)
}

// resolve settles the active names for which changed is true at commit n
// (a root commit), and keeps the others.
func (s *lastState) resolve(g *lastGroup, n node, changed func(int32) bool) error {
	id, err := s.w.idOf(n)
	if err != nil {
		return err
	}
	live := g.active[:0]
	for _, i := range g.active {
		if changed(i) {
			s.found[i] = id
			s.remaining--
			continue
		}
		live = append(live, i)
	}
	g.active = live
	return nil
}

// follow queues p in the walk of g unless it was already seen.
func (s *lastState) follow(g *lastGroup, p node) error {
	if !g.seen.add(p) {
		return nil
	}
	t, err := s.w.timeOf(p)
	if err != nil {
		return err
	}
	g.push(t, p)
	return nil
}
