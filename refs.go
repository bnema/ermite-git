// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	// maxRefFile bounds a loose ref file or HEAD.
	maxRefFile = 4096
	// maxPackedRefs bounds the packed-refs file that is loaded.
	maxPackedRefs = 1 << 30
	// maxSymrefDepth is the number of symbolic refs followed, as in Git.
	maxSymrefDepth = 5
)

// Ref is a reference.
type Ref struct {
	// Name is the full name, such as "refs/heads/main".
	Name string
	// ID is the object the ref designates, after following symbolic refs.
	// It may be an annotated tag; see Repo.PeelRef.
	ID ObjectID
	// Symref is the name of the ref this one points to when it is a symbolic
	// ref, empty otherwise.
	Symref string
	// Peeled is the object that ID designates once annotated tags are
	// followed. It is only set when PeeledKnown, which is the case for refs
	// read from packed-refs that carry peeled values.
	Peeled      ObjectID
	PeeledKnown bool
}

// packedRef is an entry of packed-refs.
type packedRef struct {
	name        string
	id          ObjectID
	peeled      ObjectID
	peeledKnown bool
}

// packedRefs is an immutable parsed packed-refs file, sorted by name.
type packedRefs struct {
	sig  fileSig
	raw  string // the file content: the names in refs point into it
	refs []packedRef
}

func (p *packedRefs) find(name string) (packedRef, bool) {
	i, ok := slices.BinarySearchFunc(p.refs, name, func(e packedRef, n string) int {
		return strings.Compare(e.name, n)
	})
	if !ok {
		return packedRef{}, false
	}
	return p.refs[i], true
}

// loadPacked returns the current content of packed-refs, reparsing the file
// when its stat data changed (or always with force). Concurrent callers share
// one parse: the check is repeated once pmu is held.
func (r *Repo) loadPacked(force bool) (*packedRefs, error) {
	sig, err := r.paths.packedRefs.sig()
	if err != nil {
		return nil, err
	}
	if cur := r.packed.Load(); !force && cur != nil && cur.sig.same(sig) {
		return cur, nil
	}
	r.pmu.Lock()
	defer r.pmu.Unlock()
	cur := r.packed.Load()
	if !force && cur != nil && (cur.sig.read > sig.read || cur.sig.same(sig)) {
		// Another caller parsed the file while we waited for the lock. It read
		// the file after we looked at it, so its result is at least as fresh
		// as ours would be.
		return cur, nil
	}
	read := time.Now().UnixNano()
	path := r.dir + "/packed-refs"
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		p := &packedRefs{sig: fileSig{read: read}}
		r.packed.Store(p)
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > maxPackedRefs {
		return nil, unsupported("packed-refs larger than %d bytes", maxPackedRefs)
	}
	data := make([]byte, st.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, err
	}
	if !force && cur != nil && cur.raw == string(data) {
		// Racy stat data, same content (Git's racy rule): keep the parse
		// and record that the file was read now.
		p := &packedRefs{sig: sigOf(st, read), raw: cur.raw, refs: cur.refs}
		r.packed.Store(p)
		return p, nil
	}
	p, err := parsePackedRefs(string(data))
	if err != nil {
		return nil, err
	}
	if i, _ := slices.BinarySearchFunc(p.refs, "refs/replace/", func(e packedRef, n string) int {
		return strings.Compare(e.name, n)
	}); i < len(p.refs) && strings.HasPrefix(p.refs[i].name, "refs/replace/") {
		return nil, unsupported("replace refs (refs/replace in packed-refs)")
	}
	p.sig = sigOf(st, read)
	r.packed.Store(p)
	return p, nil
}

// parsePackedRefs parses a packed-refs file. The ref names are substrings of
// data.
func parsePackedRefs(data string) (*packedRefs, error) {
	p := &packedRefs{raw: data}
	fullyPeeled, peeled := false, false
	first := true
	for len(data) > 0 {
		var line string
		line, data, _ = strings.Cut(data, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		switch line[0] {
		case '#':
			if first && strings.HasPrefix(line, "# pack-refs with:") {
				for _, tr := range strings.Fields(line[len("# pack-refs with:"):]) {
					switch tr {
					case "fully-peeled":
						fullyPeeled = true
					case "peeled":
						peeled = true
					}
				}
			}
			first = false
			continue
		case '^':
			if len(p.refs) == 0 {
				return nil, corrupt("packed-refs: peeled line without a ref")
			}
			id, ok := parseIDBytes([]byte(line[1:]))
			if !ok {
				return nil, corrupt("packed-refs: bad peeled line")
			}
			last := &p.refs[len(p.refs)-1]
			last.peeled, last.peeledKnown = id, true
			continue
		}
		first = false
		hex, name, ok := strings.Cut(line, " ")
		if !ok || len(hex) != 40 || name == "" {
			return nil, corrupt("packed-refs: bad line %q", truncate(line))
		}
		id, ok := parseIDBytes([]byte(hex))
		if !ok {
			return nil, corrupt("packed-refs: bad id in line %q", truncate(line))
		}
		e := packedRef{name: name, id: id}
		// Without a ^ line, a ref is known not to be a tag when the file
		// is fully peeled (or, for the old "peeled" trait, outside tags).
		if fullyPeeled || (peeled && strings.HasPrefix(name, "refs/tags/")) {
			e.peeled, e.peeledKnown = id, true
		}
		p.refs = append(p.refs, e)
	}
	cmp := func(a, b packedRef) int { return strings.Compare(a.name, b.name) }
	if !slices.IsSortedFunc(p.refs, cmp) {
		slices.SortStableFunc(p.refs, cmp)
	}
	return p, nil
}

// validRefName rejects names that could leave the repository directory or
// that Git itself refuses. It is a subset of git check-ref-format.
func validRefName(name string) bool {
	if !strings.HasPrefix(name, "refs/") {
		return false
	}
	if name == "" || len(name) > 1024 || name[0] == '/' || name[len(name)-1] == '/' || strings.HasSuffix(name, ".lock") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return false
	}
	for _, c := range []byte(name) {
		if c < 0x20 || c == 0x7f || c == '\\' || c == ' ' || c == '~' || c == '^' || c == ':' || c == '?' || c == '*' || c == '[' {
			return false
		}
	}
	for comp := range strings.SplitSeq(name, "/") {
		if comp == "" || comp[0] == '.' || strings.HasSuffix(comp, ".lock") {
			return false
		}
	}
	return true
}

// looseRef is the content of a loose ref file.
type looseRef struct {
	id     ObjectID
	symref string
}

// readLooseRef reads a loose ref. found is false when there is no such file
// (or it is a directory).
func (r *Repo) readLooseRef(name string) (lr looseRef, found bool, err error) {
	// The path goes through a stack buffer and the file is opened with a
	// plain system call: a ref that does not exist, which is what most
	// candidates of a revision lookup are, allocates nothing.
	var arr [256]byte
	nul := append(arr[:0], r.dir...)
	nul = append(nul, '/')
	nul = append(nul, name...)
	nul = append(nul, 0)
	file, err := openNul(nul)
	if err != nil {
		if isNotExistErrno(err) {
			return lr, false, nil
		}
		return lr, false, openError(nul, err)
	}
	defer file.close()
	pb := getBuf()
	defer putBuf(pb)
	pb.b = grow(pb.b, maxRefFile+1)
	buf := pb.b
	n, dir, err := file.readFull(buf)
	if err != nil {
		return lr, false, &fs.PathError{Op: "read", Path: r.dir + "/" + name, Err: err}
	}
	if dir {
		return lr, false, nil
	}
	if n > maxRefFile {
		return lr, true, corrupt("ref %s is larger than %d bytes", name, maxRefFile)
	}
	lr, err = parseRefContent(buf[:n])
	if err != nil {
		return lr, true, fmt.Errorf("ref %s: %w", name, err)
	}
	return lr, true, nil
}

func parseRefContent(b []byte) (looseRef, error) {
	// On bytes: an id ref, the common case, allocates nothing.
	b = bytes.TrimRight(b, " \t\r\n")
	if target, ok := bytes.CutPrefix(b, []byte("ref:")); ok {
		t := string(bytes.TrimSpace(target))
		if !validRef(t) {
			return looseRef{}, corrupt("bad symbolic ref target %q", truncate(t))
		}
		return looseRef{symref: t}, nil
	}
	if len(b) >= 40 {
		if id, ok := parseIDBytes(b[:40]); ok && (len(b) == 40 || b[40] == ' ' || b[40] == '\t') {
			return looseRef{id: id}, nil
		}
	}
	return looseRef{}, corrupt("bad ref content %q", truncate(string(b)))
}

// validRootRef reports whether name is a pseudo-ref of the Git directory such
// as HEAD or ORIG_HEAD: upper case letters and underscores only.
func validRootRef(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, c := range []byte(name) {
		if (c < 'A' || c > 'Z') && c != '_' {
			return false
		}
	}
	return true
}

// validRef accepts the names lookupRef may read.
func validRef(name string) bool { return validRefName(name) || validRootRef(name) }

// refWalk is a ref lookup that went through the loose files and may still
// need packed-refs.
type refWalk struct {
	ref  Ref
	cur  string // the name to look up in packed-refs when !done
	done bool   // the lookup is complete: ok tells whether it found a ref
	ok   bool
}

// isRootRef reports whether name is a pseudo-ref of the Git directory
// (HEAD, ORIG_HEAD...) rather than a name under refs/.
func isRootRef(name string) bool { return !strings.HasPrefix(name, "refs/") }

// lookupLoose follows name through the loose files and symbolic refs. It
// stops at a loose ref that holds an id, or at the first name that has no
// loose file: packed-refs decides about that one (see finish).
func (r *Repo) lookupLoose(name string) (refWalk, error) {
	if !validRef(name) {
		return refWalk{done: true}, nil
	}
	w := refWalk{ref: Ref{Name: name}, cur: name}
	for depth := 0; ; depth++ {
		if depth > maxSymrefDepth {
			return refWalk{}, corrupt("ref %s: symbolic ref loop", name)
		}
		lr, found, err := r.readLooseRef(w.cur)
		if err != nil {
			if errors.Is(err, ErrCorrupt) && isRootRef(w.cur) && w.cur != "HEAD" {
				// A pseudo-ref such as FETCH_HEAD or MERGE_HEAD can hold
				// anything (FETCH_HEAD has one line per fetched ref), and
				// a revision like "ABCDEF" may name such a file by
				// accident: it is not a ref, so not found. A broken HEAD
				// stays an error.
				return refWalk{done: true}, nil
			}
			return refWalk{}, err
		}
		if found && lr.symref != "" {
			if depth == 0 {
				w.ref.Symref = lr.symref
			}
			if !validRef(lr.symref) {
				return refWalk{done: true}, nil
			}
			w.cur = lr.symref
			continue
		}
		if found {
			w.ref.ID = lr.id
			w.done, w.ok = true, true
			return w, nil
		}
		return w, nil
	}
}

// finish completes a lookup with the content of packed-refs. The loose files
// have to be read before packed-refs is loaded: a concurrent pack-refs writes
// packed-refs first and deletes the loose file after, so it is never missed.
func (w *refWalk) finish(p *packedRefs) (Ref, bool) {
	if w.done {
		return w.ref, w.ok
	}
	e, ok := p.find(w.cur)
	if !ok {
		return Ref{}, false
	}
	w.ref.ID = e.id
	if w.cur == w.ref.Name {
		w.ref.Peeled, w.ref.PeeledKnown = e.peeled, e.peeledKnown
	}
	return w.ref, true
}

// lookupRef resolves name through symbolic refs. loose refs win over packed
// ones.
func (r *Repo) lookupRef(name string) (Ref, bool, error) {
	if r.snap.Load() == nil {
		return Ref{}, false, ErrClosed
	}
	w, err := r.lookupLoose(name)
	if err != nil {
		return Ref{}, false, err
	}
	if w.done {
		ref, ok := w.finish(nil)
		return ref, ok, nil
	}
	p, err := r.loadPacked(false)
	if err != nil {
		return Ref{}, false, err
	}
	ref, ok := w.finish(p)
	return ref, ok, nil
}

// ResolveRef returns the object the ref with this full name (such as
// "refs/heads/main", "refs/tags/v1" or "HEAD") designates, following
// symbolic refs. The result may be an annotated tag; see PeelToCommit. It
// returns ErrNotFound if the ref does not exist.
func (r *Repo) ResolveRef(name string) (ObjectID, error) {
	ref, ok, err := r.lookupRef(name)
	if err != nil {
		return ObjectID{}, err
	}
	if !ok {
		return ObjectID{}, notFound("ref %s", name)
	}
	return ref.ID, nil
}

// LookupRefID is like ResolveRef but a ref that does not exist is not an
// error: ok is false, and nothing is allocated, which makes it the call for
// trying several candidate names. err is for what went wrong while looking
// (a closed Repo, an unreadable or corrupt ref).
func (r *Repo) LookupRefID(name string) (id ObjectID, ok bool, err error) {
	ref, ok, err := r.lookupRef(name)
	if err != nil || !ok {
		return ObjectID{}, false, err
	}
	return ref.ID, true, nil
}

// LookupRef is like ResolveRef but returns the whole Ref.
func (r *Repo) LookupRef(name string) (Ref, error) {
	ref, ok, err := r.lookupRef(name)
	if err != nil {
		return Ref{}, err
	}
	if !ok {
		return Ref{}, notFound("ref %s", name)
	}
	return ref, nil
}

// Head returns the full name of the ref HEAD points to ("refs/heads/main")
// and the object it designates. An unborn branch (the target does not exist
// yet) is not an error: id is then the zero ObjectID. For a detached HEAD
// target is empty and id is the commit.
func (r *Repo) Head() (target string, id ObjectID, err error) {
	if r.snap.Load() == nil {
		return "", ObjectID{}, ErrClosed
	}
	lr, found, err := r.readLooseRef("HEAD")
	if err != nil {
		return "", ObjectID{}, err
	}
	if !found {
		return "", ObjectID{}, notFound("HEAD")
	}
	if lr.symref == "" {
		return "", lr.id, nil
	}
	ref, ok, err := r.lookupRef(lr.symref)
	if err != nil {
		return "", ObjectID{}, err
	}
	if !ok {
		return lr.symref, ObjectID{}, nil
	}
	return lr.symref, ref.ID, nil
}

// Refs iterates over the refs whose full name starts with prefix, such as
// "refs/heads/" or "refs/tags/" ("" for all of them), sorted by full name.
// Loose refs override packed ones; the peeled lines of packed-refs are used.
// Symbolic refs are resolved; a ref that cannot be read is skipped. HEAD is
// not part of it, see Head. An error ends the iteration.
func (r *Repo) Refs(prefix string) iter.Seq2[Ref, error] {
	return func(yield func(Ref, error) bool) {
		refs, err := r.collectRefs(prefix)
		if err != nil {
			yield(Ref{}, err)
			return
		}
		for _, ref := range refs {
			if !yield(ref, nil) {
				return
			}
		}
	}
}

func (r *Repo) collectRefs(prefix string) ([]Ref, error) {
	if r.snap.Load() == nil {
		return nil, ErrClosed
	}
	var dir string
	switch {
	case strings.HasPrefix(prefix, "refs/"):
		dir = prefix[:strings.LastIndexByte(prefix, '/')]
	case strings.HasPrefix("refs/", prefix):
		dir = "refs"
	default:
		return nil, nil
	}
	// Loose refs first, packed-refs after: see refWalk.finish.
	var loose []string
	if err := r.walkLoose(dir, prefix, &loose, 0); err != nil {
		return nil, err
	}
	slices.Sort(loose)
	loose = slices.Compact(loose)
	type item struct {
		name string
		w    refWalk
	}
	items := make([]item, 0, len(loose))
	for _, n := range loose {
		w, err := r.lookupLoose(n)
		if err != nil {
			if errors.Is(err, ErrCorrupt) {
				continue // Git ignores broken refs when listing
			}
			return nil, err
		}
		items = append(items, item{name: n, w: w})
	}
	// One load of packed-refs serves every name.
	p, err := r.loadPacked(false)
	if err != nil {
		return nil, err
	}
	start, _ := slices.BinarySearchFunc(p.refs, prefix, func(e packedRef, pre string) int {
		if strings.HasPrefix(e.name, pre) {
			return 0
		}
		return strings.Compare(e.name, pre)
	})
	// BinarySearchFunc with a prefix comparator may land inside the range:
	// walk back to its first element.
	for start > 0 && strings.HasPrefix(p.refs[start-1].name, prefix) {
		start--
	}
	nLoose := len(items)
	for i := start; i < len(p.refs) && strings.HasPrefix(p.refs[i].name, prefix); i++ {
		n := p.refs[i].name
		if _, ok := slices.BinarySearch(loose, n); ok {
			continue // the loose file wins; its walk is already in items
		}
		if !validRef(n) {
			continue
		}
		items = append(items, item{name: n, w: refWalk{ref: Ref{Name: n}, cur: n}})
	}
	if len(items) > nLoose {
		slices.SortFunc(items, func(a, b item) int { return strings.Compare(a.name, b.name) })
	}
	out := make([]Ref, 0, len(items))
	for i := range items {
		it := &items[i]
		if ref, ok := it.w.finish(p); ok {
			ref.Name = it.name
			out = append(out, ref)
		}
	}
	return out, nil
}

// maxRefDirDepth bounds the recursion into refs directories.
const maxRefDirDepth = 64

func (r *Repo) walkLoose(dir, prefix string, out *[]string, depth int) error {
	if depth > maxRefDirDepth {
		return nil
	}
	entries, err := os.ReadDir(r.dir + "/" + dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		name := dir + "/" + e.Name()
		if e.IsDir() {
			// Descend only where the prefix can match.
			if strings.HasPrefix(name+"/", prefix) || strings.HasPrefix(prefix, name+"/") {
				if err := r.walkLoose(name, prefix, out, depth+1); err != nil {
					return err
				}
			}
			continue
		}
		if strings.HasPrefix(name, prefix) && validRefName(name) {
			*out = append(*out, name)
		}
	}
	return nil
}

// PeelRef returns the commit a ref designates, following annotated tags. It
// uses the peeled value of packed-refs when the ref carries one. It returns
// ErrWrongType if the ref designates something else than a commit.
func (r *Repo) PeelRef(ref Ref) (ObjectID, error) {
	if ref.PeeledKnown {
		typ, _, err := r.ObjectHeader(ref.Peeled)
		if err != nil {
			return ObjectID{}, err
		}
		if typ != TypeCommit {
			return ObjectID{}, wrongType(ref.Peeled, typ, TypeCommit)
		}
		return ref.Peeled, nil
	}
	return r.PeelToCommit(ref.ID)
}

// Resolve turns a revision name into an object id the way "git rev-parse"
// does for plain names. The result is not peeled; use PeelToCommit. Revision
// operators (~, ^, @{}, :path) are not supported.
//
// The name is tried in git's order, and the first match wins:
//
//  1. a full 40 digit id. It is taken as an id even when a ref has that name
//     (git warns that the ref is ambiguous and ignores it), and it must exist
//     in the repository, which "git rev-parse" does not check;
//  2. a ref, as "<n>", "refs/<n>", "refs/tags/<n>", "refs/heads/<n>",
//     "refs/remotes/<n>" and "refs/remotes/<n>/HEAD", in that order. A tag
//     therefore wins over a branch of the same name, whereas "git checkout"
//     prefers the branch;
//  3. an abbreviated id of 4 to 39 digits, which loses against any ref of
//     that name. It matches any object, whatever its type, and fails with
//     ErrAmbiguous if several do; see ResolveCommitPrefix.
//
// A caller that wants a branch or a tag, or a commit, should not use Resolve:
// ResolveRef takes the full ref name and ResolveCommitPrefix only considers
// commits. Resolve returns an error wrapping ErrNotFound if nothing matches.
func (r *Repo) Resolve(rev string) (ObjectID, error) {
	if rev == "" {
		return ObjectID{}, notFound("empty revision")
	}
	if len(rev) == 40 {
		if id, err := ParseID(rev); err == nil {
			ok, err := r.HasObject(id)
			if err != nil {
				return ObjectID{}, err
			}
			if !ok {
				return ObjectID{}, notFound("object %s", id)
			}
			return id, nil
		}
	}
	for _, pat := range [...]string{"%s", "refs/%s", "refs/tags/%s", "refs/heads/%s", "refs/remotes/%s", "refs/remotes/%s/HEAD"} {
		ref, ok, err := r.lookupRef(fmt.Sprintf(pat, rev))
		if err != nil {
			return ObjectID{}, err
		}
		if ok {
			return ref.ID, nil
		}
	}
	if len(rev) >= minPrefix && len(rev) < 40 {
		id, err := r.ResolvePrefix(rev)
		if err == nil || !errors.Is(err, ErrInvalidID) {
			return id, err
		}
	}
	return ObjectID{}, notFound("revision %q", truncate(rev))
}
