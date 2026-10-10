// SPDX-License-Identifier: MIT

//go:build race

package git

// raceEnabled is true under the race detector, which makes sync.Pool drop
// items at random and so changes allocation counts.
const raceEnabled = true
