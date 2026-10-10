// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigChecks(t *testing.T) {
	ok := []string{
		"",
		"[core]\n\trepositoryformatversion = 0\n\tbare = true\n",
		"[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectformat = sha1\n\trefstorage = files\n",
		"[core]\n\trepositoryformatversion = 1\n[extensions]\n\tObjectFormat = SHA1 # comment\n",
		"[core]\n\trepositoryformatversion = 1\n[extensions]\n\tnoop = true\n\tworktreeConfig = true\n",
		"[remote \"origin\"]\n\turl = x\n[extensions-lookalike]\n\tfoo = bar\n",
		"# comment\n; comment\n[core]\nrepositoryformatversion=0\n",
		"[branch \"extensions\"]\n\tobjectformat = sha256\n",
	}
	for _, c := range ok {
		if err := checkConfigData([]byte(c)); err != nil {
			t.Errorf("config %q: %v", c, err)
		}
	}
	bad := map[string]string{
		"[core]\n\trepositoryformatversion = 2\n":                                         "repositoryformatversion",
		"[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectformat = sha256\n":  "objectformat",
		"[core]\n\trepositoryformatversion = 1\n[extensions]\n\trefstorage = reftable\n":  "refstorage",
		"[core]\n\trepositoryformatversion = 1\n[extensions]\n\tpartialclone = origin\n":  "partialclone",
		"[core]\n\trepositoryformatversion = 1\n[extensions]\n\tcompatobjectformat = x\n": "compatobjectformat",
		"[core]\n\trepositoryformatversion = 1\n[extensions]\n\tfutureThing = 1\n":        "futurething",
	}
	for c, want := range bad {
		err := checkConfigData([]byte(c))
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), want) {
			t.Errorf("config %q: %v, want ErrUnsupported mentioning %q", c, err, want)
		}
	}
	if err := checkConfigData([]byte("[core\n")); !errors.Is(err, ErrCorrupt) {
		t.Errorf("broken section: %v", err)
	}
}

func TestOpenUnsupported(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *fx)
		want  string
	}{
		{"sha256", func(t *testing.T, f *fx) {
			f.git("config", "core.repositoryformatversion", "1")
			f.git("config", "extensions.objectformat", "sha256")
		}, "objectformat"},
		{"reftable", func(t *testing.T, f *fx) {
			f.git("config", "core.repositoryformatversion", "1")
			f.git("config", "extensions.refstorage", "reftable")
		}, "refstorage"},
		{"extension", func(t *testing.T, f *fx) {
			f.git("config", "core.repositoryformatversion", "1")
			f.git("config", "extensions.partialclone", "origin")
		}, "partialclone"},
		{"version", func(t *testing.T, f *fx) { f.git("config", "core.repositoryformatversion", "3") }, "formatversion"},
		{"shallow", func(t *testing.T, f *fx) {
			writeFile(t, f.dir+"/shallow", f.git("rev-parse", "HEAD")+"\n")
		}, "shallow"},
		{"grafts", func(t *testing.T, f *fx) {
			writeFile(t, f.dir+"/info/grafts", f.git("rev-parse", "HEAD")+"\n")
		}, "grafts"},
		{"replace", func(t *testing.T, f *fx) {
			old := f.git("rev-parse", "HEAD")
			f.write("b", []byte("b"), 0o644)
			f.commit("two")
			f.git("replace", old, "HEAD")
		}, "replace"},
		{"replace-packed", func(t *testing.T, f *fx) {
			old := f.git("rev-parse", "HEAD")
			f.write("b", []byte("b"), 0o644)
			f.commit("two")
			f.git("replace", old, "HEAD")
			f.git("pack-refs", "--all")
		}, "replace"},
		{"alternates", func(t *testing.T, f *fx) {
			writeFile(t, f.dir+"/objects/info/alternates", t.TempDir()+"\n")
		}, "alternate"},
		{"promisor", func(t *testing.T, f *fx) {
			f.git("repack", "-ad")
			writeFile(t, firstPackIdx(t, f)[:len(firstPackIdx(t, f))-4]+".promisor", "")
		}, "partial clone"},
		{"midx-with-missing-idx", func(t *testing.T, f *fx) {
			f.git("repack", "-ad")
			f.git("multi-pack-index", "write")
			if err := os.Remove(firstPackIdx(t, f)); err != nil {
				t.Fatal(err)
			}
		}, "multi-pack-index"},
		{"worktree-gitfile", nil, "gitfile"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t)
			f.write("a", []byte("a"), 0o644)
			f.commit("one")
			path := f.work
			if c.setup != nil {
				c.setup(t, f)
			} else {
				path = t.TempDir()
				writeFile(t, path+"/.git", "gitdir: /elsewhere\n")
			}
			_, err := Open(path)
			if !errors.Is(err, ErrUnsupported) || !strings.Contains(strings.ToLower(err.Error()), c.want) {
				t.Fatalf("Open = %v, want ErrUnsupported mentioning %q", err, c.want)
			}
		})
	}
}

func TestEmptyReplaceDirIsFine(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	f.commit("one")
	if err := os.MkdirAll(f.dir+"/refs/replace", 0o755); err != nil {
		t.Fatal(err)
	}
	f.open()
}

func TestMidxIgnoredWhenIdxFilesPresent(t *testing.T) {
	f := newFx(t)
	mixedFixture(t, f)
	f.git("repack", "-ad")
	f.git("multi-pack-index", "write")
	if !exists(f.dir + "/objects/pack/multi-pack-index") {
		t.Fatal("fixture has no multi-pack-index")
	}
	r := f.open()
	checkAll(t, r, f.root())
	// Two packs and a midx covering both.
	f.write("more.txt", []byte("more\n"), 0o644)
	f.commit("more")
	f.git("repack", "-d")
	f.git("multi-pack-index", "write")
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	checkAll(t, r, f.root())
}

func TestIncrementalMidxUnsupported(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	f.commit("one")
	f.git("repack", "-ad")
	if out, err := f.gitErr("multi-pack-index", "write", "--incremental"); err != nil {
		t.Skipf("this git cannot write an incremental multi-pack-index: %v %s", err, out)
	}
	if _, err := Open(f.work); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open = %v, want ErrUnsupported", err)
	}
}

func TestRefreshSeesChanges(t *testing.T) {
	f := newBareFx(t)
	src := newFx(t)
	src.write("a", []byte("a\n"), 0o644)
	c1 := src.commit("one")
	f.git("fetch", "-q", src.dir, "+refs/heads/main:refs/heads/main")
	r := f.open()
	if id, err := r.ResolveRef("refs/heads/main"); err != nil || id.String() != c1 {
		t.Fatalf("main = %s %v", id, err)
	}
	// A push: new objects (a new pack), updated ref, new ref and tag.
	src.write("b", []byte("b\n"), 0o644)
	c2 := src.commit("two")
	src.git("tag", "-a", "-m", "v1", "v1")
	f.git("fetch", "-q", "--tags", src.dir, "+refs/heads/main:refs/heads/main", "+refs/heads/main:refs/heads/second")
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"refs/heads/main", "refs/heads/second"} {
		if id, err := r.ResolveRef(name); err != nil || id.String() != c2 {
			t.Fatalf("%s = %s %v", name, id, err)
		}
	}
	var c Commit
	if err := r.ReadCommit(mustID(t, c2), &c); err != nil {
		t.Fatal(err)
	}
	compareRefs(t, r, f.root())
	compareObjects(t, r, f.root())

	// pack-refs then a repack that deletes the old packs.
	f.git("pack-refs", "--all")
	f.git("repack", "-adf")
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	compareRefs(t, r, f.root())
	compareObjects(t, r, f.root())
	if n := len(r.snap.Load().packs); n != 1 {
		t.Fatalf("%d packs after repack -ad, want 1", n)
	}
}

// An object that arrives in a new pack is found without Refresh.
func TestMissingObjectRescansPacks(t *testing.T) {
	f := newBareFx(t)
	src := newFx(t)
	src.write("a", []byte("a\n"), 0o644)
	src.commit("one")
	f.git("fetch", "-q", src.dir, "+refs/heads/main:refs/heads/main")
	r := f.open()
	src.write("b", []byte("b\n"), 0o644)
	c2 := src.commit("two")
	f.git("fetch", "-q", src.dir, "+refs/heads/main:refs/heads/main")
	f.git("repack", "-adf")
	var c Commit
	if err := r.ReadCommit(mustID(t, c2), &c); err != nil {
		t.Fatalf("new object: %v", err)
	}
	if _, err := r.ResolvePrefix(c2[:10]); err != nil {
		t.Fatalf("new prefix: %v", err)
	}
}

func TestClosedRepo(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	c := f.commit("one")
	r := f.open()
	_ = r.Close()
	_ = r.Close()
	if _, _, err := r.ReadObject(mustID(t, c), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReadObject after Close = %v", err)
	}
	if err := r.Refresh(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Refresh after Close = %v", err)
	}
}

func TestRefreshDetectsNewUnsupported(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	f.commit("one")
	r := f.open()
	writeFile(t, f.dir+"/shallow", f.git("rev-parse", "HEAD")+"\n")
	if err := r.Refresh(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Refresh = %v, want ErrUnsupported", err)
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
