// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
)

// mixedFixture builds a history with all the kinds of content of the plan,
// leaving everything loose.
func mixedFixture(t *testing.T, f *fx) {
	t.Helper()
	f.write("README.md", []byte("# hello\n"), 0o644)
	f.write("bin/tool.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	f.write("data/blob.bin", append([]byte{0, 1, 2, 0xff, 0xfe, 0}, bytes.Repeat([]byte{0xde, 0xad}, 4000)...), 0o644)
	f.write("big.txt", textLines(60000, 0), 0o644) // ~4 MB
	f.write("src/a/b/c.go", []byte("package c\n"), 0o644)
	f.write("src/a/ünï.txt", []byte("unicode name\n"), 0o644)
	if err := os.Symlink("README.md", f.work+"/link"); err != nil {
		t.Fatal(err)
	}
	f.commit("first")
	// A submodule entry (gitlink): a nested repository.
	sub := f.work + "/vendor/sub"
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, sub, nil, nil, "init", "-q")
	if err := os.WriteFile(sub+"/x", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, sub, nil, f.tick(), "add", "x")
	runGit(t, sub, nil, f.tick(), "commit", "-q", "-m", "sub")
	f.commit("add submodule\n\nWith a body.\n")
	f.write("big.txt", textLines(60000, 1), 0o644)
	f.write("README.md", []byte("# hello\nmore\n"), 0o644)
	f.commit("")
	f.git("tag", "light")
	f.git("tag", "-a", "-m", "annotated tag\n\nbody", "annotated")
	f.git("tag", "-a", "-m", "tag of a tag", "nested1", "annotated")
	f.git("tag", "-a", "-m", "tag of a tag of a tag", "nested2", "nested1")
	f.git("tag", "-a", "-m", "tag of a tree", "treetag", "HEAD^{tree}")
	f.git("tag", "-a", "-m", "tag of a blob", "blobtag", "HEAD:README.md")
	f.git("checkout", "-q", "-b", "feature/x")
	f.write("feature.txt", []byte("feature\n"), 0o644)
	f.commit("feature work")
	f.git("checkout", "-q", "main")
	f.write("big.txt", textLines(60000, 2), 0o644)
	f.commit("main moves")
	f.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature/x")

	// Non-UTF-8 message (latin-1) and an extra header (gpgsig, multi-line).
	latin := []byte("caf\xe9 \xff\xfe message\n")
	tree := f.git("rev-parse", "HEAD^{tree}")
	parent := f.git("rev-parse", "HEAD")
	raw := "tree " + tree + "\nparent " + parent + "\nauthor Zoë <z@example.com> 1700100000 +0530\ncommitter Zoë <z@example.com> 1700100001 -0800\nencoding ISO-8859-1\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n abc\n -----END PGP SIGNATURE-----\n\n" + string(latin)
	id := f.gitIn([]byte(raw), "hash-object", "-t", "commit", "-w", "--stdin")
	f.git("update-ref", "refs/heads/latin1", id)
	f.git("symbolic-ref", "refs/heads/alias", "refs/heads/latin1")
}

// compareObjects checks every object of the repository against
// git cat-file --batch.
func compareObjects(t *testing.T, r *Repo, dir string) {
	t.Helper()
	all := runGit(t, dir, nil, []string{"GIT_DIR=" + r.Dir()}, "cat-file", "--batch-all-objects", "--batch")
	br := bufio.NewReader(bytes.NewReader(all))
	count := 0
	var buf []byte
	for {
		hdr, err := br.ReadString('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(hdr)
		if len(f) != 3 {
			t.Fatalf("bad batch header %q", hdr)
		}
		size, _ := strconv.Atoi(f[2])
		want := make([]byte, size+1)
		if _, err := io.ReadFull(br, want); err != nil {
			t.Fatal(err)
		}
		want = want[:size]
		id := mustID(t, f[0])
		typ, got, err := r.ReadObject(id, buf)
		if err != nil {
			t.Fatalf("ReadObject(%s): %v", id, err)
		}
		buf = got
		if typ.String() != f[1] || !bytes.Equal(got, want) {
			t.Fatalf("ReadObject(%s): type %s len %d, want %s len %d", id, typ, len(got), f[1], size)
		}
		htyp, hsize, err := r.ObjectHeader(id)
		if err != nil || htyp != typ || hsize != int64(size) {
			t.Fatalf("ObjectHeader(%s) = %s %d %v, want %s %d", id, htyp, hsize, err, f[1], size)
		}
		if typ == TypeBlob {
			bsize, rc, err := r.OpenBlob(id)
			if err != nil {
				t.Fatalf("OpenBlob(%s): %v", id, err)
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil || bsize != int64(size) || !bytes.Equal(data, want) {
				t.Fatalf("OpenBlob(%s): size %d len %d err %v, want %d", id, bsize, len(data), err, size)
			}
		}
		count++
	}
	if count == 0 {
		t.Fatal("no object compared")
	}
	t.Logf("%d objects match git cat-file", count)
}

func compareRefs(t *testing.T, r *Repo, dir string) {
	t.Helper()
	out := runGit(t, dir, nil, []string{"GIT_DIR=" + r.Dir()}, "for-each-ref", "--format=%(refname) %(objectname) %(*objectname) %(*objecttype)")
	want := lines(string(out))
	var got []string
	for ref, err := range r.Refs("") {
		if err != nil {
			t.Fatal(err)
		}
		peeled := ""
		if typ, _, err := r.ObjectHeader(ref.ID); err != nil {
			t.Fatal(err)
		} else if typ == TypeTag {
			pid, ptyp, err := r.Peel(ref.ID)
			if err != nil {
				t.Fatal(err)
			}
			peeled = pid.String() + " " + ptyp.String()
		}
		got = append(got, strings.TrimRight(ref.Name+" "+ref.ID.String()+" "+peeled, " "))
	}
	for i := range want {
		want[i] = strings.TrimRight(want[i], " ")
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("refs differ\n got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Prefix filters and PeelRef.
	for _, prefix := range []string{"refs/heads/", "refs/tags/", "refs/heads/feature/", "refs/"} {
		wantP := lines(string(runGit(t, dir, nil, []string{"GIT_DIR=" + r.Dir()}, "for-each-ref", "--format=%(refname)", prefix)))
		var gotP []string
		for ref, err := range r.Refs(prefix) {
			if err != nil {
				t.Fatal(err)
			}
			gotP = append(gotP, ref.Name)
		}
		if strings.Join(gotP, ",") != strings.Join(wantP, ",") {
			t.Errorf("Refs(%q) = %v, want %v", prefix, gotP, wantP)
		}
	}
	for ref, err := range r.Refs("refs/") {
		if err != nil {
			t.Fatal(err)
		}
		wantC, gerr := gitOutput(t, dir, r.Dir(), "rev-parse", "--verify", "-q", ref.Name+"^{commit}")
		gotC, err := r.PeelRef(ref)
		if gerr != nil {
			if err == nil {
				t.Errorf("PeelRef(%s) = %s, git finds no commit", ref.Name, gotC)
			}
			continue
		}
		if err != nil || gotC.String() != wantC {
			t.Errorf("PeelRef(%s) = %s, %v; want %s", ref.Name, gotC, err, wantC)
		}
		viaObj, err := r.PeelToCommit(ref.ID)
		if err != nil || viaObj.String() != wantC {
			t.Errorf("PeelToCommit(%s) = %s, %v; want %s", ref.Name, viaObj, err, wantC)
		}
	}
}

func gitOutput(t *testing.T, dir, gitDir string, args ...string) (string, error) {
	t.Helper()
	cmd := execCommand(dir, gitDir, args...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func compareHead(t *testing.T, r *Repo, dir string) {
	t.Helper()
	target, id, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	wantTarget, serr := gitOutput(t, dir, r.Dir(), "symbolic-ref", "-q", "HEAD")
	if serr != nil {
		wantTarget = ""
	}
	if target != wantTarget {
		t.Errorf("Head target = %q, want %q", target, wantTarget)
	}
	wantID, verr := gitOutput(t, dir, r.Dir(), "rev-parse", "-q", "--verify", "HEAD")
	if verr != nil {
		if !id.IsZero() {
			t.Errorf("unborn HEAD id = %s, want zero", id)
		}
		return
	}
	if id.String() != wantID {
		t.Errorf("Head id = %s, want %s", id, wantID)
	}
}

func compareTrees(t *testing.T, r *Repo, dir string) {
	t.Helper()
	revs := lines(string(runGit(t, dir, nil, []string{"GIT_DIR=" + r.Dir()}, "rev-list", "--all")))
	if len(revs) == 0 {
		return
	}
	checked := 0
	for _, rev := range revs {
		id := mustID(t, rev)
		var c Commit
		if err := r.ReadCommit(id, &c); err != nil {
			t.Fatal(err)
		}
		out := runGit(t, dir, nil, []string{"GIT_DIR=" + r.Dir()}, "ls-tree", "-r", "-t", "-l", "-z", "--full-tree", rev)
		var want []string
		for _, rec := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
			if rec != "" {
				want = append(want, rec)
			}
		}
		var got []string
		var rec func(tree ObjectID, prefix string)
		rec = func(tree ObjectID, prefix string) {
			type ent struct {
				e    Entry
				name string
			}
			var ents []ent
			if err := r.ReadTree(tree, func(e Entry) bool {
				ents = append(ents, ent{Entry{Name: nil, Mode: e.Mode, ID: e.ID}, prefix + string(e.Name)})
				return true
			}); err != nil {
				t.Fatal(err)
			}
			for _, en := range ents {
				size := "-"
				if en.e.Mode.Type() == TypeBlob {
					_, n, err := r.ObjectHeader(en.e.ID)
					if err != nil {
						t.Fatalf("size of %s: %v", en.name, err)
					}
					size = strconv.FormatInt(n, 10)
				}
				got = append(got, fmt.Sprintf("%s %s %s %7s\t%s", en.e.Mode, en.e.Mode.Type(), en.e.ID, size, en.name))
				if en.e.Mode.IsTree() {
					rec(en.e.ID, en.name+"/")
				}
			}
		}
		rec(c.Tree, "")
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("tree of %s differs\n got:\n%s\nwant:\n%s", rev, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		checked++
	}
	t.Logf("%d trees match git ls-tree -l", checked)
}

func compareCommits(t *testing.T, r *Repo, dir string) {
	t.Helper()
	revs := lines(string(runGit(t, dir, nil, []string{"GIT_DIR=" + r.Dir()}, "rev-list", "--all")))
	var c Commit
	for _, rev := range revs {
		id := mustID(t, rev)
		if err := r.ReadCommit(id, &c); err != nil {
			t.Fatal(err)
		}
		raw := runGit(t, dir, nil, []string{"GIT_DIR=" + r.Dir()}, "cat-file", "commit", rev)
		if c.Tree.String() != strings.TrimSpace(string(runGit(t, dir, nil, []string{"GIT_DIR=" + r.Dir()}, "rev-parse", rev+"^{tree}"))) {
			t.Errorf("%s: tree %s", rev, c.Tree)
		}
		wantParents := lines(string(runGit(t, dir, nil, []string{"GIT_DIR=" + r.Dir()}, "rev-list", "--parents", "-n", "1", rev)))
		wp := strings.Fields(wantParents[0])[1:]
		var gp []string
		for _, p := range c.Parents {
			gp = append(gp, p.String())
		}
		if strings.Join(gp, " ") != strings.Join(wp, " ") {
			t.Errorf("%s: parents %v, want %v", rev, gp, wp)
		}
		// Author and committer against the raw header lines.
		for _, h := range []struct {
			key string
			s   Signature
		}{{"author ", c.Author}, {"committer ", c.Committer}} {
			var line string
			for _, l := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(l, h.key) {
					line = l[len(h.key):]
					break
				}
			}
			sign := "+"
			off := h.s.Offset
			if off < 0 {
				sign, off = "-", -off
			}
			wantLine := fmt.Sprintf("%s <%s> %d %s%02d%02d", h.s.Name, h.s.Email, h.s.Time, sign, off/3600, off%3600/60)
			if line != wantLine {
				t.Errorf("%s: %s%q, want %q", rev, h.key, wantLine, line)
			}
		}
		_, body, _ := bytes.Cut(raw, []byte("\n\n"))
		if !bytes.Equal(c.Message, body) && (len(body) != 0 || len(c.Message) != 0) {
			t.Errorf("%s: message %q, want %q", rev, c.Message, body)
		}
	}
}

func compareRevParse(t *testing.T, r *Repo, dir string) {
	t.Helper()
	names := []string{"HEAD", "main", "feature/x", "light", "annotated", "nested1", "nested2", "latin1", "alias",
		"refs/heads/main", "refs/tags/light", "heads/main", "tags/annotated", "nosuchref", "config", "../x"}
	for _, n := range names {
		want, werr := gitOutput(t, dir, r.Dir(), "rev-parse", "--verify", "-q", n)
		got, err := r.Resolve(n)
		if werr != nil {
			if err == nil {
				t.Errorf("Resolve(%q) = %s, git fails", n, got)
			} else if !errors.Is(err, ErrNotFound) {
				t.Errorf("Resolve(%q): %v, want ErrNotFound", n, err)
			}
			continue
		}
		if err != nil || got.String() != want {
			t.Errorf("Resolve(%q) = %s, %v; want %s", n, got, err, want)
		}
		wantC, cerr := gitOutput(t, dir, r.Dir(), "rev-parse", "--verify", "-q", n+"^{commit}")
		gotC, err := r.PeelToCommit(got)
		if cerr != nil {
			if err == nil {
				t.Errorf("PeelToCommit(%q) = %s, git fails", n, gotC)
			}
		} else if err != nil || gotC.String() != wantC {
			t.Errorf("PeelToCommit(%q) = %s, %v; want %s", n, gotC, err, wantC)
		}
	}
	// Abbreviated ids: every length from 4 up, for the commits.
	for _, rev := range lines(string(runGit(t, dir, nil, []string{"GIT_DIR=" + r.Dir()}, "rev-list", "--all"))) {
		for _, n := range []int{4, 7, 12, 39, 40} {
			got, err := r.ResolveCommitPrefix(rev[:n])
			if err != nil || got.String() != rev {
				t.Errorf("ResolveCommitPrefix(%s) = %s, %v", rev[:n], got, err)
			}
			got, err = r.ResolvePrefix(strings.ToUpper(rev[:n]))
			if err != nil && !errors.Is(err, ErrAmbiguous) {
				t.Errorf("ResolvePrefix(%s): %v", strings.ToUpper(rev[:n]), err)
			} else if err == nil && got.String() != rev {
				t.Errorf("ResolvePrefix(%s) = %s", rev[:n], got)
			}
		}
	}
	if _, err := r.ResolvePrefix("zzzz"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("ResolvePrefix(zzzz): %v, want ErrInvalidID", err)
	}
	if _, err := r.ResolvePrefix("abc"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("ResolvePrefix(abc): %v, want ErrInvalidID", err)
	}
	if _, err := r.ResolvePrefix("0000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ResolvePrefix(0000): %v, want ErrNotFound", err)
	}
}

func checkAll(t *testing.T, r *Repo, dir string) {
	t.Helper()
	compareObjects(t, r, dir)
	compareRefs(t, r, dir)
	compareHead(t, r, dir)
	compareTrees(t, r, dir)
	compareCommits(t, r, dir)
	compareRevParse(t, r, dir)
}

func TestConformanceLoose(t *testing.T) {
	f := newFx(t)
	mixedFixture(t, f)
	r := f.open()
	checkAll(t, r, f.root())
}

func TestConformancePacked(t *testing.T) {
	f := newFx(t)
	mixedFixture(t, f)
	f.git("repack", "-adf", "--depth=50", "--window=250")
	f.git("pack-refs", "--all")
	r := f.open()
	checkAll(t, r, f.root())
}

func TestConformanceLoosePlusPackedRefsOverride(t *testing.T) {
	f := newFx(t)
	mixedFixture(t, f)
	f.git("repack", "-ad")
	f.git("pack-refs", "--all") // everything packed, loose refs removed
	// New loose objects and a loose ref overriding a packed one.
	f.write("late.txt", []byte("late\n"), 0o644)
	late := f.commit("late commit")
	f.git("update-ref", "refs/heads/feature/x", late)
	f.git("tag", "late-tag")
	f.git("branch", "-D", "alias") // deleted: must disappear although packed
	r := f.open()
	checkAll(t, r, f.root())
	id, err := r.ResolveRef("refs/heads/feature/x")
	if err != nil || id.String() != late {
		t.Fatalf("loose must override packed: %s %v", id, err)
	}
}

func TestConformanceDeepDeltas(t *testing.T) {
	f := newFx(t)
	f.write("big.txt", textLines(3000, 0), 0o644)
	for i := 1; i <= 120; i++ {
		d := textLines(3000, 0)
		d = append(d, []byte(fmt.Sprintf("extra %d\n", i))...)
		d = bytes.Replace(d, []byte("line 5 "), []byte(fmt.Sprintf("line five rev %d ", i)), 1)
		f.write("big.txt", d, 0o644)
		f.write("small/f.txt", []byte(strings.Repeat("abc\n", i)), 0o644)
		f.commit("rev " + strconv.Itoa(i))
	}
	f.git("repack", "-adf", "--depth=50", "--window=250")
	r := f.open()
	checkAll(t, r, f.root())
	// Make sure the fixture really has long delta chains.
	out := f.git("verify-pack", "-v", firstPackIdx(t, f))
	if !strings.Contains(out, "chain length = 50") && !strings.Contains(out, "chain length = 4") {
		t.Logf("chain lengths:\n%s", out[max(0, len(out)-300):])
	}
}

func firstPackIdx(t *testing.T, f *fx) string {
	t.Helper()
	ents, err := os.ReadDir(f.dir + "/objects/pack")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".idx") {
			return f.dir + "/objects/pack/" + e.Name()
		}
	}
	t.Fatal("no pack")
	return ""
}

func TestConformanceRefDelta(t *testing.T) {
	f := newFx(t)
	f.write("big.txt", textLines(2000, 0), 0o644)
	for i := 1; i <= 30; i++ {
		d := append(textLines(2000, 0), []byte(fmt.Sprintf("extra %d\n", i))...)
		f.write("big.txt", d, 0o644)
		f.commit("rev " + strconv.Itoa(i))
	}
	f.git("-c", "repack.useDeltaBaseOffset=false", "repack", "-adf", "--depth=50", "--window=250")
	// Verify the pack really has ref-deltas: entry type 7.
	pack := strings.TrimSuffix(firstPackIdx(t, f), ".idx") + ".pack"
	data, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	r := f.open()
	refDeltas := 0
	for _, p := range r.snap.Load().packs {
		for i := uint32(0); i < p.idx.n; i++ {
			off, _ := p.idx.offset(i)
			h, err := parseEntryHeader(data, off)
			if err != nil {
				t.Fatal(err)
			}
			if h.typ == objRefDelta {
				refDeltas++
			}
		}
	}
	if refDeltas == 0 {
		t.Fatal("fixture has no ref-delta")
	}
	t.Logf("%d ref-deltas", refDeltas)
	checkAll(t, r, f.root())
}

func TestConformanceBareLooseAndPushedPack(t *testing.T) {
	src := newFx(t)
	mixedFixture(t, src)
	bare := newBareFx(t)
	bare.git("fetch", "-q", src.dir, "+refs/*:refs/*")
	r := bare.open()
	checkAll(t, r, bare.root())
}

func TestConformanceUnbornHead(t *testing.T) {
	f := newBareFx(t)
	r := f.open()
	target, id, err := r.Head()
	if err != nil || target != "refs/heads/main" || !id.IsZero() {
		t.Fatalf("Head = %q %s %v", target, id, err)
	}
	for ref, err := range r.Refs("") {
		t.Fatalf("unexpected ref %v %v", ref, err)
	}
	if _, err := r.ResolveRef("HEAD"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveRef(HEAD) = %v, want ErrNotFound", err)
	}
	compareHead(t, r, f.root())
}

func TestDetachedHead(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	c := f.commit("one")
	f.git("checkout", "-q", "--detach")
	r := f.open()
	target, id, err := r.Head()
	if err != nil || target != "" || id.String() != c {
		t.Fatalf("Head = %q %s %v", target, id, err)
	}
	compareHead(t, r, f.root())
}

func TestAmbiguousPrefixes(t *testing.T) {
	for _, layout := range []string{"loose", "packed", "packed-and-loose"} {
		t.Run(layout, func(t *testing.T) {
			f := newFx(t)
			f.write("a", []byte("a"), 0o644)
			f.commit("one")
			da, db := ambiguousPair(t)
			a := f.writeBlob(da)
			if layout != "loose" {
				// -k packs the unreferenced loose blob.
				f.git("repack", "-adkq")
				f.git("prune-packed")
			}
			var b ObjectID
			if layout == "packed" {
				b = f.writeBlob(db)
				f.git("repack", "-adkq")
				f.git("prune-packed")
			} else {
				b = f.writeBlob(db)
			}
			r := f.open()
			pre := a.String()[:4]
			if _, err := r.ResolvePrefix(pre); !errors.Is(err, ErrAmbiguous) {
				t.Fatalf("ResolvePrefix(%s) = %v, want ErrAmbiguous", pre, err)
			}
			if out, err := f.gitErr("rev-parse", "--verify", "-q", pre); err == nil {
				t.Fatalf("git does not find the prefix ambiguous: %s", out)
			}
			// The shortest unambiguous prefix of each blob resolves to it.
			for _, id := range []ObjectID{a, b} {
				other := a
				if id == a {
					other = b
				}
				n := 4
				for id.String()[:n] == other.String()[:n] {
					n++
				}
				got, err := r.ResolvePrefix(id.String()[:n])
				if err != nil || got != id {
					t.Fatalf("ResolvePrefix(%s) = %s, %v; want %s", id.String()[:n], got, err, id)
				}
				if want := f.git("rev-parse", id.String()[:n]); want != id.String() {
					t.Fatalf("git rev-parse %s = %s", id.String()[:n], want)
				}
				if _, err := r.ResolvePrefix(id.String()[:n-1]); !errors.Is(err, ErrAmbiguous) {
					t.Fatalf("one digit less must stay ambiguous: %v", err)
				}
			}
			// Resolve routes abbreviations through the same code.
			if _, err := r.Resolve(pre); !errors.Is(err, ErrAmbiguous) {
				t.Fatalf("Resolve(%s) = %v, want ErrAmbiguous", pre, err)
			}
			// ResolveCommitPrefix ignores blobs: no commit matches.
			if _, err := r.ResolveCommitPrefix(pre); !errors.Is(err, ErrNotFound) {
				t.Fatalf("ResolveCommitPrefix(%s) = %v, want ErrNotFound", pre, err)
			}
		})
	}
}

func TestOpenAcceptsGitDirAndBare(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	c := f.commit("one")
	for _, p := range []string{f.work, f.dir} {
		r, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		id, err := r.ResolveRef("refs/heads/main")
		if err != nil || id.String() != c {
			t.Fatalf("%s: %s %v", p, id, err)
		}
		_ = r.Close()
	}
	if _, err := Open(t.TempDir()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(empty dir) = %v, want ErrNotFound", err)
	}
	if _, err := Open(t.TempDir() + "/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(missing) = %v, want ErrNotFound", err)
	}
}

func TestBlobStreaming(t *testing.T) {
	f := newFx(t)
	big := textLines(200000, 3) // ~13 MB, compressible
	f.write("big", big, 0o644)
	f.write("empty", nil, 0o644)
	f.commit("big")
	for _, packed := range []bool{false, true} {
		if packed {
			f.git("repack", "-adf")
		}
		r := f.open(WithMaxObjectSize(1 << 20)) // smaller than the blob: it must stream
		id := mustID(t, f.git("rev-parse", "HEAD:big"))
		size, rc, err := r.OpenBlob(id)
		if err != nil {
			t.Fatal(err)
		}
		if size != int64(len(big)) {
			t.Fatalf("size = %d, want %d", size, len(big))
		}
		got, err := io.ReadAll(rc)
		if err != nil || !bytes.Equal(got, big) {
			t.Fatalf("content differs: %d bytes, %v", len(got), err)
		}
		_ = rc.Close()
		_ = rc.Close() // idempotent
		if _, err := rc.Read(make([]byte, 1)); err == nil {
			t.Fatal("Read after Close succeeded")
		}
		// ReadObject refuses it.
		if _, _, err := r.ReadObject(id, nil); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("ReadObject(big) = %v, want ErrTooLarge", err)
		}
		// Empty blob.
		eid := mustID(t, f.git("rev-parse", "HEAD:empty"))
		size, rc, err = r.OpenBlob(eid)
		if err != nil || size != 0 {
			t.Fatalf("empty: %d %v", size, err)
		}
		if n, err := io.Copy(io.Discard, rc); n != 0 || err != nil {
			t.Fatalf("empty read: %d %v", n, err)
		}
		_ = rc.Close()
		// Not a blob and missing.
		if _, _, err := r.OpenBlob(mustID(t, f.git("rev-parse", "HEAD^{tree}"))); !errors.Is(err, ErrWrongType) {
			t.Fatalf("OpenBlob(tree) = %v, want ErrWrongType", err)
		}
		if _, _, err := r.OpenBlob(ObjectID{1}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("OpenBlob(missing) = %v, want ErrNotFound", err)
		}
		// A reader outlives Close of the Repo.
		_, rc, err = r.OpenBlob(id)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		got, err = io.ReadAll(rc)
		if err != nil || !bytes.Equal(got, big) {
			t.Fatalf("read after Close: %d bytes, %v", len(got), err)
		}
		_ = rc.Close()
	}
}

func TestLookupPathAndMissing(t *testing.T) {
	f := newFx(t)
	mixedFixture(t, f)
	r := f.open()
	root := mustID(t, f.git("rev-parse", "HEAD^{tree}"))
	for _, p := range []string{"README.md", "src/a/b/c.go", "src/a", "vendor/sub", "link", "bin/tool.sh", "src/a/ünï.txt", ""} {
		e, err := r.LookupPath(root, p)
		if err != nil {
			t.Fatalf("LookupPath(%q): %v", p, err)
		}
		if p == "" {
			continue
		}
		if want := f.git("rev-parse", "HEAD:"+p); e.ID.String() != want {
			t.Errorf("LookupPath(%q) = %s, want %s", p, e.ID, want)
		}
	}
	for _, p := range []string{"nope", "src/nope", "README.md/x", "src//a"} {
		if _, err := r.LookupPath(root, p); !errors.Is(err, ErrNotFound) {
			t.Errorf("LookupPath(%q) = %v, want ErrNotFound", p, err)
		}
	}
	if _, err := r.LookupPath(root, "vendor/sub/x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("through gitlink: %v", err)
	}
	if e, _ := r.LookupPath(root, "vendor/sub"); !e.Mode.IsGitlink() {
		t.Errorf("mode of vendor/sub = %s", e.Mode)
	}
	if e, _ := r.LookupPath(root, "link"); !e.Mode.IsSymlink() {
		t.Errorf("mode of link = %s", e.Mode)
	}
	if e, _ := r.LookupPath(root, "bin/tool.sh"); e.Mode != ModeExec {
		t.Errorf("mode of tool.sh = %s", e.Mode)
	}
	if _, _, err := r.ReadObject(ObjectID{9}, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadObject(missing) = %v", err)
	}
	if _, _, err := r.ObjectHeader(ObjectID{9}); !errors.Is(err, ErrNotFound) {
		t.Errorf("ObjectHeader(missing) = %v", err)
	}
	if err := r.ReadTree(mustID(t, f.git("rev-parse", "HEAD")), func(Entry) bool { return true }); !errors.Is(err, ErrWrongType) {
		t.Errorf("ReadTree(commit) = %v, want ErrWrongType", err)
	}
	var c Commit
	if err := r.ReadCommit(root, &c); !errors.Is(err, ErrWrongType) {
		t.Errorf("ReadCommit(tree) = %v, want ErrWrongType", err)
	}
}

func TestCommitFields(t *testing.T) {
	f := newFx(t)
	mixedFixture(t, f)
	r := f.open()
	var c Commit
	if err := r.ReadCommit(mustID(t, f.git("rev-parse", "latin1")), &c); err != nil {
		t.Fatal(err)
	}
	if string(c.Author.Name) != "Zoë" || string(c.Author.Email) != "z@example.com" || c.Author.Time != 1700100000 || c.Author.Offset != 5*3600+30*60 {
		t.Errorf("author = %+v", c.Author)
	}
	if c.Committer.Offset != -8*3600 || c.Committer.Time != 1700100001 {
		t.Errorf("committer = %+v", c.Committer)
	}
	if string(c.Encoding) != "ISO-8859-1" {
		t.Errorf("encoding = %q", c.Encoding)
	}
	if string(c.Message) != "caf\xe9 \xff\xfe message\n" {
		t.Errorf("message = %q", c.Message)
	}
	if len(c.Parents) != 1 {
		t.Errorf("parents = %v", c.Parents)
	}
	if got := c.Author.Date().Format("2006-01-02T15:04:05-07:00"); got != "2023-11-16T07:30:00+05:30" {
		t.Errorf("date = %s", got)
	}
	// Empty message commit.
	if err := r.ReadCommit(mustID(t, f.git("rev-parse", "HEAD~2")), &c); err != nil || len(c.Message) != 0 {
		t.Fatalf("empty message commit: %q %v", c.Message, err)
	}
	var tg Tag
	if err := r.ReadTag(mustID(t, f.git("rev-parse", "annotated")), &tg); err != nil {
		t.Fatal(err)
	}
	if string(tg.Name) != "annotated" || tg.Type != TypeCommit || string(tg.Message) != "annotated tag\n\nbody\n" || string(tg.Tagger.Name) != "Grace Hopper" {
		t.Errorf("tag = %+v", tg)
	}
	if err := r.ReadTag(mustID(t, f.git("rev-parse", "nested1")), &tg); err != nil || tg.Type != TypeTag {
		t.Errorf("nested tag = %+v %v", tg, err)
	}
	// Peel of every tag kind.
	for name, want := range map[string]Type{"light": TypeCommit, "annotated": TypeCommit, "nested1": TypeCommit, "nested2": TypeCommit, "treetag": TypeTree, "blobtag": TypeBlob} {
		id, err := r.ResolveRef("refs/tags/" + name)
		if err != nil {
			t.Fatal(err)
		}
		pid, typ, err := r.Peel(id)
		if err != nil || typ != want {
			t.Errorf("Peel(%s) = %s %s %v, want %s", name, pid, typ, err, want)
			continue
		}
		wantID := f.git("rev-parse", "refs/tags/"+name+"^{}")
		if pid.String() != wantID {
			t.Errorf("Peel(%s) = %s, want %s", name, pid, wantID)
		}
		_, err = r.PeelToCommit(id)
		if (want == TypeCommit) != (err == nil) {
			t.Errorf("PeelToCommit(%s) err = %v", name, err)
		}
	}
}

// The delta cache sizes decide which buffers readPacked uses for the
// intermediate results of a chain: the cache, or the pooled scratch buffers.
func TestConformanceDeltaCacheSizes(t *testing.T) {
	f := newFx(t)
	f.write("big.txt", textLines(3000, 0), 0o644)
	for i := 1; i <= 80; i++ {
		d := append(textLines(3000, 0), []byte(fmt.Sprintf("extra %d\n", i))...)
		f.write("big.txt", d, 0o644)
		f.commit("rev " + strconv.Itoa(i))
	}
	f.git("repack", "-adf", "--depth=50", "--window=250")
	for _, size := range []int64{0, 4096, 1 << 20, 64 << 20} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			r := f.open(WithDeltaCacheSize(size))
			compareObjects(t, r, f.root())
			// Twice: the second pass runs against a warm cache.
			compareObjects(t, r, f.root())
		})
	}
}

// The name of a signature loses all its trailing whitespace, as in git's
// split_ident_line (%an), not only spaces.
func TestSignatureNameTrimming(t *testing.T) {
	f := newFx(t)
	f.write("f", []byte("x\n"), 0o644)
	f.commit("one")
	tree := f.git("rev-parse", "HEAD^{tree}")
	for _, name := range []string{"A B", "A \t\r ", "A\t", "A\r", "A \t B \t", "\tlead \t", "A\v", "A\f", " "} {
		raw := "tree " + tree + "\nauthor " + name + "<a@b.c> 1700000000 +0100\ncommitter x <a@b.c> 1700000000 +0100\n\nm\n"
		id := f.gitIn([]byte(raw), "hash-object", "-t", "commit", "-w", "--literally", "--stdin")
		want := string(runGit(t, f.root(), nil, f.env(), "log", "-1", "--format=%an", id))
		want = strings.TrimSuffix(want, "\n")
		var c Commit
		if err := f.open().ReadCommit(mustID(t, id), &c); err != nil {
			t.Fatal(err)
		}
		if string(c.Author.Name) != want {
			t.Errorf("author %q: name %q, git %%an %q", name, c.Author.Name, want)
		}
	}
}
