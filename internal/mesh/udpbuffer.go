// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"fmt"
	"net"
)

// ★ THE RELAY SOCKETS MUST ASK FOR THE BUFFER THEY NEED — AND SAY WHEN
// THEY DID NOT GET IT. (Round 108.)
//
// ★ THE DEFECT THIS COMES FROM.
//
// A relayed file transfer on the real machine failed at every size from
// 64 KiB up, with a TIMEOUT and no reason:
//
//	mesh: read verdict: mesh: read the frame header: deadline exceeded
//
// The relay's data path was EXCLUDED as the cause: injecting the
// MEASURED path profile (burst queue overflow, 175–321 datagrams lost
// per leg) into the local relay still transferred every size with a
// byte-identical sha256. So the splice, the record framing and the
// windows are not at fault.
//
// What IS at fault is the socket's receive buffer:
//
//  1. the product NEVER asks for one — quic-go asks the kernel for 7 MiB
//     (protocol.DesiredReceiveBufferSize), and the kernel caps it at
//     net.core.rmem_max, SILENTLY. On the real machine rmem_max was
//     229376, so the socket got a fraction of what the path needs;
//  2. the relay's forward loop PULLS records on demand (one at a time),
//     so the socket buffer has to hold a BURST — several records — not
//     one;
//  3. a path that delivers traffic in bursts (the real machine's
//     hairpin + cloud NAT queue) therefore DROPS the datagrams the
//     socket cannot hold — and the drop shows up as a 30-second
//     timeout, with a message that names neither the buffer nor the
//     loss.
//
// The fix is in two parts, and BOTH matter:
//
//   - ASK for the buffer (the request is a request; the kernel has the
//     last word);
//   - READ BACK what the kernel actually granted, and NAME THE
//     SHORTFALL in the failure message, so an operator whose
//     rmem_max is too low is told exactly that instead of being sent to
//     debug a timeout.
//
// ★ WHY THE SHORTFALL MUST BE VISIBLE. (A lesson this project has paid
// twice.) A ceiling that silently truncates a request is exactly the
// shape of "the guard is in the wrong place" and "the check cannot
// fail": quic-go DOES log that it could not get the buffer it wanted,
// but that line goes to a logger the operator does not read, and the
// failure the operator sees names neither the buffer nor the loss.
//
// ★ THE TWO NUMBERS ARE THE CONTRACT.
//
// udpBufferTargetBytes is what to ASK for; udpBufferMinBytes is the
// floor below which a burst cannot fit. They live here (in the
// production file) because the PRODUCT uses them — the test file
// asserts against them, but it is the product that asks and that
// reports the shortfall.
const (
	// udpBufferTargetBytes is what the product asks the kernel for.
	//
	// 7 MiB is quic-go's own DesiredReceiveBufferSize, and the product
	// should not ask for LESS than the library does: a socket that gets
	// less than quic-go wants is a socket whose buffer the library's own
	// logging complains about.
	udpBufferTargetBytes = 7 << 20

	// udpBufferMinBytes is the floor the product considers usable.
	//
	// Below this the socket cannot hold a burst of relay traffic, and
	// the transfer fails with a timeout instead of with a reason.
	//
	// Four MiB, because a relay record is about 1.4 MB on the wire (1 MiB
	// of plaintext in a base64 JSON envelope) and the buffer has to hold
	// THREE of them: the relay's forwarding loop pulls records ON
	// DEMAND, so while it is pulling one record the datagrams of the
	// next ones pile up in the socket.
	udpBufferMinBytes = 4 << 20
)

// requestUDPReceiveBuffer asks the kernel for the receive buffer the
// relay path needs, and reports what the kernel ACTUALLY granted.
//
// ★ THE GRANTED VALUE IS THE ONLY ONE THAT MATTERS. The request is a
// request: the kernel caps it at net.core.rmem_max and truncates
// SILENTLY. So the number that decides whether the path works is the
// one read back from the socket, not the one requested.
//
// ★ THE SHORTFALL IS A WARNING, NOT A REFUSAL. (Round 108.) The first
// version REFUSED TO START when the grant was below the floor — and on
// macOS the kernel grants about 3.5 MiB against a 4 MiB floor, so that
// would have made the product UNSTARTABLE on a whole platform.
//
// A floor that a mainstream platform cannot meet is not a floor; it is
// a wrong number. So the shortfall is REPORTED and the node starts —
// because a smaller buffer is a degraded path, not a broken one, and
// refusing to start would trade a slow transfer for no service at all.
//
// The shortfall text is what the operator needs: the granted number,
// the floor, the sysctl to raise, and the value to raise it to.
//
// Returns the granted size and a human-readable shortfall message ("" when
// the grant is adequate).
func requestUDPReceiveBuffer(conn *net.UDPConn) (granted int, shortfall string) {
	if conn == nil {
		return 0, ""
	}
	// A failed request is not fatal: the socket still works, it just
	// works with a smaller buffer. So the error is deliberately not
	// returned — the GRANT is what the caller needs, and the shortfall
	// message says what to do about it.
	_ = conn.SetReadBuffer(udpBufferTargetBytes)
	_ = conn.SetWriteBuffer(udpBufferTargetBytes)

	granted = grantedReceiveBufferOf(conn)
	if granted >= udpBufferMinBytes {
		return granted, ""
	}
	return granted, udpBufferShortfallMessage(granted, udpBufferMinBytes)
}

// udpBufferShortfallMessage is the text the operator reads when the
// kernel granted less than the path needs.
//
// ★ IT NAMES FOUR THINGS, AND EACH ONE IS LOAD-BEARING:
//
//  1. THE GRANTED NUMBER (in bytes AND in KiB) and the FLOOR — so the
//     reader can see how far short the machine is, in the unit they
//     would compare against a sysctl value in;
//  2. HOW IT SHOWS UP ("a TIMEOUT with no reason") — because that is
//     the symptom the operator will actually meet, and without it they
//     have no way to connect this note to the transfer that failed;
//  3. THE SYSCTL AND THE VALUE — so the remedy is copy-pasteable.
//
// ★ IT IS IN THE PRODUCTION FILE, NOT THE TEST FILE. (Round 108.) The
// first version of this text lived in udpbuffer_test.go, where it was
// a helper the TEST called — so the product never produced it, and the
// test that "pinned" it was pinning its own double.
func udpBufferShortfallMessage(granted, floor int) string {
	return fmt.Sprintf(
		"the UDP socket's receive buffer is %d bytes (%d KiB), below the %d byte floor "+
			"this path wants: a path that delivers traffic in bursts makes the kernel "+
			"drop the datagrams the socket cannot hold, and a relayed transfer then "+
			"fails as a TIMEOUT with no reason. Raise it with: "+
			"sysctl -w net.core.rmem_max=%d net.core.wmem_max=%d "+
			"and restart the node — the socket cannot ask for more than the kernel's ceiling",
		granted, granted>>10, floor, udpBufferTargetBytes, udpBufferTargetBytes)
}

// grantedReceiveBufferOf reports the receive buffer the kernel granted
// a socket, in bytes.
//
// ★ IT ASKS THE SOCKET, NOT THE SYSCTL. The sysctl is the CEILING; the
// kernel grants at most twice it, and a container's network namespace
// may have its own. So the ceiling cannot be read as the answer.
func grantedReceiveBufferOf(conn *net.UDPConn) int {
	if conn == nil {
		return 0
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	var size int
	var serr error
	if err := raw.Control(func(fd uintptr) {
		size, serr = socketReceiveBufferSize(fd)
	}); err != nil {
		return 0
	}
	if serr != nil {
		return 0
	}
	return size
}
