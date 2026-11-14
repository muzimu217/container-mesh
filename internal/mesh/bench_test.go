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
	if stats.Retransmissions != nil {
		t.Fatalf("retransmissions = %v, want unavailable", *stats.Retransmissions)
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
