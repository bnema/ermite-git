// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"os"
	"testing"
	"time"
)

// age sets the modification time of the given files of the Git directory to
// an hour ago (the missing ones are skipped).
func ageFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	for _, n := range names {
		if err := os.Chtimes(dir+"/"+n, old, old); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// An unchanged repository costs Refresh no allocation once the stat data of
// what it looks at is older than the racy window.
func TestRefreshUnchangedAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not stable under the race detector")
	}
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	f.commit("one")
	f.git("repack", "-adq")
	f.git("pack-refs", "--all")
	f.git("commit-graph", "write", "--reachable", "--split")
	if err := os.MkdirAll(f.dir+"/refs/replace", 0o755); err != nil {
		t.Fatal(err)
	}
	ageFiles(t, f.dir, "packed-refs", "objects/pack", "objects/info/commit-graph", "objects/info/commit-graphs/commit-graph-chain", "refs/replace")
	r := f.open()
	for range 3 { // records the old stat data
		if err := r.Refresh(); err != nil {
			t.Fatal(err)
		}
	}
	snap, graph := r.snap.Load(), r.graph.Load()
	got := testing.AllocsPerRun(100, func() {
		if err := r.Refresh(); err != nil {
			t.Fatal(err)
		}
	})
	if got != 0 {
		t.Errorf("an unchanged Refresh allocates %v, want 0", got)
	}
	if r.snap.Load() != snap || r.graph.Load() != graph {
		t.Error("an unchanged Refresh replaced the pack list or the graph")
	}
}

// refs/replace is only listed again when its stat data moved, and a replace
// ref added later is still refused, also right after the directory was
// listed (inside the racy window).
func TestRefreshSeesReplaceRefs(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	c := f.commit("one")
	if err := os.MkdirAll(f.dir+"/refs/replace", 0o755); err != nil {
		t.Fatal(err)
	}
	r := f.open() // an empty refs/replace is fine
	for range 3 {
		if err := r.Refresh(); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, f.dir+"/refs/replace/"+c, c+"\n")
	if err := r.Refresh(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Refresh after a replace ref = %v, want ErrUnsupported", err)
	}
	// And the same once the empty directory is old enough to be trusted.
	_ = os.Remove(f.dir + "/refs/replace/" + c)
	ageFiles(t, f.dir, "refs/replace")
	for range 3 {
		if err := r.Refresh(); err != nil {
			t.Fatal(err)
		}
	}
	if r.paths.replaceSig.read-r.paths.replaceSig.mtime <= int64(racyWindow) {
		t.Fatal("the empty directory should have been recorded as trusted")
	}
	writeFile(t, f.dir+"/refs/replace/"+c, c+"\n")
	if err := r.Refresh(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Refresh after a replace ref in a trusted directory = %v, want ErrUnsupported", err)
	}
	// Removing the directory is fine again.
	_ = os.Remove(f.dir + "/refs/replace/" + c)
	_ = os.Remove(f.dir + "/refs/replace")
	if err := r.Refresh(); err != nil {
		t.Fatalf("Refresh with no refs/replace: %v", err)
	}
}

// The other unsupported layouts are noticed at each Refresh.
func TestRefreshSeesUnsupportedLayouts(t *testing.T) {
	for _, name := range []string{"shallow", "info/grafts", "commondir", "objects/info/alternates"} {
		f := newFx(t)
		f.write("a", []byte("a"), 0o644)
		f.commit("one")
		r := f.open()
		if err := r.Refresh(); err != nil {
			t.Fatal(err)
		}
		_ = os.MkdirAll(f.dir+"/"+name[:max(0, len(name)-len(baseName(name)))], 0o755)
		writeFile(t, f.dir+"/"+name, "x\n")
		if err := r.Refresh(); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: Refresh = %v, want ErrUnsupported", name, err)
		}
	}
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

func TestStatPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/f", "hello")
	f := newStatPath(dir + "/f")
	st, err := f.stat(false)
	if err != nil || !st.exists || st.isDir || st.size != 5 || st.ino == 0 {
		t.Fatalf("stat file = %+v, %v", st, err)
	}
	fi, _ := os.Stat(dir + "/f")
	if st.mtime != fi.ModTime().UnixNano() || st.ino != fileIno(fi) {
		t.Errorf("mtime %d ino %d, os.Stat says %d %d", st.mtime, st.ino, fi.ModTime().UnixNano(), fileIno(fi))
	}
	d := newStatPath(dir)
	if st, err := d.stat(false); err != nil || !st.exists || !st.isDir {
		t.Fatalf("stat dir = %+v, %v", st, err)
	}
	_ = os.Symlink(dir+"/f", dir+"/link")
	l := newStatPath(dir + "/link")
	if st, _ := l.stat(false); !st.exists || st.isDir || st.size != 5 {
		t.Errorf("stat through a link = %+v", st)
	}
	if st, _ := l.stat(true); !st.exists || st.size == 5 {
		t.Errorf("lstat of a link = %+v, want the link itself", st)
	}
	_ = os.Symlink(dir+"/gone", dir+"/dangling")
	dl := newStatPath(dir + "/dangling")
	if st, err := dl.stat(false); err != nil || st.exists {
		t.Errorf("stat of a dangling link = %+v, %v", st, err)
	}
	if st, err := dl.stat(true); err != nil || !st.exists {
		t.Errorf("lstat of a dangling link = %+v, %v", st, err)
	}
	// Absent, and a file where a directory is expected: not errors.
	for _, p := range []string{dir + "/missing", dir + "/f/below", dir + "/missing/below"} {
		sp := newStatPath(p)
		if st, err := sp.stat(false); err != nil || st.exists {
			t.Errorf("stat(%s) = %+v, %v, want a quiet absence", p, st, err)
		}
	}
	// A real failure is reported with the path.
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'x'
	}
	sp := newStatPath(dir + "/" + string(long))
	if _, err := sp.stat(false); err == nil {
		t.Error("a name that is too long should be an error")
	} else {
		var pe *os.PathError
		if !errors.As(err, &pe) {
			t.Errorf("error %v is not a *os.PathError", err)
		}
	}
	// sig matches statSig.
	want, _ := statSig(dir + "/f")
	got, _ := f.sig()
	if !got.sameStat(want) {
		t.Errorf("sig = %+v, statSig = %+v", got, want)
	}
	if gone := newStatPath(dir + "/missing"); true {
		if s, err := gone.sig(); err != nil || s.exists || s.read == 0 {
			t.Errorf("sig of a missing file = %+v, %v", s, err)
		}
	}
}
