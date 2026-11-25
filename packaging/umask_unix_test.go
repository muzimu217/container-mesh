// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build !windows

package packaging

import "syscall"

// setUmask sets the process umask and returns the previous one.
//
// The umask is a PROCESS-WIDE setting, which is why the caller must
// restore it: a test that leaves the umask at 077 would silently make
// every file the rest of the test binary creates owner-only, and the
// contamination would show up as a failure in a test that has nothing to
// do with permissions.
//
// The value is applied with syscall.Umask rather than by running a
// subshell, because the point of the test is to make the umask EXPLICIT
// for the installer process — and the installer is spawned from this
// process, so it inherits whatever this call leaves in place.
func setUmask(mask int) int {
	return syscall.Umask(mask)
}
