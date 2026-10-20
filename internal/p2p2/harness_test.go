// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"
)

// lossyPacketConn drops a fraction of datagrams in each direction so the
// transport can be exercised against loss without `tc netem`.
//
// The random source is per-wrapper and seeded explicitly: two wrappers that
// shared one source would produce a different drop pattern depending on
// goroutine interleaving, which makes a failure impossible to reproduce.
type lossyPacketConn struct {
	net.PacketConn
	drop float64

	// mu guards rnd: the read loop, the write path and retransmission all call
	// roll from different goroutines, so a bare math/rand.Rand is a data race.
	mu  sync.Mutex
	rnd *rand.Rand

	dropped int64
}

func (l *lossyPacketConn) roll() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rnd.Float64()
}

func (l *lossyPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if l.roll() < l.drop {
		l.mu.Lock()
		l.dropped++
		l.mu.Unlock()
		return len(b), nil // dropped silently, like a real network
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
			l.mu.Lock()
			l.dropped++
			l.mu.Unlock()
			continue
		}
		return n, addr, nil
	}
}

func (l *lossyPacketConn) dropCount() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

// SetReadBuffer/SetWriteBuffer forward to the wrapped socket.
//
// Embedding net.PacketConn does not promote these: they are declared on
// *net.UDPConn, not on the interface. Without explicit forwarding the
// transport's buffer tuning silently does nothing in every test that uses loss
// injection, and the resulting overflow is indistinguishable from packet loss —
// which is exactly the kind of false signal that makes a transport look broken
// when the test harness is what is broken.
func (l *lossyPacketConn) SetReadBuffer(n int) error {
	if uc, ok := l.PacketConn.(*net.UDPConn); ok {
		return uc.SetReadBuffer(n)
	}
	return nil
}

func (l *lossyPacketConn) SetWriteBuffer(n int) error {
	if uc, ok := l.PacketConn.(*net.UDPConn); ok {
		return uc.SetWriteBuffer(n)
	}
	return nil
}

// selectiveProxy forwards datagrams between two UDP sockets and drops
// datagrams matching an exact predicate.
//
// It exists because a probabilistic test can only show "unlikely to be wrong";
// dropping message 8's third fragment deterministically reproduces the exact
// reported failure, so the regression test fails every time on the code that
// has the bug instead of one run in twenty.
//
// Topology:
//
//	sender A --(front socket)--> proxy --(back socket)--> receiver B
//	sender A <--(front socket)-- proxy <--(back socket)-- receiver B
//
// "up" is sender to receiver, "down" is the reverse.
type selectiveProxy struct {
	front *net.UDPConn // the side the sender talks to
	back  *net.UDPConn // the side the receiver talks to

	// frontPeer/backPeer are the endpoints each socket forwards to. They are
	// set explicitly rather than learned: a proxy that waits to learn would
	// have to drop the first datagram of the connection, which changes the
	// very loss pattern these tests control.
	frontPeer *net.UDPAddr
	backPeer  *net.UDPAddr

	// dropFn decides per datagram, in the direction it is travelling.
	mu     sync.Mutex
	dropFn func(dir string, data []byte) bool
	drops  map[string]int
}

func newSelectiveProxy(t *testing.T, dropFn func(dir string, data []byte) bool) *selectiveProxy {
	t.Helper()
	front, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	back, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p := &selectiveProxy{
		front:  front,
		back:   back,
		dropFn: dropFn,
		drops:  map[string]int{},
	}
	go p.pump(front, back, "up", func() *net.UDPAddr { return p.backPeer })
	go p.pump(back, front, "down", func() *net.UDPAddr { return p.frontPeer })
	t.Cleanup(func() {
		_ = front.Close()
		_ = back.Close()
	})
	return p
}

// setPeers tells the proxy where the sender and the receiver live.
func (p *selectiveProxy) setPeers(sender, receiver net.Addr) {
	p.mu.Lock()
	p.frontPeer = sender.(*net.UDPAddr)
	p.backPeer = receiver.(*net.UDPAddr)
	p.mu.Unlock()
}

func (p *selectiveProxy) pump(from, to *net.UDPConn, dir string, dst func() *net.UDPAddr) {
	buf := make([]byte, 2048)
	for {
		n, _, err := from.ReadFromUDP(buf)
		if err != nil {
			return
		}

		p.mu.Lock()
		drop := p.dropFn(dir, buf[:n])
		if drop {
			p.drops[dir]++
		}
		target := dst()
		p.mu.Unlock()
		if drop || target == nil {
			continue
		}
		if _, err := to.WriteToUDP(buf[:n], target); err != nil {
			return
		}
	}
}

func (p *selectiveProxy) dropCount(dir string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.drops[dir]
}

func (p *selectiveProxy) totalDrops() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, v := range p.drops {
		n += v
	}
	return n
}

// newConnOn builds a Conn on a fresh loopback socket talking to remote.
func newConnOn(t *testing.T, remote *net.UDPAddr, connID uint64, cfg Config) (*Conn, *net.UDPConn) {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	c := NewConn(pc, remote, connID, cfg)
	t.Cleanup(func() { _ = c.Close() })
	return c, pc
}

// pair returns two Conns wired to each other over real UDP loopback, optionally
// through a lossy wrapper on each side.
func pair(t *testing.T, cfg Config, drop float64) (*Conn, *Conn) {
	t.Helper()
	const connID = 0x1122334455667788

	la, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	lb, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	var pa, pb net.PacketConn = la, lb
	if drop > 0 {
		// Distinct seeds per direction: identical seeds would make both sides
		// drop the same index in their own streams, which correlates the two
		// directions in a way real networks do not.
		pa = &lossyPacketConn{PacketConn: la, drop: drop, rnd: rand.New(rand.NewSource(11))}
		pb = &lossyPacketConn{PacketConn: lb, drop: drop, rnd: rand.New(rand.NewSource(12))}
	}

	a := NewConn(pa, lb.LocalAddr(), connID, cfg)
	b := NewConn(pb, la.LocalAddr(), connID, cfg)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

// recvN reads n messages, failing the test on error or timeout.
func recvN(t *testing.T, c *Conn, n int, timeout time.Duration) [][]byte {
	t.Helper()
	type item struct {
		msg []byte
		err error
	}
	ch := make(chan item, n)
	go func() {
		for i := 0; i < n; i++ {
			m, err := c.Recv()
			ch <- item{m, err}
			if err != nil {
				return
			}
		}
	}()

	out := make([][]byte, 0, n)
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case it := <-ch:
			if it.err != nil {
				t.Fatalf("recv %d of %d: %v", len(out), n, it.err)
			}
			out = append(out, it.msg)
		case <-deadline:
			t.Fatalf("received %d of %d messages before timeout", len(out), n)
		}
	}
	return out
}

// fillMessage returns a message whose every byte identifies its message index,
// so a fragment from the wrong message is visible at a glance in a diff.
func fillMessage(i, size int) []byte {
	b := make([]byte, size)
	for j := range b {
		b[j] = byte(i)
	}
	return b
}

// desc renders the first bytes of a message for failure output. A cross-message
// mix-up shows up as an unexpected leading byte, which is the fastest way to
// see it.
func desc(b []byte) string {
	n := 8
	if len(b) < n {
		n = len(b)
	}
	return string(b[:n])
}

// pairTB is pair for benchmarks and tests alike: testing.TB covers both.
func pairTB(tb testing.TB, drop float64) (*Conn, *Conn) {
	tb.Helper()
	const connID = 0x1122334455667788

	la, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		tb.Fatal(err)
	}
	lb, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		tb.Fatal(err)
	}

	var pa, pb net.PacketConn = la, lb
	if drop > 0 {
		pa = &lossyPacketConn{PacketConn: la, drop: drop, rnd: rand.New(rand.NewSource(11))}
		pb = &lossyPacketConn{PacketConn: lb, drop: drop, rnd: rand.New(rand.NewSource(12))}
	}

	a := NewConn(pa, lb.LocalAddr(), connID, DefaultConfig())
	b := NewConn(pb, la.LocalAddr(), connID, DefaultConfig())
	tb.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}
