// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2pquic

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"net"
	"testing"
	"time"
)

// pair brings up a QUIC connection between two loopback sockets.
func pair(t *testing.T) (*Conn, *Conn) {
	t.Helper()

	la, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	lb, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.HandshakeTimeout = 5 * time.Second

	type res struct {
		c   *Conn
		err error
	}
	srvCh := make(chan res, 1)
	go func() {
		c, err := Listen(context.Background(), lb, cfg)
		srvCh <- res{c, err}
	}()

	// Give the listener a moment to start before dialling.
	time.Sleep(50 * time.Millisecond)

	cli, err := Dial(context.Background(), la, lb.LocalAddr().(*net.UDPAddr), cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	s := <-srvCh
	if s.err != nil {
		t.Fatalf("listen: %v", s.err)
	}

	t.Cleanup(func() { _ = cli.Close(); _ = s.c.Close() })
	return cli, s.c
}

// TestRoundTrip is the basic contract: one message in, the same message out.
func TestRoundTrip(t *testing.T) {
	a, b := pair(t)

	msg := []byte("hello over quic")
	if err := a.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	done := make(chan []byte, 1)
	go func() {
		m, err := b.Recv()
		if err == nil {
			done <- m
		}
		close(done)
	}()

	select {
	case got := <-done:
		if string(got) != string(msg) {
			t.Errorf("got %q, want %q", got, msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("message never arrived")
	}
}

// TestLargeMessage covers the size the peer layer actually sends (512 KiB
// chunks), which spans many QUIC packets and exercises flow control.
func TestLargeMessage(t *testing.T) {
	a, b := pair(t)

	msg := make([]byte, 512<<10)
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
		t.Fatalf("send: %v", err)
	}

	select {
	case got := <-done:
		if sha256.Sum256(got) != want {
			t.Fatal("payload corrupted")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("large message never arrived")
	}
}

// TestManyMessagesInOrder is the case the hand-written layer failed: 20 small
// messages, all of which must arrive in order and with the right contents.
//
// With the previous transport this test delivered message 9 in place of
// message 8 after a lost datagram, because a dropped frame was silently
// discarded while Send had already returned success.
func TestManyMessagesInOrder(t *testing.T) {
	a, b := pair(t)

	const n = 20
	got := make(chan []byte, n)
	go func() {
		for i := 0; i < n; i++ {
			m, err := b.Recv()
			if err != nil {
				return
			}
			got <- m
		}
	}()

	for i := 0; i < n; i++ {
		if err := a.Send([]byte{byte(i), 'm'}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	for i := 0; i < n; i++ {
		select {
		case m := <-got:
			if len(m) != 2 || m[0] != byte(i) || m[1] != 'm' {
				t.Fatalf("message %d: got %v, want [%d 109]", i, m, i)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("only %d of %d messages arrived", i, n)
		}
	}
}

// TestBidirectional sends in both directions at once: QUIC multiplexes
// streams, so neither direction blocks the other.
func TestBidirectional(t *testing.T) {
	a, b := pair(t)

	const n = 10
	errs := make(chan error, 2)
	collect := func(c *Conn, tag byte) {
		for i := 0; i < n; i++ {
			m, err := c.Recv()
			if err != nil {
				errs <- err
				return
			}
			if len(m) != 2 || m[0] != byte(i) || m[1] != tag {
				errs <- err
				return
			}
		}
		errs <- nil
	}
	go collect(b, 'a')
	go collect(a, 'b')

	for i := 0; i < n; i++ {
		if err := a.Send([]byte{byte(i), 'a'}); err != nil {
			t.Fatalf("a send %d: %v", i, err)
		}
		if err := b.Send([]byte{byte(i), 'b'}); err != nil {
			t.Fatalf("b send %d: %v", i, err)
		}
	}

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("direction failed: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("bidirectional transfer timed out")
		}
	}
}

// TestCloseUnblocksRecv mirrors the contract the peer layer depends on.
func TestCloseUnblocksRecv(t *testing.T) {
	a, b := pair(t)
	_ = a

	errCh := make(chan error, 1)
	go func() {
		_, err := b.Recv()
		errCh <- err
	}()
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() { _ = b.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Recv should fail after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv did not unblock")
	}
}

// TestThroughputBaseline records a floor for the direct path. QUIC with proper
// congestion control should be far above the hand-written layer's 13 MiB/s;
// the assertion only catches an order-of-magnitude regression.
func TestThroughputBaseline(t *testing.T) {
	a, b := pair(t)

	const size = 2 << 20 // 2 MiB
	msg := make([]byte, size)
	if _, err := rand.Read(msg); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		_, _ = b.Recv()
		close(done)
	}()

	start := time.Now()
	if err := a.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case <-done:
		elapsed := time.Since(start)
		mbps := float64(size) / elapsed.Seconds() / (1 << 20)
		t.Logf("2 MiB in %s (%.1f MiB/s)", elapsed.Round(time.Millisecond), mbps)
		if mbps < 5 {
			t.Errorf("throughput %.1f MiB/s is below the floor", mbps)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("transfer timed out")
	}
}
