// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// maxLooseHeader bounds "<type> <size>\x00" of a loose object.
const maxLooseHeader = 32

func (r *Repo) loosePath(id *ObjectID) string {
	var hex [40]byte
	id.AppendHex(hex[:0])
	var arr [256]byte
	b := append(arr[:0], r.dir...)
	b = append(b, "/objects/"...)
	b = append(b, hex[:2]...)
	b = append(b, '/')
	b = append(b, hex[2:]...)
	return string(b)
}

// looseObject is an opened loose object whose header was read.
type looseObject struct {
	f    *os.File
	fi   *fileInflater
	typ  Type
	size uint64
	lead [maxLooseHeader]byte // content bytes read along with the header
	nlea int
	disk int64 // size of the file
}

// openLoose opens a loose object and reads its header. It returns
// ErrNotFound when the object is not loose.
func (r *Repo) openLoose(id *ObjectID) (*looseObject, error) {
	f, err := os.Open(r.loosePath(id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	l := &looseObject{f: f}
	if l.fi, err = getFile(f); err != nil {
		_ = f.Close()
		return nil, wrapLoose(id, err)
	}
	if err := l.readHeader(); err != nil {
		l.close()
		return nil, wrapLoose(id, err)
	}
	// A small file cannot hold a big object (deflate expands at most 1032
	// times): check it before allocating for the content. The stat is only
	// worth its cost for big objects.
	if l.size > statThreshold {
		st, err := f.Stat()
		if err != nil {
			l.close()
			return nil, err
		}
		l.disk = st.Size()
		if l.size/1032 > uint64(l.disk) {
			l.close()
			return nil, corrupt("loose object %s: %d bytes in a file of %d", id.String(), l.size, l.disk)
		}
	}
	return l, nil
}

// statThreshold is the object size over which the size of the loose file is
// checked against the size the header announces.
const statThreshold = 1 << 16

func wrapLoose(id *ObjectID, err error) error {
	if errors.Is(err, ErrCorrupt) {
		return corrupt("loose object %s: %v", id.String(), err)
	}
	return err
}

func (l *looseObject) close() {
	if l.fi != nil {
		putFile(l.fi)
		l.fi = nil
	}
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

func (l *looseObject) readHeader() error {
	tmp := l.fi.scratch[:]
	have := 0
	nul := -1
	for nul < 0 {
		if have == len(tmp) {
			return corrupt("header too long")
		}
		n, err := l.fi.zr.Read(tmp[have:])
		for i := have; i < have+n; i++ {
			if tmp[i] == 0 {
				nul = i
				break
			}
		}
		have += n
		if nul < 0 && err != nil {
			return corrupt("truncated header: %v", err)
		}
	}
	typ, size, err := parseObjectHeader(tmp[:nul])
	if err != nil {
		return err
	}
	l.typ, l.size = typ, size
	l.nlea = copy(l.lead[:], tmp[nul+1:have])
	return nil
}

// parseObjectHeader parses "<type> <decimal size>".
func parseObjectHeader(h []byte) (Type, uint64, error) {
	sp := -1
	for i, c := range h {
		if c == ' ' {
			sp = i
			break
		}
	}
	if sp < 0 {
		return 0, 0, corrupt("bad object header")
	}
	typ, ok := parseTypeName(h[:sp])
	if !ok {
		return 0, 0, corrupt("unknown object type %q", truncate(string(h[:sp])))
	}
	digits := h[sp+1:]
	if len(digits) == 0 || len(digits) > 18 {
		return 0, 0, corrupt("bad object size")
	}
	var size uint64
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, 0, corrupt("bad object size")
		}
		size = size*10 + uint64(c-'0')
	}
	return typ, size, nil
}

// readLoose reads a whole loose object into dst.
func (r *Repo) readLoose(id *ObjectID, dst []byte) (Type, []byte, error) {
	l, err := r.openLoose(id)
	if err != nil {
		return 0, nil, err
	}
	defer l.close()
	if l.size > uint64(r.opts.maxObject) {
		return 0, nil, tooLarge(l.size, r.opts.maxObject)
	}
	if uint64(l.nlea) > l.size {
		return 0, nil, corrupt("loose object %s: longer than its header says", id.String())
	}
	dst = grow(dst, int(l.size))
	copy(dst, l.lead[:l.nlea])
	if err := inflateExact(l.fi.zr, dst[l.nlea:], l.fi.scratch[:]); err != nil {
		return 0, nil, wrapLoose(id, err)
	}
	return l.typ, dst, nil
}

// headerPrefix is how much of a loose file is given to the inflater to find
// the header. Go's flate decodes a whole 32 KiB window before returning the
// first byte, so feeding it the whole file would inflate most small objects
// for nothing. The first block header and a few literals fit in 1 KiB.
const headerPrefix = 1024

// looseHeader reads the type and size of a loose object, inflating as little
// as possible.
func (r *Repo) looseHeader(id *ObjectID) (Type, int64, error) {
	f, err := os.Open(r.loosePath(id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return 0, 0, ErrNotFound
		}
		return 0, 0, err
	}
	pb := getBuf()
	pb.b = grow(pb.b, headerPrefix)
	n, rerr := io.ReadFull(f, pb.b)
	_ = f.Close()
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		putBuf(pb)
		return 0, 0, rerr
	}
	typ, size, ok := headerFromPrefix(pb.b[:n])
	putBuf(pb)
	if ok {
		return typ, int64(size), nil
	}
	// Unusual or damaged: take the full path, which reports precise errors.
	l, err := r.openLoose(id)
	if err != nil {
		return 0, 0, err
	}
	defer l.close()
	return l.typ, int64(l.size), nil
}

// headerFromPrefix parses the object header from the beginning of a
// compressed loose object. ok is false if the header cannot be found there.
func headerFromPrefix(src []byte) (typ Type, size uint64, ok bool) {
	m, err := getMem(src)
	if err != nil {
		return 0, 0, false
	}
	defer putMem(m)
	have := 0
	for have < len(m.scratch) {
		n, err := m.zr.Read(m.scratch[have:])
		for i := have; i < have+n; i++ {
			if m.scratch[i] == 0 {
				typ, size, perr := parseObjectHeader(m.scratch[:i])
				return typ, size, perr == nil
			}
		}
		have += n
		if err != nil {
			return 0, 0, false
		}
	}
	return 0, 0, false
}

// looseWithPrefix calls fn for the loose objects whose id starts with the
// hex digits in hex (at least 2 of them), until fn returns false.
func (r *Repo) looseWithPrefix(hex string, fn func(ObjectID) bool) error {
	dir := r.dir + "/objects/" + hex[:2]
	f, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()
	rest := hex[2:]
	for {
		names, err := f.Readdirnames(256)
		for _, n := range names {
			if len(n) != 38 || !strings.HasPrefix(n, rest) {
				continue
			}
			id, ok := parseIDBytes([]byte(hex[:2] + n))
			if !ok {
				continue // "tmp_obj_*" and other files
			}
			if !fn(id) {
				return nil
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
