// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/quic-go/quic-go"
)

// TestDialReleasesTheLocalSocket pins that a dial attempt releases its own
// UDP socket deterministically.
//
// dialOneRaw opens one UDP socket per endpoint attempt and hands it to
// quic.Dial. quic-go does not take ownership of a caller-supplied packet
// connection: setupTransport records createdConn=false for that call shape
// (quic-go client.go), and Transport.Close only closes the packet
// connection when createdConn is true.
//
// The socket therefore has to be released by this package. It used to be
// released only on the quic.Dial error path, so every other path — a
// successful dial, a peer that declines, a hello that is never answered —
// left the socket to the garbage collector. Measured on a leaked
// net.UDPConn: the descriptor stays open until a GC cycle runs its
// finalizer, so the release is real but not bounded, and a dial loop can
// hold hundreds of descriptors between collections.
//
// The test counts the open file descriptors, so it fails if an attempt
// leaves one behind.
func TestDialReleasesTheLocalSocket(t *testing.T) {
	f := newDirectFixture(t)
	l := startListener(t, f)

	eps := l.BoundAddrs()
	if len(eps) == 0 {
		t.Fatal("the listener reported no addresses")
	}

	before := openFDs(t)

	// A successful dial, then close the link. The socket must be gone.
	link, err := DialDirect(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        eps,
		Kind:             KindPing,
		HandshakeTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = link.Close()

	after := waitForFDsAtMost(t, before)

	if after > before {
		t.Fatalf("the dial left a socket open: %d descriptors before, %d after", before, after)
	}
}

// TestDialReleasesTheSocketWhenTheHelloIsNeverAnswered pins the same
// property for an attempt that fails after the QUIC handshake completed.
//
// A peer that accepts the connection and then closes the stream without
// answering the hello is reachable in practice (an old build, a crash
// mid-handshake), and it is the error path that used to return without
// closing either the QUIC connection or the socket.
func TestDialReleasesTheSocketWhenTheHelloIsNeverAnswered(t *testing.T) {
	f := newDirectFixture(t)
	addr := startSilentServer(t, f)

	before := openFDs(t)

	_, err := DialDirect(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        []string{addr},
		Kind:             KindPing,
		HandshakeTimeout: 10 * time.Second,
	})
	if err == nil {
		t.Fatal("a dial against a silent server was expected to fail")
	}

	after := waitForFDsAtMost(t, before)

	if after > before {
		t.Fatalf("the failed dial left a socket open: %d descriptors before, %d after", before, after)
	}
}

// startSilentServer accepts a QUIC connection, reads the hello, and closes
// the stream without ever answering — the shape that used to leave the
// dialer's QUIC connection and its UDP socket behind.
func startSilentServer(t *testing.T, f *directFixture) string {
	t.Helper()

	cert, err := f.aline.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	pin := func(id identity.DeviceID, pub ed25519.PublicKey) error {
		want, err := MemberListAdapter{List: f.members}.KeyFor(id)
		if err != nil {
			return err
		}
		if !ed25519KeysEqual(pub, want) {
			return fmt.Errorf("mesh: %s key mismatch", id)
		}
		return nil
	}
	tlsCfg, err := deviceTLS(&cert, pin)
	if err != nil {
		t.Fatal(err)
	}

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := quic.Listen(udp, tlsCfg, quicConfig())
	if err != nil {
		_ = udp.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				st, err := conn.AcceptStream(context.Background())
				if err != nil {
					return
				}
				// Read the hello and drop the stream: no answer.
				_, _ = bufio.NewReader(st).ReadString('\n')
				_ = st.Close()
			}()
		}
	}()

	return ln.Addr().String()
}

// waitForFDsAtMost waits for the descriptor count to fall back to at most
// want WITHOUT forcing a garbage collection.
//
// Forcing a GC would make the test useless: a leaked socket is reclaimed by
// the runtime's finalizer at the next collection, so a GC-driven wait turns
// "released deterministically" into "released eventually" and the test can
// never go red. Measured on an abandoned net.UDPConn: the descriptor stays
// open until a collection runs. The short unforced wait absorbs nothing but
// the asynchronous part of a clean release.
func waitForFDsAtMost(t *testing.T, want int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	got := openFDs(t)
	for got > want && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		got = openFDs(t)
	}
	return got
}

// openFDs reports how many file descriptors the process holds.
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("cannot count the open descriptors on this platform: %v", err)
	}
	return len(entries)
}
