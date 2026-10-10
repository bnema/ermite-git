// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"testing"
)

func TestParsePathSpec(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		bloom bool
		bad   bool
	}{
		{"a/b/c", "a/b/c", true, false},
		{"a/b/", "a/b", true, false},
		{"a", "a", true, false},
		{"a//b", "a/b", false, false},
		{"a/b//", "a/b", false, false},
		{"./a", "a", false, false},
		{"a/./b", "a/b", false, false},
		{"a/b/../c", "a/c", false, false},
		{".", "", false, false},
		{"./", "", false, false},
		{"", "", false, false},
		{"a/..", "", false, false},
		{"../a", "", false, true},
		{"/a", "", false, true},
		{"a\x00b", "", false, true},
	}
	for _, c := range cases {
		s, err := parsePathSpec(c.in)
		if c.bad {
			if !errors.Is(err, ErrInvalidPath) {
				t.Errorf("%q: err = %v, want ErrInvalidPath", c.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if s.String() != c.want || s.bloom != c.bloom {
			t.Errorf("%q: got %q bloom=%v, want %q bloom=%v", c.in, s.String(), s.bloom, c.want, c.bloom)
		}
	}
}

func TestEmptyTreeID(t *testing.T) {
	want := "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	if got := emptyTreeID.String(); got != want {
		t.Fatalf("emptyTreeID = %s", got)
	}
}

// TestPathDiffer checks same and exists against git rev-parse on trees that
// turn files into directories and back.
func TestPathDiffer(t *testing.T) {
	f := newFx(t)
	var trees []ObjectID
	step := func(setup func()) {
		setup()
		f.commit("c")
		trees = append(trees, mustID(t, f.git("rev-parse", "HEAD^{tree}")))
	}
	step(func() { f.write("a", []byte("file"), 0o644); f.write("d/x", []byte("1"), 0o644) })
	step(func() { f.write("a", []byte("file2"), 0o644) })
	step(func() { f.git("rm", "-q", "a"); f.write("a/b", []byte("1"), 0o644) })
	step(func() { f.write("a/b", []byte("2"), 0o644); f.write("d/y", []byte("1"), 0o644) })
	step(func() { f.write("a/b", []byte("2"), 0o755) })
	r := f.open()
	for _, p := range []string{"a", "a/b", "a/b/c", "d", "d/x", "d/y", "zz", ""} {
		spec, err := parsePathSpec(p)
		if err != nil {
			t.Fatal(err)
		}
		d := newPathDiffer(r, spec)
		for i := range trees {
			for j := range trees {
				got, err := d.same(trees[i], trees[j])
				if err != nil {
					t.Fatal(err)
				}
				want := f.git("diff-tree", "-r", "--name-only", trees[i].String(), trees[j].String(), "--", orDot(p)) == ""
				if got != want {
					t.Errorf("same(%d,%d,%q) = %v, git says %v", i, j, p, got, want)
				}
			}
			ex, err := d.exists(trees[i])
			if err != nil {
				t.Fatal(err)
			}
			want := f.git("ls-tree", "-r", "--name-only", trees[i].String(), "--", orDot(p)) != ""
			if p == "" {
				want = true
			}
			if ex != want {
				t.Errorf("exists(%d,%q) = %v, git says %v", i, p, ex, want)
			}
		}
	}
}

func orDot(p string) string {
	if p == "" {
		return "."
	}
	return p
}
