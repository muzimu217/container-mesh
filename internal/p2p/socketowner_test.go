// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p

import (
	"net"
	"testing"
	"time"
)

// ★ THE PUNCHED SOCKET MUST HAVE EXACTLY ONE READER. (Round 107.)
//
// The architecture map line found this defect by reading the code: the
// CLI's QUIC path runs quic-go on the socket that the hole punch
// opened, and `Negotiate` ALSO built this package's `Conn` on that same
// socket. `Conn`'s constructor starts a `readLoop` goroutine that reads
// it. quic-go's Transport.listen reads it too.
//
// Two readers on one socket split the datagrams at random. The QUIC
// handshake packets land in the built-in reader, which drops anything
// whose connection ID is not its own ("stray traffic on the port"). So
// the direct path NEVER succeeds.
//
// ★ AND THE SYMPTOM HIDES IT: the session falls back to the relay, so
// the user sees a working session. The failure is indistinguishable
// from "this network has no direct path" — which is exactly why it
// survived.
//
// This test pins the decision that prevents it.

// TestThePunchedSocketGetsExactlyOneReader covers the decision point.
func TestThePunchedSocketGetsExactlyOneReader(t *testing.T) {
	t.Run("the-legacy-transport-wants-the-socket", func(t *testing.T) {
		opts := DefaultOptions()
		if opts.SkipBuiltInTransport {
			t.Fatal("the default must still build the built-in transport: " +
				"--p2p-legacy depends on it")
		}
		if !builtInTransportWanted(opts) {
			t.Fatal("the built-in transport must be built when nobody opted out")
		}
	})

	t.Run("a-different-transport-must-not-also-read-it", func(t *testing.T) {
		opts := DefaultOptions()
		opts.SkipBuiltInTransport = true
		if builtInTransportWanted(opts) {
			t.Fatal("★ THE DEFECT: the built-in transport's reader was started on a " +
				"socket that another transport (QUIC) is about to read. Two readers " +
				"split the datagrams, the QUIC handshake packets are dropped as " +
				"'stray traffic', and the direct path never works")
		}
	})
}

// TestTheBuiltInTransportReallyReadsTheSocket is the observable
// consequence: building the Conn starts a reader on that socket.
//
// It is the other half of the same contract. If NewConn ever stopped
// reading the socket, then "two readers" would no longer be a problem —
// but the legacy transport would stop working, which is a different
// defect. So both halves are pinned.
func TestTheBuiltInTransportReallyReadsTheSocket(t *testing.T) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()

	remote := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	c := NewConn(udp, remote, 0xDEADBEEF, DefaultConfig())
	defer c.Close()

	// The reader is a goroutine, so give it a moment to park on the
	// socket. If it did NOT start, the datagram below would still be
	// sitting in the socket's buffer afterwards.
	time.Sleep(100 * time.Millisecond)

	peer, err := net.DialUDP("udp4", nil, udp.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	if _, err := peer.Write([]byte("a datagram for the built-in reader")); err != nil {
		t.Fatal(err)
	}

	// The built-in reader consumes the datagram and DECODES it. A
	// datagram that is not a valid frame is dropped ("stray traffic"),
	// so the observable effect is that the socket's buffer is empty
	// afterwards — the reader took it.
	buf := make([]byte, 64)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_ = udp.SetReadDeadline(deadline)
		n, err := udp.Read(buf)
		if err != nil {
			break
		}
		if n > 0 {
			t.Fatal("★ the datagram was still in the socket's buffer, so NOTHING " +
				"was reading it: the built-in transport did not take its reader")
		}
	}
}
