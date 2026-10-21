// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p

import (
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"
)

// TestLargeMessageThroughput is the regression guard for the RTO and socket
// buffer bugs: with a connection-wide RTO that doubled on every tick, a single
// lost datagram stalled the transfer for seconds (measured 2.2 MiB/s versus
// 68 MiB/s on the same machine). The assertion is deliberately loose — it
// catches "an order of magnitude slower", not normal variance.
func TestLargeMessageThroughput(t *testing.T) {
	a, b := pipePair(t, DefaultConfig())

	const size = 512 << 10
	msg := make([]byte, size)
	if _, err := rand.Read(msg); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(msg)

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
		elapsed := time.Since(start)
		if sha256.Sum256(got) != want {
			t.Fatal("payload corrupted")
		}
		mbps := float64(size) / elapsed.Seconds() / (1 << 20)
		t.Logf("512 KiB in %s (%.1f MiB/s)", elapsed.Round(time.Millisecond), mbps)

		// The guard is about the RTO regression, which cost an order of
		// magnitude (2.2 MiB/s vs 68 MiB/s), not about this machine's speed.
		//
		// An absolute floor was wrong: `go test ./...` runs packages in
		// parallel, and under that CPU contention a healthy loopback transfer
		// measures 3.9-4.0 MiB/s on an 8-core machine, which is below any
		// threshold tight enough to catch the bug it was guarding. That made
		// this test fail roughly one run in six on unmodified main, blocking
		// unrelated merges.
		//
		// A transfer that stalls on a retransmit takes seconds, so the
		// assertion that actually distinguishes the regression is "did this
		// take a whole retransmit period", not "did it hit N MiB/s".
		if elapsed > retransmitStallFloor {
			t.Errorf("512 KiB took %s, which is on the order of a retransmit stall; "+
				"the RTO or socket buffer path regressed", elapsed.Round(time.Millisecond))
		}
		if testing.Short() {
			// A throughput number is only meaningful on an idle machine; skip
			// the advisory check rather than report a misleading figure.
			return
		}
		// Advisory only: log a very low number for a human to look at, without
		// failing a parallel run for it.
		if mbps < 1 {
			t.Logf("WARNING: %.1f MiB/s is unusually low; re-run with -p 1 on an "+
				"idle machine before suspecting a regression", mbps)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("512 KiB message never arrived")
	}
}

// retransmitStallFloor is the elapsed time that means "a retransmit happened
// and dominated the transfer". The RTO bug this test guards stalled a 512 KiB
// message for seconds; a healthy transfer is milliseconds even under heavy
// parallel test load.
const retransmitStallFloor = 2 * time.Second

// TestRepeatedLargeMessages runs the transfer several times: the original bug
// was intermittent (1 of 3 attempts), so a single run would not catch it.
func TestRepeatedLargeMessages(t *testing.T) {
	const runs = 6
	for i := 0; i < runs; i++ {
		a, b := pipePair(t, DefaultConfig())

		msg := make([]byte, 256<<10)
		if _, err := rand.Read(msg); err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256(msg)

		done := make(chan []byte, 1)
		go func() {
			m, err := b.Recv()
			if err == nil {
				done <- m
			}
			close(done)
		}()

		if err := a.Send(msg); err != nil {
			t.Fatalf("run %d: send: %v", i, err)
		}
		select {
		case got := <-done:
			if sha256.Sum256(got) != want {
				t.Fatalf("run %d: payload corrupted", i)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("run %d: message never arrived", i)
		}
	}
}
