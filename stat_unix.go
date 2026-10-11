// SPDX-License-Identifier: MIT

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package git

import (
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
