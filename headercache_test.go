// SPDX-License-Identifier: MIT

package git

import (
	"fmt"
	"sync"
	"testing"
)

func TestHeaderCacheSizing(t *testing.T) {
	for _, c := range []struct {
		objects uint32
		slots   int
	}{
		{0, minHeaderSlots}, {1, minHeaderSlots}, {64, 64}, {65, 128}, {1000, 1024},
		{maxHeaderSlots, maxHeaderSlots}, {maxHeaderSlots + 1, maxHeaderSlots}, {1 << 31, maxHeaderSlots},
	} {
		hc := newHeaderCache(c.objects)
		if len(hc.slots) != c.slots {
			t.Errorf("%d objects: %d slots, want %d", c.objects, len(hc.slots), c.slots)
		}
	}
	if max := maxHeaderSlots * headerSlotBytes; max > 2<<20 {
		t.Errorf("a pack may pin %d bytes of header cache", max)
	}
}

func TestHeaderCacheGetPut(t *testing.T) {
	hc := newHeaderCache(1000)
	if _, _, ok := hc.get(12); ok {
		t.Fatal("hit in an empty cache")
	}
	hc.put(12, TypeTree, 4242)
	if typ, size, ok := hc.get(12); !ok || typ != TypeTree || size != 4242 {
		t.Fatalf("get = %v %v %v", typ, size, ok)
	}
	// Offset 0 and size 0 are values like others.
	hc.put(0, TypeBlob, 0)
	if typ, size, ok := hc.get(0); !ok || typ != TypeBlob || size != 0 {
		t.Fatalf("get(0) = %v %v %v", typ, size, ok)
	}
	// A size that does not fit a slot is not cached, not truncated.
	hc.put(99, TypeBlob, maxCachedSize)
	if _, _, ok := hc.get(99); ok {
		t.Fatal("oversized entry cached")
	}
	// Distinct offsets that share a slot never answer for each other.
	for off := uint64(1); off < 20000; off++ {
		hc.put(off, TypeBlob, int64(off)*3)
	}
	hits := 0
	for off := uint64(1); off < 20000; off++ {
		if typ, size, ok := hc.get(off); ok {
			hits++
			if typ != TypeBlob || size != int64(off)*3 {
				t.Fatalf("offset %d answered %v %d", off, typ, size)
			}
		}
	}
	if hits == 0 || hits > len(hc.slots) {
		t.Fatalf("%d hits for %d slots", hits, len(hc.slots))
	}
}

func TestHeaderCacheConcurrent(t *testing.T) {
	hc := newHeaderCache(64)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20000 {
				off := uint64((i*7 + g) % 500)
				if typ, size, ok := hc.get(off); ok {
					if typ != TypeBlob || size != int64(off)*5 {
						t.Errorf("torn read at %d: %v %d", off, typ, size)
						return
					}
				} else {
					hc.put(off, TypeBlob, int64(off)*5)
				}
			}
		}()
	}
	wg.Wait()
}

func TestHeaderCacheNoAllocOnHit(t *testing.T) {
	hc := newHeaderCache(64)
	hc.put(77, TypeBlob, 5)
	if got := testing.AllocsPerRun(100, func() { hc.get(77) }); got != 0 {
		t.Fatalf("a hit allocates %v", got)
	}
}

// ObjectHeader answers the same with and without the cache for every object of
// a deltified pack, a second time too (from the cache), and a hit allocates
// nothing and does not inflate.
func TestObjectHeaderCached(t *testing.T) {
	f := newFx(t)
	for i := 0; i < 40; i++ {
		f.write("f.txt", append(textLines(300, 0), fmt.Sprintf("rev %d\n", i)...), 0o644)
		f.write(fmt.Sprintf("many/file-%03d.txt", i), []byte{byte(i)}, 0o644)
		f.commit(fmt.Sprintf("commit %d", i))
	}
	f.git("repack", "-adf", "--depth=50", "--window=250")
	r := f.open()
	p := r.snap.Load().packs[0]
	deltas := 0
	for i := range p.idx.n {
		off, _ := p.idx.offset(i)
		var id ObjectID
		copy(id[:], p.idx.id(i))
		wt, ws, _, err := p.readHeader(off) // uncached
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			gt, gs, err := r.ObjectHeader(id)
			if err != nil || gt != wt || gs != ws {
				t.Fatalf("%s: ObjectHeader = %v %d %v, want %v %d", id, gt, gs, err, wt, ws)
			}
		}
		if h, _ := parseEntryHeader(p.data, off); h.isDelta() {
			deltas++
			if _, _, ok := p.hdrs.Load().get(off); !ok && len(p.hdrs.Load().slots) >= int(p.idx.n) {
				t.Errorf("%s: deltified header not cached", id)
			}
			if raceEnabled {
				continue
			}
			if got := testing.AllocsPerRun(20, func() { _, _, _ = r.ObjectHeader(id) }); got != 0 {
				t.Errorf("%s: cached ObjectHeader allocates %v", id, got)
			}
		}
	}
	if deltas == 0 {
		t.Fatal("no deltified object in the fixture")
	}
}
