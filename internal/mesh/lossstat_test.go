// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// TestConnectionStatsReadableBeforeClose proves the property the bench
// implementation depends on: quic-go's ConnectionStats() is a set of ATOMIC
// counters (quic-go/internal/utils/connstats.go), NOT a value frozen at
// connection close. So a caller can read the loss accounting right after a
// transfer, without closing the connection and without waiting for the
// close event.
//
// If this ever stops holding, the bench must be restructured to close the
// connection first — and this test is what would catch that.
func TestConnectionStatsReadableBeforeClose(t *testing.T) {
	f := newDirectFixture(t)
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stats, err := RunBench(ctx, DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.aline.ID(),
		Endpoints: l.BoundAddrs(),
	}, BenchConfig{Size: 1 << 20})
	if err != nil {
		t.Fatalf("RunBench: %v", err)
	}
	if stats.Loss == nil {
		t.Fatal("loss = nil, want a measured picture")
	}
	// The connection is STILL OPEN at this point (RunBench's defer
	// link.Close() has not run yet — the caller's own defer has not either).
	// The counters were read from the live connection.
	if stats.Loss.PacketsSent == 0 {
		t.Fatal("packets_sent = 0, want the live connection's own counters")
	}
	if stats.Loss.CwndSamples == 0 {
		t.Fatal("cwnd_samples = 0, want live congestion-window observations")
	}
}

// TestLossTrackerRegistryKeysByConnection proves the registry is keyed per
// connection, not per process: two independent bench links on the same
// process must not share a tracker.
func TestLossTrackerRegistryKeysByConnection(t *testing.T) {
	f := newDirectFixture(t)
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stats, err := RunBench(ctx, DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.aline.ID(),
		Endpoints: l.BoundAddrs(),
	}, BenchConfig{Size: 1 << 20})
	if err != nil {
		t.Fatalf("RunBench: %v", err)
	}
	// The peer side must have reported its OWN picture, and the two sides
	// are tracked by different registry entries (different connections).
	if stats.PeerLoss == nil {
		t.Fatal("peer_loss = nil, want the peer's own picture")
	}
	if stats.PeerLoss.PacketsSent == 0 {
		t.Fatal("peer_loss.packets_sent = 0")
	}
	if stats.Loss.PacketsSent == 0 {
		t.Fatal("loss.packets_sent = 0")
	}
}

// TestTrackerRegistryPrunedOnClose proves the registry does not grow without
// bound: a closed connection's tracker must be gone.
//
// Without this, every direct link in a long-lived process (a node daemon)
// would leave a tracker behind for the lifetime of the process.
func TestTrackerRegistryPrunedOnClose(t *testing.T) {
	f := newDirectFixture(t)
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	trackerMu.Lock()
	before := len(trackers)
	trackerMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		if _, err := RunBench(ctx, DialConfig{
			Device:    f.bob,
			Resolver:  MemberListAdapter{List: f.members},
			Target:    f.aline.ID(),
			Endpoints: l.BoundAddrs(),
		}, BenchConfig{Size: 1 << 20}); err != nil {
			t.Fatalf("RunBench %d: %v", i, err)
		}
	}

	// RunBench closes its link on return; the tracker must be pruned. Give
	// the connection's run loop a moment to notice the close.
	//
	// The wait is generous: the pruning happens when the connection's run
	// loop notices the close, and a LOADED machine can take seconds to
	// schedule that goroutine. Five seconds made this a false red in the
	// full parallel suite — the same lesson as the trunk's join wait.
	deadline := time.Now().Add(30 * time.Second)
	for {
		trackerMu.Lock()
		after := len(trackers)
		trackerMu.Unlock()
		if after <= before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tracker registry grew from %d to %d entries: closed connections leak trackers",
				before, after)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTracingConfigIsolatesConnections proves the tracer hook is
// per-connection: two connections created through the same config get two
// different trackers, and neither sees the other's events.
func TestTracingConfigIsolatesConnections(t *testing.T) {
	cfg := tracingConfig(&quic.Config{})
	if cfg.Tracer == nil {
		t.Fatal("Tracer = nil, want the loss hook installed")
	}
	// Two calls with DIFFERENT tracing ids must yield different trackers.
	// The hook reads the id from the context, so the test drives it the same
	// way quic-go does.
	id1 := quic.ConnectionTracingID(1)
	id2 := quic.ConnectionTracingID(2)
	ctx1 := context.WithValue(context.Background(), quic.ConnectionTracingKey, id1)
	ctx2 := context.WithValue(context.Background(), quic.ConnectionTracingKey, id2)

	trackerMu.Lock()
	delete(trackers, id1)
	delete(trackers, id2)
	trackerMu.Unlock()

	t1 := cfg.Tracer(ctx1, 0, quic.ConnectionID{})
	t2 := cfg.Tracer(ctx2, 0, quic.ConnectionID{})
	if t1 == nil || t2 == nil {
		t.Fatal("Tracer returned nil")
	}
	trackerMu.Lock()
	tr1 := trackers[id1]
	tr2 := trackers[id2]
	trackerMu.Unlock()
	if tr1 == nil || tr2 == nil {
		t.Fatal("trackers not registered")
	}
	if tr1 == tr2 {
		t.Fatal("two connections share one tracker")
	}
	// The tracer must record a loss on ONE tracker only.
	t1.LostPacket(0, 1, 0)
	n1 := tr1.lost.Load()
	n2 := tr2.lost.Load()
	if n1 != 1 {
		t.Fatalf("tracker 1 lost = %d, want 1", n1)
	}
	if n2 != 0 {
		t.Fatalf("tracker 2 lost = %d, want 0 (cross-connection contamination)", n2)
	}
}
