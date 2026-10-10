// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"errors"
	"io"
)

// OpenBlob opens the blob id for reading and returns its size. Undeltified
// blobs stream from the memory-mapped pack or the loose file, inflated as they
// are read, so a large file is never held in memory. A deltified blob is
// rebuilt in a pooled buffer first and is bounded by WithMaxObjectSize
// (ErrTooLarge). The caller must close the reader, which also releases the
// pack it reads from. Reads detect truncated or oversized data and return
// ErrCorrupt.
func (r *Repo) OpenBlob(id ObjectID) (int64, io.ReadCloser, error) {
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
			size, rc, err := r.openPackedBlob(p, off, id)
			if err != nil {
				return 0, nil, wrapPack(p.name, err)
			}
			return size, rc, nil
		}
		size, rc, err := r.openLooseBlob(&id)
		if err == nil {
			return size, rc, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return 0, nil, err
		}
		if again, err := f.again(); err != nil {
			return 0, nil, err
		} else if !again {
			return 0, nil, notFound("object %s", id)
		}
	}
}

func (r *Repo) openPackedBlob(p *pack, off uint64, id ObjectID) (int64, io.ReadCloser, error) {
	h, err := parseEntryHeader(p.data, off)
	if err != nil {
		return 0, nil, err
	}
	if h.isDelta() {
		b := getBuf()
		typ, data, err := r.readPacked(p, off, b.b)
		if err != nil {
			putBuf(b)
			return 0, nil, err
		}
		b.b = data
		if typ != TypeBlob {
			putBuf(b)
			return 0, nil, wrongType(id, typ, TypeBlob)
		}
		return int64(len(data)), &memBlob{buf: b, r: bytes.NewReader(data)}, nil
	}
	if Type(h.typ) != TypeBlob {
		return 0, nil, wrongType(id, Type(h.typ), TypeBlob)
	}
	if h.size > 1<<62 || h.size/1032 > uint64(len(p.stream(&h))) {
		return 0, nil, corrupt("blob of %d bytes at %d", h.size, off)
	}
	m, err := getMem(p.stream(&h))
	if err != nil {
		return 0, nil, err
	}
	p.retain()
	return int64(h.size), &streamBlob{src: m, pack: p, left: int64(h.size), zr: m.zr}, nil
}

func (r *Repo) openLooseBlob(id *ObjectID) (int64, io.ReadCloser, error) {
	l, err := r.openLoose(id)
	if err != nil {
		return 0, nil, err
	}
	if l.typ != TypeBlob {
		l.close()
		return 0, nil, wrongType(*id, l.typ, TypeBlob)
	}
	if l.size > 1<<62 || uint64(l.nlea) > l.size {
		l.close()
		return 0, nil, corrupt("loose object %s: size %d", id.String(), l.size)
	}
	return int64(l.size), &streamBlob{loose: l, left: int64(l.size), zr: l.fi.zr, lead: l.lead[:l.nlea]}, nil
}

// memBlob serves a blob rebuilt in memory.
type memBlob struct {
	buf *buffer
	r   *bytes.Reader
}

func (m *memBlob) Read(p []byte) (int, error) {
	if m.r == nil {
		return 0, io.ErrClosedPipe
	}
	return m.r.Read(p)
}

func (m *memBlob) WriteTo(w io.Writer) (int64, error) {
	if m.r == nil {
		return 0, io.ErrClosedPipe
	}
	return m.r.WriteTo(w)
}

func (m *memBlob) Close() error {
	if m.r != nil {
		m.r = nil
		putBuf(m.buf)
		m.buf = nil
	}
	return nil
}

// streamBlob inflates a blob while it is read.
type streamBlob struct {
	src   *memInflater // packed
	pack  *pack
	loose *looseObject // loose
	zr    io.Reader
	lead  []byte // loose: content already read with the header
	left  int64  // bytes still to return
	done  bool   // end of stream verified
	err   error
	probe [1]byte
}

func (s *streamBlob) Read(p []byte) (int, error) {
	if s.zr == nil {
		return 0, io.ErrClosedPipe
	}
	if s.err != nil {
		return 0, s.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if s.left == 0 {
		return 0, s.finish()
	}
	if int64(len(p)) > s.left {
		p = p[:s.left]
	}
	n := 0
	if len(s.lead) > 0 {
		n = copy(p, s.lead)
		s.lead = s.lead[n:]
		s.left -= int64(n)
		return n, nil
	}
	n, err := s.zr.Read(p)
	s.left -= int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = corrupt("blob: %v", err)
		return n, s.err
	}
	if errors.Is(err, io.EOF) && s.left > 0 {
		s.err = corrupt("blob shorter than its declared size")
		return n, s.err
	}
	if n > 0 {
		return n, nil
	}
	if s.left == 0 {
		return 0, s.finish()
	}
	return 0, nil
}

// finish checks that the stream ends exactly here and that its checksum is
// right, then reports EOF.
func (s *streamBlob) finish() error {
	if !s.done {
		n, err := s.zr.Read(s.probe[:])
		switch {
		case n != 0:
			s.err = corrupt("blob longer than its declared size")
		case !errors.Is(err, io.EOF):
			s.err = corrupt("blob: %v", err)
		}
		s.done = true
	}
	if s.err != nil {
		return s.err
	}
	return io.EOF
}

func (s *streamBlob) Close() error {
	if s.zr == nil {
		return nil
	}
	s.zr = nil
	if s.src != nil {
		putMem(s.src)
		s.pack.release()
	}
	if s.loose != nil {
		s.loose.close()
	}
	return nil
}
