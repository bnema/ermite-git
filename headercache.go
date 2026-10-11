// SPDX-License-Identifier: MIT

package git

import (
	"math"
	"math/bits"
	"sync/atomic"
)

// headerCache remembers the type and the result size of the deltified
// objects of one pack, by entry offset. Finding that size means inflating the
// start of the delta, which dominated listing a directory with the size of
// each file (ObjectHeader per entry).
//
// A pack never changes, so an entry never goes stale and nothing invalidates
// it; the cache goes away with the pack. It is a fixed array of slots in
// which an offset has two places (a 2-way set: the slot its hash picks and
// the neighbour), and a newer entry replaces one of the two when both are
// taken. The memory is bounded by construction (headerSlotBytes per slot, at
// most maxHeaderSlots slots per pack, allocated at the first deltified
// header), a hit costs a hash and a few atomic loads and allocates nothing,
// and there is no lock, no eviction list and no allocation per entry. One way
// only made two hot offsets that collide evict each other on every lookup:
// 4,096 deltified headers read in rotation on lazygit (65,536 slots) cost
// 334 ns each with one way and 55 ns with two. A sync.Map would allocate on every
// store and would need its own bound and eviction; a mutex-protected map would
// serialize the readers.
//
// Each slot is a seqlock made of atomics (so it is also clean for the race
// detector): a writer makes seq odd, writes the fields, makes it even again;
// a reader that sees an odd seq, or a seq that moved, takes a miss. A writer
// that finds the slot busy gives up: this is a cache.
type headerCache struct {
	slots []headerSlot
	shift uint // 64 - log2(len(slots))
}

type headerSlot struct {
	seq atomic.Uint64
	off atomic.Uint64 // entry offset + 1; 0 is an empty slot
	val atomic.Uint64 // size<<3 | type
}

const (
	headerSlotBytes = 24
	// maxHeaderSlots bounds a pack to 1.5 MiB of cache. A pack with more
	// deltified objects than that keeps the most recent ones per slot.
	maxHeaderSlots = 1 << 16
	minHeaderSlots = 64 // at least 2, a set is two slots
	// maxCachedSize is the largest size a slot holds (3 bits go to the type).
	maxCachedSize = 1 << 60
)

// newHeaderCache sizes the table for a pack of objects objects: the next
// power of two, within the bounds.
func newHeaderCache(objects uint32) *headerCache {
	n := min(max(int(objects), minHeaderSlots), maxHeaderSlots)
	n = 1 << bits.Len(uint(n-1)) // round up to a power of two
	return &headerCache{slots: make([]headerSlot, n), shift: uint(64 - bits.TrailingZeros(uint(n)))}
}

// index returns the position of the first of the two places of off.
func (c *headerCache) index(off uint64) (first int, h uint64) {
	h = off * 0x9e3779b97f4a7c15
	return int(h >> c.shift), h
}

func (s *headerSlot) read(off uint64) (v uint64, ok bool) {
	seq := s.seq.Load()
	if seq&1 != 0 {
		return 0, false
	}
	o, v := s.off.Load(), s.val.Load()
	if o != off+1 || s.seq.Load() != seq {
		return 0, false
	}
	return v, true
}

func (c *headerCache) get(off uint64) (Type, int64, bool) {
	if off == math.MaxUint64 { // off+1 would wrap to the empty marker
		return 0, 0, false
	}
	i, _ := c.index(off)
	if v, ok := c.slots[i].read(off); ok {
		return Type(v & 7), int64(v >> 3), true
	}
	if v, ok := c.slots[i^1].read(off); ok {
		return Type(v & 7), int64(v >> 3), true
	}
	return 0, 0, false
}

func (c *headerCache) put(off uint64, typ Type, size int64) {
	if size < 0 || size >= maxCachedSize || off == math.MaxUint64 {
		return
	}
	i, h := c.index(off)
	// An empty place first; else a pseudo-random one of the two.
	s := &c.slots[i]
	if s.off.Load() != 0 {
		if t := &c.slots[i^1]; t.off.Load() == 0 || h&(1<<20) != 0 {
			s = t
		}
	}
	seq := s.seq.Load()
	if seq&1 != 0 || !s.seq.CompareAndSwap(seq, seq+1) {
		return
	}
	s.off.Store(off + 1)
	s.val.Store(uint64(size)<<3 | uint64(typ))
	s.seq.Store(seq + 2)
}
