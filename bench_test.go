// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// benchFixture is a repository with loose objects, a deltified pack and a
// directory of many entries. With ERMITE_GIT_BENCH_REPO it is that
// repository instead.
type benchFixture struct {
	dir                      string
	r                        *Repo
	looseCommit, looseBlob   ObjectID
	packedCommit, packedBlob ObjectID // not deltified
	deltaBlob, deltaTree     ObjectID // deltified in the pack
	bigTree, headCommit      ObjectID
	prefix                   string
	batchIDs                 []ObjectID // objects for the git cat-file comparison
	allCommits               []ObjectID
}

var (
	benchOnce sync.Once
	benchFx   *benchFixture
)

func getBench(b *testing.B) *benchFixture {
	b.Helper()
	requireGit(b)
	benchOnce.Do(func() {
		fx := &benchFixture{}
		if dir := os.Getenv("ERMITE_GIT_BENCH_REPO"); dir != "" {
			fx.dir = dir
			fx.fromRepo(b, dir)
		} else {
			fx.generate(b)
		}
		benchFx = fx
	})
	if benchFx == nil {
		b.Fatal("benchmark fixture failed")
	}
	return benchFx
}

func (fx *benchFixture) fromRepo(b *testing.B, dir string) {
	r, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	fx.r = r
	out := runGit(b, dir, nil, nil, "rev-list", "--all")
	for _, l := range lines(string(out)) {
		fx.allCommits = append(fx.allCommits, mustID(b, l))
	}
	fx.headCommit = fx.allCommits[0]
	fx.finish(b, dir)
}

func (fx *benchFixture) generate(b *testing.B) {
	dir, err := os.MkdirTemp("", "ermite-git-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	fx.dir = dir
	env := append(gitEnv(), "GIT_AUTHOR_DATE=1700000000 +0000", "GIT_COMMITTER_DATE=1700000000 +0000")
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "gc.auto=0", "-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			b.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	for i := 0; i < 400; i++ {
		_ = os.WriteFile(dir+"/f.txt", append(textLines(800, 0), []byte(fmt.Sprintf("rev %d\n", i))...), 0o644)
		_ = os.MkdirAll(dir+"/many", 0o755)
		_ = os.WriteFile(fmt.Sprintf("%s/many/file-%04d.txt", dir, i), []byte(fmt.Sprint(i)), 0o644)
		run("add", "-A")
		run("commit", "-q", "-m", fmt.Sprintf("commit %d\n\nbody of the message", i))
	}
	// Some commits stay loose: one more after the repack.
	run("repack", "-adf", "--depth=50", "--window=100")
	_ = os.WriteFile(dir+"/loose.txt", textLines(300, 5), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "loose commit")
	var err2 error
	fx.r, err2 = Open(dir + "/.git")
	if err2 != nil {
		b.Fatal(err2)
	}
	fx.headCommit = mustID(b, run("rev-parse", "HEAD"))
	fx.looseCommit = fx.headCommit
	fx.looseBlob = mustID(b, run("rev-parse", "HEAD:loose.txt"))
	for _, l := range lines(run("rev-list", "--all")) {
		fx.allCommits = append(fx.allCommits, mustID(b, l))
	}
	fx.bigTree = mustID(b, run("rev-parse", "HEAD:many"))
	fx.finish(b, dir+"/.git")
}

// finish picks one object of each kind from the pack.
func (fx *benchFixture) finish(b *testing.B, gitDir string) {
	s := fx.r.snap.Load()
	for _, p := range s.packs {
		for i := uint32(0); i < p.idx.n && (fx.deltaBlob.IsZero() || fx.packedBlob.IsZero() || fx.deltaTree.IsZero() || fx.packedCommit.IsZero()); i++ {
			off, _ := p.idx.offset(i)
			h, err := parseEntryHeader(p.data, off)
			if err != nil {
				b.Fatal(err)
			}
			var id ObjectID
			copy(id[:], p.idx.id(i))
			switch {
			case h.isDelta() && h.size > 200:
				typ, _ := p.chainEnd(off, h)
				if typ == TypeBlob && fx.deltaBlob.IsZero() {
					fx.deltaBlob = id
				}
				if typ == TypeTree && fx.deltaTree.IsZero() {
					fx.deltaTree = id
				}
			case h.typ == uint8(TypeBlob) && h.size > 200 && fx.packedBlob.IsZero():
				fx.packedBlob = id
			case h.typ == uint8(TypeCommit) && fx.packedCommit.IsZero():
				fx.packedCommit = id
			}
		}
	}
	if fx.looseCommit.IsZero() {
		fx.looseCommit = fx.packedCommit
	}
	if fx.looseBlob.IsZero() {
		fx.looseBlob = fx.packedBlob
	}
	if fx.deltaBlob.IsZero() {
		fx.deltaBlob = fx.packedBlob
	}
	if fx.deltaTree.IsZero() {
		var c Commit
		if err := fx.r.ReadCommit(fx.headCommit, &c); err != nil {
			b.Fatal(err)
		}
		fx.deltaTree = c.Tree
	}
	if fx.bigTree.IsZero() {
		var c Commit
		if err := fx.r.ReadCommit(fx.headCommit, &c); err != nil {
			b.Fatal(err)
		}
		fx.bigTree = c.Tree
	}
	fx.prefix = fx.headCommit.String()[:7]
	// The set for the cat-file comparison: the commits, then their trees.
	var c Commit
	for i, id := range fx.allCommits {
		if i >= 2000 {
			break
		}
		fx.batchIDs = append(fx.batchIDs, id)
		if err := fx.r.ReadCommit(id, &c); err == nil {
			fx.batchIDs = append(fx.batchIDs, c.Tree)
		}
	}
}

func BenchmarkReadObject(b *testing.B) {
	fx := getBench(b)
	for _, c := range []struct {
		name string
		id   ObjectID
	}{
		{"loose-commit", fx.looseCommit},
		{"loose-blob", fx.looseBlob},
		{"packed-commit", fx.packedCommit},
		{"packed-blob", fx.packedBlob},
		{"deltified-blob", fx.deltaBlob},
		{"deltified-tree", fx.deltaTree},
	} {
		b.Run(c.name, func(b *testing.B) {
			var buf []byte
			b.ReportAllocs()
			for b.Loop() {
				_, data, err := fx.r.ReadObject(c.id, buf)
				if err != nil {
					b.Fatal(err)
				}
				buf = data
			}
			b.SetBytes(int64(len(buf)))
		})
	}
}

func BenchmarkReadObjectParallel(b *testing.B) {
	fx := getBench(b)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var buf []byte
		i := 0
		for pb.Next() {
			id := fx.batchIDs[i%len(fx.batchIDs)]
			i++
			_, data, err := fx.r.ReadObject(id, buf)
			if err != nil {
				b.Fatal(err)
			}
			buf = data
		}
	})
}

func BenchmarkObjectHeader(b *testing.B) {
	fx := getBench(b)
	for _, c := range []struct {
		name string
		id   ObjectID
	}{{"loose", fx.looseBlob}, {"packed", fx.packedBlob}, {"deltified", fx.deltaBlob}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := fx.r.ObjectHeader(c.id); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkReadCommit(b *testing.B) {
	fx := getBench(b)
	for _, c := range []struct {
		name string
		id   ObjectID
	}{{"loose", fx.looseCommit}, {"packed", fx.packedCommit}} {
		b.Run(c.name, func(b *testing.B) {
			var commit Commit
			if err := fx.r.ReadCommit(c.id, &commit); err != nil { // warm-up
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if err := fx.r.ReadCommit(c.id, &commit); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkParseCommit(b *testing.B) {
	raw := []byte("tree " + strings.Repeat("a", 40) + "\nparent " + strings.Repeat("b", 40) + "\nauthor Ada Lovelace <ada@example.com> 1700000000 +0100\ncommitter Grace Hopper <grace@example.com> 1700000001 -0800\n\nmessage\n")
	var c Commit
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		if err := ParseCommit(raw, &c); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTreeIteration(b *testing.B) {
	fx := getBench(b)
	for _, c := range []struct {
		name string
		id   ObjectID
	}{{"many-entries", fx.bigTree}, {"deltified", fx.deltaTree}} {
		b.Run(c.name, func(b *testing.B) {
			entries := 0
			var sum int
			cb := func(e Entry) bool {
				entries++
				sum += len(e.Name) + int(e.Mode)
				return true
			}
			if err := fx.r.ReadTree(c.id, cb); err != nil { // warm-up
				b.Fatal(err)
			}
			per := entries
			b.ReportAllocs()
			entries = 0
			for b.Loop() {
				if err := fx.r.ReadTree(c.id, cb); err != nil {
					b.Fatal(err)
				}
			}
			_ = sum
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(max(entries, 1)), "ns/entry")
			b.ReportMetric(float64(per), "entries/tree")
		})
	}
}

func BenchmarkParseTree(b *testing.B) {
	var tree []byte
	for i := 0; i < 500; i++ {
		tree = append(tree, fmt.Sprintf("100644 file-%04d.txt\x00", i)...)
		tree = append(tree, bytes.Repeat([]byte{byte(i)}, 20)...)
	}
	n := 0
	cb := func(e Entry) bool { n += len(e.Name); return true }
	b.ReportAllocs()
	b.SetBytes(int64(len(tree)))
	for b.Loop() {
		if err := ParseTree(tree, cb); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResolvePrefix(b *testing.B) {
	fx := getBench(b)
	for _, n := range []int{7, 12} {
		b.Run(strconv.Itoa(n)+"-digits", func(b *testing.B) {
			ids := make([]string, 0, 64)
			for i := 0; i < len(fx.allCommits) && i < 64; i++ {
				ids = append(ids, fx.allCommits[i].String()[:n])
			}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				if _, err := fx.r.ResolvePrefix(ids[i%len(ids)]); err != nil {
					b.Fatal(err)
				}
				i++
			}
		})
	}
	b.Run("missing", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := fx.r.ResolvePrefix("0000000"); err == nil {
				b.Fatal("found")
			}
		}
	})
}

func BenchmarkRefs(b *testing.B) {
	fx := getBench(b)
	b.Run("all", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			for _, err := range fx.r.Refs("") {
				if err != nil {
					b.Fatal(err)
				}
				n++
			}
		}
	})
	b.Run("resolve-head", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := fx.r.ResolveRef("HEAD"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("head", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, _, err := fx.r.Head(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("resolve-short-name", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := fx.r.Resolve("main"); err != nil {
				_, _ = fx.r.Resolve("master")
			}
		}
	})
}

func BenchmarkOpenBlob(b *testing.B) {
	fx := getBench(b)
	for _, c := range []struct {
		name string
		id   ObjectID
	}{{"loose", fx.looseBlob}, {"packed", fx.packedBlob}, {"deltified", fx.deltaBlob}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				size, rc, err := fx.r.OpenBlob(c.id)
				if err != nil {
					b.Fatal(err)
				}
				b.SetBytes(size)
				if _, err := io.Copy(io.Discard, rc); err != nil {
					b.Fatal(err)
				}
				_ = rc.Close()
			}
		})
	}
}

func BenchmarkRefresh(b *testing.B) {
	fx := getBench(b)
	b.ReportAllocs()
	for b.Loop() {
		if err := fx.r.Refresh(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGitCatFileBatch is the informal reference: the same objects read
// through one long-lived "git cat-file --batch" process, as the gitcli adapter
// does. Compare with BenchmarkReadObjectSequence.
func BenchmarkGitCatFileBatch(b *testing.B) {
	fx := getBench(b)
	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Dir = fx.dir
	cmd.Env = gitEnv()
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		b.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Wait() }()
	br := bufio.NewReaderSize(stdout, 1<<20)
	var buf []byte
	req := make([]byte, 0, 64)
	b.ReportAllocs()
	i, total := 0, int64(0)
	for b.Loop() {
		id := fx.batchIDs[i%len(fx.batchIDs)]
		i++
		req = append(id.AppendHex(req[:0]), '\n')
		if _, err := stdin.Write(req); err != nil {
			b.Fatal(err)
		}
		hdr, err := br.ReadSlice('\n')
		if err != nil {
			b.Fatal(err)
		}
		f := bytes.Fields(hdr)
		size, _ := strconv.Atoi(string(f[2]))
		buf = grow(buf, size+1)
		if _, err := io.ReadFull(br, buf); err != nil {
			b.Fatal(err)
		}
		total += int64(size)
	}
	b.SetBytes(total / int64(max(b.N, 1)))
}

func BenchmarkReadObjectSequence(b *testing.B) {
	fx := getBench(b)
	var buf []byte
	b.ReportAllocs()
	i, total := 0, int64(0)
	for b.Loop() {
		id := fx.batchIDs[i%len(fx.batchIDs)]
		i++
		_, data, err := fx.r.ReadObject(id, buf)
		if err != nil {
			b.Fatal(err)
		}
		buf = data
		total += int64(len(data))
	}
	b.SetBytes(total / int64(max(b.N, 1)))
}

func TestMain(m *testing.M) {
	code := m.Run()
	cleanupRealWalk()
	if benchFx != nil && benchFx.r != nil {
		_ = benchFx.r.Close()
		if os.Getenv("ERMITE_GIT_BENCH_REPO") == "" {
			_ = os.RemoveAll(benchFx.dir)
		}
	}
	os.Exit(code)
}
