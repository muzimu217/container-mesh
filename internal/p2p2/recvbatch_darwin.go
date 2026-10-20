// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build darwin

package p2p2

import (
	"encoding/binary"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// recvmsg_x(2) is Darwin's batched receive, the counterpart of sendmsg_x(2):
//
//	int recvmsg_x(int s, struct msghdr_x *msgp, u_int cnt, int flags);
//
// It fills msgp[0..n) with the datagrams that were queued, in arrival order, and
// returns n. The element type is the same msghdrX used by the send path
// (batch_darwin.go), with msg_datalen filled in by the kernel on return.
//
// recvmsg_x is not declared by golang.org/x/sys/unix, so the syscall number
// comes from unix.SYS_RECVMSG_X and the wrapper is written here, exactly as for
// sendmsg_x.
func recvmsgX(fd int, msgs []msghdrX, flags int) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	r0, _, errno := unix.Syscall6(
		unix.SYS_RECVMSG_X,
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

// darwinRecvBatchMax is how many datagrams one recvmsg_x call may carry.
//
// It matches darwinMaxBatch: the socket receive queue is bounded by the read
// buffer (8 MiB set in NewConn), and a batch deeper than the send path's own
// window could never be filled. Measured during the probe that validated this
// file: 5 queued datagrams, one call returned 5, an immediately following call
// returned EAGAIN — so the call reports exactly what was queued.
const darwinRecvBatchMax = darwinMaxBatch

// darwinBatchReader receives datagrams with recvmsg_x.
//
// Every array is allocated once, at construction, and reused; see
// unixBatchReader for why the msghdr array is prebuilt rather than rebuilt per
// call.
//
// Not safe for concurrent use; Conn.readLoop is the only caller.
type darwinBatchReader struct {
	rc  syscall.RawConn
	max int

	bufs  [][]byte
	lens  []int
	msgs  []msghdrX
	iovs  []unix.Iovec
	names []unix.RawSockaddrAny

	// got and rerr are the callback's return slots. They live on the reader
	// rather than in readBatch's frame because the callback captures them, which
	// forces them to the heap: measured at ~490 allocations per 512 KiB transfer
	// (30% of the transfer's allocation count) when they were locals. A field is
	// reused instead.
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
	if n > darwinRecvBatchMax {
		n = darwinRecvBatchMax
	}
	r := &darwinBatchReader{
		rc:    rc,
		max:   n,
		bufs:  make([][]byte, n),
		lens:  make([]int, n),
		msgs:  make([]msghdrX, n),
		iovs:  make([]unix.Iovec, n),
		names: make([]unix.RawSockaddrAny, n),
	}
	for i := 0; i < n; i++ {
		r.bufs[i] = make([]byte, MaxDatagram)
		r.iovs[i].Base = &r.bufs[i][0]
		r.iovs[i].SetLen(MaxDatagram)
		m := &r.msgs[i]
		m.name = (*byte)(unsafe.Pointer(&r.names[i]))
		m.namelen = unix.SizeofSockaddrAny
		m.iov = &r.iovs[i]
		m.iovlen = 1
		m.control = nil
		m.controllen = 0
		m.flags = 0
		m.datalen = 0
	}
	r.readFn = r.readOnce
	return r
}

func (r *darwinBatchReader) maxBatch() int { return r.max }

// buffers returns the reader-owned datagram buffers.
func (r *darwinBatchReader) buffers() ([][]byte, []int) { return r.bufs, r.lens }

// readBatch blocks until at least one datagram is available, then dequeues as
// many as the socket has (up to n) in one syscall.
func (r *darwinBatchReader) readBatch(n int) (int, error) {
	if n > r.max {
		n = r.max
	}
	if n <= 0 {
		return 0, nil
	}

	// namelen is an in/out field: the kernel overwrites it with the actual
	// sockaddr length on every datagram it returns, so it has to be restored
	// before each call or the next one would truncate the address. This runs once
	// per call rather than inside the callback because a callback re-entry only
	// happens after EAGAIN, which dequeues nothing and therefore leaves the array
	// exactly as this loop left it.
	for i := 0; i < n; i++ {
		r.msgs[i].namelen = unix.SizeofSockaddrAny
		r.msgs[i].flags = 0
		r.msgs[i].datalen = 0
	}

	// The closure is a field, not a literal, for the same reason got and rerr
	// are: a closure that captures the reader escapes to the heap on every call.
	// Storing it means one allocation per reader instead of one per receive
	// syscall (measured: 191 allocations per 512 KiB transfer, 13% of the
	// transfer's total).
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
		l := int(r.msgs[i].datalen)
		if l > len(r.bufs[i]) {
			l = len(r.bufs[i])
		}
		r.lens[i] = l
	}
	return got, nil
}

// readFn is the poller callback used by readBatch. It is bound once in
// newBatchReader; readN, got and rerr are the mutable state it reads and writes.
func (r *darwinBatchReader) readOnce(fd uintptr) bool {
	k, e := recvmsgX(int(fd), r.msgs[:r.readN], unix.MSG_DONTWAIT)
	if e != nil {
		if e == unix.EAGAIN || e == unix.EWOULDBLOCK {
			// Readable but empty again: ask the poller to wait and call this
			// callback once more with the same fd.
			return false
		}
		r.rerr = e
		return true
	}
	r.got = k
	return true
}

// rawSource returns the raw sockaddr of datagram i from the most recent
// readBatch. It aliases reader memory and is valid until the next call.
func (r *darwinBatchReader) rawSource(i int) []byte {
	if i < 0 || i >= r.max {
		return nil
	}
	n := int(r.msgs[i].namelen)
	if n <= 0 || n > unix.SizeofSockaddrAny {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&r.names[i])), n)
}

// sockaddrToAddr decodes the kernel's raw sockaddr into a net.Addr.
//
// Darwin's sockaddr is not the Linux one: byte 0 is sin_len and byte 1 is
// sin_family, while the port and address sit at the same offsets as everywhere
// else. Reading the bytes explicitly avoids depending on the layout of
// unix.RawSockaddrInet4/6, which is asserted nowhere. Verified against the
// kernel during development: a udp4 datagram from 127.0.0.1:port arrived as
// [16 2 <port-hi> <port-lo> 127 0 0 1 0 ...], and a udp6 one as
// [28 30 ... 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 1 ...].
func sockaddrToAddr(sa []byte) net.Addr {
	if len(sa) < 2 {
		return nil
	}
	switch sa[1] {
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
// peer's source address changes, so the syscall is not on the hot path.
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
