// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestRealRepository walks every object of the repository named by
// ERMITE_GIT_BENCH_REPO and compares it with git cat-file --batch.
func TestRealRepository(t *testing.T) {
	dir := os.Getenv("ERMITE_GIT_BENCH_REPO")
	if dir == "" {
		t.Skip("ERMITE_GIT_BENCH_REPO is not set")
	}
	requireGit(t)
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	cmd := exec.Command("git", "-C", dir, "cat-file", "--batch-all-objects", "--batch")
	cmd.Env = gitEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReaderSize(stdout, 1<<20)
	var buf, want []byte
	counts := map[Type]int{}
	n := 0
	for {
		hdr, err := br.ReadString('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(hdr)
		size, _ := strconv.Atoi(f[2])
		want = grow(want, size+1)
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
			t.Fatalf("object %s differs: %s/%d vs %s/%d", id, typ, len(got), f[1], size)
		}
		htyp, hsize, err := r.ObjectHeader(id)
		if err != nil || htyp != typ || hsize != int64(size) {
			t.Fatalf("ObjectHeader(%s) = %s %d %v, want %s %d", id, htyp, hsize, err, f[1], size)
		}
		if typ == TypeBlob && n%7 == 0 {
			bsize, rc, err := r.OpenBlob(id)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil || bsize != int64(size) || !bytes.Equal(data, want) {
				t.Fatalf("OpenBlob(%s) differs: %v", id, err)
			}
		}
		if typ == TypeCommit {
			var c Commit
			if err := ParseCommit(got, &c); err != nil {
				t.Fatalf("ParseCommit(%s): %v", id, err)
			}
		}
		if typ == TypeTree {
			if err := ParseTree(got, func(Entry) bool { return true }); err != nil {
				t.Fatalf("ParseTree(%s): %v", id, err)
			}
		}
		counts[typ]++
		n++
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d objects identical to git cat-file: %v", n, counts)
	if n == 0 {
		t.Fatal("no object")
	}

	// Refs and HEAD against git.
	checkRefsAgainstGit(t, r, dir)
}

func checkRefsAgainstGit(t *testing.T, r *Repo, dir string) {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "for-each-ref", "--format=%(refname) %(objectname) %(*objectname)").Output()
	if err != nil {
		t.Fatal(err)
	}
	var want, got []string
	for _, l := range lines(string(out)) {
		want = append(want, strings.TrimRight(l, " "))
	}
	for ref, err := range r.Refs("") {
		if err != nil {
			t.Fatal(err)
		}
		peeled := ""
		if typ, _, err := r.ObjectHeader(ref.ID); err == nil && typ == TypeTag {
			p, _, err := r.Peel(ref.ID)
			if err != nil {
				t.Fatal(err)
			}
			peeled = p.String()
		}
		got = append(got, strings.TrimRight(ref.Name+" "+ref.ID.String()+" "+peeled, " "))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("refs differ:\n%d got vs %d want", len(got), len(want))
	}
	t.Logf("%d refs identical to git for-each-ref", len(got))
}
