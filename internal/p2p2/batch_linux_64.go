// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build linux && (amd64 || arm64 || loong64 || mips64 || mips64le || ppc64 || ppc64le || riscv64 || s390x)

package p2p2

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// C layout on a 64-bit Linux target:
//
//	struct msghdr {
//	        void         *msg_name;       // 0
//	        socklen_t     msg_namelen;    // 8, +4 padding
//	        struct iovec *msg_iov;        // 16
//	        size_t        msg_iovlen;     // 24
//	        void         *msg_control;    // 32
//	        size_t        msg_controllen; // 40
//	        int           msg_flags;      // 48, +4 padding
//	};                                    // 56
//
//	struct mmsghdr { struct msghdr msg_hdr; unsigned int msg_len; }; // 64
const (
	mmsghdrHdrSize = 56
	mmsghdrSize    = 64
)

// Compile-time layout assertions. Each subtraction is a constant expression, so
// a size mismatch makes the array length negative and fails the build.
var (
	_ [unsafe.Sizeof(unix.Msghdr{}) - mmsghdrHdrSize]byte
	_ [mmsghdrHdrSize - unsafe.Sizeof(unix.Msghdr{})]byte
	_ [unsafe.Sizeof(mmsghdr{}) - mmsghdrSize]byte
	_ [mmsghdrSize - unsafe.Sizeof(mmsghdr{})]byte
)
