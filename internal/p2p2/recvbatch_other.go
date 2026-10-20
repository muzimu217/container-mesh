// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build !linux && !darwin

package p2p2

import (
	"net"
	"syscall"
)

// No batched receive on this platform.
//
// Linux has recvmmsg and Darwin has recvmsg_x; the other ports do not expose a
// vectored datagram receive through golang.org/x/sys/unix, and the ones that do
// (FreeBSD's recvmmsg) are not targets of this project. newBatchReader returns
// nil, so receiverFor reports "cannot batch", the Conn keeps a nil reader, and
// readLoop takes the per-datagram ReadFrom path — the behaviour that existed
// before receive batching, with the same interface. Nothing else in the package
// changes: the interface is what the rest of the code sees, so the fallback is
// not a separate code path to test but the same one with a nil reader.
//
// The signature must stay identical to the Linux and Darwin versions, which is
// why the parameters are named and unused rather than dropped.
func newBatchReader(rc syscall.RawConn, limit int) batchReader {
	_, _ = rc, limit
	return nil
}

// recvBatchingSupported is false here: there is no vectored datagram receive to
// fall back from, so the tests that assert receive-batch behaviour skip instead
// of passing vacuously.
const recvBatchingSupported = false

// sockaddrToAddr is unreachable here (no reader ever produces a raw sockaddr),
// but the shared read loop references it, so it must exist. Returning nil makes
// the caller keep the configured remote, which is the only sane behaviour on a
// platform whose addresses this package never decodes.
func sockaddrToAddr(sa []byte) net.Addr {
	_ = sa
	return nil
}
