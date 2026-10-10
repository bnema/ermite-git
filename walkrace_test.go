// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gitQuiet runs git in the fixture without touching the testing.T, so it can
// be used from goroutines.
func (w *wfix) gitQuiet(stdin string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = w.dir
	cmd.Env = append(gitEnv(), "GIT_DIR="+w.dir)
	cmd.Stdin = strings.NewReader(stdin)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, errb.String())
	}
	return nil
}

// TestWalkConcurrentRefresh runs walks and LastCommits while the repository
// changes under them: Refresh, new commits arriving by fast-import (a push:
// new pack, new graph layer), rewrites of the commit-graph (with and without
// Bloom filters, split and single, or removed) and repacks. Every walk starts
// from a fixed tip and must give the same answer as before the changes.
func TestWalkConcurrentRefresh(t *testing.T) {
	requireGit(t)
	cs := randomHistory(21, 120)
	w := buildWFix(t, cs)
	w.setGraph(wgraphs[4])
	tipIdx := len(cs) - 1
	tip := mustID(t, w.ids[tipIdx])
	paths := []string{"", "a", "d", "d/e/z", "q/r"}
	want := map[string][]string{}
	for _, p := range paths {
		want[p] = w.gitLog(tipIdx, 0, 0, p)
	}
	names := []string{"a", "b", "d", "m", "q", "nope"}
	wantLast := w.lastOracle(tipIdx, "", names)

	r, err := Open(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	dur := 3 * time.Second
	if testing.Short() {
		dur = time.Second
	}
	deadline := time.Now().Add(dur)
	var stop atomic.Bool
	var wg sync.WaitGroup
	errc := make(chan error, 64)
	fail := func(err error) {
		select {
		case errc <- err:
		default:
		}
		stop.Store(true)
	}

	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				p := paths[(g+i)%len(paths)]
				var got []string
				for c, err := range r.Walk(context.Background(), tip, WalkOptions{Path: p, IDOnly: i%2 == 0}) {
					if err != nil {
						fail(fmt.Errorf("walk %q: %w", p, err))
						return
					}
					got = append(got, c.ID.String())
				}
				if !slices.Equal(got, want[p]) {
					fail(fmt.Errorf("walk %q: wrong history (%d commits, want %d)", p, len(got), len(want[p])))
					return
				}
				if i%3 == 0 {
					res, err := r.LastCommits(context.Background(), tip, "", names)
					if err != nil {
						fail(fmt.Errorf("LastCommits: %w", err))
						return
					}
					if !slices.Equal(res, wantLast) {
						fail(fmt.Errorf("LastCommits: wrong answer"))
						return
					}
				}
			}
		}(g)
	}

	wg.Add(1)
	go func() { // Refresh as fast as possible
		defer wg.Done()
		for !stop.Load() {
			if err := r.Refresh(); err != nil {
				fail(fmt.Errorf("Refresh: %w", err))
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() { // the writer
		defer wg.Done()
		defer stop.Store(true)
		var in bytes.Buffer
		for _, id := range w.ids {
			fmt.Fprintln(&in, id)
		}
		for i := 0; time.Now().Before(deadline) && !stop.Load(); i++ {
			var err error
			switch i % 6 {
			case 0: // a push: a new commit on top of the tip, a new pack
				err = w.gitQuiet(fmt.Sprintf("commit refs/heads/push%d\ncommitter T <t@example.com> %d +0000\ndata 1\nx\nfrom %s\n\n", i, 1_700_000_000+i, w.ids[tipIdx]), "fast-import", "--quiet")
			case 1:
				err = w.gitQuiet(in.String(), "commit-graph", "write", "--stdin-commits", "--split", "--changed-paths")
			case 2:
				err = w.gitQuiet("", "repack", "-adq")
			case 3:
				err = w.gitQuiet(in.String(), "commit-graph", "write", "--stdin-commits", "--no-changed-paths")
			case 4:
				err = w.gitQuiet(in.String(), "commit-graph", "write", "--stdin-commits", "--split=replace", "--changed-paths")
			case 5:
				err = w.gitQuiet("", "commit-graph", "write", "--reachable", "--split=no-merge", "--changed-paths")
			}
			if err != nil {
				fail(err)
				return
			}
		}
	}()
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
}
