// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build !unix

package peer

import "errors"

var errNoStatfs = errors.New("peer: free-space check not supported on this platform")

// freeDiskBytes is unavailable on this platform; callers treat an error as
// "cannot check" and proceed without the space pre-flight.
func freeDiskBytes(string) (int64, error) {
	return 0, errNoStatfs
}
