// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"fmt"
	"strings"
)

// finder looks an object up in a snapshot of the packs and, when it is
// missing, once more after reloading the pack list: a repack or a push may
// have added the pack that holds it.
type finder struct {
	r       *Repo
	s       *snapshot
	retried bool
}

func (r *Repo) finder() (finder, error) {
	s, err := r.acquire()
	return finder{r: r, s: s}, err
}

func (f *finder) close() { f.s.release() }

func (f *finder) pack(id *ObjectID) (*pack, uint64, error) { return f.s.find(id) }

// reloadPacksHook, when set by a test, replaces the reload done by again.
var reloadPacksHook func(r *Repo) (bool, error)

// again reloads the pack list if it changed and reports whether the lookup
// is worth repeating, that is whether the current snapshot differs from the
// one this finder used. Another goroutine may already have installed the new
// list, so the answer of reloadPacks alone is not enough. It returns true at
// most once. An error from the reload is returned: the object may well exist
// in a pack that could not be listed, so "not found" would be a wrong answer.
func (f *finder) again() (bool, error) {
	if f.retried {
		return false, nil
	}
	f.retried = true
	reload := f.r.reloadPacks
	if hook := reloadPacksHook; hook != nil {
		reload = func(bool) (bool, error) { return hook(f.r) }
	}
	if _, err := reload(false); err != nil {
		return false, err
	}
	s, err := f.r.acquire()
	if err != nil {
		return false, err
	}
	if s == f.s {
		s.release()
		return false, nil
	}
	f.s.release()
	f.s = s
	return true, nil
}

// HasObject reports whether the object exists. The error is not nil when the
// answer is unknown: the repository is closed, or the pack list could not be
// reloaded while the object was missing from the current one (during a gc,
// for example).
func (r *Repo) HasObject(id ObjectID) (bool, error) {
	f, err := r.finder()
	if err != nil {
		return false, err
	}
	defer f.close()
	for {
		if p, _, err := f.pack(&id); err != nil || p != nil {
			return err == nil, err
		}
		if exists(r.loosePath(&id)) {
			return true, nil
		}
		again, err := f.again()
		if err != nil {
			return false, err
		}
		if !again {
			return false, nil
		}
	}
}

// ObjectHeader returns the type and the size of an object without reading its
// content. For a deltified object the size is the result size announced by
// the delta header. It returns ErrNotFound if the object does not exist.
func (r *Repo) ObjectHeader(id ObjectID) (Type, int64, error) {
	f, err := r.finder()
	if err != nil {
		return 0, 0, err
	}
	defer f.close()
	for {
		p, off, err := f.pack(&id)
		if err != nil {
			return 0, 0, err
		}
		if p != nil {
			typ, size, err := p.header(off)
			if err != nil {
				return 0, 0, wrapPack(p.name, err)
			}
			return typ, size, nil
		}
		typ, size, err := r.looseHeader(&id)
		if err == nil {
			return typ, size, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return 0, 0, err
		}
		if again, err := f.again(); err != nil {
			return 0, 0, err
		} else if !again {
			return 0, 0, notFound("object %s", id.String())
		}
	}
}

// ReadObject reads the whole content of an object. The content is appended
// to dst[:0], which is grown if needed; pass the previous result to reuse its
// storage. Objects over the WithMaxObjectSize bound fail with ErrTooLarge.
func (r *Repo) ReadObject(id ObjectID, dst []byte) (Type, []byte, error) {
	f, err := r.finder()
	if err != nil {
		return 0, nil, err
	}
	defer f.close()
	for {
		p, off, err := f.pack(&id)
		if err != nil {
			return 0, nil, err
		}
		if p != nil {
			typ, data, err := r.readPacked(p, off, dst)
			if err != nil {
				return 0, nil, wrapPack(p.name, err)
			}
			return typ, data, nil
		}
		typ, data, err := r.readLoose(&id, dst)
		if err == nil {
			return typ, data, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return 0, nil, err
		}
		if again, err := f.again(); err != nil {
			return 0, nil, err
		} else if !again {
			return 0, nil, notFound("object %s", id.String())
		}
	}
}

// readTyped reads an object that must have type want into a pooled buffer.
// The caller returns the buffer with putBuf.
func (r *Repo) readTyped(id ObjectID, want Type) (*buffer, error) {
	b := getBuf()
	typ, data, err := r.ReadObject(id, b.b)
	if err != nil {
		putBuf(b)
		return nil, err
	}
	b.b = data
	if typ != want {
		putBuf(b)
		return nil, wrongType(id, typ, want)
	}
	return b, nil
}

// Peel follows annotated tags from id until it reaches an object that is not
// a tag, and returns that object and its type. Chains of tags longer than 64
// are corrupt.
func (r *Repo) Peel(id ObjectID) (ObjectID, Type, error) {
	for range maxPeel {
		typ, _, err := r.ObjectHeader(id)
		if err != nil {
			return ObjectID{}, 0, err
		}
		if typ != TypeTag {
			return id, typ, nil
		}
		b, err := r.readTyped(id, TypeTag)
		if err != nil {
			return ObjectID{}, 0, err
		}
		var t Tag
		err = ParseTag(b.b, &t)
		next := t.Object
		putBuf(b)
		if err != nil {
			return ObjectID{}, 0, fmt.Errorf("tag %s: %w", id, err)
		}
		id = next
	}
	return ObjectID{}, 0, corrupt("more than %d nested tags", maxPeel)
}

const maxPeel = 64

// PeelToCommit follows annotated tags from id and returns the commit they
// designate. It returns ErrWrongType if the result is not a commit (a tag of
// a tree, for example).
func (r *Repo) PeelToCommit(id ObjectID) (ObjectID, error) {
	got, typ, err := r.Peel(id)
	if err != nil {
		return ObjectID{}, err
	}
	if typ != TypeCommit {
		return ObjectID{}, fmt.Errorf("%w: %s is a %s, not a commit", ErrWrongType, got, typ)
	}
	return got, nil
}

// minPrefix is the shortest abbreviation accepted, as in Git.
const minPrefix = 4

// ResolvePrefix returns the object whose id starts with the hexadecimal
// digits in hex (4 to 40, either case). It returns ErrNotFound when none
// matches and ErrAmbiguous when several do, whatever their type; see
// ResolveCommitPrefix. A full id must exist.
func (r *Repo) ResolvePrefix(hex string) (ObjectID, error) {
	return r.resolvePrefix(hex, nil)
}

// ResolveCommitPrefix is like ResolvePrefix but only considers the objects
// that designate a commit: commits and tags that peel to a commit. It is what
// "git rev-parse <hex>^{commit}" does.
func (r *Repo) ResolveCommitPrefix(hex string) (ObjectID, error) {
	return r.resolvePrefix(hex, func(id ObjectID) (bool, error) {
		_, err := r.PeelToCommit(id)
		if errors.Is(err, ErrWrongType) {
			return false, nil
		}
		return err == nil, err
	})
}

func (r *Repo) resolvePrefix(hex string, accept func(ObjectID) (bool, error)) (ObjectID, error) {
	n := len(hex)
	if n < minPrefix || n > 40 {
		return ObjectID{}, fmt.Errorf("%w: %q must have %d to 40 hexadecimal digits", ErrInvalidID, truncate(hex), minPrefix)
	}
	var key [20]byte
	for i := 0; i < n; i++ {
		v := oidHexVal[hex[i]]
		if v > 0x0f {
			return ObjectID{}, fmt.Errorf("%w: %q is not hexadecimal", ErrInvalidID, truncate(hex))
		}
		if i%2 == 0 {
			key[i/2] = v << 4
		} else {
			key[i/2] |= v
		}
	}
	hex = strings.ToLower(hex)

	f, err := r.finder()
	if err != nil {
		return ObjectID{}, err
	}
	defer f.close()
	for {
		var found [2]ObjectID
		count := 0
		var acceptErr error
		add := func(id ObjectID) bool {
			if count > 0 && found[0] == id {
				return true
			}
			if count > 1 && found[1] == id {
				return true
			}
			if accept != nil {
				ok, err := accept(id)
				if err != nil {
					acceptErr = err
					return false
				}
				if !ok {
					return true
				}
			}
			found[count] = id
			count++
			return count < 2
		}
		for _, p := range f.s.packs {
			p.idx.prefixMatches(key[:], n, add)
			if count >= 2 {
				break
			}
		}
		if count < 2 && acceptErr == nil {
			if err := r.looseWithPrefix(hex, add); err != nil {
				return ObjectID{}, err
			}
		}
		if acceptErr != nil {
			return ObjectID{}, acceptErr
		}
		switch {
		case count >= 2:
			return ObjectID{}, fmt.Errorf("%w: %s matches several objects", ErrAmbiguous, hex)
		case count == 1:
			return found[0], nil
		}
		if again, err := f.again(); err != nil {
			return ObjectID{}, err
		} else if !again {
			return ObjectID{}, notFound("no object starts with %s", hex)
		}
	}
}
