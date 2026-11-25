// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build unix && !linux

package mesh

import "syscall"

// socketReceiveBufferSize reports the usable receive buffer of a socket,
// in bytes.
//
// ★ ON EVERY UNIX BUT LINUX, THE KERNEL REPORTS THE AMOUNT ITSELF.
//
// The doubling is a LINUX convention: Linux charges the socket's
// bookkeeping overhead against `sk_rcvbuf`, so `getsockopt(SO_RCVBUF)`
// returns twice the reserved amount. BSD and macOS report the reserved
// amount directly.
//
// MEASURED on macOS: asking for 7 MiB read back as 7340032 bytes —
// exactly 7 MiB, with no doubling. Dividing that by two would report
// half the room the socket actually has.
//
// So the halving is Linux-only, and it lives in the Linux file.
func socketReceiveBufferSize(fd uintptr) (int, error) {
	size, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	if err != nil {
		return 0, err
	}
	return size, nil
}
