// SPDX-License-Identifier: MIT

//go:build darwin || freebsd || netbsd

package git

import "syscall"

func statMtime(sys *syscall.Stat_t) int64 {
	return int64(sys.Mtimespec.Sec)*1e9 + int64(sys.Mtimespec.Nsec) //nolint:unconvert // the types differ per platform
}
