// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build linux

package relay

import (
	"crypto/tls"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// tiocoutq is TIOCOUTQ (0x5411): an **ioctl** command that returns the bytes
// queued in the socket send queue but not yet acknowledged.
//
// Load-bearing correctness note (netns-probe round 7, kernel 5.10): the same
// numeric value passed to getsockopt returns ENOPROTOOPT — it is an ioctl
// request, not a socket option. The first implementation made that mistake,
// so the monitor died on its first tick and never kicked anything.
const tiocoutq = 0x5411

// socketOutq returns the bytes queued for transmission but not yet
// acknowledged by the peer — the kernel-level view of "how far behind is
// this consumer". This is where the issue #8 backlog lived (1.5–4.1 MB in
// sndbuf): the application-layer write had already returned, so
// write-duration based kicks could never see it.
//
// A *tls.Conn is unwrapped via NetConn: under --tls-* the members stream is
// TLS over TCP, and the type assert alone silently disabled the queue-depth
// kick for every TLS deployment (netns-probe round 31, defect F3).
func socketOutq(c net.Conn) (int, error) {
	for depth := 0; depth < 4; depth++ {
		switch t := c.(type) {
		case *net.TCPConn:
			return tcpOutq(t)
		case *tls.Conn:
			c = t.NetConn()
			continue
		default:
			return 0, fmt.Errorf("relay: unsupported conn type %T", c)
		}
	}
	return 0, fmt.Errorf("relay: conn nesting too deep for queue introspection")
}

func tcpOutq(tc *net.TCPConn) (int, error) {
	raw, err := tc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var outq int32
	var ctlErr error
	err = raw.Control(func(fd uintptr) {
		// ioctl(fd, TIOCOUTQ, &outq) — argument must be a pointer to int.
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, tiocoutq,
			uintptr(unsafe.Pointer(&outq)))
		if errno != 0 {
			ctlErr = errno
		}
	})
	if err != nil {
		return 0, err
	}
	if ctlErr != nil {
		return 0, ctlErr
	}
	return int(outq), nil
}
