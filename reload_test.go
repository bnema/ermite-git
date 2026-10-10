// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"strings"
	"testing"
)

func TestReloadErrorIsNotNotFound(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	c := mustID(t, f.commit("one"))
	r := f.open()
	boom := errors.New("pack directory is being rewritten")
	reloadPacksHook = func(*Repo) (bool, error) { return false, boom }
	t.Cleanup(func() { reloadPacksHook = nil })

	missing := mustID(t, strings.Repeat("ab", 20))
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, boom) || errors.Is(err, ErrNotFound) {
			t.Errorf("%s = %v, want the reload error", name, err)
		}
	}
	_, _, err := r.ReadObject(missing, nil)
	check("ReadObject", err)
	_, _, err = r.ObjectHeader(missing)
	check("ObjectHeader", err)
	_, _, err = r.OpenBlob(missing)
	check("OpenBlob", err)
	_, err = r.ResolvePrefix("abab")
	check("ResolvePrefix", err)
	_, err = r.ResolveCommitPrefix("abab")
	check("ResolveCommitPrefix", err)
	ok, err := r.HasObject(missing)
	check("HasObject", err)
	if ok {
		t.Error("HasObject reports an object that does not exist")
	}
	_, err = r.Resolve(missing.String())
	check("Resolve", err)
	// Objects that are found never need the reload.
	if ok, err := r.HasObject(c); !ok || err != nil {
		t.Errorf("HasObject(existing) = %v, %v", ok, err)
	}
	// Without the hook a missing object is a plain ErrNotFound.
	reloadPacksHook = nil
	if _, _, err := r.ReadObject(missing, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadObject(missing) = %v", err)
	}
	if ok, err := r.HasObject(missing); ok || err != nil {
		t.Errorf("HasObject(missing) = %v, %v", ok, err)
	}
}

func TestResolveCommitPrefixReturnsErrors(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	f.commit("one")
	// A tag whose content cannot be parsed: skipping it would turn a broken
	// repository into "not found".
	bad := mustID(t, f.gitIn([]byte("object zz\ntype commit\ntag t\ntagger T <t@x> 1 +0000\n\nmsg\n"), "hash-object", "-t", "tag", "--literally", "-w", "--stdin"))
	// A tag of a tree is merely not a commit.
	tree := f.git("rev-parse", "HEAD^{tree}")
	ofTree := mustID(t, f.gitIn([]byte("object "+tree+"\ntype tree\ntag t\ntagger T <t@x> 1 +0000\n\nmsg\n"), "hash-object", "-t", "tag", "-w", "--stdin"))
	r := f.open()
	if _, err := r.ResolveCommitPrefix(bad.String()[:10]); !errors.Is(err, ErrCorrupt) {
		t.Errorf("broken tag: %v, want ErrCorrupt", err)
	}
	if _, err := r.ResolveCommitPrefix(ofTree.String()[:10]); !errors.Is(err, ErrNotFound) {
		t.Errorf("tag of a tree: %v, want ErrNotFound", err)
	}
	if got, err := r.ResolvePrefix(bad.String()[:10]); err != nil || got != bad {
		t.Errorf("ResolvePrefix = %s, %v", got, err)
	}
}
