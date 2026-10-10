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
