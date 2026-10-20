// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build darwin

package p2p2

import (
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// msghdr_x is Darwin's extended message header, from XNU
// bsd/sys/socket_private.h. It is the element type of sendmsg_x(2):
//
//	struct msghdr_x {
//	        void         *msg_name;
//	        socklen_t     msg_namelen;
//	        struct iovec *msg_iov;
//	        int           msg_iovlen;
//	        void         *msg_control;
//	        socklen_t     msg_controllen;
//	        int           msg_flags;
//	        size_t        msg_datalen;
//	};
//
// Darwin pads each field to its own natural alignment in this structure (the
// same layout as struct msghdr plus the trailing size_t), so the two explicit
// padding fields below are required: without them msg_iov would land at offset
// 12 instead of 16 and the kernel would read a truncated pointer. The structure
// is 56 bytes on arm64 and amd64.
//
// sendmsg_x is not declared by golang.org/x/sys/unix, so the syscall number
// comes from unix.SYS_SENDMSG_X and the wrapper is written here. It is the
// documented Darwin equivalent of Linux sendmmsg: it takes an array, sends the
// elements in order, and returns how many it sent.
type msghdrX struct {
	name       *byte
	namelen    uint32
	_          uint32
	iov        *unix.Iovec
	iovlen     int32
	_          uint32
	control    *byte
	controllen uint32
	flags      int32
	datalen    uintptr
}

// darwinMaxBatch is how many datagrams one sendmsg_x call carries.
//
// The kernel does not publish a limit as low as Linux's UIO_MAXIOV; 1024 is the
// batch the send path can actually produce (Config.SendWindow defaults to 1024,
// and one Send never exceeds a message's fragment count), so a larger array
// would be allocation without use. Measured: sendmsg_x accepted and delivered
// 2048 in one call, so this is well inside what the kernel takes.
const darwinMaxBatch = 1024

// batchingSupported reports whether this platform has a vectored datagram send.
// Tests use it to skip rather than to silently pass on a platform that fell
// back to per-datagram writes.
const batchingSupported = true

// sendmsgX issues one sendmsg_x(2).
func sendmsgX(fd int, msgs []msghdrX) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	r0, _, errno := unix.Syscall6(
		unix.SYS_SENDMSG_X,
		uintptr(fd),
		uintptr(unsafe.Pointer(&msgs[0])),
		uintptr(len(msgs)),
		0, 0, 0,
	)
	if errno != 0 {
		return 0, errno
	}
	return int(r0), nil
}

// darwinBatchWriter sends datagrams with sendmsg_x.
//
// Arrays are allocated once and reused, like the Linux writer. Not safe for
// concurrent use; Conn.writeMu serialises it.
type darwinBatchWriter struct {
	rc syscall.RawConn

	msgs []msghdrX
	iovs []unix.Iovec

	sa4     unix.RawSockaddrInet4
	sa6     unix.RawSockaddrInet6
	name    unsafe.Pointer
	namelen uint32

	haveAddr bool
	lastAddr string
}

func newBatchWriter(rc syscall.RawConn) batchWriter {
	return &darwinBatchWriter{
		rc:   rc,
		msgs: make([]msghdrX, darwinMaxBatch),
		iovs: make([]unix.Iovec, darwinMaxBatch),
	}
}

func (w *darwinBatchWriter) maxBatch() int { return darwinMaxBatch }

func (w *darwinBatchWriter) prepare(addr net.Addr) error {
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		return errBatchUnsupported
	}
	key := addr.String()
	if w.haveAddr && w.lastAddr == key {
		return nil
	}

	if ip4 := ua.IP.To4(); ip4 != nil {
		w.sa4 = unix.RawSockaddrInet4{Len: unix.SizeofSockaddrInet4, Family: unix.AF_INET, Port: htons(ua.Port)}
		copy(w.sa4.Addr[:], ip4)
		w.name = unsafe.Pointer(&w.sa4)
		w.namelen = unix.SizeofSockaddrInet4
	} else {
		ip6 := ua.IP.To16()
		if ip6 == nil {
			return errBatchUnsupported
		}
		w.sa6 = unix.RawSockaddrInet6{Len: unix.SizeofSockaddrInet6, Family: unix.AF_INET6, Port: htons(ua.Port)}
		copy(w.sa6.Addr[:], ip6)
		if ua.Zone != "" {
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
// A short return is passed to the caller, which re-submits the tail: sendmsg_x
// sends a prefix, so the remainder is still unsent and ordering holds.
// EWOULDBLOCK is handled here by the raw-write poller: the closure returns
// false, the runtime waits for writability, and the closure is entered again
// with `total` carrying progress across the wait.
func (w *darwinBatchWriter) writeBatch(msgs [][]byte, addr net.Addr) (int, error) {
	if len(msgs) > darwinMaxBatch {
		msgs = msgs[:darwinMaxBatch]
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
		m := &w.msgs[i]
		m.name = (*byte)(w.name)
		m.namelen = w.namelen
		m.iov = &w.iovs[i]
		m.iovlen = 1
		m.control = nil
		m.controllen = 0
		m.flags = 0
		m.datalen = 0
	}

	total := 0
	var werr error
	err := w.rc.Write(func(fd uintptr) bool {
		for total < len(msgs) {
			n, e := sendmsgX(int(fd), w.msgs[total:len(msgs)])
			if e != nil {
				if e == unix.EAGAIN || e == unix.EWOULDBLOCK {
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
