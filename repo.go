// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Default bounds. See WithMaxObjectSize and WithDeltaCacheSize.
const (
	defaultMaxObjectSize  = 128 << 20
	defaultDeltaCacheSize = 32 << 20
	// maxDeltaDepth bounds the length of a delta chain. Git itself writes
	// chains of at most 4095 and 50 by default.
	maxDeltaDepth = 10000
)

type options struct {
	maxObject  int64
	cacheBytes int64
}

// Option configures Open.
type Option func(*options)

// WithMaxObjectSize bounds the size of an object that is held in memory:
// commits, trees, tags and blobs that are stored as deltas. Bigger ones fail
// with ErrTooLarge. Undeltified blobs stream and are not bounded. The default
// is 128 MiB. The bound is capped at the largest int of the platform
// (2 GiB - 1 on 32-bit systems), since such an object must fit in a slice.
func WithMaxObjectSize(n int64) Option {
	return func(o *options) {
		if n > 0 {
			o.maxObject = min(n, math.MaxInt)
		}
	}
}

// WithDeltaCacheSize sets the budget in bytes of the cache of delta bases
// (LRU). The default is 32 MiB. Zero or less disables the cache.
func WithDeltaCacheSize(n int64) Option {
	return func(o *options) { o.cacheBytes = n }
}

// Repo is an open Git repository. It is read-only and safe for concurrent
// use by multiple goroutines.
//
// Loose refs and loose objects are always read live. The list of packs and
// the content of packed-refs are loaded snapshots: a missing object triggers
// one rescan of the pack directory, packed-refs is stat-checked on each ref
// read, and Refresh reloads everything explicitly.
type Repo struct {
	dir  string
	opts options

	snap     atomic.Pointer[snapshot]
	mu       sync.Mutex // serializes pack list reloads and Close
	closed   bool
	nextPack atomic.Uint32

	cache *deltaCache

	packed atomic.Pointer[packedRefs]
	pmu    sync.Mutex // serializes packed-refs reloads

	graph atomic.Pointer[graphState] // see walk.go
	gmu   sync.Mutex                 // serializes commit-graph reloads

	clock func() time.Time // tests only: time.Now when nil
}

// now is the time the racy-file rules measure from.
func (r *Repo) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// Open opens the repository whose Git directory is dir: a bare repository
// or a ".git" directory. If dir contains a ".git" directory, that one is
// used.
//
// Open returns an error wrapping ErrNotFound when dir is not a repository
// and one wrapping ErrUnsupported (with the reason) when the repository uses
// a feature the package does not read: an object format other than sha1, a
// ref storage other than files, other extensions, a shallow file,
// info/grafts, replace refs, alternates, a linked worktree, a partial clone,
// or a multi-pack-index whose packs lack their .idx file. A multi-pack-index
// is otherwise ignored: the packs are read through their own .idx files.
func Open(dir string, opts ...Option) (*Repo, error) {
	gitDir, err := findGitDir(dir)
	if err != nil {
		return nil, err
	}
	o := options{maxObject: defaultMaxObjectSize, cacheBytes: defaultDeltaCacheSize}
	for _, fn := range opts {
		fn(&o)
	}
	if err := checkConfig(filepath.Join(gitDir, "config")); err != nil {
		return nil, err
	}
	if err := checkLayout(gitDir); err != nil {
		return nil, err
	}
	r := &Repo{dir: gitDir, opts: o, cache: newDeltaCache(o.cacheBytes)}
	r.snap.Store(newSnapshot(nil))
	if _, err := r.reloadPacks(true); err != nil {
		_ = r.Close()
		return nil, err
	}
	if _, err := r.loadPacked(true); err != nil {
		_ = r.Close()
		return nil, err
	}
	_ = r.refreshGraph(true) // a bad commit-graph is reported by Walk
	return r, nil
}

// Dir returns the Git directory that was opened.
func (r *Repo) Dir() string { return r.dir }

// Close releases the repository. Readers still streaming a blob keep their
// packs mapped until they are closed. Close is idempotent.
func (r *Repo) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if s := r.snap.Swap(nil); s != nil {
		s.release()
	}
	r.closeGraph()
	return nil
}

// Refresh reloads the list of packs and packed-refs when they changed, and
// checks again that the repository is still supported (it returns an error
// wrapping ErrUnsupported when, for example, a replace ref was pushed). Packs
// that disappeared stay mapped until the readers using them are done, so a
// concurrent "git repack -ad" never breaks a read in flight.
func (r *Repo) Refresh() error {
	if err := checkLayout(r.dir); err != nil {
		return err
	}
	if _, err := r.reloadPacks(false); err != nil {
		return err
	}
	if _, err := r.loadPacked(false); err != nil {
		return err
	}
	return r.refreshGraph(false)
}

func findGitDir(dir string) (string, error) {
	for range 2 {
		if isGitDir(dir) {
			return dir, nil
		}
		sub := filepath.Join(dir, ".git")
		fi, err := os.Stat(sub)
		switch {
		case err != nil:
			return "", notFound("%s is not a git repository", dir)
		case fi.IsDir():
			dir = sub
		default:
			return "", unsupported("%s is a gitfile (linked worktree or submodule)", sub)
		}
	}
	return "", notFound("%s is not a git repository", dir)
}

func isGitDir(dir string) bool {
	for _, n := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			return false
		}
	}
	return true
}

// isNotExist reports whether err means that a path, or a directory on the way
// to it, is absent.
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// checkLayout rejects the repository layouts that change what objects or
// refs mean and that the package does not implement.
func checkLayout(dir string) error {
	for _, c := range []struct{ path, why string }{
		{"shallow", "shallow repository"},
		{"info/grafts", "grafts file"},
		{"commondir", "linked worktree"},
		{"objects/info/alternates", "alternate object stores"},
	} {
		if exists(filepath.Join(dir, c.path)) {
			return unsupported("%s (%s)", c.why, c.path)
		}
	}
	f, err := os.Open(filepath.Join(dir, "refs", "replace"))
	if err != nil {
		if isNotExist(err) {
			return nil
		}
		return err
	}
	names, rerr := f.Readdirnames(1)
	_ = f.Close()
	switch {
	case rerr == nil && len(names) > 0:
		return unsupported("replace refs (refs/replace)")
	case rerr == nil, errors.Is(rerr, io.EOF), isNotExist(rerr):
		return nil
	}
	return rerr
}
