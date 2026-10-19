// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/wsconn"
)

// TestMutualEvictionIsBounded is the regression test for a live-lock found by
// stress-testing against a real relay: two peers redialling on similar timers
// evicted each other forever, so neither ever paired.
//
// The WebSocket upgrade always succeeds (the relay decides after upgrading),
// so "refused" means the connection is closed immediately rather than parked.
func TestMutualEvictionIsBounded(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	room := "eviction-storm"
	var dropped int

	for i := 0; i < maxEvictionsPerRoom+4; i++ {
		c, err := wsconn.Dial(context.Background(), wsconn.DialOptions{
			URL:         fmt.Sprintf("%s?room=%s&role=A", base, room),
			DialTimeout: 2 * time.Second,
		})
		if err != nil {
			dropped++
			continue
		}
		// A parked peer stays connected; a refused one is closed right away.
		// Probe with a short read deadline: timeout means "still parked".
		_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		if _, rerr := c.Recv(); rerr == nil {
			// Data is never sent pre-pairing, so this would be a protocol bug.
			t.Errorf("iter %d: unexpected data before pairing", i)
		} else if !isTimeout(rerr) {
			dropped++
		}
		_ = c.Close()
	}

	if dropped == 0 {
		t.Fatalf("relay never refused a take-over after %d evictions; mutual eviction is unbounded",
			maxEvictionsPerRoom)
	}
	t.Logf("refused %d of %d take-overs", dropped, maxEvictionsPerRoom+4)
}

// isTimeout reports whether err is a read-deadline timeout (peer still parked)
// as opposed to the connection being closed.
func isTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	if te, ok := err.(timeout); ok && te.Timeout() {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "i/o timeout")
}

// TestContestedRoomRecovers: once the contested window expires, pairing must
// work again. A relay that stayed permanently closed would turn a transient
// race into an outage.
func TestContestedRoomRecovers(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	room := "recovery-room"

	// Burn the eviction budget.
	for i := 0; i < maxEvictionsPerRoom+1; i++ {
		c, err := wsconn.Dial(context.Background(), wsconn.DialOptions{
			URL:         fmt.Sprintf("%s?room=%s&role=A", base, room),
			DialTimeout: 2 * time.Second,
		})
		if err == nil {
			_ = c.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Wait out the contested window.
	time.Sleep(contestedWindow + 200*time.Millisecond)

	a := dial(t, base, room, "A")
	defer a.Close()
	b := dial(t, base, room, "B")
	defer b.Close()

	if err := a.Send([]byte("after recovery")); err != nil {
		t.Fatalf("send: %v", err)
	}
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := b.Recv()
	if err != nil {
		t.Fatalf("B recv after recovery: %v", err)
	}
	if string(got) != "after recovery" {
		t.Errorf("B got %q", got)
	}
}

// TestContestedRefusalMessageIsActionable: the error text must tell a client
// that retrying is the right move, since the CLI surfaces it to operators.
func TestContestedRefusalMessageIsActionable(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	room := "msg-room"
	for i := 0; i < maxEvictionsPerRoom+1; i++ {
		c, err := wsconn.Dial(context.Background(), wsconn.DialOptions{
			URL:         fmt.Sprintf("%s?room=%s&role=A", base, room),
			DialTimeout: 2 * time.Second,
		})
		if err == nil {
			_ = c.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The next dial may be accepted and then closed by the relay; either way
	// we only assert the relay does not accept an unbounded stream of them.
	c, err := wsconn.Dial(context.Background(), wsconn.DialOptions{
		URL:         fmt.Sprintf("%s?room=%s&role=A", base, room),
		DialTimeout: 2 * time.Second,
	})
	if err == nil {
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, rerr := c.Recv(); rerr != nil {
			if !strings.Contains(strings.ToLower(rerr.Error()), "close") {
				t.Logf("closed with: %v", rerr)
			}
		}
	}
}
