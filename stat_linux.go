// SPDX-License-Identifier: MIT

//go:build linux && (amd64 || arm64)

package git

import (
	"syscall"
	"unsafe"
)

const atSymlinkNoFollow = 0x100

// statNul is syscall.Stat and syscall.Lstat without their allocation: package
// syscall copies the path into a new NUL-terminated byte slice at each call,
// here the caller built it once. It makes the system call they make on this
// platform, fstatat(AT_FDCWD, path, &stat, flags), into the same Stat_t.
func statNul(nul []byte, lstat bool, st *fileStat) error {
	var sys syscall.Stat_t
	var flags uintptr
	if lstat {
		flags = atSymlinkNoFollow
	}
	atFDCWD := -100
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(sysFstatat, uintptr(atFDCWD), uintptr(unsafe.Pointer(&nul[0])), uintptr(unsafe.Pointer(&sys)), flags, 0, 0)
		if errno != syscall.EINTR {
			break
		}
	}
	var err error
	if errno != 0 {
		err = errno
	}
	return fillStat(st, &sys, nul, err)
}

const (
	oRdonly  = 0
	oCloexec = 0x80000
)

// openNul opens the file read-only with openat(AT_FDCWD, path, flags), the
// system call syscall.Open makes, from a path that is already NUL-terminated.
func openNul(nul []byte) (rawFile, error) {
	atFDCWD := -100
	for {
		fd, _, errno := syscall.Syscall6(sysOpenat, uintptr(atFDCWD), uintptr(unsafe.Pointer(&nul[0])), oRdonly|oCloexec, 0, 0, 0)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return rawFile{}, errno
		}
		return rawFile{int(fd)}, nil
	}
}
