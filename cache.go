// SPDX-License-Identifier: MIT

package git

import "sync"

// cacheShards is the number of independently locked LRU lists.
const cacheShards = 8

type cacheKey struct {
	pack uint32
	off  uint64
}

type cacheEntry struct {
	key        cacheKey
	typ        Type
	data       []byte // immutable once cached
	prev, next *cacheEntry
}

// deltaCache is an LRU cache of delta bases bounded in bytes. Cached slices
// are never modified or recycled, so a reader may keep using one after it is
// evicted.
type deltaCache struct {
	shards [cacheShards]cacheShard
}

type cacheShard struct {
	mu         sync.Mutex
	m          map[cacheKey]*cacheEntry
	head, tail *cacheEntry // head is the most recently used
	bytes      int64
	budget     int64
}

func newDeltaCache(budget int64) *deltaCache {
	c := &deltaCache{}
	for i := range c.shards {
		c.shards[i].budget = budget / cacheShards
		c.shards[i].m = make(map[cacheKey]*cacheEntry)
	}
	return c
}

func (c *deltaCache) shard(k cacheKey) *cacheShard {
	h := uint64(k.pack)*0x9e3779b97f4a7c15 ^ k.off*0xff51afd7ed558ccd
	return &c.shards[(h>>32)%cacheShards]
}

func (c *deltaCache) get(k cacheKey) (Type, []byte, bool) {
	s := c.shard(k)
	if s.budget <= 0 {
		return 0, nil, false
	}
	s.mu.Lock()
	e := s.m[k]
	if e == nil {
		s.mu.Unlock()
		return 0, nil, false
	}
	s.moveFront(e)
	typ, data := e.typ, e.data
	s.mu.Unlock()
	return typ, data, true
}

// fits reports whether an object of n bytes can be cached.
func (c *deltaCache) fits(n int) bool {
	return int64(n)+64 <= c.shards[0].budget
}

// putOwned stores data, which the caller must not modify afterwards.
func (c *deltaCache) putOwned(k cacheKey, typ Type, data []byte) {
	s := c.shard(k)
	n := int64(len(data)) + 64
	if n > s.budget {
		return
	}
	cp := data
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.m[k]; e != nil {
		s.moveFront(e)
		return
	}
	e := &cacheEntry{key: k, typ: typ, data: cp}
	s.m[k] = e
	s.pushFront(e)
	s.bytes += n
	for s.bytes > s.budget && s.tail != nil {
		old := s.tail
		s.unlink(old)
		delete(s.m, old.key)
		s.bytes -= int64(len(old.data)) + 64
	}
}

func (s *cacheShard) unlink(e *cacheEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		s.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		s.tail = e.prev
	}
	e.prev, e.next = nil, nil
}

func (s *cacheShard) pushFront(e *cacheEntry) {
	e.next = s.head
	if s.head != nil {
		s.head.prev = e
	}
	s.head = e
	if s.tail == nil {
		s.tail = e
	}
}

func (s *cacheShard) moveFront(e *cacheEntry) {
	if s.head == e {
		return
	}
	s.unlink(e)
	s.pushFront(e)
}
