// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// oid is the raw object id used by the graph code until the core's ObjectID
// is merged.
type oid = [20]byte

// graphTestHex parses a 40-digit hex string.
func graphTestHex(s string) oid {
	var id oid
	if _, err := hex.Decode(id[:], []byte(s)); err != nil || len(s) != 40 {
		panic("graphTestHex: bad id " + strconv.Quote(s))
	}
	return id
}

// ---- git fixtures ---------------------------------------------------------

func graphGitEnv(date int64) []string {
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	if date != 0 {
		d := strconv.FormatInt(date, 10) + " +0000"
		env = append(env, "GIT_AUTHOR_DATE="+d, "GIT_COMMITTER_DATE="+d)
	}
	return env
}

func graphGit(t testing.TB, dir string, date int64, args ...string) string {
	t.Helper()
	return graphGitIn(t, dir, date, "", args...)
}

func graphGitStdin(t testing.TB, dir, stdin string, args ...string) string {
	t.Helper()
	return graphGitIn(t, dir, 0, stdin, args...)
}

func graphGitIn(t testing.TB, dir string, date int64, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Dir = dir
	cmd.Env = graphGitEnv(date)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// gtRepo is a scratch repository with a deterministic clock.
type gtRepo struct {
	t     testing.TB
	dir   string
	clock int64
}

func newGTRepo(t testing.TB) *gtRepo {
	t.Helper()
	requireGit(t)
	r := &gtRepo{t: t, dir: t.TempDir(), clock: 1_700_000_000}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *gtRepo) git(args ...string) string {
	r.t.Helper()
	return graphGit(r.t, r.dir, r.clock, args...)
}

func (r *gtRepo) objects() string { return filepath.Join(r.dir, ".git", "objects") }

func (r *gtRepo) write(path, content string) {
	r.t.Helper()
	full := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *gtRepo) tick() int64 {
	r.clock += 60
	return r.clock
}

// commit writes files (path, content pairs) and commits them.
func (r *gtRepo) commit(files ...string) {
	r.t.Helper()
	for i := 0; i+1 < len(files); i += 2 {
		r.write(files[i], files[i+1])
	}
	r.tick()
	r.git("add", "-A")
	r.git("commit", "-q", "-m", fmt.Sprintf("c%d", r.clock))
}

// commitAt commits with an explicit date (to build clock skew).
func (r *gtRepo) commitAt(date int64, files ...string) {
	r.t.Helper()
	for i := 0; i+1 < len(files); i += 2 {
		r.write(files[i], files[i+1])
	}
	r.git("add", "-A")
	graphGit(r.t, r.dir, date, "commit", "-q", "-m", fmt.Sprintf("c%d", date))
}

func (r *gtRepo) remove(path string) {
	r.t.Helper()
	r.tick()
	r.git("rm", "-q", "-r", path)
	r.git("commit", "-q", "-m", "rm "+path)
}

// stageOne: linear history with nested dirs, deletes, non-ASCII and
// high-bit paths, an empty commit, clock skew and ordinary merges.
func (r *gtRepo) stageOne() {
	r.commit("README", "r\n", "src/a.go", "a\n", "src/pkg/b.go", "b\n")
	r.commit("src/a.go", "a2\n", "docs/guide.md", "g\n")
	r.commit("café/menü.txt", "x\n", "\xe9\xe8/\xff\xfe.txt", "latin1\n", "日本語/ファイル.txt", "j\n")
	r.commit("src/pkg/deep/er/c.go", "c\n")
	r.tick()
	r.git("commit", "-q", "--allow-empty", "-m", "empty")
	r.remove("docs")
	// clock skew: a child older than its parent.
	r.commitAt(r.clock-5000, "src/skew.go", "s\n")
	r.commit("src/a.go", "a3\n")

	r.git("checkout", "-q", "-b", "topic")
	r.commit("topic/one.txt", "1\n")
	r.commit("topic/two.txt", "2\n", "src/pkg/b.go", "b2\n")
	r.git("checkout", "-q", "main")
	r.commit("main-only.txt", "m\n")
	r.tick()
	r.git("merge", "-q", "--no-ff", "-m", "merge topic", "topic")
	r.commit("README", "r2\n")
}

// stageTwo: an octopus merge (5 parents) and a merge of an older branch.
func (r *gtRepo) stageTwo() {
	for i := 1; i <= 4; i++ {
		r.git("checkout", "-q", "-b", fmt.Sprintf("b%d", i), "main")
		r.commit(fmt.Sprintf("oct/b%d.txt", i), fmt.Sprintf("%d\n", i), "ünï/b"+strconv.Itoa(i), "x\n")
		r.commit(fmt.Sprintf("oct/b%d.txt", i), fmt.Sprintf("%d%d\n", i, i))
	}
	r.git("checkout", "-q", "main")
	r.commit("after.txt", "a\n")
	r.tick()
	r.git("merge", "-q", "--no-ff", "-m", "octopus", "b1", "b2", "b3", "b4")
	r.commit("src/a.go", "a4\n")
	r.tick()
	r.git("merge", "-q", "--no-ff", "-m", "merge topic again", "topic")
}

// stageThree: more linear commits, a merge whose parents are in base layers.
func (r *gtRepo) stageThree() {
	r.git("checkout", "-q", "-b", "late", "topic")
	r.commit("late/x.txt", "x\n")
	r.git("checkout", "-q", "main")
	r.commit("src/pkg/b.go", "b3\n")
	r.tick()
	r.git("merge", "-q", "--no-ff", "-m", "merge late", "late")
	for i := range 5 {
		r.commit(fmt.Sprintf("tail/%d.txt", i), "t\n")
	}
}

type gcase struct {
	name      string
	split     bool
	bloom     bool
	cfg       []string // -c key=value before the subcommand
	bloomVer  uint32   // expected BDAT version when bloom
	wantGenV2 bool
}

// writeGraph runs git commit-graph write. git leaves its files read-only.
func (r *gtRepo) writeGraph(c gcase) {
	r.t.Helper()
	args := []string{}
	for _, kv := range c.cfg {
		args = append(args, "-c", kv)
	}
	args = append(args, "commit-graph", "write", "--reachable")
	if c.bloom {
		args = append(args, "--changed-paths")
	} else {
		args = append(args, "--no-changed-paths")
	}
	if c.split {
		args = append(args, "--split=no-merge")
	}
	r.git(args...)
}

// buildCase creates the three-stage history, writing a graph after each.
func buildCase(t *testing.T, c gcase) *gtRepo {
	r := newGTRepo(t)
	r.stageOne()
	r.writeGraph(c)
	r.stageTwo()
	r.writeGraph(c)
	r.stageThree()
	r.writeGraph(c)
	r.git("commit-graph", "verify")
	return r
}

// ---- reference data from git ---------------------------------------------

type refCommit struct {
	id      oid
	tree    oid
	parents []oid
	ctime   int64
}

func gitCommits(t testing.TB, dir string) []refCommit {
	t.Helper()
	out := graphGit(t, dir, 0, "log", "--all", "--format=%H %T %P|%ct")
	var cs []refCommit
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		left, ct, ok := strings.Cut(line, "|")
		if !ok {
			t.Fatalf("bad log line %q", line)
		}
		f := strings.Fields(left)
		c := refCommit{id: graphTestHex(f[0]), tree: graphTestHex(f[1])}
		for _, p := range f[2:] {
			c.parents = append(c.parents, graphTestHex(p))
		}
		c.ctime, _ = strconv.ParseInt(ct, 10, 64)
		cs = append(cs, c)
	}
	return cs
}

// checkGraphAgainstGit compares every commit of g with git's view.
func checkGraphAgainstGit(t *testing.T, dir string, g *commitGraph, wantMerges bool) []refCommit {
	t.Helper()
	cs := gitCommits(t, dir)
	if int(g.numCommits()) != len(cs) {
		t.Fatalf("numCommits = %d, git has %d commits", g.numCommits(), len(cs))
	}
	byID := make(map[oid]*refCommit, len(cs))
	for i := range cs {
		byID[cs[i].id] = &cs[i]
	}
	var buf [8]uint32
	sawOctopus, sawMerge := false, false
	for i := range cs {
		c := &cs[i]
		pos, ok := g.lookup(c.id)
		if !ok {
			t.Fatalf("lookup(%x) not found", c.id)
		}
		if id, err := g.commitID(pos); err != nil || id != c.id {
			t.Fatalf("commitID(%d) = %x, %v; want %x", pos, id, err, c.id)
		}
		if tree, err := g.rootTree(pos); err != nil || tree != c.tree {
			t.Fatalf("rootTree(%d) = %x, %v; want %x", pos, tree, err, c.tree)
		}
		if ct, err := g.commitTime(pos); err != nil || ct != c.ctime {
			t.Fatalf("commitTime(%d) = %d, %v; want %d", pos, ct, err, c.ctime)
		}
		ps, err := g.appendParents(buf[:0], pos)
		if err != nil {
			t.Fatalf("appendParents(%d): %v", pos, err)
		}
		if len(ps) != len(c.parents) {
			t.Fatalf("commit %x: %d parents, git has %d", c.id, len(ps), len(c.parents))
		}
		for j, p := range ps {
			pid, err := g.commitID(p)
			if err != nil || pid != c.parents[j] {
				t.Fatalf("commit %x parent %d = %x, %v; want %x", c.id, j, pid, err, c.parents[j])
			}
		}
		sawMerge = sawMerge || len(ps) == 2
		sawOctopus = sawOctopus || len(ps) > 2
	}
	if wantMerges && (!sawMerge || !sawOctopus) {
		t.Errorf("fixture lacks merge (%v) or octopus (%v)", sawMerge, sawOctopus)
	}
	checkGeneration(t, g, cs, byID, wantMerges)
	return cs
}

// checkGeneration verifies the topological level exactly (1 + max parents)
// and, when present, the corrected commit date (max(date, max parents + 1)).
func checkGeneration(t *testing.T, g *commitGraph, cs []refCommit, byID map[oid]*refCommit, wantSkew bool) {
	t.Helper()
	level := func(id oid) uint32 {
		pos, _ := g.lookup(id)
		v, err := g.topoLevel(pos)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	gen := func(id oid) uint64 {
		pos, _ := g.lookup(id)
		v, err := g.generation(pos)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	skewed := false
	for i := range cs {
		c := &cs[i]
		wantLevel := uint32(1)
		wantGen := uint64(c.ctime)
		for _, p := range c.parents {
			if l := level(p) + 1; l > wantLevel {
				wantLevel = l
			}
			if v := gen(p) + 1; g.hasGenerationV2() && v > wantGen {
				wantGen = v
			}
		}
		if got := level(c.id); got != wantLevel {
			t.Fatalf("topoLevel(%x) = %d, want %d", c.id, got, wantLevel)
		}
		if g.hasGenerationV2() {
			got := gen(c.id)
			if got != wantGen {
				t.Fatalf("generation(%x) = %d, want corrected date %d", c.id, got, wantGen)
			}
			skewed = skewed || got != uint64(c.ctime)
		} else if got := gen(c.id); got != uint64(wantLevel) {
			t.Fatalf("generation(%x) = %d, want level %d", c.id, got, wantLevel)
		}
		for _, p := range c.parents {
			if gen(p) >= gen(c.id) {
				t.Fatalf("generation not increasing from parent %x to %x", p, c.id)
			}
		}
	}
	if wantSkew && g.hasGenerationV2() && !skewed {
		t.Error("clock skew fixture produced no corrected date different from the commit time")
	}
	_ = byID
}

// ---- conformance ----------------------------------------------------------

func graphCases() []gcase {
	return []gcase{
		{name: "single", wantGenV2: true},
		{name: "single-bloom", bloom: true, bloomVer: 1, wantGenV2: true},
		{name: "split", split: true, wantGenV2: true},
		{name: "split-bloom", split: true, bloom: true, bloomVer: 1, wantGenV2: true},
		{name: "split-bloom-v1", split: true, bloom: true, bloomVer: 1, wantGenV2: true,
			cfg: []string{"commitGraph.changedPathsVersion=1"}},
		{name: "split-bloom-v2-explicit", split: true, bloom: true, bloomVer: 2, wantGenV2: true,
			cfg: []string{"commitGraph.changedPathsVersion=2"}},
		{name: "split-gen1", split: true, bloom: true, bloomVer: 1,
			cfg: []string{"commitGraph.generationVersion=1"}},
		{name: "single-gen1", cfg: []string{"commitGraph.generationVersion=1"}},
	}
}

func TestGraphConformance(t *testing.T) {
	for _, c := range graphCases() {
		t.Run(c.name, func(t *testing.T) {
			r := buildCase(t, c)
			g, err := openCommitGraph(r.objects())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.close() }()

			wantLayers := 1
			if c.split {
				wantLayers = 3
			}
			if g.numLayers() != wantLayers {
				t.Fatalf("layers = %d, want %d", g.numLayers(), wantLayers)
			}
			if g.hasGenerationV2() != c.wantGenV2 {
				t.Fatalf("hasGenerationV2 = %v, want %v", g.hasGenerationV2(), c.wantGenV2)
			}
			cs := checkGraphAgainstGit(t, r.dir, g, true)

			// Positions are dense and round-trip, and absent ids miss.
			for pos := uint32(0); pos < g.numCommits(); pos++ {
				id, err := g.commitID(pos)
				if err != nil {
					t.Fatal(err)
				}
				if p, ok := g.lookup(id); !ok || p != pos {
					t.Fatalf("lookup(commitID(%d)) = %d, %v", pos, p, ok)
				}
			}
			for i := range cs {
				miss := cs[i].id
				miss[19] ^= 0x5a
				if _, ok := g.lookup(miss); ok {
					if _, real := byIDHas(cs, miss); !real {
						t.Fatalf("lookup found absent id %x", miss)
					}
				}
			}
			if _, err := g.commitID(g.numCommits()); !errors.Is(err, errGraphPosition) {
				t.Fatalf("commitID(numCommits) err = %v", err)
			}
			if _, err := g.appendParents(nil, g.numCommits()); !errors.Is(err, errGraphPosition) {
				t.Fatalf("appendParents(numCommits) err = %v", err)
			}

			checkBloomAgainstGit(t, r.dir, g, cs, c)
		})
	}
}

func byIDHas(cs []refCommit, id oid) (int, bool) {
	for i := range cs {
		if cs[i].id == id {
			return i, true
		}
	}
	return 0, false
}

func TestGraphNone(t *testing.T) {
	r := newGTRepo(t)
	r.commit("a", "a\n")
	if _, err := openCommitGraph(r.objects()); !errors.Is(err, errNoGraph) {
		t.Fatalf("err = %v, want errNoGraph", err)
	}
}

// A split chain written with the default merge strategy still reads fine.
func TestGraphSplitDefaultMerge(t *testing.T) {
	r := newGTRepo(t)
	for i := range 4 {
		r.commit(fmt.Sprintf("f%d", i), "x\n")
		r.git("commit-graph", "write", "--reachable", "--split", "--changed-paths")
	}
	g, err := openCommitGraph(r.objects())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.close() }()
	checkGraphAgainstGit(t, r.dir, g, false)
}

func TestGraphRefcount(t *testing.T) {
	r := newGTRepo(t)
	r.commit("a", "a\n")
	r.git("commit-graph", "write", "--reachable")
	g, err := openCommitGraph(r.objects())
	if err != nil {
		t.Fatal(err)
	}
	if !g.retain() {
		t.Fatal("retain failed on open graph")
	}
	if err := g.close(); err != nil { // owner drops its reference
		t.Fatal(err)
	}
	// Still usable: the reader holds a reference.
	if g.numCommits() != 1 {
		t.Fatal("numCommits")
	}
	if _, err := g.commitID(0); err != nil {
		t.Fatal(err)
	}
	if err := g.release(); err != nil {
		t.Fatal(err)
	}
	if g.retain() {
		t.Fatal("retain succeeded on closed graph")
	}
	if err := g.release(); err == nil {
		t.Fatal("over-release not reported")
	}
}

// close is idempotent: a second call must not drop a reader's reference.
func TestGraphCloseIdempotent(t *testing.T) {
	r := newGTRepo(t)
	r.commit("a", "a\n")
	r.git("commit-graph", "write", "--reachable")
	g, err := openCommitGraph(r.objects())
	if err != nil {
		t.Fatal(err)
	}
	if !g.retain() {
		t.Fatal("retain failed on open graph")
	}
	var unmapped atomic.Int32
	for i, u := range g.unmaps {
		g.unmaps[i] = func() error { unmapped.Add(1); return u() }
	}
	for range 3 {
		if err := g.close(); err != nil {
			t.Fatal(err)
		}
	}
	if unmapped.Load() != 0 {
		t.Fatal("unmapped while a reader holds a reference")
	}
	if g.retain() {
		t.Fatal("retain succeeded after close")
	}
	if _, err := g.commitID(0); err != nil {
		t.Fatal(err)
	}
	if err := g.release(); err != nil {
		t.Fatal(err)
	}
	if unmapped.Load() != int32(len(g.layers)) {
		t.Fatalf("unmapped %d files, want %d", unmapped.Load(), len(g.layers))
	}
	if err := g.close(); err != nil {
		t.Fatal(err)
	}
	if unmapped.Load() != int32(len(g.layers)) {
		t.Fatal("close after the last release unmapped again")
	}
}

// Readers retain, read and release while the owner closes (more than once).
// The files must never be unmapped while a reader holds a reference; a read
// of an unmapped file would also crash the test.
func TestGraphConcurrentCloseRetain(t *testing.T) {
	r := newGTRepo(t)
	for i := range 5 {
		r.commit(fmt.Sprintf("f%d", i), "x\n")
		r.git("commit-graph", "write", "--reachable", "--split=no-merge", "--changed-paths")
	}
	const readers = 16
	rounds := 20
	if testing.Short() {
		rounds = 3
	}
	for range rounds {
		g, err := openCommitGraph(r.objects())
		if err != nil {
			t.Fatal(err)
		}
		var active, unmapped atomic.Int32
		for i, u := range g.unmaps {
			g.unmaps[i] = func() error {
				if active.Load() != 0 {
					t.Error("unmapped while referenced")
				}
				unmapped.Store(1)
				return u()
			}
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		var retained atomic.Int64
		for range readers {
			wg.Go(func() {
				<-start
				for range 200 {
					if !g.retain() {
						return
					}
					retained.Add(1)
					active.Add(1)
					if unmapped.Load() != 0 {
						t.Error("reference granted on unmapped graph")
					}
					for pos := range g.numCommits() {
						id, err := g.commitID(pos)
						if err != nil {
							t.Error(err)
						}
						if got, ok := g.lookup(id); !ok || got != pos {
							t.Errorf("lookup(%d) = %d, %v", pos, got, ok)
						}
						if _, err := g.appendParents(nil, pos); err != nil {
							t.Error(err)
						}
						g.maybeChanged(pos, []byte("f1"))
					}
					active.Add(-1)
					if err := g.release(); err != nil {
						t.Error(err)
					}
				}
			})
		}
		close(start)
		for range 3 {
			wg.Go(func() {
				if err := g.close(); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if unmapped.Load() != 1 {
			t.Fatal("files never unmapped")
		}
		if g.retain() {
			t.Fatal("retain succeeded after close")
		}
	}
}

// A layer removed or rewritten while a graph is open must not affect it.
func TestGraphSurvivesRewrite(t *testing.T) {
	r := newGTRepo(t)
	r.stageOne()
	c := gcase{split: true, bloom: true}
	r.writeGraph(c)
	g, err := openCommitGraph(r.objects())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.close() }()
	before := g.numCommits()
	r.stageTwo()
	r.git("commit-graph", "write", "--reachable", "--changed-paths") // replaces the chain
	if st := statGraph(r.objects()); !st.single.exists || st.chain.exists {
		t.Fatalf("unexpected stamp %+v", st)
	}
	if g.numCommits() != before {
		t.Fatal("numCommits changed")
	}
	for pos := uint32(0); pos < before; pos++ {
		if _, err := g.commitID(pos); err != nil {
			t.Fatal(err)
		}
	}
}

// setLayerHook installs graphBeforeLayerOpen for the duration of the test.
func setLayerHook(t *testing.T, f func(path string)) {
	t.Helper()
	graphBeforeLayerOpen = f
	t.Cleanup(func() { graphBeforeLayerOpen = nil })
}

// splitChainRepo has a graph chain of several layers.
func splitChainRepo(t *testing.T) *gtRepo {
	r := newGTRepo(t)
	for i := range 3 {
		r.commit(fmt.Sprintf("f%d", i), "x\n")
		r.git("commit-graph", "write", "--reachable", "--split=no-merge", "--changed-paths")
	}
	return r
}

// A concurrent merging "commit-graph write --split" deletes the layers named
// by the chain just read: opening starts again and succeeds.
func TestGraphOpenRetriesVanishedLayer(t *testing.T) {
	r := splitChainRepo(t)
	calls := 0
	setLayerHook(t, func(string) {
		if calls++; calls == 1 {
			r.git("commit-graph", "write", "--reachable", "--split=replace", "--changed-paths", "--expire-time=now")
		}
	})
	g, err := openCommitGraph(r.objects())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.close() }()
	if g.numLayers() != 1 {
		t.Fatalf("layers = %d, want the replaced chain of 1", g.numLayers())
	}
	checkGraphAgainstGit(t, r.dir, g, false)
}

// Layers that keep vanishing end in errNoGraph, which callers treat as
// "read the objects instead", wrapping the cause.
func TestGraphOpenGivesUpOnVanishedLayer(t *testing.T) {
	r := splitChainRepo(t)
	calls := 0
	setLayerHook(t, func(path string) {
		calls++
		_ = os.Remove(path)
	})
	g, err := openCommitGraph(r.objects())
	if err == nil {
		_ = g.close()
		t.Fatal("opened a graph with a missing layer")
	}
	if !errors.Is(err, errNoGraph) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want errNoGraph wrapping ErrNotExist", err)
	}
	if calls != 1+graphOpenRetries {
		t.Fatalf("%d attempts, want %d", calls, 1+graphOpenRetries)
	}
}

// Opening in a loop while git rewrites the chain: success or errNoGraph only.
func TestGraphOpenStressSplitWrite(t *testing.T) {
	r := newGTRepo(t)
	r.commit("a", "a\n")
	r.git("commit-graph", "write", "--reachable", "--split", "--changed-paths")
	commits := 150
	if testing.Short() {
		commits = 10
	}
	var done atomic.Bool
	writerErr := make(chan error, 1)
	go func() {
		defer done.Store(true)
		for i := range commits {
			for _, args := range [][]string{
				{"commit", "-q", "--allow-empty", "-m", fmt.Sprint("s", i)},
				{"commit-graph", "write", "--reachable", "--split", "--changed-paths", "--expire-time=now"},
			} {
				cmd := exec.Command("git", args...)
				cmd.Dir = r.dir
				cmd.Env = graphGitEnv(0)
				if out, err := cmd.CombinedOutput(); err != nil {
					writerErr <- fmt.Errorf("git %v: %v\n%s", args, err, out)
					return
				}
			}
		}
		writerErr <- nil
	}()
	// Count the layer opens beyond the chain length: retries.
	var layerOpens atomic.Int64
	setLayerHook(t, func(string) { layerOpens.Add(1) })
	var opened, none int
	deadline := time.Now().Add(2 * time.Minute)
	for !done.Load() && time.Now().Before(deadline) {
		g, err := openCommitGraph(r.objects())
		switch {
		case err == nil:
			opened++
			if g.numCommits() == 0 {
				t.Error("empty graph")
			}
			_ = g.close()
		case errors.Is(err, errNoGraph):
			none++
		default:
			t.Fatalf("open: %v", err)
		}
	}
	if err := <-writerErr; err != nil {
		t.Fatal(err)
	}
	t.Logf("opened %d, no graph %d, layer opens %d", opened, none, layerOpens.Load())
	if opened == 0 {
		t.Error("never opened a graph")
	}
}

func TestGraphStamp(t *testing.T) {
	r := newGTRepo(t)
	r.commit("a", "a\n")
	s0 := statGraph(r.objects())
	if s0.single.exists || s0.chain.exists {
		t.Fatalf("stamp = %+v", s0)
	}
	r.git("commit-graph", "write", "--reachable", "--split")
	s1 := statGraph(r.objects())
	if !s1.chain.exists || s1 == s0 {
		t.Fatalf("stamp = %+v", s1)
	}
	r.commit("b", "b\n")
	r.git("commit-graph", "write", "--reachable", "--split=no-merge")
	if s2 := statGraph(r.objects()); s2 == s1 {
		t.Fatal("stamp did not change after a new layer")
	}
}

// ---- synthetic graph writer ----------------------------------------------

// synthLayer builds a commit-graph layer byte for byte, for cases git does
// not produce (generation overflow, wide octopus, corruption).
type synthLayer struct {
	ids      []oid // must be sorted
	trees    []oid
	parents  [][]uint32 // global positions
	times    []int64
	levels   []uint32
	gda2     []uint32 // nil: no chunk
	gdo2     []uint64
	bidx     []uint32
	bdat     []byte // including the 12-byte header
	base     []oid  // base graph checksums
	skipEdge bool
}

func (s *synthLayer) bytes() []byte {
	n := len(s.ids)
	var oidl, cdat, edge, gda2, gdo2, bidx, base bytes.Buffer
	fan := make([]uint32, 256)
	for _, id := range s.ids {
		fan[id[0]]++
		oidl.Write(id[:])
	}
	for i := 1; i < 256; i++ {
		fan[i] += fan[i-1]
	}
	var fanout bytes.Buffer
	for _, v := range fan {
		_ = binary.Write(&fanout, binary.BigEndian, v)
	}
	be := binary.BigEndian
	put32 := func(b *bytes.Buffer, v uint32) { _ = binary.Write(b, be, v) }
	for i := range n {
		var tree oid
		if i < len(s.trees) {
			tree = s.trees[i]
		}
		cdat.Write(tree[:])
		var ps []uint32
		if i < len(s.parents) {
			ps = s.parents[i]
		}
		p1, p2 := uint32(graphParentNone), uint32(graphParentNone)
		if len(ps) > 0 {
			p1 = ps[0]
		}
		switch {
		case len(ps) == 2:
			p2 = ps[1]
		case len(ps) > 2:
			p2 = graphParentExtra | uint32(edge.Len()/4)
			for j, p := range ps[1:] {
				if j == len(ps)-2 {
					p |= graphEdgeLast
				}
				put32(&edge, p)
			}
		}
		put32(&cdat, p1)
		put32(&cdat, p2)
		var tm int64
		if i < len(s.times) {
			tm = s.times[i]
		}
		var lv uint32 = 1
		if i < len(s.levels) {
			lv = s.levels[i]
		}
		put32(&cdat, lv<<2|uint32(tm>>32)&3)
		put32(&cdat, uint32(tm))
	}
	for _, v := range s.gda2 {
		put32(&gda2, v)
	}
	for _, v := range s.gdo2 {
		_ = binary.Write(&gdo2, be, v)
	}
	for _, v := range s.bidx {
		put32(&bidx, v)
	}
	for _, b := range s.base {
		base.Write(b[:])
	}

	type chunk struct {
		id   uint32
		data []byte
	}
	chunks := []chunk{{chunkOIDF, fanout.Bytes()}, {chunkOIDL, oidl.Bytes()}, {chunkCDAT, cdat.Bytes()}}
	if edge.Len() > 0 && !s.skipEdge {
		chunks = append(chunks, chunk{chunkEDGE, edge.Bytes()})
	}
	if s.gda2 != nil {
		chunks = append(chunks, chunk{chunkGDA2, gda2.Bytes()})
	}
	if s.gdo2 != nil {
		chunks = append(chunks, chunk{chunkGDO2, gdo2.Bytes()})
	}
	if s.bdat != nil {
		chunks = append(chunks, chunk{chunkBIDX, bidx.Bytes()}, chunk{chunkBDAT, s.bdat})
	}
	if len(s.base) > 0 {
		chunks = append(chunks, chunk{chunkBASE, base.Bytes()})
	}

	var out bytes.Buffer
	out.WriteString("CGPH")
	out.Write([]byte{1, 1, byte(len(chunks)), byte(len(s.base))})
	off := uint64(graphHeaderSize + (len(chunks)+1)*graphChunkEntrySize)
	for _, c := range chunks {
		put32(&out, c.id)
		_ = binary.Write(&out, be, off)
		off += uint64(len(c.data))
	}
	put32(&out, 0)
	_ = binary.Write(&out, be, off)
	for _, c := range chunks {
		out.Write(c.data)
	}
	out.Write(make([]byte, graphHashSize))
	return out.Bytes()
}

func synthIDs(n int) []oid {
	ids := make([]oid, n)
	for i := range ids {
		ids[i][0] = byte(i * 251 / max(n, 1))
		binary.BigEndian.PutUint32(ids[i][16:], uint32(i))
	}
	sort.Slice(ids, func(a, b int) bool { return bytes.Compare(ids[a][:], ids[b][:]) < 0 })
	return ids
}

func mustParseSynth(t testing.TB, b []byte) *commitGraph {
	t.Helper()
	l, err := parseGraphLayer(b)
	if err != nil {
		t.Fatal(err)
	}
	g, err := assembleGraph([]*graphLayer{l}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGraphSyntheticOctopusAndOverflow(t *testing.T) {
	const n = 12
	s := &synthLayer{ids: synthIDs(n)}
	s.parents = make([][]uint32, n)
	s.times = make([]int64, n)
	s.gda2 = make([]uint32, n)
	for i := range n {
		s.times[i] = 1_000_000 + int64(i)
		s.gda2[i] = uint32(i)
	}
	s.parents[11] = []uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10} // 11 parents
	s.parents[10] = []uint32{9}
	s.parents[9] = []uint32{8, 7}
	s.parents[8] = []uint32{7, 6, 5}
	// Overflow: commit 3 has a 40-bit offset, and a time over 32 bits.
	s.gda2[3] = graphGenOverflow | 1
	s.gdo2 = []uint64{7, 1 << 40}
	s.times[4] = 1<<33 + 5

	g := mustParseSynth(t, s.bytes())
	defer func() { _ = g.close() }()
	var buf [4]uint32
	for pos := range uint32(n) {
		ps, err := g.appendParents(buf[:0], pos)
		if err != nil {
			t.Fatal(err)
		}
		want := s.parents[pos]
		if len(ps) != len(want) {
			t.Fatalf("pos %d: parents %v, want %v", pos, ps, want)
		}
		for i := range ps {
			if ps[i] != want[i] {
				t.Fatalf("pos %d: parents %v, want %v", pos, ps, want)
			}
		}
	}
	if !g.hasGenerationV2() {
		t.Fatal("expected generation v2")
	}
	if v, err := g.generation(3); err != nil || v != uint64(s.times[3])+1<<40 {
		t.Fatalf("overflow generation = %d, %v", v, err)
	}
	if v, err := g.generation(2); err != nil || v != uint64(s.times[2])+2 {
		t.Fatalf("generation(2) = %d, %v", v, err)
	}
	if v, err := g.commitTime(4); err != nil || v != 1<<33+5 {
		t.Fatalf("34-bit time = %d, %v", v, err)
	}
}

func TestGraphSyntheticCorruption(t *testing.T) {
	const n = 6
	mk := func(f func(s *synthLayer)) *synthLayer {
		s := &synthLayer{ids: synthIDs(n), parents: make([][]uint32, n), times: make([]int64, n)}
		s.parents[5] = []uint32{0, 1, 2}
		if f != nil {
			f(s)
		}
		return s
	}
	t.Run("ok", func(t *testing.T) {
		g := mustParseSynth(t, mk(nil).bytes())
		_ = g.close()
	})
	t.Run("parent out of range", func(t *testing.T) {
		g := mustParseSynth(t, mk(func(s *synthLayer) { s.parents[2] = []uint32{9} }).bytes())
		if _, err := g.appendParents(nil, 2); !errors.Is(err, errGraphCorrupt) {
			t.Fatalf("err = %v", err)
		}
		// A parent equal to the commit's own layer end is rejected, a
		// parent in a later position of the same layer is accepted (the
		// graph is not required to be topologically ordered by id).
		if _, err := g.appendParents(nil, 5); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("edge list overrun", func(t *testing.T) {
		g := mustParseSynth(t, mk(func(s *synthLayer) {
			s.parents[5] = []uint32{0, 1, 2}
		}).bytes())
		l := &g.layers[0]
		// Clear the "last" bit of the final EDGE entry.
		last := len(l.edge) - 4
		mutable := append([]byte(nil), l.edge...)
		mutable[last] &^= 0x80
		l.edge = mutable
		if _, err := g.appendParents(nil, 5); !errors.Is(err, errGraphCorrupt) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing edge chunk", func(t *testing.T) {
		g := mustParseSynth(t, mk(func(s *synthLayer) { s.skipEdge = true }).bytes())
		if _, err := g.appendParents(nil, 5); !errors.Is(err, errGraphCorrupt) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("overflow index out of range", func(t *testing.T) {
		g := mustParseSynth(t, mk(func(s *synthLayer) {
			s.gda2 = make([]uint32, n)
			s.gda2[1] = graphGenOverflow | 3
			s.gdo2 = []uint64{1}
		}).bytes())
		if _, err := g.generation(1); !errors.Is(err, errGraphCorrupt) {
			t.Fatalf("err = %v", err)
		}
		if _, err := g.generation(0); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("second parent without first", func(t *testing.T) {
		g := mustParseSynth(t, mk(nil).bytes())
		l := &g.layers[0]
		rec := append([]byte(nil), l.cdat...)
		binary.BigEndian.PutUint32(rec[graphHashSize:], graphParentNone)
		binary.BigEndian.PutUint32(rec[graphHashSize+4:], 1)
		l.cdat = rec
		if _, err := g.appendParents(nil, 0); !errors.Is(err, errGraphCorrupt) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestGraphParseErrors(t *testing.T) {
	good := (&synthLayer{ids: synthIDs(4)}).bytes()
	if _, err := parseGraphLayer(good); err != nil {
		t.Fatal(err)
	}
	mut := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	cases := map[string][]byte{
		"empty":         nil,
		"short":         good[:20],
		"signature":     mut(func(b []byte) []byte { b[0] = 'X'; return b }),
		"hash version":  mut(func(b []byte) []byte { b[5] = 9; return b }),
		"chunk count":   mut(func(b []byte) []byte { b[6] = 255; return b }),
		"offset past":   mut(func(b []byte) []byte { binary.BigEndian.PutUint64(b[12:], 1<<62); return b }),
		"offset behind": mut(func(b []byte) []byte { binary.BigEndian.PutUint64(b[12:], 1); return b }),
		"dup chunk":     mut(func(b []byte) []byte { copy(b[8+12:], b[8:12]); return b }),
		"fanout order":  mut(func(b []byte) []byte { binary.BigEndian.PutUint32(b[(8+5*12)+4:], 99); return b }),
		"truncated":     good[:len(good)-30],
	}
	for name, b := range cases {
		if _, err := parseGraphLayer(b); !errors.Is(err, errGraphCorrupt) {
			t.Errorf("%s: err = %v, want corrupt", name, err)
		}
	}
	sha256 := mut(func(b []byte) []byte { b[5] = 2; return b })
	if _, err := parseGraphLayer(sha256); !errors.Is(err, errGraphUnsupported) {
		t.Errorf("sha256: err = %v", err)
	}
	v2 := mut(func(b []byte) []byte { b[4] = 2; return b })
	if _, err := parseGraphLayer(v2); !errors.Is(err, errGraphUnsupported) {
		t.Errorf("version 2: err = %v", err)
	}
	// Every truncation and every single-byte corruption of the header and
	// chunk table must be handled without panicking.
	for i := range good {
		_, _ = parseGraphLayer(good[:i])
	}
	for i := range min(len(good), 8+6*12) {
		for _, v := range []byte{0, 1, 0x7f, 0x80, 0xff} {
			b := append([]byte(nil), good...)
			b[i] = v
			exerciseGraphBytes(b)
		}
	}
}

func TestGraphChainParse(t *testing.T) {
	h1 := strings.Repeat("ab", 20)
	h2 := strings.Repeat("01", 20)
	names, err := parseGraphChain([]byte(h1 + "\n" + h2 + "\n"))
	if err != nil || len(names) != 2 || names[1][0] != 1 {
		t.Fatalf("names = %v, err = %v", names, err)
	}
	for _, bad := range []string{h1, h1 + "\r\n", strings.Repeat("zz", 20) + "\n", h1 + "\n" + "x\n"} {
		if _, err := parseGraphChain([]byte(bad)); !errors.Is(err, errGraphCorrupt) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	if _, err := parseGraphChain(nil); !errors.Is(err, errNoGraph) {
		t.Errorf("empty chain: err = %v", err)
	}
}

func TestGraphChainMismatch(t *testing.T) {
	r := newGTRepo(t)
	r.commit("a", "a\n")
	r.git("commit-graph", "write", "--reachable", "--split=no-merge")
	r.commit("b", "b\n")
	r.git("commit-graph", "write", "--reachable", "--split=no-merge")
	chainPath := filepath.Join(r.objects(), "info", "commit-graphs", "commit-graph-chain")
	raw, err := os.ReadFile(chainPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(raw))
	if len(lines) != 2 {
		t.Fatalf("chain = %q", raw)
	}
	g, err := openCommitGraph(r.objects())
	if err != nil {
		t.Fatal(err)
	}
	_ = g.close()

	// Reverse: layer 0 would be the top layer, which declares a base.
	if err := os.Chmod(chainPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chainPath, []byte(lines[1]+"\n"+lines[0]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openCommitGraph(r.objects()); !errors.Is(err, errGraphCorrupt) {
		t.Fatalf("reversed chain: err = %v", err)
	}
	// Top layer alone declares one base but the chain has none.
	if err := os.WriteFile(chainPath, []byte(lines[1]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openCommitGraph(r.objects()); !errors.Is(err, errGraphCorrupt) {
		t.Fatalf("truncated chain: err = %v", err)
	}
	// A missing layer file is an error, not a panic.
	if err := os.WriteFile(chainPath, []byte(strings.Repeat("0", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openCommitGraph(r.objects()); err == nil {
		t.Fatal("missing layer accepted")
	}
	// Two layers whose base list names the wrong checksum.
	if err := os.WriteFile(chainPath, []byte(lines[0]+"\n"+lines[0]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openCommitGraph(r.objects()); !errors.Is(err, errGraphCorrupt) {
		t.Fatalf("duplicated layer: err = %v", err)
	}
}

// A layer file whose trailer is not the name the chain gives it is rejected.
func TestGraphLayerChecksumMustMatchName(t *testing.T) {
	mk := func(sum byte, base []oid) *graphLayer {
		s := &synthLayer{ids: synthIDs(3), base: base}
		b := s.bytes()
		for i := len(b) - graphHashSize; i < len(b); i++ {
			b[i] = sum
		}
		l, err := parseGraphLayer(b)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	name := func(b byte) [graphHashSize]byte { return [graphHashSize]byte(bytes.Repeat([]byte{b}, graphHashSize)) }
	chain := func(a, b byte) (*commitGraph, error) {
		return assembleGraph([]*graphLayer{mk(0xa1, nil), mk(0xb2, []oid{name(0xa1)})},
			[][graphHashSize]byte{name(a), name(b)})
	}
	if _, err := chain(0xa1, 0xb2); err != nil {
		t.Fatalf("matching names: %v", err)
	}
	if _, err := chain(0xa1, 0xb3); !errors.Is(err, errGraphCorrupt) {
		t.Errorf("top layer renamed: err = %v", err)
	}
	// The base list of the top layer names the base correctly, but the base
	// file itself carries another checksum.
	if _, err := assembleGraph([]*graphLayer{mk(0xee, nil), mk(0xb2, []oid{name(0xa1)})},
		[][graphHashSize]byte{name(0xa1), name(0xb2)}); !errors.Is(err, errGraphCorrupt) {
		t.Errorf("base layer with another checksum: err = %v", err)
	}
	// Several layers need chain names.
	if _, err := assembleGraph([]*graphLayer{mk(1, nil), mk(2, []oid{name(1)})}, nil); !errors.Is(err, errGraphCorrupt) {
		t.Errorf("two layers without names: err = %v", err)
	}
}

// On disk: a layer copied under another name is not accepted as that layer.
func TestGraphRenamedLayerRejected(t *testing.T) {
	r := newGTRepo(t)
	r.commit("a", "a\n")
	r.git("commit-graph", "write", "--reachable", "--split")
	dir := filepath.Join(r.objects(), "info", "commit-graphs")
	chainPath := filepath.Join(dir, "commit-graph-chain")
	raw, err := os.ReadFile(chainPath)
	if err != nil {
		t.Fatal(err)
	}
	good := strings.TrimSpace(string(raw))
	fake := strings.Repeat("0", 40)
	b, err := os.ReadFile(filepath.Join(dir, "graph-"+good+".graph"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "graph-"+fake+".graph"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(chainPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chainPath, []byte(fake+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if g, err := openCommitGraph(r.objects()); !errors.Is(err, errGraphCorrupt) {
		if err == nil {
			_ = g.close()
		}
		t.Fatalf("err = %v, want corrupt", err)
	}
}

// Optional chunks of the wrong size disable their feature only.
func TestGraphOptionalChunkSizes(t *testing.T) {
	const n = 4
	hdr := binary.BigEndian.AppendUint32(nil, 2)
	hdr = binary.BigEndian.AppendUint32(hdr, 7)
	hdr = binary.BigEndian.AppendUint32(hdr, 10)
	mk := func() *synthLayer {
		s := &synthLayer{ids: synthIDs(n), parents: make([][]uint32, n), times: make([]int64, n), levels: []uint32{5, 6, 7, 8}}
		s.times = []int64{100, 200, 300, 400}
		s.parents[1] = []uint32{0}
		s.gda2 = []uint32{0, 1, 2, 3}
		s.bdat = append(hdr[:12:12], 0x00)
		s.bidx = []uint32{1, 1, 1, 1}
		return s
	}
	t.Run("ok", func(t *testing.T) {
		g := mustParseSynth(t, mk().bytes())
		defer func() { _ = g.close() }()
		if !g.hasGenerationV2() || !g.layers[0].hasBloom {
			t.Fatal("features missing on a good layer")
		}
		if v, _ := g.generation(1); v != 201 {
			t.Fatalf("generation = %d, want 201", v)
		}
	})
	t.Run("GDA2", func(t *testing.T) {
		for _, size := range []int{n - 1, n + 1} {
			s := mk()
			s.gda2 = make([]uint32, size)
			g := mustParseSynth(t, s.bytes())
			if g.hasGenerationV2() {
				t.Errorf("GDA2 of %d entries accepted", size)
			}
			if v, err := g.generation(1); err != nil || v != 6 {
				t.Errorf("generation = %d, %v, want the level 6", v, err)
			}
			if !g.layers[0].hasBloom {
				t.Error("a bad GDA2 disabled the Bloom filters")
			}
			if _, err := g.appendParents(nil, 1); err != nil {
				t.Error(err)
			}
			_ = g.close()
		}
	})
	t.Run("BIDX", func(t *testing.T) {
		s := mk()
		s.bidx = s.bidx[:n-1]
		g := mustParseSynth(t, s.bytes())
		defer func() { _ = g.close() }()
		if g.layers[0].hasBloom || g.hasBloom(0) {
			t.Error("short BIDX accepted")
		}
		if !g.hasGenerationV2() {
			t.Error("a bad BIDX disabled the generation")
		}
	})
	t.Run("BDAT", func(t *testing.T) {
		s := mk()
		s.bdat = s.bdat[:bloomHeaderSize-1]
		g := mustParseSynth(t, s.bytes())
		defer func() { _ = g.close() }()
		if g.layers[0].hasBloom || g.hasBloom(0) {
			t.Error("short BDAT accepted")
		}
		if !g.hasGenerationV2() {
			t.Error("a bad BDAT disabled the generation")
		}
	})
}

func TestGraphCorruptFiles(t *testing.T) {
	r := newGTRepo(t)
	r.commit("a", "a\n")
	r.git("commit-graph", "write", "--reachable", "--changed-paths")
	path := filepath.Join(r.objects(), "info", "commit-graph")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, b := range [][]byte{good[:10], {}, []byte("CGPH"), bytes.Repeat([]byte{0xff}, 200)} {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := openCommitGraph(r.objects()); err == nil {
			t.Fatalf("accepted %d corrupt bytes", len(b))
		}
	}
	// A directory in place of the file must not panic or be mapped.
	_ = os.Remove(path)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openCommitGraph(r.objects()); err == nil {
		t.Fatal("directory accepted")
	}
}

// ---- fuzzing --------------------------------------------------------------

// exerciseGraphBytes runs every accessor over a layer parsed from data, alone
// and stacked on itself, and must never panic.
func exerciseGraphBytes(data []byte) {
	l, err := parseGraphLayer(data)
	if err != nil {
		return
	}
	one := *l
	one.numBase = 0
	two := one
	two.base = one.n
	for _, g := range []*commitGraph{
		{layers: []graphLayer{one}, total: one.n, genV2: one.gda2 != nil},
		{layers: []graphLayer{one, two}, total: 2 * one.n, genV2: one.gda2 != nil},
	} {
		exerciseGraph(g)
	}
}

// exerciseGraph runs every accessor over (up to 256 positions of) g.
func exerciseGraph(g *commitGraph) {
	var buf [4]uint32
	n := min(g.total, 256)
	for pos := range n {
		id, _ := g.commitID(pos)
		g.lookup(id)
		_, _ = g.rootTree(pos)
		_, _ = g.commitTime(pos)
		_, _ = g.topoLevel(pos)
		_, _ = g.generation(pos)
		_, _ = g.appendParents(buf[:0], pos)
		g.maybeChanged(pos, []byte("src/main.go"))
		g.maybeChanged(pos, []byte("caf\xc3\xa9/"))
		g.hasBloom(pos)
	}
	_, _ = g.commitID(g.total)
	_, _ = g.appendParents(nil, g.total+1)
	g.maybeChanged(g.total, nil)
	g.lookup(oid{0xff, 0xff})
	g.lookup(oid{})
}

func graphSeedFiles(tb testing.TB) map[string][]byte {
	tb.Helper()
	entries, err := os.ReadDir(filepath.Join("testdata", "graph"))
	if err != nil {
		tb.Fatal(err)
	}
	seeds := map[string][]byte{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".graph") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("testdata", "graph", e.Name()))
		if err != nil {
			tb.Fatal(err)
		}
		seeds[e.Name()] = b
	}
	if len(seeds) == 0 {
		tb.Fatal("no fuzz seeds in testdata/graph")
	}
	return seeds
}

func FuzzGraphFile(f *testing.F) {
	for _, b := range graphSeedFiles(f) {
		f.Add(b)
		f.Add(b[:len(b)/2])
	}
	f.Fuzz(func(t *testing.T, data []byte) { exerciseGraphBytes(data) })
}

func FuzzGraphChain(f *testing.F) {
	f.Add([]byte(strings.Repeat("ab", 20) + "\n"))
	f.Add([]byte(strings.Repeat("ab", 20) + "\n" + strings.Repeat("cd", 20) + "\n"))
	f.Add([]byte("\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		names, err := parseGraphChain(data)
		if err == nil && len(names)*41 != len(data) {
			t.Fatalf("parsed %d names from %d bytes", len(names), len(data))
		}
	})
}

// graphChainSeeds returns, for each committed split chain, the chain file
// content and its layer files (base first).
func graphChainSeeds(tb testing.TB) (chains [][]byte, layers [][2][]byte) {
	tb.Helper()
	seeds := graphSeedFiles(tb)
	for _, prefix := range []string{"split-bloom-v1", "split-bloom-v2", "split-gen1"} {
		l0, l1 := seeds[prefix+"-layer0.graph"], seeds[prefix+"-layer1.graph"]
		if l0 == nil || l1 == nil {
			tb.Fatalf("seed %s missing", prefix)
		}
		chain := hex.EncodeToString(l0[len(l0)-graphHashSize:]) + "\n" +
			hex.EncodeToString(l1[len(l1)-graphHashSize:]) + "\n"
		chains = append(chains, []byte(chain))
		layers = append(layers, [2][]byte{l0, l1})
	}
	return chains, layers
}

// assembleChainBytes is openCommitGraphChain on in-memory files: layer i of
// the chain is files[i%len(files)].
func assembleChainBytes(chain []byte, files ...[]byte) (*commitGraph, error) {
	names, err := parseGraphChain(chain)
	if err != nil {
		return nil, err
	}
	layers := make([]*graphLayer, 0, len(names))
	for i := range names {
		l, err := parseGraphLayer(files[i%len(files)])
		if err != nil {
			return nil, err
		}
		layers = append(layers, l)
	}
	return assembleGraph(layers, names)
}

func TestGraphChainSeedsAssemble(t *testing.T) {
	chains, layers := graphChainSeeds(t)
	for i := range chains {
		g, err := assembleChainBytes(chains[i], layers[i][0], layers[i][1])
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if g.numLayers() != 2 || g.numCommits() == 0 {
			t.Errorf("seed %d: %d layers, %d commits", i, g.numLayers(), g.numCommits())
		}
		exerciseGraph(g)
		// Swapped layers or a damaged name must be refused, not crash.
		if _, err := assembleChainBytes(chains[i], layers[i][1], layers[i][0]); err == nil {
			t.Errorf("seed %d: swapped layers accepted", i)
		}
		bad := append([]byte(nil), chains[i]...)
		bad[3] ^= 1
		if _, err := assembleChainBytes(bad, layers[i][0], layers[i][1]); !errors.Is(err, errGraphCorrupt) {
			t.Errorf("seed %d: wrong layer name: err = %v", i, err)
		}
	}
}

// FuzzGraphAssemble reaches parseGraphChain, parseGraphLayer and
// assembleGraph together: a chain file and two layer files (used round-robin
// for the layers the chain lists).
func FuzzGraphAssemble(f *testing.F) {
	chains, layers := graphChainSeeds(f)
	for i := range chains {
		f.Add(chains[i], layers[i][0], layers[i][1])
		f.Add(chains[i][:41], layers[i][0], layers[i][1]) // base layer alone
		f.Add(append(chains[i][:41:41], chains[i][:41]...), layers[i][0], layers[i][0])
	}
	f.Add([]byte(strings.Repeat("00", 20)+"\n"), (&synthLayer{ids: synthIDs(3)}).bytes(), []byte{})
	f.Fuzz(func(t *testing.T, chain, l0, l1 []byte) {
		g, err := assembleChainBytes(chain, l0, l1)
		if err != nil {
			return
		}
		if g.numLayers() == 0 || g.numLayers() > graphMaxLayers {
			t.Fatalf("%d layers", g.numLayers())
		}
		exerciseGraph(g)
	})
}

// TestGraphSeedsParse checks the committed seeds stay valid files.
func TestGraphSeedsParse(t *testing.T) {
	for name, b := range graphSeedFiles(t) {
		l, err := parseGraphLayer(b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		exerciseGraphBytes(b)
		_ = l
	}
}

// TestWriteGraphSeeds regenerates testdata/graph with the git binary:
//
//	ERMITE_GIT_UPDATE_SEEDS=1 go test -run TestWriteGraphSeeds
func TestWriteGraphSeeds(t *testing.T) {
	if os.Getenv("ERMITE_GIT_UPDATE_SEEDS") == "" {
		t.Skip("set ERMITE_GIT_UPDATE_SEEDS=1 to regenerate testdata/graph")
	}
	out := filepath.Join("testdata", "graph")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	save := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(out, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copyLayers := func(prefix string, r *gtRepo) {
		dir := filepath.Join(r.objects(), "info")
		if b, err := os.ReadFile(filepath.Join(dir, "commit-graph")); err == nil {
			save(prefix+".graph", b)
			return
		}
		chain, err := os.ReadFile(filepath.Join(dir, "commit-graphs", "commit-graph-chain"))
		if err != nil {
			t.Fatal(err)
		}
		for i, h := range strings.Fields(string(chain)) {
			b, err := os.ReadFile(filepath.Join(dir, "commit-graphs", "graph-"+h+".graph"))
			if err != nil {
				t.Fatal(err)
			}
			save(fmt.Sprintf("%s-layer%d.graph", prefix, i), b)
		}
	}
	for _, c := range []gcase{
		{name: "single-nobloom"},
		{name: "single-bloom-v2", bloom: true, cfg: []string{"commitGraph.changedPathsVersion=2"}},
		{name: "split-bloom-v2", split: true, bloom: true, cfg: []string{"commitGraph.changedPathsVersion=2"}},
		{name: "split-bloom-v1", split: true, bloom: true, cfg: []string{"commitGraph.changedPathsVersion=1"}},
		{name: "split-gen1", split: true, cfg: []string{"commitGraph.generationVersion=1"}},
	} {
		r := newGTRepo(t)
		r.stageOne()
		r.writeGraph(c)
		r.stageTwo()
		r.writeGraph(c)
		copyLayers(c.name, r)
	}
	// Synthetic: wide octopus, overflow generation, Bloom edge cases.
	s := &synthLayer{ids: synthIDs(12), parents: make([][]uint32, 12), times: make([]int64, 12)}
	s.parents[11] = []uint32{0, 1, 2, 3, 4, 5}
	s.gda2 = make([]uint32, 12)
	s.gda2[3] = graphGenOverflow
	s.gdo2 = []uint64{1 << 40}
	hdr := binary.BigEndian.AppendUint32(nil, 2)
	hdr = binary.BigEndian.AppendUint32(hdr, 7)
	hdr = binary.BigEndian.AppendUint32(hdr, 10)
	s.bdat = append(hdr, 0x00, 0xff, 0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0)
	s.bidx = []uint32{1, 2, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10}
	save("synthetic.graph", s.bytes())
}

// ---- benchmarks -----------------------------------------------------------

// fastImportRepo creates a bare repo with n commits (a merge every 50) using
// git fast-import, and writes its split commit-graph.
func fastImportRepo(tb testing.TB, n int) string {
	tb.Helper()
	requireGit(tb)
	dir := tb.TempDir()
	graphGit(tb, dir, 0, "init", "-q", "--bare", "-b", "main")
	cmd := exec.Command("git", "fast-import", "--quiet")
	cmd.Dir = dir
	cmd.Env = graphGitEnv(0)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		tb.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		tb.Fatal(err)
	}
	w := bufio.NewWriter(in)
	for i := 1; i <= n; i++ {
		msg := "commit " + strconv.Itoa(i)
		_, _ = fmt.Fprintf(w, "commit refs/heads/main\nmark :%d\ncommitter T <t@example.com> %d +0000\ndata %d\n%s\n", i, 1_600_000_000+i*30, len(msg), msg)
		if i > 1 {
			_, _ = fmt.Fprintf(w, "from :%d\n", i-1)
		}
		if i > 60 && i%50 == 0 {
			_, _ = fmt.Fprintf(w, "merge :%d\n", i-30)
		}
		for _, p := range []string{
			fmt.Sprintf("d%d/f%d.txt", i%17, i%5),
			fmt.Sprintf("pkg%d/sub%d/g%d.go", i%7, i%3, i),
		} {
			body := "v" + strconv.Itoa(i) + "\n"
			_, _ = fmt.Fprintf(w, "M 644 inline %s\ndata %d\n%s", p, len(body), body)
		}
		_, _ = w.WriteString("\n")
	}
	if err := w.Flush(); err != nil {
		tb.Fatal(err)
	}
	_ = in.Close()
	if err := cmd.Wait(); err != nil {
		tb.Fatalf("fast-import: %v\n%s", err, stderr.String())
	}
	graphGit(tb, dir, 0, "commit-graph", "write", "--reachable", "--changed-paths", "--split")
	return dir
}

func benchGraph(b *testing.B) (*commitGraph, []oid) {
	dir := fastImportRepo(b, 5000)
	g, err := openCommitGraph(filepath.Join(dir, "objects"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = g.close() })
	ids := make([]oid, g.numCommits())
	for i := range ids {
		ids[i], _ = g.commitID(uint32(i))
	}
	return g, ids
}

func BenchmarkGraphLookup(b *testing.B) {
	g, ids := benchGraph(b)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if _, ok := g.lookup(ids[i]); !ok {
			b.Fatal("miss")
		}
		if i++; i == len(ids) {
			i = 0
		}
	}
}

func BenchmarkGraphParents(b *testing.B) {
	g, _ := benchGraph(b)
	var buf [8]uint32
	n := g.numCommits()
	b.ReportAllocs()
	var pos uint32
	for b.Loop() {
		if _, err := g.appendParents(buf[:0], pos); err != nil {
			b.Fatal(err)
		}
		if pos++; pos == n {
			pos = 0
		}
	}
}

func BenchmarkGraphCommitData(b *testing.B) {
	g, _ := benchGraph(b)
	n := g.numCommits()
	b.ReportAllocs()
	var pos uint32
	for b.Loop() {
		if _, err := g.generation(pos); err != nil {
			b.Fatal(err)
		}
		if _, err := g.commitTime(pos); err != nil {
			b.Fatal(err)
		}
		if _, err := g.rootTree(pos); err != nil {
			b.Fatal(err)
		}
		if pos++; pos == n {
			pos = 0
		}
	}
}
