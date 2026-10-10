// SPDX-License-Identifier: MIT

package git

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"time"
)

// This file implements history walks that match "git log" exactly.
//
// Order. Git's default walk keeps the commits to visit in a list sorted by
// committer date, newest first; a parent is inserted after every queued
// commit that has the same date or a newer one (commit_list_insert_by_date),
// and the initial list is sorted by a stable sort. The result is a priority
// queue by (date descending, insertion order ascending), which is what
// queue implements. A commit is queued the first time it is seen as a parent
// and never again. The commit-graph is not needed for the order; it only
// makes parents, dates and root trees cheap to read.
//
// Path history (git's default simplification). When a path is given, a
// commit is compared with its parents in order. The first parent that has
// the same content at the path makes the commit TREESAME: the commit is not
// shown and only that parent is followed. A commit that differs from every
// parent is shown and all its parents are followed. A root commit is shown
// when the path exists in it. The rewriting of parents that git does when
// printing does not change which commits are shown, so it is not done.
//
// The Bloom filter of a commit answers "did the path change relative to the
// first parent"; when the answer is a definite no, the commit is TREESAME to
// its first parent without reading any tree.

// WalkOptions selects what Walk yields.
type WalkOptions struct {
	// Skip is the number of commits to leave out, counted after path
	// simplification, like "git log --skip".
	Skip int
	// Limit is the number of commits to yield, like "git log -n". Zero or
	// less means no limit.
	Limit int
	// Path restricts the history like "git log -- <path>": a file, a
	// directory or a submodule, written relative to the root of the
	// repository, taken literally (no globs). The empty path means no
	// restriction. "." and "dir/" are accepted; a path outside the
	// repository gives ErrInvalidPath.
	Path string
	// IDOnly makes Walk fill only ID, Tree, Parents and Committer.Time when
	// the commit-graph has the commit, which then costs no object read.
	// Without it every field is filled.
	IDOnly bool
}

// walkCheckEvery is the number of visited commits between two checks of the
// context.
const walkCheckEvery = 64

// Walk yields the commits reachable from start in the order of "git log
// <start>", restricted as opts says. The yielded *Commit is reused: it and
// the bytes it points to are valid until the next iteration. On failure the
// sequence yields (nil, err) once and ends. start must be a commit id; use
// PeelToCommit first for a tag.
//
// A Walk uses the commit-graph (parents, dates, root trees, changed-path
// Bloom filters) when the repository has one, and reads the objects for the
// commits it does not cover. The graph in use when the walk starts is kept
// for its whole duration, even if Refresh replaces it.
func (r *Repo) Walk(ctx context.Context, start ObjectID, opts WalkOptions) iter.Seq2[*Commit, error] {
	return func(yield func(*Commit, error) bool) {
		w, err := r.newWalker(ctx, start, opts)
		if err != nil {
			yield(nil, err)
			return
		}
		defer w.close()
		if err := w.run(yield); err != nil {
			yield(nil, err)
		}
	}
}

// node designates a commit: by position when the graph has it, else by id.
type node struct {
	id  ObjectID
	pos uint32
	ing bool // in the graph: pos is valid and id is not set
}

type qent struct {
	time int64
	seq  uint64
	n    node
}

// walkState is the part of a walk that decides its order: the queue of
// commits to visit and the set of those already queued.
type walkState struct {
	q    []qent
	seq  uint64
	seen seenSet
}

func (s *walkState) clone() walkState {
	return walkState{q: slices.Clone(s.q), seq: s.seq, seen: s.seen.clone()}
}

type walker struct {
	r    *Repo
	g    *commitGraph // retained, may be nil
	ctx  context.Context
	opts WalkOptions

	walkState

	path *pathDiffer // nil without a path
	key  bloomKey
	// useBloom is set when the path is spelled the way Git inserts it.
	useBloom bool

	cm      Commit // scratch for commits read to follow history
	out     Commit // what is yielded
	pbuf    []uint32
	parents []node
}

func (r *Repo) newWalker(ctx context.Context, start ObjectID, opts WalkOptions) (*walker, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	w := &walker{r: r, ctx: ctx, opts: opts}
	if opts.Skip < 0 {
		w.opts.Skip = 0
	}
	if opts.Path != "" {
		spec, err := parsePathSpec(opts.Path)
		if err != nil {
			return nil, err
		}
		w.path = newPathDiffer(r, spec)
		if spec.bloom {
			w.key = newBloomKey([]byte(spec.String()))
			w.useBloom = w.key.usable
		}
	}
	g, err := r.acquireGraph()
	if err != nil {
		return nil, err
	}
	w.g = g
	w.seen.init(g)
	n, err := w.nodeOf(start)
	if err != nil {
		w.close()
		return nil, err
	}
	t, err := w.timeOf(n)
	if err != nil {
		w.close()
		return nil, err
	}
	w.seen.add(n)
	w.push(t, n)
	return w, nil
}

func (w *walker) close() {
	if w.g != nil {
		_ = w.g.release()
		w.g = nil
	}
}

// nodeOf resolves an id to a node, preferring the graph.
func (w *walker) nodeOf(id ObjectID) (node, error) {
	if w.g != nil {
		if pos, ok := w.g.lookup(id); ok {
			return node{pos: pos, ing: true}, nil
		}
	}
	return node{id: id}, nil
}

// idOf returns the object id of n.
func (w *walker) idOf(n node) (ObjectID, error) {
	if !n.ing {
		return n.id, nil
	}
	id, err := w.g.commitID(n.pos)
	return ObjectID(id), err
}

// timeOf returns the committer date of n.
func (w *walker) timeOf(n node) (int64, error) {
	if n.ing {
		return w.g.commitTime(n.pos)
	}
	if err := w.r.ReadCommit(n.id, &w.cm); err != nil {
		return 0, err
	}
	return w.cm.Committer.Time, nil
}

// treeOf returns the root tree of n.
func (w *walker) treeOf(n node) (ObjectID, error) {
	if n.ing {
		t, err := w.g.rootTree(n.pos)
		return ObjectID(t), err
	}
	if err := w.r.ReadCommit(n.id, &w.cm); err != nil {
		return ObjectID{}, err
	}
	return w.cm.Tree, nil
}

// load returns the root tree of n and appends its parents to dst.
func (w *walker) load(n node, dst []node) (tree ObjectID, out []node, err error) {
	if n.ing {
		t, err := w.g.rootTree(n.pos)
		if err != nil {
			return tree, dst, err
		}
		w.pbuf, err = w.g.appendParents(w.pbuf[:0], n.pos)
		if err != nil {
			return tree, dst, err
		}
		for _, p := range w.pbuf {
			dst = append(dst, node{pos: p, ing: true})
		}
		return ObjectID(t), dst, nil
	}
	if err := w.r.ReadCommit(n.id, &w.cm); err != nil {
		return tree, dst, err
	}
	for _, p := range w.cm.Parents {
		pn, _ := w.nodeOf(p)
		dst = append(dst, pn)
	}
	return w.cm.Tree, dst, nil
}

// run drives the walk.
func (w *walker) run(yield func(*Commit, error) bool) error {
	skip, limit := w.opts.Skip, w.opts.Limit
	for visited := 0; len(w.q) > 0; visited++ {
		if visited%walkCheckEvery == 0 {
			if err := w.ctx.Err(); err != nil {
				return err
			}
		}
		n := w.pop().n
		tree, parents, err := w.load(n, w.parents[:0])
		w.parents = parents
		if err != nil {
			return err
		}
		show := true
		if w.path != nil {
			same, followed, err := w.simplify(n, tree, parents)
			if err != nil {
				return err
			}
			show = !same
			parents = followed
		}
		if show {
			if skip > 0 {
				skip--
			} else {
				c, err := w.fill(n)
				if err != nil {
					return err
				}
				if !yield(c, nil) {
					return nil
				}
				if limit > 0 {
					if limit--; limit == 0 {
						return nil
					}
				}
			}
		}
		for _, p := range parents {
			if !w.seen.add(p) {
				continue
			}
			t, err := w.timeOf(p)
			if err != nil {
				return err
			}
			w.push(t, p)
		}
	}
	return nil
}

// simplify applies git's default history simplification to n. It reports
// whether n is TREESAME and returns the parents to follow.
func (w *walker) simplify(n node, tree ObjectID, parents []node) (same bool, follow []node, err error) {
	if len(parents) == 0 {
		if w.useBloom && n.ing {
			if maybe, ok := w.g.maybeChangedKey(n.pos, &w.key); ok && !maybe {
				return true, parents, nil
			}
		}
		ex, err := w.path.exists(tree)
		return !ex, parents, err
	}
	for i, p := range parents {
		if i == 0 && w.useBloom && n.ing {
			if maybe, ok := w.g.maybeChangedKey(n.pos, &w.key); ok && !maybe {
				return true, parents[:1], nil
			}
		}
		pt, err := w.treeOf(p)
		if err != nil {
			return false, nil, err
		}
		s, err := w.path.same(pt, tree)
		if err != nil {
			return false, nil, err
		}
		if s {
			return true, parents[i : i+1], nil
		}
	}
	return false, parents, nil
}

// fill prepares the commit to yield.
func (w *walker) fill(n node) (*Commit, error) {
	id, err := w.idOf(n)
	if err != nil {
		return nil, err
	}
	c := &w.out
	if w.opts.IDOnly && n.ing {
		tree, err := w.g.rootTree(n.pos)
		if err != nil {
			return nil, err
		}
		t, err := w.g.commitTime(n.pos)
		if err != nil {
			return nil, err
		}
		w.pbuf, err = w.g.appendParents(w.pbuf[:0], n.pos)
		if err != nil {
			return nil, err
		}
		*c = Commit{buf: c.buf, Parents: c.Parents[:0]}
		c.ID, c.Tree = id, ObjectID(tree)
		c.Committer.Time = t
		for _, p := range w.pbuf {
			pid, err := w.g.commitID(p)
			if err != nil {
				return nil, err
			}
			c.Parents = append(c.Parents, ObjectID(pid))
		}
		return c, nil
	}
	if err := w.r.ReadCommit(id, c); err != nil {
		return nil, err
	}
	return c, nil
}

// push adds a commit to the queue.
func (w *walkState) push(t int64, n node) {
	w.seq++
	w.q = append(w.q, qent{time: t, seq: w.seq, n: n})
	i := len(w.q) - 1
	for i > 0 {
		p := (i - 1) / 2
		if !qless(&w.q[i], &w.q[p]) {
			break
		}
		w.q[i], w.q[p] = w.q[p], w.q[i]
		i = p
	}
}

// pop removes the first commit of the queue.
func (w *walkState) pop() qent {
	top := w.q[0]
	last := len(w.q) - 1
	w.q[0] = w.q[last]
	w.q = w.q[:last]
	i := 0
	for {
		l := 2*i + 1
		if l >= last {
			break
		}
		m := l
		if r := l + 1; r < last && qless(&w.q[r], &w.q[l]) {
			m = r
		}
		if !qless(&w.q[m], &w.q[i]) {
			break
		}
		w.q[i], w.q[m] = w.q[m], w.q[i]
		i = m
	}
	return top
}

// qless orders by date, newest first, then by insertion.
func qless(a, b *qent) bool {
	if a.time != b.time {
		return a.time > b.time
	}
	return a.seq < b.seq
}

// seenSet records the commits already queued: a bitset by graph position and
// a map for the commits outside the graph. A commit is never queued twice,
// so a walk ends even on a graph whose parents form a cycle.
type seenSet struct {
	bits []uint64
	ids  map[ObjectID]struct{}
}

func (s *seenSet) init(g *commitGraph) {
	if g != nil {
		s.bits = make([]uint64, (uint64(g.numCommits())+63)/64)
	}
}

func (s *seenSet) clone() seenSet {
	c := seenSet{bits: slices.Clone(s.bits)}
	if s.ids != nil {
		c.ids = maps.Clone(s.ids)
	}
	return c
}

// add marks n and reports whether it was not marked yet.
func (s *seenSet) add(n node) bool {
	if n.ing {
		w, b := n.pos/64, uint64(1)<<(n.pos%64)
		if int(w) >= len(s.bits) {
			return false
		}
		if s.bits[w]&b != 0 {
			return false
		}
		s.bits[w] |= b
		return true
	}
	if _, ok := s.ids[n.id]; ok {
		return false
	}
	if s.ids == nil {
		s.ids = make(map[ObjectID]struct{})
	}
	s.ids[n.id] = struct{}{}
	return true
}

// graphState is the commit-graph of a Repo as of the last load.
type graphState struct {
	g     *commitGraph // owner's reference; nil when there is none
	stamp graphStamp
	err   error // corruption found when opening, reported by walks
	at    time.Time
}

// acquireGraph returns the current commit-graph with a reference the caller
// must release, nil if there is none. Only a corrupt graph is an error: a
// graph that cannot be opened for another reason (a layer removed by a
// concurrent write, an unsupported version) is as good as absent.
func (r *Repo) acquireGraph() (*commitGraph, error) {
	for {
		s := r.graph.Load()
		if s == nil {
			if err := r.refreshGraph(false); err != nil {
				return nil, err
			}
			if s = r.graph.Load(); s == nil {
				return nil, ErrClosed
			}
		}
		if s.err != nil {
			return nil, s.err
		}
		if s.g == nil {
			return nil, nil
		}
		if s.g.retain() {
			return s.g, nil
		}
		// Replaced and closed between the load and the retain.
	}
}

// refreshGraph reloads the commit-graph when its files changed, or always
// with force. The old graph stays alive for the walks that retained it.
func (r *Repo) refreshGraph(force bool) error {
	r.gmu.Lock()
	defer r.gmu.Unlock()
	cur := r.graph.Load()
	if cur != nil && cur.err == ErrClosed {
		return ErrClosed
	}
	objects := r.dir + "/objects"
	stamp := statGraph(objects)
	if cur != nil && !force && cur.stamp == stamp && time.Since(cur.at) > racyWindow {
		return nil
	}
	st := &graphState{stamp: stamp, at: time.Now()}
	g, err := openCommitGraph(objects)
	switch {
	case err == nil:
		st.g = g
	case errors.Is(err, errGraphCorrupt):
		st.err = fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	// Any other error (no graph, vanished layer, unsupported) means no graph.
	old := r.graph.Swap(st)
	if old != nil && old.g != nil {
		_ = old.g.close()
	}
	return nil
}

func (r *Repo) closeGraph() {
	r.gmu.Lock()
	defer r.gmu.Unlock()
	if old := r.graph.Swap(&graphState{err: ErrClosed}); old != nil && old.g != nil {
		_ = old.g.close()
	}
}
