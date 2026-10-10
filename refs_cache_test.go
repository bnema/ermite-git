// SPDX-License-Identifier: MIT

package git

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// writePackedRefs writes n branches, all on id, to packed-refs, in the format
// of "git pack-refs --all".
func writePackedRefs(t testing.TB, dir string, id ObjectID, n int) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("# pack-refs with: peeled fully-peeled sorted \n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "%s refs/heads/b%05d\n", id, i)
	}
	if err := os.WriteFile(dir+"/packed-refs", []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Right after packed-refs was rewritten its stat data cannot be trusted, but
// listing the refs must still parse it once, not once per ref.
func TestRefsRacyPackedRefsParsedOnce(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	id := mustID(t, f.commit("one"))
	const n = 5000
	writePackedRefs(t, f.dir, id, n)
	r := f.open()
	first, err := r.loadPacked(false)
	if err != nil {
		t.Fatal(err)
	}
	if time.Now().UnixNano()-first.sig.mtime > int64(racyWindow) {
		t.Skip("slow machine: the file is no longer racy")
	}
	start := time.Now()
	count := 0
	for ref, err := range r.Refs("refs/heads/") {
		if err != nil {
			t.Fatal(err)
		}
		if ref.ID != id {
			t.Fatalf("%s = %s", ref.Name, ref.ID)
		}
		count++
	}
	took := time.Since(start)
	if count != n+1 { // plus the branch of the fixture
		t.Fatalf("%d refs, want %d", count, n+1)
	}
	again, err := r.loadPacked(false)
	if err != nil {
		t.Fatal(err)
	}
	if &again.refs[0] != &first.refs[0] {
		t.Error("packed-refs was parsed again although its content did not change")
	}
	// Before the fix every ref re-read and re-parsed the file: 0.7 s and more.
	if !raceEnabled && took > 250*time.Millisecond {
		t.Errorf("Refs over %d packed refs took %v right after a rewrite", n, took)
	}
	// A real change inside the racy window is seen.
	writePackedRefs(t, f.dir, id, n-1)
	changed, err := r.loadPacked(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed.refs) != n-1 {
		t.Errorf("rewritten packed-refs has %d refs, want %d", len(changed.refs), n-1)
	}
}

func TestLoadPackedConcurrentShareOneParse(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	id := mustID(t, f.commit("one"))
	writePackedRefs(t, f.dir, id, 2000)
	r := f.open()
	base, err := r.loadPacked(false)
	if err != nil {
		t.Fatal(err)
	}
	writePackedRefs(t, f.dir, id, 1999) // a different size
	res := make(chan *packedRefs, 16)
	for range 16 {
		go func() {
			p, err := r.loadPacked(false)
			if err != nil {
				t.Error(err)
			}
			res <- p
		}()
	}
	var got *packedRefs
	for range 16 {
		p := <-res
		if p == nil {
			continue
		}
		if len(p.refs) != 1999 || p == base {
			t.Fatalf("stale packed-refs: %d refs", len(p.refs))
		}
		if got != nil && &p.refs[0] != &got.refs[0] {
			t.Fatal("concurrent callers parsed the file more than once")
		}
		got = p
	}
}

// The racy rule runs from the time the file was read, not from now.
func TestFileSigRacy(t *testing.T) {
	mtime := time.Now().Add(-time.Hour).UnixNano()
	w := int64(racyWindow)
	cases := []struct {
		name string
		read int64 // relative to mtime
		same bool
	}{
		{"read just after the write", 1, false},
		{"read inside the window", w / 2, false},
		{"read at the end of the window", w, false},
		{"read after the window", w + 1, true},
	}
	for _, c := range cases {
		a := fileSig{exists: true, size: 10, mtime: mtime, ino: 7, read: mtime + c.read}
		b := a
		b.read = time.Now().UnixNano() // when the new stat was taken: irrelevant
		// The check happens an hour after the read: the old code, which
		// measured from "now", trusted every one of these.
		if got := a.same(b); got != c.same {
			t.Errorf("%s: same = %v, want %v", c.name, got, c.same)
		}
	}
	a := fileSig{exists: true, size: 10, mtime: mtime, read: mtime + 10*w}
	for _, b := range []fileSig{
		{exists: true, size: 11, mtime: mtime},
		{exists: true, size: 10, mtime: mtime + 1},
		{exists: true, size: 10, mtime: mtime, ino: 1},
		{},
	} {
		if a.same(b) {
			t.Errorf("%+v taken for %+v", b, a)
		}
	}
	if gone := (fileSig{read: 1}); !gone.same(fileSig{read: 99}) {
		t.Error("a missing file is not the same as a missing file")
	}
	// A recorded signature carries the time of the read.
	f := newFx(t)
	writeFile(t, f.dir+"/probe", "x")
	before := time.Now().UnixNano()
	sig, err := statSig(f.dir + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	if sig.read < before || sig.read > time.Now().UnixNano() {
		t.Errorf("read = %d, want a time between %d and now", sig.read, before)
	}
}

// A pack directory that was racy when the list was built is trusted again
// once a reload has seen it unchanged after the window.
func TestPackDirRacyUntilReread(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	f.commit("one")
	f.git("repack", "-adq")
	r := f.open()
	s := r.snap.Load()
	// Pretend the directory was modified right before the list was read.
	r.mu.Lock()
	s.dirSig.mtime = s.dirSig.read - int64(time.Millisecond)
	r.mu.Unlock()
	if s.dirSig.same(s.dirSig) {
		t.Fatal("a signature read 1ms after the change must not be trusted")
	}
	before := s.dirSig.read
	// The stat data is the real one, but the stored mtime was faked: the
	// reload sees a different directory and installs a new snapshot, which
	// carries the time of its own read.
	if _, err := r.reloadPacks(false); err != nil {
		t.Fatal(err)
	}
	if got := r.snap.Load().dirSig.read; got <= before {
		t.Errorf("dirSig.read = %d, was %d: not refreshed", got, before)
	}
}

// BenchmarkRefsPackedRefs lists 5000 packed refs with a steady packed-refs
// and right after it was rewritten (inside the racy window).
func BenchmarkRefsPackedRefs(b *testing.B) {
	requireGit(b)
	dir := b.TempDir()
	runGit(b, dir, nil, nil, "init", "-q")
	runGit(b, dir, nil, nil, "commit", "-q", "--allow-empty", "-m", "x")
	id := mustID(b, strings.TrimSpace(string(runGit(b, dir, nil, nil, "rev-parse", "HEAD"))))
	const n = 5000
	writePackedRefs(b, dir+"/.git", id, n)
	r, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	list := func(b *testing.B) {
		count := 0
		for _, err := range r.Refs("refs/heads/") {
			if err != nil {
				b.Fatal(err)
			}
			count++
		}
		if count != n+1 {
			b.Fatalf("%d refs", count)
		}
	}
	for _, c := range []struct {
		name string
		age  time.Duration
	}{{"steady", time.Hour}, {"just-rewritten", 0}} {
		b.Run(c.name, func(b *testing.B) {
			mt := time.Now().Add(-c.age)
			if err := os.Chtimes(dir+"/.git/packed-refs", mt, mt); err != nil {
				b.Fatal(err)
			}
			if _, err := r.loadPacked(true); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				list(b)
			}
		})
	}
}
