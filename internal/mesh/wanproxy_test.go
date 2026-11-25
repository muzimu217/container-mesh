// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

// The WAN harness: a UDP proxy that adds latency and loss to a leg.
//
// ★ WHY THIS FILE IS SHARED. It is the harness several investigations
// use: the loss investigation (relaytrunk_lossrepro_test.go) and the
// exclusion of the latency hypothesis (udpbuffer_test.go). It is one
// file so that the two agree on what "a path with latency and loss"
// MEANS — a harness that two investigations define differently is a
// harness that can prove anything.
//
// ★ WHY A PROXY AND NOT A SIMULATOR. The thing under test is QUIC: the
// handshake, the flow control windows and the retransmissions are
// exactly what the mesh code has to survive. A simulator would replace
// the thing that matters.
//
// ★ WHAT THE HARNESS IS FOR. The real machine reported that a relayed
// file transfer fails from 16 KiB upwards. The obvious hypothesis is
// "the path is slow" or "the path loses packets". The measurements
// below rule both out: a 100 ms round trip with no loss carries every
// size, and 5% uniform loss carries every size too.
//
// So the harness exists to answer "is the relay data plane sensitive to
// latency or to uniform loss?", and the answer is NO.
//
// (The DEFECT that the real machine's gradient turned out to be — the
// kernel capping the UDP socket's receive buffer at 448 KiB and
// dropping the datagrams it cannot hold — is pinned in
// udpbuffer_test.go.)

import (
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"
)

// wanProxy is a UDP proxy that adds a fixed one-way delay and drops a
// fraction of the datagrams on the way IN.
//
// The replies are NOT dropped: a proxy that drops only the client's
// datagrams models a HALF lossy path, and a real WAN loses packets in
// both directions. The loss investigation's own proxy models the two
// directions separately.
type wanProxy struct {
	ln     *net.UDPConn
	target *net.UDPAddr
	delay  time.Duration
	loss   float64

	mu     sync.Mutex
	conns  map[string]*net.UDPConn
	rnd    *rand.Rand
	closed bool
}

// newWanProxy starts a proxy in front of the given UDP target.
//
// The delay is ONE-WAY: the datagram is held for `delay` before it is
// handed on. So `delay` of 50 ms gives a 100 ms round trip.
//
// The loss is a fraction of the datagrams, dropped on the way IN.
func newWanProxy(t testing.TB, target string, delay time.Duration, loss float64) *wanProxy {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		t.Fatalf("the proxy target %q is not an address: %v", target, err)
	}
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("the proxy cannot bind: %v", err)
	}
	p := &wanProxy{
		ln:     ln,
		target: addr,
		delay:  delay,
		loss:   loss,
		conns:  map[string]*net.UDPConn{},
		rnd:    rand.New(rand.NewSource(20261125)),
	}
	go p.serve()
	t.Cleanup(p.Close)
	return p
}

// Addr is the address the proxy listens on.
func (p *wanProxy) Addr() string {
	return p.ln.LocalAddr().String()
}

// Close stops the proxy and releases its sockets.
func (p *wanProxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	conns := make([]*net.UDPConn, 0, len(p.conns))
	for _, c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	_ = p.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

// serve is the proxy's read loop.
func (p *wanProxy) serve() {
	buf := make([]byte, 64<<10)
	for {
		n, from, err := p.ln.ReadFromUDP(buf)
		if err != nil {
			return
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		p.handle(payload, from)
	}
}

// handle forwards one datagram to the target, after the delay, unless it
// is dropped.
func (p *wanProxy) handle(payload []byte, from *net.UDPAddr) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	drop := p.rnd.Float64() < p.loss
	key := from.String()
	out, ok := p.conns[key]
	if !ok {
		c, err := net.DialUDP("udp", nil, p.target)
		if err != nil {
			p.mu.Unlock()
			return
		}
		p.conns[key] = c
		out = c
		go p.reply(c, from)
	}
	p.mu.Unlock()
	if drop {
		return
	}
	p.send(out, payload)
}

// reply copies the target's datagrams back to the client.
//
// The replies go out from the proxy's LISTENING socket, not from the
// egress socket: QUIC checks that a packet comes from the address it
// sent to, and a reply from a different source address would look like
// a path change.
func (p *wanProxy) reply(out *net.UDPConn, to *net.UDPAddr) {
	buf := make([]byte, 64<<10)
	for {
		n, err := out.Read(buf)
		if err != nil {
			return
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		p.sendTo(payload, to)
	}
}

// send writes to the egress socket after the delay.
func (p *wanProxy) send(out *net.UDPConn, payload []byte) {
	if p.delay <= 0 {
		_, _ = out.Write(payload)
		return
	}
	time.AfterFunc(p.delay, func() {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return
		}
		_, _ = out.Write(payload)
	})
}

// sendTo writes to the listening socket after the delay.
func (p *wanProxy) sendTo(payload []byte, to *net.UDPAddr) {
	if p.delay <= 0 {
		_, _ = p.ln.WriteToUDP(payload, to)
		return
	}
	time.AfterFunc(p.delay, func() {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return
		}
		_, _ = p.ln.WriteToUDP(payload, to)
	})
}

// describeRelayFailure turns a failure into the PHASE it belongs to.
//
// ★ THE PROJECT'S OWN LESSON APPLIES HERE: the seconds in a failure can
// be two timeouts added together, and the same text can come from two
// different layers. So the phase is derived from the layer that produced
// the error, not from the wording alone.
//
// It is shared because the loss investigation and the buffer
// investigation must name a failure the SAME way — otherwise two
// investigations can describe the same failure differently and neither
// one's conclusion can be checked against the other's.
func describeRelayFailure(err error) string {
	if err == nil {
		return "no failure"
	}
	msg := err.Error()
	switch {
	case containsText(msg, "read the target's hello"):
		return "HANDSHAKE (the requester never got the target's hello)"
	case containsText(msg, "read the trunk's answer"):
		return "HANDSHAKE (the anchor never answered the open record)"
	case containsText(msg, "send body"):
		return "TRANSFER-WRITE (the sender's body write failed)"
	case containsText(msg, "read verdict"):
		return "TRANSFER-VERDICT (the sender wrote the body but never got the verdict)"
	case containsText(msg, "read file ack"):
		return "TRANSFER-ACK (the receiver never answered the header)"
	case containsText(msg, "relay peer failed"):
		return "RELAY (the anchor tore the flow down)"
	case containsText(msg, "peer unreachable"):
		return "DIAL (the path could not be established)"
	case containsText(msg, "deadline exceeded"):
		return "TIMEOUT (some deadline)"
	case containsText(msg, "EOF"):
		return "EOF (a peer went away mid-session)"
	default:
		return "OTHER (" + msg + ")"
	}
}

// containsText reports whether haystack contains needle.
func containsText(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
