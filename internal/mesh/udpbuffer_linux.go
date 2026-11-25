// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build linux

package mesh

import "syscall"

// socketReceiveBufferSize reports the USABLE receive buffer of a socket,
// in bytes.
//
// ★ ON LINUX THE KERNEL REPORTS TWICE WHAT IT RESERVED.
//
// Linux's `sk_rcvbuf` accounting charges the bookkeeping overhead
// against the value, so `getsockopt(SO_RCVBUF)` returns DOUBLE the
// amount actually reserved for packets. That is why the ceiling works
// out as it does: `net.core.rmem_max` is the reserved amount, and the
// read-back is `2 × rmem_max`.
//
// MEASURED on the authorized host (the A/B that proved the root
// cause): with `net.core.rmem_max=229376` (224 KiB) the read-back was
// 458752 (448 KiB) — exactly twice — and with the ceiling raised to
// 16 MiB the read-back was 14336 KiB (14 MiB = 2 × the 7 MiB asked
// for). quic-go applies the same halving, and its own log line
// reported "was: 224 kiB, wanted: 7168 kiB, got: 448 kiB" — the same
// numbers.
//
// So the USABLE amount is half of what the kernel reports, and this
// function returns that. Reading the raw value would overstate the
// buffer by exactly 2×, and the product would believe it had twice the
// room it actually has.
func socketReceiveBufferSize(fd uintptr) (int, error) {
	size, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	if err != nil {
		return 0, err
	}
	return size / 2, nil
}
