// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"testing"
)

func TestCorruptRootRefIsNotFound(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	c := mustID(t, f.commit("one"))
	writeFile(t, f.dir+"/GARBAGE", "this is not a ref\n")
	writeFile(t, f.dir+"/GOOD_REF", c.String()+"\n")
	writeFile(t, f.dir+"/refs/heads/sym", "ref: GARBAGE\n")
	r := f.open()
	if _, err := r.ResolveRef("GARBAGE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ResolveRef(GARBAGE) = %v, want ErrNotFound", err)
	}
	if _, err := r.LookupRef("GARBAGE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LookupRef(GARBAGE) = %v, want ErrNotFound", err)
	}
	if _, err := r.Resolve("GARBAGE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve(GARBAGE) = %v, want ErrNotFound", err)
	}
	if got, err := r.ResolveRef("GOOD_REF"); err != nil || got != c {
		t.Errorf("ResolveRef(GOOD_REF) = %s, %v", got, err)
	}
	// A symbolic ref to a broken pseudo-ref designates nothing; it does not
	// break the listing.
	n := 0
	for ref, err := range r.Refs("refs/heads/") {
		if err != nil {
			t.Fatal(err)
		}
		if ref.Name == "refs/heads/sym" {
			t.Errorf("sym resolved to %s", ref.ID)
		}
		n++
	}
	if n != 1 {
		t.Errorf("%d branches listed, want 1", n)
	}
	// A broken loose ref under refs/ stays corrupt, and HEAD too.
	writeFile(t, f.dir+"/refs/heads/broken", "nonsense\n")
	if _, err := r.ResolveRef("refs/heads/broken"); !errors.Is(err, ErrCorrupt) {
		t.Errorf("ResolveRef(broken) = %v, want ErrCorrupt", err)
	}
	writeFile(t, f.dir+"/HEAD", "nonsense\n")
	if _, err := r.ResolveRef("HEAD"); !errors.Is(err, ErrCorrupt) {
		t.Errorf("ResolveRef(HEAD) = %v, want ErrCorrupt", err)
	}
}

func TestClosedRepoRefs(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	f.commit("one")
	r := f.open()
	_ = r.Close()
	if _, _, err := r.Head(); !errors.Is(err, ErrClosed) {
		t.Errorf("Head = %v", err)
	}
	if _, err := r.ResolveRef("HEAD"); !errors.Is(err, ErrClosed) {
		t.Errorf("ResolveRef = %v", err)
	}
	if _, err := r.LookupRef("refs/heads/main"); !errors.Is(err, ErrClosed) {
		t.Errorf("LookupRef = %v", err)
	}
	if _, err := r.Resolve("main"); !errors.Is(err, ErrClosed) {
		t.Errorf("Resolve = %v", err)
	}
	n := 0
	for _, err := range r.Refs("") {
		n++
		if !errors.Is(err, ErrClosed) {
			t.Errorf("Refs = %v", err)
		}
	}
	if n != 1 {
		t.Errorf("Refs yielded %d times, want one error", n)
	}
}
