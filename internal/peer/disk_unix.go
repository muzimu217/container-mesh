// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build unix

package peer

import "golang.org/x/sys/unix"

// freeDiskBytes reports the space available to unprivileged writers on the
// filesystem holding path.
//
// Bavail (not Bfree) is used deliberately: Bfree includes blocks reserved for
// root, so a transfer sized against it would still fail for a non-root
// receiver — which is the normal deployment for this tool.
func freeDiskBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Bsize is int64 on Linux and uint32 on Darwin; convert explicitly.
	return int64(st.Bavail) * int64(st.Bsize), nil
}
