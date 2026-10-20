// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build linux && (386 || arm || mips || mipsle || ppc)

package p2p2

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// C layout on a 32-bit Linux target:
//
//	struct msghdr {
//	        void         *msg_name;       // 0
//	        socklen_t     msg_namelen;    // 4
//	        struct iovec *msg_iov;        // 8
//	        size_t        msg_iovlen;     // 12
//	        void         *msg_control;    // 16
//	        size_t        msg_controllen; // 20
//	        int           msg_flags;      // 24
//	};                                    // 28
//
//	struct mmsghdr { struct msghdr msg_hdr; unsigned int msg_len; }; // 32
const (
	mmsghdrHdrSize = 28
	mmsghdrSize    = 32
)

// Compile-time layout assertions; see batch_linux_64.go.
var (
	_ [unsafe.Sizeof(unix.Msghdr{}) - mmsghdrHdrSize]byte
	_ [mmsghdrHdrSize - unsafe.Sizeof(unix.Msghdr{})]byte
	_ [unsafe.Sizeof(mmsghdr{}) - mmsghdrSize]byte
	_ [mmsghdrSize - unsafe.Sizeof(mmsghdr{})]byte
)
