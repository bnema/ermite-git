// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// ---- fixtures -------------------------------------------------------------

// wcommit describes one commit of a fixture. Parents are indexes of earlier
// commits; files is the complete tree.
type wcommit struct {
	parents []int
	date    int64
	files   map[string]string
}

// wfix is a bare repository holding the commits of a description, built with
// git fast-import (one process, exact dates, arbitrary trees).
type wfix struct {
	t       testing.TB
	dir     string
	commits []wcommit
	ids     []string
}

func buildWFix(t testing.TB, cs []wcommit) *wfix {
	t.Helper()
	requireGit(t)
	dir := filepath.Join(t.TempDir(), "w.git")
	runGit(t, t.TempDir(), nil, nil, "init", "-q", "--bare", dir)
	var in bytes.Buffer
	for i, c := range cs {
		fmt.Fprintf(&in, "commit refs/heads/c%d\nmark :%d\ncommitter T <t@example.com> %d +0000\ndata 2\nc\n", i, i+1, c.date)
		for j, p := range c.parents {
			if j == 0 {
				fmt.Fprintf(&in, "from :%d\n", p+1)
			} else {
				fmt.Fprintf(&in, "merge :%d\n", p+1)
			}
		}
		in.WriteString("deleteall\n")
		names := make([]string, 0, len(c.files))
		for n := range c.files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&in, "M 100644 inline %s\ndata %d\n%s\n", n, len(c.files[n]), c.files[n])
		}
	}
	marks := filepath.Join(t.TempDir(), "marks")
	runGit(t, dir, in.Bytes(), []string{"GIT_DIR=" + dir}, "fast-import", "--quiet", "--export-marks="+marks)
	raw, err := os.ReadFile(marks)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(cs))
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m int
		var id string
		if _, err := fmt.Sscanf(l, ":%d %s", &m, &id); err != nil {
			t.Fatal(err)
		}
		ids[m-1] = id
	}
	return &wfix{t: t, dir: dir, commits: cs, ids: ids}
}

func (w *wfix) git(stdin string, args ...string) string {
	w.t.Helper()
	var in []byte
	if stdin != "" {
		in = []byte(stdin)
	}
	return string(runGit(w.t, w.dir, in, []string{"GIT_DIR=" + w.dir}, args...))
}

// gitLog is the oracle: the graph is switched off so that it is plain git.
func (w *wfix) gitLog(tip int, skip, limit int, path string) []string {
	w.t.Helper()
	args := []string{"-c", "core.commitGraph=false", "log", "--format=%H"}
	if skip > 0 {
		args = append(args, fmt.Sprintf("--skip=%d", skip))
	}
	if limit > 0 {
		args = append(args, "-n", fmt.Sprint(limit))
	}
	args = append(args, w.ids[tip])
	if path != "" {
		args = append(args, "--", path)
	}
	return lines(w.git("", args...))
}

// graph configurations ------------------------------------------------------

type wgraph struct {
	name   string
	bloom  int // 0 none, 1, 2
	layers int // 0: no graph; k>0: graph over the first k stages
	split  bool
	gen1   bool
}

var wgraphs = []wgraph{
	{name: "nograph"},
	{name: "single-nobloom", layers: 1},
	{name: "single-bloom2", layers: 1, bloom: 2},
	{name: "split-bloom1-partial", layers: 3, split: true, bloom: 1},
	{name: "split-bloom2-partial", layers: 3, split: true, bloom: 2},
	{name: "split-bloom2-gen1", layers: 2, split: true, bloom: 2, gen1: true},
	{name: "split-nobloom-partial", layers: 2, split: true},
}

// setGraph rewrites the commit-graph of the fixture. With layers = k the
// commits are covered in k stages of equal size, except that the last tenth
// of the commits is never covered when k > 1 (new commits not yet in the
// graph).
func (w *wfix) setGraph(g wgraph) {
	w.t.Helper()
	info := filepath.Join(w.dir, "objects", "info")
	for _, p := range []string{"commit-graph", "commit-graphs"} {
		if err := os.RemoveAll(filepath.Join(info, p)); err != nil {
			w.t.Fatal(err)
		}
	}
	if g.layers == 0 {
		return
	}
	n := len(w.commits)
	covered := n
	if g.layers > 1 {
		covered = n - n/10
	}
	for k := 1; k <= g.layers; k++ {
		upto := covered * k / g.layers
		var in strings.Builder
		for i := 0; i < upto; i++ {
			in.WriteString(w.ids[i] + "\n")
		}
		args := []string{"-c", "commitGraph.generationVersion=2"}
		if g.gen1 {
			args = []string{"-c", "commitGraph.generationVersion=1"}
		}
		args = append(args, "commit-graph", "write", "--stdin-commits")
		if g.bloom > 0 {
			args = append([]string{"-c", fmt.Sprintf("commitGraph.changedPathsVersion=%d", g.bloom)}, args...)
			args = append(args, "--changed-paths")
		} else {
			args = append(args, "--no-changed-paths")
		}
		if g.split {
			args = append(args, "--split=no-merge")
		}
		w.git(in.String(), args...)
	}
}

// ---- generators -------------------------------------------------------------

var wpaths = []string{"a", "b", "m", "d/x", "d/y", "d/e/z", "d/e/w", "q/r"}

// randomHistory builds a merge-heavy DAG: merges of every TREESAME kind,
// octopus merges, several roots, repeated contents (reverts), equal dates and
// clock skew.
func randomHistory(seed int64, n int) []wcommit {
	rng := rand.New(rand.NewSource(seed))
	cs := make([]wcommit, 0, n)
	date := int64(1_600_000_000)
	for i := 0; i < n; i++ {
		var c wcommit
		switch r := rng.Intn(100); {
		case i == 0 || r < 2:
			// root
		case r < 20 && i > 2:
			c.parents = []int{pick(rng, i), pick(rng, i)}
			if c.parents[0] == c.parents[1] {
				c.parents = c.parents[:1]
			}
		case r < 24 && i > 4:
			c.parents = []int{pick(rng, i), pick(rng, i), pick(rng, i)}
			c.parents = slices.Compact(c.parents)
		default:
			c.parents = []int{pick(rng, i)}
		}
		switch r := rng.Intn(100); {
		case r < 12:
			// same date as the previous commit
		case r < 25:
			date -= int64(rng.Intn(400)) // skew
		default:
			date += int64(1 + rng.Intn(200))
		}
		c.date = date
		c.files = map[string]string{}
		if len(c.parents) > 0 {
			base := cs[c.parents[0]].files
			switch rng.Intn(4) {
			case 0: // as the first parent
				maps_copy(c.files, base)
			case 1: // as another parent (a revert of the first side)
				maps_copy(c.files, cs[c.parents[len(c.parents)-1]].files)
			case 2: // union, first parent wins
				for _, p := range slices.Backward(c.parents) {
					maps_copy(c.files, cs[p].files)
				}
			default:
				maps_copy(c.files, base)
			}
		}
		for k := rng.Intn(3); k > 0; k-- {
			p := wpaths[rng.Intn(len(wpaths))]
			if rng.Intn(5) == 0 {
				delete(c.files, p)
			} else {
				c.files[p] = fmt.Sprintf("v%d\n", rng.Intn(3)) // few values: contents come back
			}
		}
		if rng.Intn(15) == 0 { // a file where a directory was, and back
			delete(c.files, "d/x")
			delete(c.files, "d/y")
			delete(c.files, "d/e/z")
			delete(c.files, "d/e/w")
			c.files["d"] = fmt.Sprintf("file%d\n", rng.Intn(2))
		}
		dropConflicts(c.files)
		cs = append(cs, c)
	}
	return cs
}

func pick(rng *rand.Rand, i int) int {
	if rng.Intn(3) > 0 {
		return max(0, i-1-rng.Intn(4))
	}
	return rng.Intn(i)
}

func maps_copy(dst, src map[string]string) {
	for k, v := range src {
		dst[k] = v
	}
}

// dropConflicts removes the paths below a path that is a file.
func dropConflicts(files map[string]string) {
	for p := range files {
		for q := range files {
			if strings.HasPrefix(q, p+"/") {
				delete(files, q)
			}
		}
	}
}

// ---- comparison -------------------------------------------------------------

func walkIDs(t testing.TB, r *Repo, start ObjectID, opts WalkOptions) []string {
	t.Helper()
	var out []string
	for c, err := range r.Walk(context.Background(), start, opts) {
		if err != nil {
			t.Fatalf("Walk(%+v): %v", opts, err)
		}
		out = append(out, c.ID.String())
	}
	return out
}

var wqueryPaths = []string{"", "a", "b", "m", "d", "d/", "d/x", "d/y", "d/e", "d/e/z", "d/e/w", "q", "q/r", "q/r/s", "nope", "d/x/y", "./d", "d//e", "d/./e", "d/e/../x", "."}

// checkWFix compares Walk with git log for several tips, paths and windows,
// in every graph configuration.
func checkWFix(t *testing.T, w *wfix, tips []int, paths []string) {
	t.Helper()
	type key struct {
		tip  int
		path string
	}
	want := map[key][]string{}
	for _, tip := range tips {
		for _, p := range paths {
			want[key{tip, p}] = w.gitLog(tip, 0, 0, p)
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
				start := mustID(t, w.ids[tip])
				for _, p := range paths {
					full := want[key{tip, p}]
					got := walkIDs(t, r, start, WalkOptions{Path: p})
					if !slices.Equal(got, full) {
						t.Fatalf("tip %d path %q: got %d commits, want %d\n got %v\nwant %v", tip, p, len(got), len(full), got, full)
					}
					for _, win := range [][2]int{{0, 1}, {0, 5}, {3, 7}, {2, 0}, {len(full) + 1, 3}} {
						exp := window(full, win[0], win[1])
						got := walkIDs(t, r, start, WalkOptions{Path: p, Skip: win[0], Limit: win[1]})
						if !slices.Equal(got, exp) {
							t.Fatalf("tip %d path %q skip %d limit %d:\n got %v\nwant %v", tip, p, win[0], win[1], got, exp)
						}
					}
				}
			}
		})
	}
}

func window(all []string, skip, limit int) []string {
	if skip > len(all) {
		skip = len(all)
	}
	all = all[skip:]
	if limit > 0 && limit < len(all) {
		all = all[:limit]
	}
	return all
}

func lastTips(n int) []int {
	return []int{n - 1, n - 2, n / 2, n / 3}
}

func TestWalkRandomHistories(t *testing.T) {
	requireGit(t)
	seeds := []int64{1, 2, 3, 4, 5, 6}
	if testing.Short() {
		seeds = seeds[:2]
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			cs := randomHistory(seed, 140)
			w := buildWFix(t, cs)
			checkWFix(t, w, lastTips(len(cs)), wqueryPaths)
		})
	}
}

// TestWalkGitOptionWindows checks the Skip and Limit semantics against real
// --skip and -n arguments, not against a slice of the full list.
func TestWalkGitOptionWindows(t *testing.T) {
	cs := randomHistory(11, 90)
	w := buildWFix(t, cs)
	w.setGraph(wgraphs[4])
	r, err := Open(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	tip := len(cs) - 1
	for _, p := range []string{"", "d", "a", "d/e/z"} {
		for _, win := range [][2]int{{0, 1}, {0, 10}, {7, 0}, {5, 5}, {1000, 3}} {
			exp := w.gitLog(tip, win[0], win[1], p)
			got := walkIDs(t, r, mustID(t, w.ids[tip]), WalkOptions{Path: p, Skip: win[0], Limit: win[1]})
			if !slices.Equal(got, exp) {
				t.Errorf("path %q skip %d limit %d: got %v want %v", p, win[0], win[1], got, exp)
			}
		}
	}
}

func TestWalkInvalidPathAndContext(t *testing.T) {
	cs := randomHistory(5, 30)
	w := buildWFix(t, cs)
	r, err := Open(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	tip := mustID(t, w.ids[len(cs)-1])
	for _, p := range []string{"../a", "/a", "a\x00"} {
		for _, err := range r.Walk(context.Background(), tip, WalkOptions{Path: p}) {
			if err == nil || !errorsIs(err, ErrInvalidPath) {
				t.Errorf("path %q: err = %v", p, err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	for _, err := range r.Walk(ctx, tip, WalkOptions{}) {
		if err == nil {
			t.Fatal("cancelled walk yielded a commit")
		}
		if err != context.Canceled {
			t.Fatalf("err = %v", err)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("got %d errors", n)
	}
	// A missing start commit is an error.
	for _, err := range r.Walk(context.Background(), ObjectID{1}, WalkOptions{}) {
		if err == nil || !errorsIs(err, ErrNotFound) {
			t.Fatalf("missing start: %v", err)
		}
	}
	// Breaking out of the loop releases the graph.
	for range r.Walk(context.Background(), tip, WalkOptions{}) {
		break
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if ok {
			err = u.Unwrap()
			continue
		}
		if m, ok := err.(interface{ Unwrap() []error }); ok {
			for _, e := range m.Unwrap() {
				if errorsIs(e, target) {
					return true
				}
			}
		}
		return false
	}
	return false
}

// ---- handcrafted histories ----------------------------------------------------

func fl(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func with(base map[string]string, kv ...string) map[string]string {
	m := map[string]string{}
	maps_copy(m, base)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "-" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

var handPaths = []string{"", "a", "b", "d", "d/x", "d/e", "d/e/z", "gone", "nope", "readded", "m"}

func handHistories() map[string][]wcommit {
	base := fl("a", "1\n", "b", "1\n", "d/x", "1\n", "d/e/z", "1\n", "m", "0\n")
	h := map[string][]wcommit{}

	// Criss-cross: two branches merge into each other, then merge again.
	h["crisscross"] = []wcommit{
		{date: 100, files: base}, // 0
		{parents: []int{0}, date: 110, files: with(base, "a", "2\n")},                          // 1 side A
		{parents: []int{0}, date: 120, files: with(base, "b", "2\n")},                          // 2 side B
		{parents: []int{1, 2}, date: 130, files: with(base, "a", "2\n", "b", "2\n")},           // 3 merge A+B
		{parents: []int{2, 1}, date: 131, files: with(base, "a", "2\n", "b", "2\n")},           // 4 merge B+A
		{parents: []int{3, 4}, date: 140, files: with(base, "a", "2\n", "b", "2\n")},           // 5 criss-cross merge
		{parents: []int{5}, date: 150, files: with(base, "a", "3\n", "b", "2\n")},              // 6
		{parents: []int{6, 4}, date: 160, files: with(base, "a", "3\n", "b", "2\n", "m", "9")}, // 7
	}

	// Merges of every TREESAME kind for the path "a".
	a0 := fl("a", "0\n", "b", "0\n")
	h["treesame-kinds"] = []wcommit{
		{date: 10, files: a0}, // 0
		{parents: []int{0}, date: 20, files: with(a0, "a", "1\n")},                         // 1 changes a
		{parents: []int{0}, date: 21, files: with(a0, "b", "1\n")},                         // 2 changes b only
		{parents: []int{1, 2}, date: 30, files: with(a0, "a", "1\n", "b", "1\n")},          // 3 same as 1 for a, differs from 2
		{parents: []int{2, 1}, date: 31, files: with(a0, "a", "1\n", "b", "1\n")},          // 4 same a as 1 (2nd parent)
		{parents: []int{1, 2}, date: 32, files: with(a0, "a", "5\n", "b", "1\n")},          // 5 differs from both
		{parents: []int{1, 2}, date: 33, files: with(a0, "b", "1\n")},                      // 6 reverts a: same as 2 (2nd parent)
		{parents: []int{2, 1}, date: 34, files: with(a0, "b", "1\n")},                      // 7 reverts a: same as 2 (1st parent)
		{parents: []int{3, 4, 5, 6, 7}, date: 40, files: with(a0, "a", "1\n", "b", "1\n")}, // 8 octopus
	}

	// Equal dates everywhere: only the tie-breaks order the commits.
	h["equal-dates"] = []wcommit{
		{date: 50, files: fl("a", "0")},
		{parents: []int{0}, date: 50, files: fl("a", "1")},
		{parents: []int{0}, date: 50, files: fl("a", "2")},
		{parents: []int{1, 2}, date: 50, files: fl("a", "3")},
		{parents: []int{2, 1}, date: 50, files: fl("a", "4")},
		{parents: []int{4, 3}, date: 50, files: fl("a", "4", "b", "1")},
		{parents: []int{5, 1, 2}, date: 50, files: fl("a", "4", "b", "2")},
	}

	// Clock skew: a parent is newer than its child, and a branch is older.
	h["skew"] = []wcommit{
		{date: 1000, files: fl("a", "0")},
		{parents: []int{0}, date: 900, files: fl("a", "1")},
		{parents: []int{1}, date: 2000, files: fl("a", "2")},
		{parents: []int{0}, date: 1500, files: fl("b", "1")},
		{parents: []int{2, 3}, date: 1100, files: fl("a", "2", "b", "1")},
		{parents: []int{4}, date: 1050, files: fl("a", "3", "b", "1")},
	}

	// A file that is deleted and added back, a path that never existed,
	// a file replaced by a directory and the other way round.
	h["delete-readd"] = []wcommit{
		{date: 1, files: fl("readded", "1", "gone", "1", "x", "1")},
		{parents: []int{0}, date: 2, files: fl("x", "1", "gone", "1")},
		{parents: []int{1}, date: 3, files: fl("x", "2", "gone", "1")},
		{parents: []int{2}, date: 4, files: fl("x", "2", "gone", "1", "readded", "1")},
		{parents: []int{3}, date: 5, files: fl("x", "2", "readded", "2")},
		{parents: []int{4}, date: 6, files: fl("x", "2", "readded/sub", "1")},
		{parents: []int{5}, date: 7, files: fl("x", "2", "readded", "1")},
		{parents: []int{0, 6}, date: 8, files: fl("readded", "1", "gone", "1", "x", "2")},
	}

	// Several roots merged, one of them without the path.
	h["roots"] = []wcommit{
		{date: 1, files: fl("a", "1")},
		{date: 2, files: fl("b", "1")},
		{date: 3, files: fl("d/x", "1")},
		{parents: []int{0, 1, 2}, date: 4, files: fl("a", "1", "b", "1", "d/x", "1")},
		{parents: []int{3}, date: 5, files: fl("a", "1", "b", "1", "d/x", "2")},
	}
	return h
}

func TestWalkHandcrafted(t *testing.T) {
	requireGit(t)
	for name, cs := range handHistories() {
		t.Run(name, func(t *testing.T) {
			w := buildWFix(t, cs)
			tips := make([]int, 0, len(cs))
			for i := range cs {
				tips = append(tips, i)
			}
			checkWFix(t, w, tips, handPaths)
		})
	}
}

// ---- cyclic graphs ------------------------------------------------------------

// TestWalkCyclicGraph feeds a graph whose parents form cycles (a crafted
// file): the walk must end and yield each commit at most once.
func TestWalkCyclicGraph(t *testing.T) {
	requireGit(t)
	dir := filepath.Join(t.TempDir(), "c.git")
	runGit(t, t.TempDir(), nil, nil, "init", "-q", "--bare", dir)
	const n = 6
	ids := synthIDs(n)
	s := &synthLayer{ids: ids, parents: make([][]uint32, n), times: make([]int64, n)}
	for i := range n {
		s.times[i] = 1000 + int64(i)
		s.parents[i] = []uint32{uint32((i + 1) % n)} // 0 -> 1 -> ... -> 5 -> 0
	}
	s.parents[2] = []uint32{3, 2, 0} // a self-loop and a back edge
	info := filepath.Join(dir, "objects", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "commit-graph"), s.bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	seen := map[ObjectID]bool{}
	for c, err := range r.Walk(context.Background(), ObjectID(ids[0]), WalkOptions{IDOnly: true}) {
		if err != nil {
			t.Fatal(err)
		}
		if seen[c.ID] {
			t.Fatalf("%s yielded twice", c.ID)
		}
		seen[c.ID] = true
		if len(seen) > n {
			t.Fatal("walk does not end")
		}
	}
	if len(seen) != n {
		t.Fatalf("yielded %d commits, want %d", len(seen), n)
	}
}

// TestWalkGraphErrors checks that a graph that cannot be used because a layer
// vanished is ignored, while a corrupt one is reported.
func TestWalkGraphErrors(t *testing.T) {
	cs := randomHistory(3, 40)
	w := buildWFix(t, cs)
	w.setGraph(wgraphs[4])
	tip := mustID(t, w.ids[len(cs)-1])
	want := w.gitLog(len(cs)-1, 0, 0, "")

	// A layer of the chain is gone: no graph, same result.
	chain := filepath.Join(w.dir, "objects", "info", "commit-graphs")
	files, _ := filepath.Glob(filepath.Join(chain, "graph-*.graph"))
	if len(files) < 2 {
		t.Fatalf("want a split chain, got %v", files)
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	r, err := Open(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := walkIDs(t, r, tip, WalkOptions{}); !slices.Equal(got, want) {
		t.Fatalf("with a vanished layer: got %v want %v", got, want)
	}
	_ = r.Close()

	// A truncated single graph is corruption.
	w.setGraph(wgraphs[1])
	path := filepath.Join(w.dir, "objects", "info", "commit-graph")
	data, _ := os.ReadFile(path)
	_ = os.Remove(path)
	if err := os.WriteFile(path, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	r, err = Open(w.dir)
	if err != nil {
		t.Fatalf("Open must not fail on a bad graph: %v", err)
	}
	defer func() { _ = r.Close() }()
	for _, err := range r.Walk(context.Background(), tip, WalkOptions{}) {
		if !errorsIs(err, ErrCorrupt) {
			t.Fatalf("err = %v, want ErrCorrupt", err)
		}
	}
	// Once the graph is rewritten, Refresh picks it up.
	_ = os.Remove(path)
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	if got := walkIDs(t, r, tip, WalkOptions{}); !slices.Equal(got, want) {
		t.Fatal("after repair: wrong history")
	}
}
