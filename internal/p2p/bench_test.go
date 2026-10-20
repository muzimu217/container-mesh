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
		// A healthy loopback transfer is tens of MiB/s. Anything under 5 MiB/s
		// means the retransmit path is misbehaving, which is what this guards.
		if mbps < 5 {
			t.Errorf("throughput %.1f MiB/s is far below the loopback baseline; "+
				"the retransmit timer or socket buffers regressed", mbps)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("512 KiB message never arrived")
	}
}

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
