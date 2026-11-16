// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// BenchmarkRunBenchDirect measures the full mesh bench path over loopback:
// request frame, both directions moving Size bytes, then the end and stats
// frames. It reports total wire bytes per second across both directions.
//
// This is an observation tool, not a gate. The throughput target lives in
// docs/REQUIREMENTS.md (P-3: direct large-file throughput >= 200 MiB/s) and
// the authoritative numbers come from the performance research line.
func BenchmarkRunBenchDirect(b *testing.B) {
	for _, size := range []int64{1 << 20, 8 << 20, 32 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				f := newDirectFixture(b)
				l, err := NewListener(context.Background(), ListenerConfig{
					Device:  f.aline,
					Members: MemberListAdapter{List: f.members},
					Addrs:   []string{"127.0.0.1:0"},
				})
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				stats, err := RunBench(ctx, DialConfig{
					Device:    f.bob,
					Resolver:  MemberListAdapter{List: f.members},
					Target:    f.aline.ID(),
					Endpoints: l.BoundAddrs(),
				}, BenchConfig{Size: size})
				cancel()
				l.Close()
				if err != nil {
					b.Fatal(err)
				}
				if stats.Duration <= 0 {
					b.Fatal("non-positive duration")
				}
				b.ReportMetric(float64(size*2)/stats.Duration.Seconds()/(1<<20), "MiB/s")
			}
		})
	}
}
