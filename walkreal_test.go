// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// realWalkRepo clones the repository named by ERMITE_GIT_BENCH_REPO (bare,
// no hardlinks, so the source is never touched) and writes a split
// commit-graph with changed-path Bloom filters. The clone is shared by the
// tests and benchmarks of the run.
type realWalk struct {
	dir  string
	r    *Repo
	head ObjectID
}

var (
	realWalkOnce sync.Once
	realWalkFx   *realWalk
	realWalkErr  error
)

func getRealWalk(tb testing.TB) *realWalk {
	tb.Helper()
	src := os.Getenv("ERMITE_GIT_BENCH_REPO")
	if src == "" {
		tb.Skip("ERMITE_GIT_BENCH_REPO is not set")
	}
	requireGit(tb)
	realWalkOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ermite-walk-*")
		if err != nil {
			realWalkErr = err
			return
		}
		clone := filepath.Join(dir, "clone.git")
		for _, args := range [][]string{
			{"clone", "--quiet", "--bare", "--no-local", src, clone},
			{"-C", clone, "commit-graph", "write", "--reachable", "--changed-paths", "--split"},
		} {
			cmd := exec.Command("git", args...)
			cmd.Env = gitEnv()
			if out, err := cmd.CombinedOutput(); err != nil {
				realWalkErr = fmt.Errorf("git %v: %v\n%s", args, err, out)
				return
			}
		}
		r, err := Open(clone)
		if err != nil {
			realWalkErr = err
			return
		}
		head, err := exec.Command("git", "-C", clone, "rev-parse", "HEAD").Output()
		if err != nil {
			realWalkErr = err
			return
		}
		id, err := ParseID(strings.TrimSpace(string(head)))
		if err != nil {
			realWalkErr = err
			return
		}
		realWalkFx = &realWalk{dir: clone, r: r, head: id}
	})
	if realWalkErr != nil {
		tb.Fatal(realWalkErr)
	}
	return realWalkFx
}

func (x *realWalk) git(tb testing.TB, args ...string) string {
	tb.Helper()
	cmd := exec.Command("git", append([]string{"-C", x.dir}, args...)...)
	cmd.Env = gitEnv()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		tb.Fatalf("git %v: %v\n%s", args, err, errb.String())
	}
	return string(out)
}

// cleanupRealWalk is called by TestMain.
func cleanupRealWalk() {
	if realWalkFx != nil {
		_ = realWalkFx.r.Close()
		_ = os.RemoveAll(filepath.Dir(realWalkFx.dir))
	}
}

func TestWalkRealRepository(t *testing.T) {
	x := getRealWalk(t)
	head := x.head.String()

	// Full history.
	want := lines(x.git(t, "log", "--format=%H", head))
	got := walkIDs(t, x.r, x.head, WalkOptions{})
	if !slices.Equal(got, want) {
		t.Fatalf("full history differs: %d commits, git has %d", len(got), len(want))
	}
	t.Logf("full history: %d commits", len(got))
	for _, win := range [][2]int{{5000, 100}, {1, 1}, {len(want) - 3, 10}} {
		exp := window(want, win[0], win[1])
		if g := walkIDs(t, x.r, x.head, WalkOptions{Skip: win[0], Limit: win[1]}); !slices.Equal(g, exp) {
			t.Errorf("skip %d limit %d differs", win[0], win[1])
		}
	}

	// 20 sampled paths: files and directories, deterministic.
	files := lines(x.git(t, "ls-tree", "-r", "--name-only", head))
	dirSet := map[string]bool{}
	for _, f := range files {
		for d := path.Dir(f); d != "."; d = path.Dir(d) {
			dirSet[d] = true
		}
	}
	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	rng := rand.New(rand.NewSource(42))
	var paths []string
	for len(paths) < 14 {
		paths = append(paths, files[rng.Intn(len(files))])
	}
	for len(paths) < 20 && len(dirs) > 0 {
		paths = append(paths, dirs[rng.Intn(len(dirs))])
	}
	paths = append(paths, "go.mod", "pkg/gui/")
	for _, p := range paths {
		want := lines(x.git(t, "log", "--format=%H", head, "--", p))
		got := walkIDs(t, x.r, x.head, WalkOptions{Path: p})
		if !slices.Equal(got, want) {
			t.Errorf("path %q: %d commits, git has %d", p, len(got), len(want))
			continue
		}
		if len(want) > 7 {
			if g := walkIDs(t, x.r, x.head, WalkOptions{Path: p, Skip: 3, Limit: 4}); !slices.Equal(g, want[3:7]) {
				t.Errorf("path %q: skip/limit differ", p)
			}
		}
	}
	t.Logf("%d paths compared", len(paths))
}

func TestLastCommitsRealRepository(t *testing.T) {
	x := getRealWalk(t)
	head := x.head.String()
	// The root and three subdirectories, the biggest ones among the first.
	dirs := []string{""}
	for _, d := range []string{"pkg/gui", "pkg/commands", "docs"} {
		if x.git(t, "ls-tree", head, d+"/") != "" {
			dirs = append(dirs, d)
		}
	}
	for _, dir := range dirs {
		spec := head
		if dir != "" {
			spec += ":" + dir
		}
		var names []string
		for _, l := range lines(x.git(t, "ls-tree", "--name-only", spec)) {
			names = append(names, l)
		}
		if len(names) > 120 {
			names = names[:120]
		}
		got, err := x.r.LastCommits(context.Background(), x.head, dir, names)
		if err != nil {
			t.Fatal(err)
		}
		for i, n := range names {
			p := path.Join(dir, n)
			exp := strings.TrimSpace(x.git(t, "log", "-n", "1", "--format=%H", head, "--", p))
			if got[i].String() != exp {
				t.Errorf("%q: got %s want %s", p, got[i], exp)
			}
		}
		t.Logf("dir %q: %d entries compared", dir, len(names))
	}
}
