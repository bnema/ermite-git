// SPDX-License-Identifier: MIT

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package git

import (
	"io"
	"os"
	"syscall"
)

// fillStat converts the result of a stat system call into st. An absent path
// is not an error.
func fillStat(st *fileStat, sys *syscall.Stat_t, nul []byte, err error) error {
	*st = fileStat{}
	if err != nil {
		if err == syscall.ENOENT || err == syscall.ENOTDIR {
			return nil
		}
		return &os.PathError{Op: "stat", Path: string(nul[:len(nul)-1]), Err: err}
	}
	*st = fileStat{
		exists: true,
		isDir:  sys.Mode&syscall.S_IFMT == syscall.S_IFDIR,
		size:   sys.Size,
		mtime:  statMtime(sys),
		ino:    uint64(sys.Ino), //nolint:unconvert // the type differs per OS
	}
	return nil
}

// rawFile is an open file descriptor.
type rawFile struct{ fd int }

// read returns io.EOF at the end of the file.
func (f rawFile) read(p []byte) (int, error) {
	for {
		n, err := syscall.Read(f.fd, p)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	}
}

func (f rawFile) close() { _ = syscall.Close(f.fd) }

// isNotExistErrno reports whether err, from openNul, says that the file or
// a directory on the way is absent.
func isNotExistErrno(err error) bool { return err == syscall.ENOENT || err == syscall.ENOTDIR }

func openError(nul []byte, err error) error {
	return &os.PathError{Op: "open", Path: string(nul[:len(nul)-1]), Err: err}
}
