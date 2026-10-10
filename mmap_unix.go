// SPDX-License-Identifier: MIT

//go:build linux || darwin || freebsd

package git

import (
	"os"
	"syscall"
)

// mapFile maps a whole file read-only. The returned function unmaps it.
func mapFile(f *os.File, size int64) ([]byte, func() error, error) {
	if size <= 0 || int64(int(size)) != size {
		return nil, nil, corrupt("%s: unusable size %d", f.Name(), size)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, &os.PathError{Op: "mmap", Path: f.Name(), Err: err}
	}
	adviseRandom(data)
	return data, func() error { return syscall.Munmap(data) }, nil
}

// fileIno returns the inode number of a stat result, 0 when unknown.
func fileIno(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino) //nolint:unconvert // the type differs per OS
	}
	return 0
}
