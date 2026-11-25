// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build windows

package packaging

// setUmask is a no-op on Windows, which has no umask.
//
// The tests that depend on a restrictive umask are skipped there (the
// installer is a POSIX shell script and does not run on Windows
// anyway); this stub exists so the package still compiles for a
// cross-platform `go vet ./...`.
func setUmask(mask int) int {
	return 0
}
