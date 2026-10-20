// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"bytes"
	"errors"
	"net"
	"syscall"
)

// Batched datagram reception: the mirror of batch.go.
//
// Round 13 removed the send side's one-syscall-per-datagram floor and took
// 512 KiB at 0% loss from ~101 to ~210 MiB/s (KPI.md round 13, D57-D60). The
// receive side kept its floor: readLoop still made one ReadFrom per datagram,
// and the profile put 38% of the transfer's remaining CPU there — the largest
// single item left, and roughly equal to the batched send syscalls themselves.
//
// Both target platforms have a vectored receive that carries N datagrams in one
// syscall:
//
//	Linux   recvmmsg(2)   N msghdr,   returns how many arrived
//	Darwin  recvmsg_x(2)  N msghdr_x, returns how many arrived
//
// The syscall is platform-specific and lives in recvbatch_linux.go,
// recvbatch_darwin.go and recvbatch_other.go behind batchReader. Everything
// that decides what a short return means, how buffers are reused, and how the
// peer's address is tracked is here and is shared.
//
// The property that must not change is ordering. Both calls fill the array in
// arrival order, so processing the returned prefix in index order is exactly
// the order the datagrams arrived in. A short return is normal, not an error:
// it means the socket queue had fewer datagrams than the batch could hold, and
// the next call picks up whatever arrived after. Nothing is skipped and nothing
// is reordered.
//
// The receive path cannot be confined to "first transmissions" the way the send
// path was, because every datagram — new data, retransmission, ACK, ping — goes
// through it. What makes that safe is that batching here changes only *how many
// datagrams one syscall hands over*, never what happens to a datagram once it
// is decoded: the read loop dispatches each one through the same decode, the
// same duplicate test and the same reassembly as before.

// errBatchRecvStalled means a batched receive reported neither a datagram nor
// an error, which no implementation is allowed to do. Retrying would spin
// forever, so it is treated as fatal.
var errBatchRecvStalled = errors.New("p2p2: batch receive made no progress")

// batchReader is the platform half of batched receiving: one syscall that fills
// up to n of the reader's own buffers with datagrams, in arrival order.
//
// Implementations block until at least one datagram is available and then take
// as many as the socket has (up to n). They must never report (0, nil): the
// caller cannot distinguish that from a lost wakeup and would spin.
//
// The buffers belong to the reader, not to the caller. That is deliberate: the
// destination pointers and the address storage are the same on every call, so
// the reader prepares its msghdr array once at construction and a call costs one
// syscall plus a handful of field resets. Passing caller-owned buffers would
// force the reader to rebuild the array per call, and at the depth this path
// actually sees (mean 2.9 datagrams — KPI.md round 15) that setup is more
// expensive than the syscall it amortises.
type batchReader interface {
	// readBatch blocks for at least one datagram and returns how many it
	// dequeued into the buffers from buffers(), at most n.
	readBatch(n int) (int, error)
	// buffers returns the reader's datagram buffers and their lengths. Both
	// slices are valid for the life of the reader; a decoded packet's payload
	// aliases buffers()[i], so it is only valid until the next readBatch.
	buffers() ([][]byte, []int)
	// maxBatch is the largest number of datagrams one call accepts. It is a
	// property of the platform call and of how many buffers the reader
	// preallocated, not a tuning knob.
	maxBatch() int
	// rawSource returns the raw sockaddr of datagram i from the most recent
	// readBatch. The slice aliases reader memory and is valid until the next
	// call. It exists so the caller can decide "did the source change" with a
	// 16-byte compare instead of allocating a net.Addr per datagram.
	//
	// Callers must ask in ascending index order within a batch.
	rawSource(i int) []byte
}

// batchReceiver is implemented by a packet connection that can expose a raw
// handle for batched receives. *net.UDPConn satisfies it through SyscallConn.
type batchReceiver interface {
	SyscallConn() (syscall.RawConn, error)
}

// receiverFor returns the batch reader for pc, or nil when pc cannot batch the
// receive path. The decision is made once, at construction, for the same reason
// as senderFor: the read loop must not pay an interface assertion per datagram.
//
// limit is the configured batch depth; it sizes the preallocated buffers, so a
// connection configured for 32 datagrams does not reserve a megabyte of receive
// space for a 1024-deep batch it will never ask for.
//
// The batchProvider branch comes first and is the reason a wrapper stays in the
// loop. A wrapper that embeds a net.PacketConn and forwards ReadFrom — the loss
// injection proxy in harness_test.go is exactly that — must be asked for its
// own reader, or the batched path would read the wrapped socket directly and
// every loss test would silently measure a lossless path. That is the same
// correctness argument as D58's, applied to the other direction.
func receiverFor(pc net.PacketConn, limit int) batchReader {
	if bp, ok := pc.(batchProvider); ok {
		return bp.batchReader(limit)
	}
	if br, ok := pc.(batchReceiver); ok {
		sc, err := br.SyscallConn()
		if err != nil {
			return nil
		}
		return newBatchReader(sc, limit)
	}
	return nil
}

// maxSockaddrLen is larger than both platforms' sockaddr storage (112 on Linux,
// 108 on Darwin), so a cached source never has to be truncated.
const maxSockaddrLen = 128

// srcTracker turns the raw sockaddrs of a batch into "the source changed, and
// here it is" events.
//
// The per-datagram path compared addr.String() against the remote for every
// datagram, which allocates a string and takes the remote lock 464 times for a
// 512 KiB message. The batched path compares the kernel's raw sockaddr instead:
// a 16- or 28-byte compare per datagram, and a decode plus a SetRemote only when
// the source actually changes — once per connection in the normal case, and
// once per migration otherwise.
//
// This is not only a micro-optimisation. It preserves the existing semantics
// exactly: the read loop still follows a source that differs from the current
// remote, and still leaves the remote alone when it matches.
type srcTracker struct {
	raw  [maxSockaddrLen]byte
	n    int
	have bool
}

// changed decodes the raw sockaddr sa and reports it as a new source address
// when it differs from both the previous batch's source and cur, the address
// the connection currently uses. cur is read once per batch by the caller.
func (t *srcTracker) changed(sa []byte, cur net.Addr) (net.Addr, bool) {
	if len(sa) == 0 || len(sa) > maxSockaddrLen {
		return nil, false
	}
	if t.have && t.n == len(sa) && bytes.Equal(t.raw[:t.n], sa) {
		return nil, false
	}
	copy(t.raw[:], sa)
	t.n = len(sa)
	t.have = true
	a := sockaddrToAddr(sa)
	if a == nil {
		return nil, false
	}
	if cur != nil && sameAddr(a, cur) {
		return nil, false
	}
	return a, true
}

// sameAddr compares two UDP addresses without allocating.
func sameAddr(a, b net.Addr) bool {
	ua, ok := a.(*net.UDPAddr)
	if !ok {
		return a.String() == b.String()
	}
	ub, ok := b.(*net.UDPAddr)
	if !ok {
		return a.String() == b.String()
	}
	return ua.Port == ub.Port && ua.Zone == ub.Zone && ua.IP.Equal(ub.IP)
}
