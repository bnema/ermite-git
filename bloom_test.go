// SPDX-License-Identifier: MIT

package git

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMurmur3Vectors(t *testing.T) {
	// Reference values of MurmurHash3_x86_32 (same vectors as git t0095).
	for _, c := range []struct {
		in   string
		seed uint32
		want uint32
	}{
		{"", 0, 0},
		{"", 1, 0x514e28b7},
		{"", 0xffffffff, 0x81f16f39},
		{"test", 0, 0xba6bd213},
		{"test", 0x9747b28c, 0x704b81dc},
		{"Hello, world!", 0, 0xc0363e43},
		{"Hello, world!", 0x9747b28c, 0x24884cba},
		{"The quick brown fox jumps over the lazy dog", 0, 0x2e4ff723},
		{"The quick brown fox jumps over the lazy dog", 0x9747b28c, 0x2fa826cd},
		{"aaaa", 0x9747b28c, 0x5a97808a},
		{"aaa", 0x9747b28c, 0x283e0130},
		{"aa", 0x9747b28c, 0x5d211726},
		{"a", 0x9747b28c, 0x7fa09ea6},
	} {
		if got := murmur3Seeded(c.seed, []byte(c.in), false); got != c.want {
			t.Errorf("murmur3(%q, %#x) = %#x, want %#x", c.in, c.seed, got, c.want)
		}
		// ASCII input is identical in both versions.
		if got := murmur3Seeded(c.seed, []byte(c.in), true); got != c.want {
			t.Errorf("signed murmur3(%q, %#x) = %#x, want %#x", c.in, c.seed, got, c.want)
		}
	}
}

func TestBloomKeyVersions(t *testing.T) {
	a := newBloomKey([]byte("src/main.go"))
	if a.v1 != a.v2 {
		t.Error("ASCII path must hash identically in v1 and v2")
	}
	b := newBloomKey([]byte("caf\xc3\xa9/men\xc3\xbc.txt"))
	if b.v1 == b.v2 {
		t.Error("high-bit path must hash differently in v1 and v2")
	}
	if newBloomKeyString("x") != newBloomKey([]byte("x")) {
		t.Error("string and bytes keys differ")
	}
}

// bloomPaths returns the keys git inserts for a changed path: the path and
// every leading directory.
func bloomPaths(p string) []string {
	out := []string{p}
	for i := len(p) - 1; i > 0; i-- {
		if p[i] == '/' {
			out = append(out, p[:i])
		}
	}
	return out
}

// changedPaths lists the paths that differ between c and its first parent
// (the empty tree for a root commit), as git sees them.
func changedPaths(t testing.TB, dir string, c *refCommit) []string {
	t.Helper()
	var out string
	if len(c.parents) == 0 {
		out = graphGit(t, dir, 0, "diff-tree", "-r", "-z", "--root", "--name-only", "--no-commit-id", fmt.Sprintf("%x", c.id))
	} else {
		out = graphGit(t, dir, 0, "diff-tree", "-r", "-z", "--name-only", "--no-commit-id", fmt.Sprintf("%x", c.parents[0]), fmt.Sprintf("%x", c.id))
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// checkBloomAgainstGit verifies the Bloom filters of every commit of g for
// the changed paths reported by git: no false negatives for the path or any
// leading directory, and unchanged paths mostly rejected.
func checkBloomAgainstGit(t *testing.T, dir string, g *commitGraph, cs []refCommit, c gcase) {
	t.Helper()
	checkBloomSample(t, dir, g, cs, c.bloom, c.bloomVer, 1)
}

// checkBloomSample is checkBloomAgainstGit for every step-th commit. wantVer
// is the BDAT version expected when bloom is true.
func checkBloomSample(t *testing.T, dir string, g *commitGraph, cs []refCommit, bloom bool, wantVer uint32, step int) {
	t.Helper()
	var checked, queries, unchangedQueries, falsePositives, unknown int
	for i := 0; i < len(cs); i += step {
		cm := &cs[i]
		pos, ok := g.lookup(cm.id)
		if !ok {
			t.Fatalf("lookup(%x)", cm.id)
		}
		paths := changedPaths(t, dir, cm)
		if !bloom {
			if g.hasBloom(pos) {
				t.Fatalf("commit %x has a Bloom filter in a graph written without --changed-paths", cm.id)
			}
			for _, p := range paths {
				if maybe, ok := g.maybeChanged(pos, []byte(p)); ok || maybe {
					t.Fatalf("maybeChanged = %v,%v without filters", maybe, ok)
				}
			}
			continue
		}
		l, _ := g.layerOf(pos)
		if !l.hasBloom || l.bloomVersion != wantVer {
			t.Fatalf("layer bloom = %v version %d, want version %d", l.hasBloom, l.bloomVersion, wantVer)
		}
		if len(paths) > 512 {
			if _, ok := g.maybeChanged(pos, []byte(paths[0])); ok {
				t.Fatalf("commit with %d changes should be unknown", len(paths))
			}
			continue
		}
		set := map[string]bool{}
		for _, p := range paths {
			for _, k := range bloomPaths(p) {
				set[k] = true
			}
		}
		if len(paths) == 0 {
			// git writes the one-byte all-zero filter for "nothing changed".
			if maybe, ok := g.maybeChanged(pos, []byte("README")); !ok || maybe {
				t.Fatalf("empty change set: maybe=%v ok=%v", maybe, ok)
			}
		}
		for k := range set {
			key := newBloomKey([]byte(k))
			maybe, ok := g.maybeChangedKey(pos, &key)
			if !ok {
				unknown++
				continue
			}
			if !maybe {
				t.Fatalf("commit %x: false negative for %q (changed: %q)", cm.id, k, paths)
			}
			queries++
		}
		// Unchanged paths: names that cannot be in the set.
		for j := range 40 {
			p := fmt.Sprintf("no/such/dir%d/file-%d.txt", j, j*7+i)
			if set[p] {
				continue
			}
			for _, q := range bloomPaths(p) {
				if set[q] {
					continue
				}
				key := newBloomKey([]byte(q))
				maybe, ok := g.maybeChangedKey(pos, &key)
				if !ok {
					continue
				}
				unchangedQueries++
				if maybe {
					falsePositives++
				}
			}
		}
		checked++
	}
	if !bloom {
		return
	}
	if checked == 0 || queries == 0 {
		t.Fatalf("no Bloom queries ran (checked %d commits)", checked)
	}
	rate := float64(falsePositives) / float64(max(unchangedQueries, 1))
	t.Logf("bloom v%d: %d commits, %d positive queries (%d unknown), false positives %d/%d = %.2f%%",
		wantVer, checked, queries, unknown, falsePositives, unchangedQueries, rate*100)
	if rate > 0.10 {
		t.Errorf("false positive rate %.1f%% is too high for 10 bits per entry", rate*100)
	}
}

// With a v1 graph, the v2 hash must not be used for high-bit paths (that is
// the bug v2 fixes); with v2, both agree on being negative-free.
func TestBloomHighBitVersions(t *testing.T) {
	for _, ver := range []uint32{1, 2} {
		t.Run(fmt.Sprintf("v%d", ver), func(t *testing.T) {
			r := newGTRepo(t)
			for i := range 30 {
				p := fmt.Sprintf("d\xe9\xff%d/\xc3\xa9\xfe%d.txt", i, i)
				r.commit(p, "x\n")
			}
			r.git("-c", fmt.Sprintf("commitGraph.changedPathsVersion=%d", ver),
				"commit-graph", "write", "--reachable", "--changed-paths")
			g, err := openCommitGraph(r.objects())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.close() }()
			cs := gitCommits(t, r.dir)
			wrong := 0
			for _, cm := range cs {
				pos, _ := g.lookup(cm.id)
				for _, p := range changedPaths(t, r.dir, &cm) {
					for _, k := range bloomPaths(p) {
						if maybe, ok := g.maybeChanged(pos, []byte(k)); !ok || !maybe {
							t.Fatalf("v%d: %q not found (maybe=%v ok=%v)", ver, k, maybe, ok)
						}
						// Probe with the other version's hash.
						key := newBloomKey([]byte(k))
						other := key
						other.v1, other.v2 = key.v2, key.v1
						// A v1 layer tries both hashes: test one only.
						other.high = false
						if maybe, ok := g.maybeChangedKey(pos, &other); ok && !maybe {
							wrong++
						}
					}
				}
			}
			if wrong == 0 {
				t.Errorf("using the other hash version never failed: v%d not distinguished", ver)
			}
		})
	}
}

// ---- synthetic filters ----------------------------------------------------

// synthBloom builds a graph with the given filters (nil entry: empty).
func synthBloom(t testing.TB, version, hashes, bitsPer uint32, filters [][]byte) *commitGraph {
	n := len(filters)
	s := &synthLayer{ids: synthIDs(n)}
	hdr := binary.BigEndian.AppendUint32(nil, version)
	hdr = binary.BigEndian.AppendUint32(hdr, hashes)
	hdr = binary.BigEndian.AppendUint32(hdr, bitsPer)
	var total uint32
	s.bdat = hdr
	for _, f := range filters {
		total += uint32(len(f))
		s.bidx = append(s.bidx, total)
		s.bdat = append(s.bdat, f...)
	}
	return mustParseSynth(t, s.bytes())
}

// setBloomBits sets the bits of path in filter f.
func setBloomBits(f []byte, hashes uint32, k bloomKey, v1 bool) {
	h := k.v2
	if v1 {
		h = k.v1
	}
	for i := uint32(0); i < hashes; i++ {
		bit := uint64(h[0]+i*h[1]) % uint64(len(f)*8)
		f[bit>>3] |= 1 << (bit & 7)
	}
}

func TestBloomSynthetic(t *testing.T) {
	const hashes = 7
	f := make([]byte, 16)
	setBloomBits(f, hashes, newBloomKey([]byte("a/b/c.go")), false)
	setBloomBits(f, hashes, newBloomKey([]byte("a/b")), false)
	setBloomBits(f, hashes, newBloomKey([]byte("a")), false)
	filters := [][]byte{
		f,
		{0x00},       // nothing changed
		{0xff},       // too many changes
		nil,          // no data
		f[:8],        // different size: still a valid filter
		{0x01, 0x02}, // random garbage filter
	}
	g := synthBloom(t, 2, hashes, 10, filters)
	defer func() { _ = g.close() }()
	if maybe, ok := g.maybeChanged(0, []byte("a/b/c.go")); !maybe || !ok {
		t.Errorf("filter 0: %v %v", maybe, ok)
	}
	if maybe, ok := g.maybeChanged(0, []byte("zzz")); maybe || !ok {
		t.Errorf("filter 0 unrelated: %v %v", maybe, ok)
	}
	if maybe, ok := g.maybeChanged(1, []byte("a")); maybe || !ok {
		t.Errorf("zero filter: %v %v", maybe, ok)
	}
	if _, ok := g.maybeChanged(2, []byte("a")); ok {
		t.Error("0xff filter must be unknown")
	}
	if _, ok := g.maybeChanged(3, []byte("a")); ok {
		t.Error("empty filter must be unknown")
	}
	if _, ok := g.maybeChanged(g.numCommits(), []byte("a")); ok {
		t.Error("bad position must be unknown")
	}
	if g.hasBloom(2) || !g.hasBloom(1) {
		t.Error("hasBloom")
	}
}

func TestBloomTruncated(t *testing.T) {
	// BIDX claims more bytes than BDAT holds: unknown, no panic.
	s := &synthLayer{ids: synthIDs(3)}
	hdr := binary.BigEndian.AppendUint32(nil, 2)
	hdr = binary.BigEndian.AppendUint32(hdr, 7)
	hdr = binary.BigEndian.AppendUint32(hdr, 10)
	s.bdat = append(hdr, 1, 2, 3, 4)
	s.bidx = []uint32{4, 4000, 0xffffffff}
	g := mustParseSynth(t, s.bytes())
	defer func() { _ = g.close() }()
	for pos := range uint32(3) {
		if _, ok := g.maybeChanged(pos, []byte("x")); pos > 0 && ok {
			t.Errorf("pos %d: truncated filter reported known", pos)
		}
	}
	// Non-monotonic offsets.
	s.bidx = []uint32{4, 2, 3}
	g2 := mustParseSynth(t, s.bytes())
	for pos := range uint32(3) {
		g2.maybeChanged(pos, []byte("x"))
	}
}

func TestBloomIgnoredSettings(t *testing.T) {
	for _, c := range []struct{ version, hashes uint32 }{{3, 7}, {0, 7}, {2, 0}, {2, bloomMaxHashes + 1}, {2, 1 << 31}} {
		g := synthBloom(t, c.version, c.hashes, 10, [][]byte{{0x00}, {0x00}})
		if g.hasBloom(0) {
			t.Errorf("version %d hashes %d should be ignored", c.version, c.hashes)
		}
		if _, ok := g.maybeChanged(0, []byte("a")); ok {
			t.Errorf("version %d hashes %d should be unknown", c.version, c.hashes)
		}
	}
}

// A wrongly sized BIDX or a BDAT shorter than its header disables the Bloom
// filters of the layer only, like git; the rest of the layer stays usable.
func TestBloomBadChunkDisablesFeature(t *testing.T) {
	hdr := binary.BigEndian.AppendUint32(nil, 2)
	hdr = binary.BigEndian.AppendUint32(hdr, 7)
	hdr = binary.BigEndian.AppendUint32(hdr, 10)
	for name, s := range map[string]*synthLayer{
		"short BDAT":    {ids: synthIDs(2), bidx: []uint32{1, 2}, bdat: []byte{0, 0, 0, 2}},
		"empty BDAT":    {ids: synthIDs(2), bidx: []uint32{1, 2}, bdat: []byte{}},
		"BIDX too few":  {ids: synthIDs(3), bidx: []uint32{1, 2}, bdat: append(hdr[:12:12], 0, 0)},
		"BIDX too many": {ids: synthIDs(2), bidx: []uint32{1, 2, 2}, bdat: append(hdr[:12:12], 0, 0)},
	} {
		t.Run(name, func(t *testing.T) {
			g := mustParseSynth(t, s.bytes())
			defer func() { _ = g.close() }()
			if g.layers[0].hasBloom {
				t.Fatal("hasBloom")
			}
			for pos := range g.numCommits() {
				if g.hasBloom(pos) {
					t.Errorf("pos %d: hasBloom", pos)
				}
				if _, ok := g.maybeChanged(pos, []byte("a")); ok {
					t.Errorf("pos %d: answer without filter", pos)
				}
				id, err := g.commitID(pos)
				if err != nil {
					t.Fatal(err)
				}
				if got, ok := g.lookup(id); !ok || got != pos {
					t.Errorf("lookup(%d) = %d, %v", pos, got, ok)
				}
			}
		})
	}
}

// A v1 filter written where char is unsigned (Linux arm64, ppc64, s390x) holds
// the same bits as a v2 one. It must be found as well as one written with
// signed chars.
func TestBloomV1UnsignedWriter(t *testing.T) {
	const hashes = 7
	path := []byte("d\xe9\xff/\xc3\xa9.txt")
	k := newBloomKey(path)
	if !k.high || k.v1 == k.v2 {
		t.Fatal("test path does not distinguish the hashes")
	}
	signed := make([]byte, 64)
	setBloomBits(signed, hashes, k, true)
	unsigned := make([]byte, 64)
	setBloomBits(unsigned, hashes, k, false)
	other := make([]byte, 64)
	setBloomBits(other, hashes, newBloomKey([]byte("zz/other")), false)
	g := synthBloom(t, 1, hashes, 10, [][]byte{signed, unsigned, other})
	defer func() { _ = g.close() }()
	for pos, want := range []bool{true, true, false} {
		if maybe, ok := g.maybeChanged(uint32(pos), path); !ok || maybe != want {
			t.Errorf("v1 filter %d: maybe=%v ok=%v, want %v", pos, maybe, ok, want)
		}
	}
	// A v2 layer must not accept the signed hash.
	g2 := synthBloom(t, 2, hashes, 10, [][]byte{signed, unsigned})
	defer func() { _ = g2.close() }()
	if maybe, _ := g2.maybeChanged(0, path); maybe {
		t.Error("v2 layer matched a signed-char filter")
	}
	if maybe, _ := g2.maybeChanged(1, path); !maybe {
		t.Error("v2 layer missed an unsigned filter")
	}
}

// Keys git never inserts are unknown, not "unchanged".
func TestBloomUnusableKeys(t *testing.T) {
	const hashes = 7
	f := make([]byte, 64)
	for _, p := range []string{"a/b", "a", ".hidden", "..x", "a/.b"} {
		setBloomBits(f, hashes, newBloomKey([]byte(p)), false)
	}
	g := synthBloom(t, 2, hashes, 10, [][]byte{f, {0x00}})
	defer func() { _ = g.close() }()

	for _, p := range []string{"", "/", "//", "/a", "/a/b", "a//b", "a/b//", "./a", "a/./b", "a/.", ".", "..",
		"../a", "a/../b", "a/..", "a\x00b"} {
		for pos := range uint32(2) {
			if maybe, ok := g.maybeChanged(pos, []byte(p)); ok || maybe {
				t.Errorf("%q at %d: maybe=%v ok=%v, want unknown", p, pos, maybe, ok)
			}
		}
		if k := newBloomKey([]byte(p)); k.usable {
			t.Errorf("%q: key usable", p)
		}
	}
	// One trailing slash is dropped, like git does.
	for _, p := range []string{"a/b", "a/b/", "a", "a/", ".hidden", "..x", "a/.b/"} {
		if maybe, ok := g.maybeChanged(0, []byte(p)); !ok || !maybe {
			t.Errorf("%q: maybe=%v ok=%v, want found", p, maybe, ok)
		}
		if maybe, ok := g.maybeChanged(1, []byte(p)); !ok || maybe {
			t.Errorf("%q in empty filter: maybe=%v ok=%v", p, maybe, ok)
		}
	}
	if newBloomKey([]byte("a/b/")) != newBloomKey([]byte("a/b")) {
		t.Error("trailing slash changes the key")
	}
	var zero bloomKey
	if _, ok := g.maybeChangedKey(0, &zero); ok {
		t.Error("zero key must be unknown")
	}
}

// ---- allocation and fuzz --------------------------------------------------

func TestBloomNoAllocs(t *testing.T) {
	const hashes = 7
	f := make([]byte, 16)
	setBloomBits(f, hashes, newBloomKey([]byte("a/b")), false)
	g := synthBloom(t, 2, hashes, 10, [][]byte{f, f})
	path := []byte("a/b/ünï.go")
	var sink bool
	if n := testing.AllocsPerRun(100, func() {
		k := newBloomKey(path)
		m, _ := g.maybeChangedKey(0, &k)
		m2, _ := g.maybeChanged(1, path)
		sink = sink || m || m2
	}); n != 0 {
		t.Errorf("allocs = %v", n)
	}
	_ = sink
}

func FuzzBloomLookup(f *testing.F) {
	f.Add([]byte{0x00}, []byte("a"), uint32(2), uint32(7))
	f.Add([]byte{0xff}, []byte("a/b"), uint32(1), uint32(7))
	f.Add([]byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0}, []byte("caf\xc3\xa9/x"), uint32(1), uint32(3))
	f.Add([]byte{}, []byte{}, uint32(2), uint32(7))
	f.Add([]byte{0x80, 0x01}, []byte("\xff\xfe\xfd\xfc\xfb"), uint32(9), uint32(1<<31))
	f.Fuzz(func(t *testing.T, filter, path []byte, version, hashes uint32) {
		hdr := binary.BigEndian.AppendUint32(nil, version)
		hdr = binary.BigEndian.AppendUint32(hdr, hashes)
		hdr = binary.BigEndian.AppendUint32(hdr, 10)
		s := &synthLayer{ids: synthIDs(2)}
		s.bdat = append(hdr, filter...)
		// Second filter claims the rest; the first is cut anywhere.
		s.bidx = []uint32{uint32(len(filter) / 2), uint32(len(filter))}
		l, err := parseGraphLayer(s.bytes())
		if err != nil {
			return
		}
		g := &commitGraph{layers: []graphLayer{*l}, total: l.n}
		k := newBloomKey(path)
		for pos := range uint32(3) {
			maybe, ok := g.maybeChangedKey(pos, &k)
			if maybe && !ok {
				t.Fatal("maybe without ok")
			}
			if ok && !k.usable {
				t.Fatal("answer for an unusable key")
			}
		}
		if !k.usable {
			return
		}
		// A path inserted into a valid filter is always found.
		half := len(filter) / 2
		if (version == 1 || version == 2) && hashes >= 1 && hashes <= bloomMaxHashes && len(filter)-half >= 1 {
			second := append([]byte(nil), filter[half:]...)
			setBloomBits(second, hashes, k, version == 1)
			s.bdat = append(append(hdr[:12:12], filter[:half]...), second...)
			l2, err := parseGraphLayer(s.bytes())
			if err != nil {
				t.Fatal(err)
			}
			g2 := &commitGraph{layers: []graphLayer{*l2}, total: l2.n}
			if maybe, ok := g2.maybeChangedKey(1, &k); ok && !maybe {
				t.Fatal("false negative after insertion")
			}
			if version == 1 {
				// The same filter written with the unsigned hash.
				third := append([]byte(nil), filter[half:]...)
				setBloomBits(third, hashes, k, false)
				s.bdat = append(append(hdr[:12:12], filter[:half]...), third...)
				l3, err := parseGraphLayer(s.bytes())
				if err != nil {
					t.Fatal(err)
				}
				g3 := &commitGraph{layers: []graphLayer{*l3}, total: l3.n}
				if maybe, ok := g3.maybeChangedKey(1, &k); ok && !maybe {
					t.Fatal("false negative with an unsigned v1 writer")
				}
			}
		}
	})
}

// ---- benchmarks -----------------------------------------------------------

func BenchmarkBloomMaybeChanged(b *testing.B) {
	g, _ := benchGraph(b)
	n := g.numCommits()
	key := newBloomKey([]byte("pkg3/sub1/g1234.go"))
	b.ReportAllocs()
	var pos uint32
	var hits int
	for b.Loop() {
		if maybe, ok := g.maybeChangedKey(pos, &key); ok && maybe {
			hits++
		}
		if pos++; pos == n {
			pos = 0
		}
	}
	_ = hits
}

func BenchmarkBloomMaybeChangedPath(b *testing.B) {
	g, _ := benchGraph(b)
	n := g.numCommits()
	path := []byte("pkg3/sub1/g1234.go")
	b.ReportAllocs()
	var pos uint32
	for b.Loop() {
		g.maybeChanged(pos, path)
		if pos++; pos == n {
			pos = 0
		}
	}
}

func BenchmarkBloomNewKey(b *testing.B) {
	path := []byte("internal/adapters/gitcli/repos.go")
	b.ReportAllocs()
	for b.Loop() {
		_ = newBloomKey(path)
	}
}

// ---- real repository ------------------------------------------------------

// TestGraphBenchRepo checks a large real repository (ERMITE_GIT_BENCH_REPO).
// The repository is never modified: its objects are copied into a bare clone
// where the split graph is written.
func TestGraphBenchRepo(t *testing.T) {
	src := os.Getenv("ERMITE_GIT_BENCH_REPO")
	if src == "" {
		t.Skip("ERMITE_GIT_BENCH_REPO not set")
	}
	requireGit(t)
	for _, tc := range []struct {
		ver   uint32
		multi bool
	}{{2, false}, {1, false}, {2, true}, {1, true}} {
		ver, multi := tc.ver, tc.multi
		t.Run(fmt.Sprintf("bloom-v%d-multi=%v", ver, multi), func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "clone.git")
			graphGit(t, "", 0, "clone", "--bare", "--no-local", "-q", src, dst)
			cfg := fmt.Sprintf("commitGraph.changedPathsVersion=%d", ver)
			if multi {
				// Several layers, like successive pushes would produce.
				all := strings.Fields(graphGit(t, dst, 0, "rev-list", "--all"))
				for _, back := range []int{5000, 2500, 500} {
					tip := all[min(back, len(all)-1)]
					graphGitStdin(t, dst, tip+"\n", "-c", cfg,
						"commit-graph", "write", "--stdin-commits", "--changed-paths", "--split=no-merge")
				}
				graphGit(t, dst, 0, "-c", cfg, "commit-graph", "write", "--reachable", "--changed-paths", "--split=no-merge")
			} else {
				graphGit(t, dst, 0, "-c", cfg, "commit-graph", "write", "--reachable", "--changed-paths", "--split")
			}
			g, err := openCommitGraph(filepath.Join(dst, "objects"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.close() }()
			out := graphGit(t, dst, 0, "log", "--all", "--format=%H %T %P|%ct")
			var cs []refCommit
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				left, ct, _ := strings.Cut(line, "|")
				f := strings.Fields(left)
				c := refCommit{id: graphTestHex(f[0]), tree: graphTestHex(f[1])}
				for _, p := range f[2:] {
					c.parents = append(c.parents, graphTestHex(p))
				}
				c.ctime, _ = strconv.ParseInt(ct, 10, 64)
				cs = append(cs, c)
			}
			if int(g.numCommits()) != len(cs) {
				t.Fatalf("numCommits = %d, git log has %d", g.numCommits(), len(cs))
			}
			t.Logf("%d commits, %d layers, generation v2 = %v", len(cs), g.numLayers(), g.hasGenerationV2())
			if multi && g.numLayers() < 4 {
				t.Fatalf("expected a multi-layer chain, got %d layers", g.numLayers())
			}
			var buf [16]uint32
			for i := range cs {
				c := &cs[i]
				pos, ok := g.lookup(c.id)
				if !ok {
					t.Fatalf("lookup(%x)", c.id)
				}
				if id, _ := g.commitID(pos); id != c.id {
					t.Fatalf("commitID(%d)", pos)
				}
				if tr, _ := g.rootTree(pos); tr != c.tree {
					t.Fatalf("rootTree of %x = %x, want %x", c.id, tr, c.tree)
				}
				if ct, _ := g.commitTime(pos); ct != c.ctime {
					t.Fatalf("commitTime of %x = %d, want %d", c.id, ct, c.ctime)
				}
				ps, err := g.appendParents(buf[:0], pos)
				if err != nil || len(ps) != len(c.parents) {
					t.Fatalf("parents of %x: %v, %v; want %d", c.id, ps, err, len(c.parents))
				}
				for j, p := range ps {
					if pid, _ := g.commitID(p); pid != c.parents[j] {
						t.Fatalf("parent %d of %x", j, c.id)
					}
				}
				gen, err := g.generation(pos)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range ps {
					if pg, _ := g.generation(p); pg >= gen {
						t.Fatalf("generation(%x) = %d not above parent %d", c.id, gen, pg)
					}
				}
			}
			checkBloomSample(t, dst, g, cs, true, ver, 25)
		})
	}
}
