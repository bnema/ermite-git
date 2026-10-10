// SPDX-License-Identifier: MIT

//go:build !(linux || darwin || freebsd || openbsd || netbsd || dragonfly)

package git

import (
	"io"
	"os"
)

// mapFile reads a whole file in memory, where mmap is not available.
func mapFile(f *os.File, size int64) ([]byte, func() error, error) {
	if size <= 0 || int64(int(size)) != size {
		return nil, nil, corrupt("%s: unusable size %d", f.Name(), size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, nil, err
	}
	return data, func() error { return nil }, nil
}

func fileIno(os.FileInfo) uint64 { return 0 }
