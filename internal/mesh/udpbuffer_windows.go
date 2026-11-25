// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build windows

package mesh

import "errors"

// socketReceiveBufferSize is not implemented on Windows.
//
// The buffer accounting the product depends on is a Unix socket option
// convention, and the relay's own deployment target is Linux. The
// product still ASKS for the buffer on Windows (SetReadBuffer is
// portable); it just cannot read the grant back with the same
// accounting, so it reports zero and the shortfall message is left
// empty rather than guessing.
func socketReceiveBufferSize(fd uintptr) (int, error) {
	return 0, errors.New("mesh: reading the socket receive buffer is not supported on this platform")
}
