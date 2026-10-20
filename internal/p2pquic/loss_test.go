// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2pquic

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"
)

// lossyPacketConn drops a fraction of datagrams in each direction so the
// transport can be tested against loss without `tc netem` (absent on the
// verification hosts).
type lossyPacketConn struct {
	net.PacketConn
	drop float64
	// rnd is guarded: QUIC writes from its own goroutines, so a bare
	// math/rand.Rand here is a data race (which -race catches in this test).
	mu  sync.Mutex
	rnd *rand.Rand
}

func (l *lossyPacketConn) roll() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rnd.Float64()
}

func (l *lossyPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if l.roll() < l.drop {
		return len(b), nil // silently dropped, like a real network
	}
	return l.PacketConn.WriteTo(b, addr)
}

func (l *lossyPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, addr, err := l.PacketConn.ReadFrom(b)
		if err != nil {
			return n, addr, err
		}
		if l.roll() < l.drop {
			continue
		}
		return n, addr, nil
	}
}

// lossyPair starts a QUIC connection over sockets that drop datagrams.
func lossyPair(t *testing.T, drop float64) (*Conn, *Conn) {
	t.Helper()

	la, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	lb, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}

	src := rand.New(rand.NewSource(11))
	ca := &lossyPacketConn{PacketConn: la, drop: drop, rnd: src}
	cb := &lossyPacketConn{PacketConn: lb, drop: drop, rnd: rand.New(rand.NewSource(12))}

	cfg := DefaultConfig()
	cfg.HandshakeTimeout = 20 * time.Second

	type res struct {
		c   *Conn
		err error
	}
	srvCh := make(chan res, 1)
	go func() {
		c, err := Listen(context.Background(), cb, cfg)
		srvCh <- res{c, err}
	}()
	time.Sleep(100 * time.Millisecond)

	cli, err := Dial(context.Background(), ca, lb.LocalAddr().(*net.UDPAddr), cfg)
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

// TestMessageIntegrityUnderLoss is the property the hand-written transport
// failed: 20 messages at 10% loss, all must arrive in order with the right
// contents. There QUIC's congestion control and SACK do the work.
func TestMessageIntegrityUnderLoss(t *testing.T) {
	for _, drop := range []float64{0.05, 0.10, 0.20} {
		t.Run(dropName(drop), func(t *testing.T) {
			a, b := lossyPair(t, drop)

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
				case <-time.After(120 * time.Second):
					t.Fatalf("only %d of %d messages arrived at %.0f%% loss", i, n, drop*100)
				}
			}
		})
	}
}

// TestLargePayloadUnderLoss transfers a real chunk size over a lossy link.
func TestLargePayloadUnderLoss(t *testing.T) {
	a, b := lossyPair(t, 0.10)

	msg := make([]byte, 512<<10)
	if _, err := crand.Read(msg); err != nil {
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
		if sha256.Sum256(got) != want {
			t.Fatal("payload corrupted under loss")
		}
		elapsed := time.Since(start)
		mbps := float64(len(msg)) / elapsed.Seconds() / (1 << 20)
		t.Logf("512 KiB at 10%% loss in %s (%.1f MiB/s)", elapsed.Round(time.Millisecond), mbps)
	case <-time.After(180 * time.Second):
		t.Fatal("large payload never arrived under loss")
	}
}

func dropName(d float64) string {
	switch d {
	case 0.05:
		return "5%"
	case 0.10:
		return "10%"
	case 0.20:
		return "20%"
	}
	return "loss"
}
