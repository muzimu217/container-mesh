// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build windows

package mesh

import "golang.org/x/sys/windows"

// fsStat is the portable disk-usage snapshot for the @disk verb.
type fsStat struct {
	totalMiB uint64
	freeMiB  uint64
}

// statFS reads filesystem usage for path (windows implementation).
func statFS(path string, out *fsStat) error {
	cwd, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var freeQ, totalQ, availQ uint64
	if err := windows.GetDiskFreeSpaceEx(cwd, &freeQ, &totalQ, &availQ); err != nil {
		return err
	}
	out.totalMiB = totalQ / (1 << 20)
	out.freeMiB = availQ / (1 << 20)
	return nil
}
