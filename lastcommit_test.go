// SPDX-License-Identifier: MIT

package git

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// lastOracle runs git log -n 1 for each name.
func (w *wfix) lastOracle(tip int, dir string, names []string) []ObjectID {
	w.t.Helper()
	out := make([]ObjectID, len(names))
	for i, n := range names {
		p := n
		if dir != "" {
			p = dir + "/" + n
		}
		if l := lines(w.git("", "-c", "core.commitGraph=false", "log", "-n", "1", "--format=%H", w.ids[tip], "--", p)); len(l) > 0 {
			out[i] = mustID(w.t, l[0])
		}
	}
	return out
}

var lastDirs = map[string][]string{
	"":      {"a", "b", "m", "d", "q", "nope", "gone", "readded", "x", "a"},
	"d":     {"x", "y", "e", "nope"},
	"d/e":   {"z", "w", "nope"},
	"d/":    {"x", "e"},
	"./d/e": {"z"},
	"nodir": {"a", "x"},
}

func checkLast(t *testing.T, w *wfix, tips []int) {
	t.Helper()
	type key struct {
		tip int
		dir string
	}
	want := map[key][]ObjectID{}
	for _, tip := range tips {
		for dir, names := range lastDirs {
			want[key{tip, dir}] = w.lastOracle(tip, dir, names)
		}
	}
	for _, g := range wgraphs {
		t.Run(g.name, func(t *testing.T) {
			w.setGraph(g)
			r, err := Open(w.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			for _, tip := range tips {
				for dir, names := range lastDirs {
					got, err := r.LastCommits(context.Background(), mustID(t, w.ids[tip]), dir, names)
					if err != nil {
						t.Fatal(err)
					}
					if exp := want[key{tip, dir}]; !slices.Equal(got, exp) {
						t.Fatalf("tip %d dir %q:\n got %v\nwant %v", tip, dir, got, exp)
					}
				}
			}
		})
	}
}

func TestLastCommitsRandom(t *testing.T) {
	requireGit(t)
	seeds := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	if testing.Short() {
		seeds = seeds[:2]
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			cs := randomHistory(seed, 140)
			checkLast(t, buildWFix(t, cs), lastTips(len(cs)))
		})
	}
}

func TestLastCommitsHandcrafted(t *testing.T) {
	requireGit(t)
	for name, cs := range handHistories() {
		t.Run(name, func(t *testing.T) {
			tips := make([]int, len(cs))
			for i := range tips {
				tips[i] = i
			}
			checkLast(t, buildWFix(t, cs), tips)
		})
	}
}

func TestLastCommitSingle(t *testing.T) {
	requireGit(t)
	cs := randomHistory(9, 80)
	w := buildWFix(t, cs)
	w.setGraph(wgraphs[4])
	r, err := Open(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	tip := len(cs) - 1
	for _, p := range []string{"a", "d", "d/e/z", "nope", ""} {
		id, ok, err := r.LastCommit(context.Background(), mustID(t, w.ids[tip]), p)
		if err != nil {
			t.Fatal(err)
		}
		exp := w.gitLog(tip, 0, 1, p)
		if ok != (len(exp) == 1) || (ok && id.String() != exp[0]) {
			t.Errorf("%q: got %v %v, want %v", p, id, ok, exp)
		}
	}
	for _, bad := range []string{"", ".", "a/b", "../x"} {
		if _, err := r.LastCommits(context.Background(), mustID(t, w.ids[tip]), "", []string{bad}); !errorsIs(err, ErrInvalidPath) {
			t.Errorf("name %q: err = %v", bad, err)
		}
	}
	if got, err := r.LastCommits(context.Background(), mustID(t, w.ids[tip]), "", nil); err != nil || len(got) != 0 {
		t.Errorf("no names: %v %v", got, err)
	}
}
