// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"math"
	"os"
	"testing"
)

func TestWithMaxObjectSizeClamped(t *testing.T) {
	var o options
	WithMaxObjectSize(math.MaxInt64)(&o)
	if o.maxObject != math.MaxInt {
		t.Errorf("maxObject = %d, want %d", o.maxObject, math.MaxInt)
	}
	WithMaxObjectSize(1 << 20)(&o)
	if o.maxObject != 1<<20 {
		t.Errorf("maxObject = %d", o.maxObject)
	}
	WithMaxObjectSize(-5)(&o)
	if o.maxObject != 1<<20 {
		t.Errorf("a negative bound changed it to %d", o.maxObject)
	}
}

func TestReplaceDirErrorsAreReturned(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	f.commit("one")
	dir := f.dir + "/refs/replace"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := Open(f.work); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Open with an unreadable refs/replace = %v, want a permission error", err)
	}
	// Absent, or not a directory: nothing to refuse.
	_ = os.Chmod(dir, 0o755)
	_ = os.Remove(dir)
	if err := checkLayout(f.dir); err != nil {
		t.Errorf("no refs/replace: %v", err)
	}
	writeFile(t, dir, "a file")
	if err := checkLayout(f.dir); err != nil {
		t.Errorf("refs/replace is a file: %v", err)
	}
}
