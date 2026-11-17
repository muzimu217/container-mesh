// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"testing"
	"time"
)

func TestRunBenchBidirectionalDirectLink(t *testing.T) {
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
	}, BenchConfig{Size: 3 << 20})
	if err != nil {
		t.Fatalf("RunBench: %v", err)
	}
	if stats.SentBytes != 3<<20 || stats.ReceivedBytes != 3<<20 {
		t.Fatalf("stats = %+v, want 3 MiB in both directions", stats)
	}
	if stats.Duration <= 0 {
		t.Fatalf("duration = %s, want positive", stats.Duration)
	}
	// The loss accounting must be REAL, not nil: quic-go exposes per
	// connection loss counters, and a bench session is one connection with
	// one stream.
	if stats.PacketsLost == nil {
		t.Fatal("packets_lost = nil, want the measured connection loss count")
	}
	if stats.Loss == nil {
		t.Fatal("loss = nil, want the measured connection loss picture")
	}
	if stats.Loss.PacketsLost != *stats.PacketsLost {
		t.Fatalf("packets_lost = %d, loss.PacketsLost = %d, want equal",
			*stats.PacketsLost, stats.Loss.PacketsLost)
	}
	// The tracer path and the ConnectionStats path are independent, so they
	// must agree, except for the two accounted differences: Path-MTU probe
	// losses (tracer only) and ECN congestion events (stats only).
	if stats.Loss.TracerLost+stats.Loss.ECNCongested < stats.Loss.PacketsLost {
		t.Fatalf("tracer_lost = %d, packets = %d, ecn = %d: tracer under-counts",
			stats.Loss.TracerLost, stats.Loss.PacketsLost, stats.Loss.ECNCongested)
	}
	if stats.Loss.TracerLost < stats.Loss.PacketsLost {
		t.Fatalf("tracer_lost = %d, packets = %d: tracer must not under-count",
			stats.Loss.TracerLost, stats.Loss.PacketsLost)
	}
	if stats.Loss.CwndSamples == 0 {
		t.Fatal("cwnd_samples = 0, want congestion-window observations")
	}
	// The peer must report its own loss picture back over the bench stats
	// frame, so one run shows BOTH directions.
	if stats.PeerLoss == nil {
		t.Fatal("peer_loss = nil, want the peer's own loss picture")
	}
	if stats.PeerLoss.PacketsSent == 0 {
		t.Fatal("peer_loss.packets_sent = 0")
	}
}

func TestValidateBenchConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  BenchConfig
		want bool
	}{
		{"zero size", BenchConfig{}, false},
		{"negative rate", BenchConfig{Size: 1, RateBytesPerSecond: -1}, false},
		{"valid", BenchConfig{Size: 1}, true},
		{"too large", BenchConfig{Size: benchMaxSize + 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateBenchConfig(tc.cfg) == nil; got != tc.want {
				t.Fatalf("validateBenchConfig(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}
