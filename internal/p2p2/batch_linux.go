// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build linux

package p2p2

import (
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// mmsghdr is struct mmsghdr from <linux/socket.h>:
//
//	struct mmsghdr {
//	        struct msghdr msg_hdr;
//	        unsigned int  msg_len;
//	};
//
// Neither it nor Sendmmsg is declared by golang.org/x/sys/unix v0.39, so both
// are declared here. Go's alignment rules reproduce the C layout on every
// architecture: msghdr is 56 bytes and 8-aligned on 64-bit, so the trailing
// uint32 is padded out to a 64-byte struct; on 32-bit msghdr is 28 bytes and the
// struct is 32. The kernel writes msg_len, which this code never reads back —
// it uses the return value of sendmmsg instead — but the field must be present
// so the array stride matches.
type mmsghdr struct {
	Hdr unix.Msghdr
	Len uint32
}

// The struct's layout is asserted at compile time in batch_linux_64.go and
// batch_linux_32.go: if Go's Msghdr ever stops matching the kernel's struct
// msghdr, every batched send would hand the kernel a garbage pointer and the
// failure would show up as corrupt datagrams or a crash inside the syscall. A
// constant-expression array bound turns that into a build error on the affected
// architecture instead.

// linuxMaxBatch is UIO_MAXIOV, the kernel's per-call cap on sendmmsg. Asking
// for more is not an error, the kernel just sends this many.
const linuxMaxBatch = 1024

// batchingSupported reports whether this platform has a vectored datagram send.
// Tests use it to skip rather than to silently pass on a platform that fell
// back to per-datagram writes.
const batchingSupported = true

// sendmmsg issues one sendmmsg(2).
//
// The uintptr conversion is safe because the caller keeps msgs reachable for
// the duration of the call: it is a field of the writer, which is referenced by
// the closure that performs the call. Go's GC does not move heap objects, so
// the array stays put while the kernel walks it.
func sendmmsg(fd int, msgs []mmsghdr, flags int) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	r0, _, errno := unix.Syscall6(
		unix.SYS_SENDMMSG,
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

// unixBatchWriter sends datagrams with sendmmsg on Linux.
//
// The msghdr and iovec arrays are allocated once, at construction, and reused:
// allocating per call would put an allocation on the hot path that batching is
// meant to remove. The writer is not safe for concurrent use; Conn.writeMu
// serialises it.
type unixBatchWriter struct {
	rc syscall.RawConn

	mmsgs []mmsghdr
	iovs  []unix.Iovec

	sa4     unix.RawSockaddrInet4
	sa6     unix.RawSockaddrInet6
	name    unsafe.Pointer
	namelen uint32

	// The sockaddr is rebuilt only when the destination changes: resolving the
	// family and copying the address is per-batch work, and the destination is
	// constant for a whole message in every case that matters.
	haveAddr bool
	lastAddr string
}

func newBatchWriter(rc syscall.RawConn) batchWriter {
	return &unixBatchWriter{
		rc:    rc,
		mmsgs: make([]mmsghdr, linuxMaxBatch),
		iovs:  make([]unix.Iovec, linuxMaxBatch),
	}
}

func (w *unixBatchWriter) maxBatch() int { return linuxMaxBatch }

// prepare fills the destination sockaddr. Only UDP is supported: the transport
// is UDP by construction, and anything else would mean the Conn was handed a
// socket it cannot reason about.
func (w *unixBatchWriter) prepare(addr net.Addr) error {
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		return errBatchUnsupported
	}
	key := addr.String()
	if w.haveAddr && w.lastAddr == key {
		return nil
	}

	if ip4 := ua.IP.To4(); ip4 != nil {
		w.sa4 = unix.RawSockaddrInet4{Family: unix.AF_INET, Port: htons(ua.Port)}
		copy(w.sa4.Addr[:], ip4)
		w.name = unsafe.Pointer(&w.sa4)
		w.namelen = unix.SizeofSockaddrInet4
	} else {
		ip6 := ua.IP.To16()
		if ip6 == nil {
			return errBatchUnsupported
		}
		w.sa6 = unix.RawSockaddrInet6{Family: unix.AF_INET6, Port: htons(ua.Port)}
		copy(w.sa6.Addr[:], ip6)
		if ua.Zone != "" {
			// A scoped link-local address is only routable with the interface
			// index in Scope_id. Failing to resolve the zone is not fatal for the
			// connection, only for batching: the caller falls back.
			ifi, err := net.InterfaceByName(ua.Zone)
			if err != nil {
				return errBatchUnsupported
			}
			w.sa6.Scope_id = uint32(ifi.Index)
		}
		w.name = unsafe.Pointer(&w.sa6)
		w.namelen = unix.SizeofSockaddrInet6
	}
	w.haveAddr = true
	w.lastAddr = key
	return nil
}

// writeBatch sends msgs in order and returns how many left.
//
// A short return is passed back to the caller, which re-submits the tail: that
// is what keeps ordering intact, because the kernel sends a prefix and the
// remainder is still unsent. EAGAIN is the one case handled here rather than by
// the caller — the raw write parks on the runtime poller until the socket is
// writable and then calls the closure again, so the retry does not re-enter
// Send and does not disturb the sequence order.
func (w *unixBatchWriter) writeBatch(msgs [][]byte, addr net.Addr) (int, error) {
	if len(msgs) > linuxMaxBatch {
		msgs = msgs[:linuxMaxBatch]
	}
	if len(msgs) == 0 {
		return 0, nil
	}
	if err := w.prepare(addr); err != nil {
		return 0, err
	}

	for i, b := range msgs {
		if len(b) == 0 {
			return 0, syscall.EINVAL
		}
		w.iovs[i].Base = &b[0]
		w.iovs[i].SetLen(len(b))
		h := &w.mmsgs[i].Hdr
		h.Name = (*byte)(w.name)
		h.Namelen = w.namelen
		h.Iov = &w.iovs[i]
		h.Iovlen = 1
		h.Control = nil
		h.Controllen = 0
		h.Flags = 0
		w.mmsgs[i].Len = 0
	}

	total := 0
	var werr error
	err := w.rc.Write(func(fd uintptr) bool {
		for total < len(msgs) {
			n, e := sendmmsg(int(fd), w.mmsgs[total:len(msgs)], 0)
			if e != nil {
				if e == unix.EAGAIN || e == unix.EWOULDBLOCK {
					// The socket buffer is full. Returning false asks the runtime
					// to wait for writability and call this closure again with the
					// same fd; `total` carries the progress across the wait.
					return false
				}
				werr = e
				return true
			}
			if n <= 0 {
				werr = errBatchStalled
				return true
			}
			total += n
		}
		return true
	})
	if err != nil {
		return total, err
	}
	if werr != nil {
		return total, werr
	}
	return total, nil
}
