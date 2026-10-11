// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"os"
	"testing"
	"time"
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

func TestLookupRefID(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	head := mustID(t, f.commit("one"))
	f.git("tag", "light")
	f.git("update-ref", "refs/heads/packed", head.String())
	f.git("pack-refs", "--all")
	f.git("update-ref", "refs/heads/loose", head.String())
	f.git("symbolic-ref", "refs/heads/sym", "refs/heads/loose")
	// packed-refs written just now is inside the racy window and is parsed
	// again at each lookup; the steady state is the one to measure.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(f.dir+"/packed-refs", old, old); err != nil {
		t.Fatal(err)
	}
	r := f.open()
	for _, name := range []string{"HEAD", "refs/heads/loose", "refs/heads/packed", "refs/tags/light", "refs/heads/sym"} {
		id, ok, err := r.LookupRefID(name)
		if err != nil || !ok || id != head {
			t.Errorf("LookupRefID(%q) = %v, %v, %v", name, id, ok, err)
		}
		if want, err := r.ResolveRef(name); err != nil || want != id {
			t.Errorf("ResolveRef(%q) = %v, %v", name, want, err)
		}
	}
	// A miss is not an error, whatever the reason the name does not exist:
	// no such file, a directory in its place, a file where a directory is
	// expected, an invalid name, a name outside refs/.
	for _, name := range []string{"refs/heads/none", "refs/heads", "refs/heads/loose/under", "refs/tags/none", "refs/heads/a..b", "../config", "", "refs/heads/x.lock", "orig_head"} {
		id, ok, err := r.LookupRefID(name)
		if err != nil || ok || !id.IsZero() {
			t.Errorf("LookupRefID(%q) = %v, %v, %v, want a quiet miss", name, id, ok, err)
		}
		if _, err := r.ResolveRef(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("ResolveRef(%q) = %v, want ErrNotFound", name, err)
		}
	}
	if raceEnabled {
		return
	}
	for _, name := range []string{"refs/heads/none", "refs/tags/none", "refs/remotes/origin/none"} {
		if got := testing.AllocsPerRun(50, func() { _, _, _ = r.LookupRefID(name) }); got != 0 {
			t.Errorf("LookupRefID(%q) allocates %v on a miss, want 0", name, got)
		}
	}
	if got := testing.AllocsPerRun(50, func() { _, _, _ = r.LookupRefID("refs/heads/loose") }); got != 0 {
		t.Errorf("LookupRefID of a loose ref allocates %v, want 0", got)
	}
	if got := testing.AllocsPerRun(50, func() { _, _, _ = r.LookupRefID("refs/heads/packed") }); got != 0 {
		t.Errorf("LookupRefID of a packed ref allocates %v, want 0", got)
	}
	_ = r.Close()
	if _, _, err := r.LookupRefID("refs/heads/loose"); !errors.Is(err, ErrClosed) {
		t.Errorf("closed: %v", err)
	}
}
