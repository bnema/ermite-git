// SPDX-License-Identifier: MIT

package git

import (
	"encoding/binary"
)

// Changed-path Bloom filters (BIDX and BDAT chunks of the commit-graph).
//
// For each commit the filter holds the paths that differ between the commit
// and its first parent (or the empty tree for a root commit). Git inserts
// every changed path together with each of its leading directories, without
// a trailing slash: changing "a/b/c" adds "a/b/c", "a/b" and "a". Looking
// up a path therefore only needs the key of that path, whether it is a file
// or a directory.
//
// Hashing mirrors git's bloom.c. A key is two 32-bit murmur3 hashes of the
// path with fixed seeds, h0 and h1; the i-th probe is h0 + i*h1 (mod 2^32),
// taken modulo the number of bits of the filter. Bit n of the filter is
// bit (n & 7) of byte n/8.
//
// BDAT header version 1 is the original hash, which sign-extends bytes >=
// 0x80 when git was built with a signed char (x86); version 2 fixes it with
// unsigned arithmetic. Both are supported; the version is read per layer
// from BDAT. For paths made of bytes < 0x80 the two are identical.
//
// A filter of one byte 0x00 means "no changed path" and one byte 0xff means
// "too many changed paths (more than 512)": the latter is reported as
// unknown. An empty or out-of-bounds filter is unknown as well.

const (
	bloomHeaderSize = 12  // version, number of hashes, bits per entry
	bloomMaxHashes  = 256 // sanity bound on the probe count read from disk
	bloomSeed0      = 0x293ae76f
	bloomSeed1      = 0x7e646e2c
)

// bloomKey holds the precomputed hashes of a path for both hash versions. It
// is a small value; build it once per path and reuse it for many commits.
type bloomKey struct {
	v1, v2 [2]uint32
}

// newBloomKey computes the key of path. It does not allocate.
func newBloomKey(path []byte) bloomKey {
	k := bloomKey{v2: [2]uint32{
		murmur3Seeded(bloomSeed0, path, false),
		murmur3Seeded(bloomSeed1, path, false),
	}}
	k.v1 = k.v2
	for _, b := range path {
		if b >= 0x80 {
			k.v1 = [2]uint32{
				murmur3Seeded(bloomSeed0, path, true),
				murmur3Seeded(bloomSeed1, path, true),
			}
			break
		}
	}
	return k
}

// newBloomKeyString is newBloomKey for a string path.
func newBloomKeyString(path string) bloomKey {
	return newBloomKey([]byte(path))
}

func rotl32(x uint32, r uint) uint32 { return x<<r | x>>(32-r) }

// murmur3Seeded is MurmurHash3 x86 32-bit as implemented by git. With
// signedChars it reproduces git's version 1 behaviour on platforms where
// char is signed (x86): every byte >= 0x80, in full blocks and in the tail, is
// sign-extended before being shifted and OR-ed, which corrupts the bits of
// the neighbouring bytes of the block.
func murmur3Seeded(seed uint32, data []byte, signedChars bool) uint32 {
	const (
		c1 = 0xcc9e2d51
		c2 = 0x1b873593
	)
	ext := func(b byte) uint32 {
		if signedChars {
			return uint32(int32(int8(b)))
		}
		return uint32(b)
	}
	n4 := len(data) / 4
	for i := 0; i < n4; i++ {
		d := data[4*i : 4*i+4]
		k := ext(d[0]) | ext(d[1])<<8 | ext(d[2])<<16 | ext(d[3])<<24
		k *= c1
		k = rotl32(k, 15)
		k *= c2
		seed ^= k
		seed = rotl32(seed, 13)*5 + 0xe6546b64
	}
	tail := data[4*n4:]
	var k uint32
	switch len(tail) {
	case 3:
		k ^= ext(tail[2]) << 16
		fallthrough
	case 2:
		k ^= ext(tail[1]) << 8
		fallthrough
	case 1:
		k ^= ext(tail[0])
		k *= c1
		k = rotl32(k, 15)
		k *= c2
		seed ^= k
	}
	seed ^= uint32(len(data))
	seed ^= seed >> 16
	seed *= 0x85ebca6b
	seed ^= seed >> 13
	seed *= 0xc2b2ae35
	seed ^= seed >> 16
	return seed
}

// bloomFilter returns the filter bytes of the commit at pos. ok is false when
// the layer has no usable Bloom data, or the filter is empty, truncated or
// the "too many changes" marker.
func (g *commitGraph) bloomFilter(pos uint32) (l *graphLayer, filter []byte, ok bool) {
	l, idx := g.layerOf(pos)
	if l == nil || !l.hasBloom {
		return nil, nil, false
	}
	var start uint32
	if idx > 0 {
		start = binary.BigEndian.Uint32(l.bidx[(idx-1)*4:])
	}
	end := binary.BigEndian.Uint32(l.bidx[idx*4:])
	data := l.bdat[bloomHeaderSize:]
	if end <= start || uint64(end) > uint64(len(data)) {
		return l, nil, false
	}
	filter = data[start:end]
	if len(filter) == 1 && filter[0] == 0xff {
		return l, nil, false
	}
	return l, filter, true
}

// maybeChangedKey reports whether the path of k may have changed in the
// commit at pos relative to its first parent. maybe=false is definitive (the
// path did not change); maybe=true may be a false positive. ok=false means
// the answer is unknown (no filter, too large, truncated, bad position) and
// the caller must diff the trees.
func (g *commitGraph) maybeChangedKey(pos uint32, k *bloomKey) (maybe, ok bool) {
	l, f, ok := g.bloomFilter(pos)
	if !ok {
		return false, false
	}
	h := &k.v2
	if l.bloomVersion == 1 {
		h = &k.v1
	}
	mod := uint64(len(f)) * 8
	var h0, h1 = h[0], h[1]
	for i := uint32(0); i < l.bloomHashes; i++ {
		bit := uint64(h0+i*h1) % mod
		if f[bit>>3]&(1<<(bit&7)) == 0 {
			return false, true
		}
	}
	return true, true
}

// maybeChanged is maybeChangedKey for a path given as bytes, without a
// trailing slash, relative to the repository root. Use newBloomKey when
// testing the same path against many commits.
func (g *commitGraph) maybeChanged(pos uint32, path []byte) (maybe, ok bool) {
	k := newBloomKey(path)
	return g.maybeChangedKey(pos, &k)
}

// hasBloom reports whether the commit at pos has a usable changed-path
// filter.
func (g *commitGraph) hasBloom(pos uint32) bool {
	_, _, ok := g.bloomFilter(pos)
	return ok
}
