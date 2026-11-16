// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestDialDirectFallsBackToSecondEndpoint pins the multi-endpoint fallback.
//
// A member announces more than one endpoint (a private address and a
// NAT-mapped one, say). DialDirect tries them in order, and the first one
// may well be the unreachable one.
//
// The bug this guards against: dialOne closed the shared UDP socket when an
// attempt failed, so every later endpoint in the loop was dialled on a
// closed socket and failed with "use of closed network connection". The
// fallback could therefore never reach a working endpoint.
//
// quic-go makes this unavoidable to get right: quic.Dial wraps the
// net.PacketConn in a single-use Transport, which closes the connection
// when the QUIC session ends (quic-go transport.go, isSingleUse). One
// socket serves exactly one dial attempt.
func TestDialDirectFallsBackToSecondEndpoint(t *testing.T) {
	f := newDirectFixture(t)
	l := startListener(t, f)

	// A port nobody listens on: the first attempt fails at the handshake.
	dead := deadEndpoint(t)

	eps := []string{dead, l.BoundAddrs()[0]}
	link, err := DialDirect(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        eps,
		Kind:             KindPing,
		HandshakeTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("DialDirect did not reach the working endpoint: %v", err)
	}
	t.Cleanup(func() { _ = link.Close() })

	// The link must be usable, not just established.
	rtts, err := PingExchange(context.Background(), link, 5, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("ping over the fallback link: %v", err)
	}
	if len(rtts) != 5 {
		t.Fatalf("got %d RTTs, want 5", len(rtts))
	}
}

// deadEndpoint returns a host:port that is bound but never answers, so the
// dial fails at the handshake rather than at the address resolution.
func deadEndpoint(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// The TCP port is closed now; the UDP port of the same number has nothing
	// behind it either.
	return "127.0.0.1:" + port
}
