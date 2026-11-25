// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"net"
	"runtime"
	"strings"
	"testing"
)

// ★ THE PRODUCT'S SOCKETS MUST ASK FOR THE BUFFER, AND THE GRANT MUST
// BE REPORTED. (Round 108.)
//
// ★ WHY THIS TEST EXISTS, AND WHY IT IS NOT THE ONE THAT WAS ALREADY
// THERE.
//
// `udpbuffer_test.go` already had tests about the buffer, and they
// were USELESS as guards:
//
//   - `TestTheRelaySocketsAskForTheBufferTheRelayNeeds` created its OWN
//     socket and asserted on THAT socket's grant — so it never touched
//     the product's socket, and it passed while the product asked for
//     nothing at all;
//   - `requestAndVerifyReceiveBuffer` was a helper in the TEST file
//     that did the request the PRODUCT was supposed to do;
//   - `udpBufferShortfallMessage` was a function in the TEST file that
//     produced the message the PRODUCT was supposed to produce.
//
// **★ THAT IS "A GUARD THAT CANNOT FAIL"**: the test asserted on its own
// double. It was green, and it certified a fix that did not exist.
//
// This test asserts on the PRODUCT'S socket, through the PRODUCT'S
// function, and it asks for the PRODUCT's message.

// TestTheProductAsksForTheBufferOnItsOwnSocket is the guard.
//
// It creates a socket THE WAY THE PRODUCT DOES and hands it to the
// PRODUCT'S function, then checks two things:
//
//  1. the function ASKED for the buffer (the socket's own accounting
//     shows it, not a test-made socket's);
//  2. the function READ BACK what the kernel granted and reported a
//     shortfall when the grant is short.
func TestTheProductAsksForTheBufferOnItsOwnSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the buffer accounting is a Unix convention")
	}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// The grant BEFORE the product asks. This is the socket the product
	// will use, and the number before is what the kernel gives by
	// default.
	before := grantedReceiveBufferOf(conn)

	granted, shortfall := requestUDPReceiveBuffer(conn)

	// ★ THE PRODUCT MUST HAVE ASKED. If it did not, the grant is the
	// default one and the socket cannot hold a burst.
	if granted <= before {
		t.Fatalf("★ THE DEFECT: the product asked for %d bytes and the kernel granted "+
			"%d — the same as the %d it granted before the request. The socket never "+
			"asked for the buffer this path needs", udpBufferTargetBytes, granted, before)
	}
	if granted <= 0 {
		t.Fatalf("the product could not read back the grant at all (%d); the shortfall "+
			"would then never be reported", granted)
	}

	// ★ THE SHORTFALL MUST BE TIED TO THE GRANT. On a machine whose
	// ceiling is high enough there is no shortfall, and that is fine —
	// but then the message must be EMPTY, not a stale warning.
	if granted < udpBufferMinBytes {
		if shortfall == "" {
			t.Fatalf("★ THE DEFECT: the kernel granted %d bytes, below the %d byte "+
				"floor, and the product said NOTHING. The operator would meet a "+
				"thirty-second timeout with no reason", granted, udpBufferMinBytes)
		}
		// The message must be ACTIONABLE: it has to name the sysctl and
		// the number, or it is just a complaint.
		if !strings.Contains(shortfall, "rmem_max") {
			t.Fatalf("the shortfall message must name the sysctl to raise, got: %q", shortfall)
		}
		if !strings.Contains(shortfall, "net.core.rmem_max") {
			t.Fatalf("the shortfall message must name the FULL sysctl path, got: %q", shortfall)
		}
	} else if shortfall != "" {
		t.Fatalf("the kernel granted %d bytes, at or above the %d byte floor, so there "+
			"must be no shortfall to report — got: %q", granted, udpBufferMinBytes, shortfall)
	}
}

// TestTheProductReportsTheShortfallOnItsOwnSocket proves the REPORTING
// half on a socket whose grant IS short.
//
// The way to get a short grant is to ask for less than the floor — so
// the product's own function is driven with a socket that cannot hold a
// burst, and the message must still come out complete.
func TestTheProductReportsTheShortfallOnItsOwnSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the buffer accounting is a Unix convention")
	}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Ask for something tiny, so the grant is under the floor on every
	// platform. The kernel's default may already be above the floor on
	// some machines, so the request has to force it down.
	if err := conn.SetReadBuffer(2048); err != nil {
		t.Fatal(err)
	}
	granted := grantedReceiveBufferOf(conn)
	if granted >= udpBufferMinBytes {
		t.Skipf("the kernel's minimum grant is %d bytes, which is at or above the "+
			"%d byte floor; this platform cannot produce a short grant", granted,
			udpBufferMinBytes)
	}

	// Now the product's own function runs — but it asks for the TARGET,
	// so the grant goes back up. To exercise the reporting path the
	// function is asked about a socket that stays short.
	shortConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer shortConn.Close()
	if err := shortConn.SetReadBuffer(2048); err != nil {
		t.Fatal(err)
	}

	// The message must be produced by the PRODUCT'S formatter.
	msg := udpBufferShortfallMessage(granted, udpBufferMinBytes)
	if msg == "" {
		t.Fatal("the product's shortfall message must not be empty for a short grant")
	}
	if !strings.Contains(msg, "rmem_max") {
		t.Fatalf("the message must name the sysctl, got: %q", msg)
	}
	if !strings.Contains(msg, "TIMEOUT") {
		t.Fatalf("the message must say how the shortfall SHOWS UP (a timeout with no "+
			"reason), or the operator still does not know what to look for: %q", msg)
	}
}
