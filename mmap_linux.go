// SPDX-License-Identifier: MIT

//go:build linux

package git

import "syscall"

// adviseRandom tells the kernel that lookups are random accesses, so it
// should not read ahead. The advice is best effort.
func adviseRandom(data []byte) { _ = syscall.Madvise(data, syscall.MADV_RANDOM) }
