// SPDX-License-Identifier: MIT

package git

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// fileSig identifies a version of a file or directory by stat data, and
// remembers when that data was read.
type fileSig struct {
	exists bool
	size   int64
	mtime  int64 // unix nanoseconds
	ino    uint64
	read   int64 // unix nanoseconds: when the file was stat'ed or read
}

// sigOf returns the signature of fi, which was obtained at read (unix
// nanoseconds). Take read before reading the content: a change that happens
// later is then never mistaken for the version that was read.
func sigOf(fi os.FileInfo, read int64) fileSig {
	return fileSig{exists: true, size: fi.Size(), mtime: fi.ModTime().UnixNano(), ino: fileIno(fi), read: read}
}

func statSig(path string) (fileSig, error) {
	read := time.Now().UnixNano()
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileSig{read: read}, nil
	}
	if err != nil {
		return fileSig{}, err
	}
	return sigOf(fi, read), nil
}

// racyWindow is how long after its modification time a file may still change
// without its stat data changing, because of coarse file system clocks.
const racyWindow = 2 * time.Second

// sameStat reports whether a and b have the same stat data, whenever they
// were read.
func (a fileSig) sameStat(b fileSig) bool {
	return a.exists == b.exists && a.size == b.size && a.mtime == b.mtime && a.ino == b.ino
}

// same reports whether b is the same version as a and can be trusted to be
// unchanged. This is Git's racy rule: a was read at a.read, and a write that
// follows within the clock granularity of the previous modification leaves
// the stat data as it was. So the stat data only proves that nothing
// changed when the file was read more than racyWindow after its mtime.
func (a fileSig) same(b fileSig) bool {
	if !a.sameStat(b) {
		return false
	}
	return !a.exists || a.read-a.mtime > int64(racyWindow)
}

func ignoreMissing(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// snapshot is an immutable list of packs, shared by readers. It is released
// by each reader and by the Repo; the last release drops its packs.
type snapshot struct {
	refs   atomic.Int32
	packs  []*pack // newest first
	dirSig fileSig // of objects/pack when the list was built
}

func newSnapshot(packs []*pack) *snapshot {
	s := &snapshot{packs: packs}
	s.refs.Store(1)
	return s
}

func (s *snapshot) release() {
	if s.refs.Add(-1) == 0 {
		for _, p := range s.packs {
			p.release()
		}
	}
}

// find looks id up in the packs.
func (s *snapshot) find(id *ObjectID) (*pack, uint64, error) {
	for _, p := range s.packs {
		if pos, ok := p.idx.find(id); ok {
			off, err := p.idx.offset(pos)
			if err != nil {
				return nil, 0, wrapPack(p.name, err)
			}
			return p, off, nil
		}
	}
	return nil, 0, nil
}

// acquire returns the current snapshot, which the caller must release.
func (r *Repo) acquire() (*snapshot, error) {
	for {
		s := r.snap.Load()
		if s == nil {
			return nil, ErrClosed
		}
		n := s.refs.Load()
		if n > 0 && s.refs.CompareAndSwap(n, n+1) {
			return s, nil
		}
	}
}

func (r *Repo) packDir() string { return r.dir + "/objects/pack" }

// reloadPacks rescans objects/pack and installs a new snapshot when the
// directory changed, or always with force. It reports whether the list of
// packs changed.
func (r *Repo) reloadPacks(force bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false, ErrClosed
	}
	cur := r.snap.Load()
	dsig, err := r.paths.packDir.sig()
	if err != nil {
		return false, err
	}
	if !force && cur.dirSig.same(dsig) {
		return false, nil
	}
	dir := r.packDir()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	var idxNames []string
	hasMidx := false
	for _, e := range entries {
		n := e.Name()
		switch {
		case n == "multi-pack-index":
			hasMidx = true
		case n == "multi-pack-index.d":
			return false, unsupported("incremental multi-pack-index (objects/pack/multi-pack-index.d)")
		case strings.HasPrefix(n, "pack-") && strings.HasSuffix(n, ".promisor"):
			return false, unsupported("partial clone (%s)", n)
		case strings.HasPrefix(n, "pack-") && strings.HasSuffix(n, ".idx"):
			idxNames = append(idxNames, strings.TrimSuffix(n, ".idx"))
		}
	}
	if hasMidx {
		if err := checkMidx(dir+"/multi-pack-index", idxNames); err != nil {
			return false, err
		}
	}

	old := make(map[string]*pack, len(cur.packs))
	for _, p := range cur.packs {
		old[p.name] = p
	}
	var packs []*pack
	changed := len(idxNames) != len(cur.packs)
	for _, name := range idxNames {
		isig, err := statSig(dir + "/" + name + ".idx")
		if err != nil {
			return false, err
		}
		if p := old[name]; p != nil && p.sig.sameStat(isig) {
			p.retain()
			packs = append(packs, p)
			continue
		}
		p, err := openPack(dir, name, r.nextPack.Add(1), isig)
		if err != nil {
			for _, q := range packs {
				q.release()
			}
			return false, err
		}
		if p == nil { // replaced while we looked
			changed = true
			continue
		}
		changed = true
		packs = append(packs, p)
	}
	if len(packs) != len(cur.packs) {
		changed = true
	}
	slices.SortStableFunc(packs, func(a, b *pack) int {
		switch {
		case a.sig.mtime > b.sig.mtime:
			return -1
		case a.sig.mtime < b.sig.mtime:
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	if !changed {
		// Same packs: keep the current snapshot (and its readers) unless the
		// directory stat data moved on.
		for _, p := range packs {
			p.release()
		}
		if cur.dirSig.sameStat(dsig) {
			// The listing above was made after dsig was taken: record it, so
			// that a directory that was racy when first read stops being
			// rescanned once its window has passed. dirSig is only used
			// with r.mu held.
			cur.dirSig = dsig
			return false, nil
		}
		packs = cur.packs
		for _, p := range packs {
			p.retain()
		}
	}
	ns := newSnapshot(packs)
	ns.dirSig = dsig
	r.snap.Store(ns)
	cur.release()
	return changed, nil
}

const (
	midxHeaderLen = 12
	midxChunkPNAM = 0x504e414d
	maxMidxNames  = 4 << 20
)

// checkMidx accepts a multi-pack-index only when every pack it lists has
// its own .idx file: the midx is then redundant and ignored.
func checkMidx(path string, idxNames []string) error {
	f, err := os.Open(path)
	if err != nil {
		return ignoreMissing(err)
	}
	defer func() { _ = f.Close() }()
	var hdr [midxHeaderLen]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil || string(hdr[:4]) != "MIDX" {
		return unsupported("unreadable multi-pack-index")
	}
	if hdr[4] != 1 || hdr[5] != 1 {
		return unsupported("multi-pack-index version %d, object format %d", hdr[4], hdr[5])
	}
	chunks := int(hdr[6])
	if hdr[7] != 0 {
		return unsupported("incremental multi-pack-index")
	}
	nPacks := binary.BigEndian.Uint32(hdr[8:])
	table := make([]byte, (chunks+1)*12)
	if _, err := f.ReadAt(table, midxHeaderLen); err != nil {
		return unsupported("multi-pack-index chunk table: %v", err)
	}
	var start, end uint64
	for i := 0; i < chunks; i++ {
		if binary.BigEndian.Uint32(table[i*12:]) == midxChunkPNAM {
			start = binary.BigEndian.Uint64(table[i*12+4:])
			end = binary.BigEndian.Uint64(table[(i+1)*12+4:])
			break
		}
	}
	if end <= start || end-start > maxMidxNames {
		return unsupported("multi-pack-index without a usable pack name chunk")
	}
	names := make([]byte, end-start)
	if _, err := f.ReadAt(names, int64(start)); err != nil {
		return unsupported("multi-pack-index pack names: %v", err)
	}
	have := make(map[string]bool, len(idxNames))
	for _, n := range idxNames {
		have[n] = true
	}
	listed := uint32(0)
	for len(names) > 0 && listed < nPacks {
		i := 0
		for i < len(names) && names[i] != 0 {
			i++
		}
		if i == len(names) {
			break
		}
		name := string(names[:i])
		names = names[i+1:]
		listed++
		if name == "" { // padding
			continue
		}
		name = strings.TrimSuffix(strings.TrimSuffix(name, ".idx"), ".pack")
		if !have[name] {
			return unsupported("multi-pack-index lists %s, which has no .idx file", truncate(name))
		}
	}
	return nil
}
