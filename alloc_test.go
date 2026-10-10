// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"fmt"
	"testing"
)

func TestAllocTreeIterationPerEntry(t *testing.T) {
	var tree []byte
	const n = 1000
	for i := 0; i < n; i++ {
		tree = append(tree, fmt.Sprintf("100644 file-%04d.txt\x00", i)...)
		tree = append(tree, bytes.Repeat([]byte{byte(i)}, 20)...)
	}
	count := 0
	cb := func(e Entry) bool { count++; return true }
	allocs := testing.AllocsPerRun(50, func() {
		if err := ParseTree(tree, cb); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("ParseTree allocates %v per tree of %d entries, want 0", allocs, n)
	}
}

func TestAllocReadTreeAndCommit(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under the race detector")
	}
	f := newFx(t)
	for i := 0; i < 200; i++ {
		f.write(fmt.Sprintf("dir/file-%03d.txt", i), []byte{byte(i)}, 0o644)
	}
	c := mustID(t, f.commit("one"))
	tree := mustID(t, f.git("rev-parse", "HEAD:dir"))
	for _, packed := range []bool{false, true} {
		if packed {
			f.git("repack", "-adf")
		}
		r := f.open()
		entries := 0
		cb := func(e Entry) bool { entries++; return true }
		_ = r.ReadTree(tree, cb) // warm up the pools
		got := testing.AllocsPerRun(50, func() {
			if err := r.ReadTree(tree, cb); err != nil {
				t.Fatal(err)
			}
		})
		// A constant cost per tree (file handle, path), not per entry.
		if got > 8 {
			t.Errorf("packed=%v: ReadTree allocates %v for 200 entries", packed, got)
		}
		if got/float64(entries/50) > 0.1 {
			t.Errorf("packed=%v: %v allocs per tree is not 0 per entry", packed, got)
		}
		var commit Commit
		_ = r.ReadCommit(c, &commit)
		got = testing.AllocsPerRun(50, func() {
			if err := r.ReadCommit(c, &commit); err != nil {
				t.Fatal(err)
			}
		})
		limit := 8.0 // loose: path string, *os.File, inflater state
		if packed {
			limit = 1 // the snapshot reference only
		}
		t.Logf("packed=%v: ReadCommit allocs/op = %v", packed, got)
		if got > limit {
			t.Errorf("packed=%v: ReadCommit allocates %v per call after warm-up, want <= %v", packed, got, limit)
		}
	}
	var commit Commit
	raw := []byte("tree " + fmt.Sprintf("%040d", 1) + "\nparent " + fmt.Sprintf("%040d", 2) + "\nauthor A <a@x> 1 +0000\ncommitter B <b@x> 2 +0000\n\nmsg\n")
	_ = ParseCommit(raw, &commit)
	if got := testing.AllocsPerRun(100, func() { _ = ParseCommit(raw, &commit) }); got != 0 {
		t.Errorf("ParseCommit allocates %v on a reused Commit", got)
	}
}

// Packed reads allocate nothing once the pools are warm: the zlib reader
// (flate decompressor and adler32 state) is reused, and so are the buffers.
// The delta cache is on (the default): a deltified object is then a cached
// base plus one small delta. Without it the base is inflated again at each
// read, and compress/flate allocates the link tables of every dynamic block
// with a code longer than 9 bits (see TestAllocColdDynamicBlock).
func TestAllocPackedReads(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under the race detector")
	}
	f := newFx(t)
	for i := 0; i < 30; i++ {
		f.write("f.txt", append(textLines(400, 0), fmt.Sprintf("rev %d\n", i)...), 0o644)
		f.write(fmt.Sprintf("many/file-%03d.txt", i), []byte{byte(i)}, 0o644)
		f.commit(fmt.Sprintf("commit %d", i))
	}
	f.git("repack", "-adf", "--depth=50", "--window=250")
	r := f.open()
	s := r.snap.Load()
	var commit, deltaBlob, deltaTree ObjectID
	for _, p := range s.packs {
		for i := range p.idx.n {
			off, _ := p.idx.offset(i)
			h, err := parseEntryHeader(p.data, off)
			if err != nil {
				t.Fatal(err)
			}
			var id ObjectID
			copy(id[:], p.idx.id(i))
			if !h.isDelta() {
				if h.typ == uint8(TypeCommit) {
					commit = id
				}
				continue
			}
			typ, err := p.chainEnd(off, h)
			if err != nil {
				t.Fatal(err)
			}
			switch typ {
			case TypeBlob:
				deltaBlob = id
			case TypeTree:
				deltaTree = id
			}
		}
	}
	if commit.IsZero() || deltaBlob.IsZero() || deltaTree.IsZero() {
		t.Fatalf("fixture lacks an object: commit %v, deltified blob %v, deltified tree %v", !commit.IsZero(), !deltaBlob.IsZero(), !deltaTree.IsZero())
	}
	for _, c := range []struct {
		name string
		id   ObjectID
	}{{"packed commit", commit}, {"deltified blob", deltaBlob}, {"deltified tree", deltaTree}} {
		var buf []byte
		read := func() {
			_, data, err := r.ReadObject(c.id, buf)
			if err != nil {
				t.Fatal(err)
			}
			buf = data
		}
		read() // warm up
		if got := testing.AllocsPerRun(100, read); got != 0 {
			t.Errorf("%s: ReadObject allocates %v per call after warm-up, want 0", c.name, got)
		}
	}
	var cm Commit
	_ = r.ReadCommit(commit, &cm)
	if got := testing.AllocsPerRun(100, func() { _ = r.ReadCommit(commit, &cm) }); got != 0 {
		t.Errorf("packed commit: ReadCommit allocates %v per call after warm-up, want 0", got)
	}
}

// compress/flate keeps the 9-bit decoding table of each Huffman tree in the
// decompressor, so Reset reuses it. For codes longer than 9 bits it builds
// link tables with make() in huffmanDecoder.init at every dynamic block, and
// Reset clears them (*h = huffmanDecoder{}), so they are never reused. A
// zlibReader therefore allocates per dynamic block with long codes and
// nothing otherwise. This pins the behaviour: the numbers are Go's, not
// ours, and the test fails if a Go release changes them (good news then).
func TestAllocColdDynamicBlock(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under the race detector")
	}
	var z zlibReader
	for _, c := range []struct {
		name string
		data []byte
		zero bool
	}{
		{"short codes (all literals the same)", bytes.Repeat([]byte("ab"), 4000), true},
		{"fixed Huffman block", []byte("tiny"), true},
		{"prose-like text", textLines(400, 0), false},
	} {
		src := zlibBytes(c.data)
		dst := make([]byte, len(c.data))
		var br bytes.Reader
		probe := make([]byte, 1)
		read := func() {
			br.Reset(src)
			if err := z.Reset(&br); err != nil {
				t.Fatal(err)
			}
			if err := inflateExact(&z, dst, probe); err != nil {
				t.Fatal(err)
			}
		}
		read()
		got := testing.AllocsPerRun(50, read)
		t.Logf("%s: %v allocs per stream", c.name, got)
		if c.zero && got != 0 {
			t.Errorf("%s: %v allocs, want 0", c.name, got)
		}
	}
}
