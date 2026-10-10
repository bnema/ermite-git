// SPDX-License-Identifier: MIT

package git

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestGraphStampTrusted(t *testing.T) {
	const sec = int64(time.Second)
	mt := 1_000 * sec
	win := int64(racyWindow)
	exists := func(m int64) graphFileStamp { return graphFileStamp{exists: true, mtime: m} }
	for _, c := range []struct {
		name  string
		stamp graphStamp
		at    int64
		want  bool
	}{
		{"no graph", graphStamp{}, mt, true},
		{"stamped inside the window", graphStamp{single: exists(mt)}, mt + win, false},
		{"stamped just after the window", graphStamp{single: exists(mt)}, mt + win + 1, true},
		{"newest of two decides", graphStamp{single: exists(mt - 100*sec), chain: exists(mt)}, mt + sec, false},
		{"both old", graphStamp{single: exists(mt - 100*sec), chain: exists(mt - 50*sec)}, mt, true},
		{"mtime in the future", graphStamp{chain: exists(mt + 10*sec)}, mt, false},
	} {
		if got := c.stamp.trusted(c.at); got != c.want {
			t.Errorf("%s: trusted = %v, want %v", c.name, got, c.want)
		}
	}
}

func graphFiles(r *gtRepo) []string {
	info := filepath.Join(r.objects(), "info")
	return []string{filepath.Join(info, "commit-graph"), filepath.Join(info, "commit-graphs", "commit-graph-chain")}
}

func ageGraph(t *testing.T, r *gtRepo, age time.Duration) {
	t.Helper()
	mt := time.Now().Add(-age)
	n := 0
	for _, p := range graphFiles(r) {
		if _, err := os.Stat(p); err == nil {
			n++
			if err := os.Chtimes(p, mt, mt); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n == 0 {
		t.Fatal("no graph file")
	}
}

// A graph that is older than the racy window is trusted when its stat data
// is unchanged, however recently it was loaded: Refresh at the pace of the
// adapter (every second) must not reload it, and so not remap it.
func TestGraphRefreshDoesNotReloadAnUnchangedGraph(t *testing.T) {
	r := newGTRepo(t)
	r.commit("a", "a\n")
	r.commit("b", "b\n")
	r.git("commit-graph", "write", "--reachable", "--split")
	ageGraph(t, r, time.Hour)
	repo, err := Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	first := repo.graph.Load()
	if first == nil || first.g == nil {
		t.Fatal("no graph loaded")
	}
	for i := range 20 {
		if err := repo.Refresh(); err != nil {
			t.Fatal(err)
		}
		if repo.graph.Load() != first {
			t.Fatalf("Refresh %d reloaded an unchanged graph", i)
		}
	}
}

// Refresh once a second on a graph written just now: the graph is reloaded
// while its modification time is within the racy window of the stamp, and
// never again after that. A fake clock stands in for the waiting.
func TestGraphRefreshRacyWindow(t *testing.T) {
	r := newGTRepo(t)
	r.commit("a", "a\n")
	r.git("commit-graph", "write", "--reachable", "--split")
	start := time.Now()
	var elapsed atomic.Int64
	repo, err := Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	repo.clock = func() time.Time { return start.Add(time.Duration(elapsed.Load())) }
	if err := repo.Refresh(); err != nil { // stamps the graph at "start"
		t.Fatal(err)
	}
	reloads := 0
	last := repo.graph.Load()
	for sec := 1; sec <= 12; sec++ {
		elapsed.Store(int64(sec) * int64(time.Second))
		if err := repo.Refresh(); err != nil {
			t.Fatal(err)
		}
		cur := repo.graph.Load()
		if cur != last {
			reloads++
			last = cur
			if sec > 4 {
				t.Errorf("Refresh %d s after the write still reloads the graph", sec)
			}
		}
	}
	if reloads == 0 {
		t.Error("a graph inside the racy window was trusted")
	}
	t.Logf("reloads: %d in 12 refreshes, 1 s apart", reloads)
}

// A graph rewritten the way git does it (new file renamed over the old) is
// always seen, trusted or not.
func TestGraphRefreshSeesARewrite(t *testing.T) {
	r := newGTRepo(t)
	r.commit("a", "a\n")
	r.git("commit-graph", "write", "--reachable")
	ageGraph(t, r, time.Hour)
	repo, err := Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	first := repo.graph.Load()
	for range 3 {
		if err := repo.Refresh(); err != nil {
			t.Fatal(err)
		}
	}
	if repo.graph.Load() != first {
		t.Fatal("unchanged graph reloaded")
	}
	n := first.g.numCommits()
	r.commit("b", "b\n")
	r.git("commit-graph", "write", "--reachable")
	if err := repo.Refresh(); err != nil {
		t.Fatal(err)
	}
	next := repo.graph.Load()
	if next == first || next.g == nil || next.g.numCommits() != n+1 {
		t.Fatalf("rewrite not seen: reloaded %v", next != first)
	}
}
