// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build linux

package p2p2

import (
	"encoding/binary"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// recvmmsg(2) is the receive-side mirror of sendmmsg(2).
//
//	int recvmmsg(int sockfd, struct mmsghdr *msgvec, unsigned int vlen,
//	             int flags, struct timespec *timeout);
//
// It fills msgvec[0..n) with the datagrams that were queued, in arrival order,
// and returns n. Unlike sendmmsg there is no "prefix went out" subtlety: the
// datagrams it reports are the ones it dequeued, and the rest stay in the
// socket for the next call.
//
// The timeout argument is passed as NULL. The call is made with MSG_DONTWAIT
// after the runtime poller has reported the socket readable, so a timeout would
// never fire; NULL is the documented way to say "no timeout".
func recvmmsg(fd int, msgs []mmsghdr, flags int) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	r0, _, errno := unix.Syscall6(
		unix.SYS_RECVMMSG,
		uintptr(fd),
		uintptr(unsafe.Pointer(&msgs[0])),
		uintptr(len(msgs)),
		uintptr(flags),
		0, 0,
	)
	if errno != 0 {
		return 0, errno
	}
	return int(r0), nil
}

// recvBatchingSupported reports whether this platform has a vectored datagram
// receive. Tests use it to skip rather than to silently pass on a platform that
// fell back to per-datagram reads.
const recvBatchingSupported = true

// linuxRecvBatchMax is UIO_MAXIOV, the kernel's per-call cap on recvmmsg. Asking
// for more is not an error; the kernel just fills this many.
const linuxRecvBatchMax = linuxMaxBatch

// unixBatchReader receives datagrams with recvmmsg.
//
// Every array it hands the kernel is allocated once, at construction, and
// reused: a receive batch of 32 needs 32 datagram buffers, and allocating them
// per call would put ~38 KiB of garbage on the hot path per batch, which is
// exactly the cost batching exists to remove.
//
// The reader owns the buffers, and readBatch prepares nothing but the two
// fields the kernel writes back. That matters more than it looks: the first
// version rebuilt the whole msghdr array per call, and at depth 1 (which is the
// common depth — see the histogram in KPI.md round 15) the setup for 32
// datagrams cost more than the syscall saved, measuring 2.59 µs against
// recvfrom's 2.00 µs.
//
// Not safe for concurrent use; Conn.readLoop is the only caller.
type unixBatchReader struct {
	rc  syscall.RawConn
	max int

	bufs  [][]byte
	lens  []int
	mmsgs []mmsghdr
	iovs  []unix.Iovec
	names []unix.RawSockaddrAny

	// got and rerr are the callback's return slots. They live on the reader
	// rather than in readBatch's frame because the callback captures them, which
	// forces them to the heap: measured on Darwin at ~490 allocations per 512 KiB
	// transfer (30% of the transfer's allocation count) when they were locals. A
	// field is reused instead.
	got  int
	rerr error
	// readN is how many datagrams the current call asked for, and readFn is the
	// bound callback. Both exist so the callback is allocated once per reader
	// rather than once per call.
	readN  int
	readFn func(uintptr) bool
}

func newBatchReader(rc syscall.RawConn, limit int) batchReader {
	n := limit
	if n <= 0 {
		n = DefaultBatchSize
	}
	if n > linuxRecvBatchMax {
		n = linuxRecvBatchMax
	}
	r := &unixBatchReader{
		rc:    rc,
		max:   n,
		bufs:  make([][]byte, n),
		lens:  make([]int, n),
		mmsgs: make([]mmsghdr, n),
		iovs:  make([]unix.Iovec, n),
		names: make([]unix.RawSockaddrAny, n),
	}
	for i := 0; i < n; i++ {
		r.bufs[i] = make([]byte, MaxDatagram)
		r.iovs[i].Base = &r.bufs[i][0]
		r.iovs[i].SetLen(MaxDatagram)
		h := &r.mmsgs[i].Hdr
		h.Name = (*byte)(unsafe.Pointer(&r.names[i]))
		h.Namelen = unix.SizeofSockaddrAny
		h.Iov = &r.iovs[i]
		h.Iovlen = 1
		h.Control = nil
		h.Controllen = 0
		h.Flags = 0
	}
	r.readFn = r.readOnce
	return r
}

// readOnce is the poller callback used by readBatch. It is bound once in
// newBatchReader; readN, got and rerr are the mutable state it reads and writes.
func (r *unixBatchReader) readOnce(fd uintptr) bool {
	k, e := recvmmsg(int(fd), r.mmsgs[:r.readN], unix.MSG_DONTWAIT)
	if e != nil {
		if e == unix.EAGAIN || e == unix.EWOULDBLOCK {
			// The poller reported readable but the queue is empty again (another
			// consumer, or a wakeup that did not correspond to a datagram).
			// Returning false asks the runtime to wait and call this callback
			// again with the same fd.
			return false
		}
		r.rerr = e
		return true
	}
	r.got = k
	return true
}

func (r *unixBatchReader) maxBatch() int { return r.max }

// buffers returns the reader-owned datagram buffers. The slice is valid for the
// life of the reader; each element is MaxDatagram long.
func (r *unixBatchReader) buffers() ([][]byte, []int) { return r.bufs, r.lens }

// readBatch blocks until at least one datagram is available, then dequeues as
// many as the socket has (up to the batch size) in one syscall.
//
// A short return is the normal case, not an error: recvmmsg reports how many
// datagrams were queued, and the caller's next call picks up whatever arrived
// after. A return of zero with no error would be a lost wakeup, so it is
// reported as a stall rather than retried.
func (r *unixBatchReader) readBatch(n int) (int, error) {
	if n > r.max {
		n = r.max
	}
	if n <= 0 {
		return 0, nil
	}

	// Reset only the fields the kernel writes back. The pointers, the address
	// storage and the iovec lengths are constant for the life of the reader.
	//
	// Namelen is an in/out field (the kernel overwrites it with the actual
	// sockaddr length), so it must be restored before every call. This runs once
	// per call rather than inside the callback because a callback re-entry only
	// happens after EAGAIN, which dequeues nothing and leaves the array exactly as
	// this loop left it.
	for i := 0; i < n; i++ {
		r.mmsgs[i].Hdr.Namelen = unix.SizeofSockaddrAny
		r.mmsgs[i].Len = 0
	}

	// The callback is a field, not a literal, for the same reason got and rerr
	// are: a callback that captures the reader escapes to the heap on every
	// call. Storing it means one allocation per reader instead of one per
	// receive syscall.
	r.readN = n
	r.got = 0
	r.rerr = nil
	err := r.rc.Read(r.readFn)
	if err != nil {
		return r.got, err
	}
	if r.rerr != nil {
		return r.got, r.rerr
	}
	got := r.got
	if got <= 0 {
		return got, errBatchRecvStalled
	}
	for i := 0; i < got; i++ {
		l := int(r.mmsgs[i].Len)
		if l > len(r.bufs[i]) {
			// The kernel never reports more than the iovec length without
			// MSG_TRUNC, but a stale Len would otherwise be read as this call's
			// length.
			l = len(r.bufs[i])
		}
		r.lens[i] = l
	}
	return got, nil
}

// rawSource returns the raw sockaddr of datagram i from the most recent
// readBatch. It aliases reader memory and is valid until the next call.
func (r *unixBatchReader) rawSource(i int) []byte {
	if i < 0 || i >= r.max {
		return nil
	}
	n := int(r.mmsgs[i].Hdr.Namelen)
	if n <= 0 || n > unix.SizeofSockaddrAny {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&r.names[i])), n)
}

// sockaddrToAddr decodes the kernel's raw sockaddr into a net.Addr.
//
// The field offsets are the kernel's, not Go's: the family is a native-endian
// uint16 at offset 0, the port is network order at 2, and the address bytes
// follow. sockaddr_in6 adds flowinfo (native, 4..8) before the address and
// scope_id (native, 24..28) after it. Reading them explicitly avoids depending
// on the layout of unix.RawSockaddrInet4/6, which is asserted nowhere.
func sockaddrToAddr(sa []byte) net.Addr {
	if len(sa) < 2 {
		return nil
	}
	switch binary.NativeEndian.Uint16(sa[0:2]) {
	case unix.AF_INET:
		if len(sa) < 8 {
			return nil
		}
		return &net.UDPAddr{
			IP:   net.IPv4(sa[4], sa[5], sa[6], sa[7]),
			Port: int(binary.BigEndian.Uint16(sa[2:4])),
		}
	case unix.AF_INET6:
		if len(sa) < 28 {
			return nil
		}
		ip := make(net.IP, 16)
		copy(ip, sa[8:24])
		return &net.UDPAddr{
			IP:   ip,
			Port: int(binary.BigEndian.Uint16(sa[2:4])),
			Zone: zoneName(int32(binary.NativeEndian.Uint32(sa[24:28]))),
		}
	}
	return nil
}

// zoneName resolves a scope id to an interface name. It is only called when the
// peer's source address changes, which is once per connection in the normal
// case, so the syscall is not on the hot path.
func zoneName(scope int32) string {
	if scope == 0 {
		return ""
	}
	ifi, err := net.InterfaceByIndex(int(scope))
	if err != nil {
		return ""
	}
	return ifi.Name
}
