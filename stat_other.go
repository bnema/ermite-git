// SPDX-License-Identifier: MIT

//go:build !(linux || darwin || freebsd || openbsd || netbsd || dragonfly)

package git

import "os"

// statNul is os.Stat or os.Lstat where there is no stat system call to make
// directly.
func statNul(nul []byte, lstat bool, st *fileStat) error {
	path := string(nul[:len(nul)-1])
	var fi os.FileInfo
	var err error
	if lstat {
		fi, err = os.Lstat(path)
	} else {
		fi, err = os.Stat(path)
	}
	*st = fileStat{}
	if err != nil {
		if isNotExist(err) {
			return nil
		}
		return err
	}
	*st = fileStat{exists: true, isDir: fi.IsDir(), size: fi.Size(), mtime: fi.ModTime().UnixNano(), ino: fileIno(fi)}
	return nil
}
