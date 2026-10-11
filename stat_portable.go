// SPDX-License-Identifier: MIT

//go:build (linux && !(amd64 || arm64)) || darwin || freebsd || openbsd || netbsd || dragonfly

package git

import "syscall"

// statNul fills st with a stat system call into a Stat_t on the stack. The
// one allocation is the NUL-free copy of the path that package syscall turns
// into a C string at each call; stat_linux.go avoids it where the system call
// can be made directly.
func statNul(nul []byte, lstat bool, st *fileStat) error {
	var sys syscall.Stat_t
	path := string(nul[:len(nul)-1])
	var err error
	for {
		if lstat {
			err = syscall.Lstat(path, &sys)
		} else {
			err = syscall.Stat(path, &sys)
		}
		if err != syscall.EINTR {
			break
		}
	}
	return fillStat(st, &sys, nul, err)
}

// openNul opens the file read-only. It allocates the C string of the path,
// which stat_linux.go does not.
func openNul(nul []byte) (rawFile, error) {
	path := string(nul[:len(nul)-1])
	for {
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return rawFile{}, err
		}
		return rawFile{fd}, nil
	}
}
