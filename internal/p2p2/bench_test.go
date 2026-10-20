// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"
)

// The benchmarks in this file are the KPI harness. They are benchmarks rather
// than tests so that `go test -bench` reports throughput without a pass/fail
// threshold that would flake on a loaded machine; the thresholds live in
// TestThroughputFloor, which uses a deliberately loose bound.

// benchTransfer sends one message of size bytes over a fresh pair and reports
// MiB/s. drop injects loss in both directions.
func benchTransfer(b *testing.B, size int, drop float64) {
	b.Helper()
	b.SetBytes(int64(size))
	b.ReportAllocs()

	msg := make([]byte, size)
	if _, err := rand.Read(msg); err != nil {
		b.Fatal(err)
	}
	want := sha256.Sum256(msg)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a, bb := pairB(b, drop)

		done := make(chan []byte, 1)
		go func() {
			m, err := bb.Recv()
			if err == nil {
				done <- m
			}
			close(done)
		}()

		if err := a.Send(msg); err != nil {
			b.Fatalf("send: %v", err)
		}
		select {
		case got := <-done:
			if sha256.Sum256(got) != want {
				b.Fatal("payload corrupted")
			}
		case <-time.After(180 * time.Second):
			b.Fatal("message never arrived")
		}
		_ = a.Close()
		_ = bb.Close()
	}
}

// BenchmarkTransfer512KiB is the headline throughput KPI: one 512 KiB message,
// which is exactly the peer layer's file chunk size.
func BenchmarkTransfer512KiB(b *testing.B) { benchTransfer(b, 512<<10, 0) }

// BenchmarkTransfer512KiB_Loss5 measures the same transfer with 5% loss, where
// retransmission and ACK policy dominate.
func BenchmarkTransfer512KiB_Loss5(b *testing.B) { benchTransfer(b, 512<<10, 0.05) }

// BenchmarkTransfer4MiB checks that throughput does not degrade with size: a
// per-message cost would show up here as a lower number than the 512 KiB case.
func BenchmarkTransfer4MiB(b *testing.B) { benchTransfer(b, 4<<20, 0) }

// pairB is pair for benchmarks (testing.B has no Cleanup before Go 1.14's
// TB.Cleanup, but it does; this keeps the signature explicit).
func pairB(b *testing.B, drop float64) (*Conn, *Conn) {
	b.Helper()
	a, bb := pairTB(b, drop)
	return a, bb
}

// TestThroughputFloor guards the throughput KPI with a loose bound. The target
// is 100 MiB/s on loopback; the floor here is deliberately far below it so the
// test catches an order-of-magnitude regression without failing on a busy CI
// machine.
func TestThroughputFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput measurement skipped in -short mode")
	}
	// The race detector instruments every memory access, and this transfer is
	// dominated by one syscall per 1 KB datagram: measured 14 MiB/s under -race
	// against 100+ MiB/s without it. A throughput floor is meaningless there, so
	// the test reports the number and skips the assertion rather than failing on
	// the instrumentation.
	raced := raceEnabled
	const size = 512 << 10
	a, b := pair(t, DefaultConfig(), 0)

	msg := make([]byte, size)
	if _, err := rand.Read(msg); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(msg)

	// Warm up: the first transfer pays for socket buffer growth and the RTT
	// estimate. Measuring it would report the ramp, not the steady state.
	run := func() (time.Duration, bool) {
		done := make(chan []byte, 1)
		go func() {
			m, err := b.Recv()
			if err == nil {
				done <- m
			}
			close(done)
		}()
		start := time.Now()
		if err := a.Send(msg); err != nil {
			t.Fatalf("send: %v", err)
		}
		select {
		case got := <-done:
			return time.Since(start), sha256.Sum256(got) == want
		case <-time.After(120 * time.Second):
			t.Fatal("message never arrived")
			return 0, false
		}
	}

	run() // warm-up
	var best time.Duration
	const rounds = 5
	for i := 0; i < rounds; i++ {
		elapsed, ok := run()
		if !ok {
			t.Fatal("payload corrupted")
		}
		if best == 0 || elapsed < best {
			best = elapsed
		}
	}
	miBs := float64(size) / best.Seconds() / (1 << 20)
	t.Logf("512 KiB best of %d: %s (%.1f MiB/s), stats=%+v",
		rounds, best.Round(time.Microsecond), miBs, a.Stats())

	// 20 MiB/s is the floor, not the goal: the legacy transport managed 13
	// MiB/s and QUIC reaches ~220 MiB/s, so anything below 20 means the send
	// path is doing something structurally wrong (a per-packet sleep, a
	// syscall per byte, or an RTO in the steady state).
	if miBs < 20 {
		if raced {
			t.Logf("throughput %.1f MiB/s under -race: assertion skipped", miBs)
			return
		}
		t.Errorf("throughput %.1f MiB/s is below the 20 MiB/s floor", miBs)
	}
}
