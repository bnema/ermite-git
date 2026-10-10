// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRefreshDuringRepack runs readers while git repacks (deleting the packs
// they read from), prunes and packs refs, and the Repo is refreshed. No read
// may fail, and every read must return the right content.
func TestRefreshDuringRepack(t *testing.T) {
	f := newFx(t)
	var ids []ObjectID
	var blobs [][]byte
	for i := 0; i < 40; i++ {
		data := append(textLines(500, i%3), []byte(fmt.Sprintf("rev %d\n", i))...)
		f.write("f.txt", data, 0o644)
		f.write(fmt.Sprintf("d/%d.txt", i), []byte(fmt.Sprint(i)), 0o644)
		f.commit(fmt.Sprintf("c%d", i))
		ids = append(ids, mustID(t, f.git("rev-parse", "HEAD:f.txt")))
		blobs = append(blobs, data)
	}
	f.git("repack", "-adf", "--depth=50", "--window=50")
	r := f.open(WithDeltaCacheSize(1 << 20))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads atomic.Int64
	var failures atomic.Int64
	fail := func(format string, a ...any) {
		failures.Add(1)
		t.Errorf(format, a...)
	}
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var buf []byte
			for i := w; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				k := i % len(ids)
				switch i % 4 {
				case 0:
					typ, got, err := r.ReadObject(ids[k], buf)
					if err != nil || typ != TypeBlob || !bytes.Equal(got, blobs[k]) {
						fail("ReadObject: %v", err)
						return
					}
					buf = got
				case 1:
					size, rc, err := r.OpenBlob(ids[k])
					if err != nil {
						fail("OpenBlob: %v", err)
						return
					}
					// Read slowly so the repack happens meanwhile.
					got, err := io.ReadAll(io.LimitReader(rc, 1<<20))
					_ = rc.Close()
					if err != nil || size != int64(len(blobs[k])) || !bytes.Equal(got, blobs[k]) {
						fail("blob read: %v", err)
						return
					}
				case 2:
					if _, n, err := r.ObjectHeader(ids[k]); err != nil || n != int64(len(blobs[k])) {
						fail("ObjectHeader: %v", err)
						return
					}
				case 3:
					if _, err := r.ResolvePrefix(ids[k].String()[:12]); err != nil && !errors.Is(err, ErrAmbiguous) {
						fail("ResolvePrefix: %v", err)
						return
					}
					for ref, err := range r.Refs("refs/") {
						if err != nil {
							fail("Refs: %v", err)
							return
						}
						if ref.Name == "refs/heads/main" && ref.ID.IsZero() {
							fail("main is zero")
							return
						}
					}
				}
				reads.Add(1)
			}
		}(w)
	}
	// Refresher.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := r.Refresh(); err != nil {
				fail("Refresh: %v", err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// The writer: repack -ad (deletes the packs), new commits, pack-refs, prune.
	for round := 0; round < 12 && failures.Load() == 0; round++ {
		f.write("w.txt", []byte(fmt.Sprint(round)), 0o644)
		f.commit(fmt.Sprintf("w%d", round))
		f.git("tag", fmt.Sprintf("t%d", round))
		switch round % 3 {
		case 0:
			f.git("repack", "-ad")
		case 1:
			f.git("repack", "-adf", "--depth=20", "--window=20")
			f.git("pack-refs", "--all")
		case 2:
			f.git("gc", "--prune=now", "-q")
		}
	}
	close(stop)
	wg.Wait()
	if failures.Load() != 0 {
		t.FailNow()
	}
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d reads during 12 repack rounds", reads.Load())
	compareRefs(t, r, f.root())
	compareObjects(t, r, f.root())
	// Packs that were replaced are unmapped once no reader needs them.
	if n := len(r.snap.Load().packs); n != 1 {
		t.Errorf("%d packs left in the snapshot", n)
	}
}

// A reader blocked on an old pack keeps it mapped until it is closed.
func TestOldPackStaysMappedWhileInUse(t *testing.T) {
	f := newFx(t)
	big := textLines(100000, 1)
	f.write("big", big, 0o644)
	f.commit("one")
	f.git("repack", "-adf")
	r := f.open()
	id := mustID(t, f.git("rev-parse", "HEAD:big"))
	_, rc, err := r.OpenBlob(id)
	if err != nil {
		t.Fatal(err)
	}
	old := r.snap.Load().packs[0]
	buf := make([]byte, 1000)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatal(err)
	}
	f.write("more", []byte("more"), 0o644)
	f.commit("two")
	f.git("repack", "-adf") // deletes the pack that rc reads from
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	if r.snap.Load().packs[0] == old {
		t.Fatal("the pack list did not change")
	}
	if old.data == nil || old.refs.Load() != 1 {
		t.Fatalf("old pack released while in use (refs=%d)", old.refs.Load())
	}
	rest, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(append(buf, rest...), big) {
		t.Fatalf("read after repack: %v", err)
	}
	_ = rc.Close()
	if old.data != nil {
		t.Fatal("old pack still mapped after the last reader closed")
	}
}

// A ref-delta cycle must not loop forever.
func TestRefDeltaCycle(t *testing.T) {
	// Two ref-deltas that are each other's base.
	idA, idB := ObjectID{0x10}, ObjectID{0x20}
	body := append(varint(4), varint(4)...)
	body = append(body, 4, 'a', 'b', 'c', 'd')
	ea := entryBytes(objRefDelta, len(body), idB[:], body)
	eb := entryBytes(objRefDelta, len(body), idA[:], body)
	var offs []uint64
	pk, idx := buildPack([][]byte{ea, eb}, []ObjectID{idA, idB}, &offs)
	g := t.TempDir()
	writeFile(t, g+"/HEAD", "ref: refs/heads/main\n")
	writeFile(t, g+"/refs/.keep", "")
	name := fmt.Sprintf("pack-%x", pk[len(pk)-20:])
	writeFile(t, g+"/objects/pack/"+name+".pack", string(pk))
	writeFile(t, g+"/objects/pack/"+name+".idx", string(idx))
	r, err := Open(g)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, _, err := r.ReadObject(idA, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadObject = %v, want ErrCorrupt", err)
	}
	if _, _, err := r.ObjectHeader(idA); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ObjectHeader = %v, want ErrCorrupt", err)
	}
}
