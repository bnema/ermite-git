// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"sync/atomic"
)

const (
	packHeaderLen  = 12
	packTrailerLen = 20

	objOfsDelta = 6
	objRefDelta = 7
)

// pack is an open pack file with its index, both mapped read-only. It is
// shared by the snapshots that list it and unmapped when the last one is
// released.
type pack struct {
	id    uint32
	name  string // "pack-<hash>", without extension
	idx   *idxFile
	data  []byte // the whole .pack file
	sig   fileSig
	unmap [2]func() error
	refs  atomic.Int32

	hdrs atomic.Pointer[headerCache] // created by the first deltified header
}

func (p *pack) retain() { p.refs.Add(1) }

func (p *pack) release() {
	if p.refs.Add(-1) == 0 {
		for _, f := range p.unmap {
			if f != nil {
				_ = f()
			}
		}
		p.data, p.idx = nil, nil
	}
}

// openPack maps and validates dir/<name>.idx and dir/<name>.pack. It returns
// (nil, nil) when a file vanished, which happens while a repack replaces it.
func openPack(dir, name string, id uint32, sig fileSig) (*pack, error) {
	idxF, err := os.Open(dir + "/" + name + ".idx")
	if err != nil {
		return nil, ignoreMissing(err)
	}
	defer func() { _ = idxF.Close() }()
	packF, err := os.Open(dir + "/" + name + ".pack")
	if err != nil {
		return nil, ignoreMissing(err)
	}
	defer func() { _ = packF.Close() }()
	idxStat, err := idxF.Stat()
	if err != nil {
		return nil, err
	}
	packStat, err := packF.Stat()
	if err != nil {
		return nil, err
	}
	idxData, unmapIdx, err := mapFile(idxF, idxStat.Size())
	if err != nil {
		return nil, err
	}
	packData, unmapPack, err := mapFile(packF, packStat.Size())
	if err != nil {
		_ = unmapIdx()
		return nil, err
	}
	p := &pack{id: id, name: name, data: packData, sig: sig, unmap: [2]func() error{unmapIdx, unmapPack}}
	p.refs.Store(1)
	if p.idx, err = parseIdx(idxData); err != nil {
		p.release()
		return nil, wrapPack(name, err)
	}
	if err := p.validate(); err != nil {
		p.release()
		return nil, wrapPack(name, err)
	}
	return p, nil
}

// validate checks the pack header and ties the pack to its index.
func (p *pack) validate() error {
	if len(p.data) < packHeaderLen+packTrailerLen {
		return corrupt("pack too short (%d bytes)", len(p.data))
	}
	if string(p.data[:4]) != "PACK" {
		return corrupt("bad pack signature")
	}
	if v := binary.BigEndian.Uint32(p.data[4:8]); v != 2 && v != 3 {
		return corrupt("pack version %d", v)
	}
	if n := binary.BigEndian.Uint32(p.data[8:12]); n != p.idx.n {
		return corrupt("pack holds %d objects, index %d", n, p.idx.n)
	}
	if !bytes.Equal(p.data[len(p.data)-packTrailerLen:], p.idx.packHash) {
		return corrupt("pack checksum differs from its index")
	}
	return nil
}

func wrapPack(name string, err error) error {
	return &packError{name: name, err: err}
}

type packError struct {
	name string
	err  error
}

func (e *packError) Error() string { return e.name + ": " + e.err.Error() }
func (e *packError) Unwrap() error { return e.err }

// entryHdr is a parsed pack entry header.
type entryHdr struct {
	typ     uint8  // 1..4, objOfsDelta or objRefDelta
	size    uint64 // inflated size of the entry data (the delta for deltas)
	data    uint64 // offset of the zlib stream
	baseOff uint64 // objOfsDelta: absolute offset of the base
	baseID  ObjectID
}

func (h *entryHdr) isDelta() bool { return h.typ == objOfsDelta || h.typ == objRefDelta }

// parseEntryHeader parses the header of the entry at off in a whole pack
// file (header, entries and trailer). It never reads outside data.
func parseEntryHeader(data []byte, off uint64) (h entryHdr, err error) {
	end := uint64(len(data))
	if end < packHeaderLen+packTrailerLen {
		return h, corrupt("pack too short")
	}
	end -= packTrailerLen
	if off < packHeaderLen || off >= end {
		return h, corrupt("entry offset %d outside pack", off)
	}
	p := off
	c := data[p]
	p++
	h.typ = (c >> 4) & 7
	h.size = uint64(c & 0x0f)
	for shift := uint(4); c&0x80 != 0; shift += 7 {
		if p >= end || shift > 56 {
			return h, corrupt("bad entry size at %d", off)
		}
		c = data[p]
		p++
		h.size |= uint64(c&0x7f) << shift
	}
	switch h.typ {
	case uint8(TypeCommit), uint8(TypeTree), uint8(TypeBlob), uint8(TypeTag):
	case objOfsDelta:
		if p >= end {
			return h, corrupt("truncated delta offset at %d", off)
		}
		c = data[p]
		p++
		rel := uint64(c & 0x7f)
		for c&0x80 != 0 {
			if p >= end || rel >= 1<<56 {
				return h, corrupt("bad delta offset at %d", off)
			}
			c = data[p]
			p++
			rel = (rel+1)<<7 | uint64(c&0x7f)
		}
		if rel == 0 || rel > off-packHeaderLen {
			return h, corrupt("delta base offset %d outside pack at %d", rel, off)
		}
		h.baseOff = off - rel
	case objRefDelta:
		if end-p < 20 {
			return h, corrupt("truncated delta base id at %d", off)
		}
		copy(h.baseID[:], data[p:p+20])
		p += 20
	default:
		return h, corrupt("entry type %d at %d", h.typ, off)
	}
	if p >= end {
		return h, corrupt("entry at %d has no data", off)
	}
	h.data = p
	return h, nil
}

// stream returns the compressed bytes of the entry.
func (p *pack) stream(h *entryHdr) []byte {
	return p.data[h.data : len(p.data)-packTrailerLen]
}

// inflateEntry inflates the entry data into dst, which has the exact size
// announced by the header.
func (p *pack) inflateEntry(h *entryHdr, dst []byte) error {
	src := p.stream(h)
	// Deflate cannot expand a byte into more than 1032 bytes.
	if h.size/1032 > uint64(len(src)) {
		return corrupt("entry of %d bytes in %d compressed bytes", h.size, len(src))
	}
	m, err := getMem(src)
	if err != nil {
		return err
	}
	defer putMem(m)
	return inflateExact(&m.zr, dst, m.scratch[:])
}

// chainEnd follows the chain of delta headers from off without inflating
// anything. It returns the type of the base object.
func (p *pack) chainEnd(off uint64, h entryHdr) (Type, error) {
	for depth := 0; ; depth++ {
		if !h.isDelta() {
			return Type(h.typ), nil
		}
		if depth >= maxDeltaDepth {
			return 0, corrupt("delta chain longer than %d", maxDeltaDepth)
		}
		var err error
		off, err = p.baseOffset(&h, off)
		if err != nil {
			return 0, err
		}
		if h, err = parseEntryHeader(p.data, off); err != nil {
			return 0, err
		}
	}
}

// baseOffset returns the offset of the base of a delta entry. A ref-delta
// base must be in the same pack, as Git requires.
func (p *pack) baseOffset(h *entryHdr, self uint64) (uint64, error) {
	if h.typ == objOfsDelta {
		return h.baseOff, nil
	}
	pos, ok := p.idx.find(&h.baseID)
	if !ok {
		return 0, corrupt("delta base %s not in pack %s", h.baseID.String(), p.name)
	}
	off, err := p.idx.offset(pos)
	if err != nil {
		return 0, err
	}
	if off == self {
		return 0, corrupt("delta is its own base at %d", self)
	}
	return off, nil
}

// header returns the type and size of the object at off. The result of a
// deltified object, which costs an inflate to find, is cached per pack.
func (p *pack) header(off uint64) (Type, int64, error) {
	hc := p.hdrs.Load()
	if hc != nil {
		if typ, size, ok := hc.get(off); ok {
			return typ, size, nil
		}
	}
	typ, size, deltified, err := p.readHeader(off)
	if err != nil || !deltified {
		return typ, size, err
	}
	if hc == nil {
		hc = newHeaderCache(p.idx.n)
		if !p.hdrs.CompareAndSwap(nil, hc) {
			hc = p.hdrs.Load()
		}
	}
	hc.put(off, typ, size)
	return typ, size, nil
}

// readHeader reads the type and size of the object at off from the pack.
// deltified tells that the size comes from the start of a delta, which it
// had to inflate.
func (p *pack) readHeader(off uint64) (_ Type, _ int64, deltified bool, _ error) {
	h, err := parseEntryHeader(p.data, off)
	if err != nil {
		return 0, 0, false, err
	}
	if !h.isDelta() {
		if h.size > 1<<62 {
			return 0, 0, false, corrupt("entry size %d", h.size)
		}
		return Type(h.typ), int64(h.size), false, nil
	}
	typ, err := p.chainEnd(off, h)
	if err != nil {
		return 0, 0, false, err
	}
	// The result size is the second varint of the delta, at its start.
	m, err := getMem(p.stream(&h))
	if err != nil {
		return 0, 0, false, err
	}
	defer putMem(m)
	n := min(uint64(len(m.scratch)), h.size)
	if _, err := io.ReadFull(&m.zr, m.scratch[:n]); err != nil {
		return 0, 0, false, corrupt("delta header: %v", err)
	}
	_, rs, _, err := deltaHeader(m.scratch[:n])
	if err != nil {
		return 0, 0, false, err
	}
	if rs > 1<<62 {
		return 0, 0, false, corrupt("delta result size %d", rs)
	}
	return typ, int64(rs), true, nil
}

// readPacked resolves the object at off into dst (grown as needed) and
// returns its type and content.
func (r *Repo) readPacked(p *pack, off uint64, dst []byte) (Type, []byte, error) {
	h, err := parseEntryHeader(p.data, off)
	if err != nil {
		return 0, nil, err
	}
	if !h.isDelta() {
		if h.size > uint64(r.opts.maxObject) {
			return 0, nil, tooLarge(h.size, r.opts.maxObject)
		}
		dst = grow(dst, int(h.size))
		if err := p.inflateEntry(&h, dst); err != nil {
			return 0, nil, err
		}
		return Type(h.typ), dst, nil
	}

	// Collect the chain of deltas down to a base that is a plain object or
	// a cached result.
	type link struct {
		hdr entryHdr
		off uint64
	}
	var arr [64]link
	chain := arr[:0]
	var (
		baseTyp   Type
		base      []byte
		baseFresh bool // base is a private slice that may be cached as is
		scratch   [2]*buffer
	)
	defer func() {
		putBuf(scratch[0])
		putBuf(scratch[1])
	}()
	cur, curHdr := off, h
	for {
		if curHdr.isDelta() {
			if len(chain) >= maxDeltaDepth {
				return 0, nil, corrupt("delta chain longer than %d", maxDeltaDepth)
			}
			chain = append(chain, link{curHdr, cur})
			next, err := p.baseOffset(&curHdr, cur)
			if err != nil {
				return 0, nil, err
			}
			cur = next
			if t, data, ok := r.cache.get(cacheKey{p.id, cur}); ok {
				baseTyp, base = t, data
				break
			}
			if curHdr, err = parseEntryHeader(p.data, cur); err != nil {
				return 0, nil, err
			}
			continue
		}
		if curHdr.size > uint64(r.opts.maxObject) {
			return 0, nil, tooLarge(curHdr.size, r.opts.maxObject)
		}
		baseTyp = Type(curHdr.typ)
		n := int(curHdr.size)
		if r.cache.fits(n) {
			base, baseFresh = make([]byte, n), true
		} else {
			scratch[0] = getBuf()
			scratch[0].b = grow(scratch[0].b, n)
			base = scratch[0].b
		}
		if err := p.inflateEntry(&curHdr, base); err != nil {
			return 0, nil, err
		}
		if baseFresh {
			r.cache.putOwned(cacheKey{p.id, cur}, baseTyp, base)
		}
		break
	}

	delta := getBuf()
	defer putBuf(delta)
	for i := len(chain) - 1; i >= 0; i-- {
		l := chain[i]
		if l.hdr.size > uint64(r.opts.maxObject) {
			return 0, nil, tooLarge(l.hdr.size, r.opts.maxObject)
		}
		delta.b = grow(delta.b, int(l.hdr.size))
		if err := p.inflateEntry(&l.hdr, delta.b); err != nil {
			return 0, nil, err
		}
		bs, rs, body, err := deltaHeader(delta.b)
		if err != nil {
			return 0, nil, err
		}
		if bs != uint64(len(base)) {
			return 0, nil, corrupt("delta at %d expects a base of %d bytes, got %d", l.off, bs, len(base))
		}
		if rs > uint64(r.opts.maxObject) {
			return 0, nil, tooLarge(rs, r.opts.maxObject)
		}
		var out []byte
		fresh := false
		switch {
		case i == 0:
			out = grow(dst, int(rs))
		case r.cache.fits(int(rs)):
			out, fresh = make([]byte, rs), true
		default:
			// Write into the scratch buffer that does not hold base.
			k := 0
			if scratch[0] != nil && len(base) > 0 && len(scratch[0].b) > 0 && &scratch[0].b[0] == &base[0] {
				k = 1
			}
			if scratch[k] == nil {
				scratch[k] = getBuf()
			}
			scratch[k].b = grow(scratch[k].b, int(rs))
			out = scratch[k].b
		}
		if err := applyDelta(out, base, delta.b, body); err != nil {
			return 0, nil, err
		}
		if fresh {
			r.cache.putOwned(cacheKey{p.id, l.off}, baseTyp, out)
		}
		base = out
	}
	return baseTyp, base, nil
}

func tooLarge(size uint64, limit int64) error {
	return &sizeError{size: size, limit: limit}
}

type sizeError struct {
	size  uint64
	limit int64
}

func (e *sizeError) Error() string {
	return ErrTooLarge.Error() + ": " + itoa(e.size) + " bytes, limit " + itoa(uint64(e.limit))
}
func (e *sizeError) Unwrap() error { return ErrTooLarge }

func itoa(v uint64) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	return string(b[i:])
}
