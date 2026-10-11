// SPDX-License-Identifier: MIT

//go:build linux || openbsd || dragonfly

package git

import "syscall"

func statMtime(sys *syscall.Stat_t) int64 {
	return int64(sys.Mtim.Sec)*1e9 + int64(sys.Mtim.Nsec) //nolint:unconvert // the types differ per platform
}
