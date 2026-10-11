// SPDX-License-Identifier: MIT

//go:build linux && amd64

package git

import "syscall"

const sysFstatat = syscall.SYS_NEWFSTATAT
