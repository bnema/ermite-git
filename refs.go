// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"slices"
	"strings"
	"syscall"
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
// when its stat data changed (or always with force).
func (r *Repo) loadPacked(force bool) (*packedRefs, error) {
	path := r.dir + "/packed-refs"
	sig, err := statSig(path)
	if err != nil {
		return nil, err
	}
	if cur := r.packed.Load(); !force && cur != nil && cur.sig.same(sig) {
		return cur, nil
	}
	r.pmu.Lock()
	defer r.pmu.Unlock()
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		p := &packedRefs{}
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
	p, err := parsePackedRefs(string(data))
	if err != nil {
		return nil, err
	}
	if i, _ := slices.BinarySearchFunc(p.refs, "refs/replace/", func(e packedRef, n string) int {
		return strings.Compare(e.name, n)
	}); i < len(p.refs) && strings.HasPrefix(p.refs[i].name, "refs/replace/") {
		return nil, unsupported("replace refs (refs/replace in packed-refs)")
	}
	p.sig = sigOf(st)
	r.packed.Store(p)
	return p, nil
}

// parsePackedRefs parses a packed-refs file. The ref names are substrings of
// data.
func parsePackedRefs(data string) (*packedRefs, error) {
	p := &packedRefs{}
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
	f, err := os.Open(r.dir + "/" + name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return lr, false, nil
		}
		return lr, false, err
	}
	defer func() { _ = f.Close() }()
	pb := getBuf()
	defer putBuf(pb)
	pb.b = grow(pb.b, maxRefFile+1)
	buf := pb.b
	n, err := io.ReadFull(f, buf)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) && errors.Is(pe.Err, syscall.EISDIR) {
			return lr, false, nil
		}
		return lr, false, err
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
	s := strings.TrimRight(string(b), " \t\r\n")
	if target, ok := strings.CutPrefix(s, "ref:"); ok {
		target = strings.TrimSpace(target)
		if !validRef(target) {
			return looseRef{}, corrupt("bad symbolic ref target %q", truncate(target))
		}
		return looseRef{symref: target}, nil
	}
	if len(s) >= 40 {
		if id, ok := parseIDBytes([]byte(s[:40])); ok && (len(s) == 40 || s[40] == ' ' || s[40] == '\t') {
			return looseRef{id: id}, nil
		}
	}
	return looseRef{}, corrupt("bad ref content %q", truncate(s))
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

// lookupRef resolves name through symbolic refs. loose refs win over packed
// ones. The loose file is read before packed-refs, so a concurrent pack-refs
// (which writes packed-refs first and deletes the loose file after) is never
// missed.
func (r *Repo) lookupRef(name string) (Ref, bool, error) {
	if !validRef(name) {
		return Ref{}, false, nil
	}
	ref := Ref{Name: name}
	cur := name
	for depth := 0; ; depth++ {
		if depth > maxSymrefDepth {
			return Ref{}, false, corrupt("ref %s: symbolic ref loop", name)
		}
		lr, found, err := r.readLooseRef(cur)
		if err != nil {
			return Ref{}, false, err
		}
		if found && lr.symref != "" {
			if depth == 0 {
				ref.Symref = lr.symref
			}
			if !validRef(lr.symref) {
				return Ref{}, false, nil
			}
			cur = lr.symref
			continue
		}
		if found {
			ref.ID = lr.id
			return ref, true, nil
		}
		p, err := r.loadPacked(false)
		if err != nil {
			return Ref{}, false, err
		}
		e, ok := p.find(cur)
		if !ok {
			return Ref{}, false, nil
		}
		ref.ID = e.id
		if depth == 0 {
			ref.Peeled, ref.PeeledKnown = e.peeled, e.peeledKnown
		}
		return ref, true, nil
	}
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
	// Loose refs first, packed-refs after: see lookupRef.
	var dir string
	switch {
	case strings.HasPrefix(prefix, "refs/"):
		dir = prefix[:strings.LastIndexByte(prefix, '/')]
	case strings.HasPrefix("refs/", prefix):
		dir = "refs"
	default:
		return nil, nil
	}
	var loose []string
	if err := r.walkLoose(dir, prefix, &loose, 0); err != nil {
		return nil, err
	}
	p, err := r.loadPacked(false)
	if err != nil {
		return nil, err
	}
	names := loose
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
	for i := start; i < len(p.refs) && strings.HasPrefix(p.refs[i].name, prefix); i++ {
		names = append(names, p.refs[i].name)
	}
	slices.Sort(names)
	names = slices.Compact(names)

	out := make([]Ref, 0, len(names))
	for _, n := range names {
		ref, ok, err := r.lookupRef(n)
		if err != nil {
			if errors.Is(err, ErrCorrupt) {
				continue // Git ignores broken refs when listing
			}
			return nil, err
		}
		if ok {
			ref.Name = n
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
// does for plain names: a full 40 digit id, "HEAD", a full ref name, a short
// ref name tried as "<n>", "refs/<n>", "refs/tags/<n>", "refs/heads/<n>",
// "refs/remotes/<n>" and "refs/remotes/<n>/HEAD" in that order, and finally
// an abbreviated id of 4 to 39 digits. Revision operators (~, ^, @{}) are not
// supported. The result is not peeled; use PeelToCommit.
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
