// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// This file holds the tree comparison used by history walks with a path: a
// cheap TREESAME test for one path, done by comparing the entry ids
// component by component. Only the trees on the way to the path are read,
// and only while the ids differ, so a commit that did not touch the path
// usually costs the two root trees at most, and nothing when the root trees
// are identical. Lookups are memoised per (tree, depth), which makes a walk
// along a chain read each tree once instead of twice.

// ErrInvalidPath reports a path that cannot designate anything in a
// repository: an absolute path or one that leaves the root with "..".
var ErrInvalidPath = errors.New("git: invalid path")

// emptyTreeID is the id of the empty tree, which Git knows without storing it.
var emptyTreeID = ObjectID{
	0x4b, 0x82, 0x5d, 0xc6, 0x42, 0xcb, 0x6e, 0xb9, 0xa0, 0x60,
	0xe5, 0x4b, 0xf8, 0xd6, 0x92, 0x88, 0xfb, 0xee, 0x49, 0x04,
}

// pathSpec is a literal path, normalised as Git does for a pathspec.
type pathSpec struct {
	comps []string // no empty, "." or ".." component; none means the root
	// bloom reports that the path is exactly what Git inserted in the
	// filters: spelled without a "." or ".." component, an empty one, or
	// more than one trailing slash. Otherwise the trees are compared.
	bloom bool
	// dirOnly is set by a trailing slash: the last component then matches
	// a directory or a submodule, never a file ("f/" finds nothing when f
	// is a file).
	dirOnly bool
}

// parsePathSpec normalises p: empty and "." components are dropped, ".."
// removes the previous one, and one trailing slash is ignored. Absolute
// paths and paths that climb above the root are ErrInvalidPath.
func parsePathSpec(p string) (pathSpec, error) {
	if strings.IndexByte(p, 0) >= 0 {
		return pathSpec{}, fmt.Errorf("%w: NUL byte", ErrInvalidPath)
	}
	if strings.HasPrefix(p, "/") {
		return pathSpec{}, fmt.Errorf("%w: %q is absolute", ErrInvalidPath, p)
	}
	s := pathSpec{bloom: true, dirOnly: strings.HasSuffix(p, "/")}
	t := strings.TrimSuffix(p, "/")
	if t == "" {
		s.bloom = false // the root: every commit with a changed tree
		return s, nil
	}
	for _, c := range strings.Split(t, "/") {
		switch c {
		case "", ".":
			s.bloom = false
		case "..":
			s.bloom = false
			if len(s.comps) == 0 {
				return pathSpec{}, fmt.Errorf("%w: %q is outside the repository", ErrInvalidPath, p)
			}
			s.comps = s.comps[:len(s.comps)-1]
		default:
			s.comps = append(s.comps, c)
		}
	}
	if len(s.comps) == 0 {
		s.bloom = false
	}
	return s, nil
}

// String returns the normalised path ("" for the root).
func (s pathSpec) String() string { return strings.Join(s.comps, "/") }

// pathEntry is what a tree holds at a path.
type pathEntry struct {
	id    ObjectID
	mode  Mode
	found bool
}

type lookupKey struct {
	tree  ObjectID
	level uint16
}

const maxLookupCache = 1 << 14

// pathDiffer answers TREESAME questions for one path. It is not safe for
// concurrent use.
type pathDiffer struct {
	r       *Repo
	comps   []string
	dirOnly bool
	names   [][]byte
	cache   map[lookupKey]pathEntry
	buf     []byte
}

func newPathDiffer(r *Repo, spec pathSpec) *pathDiffer {
	comps := spec.comps
	d := &pathDiffer{r: r, comps: comps, dirOnly: spec.dirOnly, cache: make(map[lookupKey]pathEntry, 256)}
	d.names = make([][]byte, len(comps))
	for i, c := range comps {
		d.names[i] = []byte(c)
	}
	return d
}

// lookup finds the entry named comps[level] in tree.
func (d *pathDiffer) lookup(tree ObjectID, level int) (pathEntry, error) {
	if tree == emptyTreeID {
		return pathEntry{}, nil
	}
	k := lookupKey{tree, uint16(level)}
	if e, ok := d.cache[k]; ok {
		return e, nil
	}
	typ, data, err := d.r.ReadObject(tree, d.buf)
	if err != nil {
		return pathEntry{}, err
	}
	d.buf = data
	if typ != TypeTree {
		return pathEntry{}, wrongType(tree, typ, TypeTree)
	}
	e, err := findEntry(data, d.names[level])
	if err != nil {
		return pathEntry{}, wrapObj(tree, err)
	}
	if len(d.cache) >= maxLookupCache {
		clear(d.cache)
	}
	d.cache[k] = e
	return e, nil
}

// findEntry scans a tree for name. It validates the entries it walks over
// like ParseTree and does not allocate.
func findEntry(data, name []byte) (pathEntry, error) {
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp <= 0 || sp > 7 {
			return pathEntry{}, corrupt("tree: bad mode")
		}
		var mode uint32
		for _, c := range data[:sp] {
			if c < '0' || c > '7' {
				return pathEntry{}, corrupt("tree: bad mode")
			}
			mode = mode<<3 | uint32(c-'0')
		}
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul <= 0 || len(data)-nul-1 < 20 {
			return pathEntry{}, corrupt("tree: truncated entry")
		}
		if bytes.Equal(data[:nul], name) {
			e := pathEntry{mode: Mode(mode), found: true}
			copy(e.id[:], data[nul+1:nul+21])
			return e, nil
		}
		data = data[nul+21:]
	}
	return pathEntry{}, nil
}

// final applies the trailing-slash rule to the entry found at the full path.
func (d *pathDiffer) final(e pathEntry) pathEntry {
	if d.dirOnly && e.found && !e.mode.IsTree() && !e.mode.IsGitlink() {
		return pathEntry{}
	}
	return e
}

// resolve returns the entry at the path below the root tree. The root path
// (no component) resolves to the tree itself.
func (d *pathDiffer) resolve(root ObjectID) (pathEntry, error) {
	cur := pathEntry{id: root, mode: ModeTree, found: true}
	for i := range d.comps {
		if !cur.mode.IsTree() {
			return pathEntry{}, nil
		}
		e, err := d.lookup(cur.id, i)
		if err != nil || !e.found {
			return pathEntry{}, err
		}
		cur = e
	}
	return d.final(cur), nil
}

// child returns what the directory entry e holds at comps[level]: absent when
// e is absent or not a directory.
func (d *pathDiffer) child(e pathEntry, level int) (pathEntry, error) {
	if !e.found || !e.mode.IsTree() {
		return pathEntry{}, nil
	}
	return d.lookup(e.id, level)
}

// same reports whether the path is identical in the root trees a and b: both
// absent, or the same id and mode. The comparison stops at the first level
// where the entries are equal.
func (d *pathDiffer) same(a, b ObjectID) (bool, error) {
	ea := pathEntry{id: a, mode: ModeTree, found: true}
	eb := pathEntry{id: b, mode: ModeTree, found: true}
	for i := 0; ; i++ {
		if i == len(d.comps) {
			return d.final(ea) == d.final(eb), nil
		}
		if ea == eb {
			return true, nil
		}
		var err error
		if ea, err = d.child(ea, i); err != nil {
			return false, err
		}
		if eb, err = d.child(eb, i); err != nil {
			return false, err
		}
	}
}

// exists reports whether the path exists in the root tree, that is whether a
// root commit changes it.
func (d *pathDiffer) exists(root ObjectID) (bool, error) {
	if len(d.comps) == 0 {
		return root != emptyTreeID, nil
	}
	e, err := d.resolve(root)
	return e.found, err
}
