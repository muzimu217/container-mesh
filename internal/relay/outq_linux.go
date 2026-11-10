// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build linux

package relay

import (
	"fmt"
	"net"
	"syscall"
)

// siocoutq is SIOCOUTQ (0x5411): bytes not yet transmitted in the socket
// send queue. Defined locally — the constant is missing from some syscall
// packages/architecture combos Go ships.
const siocoutq = 0x5411

// socketOutq returns the bytes queued for transmission but not yet
// acknowledged by the peer (SIOCOUTQ) — the kernel-level view of "how far
// behind is this consumer". This is where the issue #8 backlog lived
// (1.5–3.7 MB in sndbuf): the application-layer write had already returned,
// so write-duration based kicks could never see it.
func socketOutq(c net.Conn) (int, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return 0, fmt.Errorf("relay: not a TCP conn")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var outq int
	var ctlErr error
	err = raw.Control(func(fd uintptr) {
		n, e := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, siocoutq)
		if e != nil {
			ctlErr = e
			return
		}
		outq = n
	})
	if err != nil {
		return 0, err
	}
	return outq, ctlErr
}
