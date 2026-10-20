// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build !linux && !darwin

package p2p2

import (
	"net"
	"syscall"
)

// No batched send on this platform.
//
// Linux has sendmmsg and Darwin has sendmsg_x; the other ports do not expose a
// vectored datagram send through golang.org/x/sys/unix, and the ones that do
// (FreeBSD's sendmmsg) are not targets of this project. newBatchWriter returns
// nil, so senderFor reports "cannot batch", batchQueue is nil, and the send
// path writes one datagram per WriteTo — the behaviour that existed before
// batching, with the same interface. Nothing else in the package changes: the
// interface is what the rest of the code sees, so the fallback is not a
// separate code path to test but the same one with a nil queue.
//
// The signature must stay identical to the Linux and Darwin versions, which is
// why the parameter is named and unused rather than dropped.
func newBatchWriter(rc syscall.RawConn) batchWriter {
	_ = rc
	return nil
}

// batchingSupported is false here: there is no vectored datagram send to fall
// back from, so the tests that assert batch behaviour skip instead of passing
// vacuously.
const batchingSupported = false

// net is imported for the signature symmetry of the platform files; the
// fallback builds no sockaddr and so does not use it.
var _ net.Addr
