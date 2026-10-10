// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"encoding/binary"
)

// idxFile is a validated, read-only pack index (version 2) held in memory.
type idxFile struct {
	data     []byte
	n        uint32
	fanout   [256]uint32
	names    []byte // n*20 sorted object ids
	offs     []byte // n*4 offsets
	large    []byte // 8 byte offsets
	packHash []byte // 20 bytes: the checksum of the pack file
}

const (
	idxHeaderLen = 8
	idxFanoutLen = 256 * 4
	idxTrailer   = 40
)

var idxMagic = []byte{0xff, 't', 'O', 'c'}

// parseIdx validates the layout of a v2 index: sizes, fan-out monotony and
// the large offset table. It does not verify the checksums.
func parseIdx(data []byte) (*idxFile, error) {
	if len(data) < idxHeaderLen+idxFanoutLen+idxTrailer {
		return nil, corrupt("idx: file too short (%d bytes)", len(data))
	}
	if !bytes.Equal(data[:4], idxMagic) {
		return nil, unsupported("idx: version 1 index")
	}
	if v := binary.BigEndian.Uint32(data[4:8]); v != 2 {
		return nil, unsupported("idx: version %d", v)
	}
	x := &idxFile{data: data}
	prev := uint32(0)
	for i := range x.fanout {
		v := binary.BigEndian.Uint32(data[idxHeaderLen+4*i:])
		if v < prev {
			return nil, corrupt("idx: fan-out decreases at %#02x", i)
		}
		x.fanout[i] = v
		prev = v
	}
	x.n = prev
	n := uint64(x.n)
	fixed := uint64(idxHeaderLen+idxFanoutLen) + n*28 + idxTrailer
	if uint64(len(data)) < fixed {
		return nil, corrupt("idx: %d objects do not fit in %d bytes", n, len(data))
	}
	extra := uint64(len(data)) - fixed
	if extra%8 != 0 {
		return nil, corrupt("idx: large offset table of %d bytes", extra)
	}
	p := uint64(idxHeaderLen + idxFanoutLen)
	x.names = data[p : p+n*20]
	p += n * 20
	p += n * 4 // crc32
	x.offs = data[p : p+n*4]
	p += n * 4
	x.large = data[p : p+extra]
	p += extra
	x.packHash = data[p : p+20]
	return x, nil
}

// id returns the id at position i.
func (x *idxFile) id(i uint32) []byte { return x.names[uint64(i)*20 : uint64(i)*20+20] }

// find returns the position of id.
func (x *idxFile) find(id *ObjectID) (uint32, bool) {
	lo, hi := x.bounds(id[0])
	pos := x.lowerBound(id[:], lo, hi)
	if pos < hi && bytes.Equal(x.id(pos), id[:]) {
		return pos, true
	}
	return 0, false
}

// bounds returns the range of positions whose first byte is b.
func (x *idxFile) bounds(b byte) (lo, hi uint32) {
	if b > 0 {
		lo = x.fanout[b-1]
	}
	return lo, x.fanout[b]
}

// lowerBound returns the first position in [lo, hi) whose id is not less
// than key, comparing len(key) bytes.
func (x *idxFile) lowerBound(key []byte, lo, hi uint32) uint32 {
	for lo < hi {
		mid := lo + (hi-lo)/2
		if bytes.Compare(x.id(mid)[:len(key)], key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// offset returns the offset in the pack of the object at position i.
func (x *idxFile) offset(i uint32) (uint64, error) {
	v := binary.BigEndian.Uint32(x.offs[uint64(i)*4:])
	if v&0x80000000 == 0 {
		return uint64(v), nil
	}
	k := uint64(v & 0x7fffffff)
	if (k+1)*8 > uint64(len(x.large)) {
		return 0, corrupt("idx: large offset %d out of %d", k, len(x.large)/8)
	}
	return binary.BigEndian.Uint64(x.large[k*8:]), nil
}

// prefixMatches calls fn with each id that starts with the nibble prefix
// (nibbles hex digits, packed two per byte in key), stopping when fn returns
// false.
func (x *idxFile) prefixMatches(key []byte, nibbles int, fn func(id ObjectID) bool) {
	var lo, hi uint32
	var first, last byte
	whole := nibbles / 2
	first, last = key[0], key[0]
	if nibbles == 1 {
		first, last = key[0]&0xf0, key[0]|0x0f
	}
	lo, _ = x.bounds(first)
	_, hi = x.bounds(last)
	var pad [20]byte
	copy(pad[:], key[:(nibbles+1)/2])
	if nibbles%2 == 1 {
		pad[whole] &= 0xf0
	}
	pos := x.lowerBound(pad[:(nibbles+1)/2], lo, hi)
	for ; pos < hi; pos++ {
		id := x.id(pos)
		if !hasNibblePrefix(id, key, nibbles) {
			return
		}
		var o ObjectID
		copy(o[:], id)
		if !fn(o) {
			return
		}
	}
}

// hasNibblePrefix reports whether id starts with the first n hex digits
// encoded in key.
func hasNibblePrefix(id, key []byte, n int) bool {
	whole := n / 2
	if !bytes.Equal(id[:whole], key[:whole]) {
		return false
	}
	if n%2 == 1 {
		return id[whole]&0xf0 == key[whole]&0xf0
	}
	return true
}
