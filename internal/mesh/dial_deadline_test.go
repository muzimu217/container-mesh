// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestDialDirectHonoursTheCallersDeadline pins that a caller's own budget
// bounds the dial.
//
// The endpoint loop tries every endpoint a member announced, and each
// attempt costs a handshake timeout. The per-attempt context used to be
// built from context.Background(), so the caller's deadline had no effect
// on the loop at all: the dial took as long as the number of endpoints
// times the per-attempt timeout, whatever the caller asked for.
//
// The product's own probe was measured running 5.12 s with a three second
// budget, because of this.
func TestDialDirectHonoursTheCallersDeadline(t *testing.T) {
	f := newDirectFixture(t)
	l := startListener(t, f)

	// A list of endpoints that all go nowhere, so every attempt has to time
	// out. The dead endpoints are unbound UDP ports.
	eps := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		eps = append(eps, deadEndpoint(t))
	}
	eps = append(eps, l.BoundAddrs()[0])

	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := DialDirect(ctx, DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.aline.ID(),
		Endpoints: eps,
		Kind:      KindPing,
		// The per-attempt timeout is deliberately long, so only the
		// caller's deadline can end the loop.
		HandshakeTimeout: 30 * time.Second,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the dial succeeded although every endpoint before the live one is dead")
	}
	// The caller's deadline must win. Without the fix, eight dead
	// endpoints at thirty seconds each would run far past it.
	if elapsed > 5*time.Second {
		t.Fatalf("the dial ran for %s, want it bounded by the caller's 900ms "+
			"deadline: the endpoint loop ignored the caller's context",
			elapsed.Round(time.Millisecond))
	}
}

var _ = net.IPv4zero
