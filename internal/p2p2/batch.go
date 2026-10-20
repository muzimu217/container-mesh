// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"errors"
	"net"
	"sync/atomic"
	"syscall"
)

// Batched datagram transmission.
//
// The send path used to make one WriteTo syscall per datagram. A 512 KiB
// message is 464 datagrams, and profiling put 66-84% of the transfer's CPU in
// rawsyscalln: the transfer was syscall-bound, not protocol-bound (KPI.md
// round 10, D55). Both target platforms have a vectored send that carries N
// datagrams in one syscall:
//
//	Linux   sendmmsg(2)   N msghdr,   returns how many were sent
//	Darwin  sendmsg_x(2)  N msghdr_x, returns how many were sent
//
// The syscall itself is platform-specific and lives in batch_linux.go,
// batch_darwin.go and batch_other.go behind batchWriter. Everything that
// decides *what* to batch, and what a short send means, is here and is shared.
//
// The property that must not change is ordering. Both calls transmit the array
// in order and a short return means "the first n went out", so a batch is
// always a contiguous ascending run of sequence numbers, and a short return is
// handled by re-submitting the tail. Nothing is reordered and nothing unsent is
// declared lost.
//
// Batching is confined to the first transmission of a message, inside Send.
// Retransmissions (fast retransmit, RTO) still write one datagram at a time:
// they are rare, they are not contiguous with the new-data stream, and keeping
// them out of the batch means the batching code cannot affect loss recovery at
// all. That is why the loss-rate KPIs are unchanged by construction rather than
// by tuning.

var (
	// errNoBatch means batching is unavailable; callers fall back to one write
	// per datagram.
	errNoBatch = errors.New("p2p2: batching unavailable")
	// errBatchStalled means a batch call reported neither progress nor an error,
	// which no implementation is allowed to do. Treating it as fatal is the
	// honest response: retrying would spin forever.
	errBatchStalled = errors.New("p2p2: batch send made no progress")
	// errBatchUnsupported means the peer address is not an address family the
	// batched call can express. It disables batching for the connection.
	errBatchUnsupported = errors.New("p2p2: address family not supported by batch send")
)

// batchWriter is the platform half of batched sending: one syscall that carries
// up to len(msgs) datagrams to addr.
//
// Implementations either send a prefix of msgs and report how many, or return
// an error. They must never report (0, nil): the caller cannot distinguish that
// from a lost wakeup and would spin.
type batchWriter interface {
	writeBatch(msgs [][]byte, addr net.Addr) (sent int, err error)
	// maxBatch is the largest number of datagrams one call accepts. It is a
	// property of the platform call, not a tuning knob.
	maxBatch() int
}

// batchSender is implemented by a packet connection that can expose a raw
// handle for batched sends. *net.UDPConn satisfies it through SyscallConn.
type batchSender interface {
	SyscallConn() (syscall.RawConn, error)
}

// batchProvider lets a wrapper decide batching for itself.
//
// This matters for correctness, not just for tests. A wrapper that embeds a
// net.PacketConn and forwards WriteTo — the loss-injection proxy in
// harness_test.go is exactly that — would otherwise have its WriteTo bypassed:
// the batch call would go straight to the wrapped socket and every loss test
// using the wrapper would silently measure a lossless path. Returning nil here
// keeps the wrapper in the loop; returning a wrapped writer keeps its
// per-datagram decisions in front of the syscall.
type batchProvider interface {
	batchWriter() batchWriter
}

// senderFor returns the batch writer for pc, or nil when pc cannot batch. The
// decision is made once, at construction: a socket does not gain or lose the
// ability during its life, and the send path must not pay an interface
// assertion per datagram.
func senderFor(pc net.PacketConn) batchWriter {
	if bp, ok := pc.(batchProvider); ok {
		return bp.batchWriter()
	}
	if bs, ok := pc.(batchSender); ok {
		sc, err := bs.SyscallConn()
		if err != nil {
			return nil
		}
		return newBatchWriter(sc)
	}
	return nil
}

// batchQueue accumulates the datagrams of one Send and flushes them in order.
//
// It is a buffer with an explicit flush rather than a goroutine and a channel.
// A channel would add a handoff per datagram — exactly the per-datagram cost
// batching exists to remove — and would decouple the write from the caller's
// flow control: Send must not report progress for datagrams that are still
// sitting in a queue, or the send window would be measuring buffered bytes
// instead of bytes on the wire.
//
// The pending buffer is owned by Send, which sendMu serialises, so it needs no
// lock of its own. The counters are read by Stats from other goroutines, so
// they are atomic.
type batchQueue struct {
	w   batchWriter
	max int

	calls   atomic.Uint64
	packets atomic.Uint64
}

func newBatchQueue(w batchWriter) *batchQueue {
	if w == nil {
		return nil
	}
	n := w.maxBatch()
	if n < 1 {
		return nil
	}
	return &batchQueue{w: w, max: n}
}

// enabled reports whether this queue batches. Safe on a nil receiver.
func (q *batchQueue) enabled() bool { return q != nil && q.w != nil }

// limit is how many datagrams may be accumulated before a flush. Safe on a nil
// receiver, where it is 1 (write one at a time).
func (q *batchQueue) limit() int {
	if !q.enabled() {
		return 1
	}
	return q.max
}

// write transmits a run of datagrams to addr, retrying the tail after a short
// return.
//
// A short return is not an error: sendmmsg/sendmsg_x report how many datagrams
// they managed, and the rest are still ours to send. Only an error that makes
// no progress is fatal. A full socket buffer does not produce a short return
// here — the raw write parks on the runtime poller until the socket is writable
// — so a partial count means the kernel accepted fewer than asked, and the
// remainder goes out on the next call.
func (q *batchQueue) write(msgs [][]byte, addr net.Addr) error {
	if !q.enabled() {
		return errNoBatch
	}
	rest := msgs
	for len(rest) > 0 {
		n, err := q.w.writeBatch(rest, addr)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(rest) {
			return errBatchStalled
		}
		q.packets.Add(uint64(n))
		rest = rest[n:]
	}
	q.calls.Add(1)
	return nil
}

// batchSnapshot is a copy of the batch counters for Stats.
type batchSnapshot struct {
	calls   uint64
	packets uint64
	max     int
	enabled bool
}

func (q *batchQueue) snapshot() batchSnapshot {
	if !q.enabled() {
		return batchSnapshot{}
	}
	return batchSnapshot{
		calls:   q.calls.Load(),
		packets: q.packets.Load(),
		max:     q.max,
		enabled: true,
	}
}

// htons converts a port to network byte order. Both platforms' sockaddr put the
// port in network order while the family field stays in host order, so the
// typed RawSockaddr structures are filled in and handed to the kernel directly.
func htons(port int) uint16 { return uint16(port)<<8 | uint16(port)>>8 }

// cappedWriter applies Config.BatchSize on top of a platform writer.
//
// Two limits meet here and they are different things: the platform limit is a
// hard property of the syscall (Linux UIO_MAXIOV, and the preallocated control
// arrays), while the configured size is the operating point. The effective
// batch is the smaller of the two, and the truncation happens before the call
// so the platform writer never sees more messages than it prepared for.
type cappedWriter struct {
	w   batchWriter
	max int
}

func (c *cappedWriter) writeBatch(msgs [][]byte, addr net.Addr) (int, error) {
	if n := c.maxBatch(); len(msgs) > n {
		msgs = msgs[:n]
	}
	return c.w.writeBatch(msgs, addr)
}

func (c *cappedWriter) maxBatch() int {
	n := c.w.maxBatch()
	if c.max > 0 && c.max < n {
		return c.max
	}
	return n
}
