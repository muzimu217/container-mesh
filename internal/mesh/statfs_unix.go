// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build !windows

package mesh

import "syscall"

// fsStat is the portable disk-usage snapshot for the @disk verb.
type fsStat struct {
	totalMiB uint64
	freeMiB  uint64
}

// statFS reads filesystem usage for path. UNIX implementation (linux,
// darwin, and the other unix C-2 targets); windows gets its own file.
func statFS(path string, out *fsStat) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return err
	}
	bsize := uint64(st.Bsize)
	out.totalMiB = st.Blocks * bsize / (1 << 20)
	out.freeMiB = st.Bavail * bsize / (1 << 20)
	return nil
}
