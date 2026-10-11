// SPDX-License-Identifier: MIT

//go:build !(linux || darwin || freebsd || openbsd || netbsd || dragonfly)

package git

import (
	"errors"
	"io"
	"os"
)

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

// rawFile is an open file.
type rawFile struct{ f *os.File }

func openNul(nul []byte) (rawFile, error) {
	f, err := os.Open(string(nul[:len(nul)-1]))
	return rawFile{f}, err
}

// read returns io.EOF at the end of the file.
func (f rawFile) read(p []byte) (int, error) {
	n, err := f.f.Read(p)
	if n > 0 && errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

func (f rawFile) close() { _ = f.f.Close() }

func isNotExistErrno(err error) bool { return isNotExist(err) }

func openError(_ []byte, err error) error { return err }
