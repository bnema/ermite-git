// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"path"
	"testing"
)

// gitLogCount runs git log like the gitcli adapter does and reads every
// line of the output.
func gitLogCount(b *testing.B, x *realWalk, args ...string) int {
	cmd := exec.Command("git", append([]string{"-C", x.dir}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.StdoutPipe()
	if err != nil {
		b.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		b.Fatal(err)
	}
	n := 0
	br := bufio.NewReader(out)
	for {
		_, err := br.ReadSlice('\n')
		if err == io.EOF {
			break
		}
		if err != nil && err != bufio.ErrBufferFull {
			b.Fatal(err)
		}
		n++
	}
	if err := cmd.Wait(); err != nil {
		b.Fatal(err)
	}
	return n
}

// gitcliFormat is the format the gitcli adapter of ermite asks git log for.
const gitcliFormat = "%H%x00%P%x00%an%x00%ae%x00%at%x00%cn%x00%ce%x00%ct%x00%B"

// benchWalk compares Walk with git log in two shapes: ids only (git answers
// from the commit-graph without reading commits) and complete commits, which
// is what the gitcli adapter asks for (git has to read every commit).
func benchWalk(b *testing.B, x *realWalk, opts WalkOptions, gitArgs ...string) {
	run := func(b *testing.B, o WalkOptions) {
		b.ReportAllocs()
		var n int
		for i := 0; i < b.N; i++ {
			n = 0
			for c, err := range x.r.Walk(context.Background(), x.head, o) {
				if err != nil {
					b.Fatal(err)
				}
				if !o.IDOnly {
					n += len(c.Message) // the commit is really parsed
				}
				n++
			}
		}
		_ = n
	}
	for _, shape := range []struct {
		name   string
		idOnly bool
		format string
	}{{"ids", true, "--format=%H"}, {"full", false, "-z --format=" + gitcliFormat}} {
		b.Run(shape.name+"/ermite", func(b *testing.B) {
			o := opts
			o.IDOnly = shape.idOnly
			run(b, o)
		})
		b.Run(shape.name+"/git", func(b *testing.B) {
			b.ReportAllocs()
			args := []string{"log"}
			if shape.idOnly {
				args = append(args, shape.format)
			} else {
				args = append(args, "-z", "--format="+gitcliFormat)
			}
			args = append(args, gitArgs...)
			for i := 0; i < b.N; i++ {
				gitLogCount(b, x, args...)
			}
		})
	}
}

func BenchmarkWalk(b *testing.B) {
	x := getRealWalk(b)
	head := x.head.String()
	b.Run("full-history", func(b *testing.B) { benchWalk(b, x, WalkOptions{}, head) })
	b.Run("skip5000-limit100", func(b *testing.B) {
		benchWalk(b, x, WalkOptions{Skip: 5000, Limit: 100}, "--skip=5000", "-n", "100", head)
	})
	b.Run("file", func(b *testing.B) {
		benchWalk(b, x, WalkOptions{Path: "pkg/gui/gui.go"}, head, "--", "pkg/gui/gui.go")
	})
	b.Run("dir", func(b *testing.B) {
		benchWalk(b, x, WalkOptions{Path: "pkg/commands"}, head, "--", "pkg/commands")
	})
	b.Run("rare-file", func(b *testing.B) {
		benchWalk(b, x, WalkOptions{Path: "go.mod"}, head, "--", "go.mod")
	})
	b.Run("page-100-of-file", func(b *testing.B) {
		benchWalk(b, x, WalkOptions{Path: "pkg/gui/gui.go", Limit: 100}, "-n", "100", head, "--", "pkg/gui/gui.go")
	})
}

func BenchmarkLastCommits(b *testing.B) {
	x := getRealWalk(b)
	head := x.head.String()
	for _, dir := range []string{"", "pkg/gui"} {
		spec := head
		if dir != "" {
			spec += ":" + dir
		}
		names := lines(x.git(b, "ls-tree", "--name-only", spec))
		name := dir
		if name == "" {
			name = "root"
		}
		b.Run(name+"/ermite", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := x.r.LastCommits(context.Background(), x.head, dir, names); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(names)), "entries")
		})
		b.Run(name+"/git-n-calls", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for _, n := range names {
					gitLogCount(b, x, "log", "-n", "1", "--format=%H", head, "--", path.Join(dir, n))
				}
			}
		})
	}
}
