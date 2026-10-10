// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"strings"
	"testing"
)

// Headers after the first line are read the way Git reads them: the tree
// must come first, the parents follow it directly, and later tree or parent
// lines are ordinary headers that nothing uses.
func TestParseCommitHostileHeadersMatchGit(t *testing.T) {
	f := newFx(t)
	f.write("a", []byte("a"), 0o644)
	p1 := f.commit("one")
	f.write("a", []byte("b"), 0o644)
	p2 := f.commit("two")
	tree := f.git("rev-parse", "HEAD^{tree}")
	sig := "author A <a@x> 1 +0000\ncommitter C <c@x> 2 +0000\n"
	cases := []struct {
		name, body string
		corrupt    bool
	}{
		{"second tree", "tree " + tree + "\ntree " + p1 + "\nparent " + p1 + "\n" + sig + "\nmsg\n", false},
		{"parents then tree", "tree " + tree + "\nparent " + p1 + "\nparent " + p2 + "\ntree " + p1 + "\n" + sig + "\nmsg\n", false},
		{"parent after author", "tree " + tree + "\nparent " + p1 + "\n" + sig + "parent " + p2 + "\n\nmsg\n", false},
		{"parent after other header", "tree " + tree + "\n" + "author A <a@x> 1 +0000\nparent " + p1 + "\ncommitter C <c@x> 2 +0000\n\nmsg\n", false},
		{"bad parent after author", "tree " + tree + "\n" + sig + "parent zzz\n\nmsg\n", false},
		{"tree after author", "author A <a@x> 1 +0000\ntree " + tree + "\n\nmsg\n", true},
		{"parent before tree", "parent " + p1 + "\ntree " + tree + "\n" + sig + "\nmsg\n", true},
		{"no tree", sig + "\nmsg\n", true},
		{"bad parent", "tree " + tree + "\nparent zzz\n" + sig + "\nmsg\n", true},
		{"parent with junk", "tree " + tree + "\nparent " + p1 + " junk\n" + sig + "\nmsg\n", true},
	}
	r := f.open()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := mustID(t, f.gitIn([]byte(c.body), "hash-object", "-t", "commit", "--literally", "-w", "--stdin"))
			var got Commit
			err := r.ReadCommit(id, &got)
			want, gerr := f.gitErr("log", "-1", "--format=%P %T", id.String())
			if c.corrupt {
				if gerr == nil {
					t.Fatalf("git accepts it: %q", want)
				}
				if !errors.Is(err, ErrCorrupt) {
					t.Fatalf("ReadCommit = %v, want ErrCorrupt", err)
				}
				return
			}
			if gerr != nil {
				t.Fatalf("git rejects it: %v", gerr)
			}
			if err != nil {
				t.Fatal(err)
			}
			var ps []string
			for _, p := range got.Parents {
				ps = append(ps, p.String())
			}
			if have := strings.TrimSpace(strings.Join(ps, " ") + " " + got.Tree.String()); have != strings.TrimSpace(want) {
				t.Errorf("parents and tree = %q, git says %q", have, strings.TrimSpace(want))
			}
		})
	}
}
