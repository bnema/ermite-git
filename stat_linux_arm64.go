// SPDX-License-Identifier: MIT

//go:build linux && arm64

package git

import "syscall"

const sysFstatat = syscall.SYS_FSTATAT
