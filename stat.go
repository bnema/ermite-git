// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"io"
	"syscall"
	"time"
)

// fileStat is the part of a stat result that change detection uses.
type fileStat struct {
	exists bool
	isDir  bool
	size   int64
	mtime  int64 // unix nanoseconds
	ino    uint64
}

// statPath is a path prepared once, so that stat-ing it again and again (what
// Refresh does) does not build a string, nor the NUL-terminated copy of it
// that the system call takes, at each call.
type statPath struct {
	nul []byte // the path and a NUL byte
}

func newStatPath(path string) statPath {
	return statPath{nul: append([]byte(path), 0)}
}

func (p *statPath) path() string { return string(p.nul[:len(p.nul)-1]) }

// stat returns the stat data of the path, following symbolic links unless
// lstat. A path that is absent, or has a file in place of a directory on the
// way, is not an error: exists is false.
func (p *statPath) stat(lstat bool) (fileStat, error) {
	var st fileStat
	err := statNul(p.nul, lstat, &st)
	return st, err
}

// sig returns the signature of the file. The time of the read is taken
// before the stat, see sigOf.
func (p *statPath) sig() (fileSig, error) {
	read := time.Now().UnixNano()
	st, err := p.stat(false)
	if err != nil {
		return fileSig{}, err
	}
	if !st.exists {
		return fileSig{read: read}, nil
	}
	return fileSig{exists: true, size: st.size, mtime: st.mtime, ino: st.ino, read: read}, nil
}

// readFull reads the file into buf until buf is full or the file ends, and
// returns the number of bytes. A directory reads as an empty, missing file:
// dir is true.
func (f rawFile) readFull(buf []byte) (n int, dir bool, err error) {
	for n < len(buf) {
		m, err := f.read(buf[n:])
		n += m
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, syscall.EISDIR) {
				return 0, true, nil
			}
			return n, false, err
		}
	}
	return n, false, nil
}
