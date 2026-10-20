// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p

import (
	"net"
	"sync"
	"testing"
	"time"
)

// newTestConn builds a Conn with no goroutines, for exercising the send/ACK
// bookkeeping directly. NewConn would start a read loop that races the test's
// own handleAck calls on the same socket.
func newTestConn(t *testing.T) *Conn {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.fill()
	c := &Conn{
		udp:       udp,
		remote:    &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9},
		connID:    1,
		sendBuf:   make(map[uint32]*outFrame),
		recvParts: make(map[uint32][]byte),
		recvSeen:  make(map[uint32]bool),
		inbox:     make(chan []byte, cfg.InboxSize),
		done:      make(chan struct{}),
		rto:       cfg.InitialRTO,
		window:    cfg.Window,
	}
	c.windowFree = sync.NewCond(&c.sendMu)
	t.Cleanup(func() { _ = udp.Close() })
	return c
}

// TestCumulativeAckIsExclusive is the regression test for a lost-datagram hang.
//
// The receiver advertises recvNext — the next sequence number it has NOT seen.
// handleAck must therefore delete only sequences strictly below that value.
// Treating it as inclusive removed one extra frame, so the first datagram lost
// in flight was never retransmitted: Send returned, Recv hung forever.
func TestCumulativeAckIsExclusive(t *testing.T) {
	c := newTestConn(t)

	// Pretend three datagrams are in flight: seq 0, 1, 2.
	c.sendMu.Lock()
	for seq := uint32(0); seq < 3; seq++ {
		c.sendBuf[seq] = &outFrame{data: []byte("x"), sentAt: time.Now()}
		c.inFlight++
	}
	c.sendMu.Unlock()

	// The peer received 0 and 1, so it advertises recvNext = 2. Sequence 2 is
	// still missing and MUST stay in the buffer for retransmission.
	c.handleAck(&packet{typ: pktAck, connID: 1, ack: 2})

	c.sendMu.Lock()
	_, seq2Present := c.sendBuf[2]
	_, seq0Present := c.sendBuf[0]
	_, seq1Present := c.sendBuf[1]
	inFlight := c.inFlight
	c.sendMu.Unlock()

	if seq0Present || seq1Present {
		t.Error("acknowledged frames (0, 1) were not removed")
	}
	if !seq2Present {
		t.Error("frame 2 was dropped by an ACK advertising recvNext=2: " +
			"a lost datagram would never be retransmitted")
	}
	if inFlight != 1 {
		t.Errorf("inFlight = %d, want 1 (only seq 2 outstanding)", inFlight)
	}
}

// TestAckAllBelowIsRemoved covers the ordinary case where everything sent so
// far has arrived.
func TestAckAllBelowIsRemoved(t *testing.T) {
	c := newTestConn(t)

	c.sendMu.Lock()
	for seq := uint32(0); seq < 4; seq++ {
		c.sendBuf[seq] = &outFrame{data: []byte("x"), sentAt: time.Now()}
		c.inFlight++
	}
	c.sendMu.Unlock()

	// recvNext = 4 means 0..3 all arrived.
	c.handleAck(&packet{typ: pktAck, connID: 1, ack: 4})

	c.sendMu.Lock()
	n := len(c.sendBuf)
	inFlight := c.inFlight
	c.sendMu.Unlock()

	if n != 0 {
		t.Errorf("%d frames still queued, want 0", n)
	}
	if inFlight != 0 {
		t.Errorf("inFlight = %d, want 0", inFlight)
	}
}

// TestAckDoesNotFreeUnsentFrames: an ACK for a sequence we never sent must not
// disturb the buffer (it can happen with a duplicated or stale ACK).
func TestAckDoesNotFreeUnsentFrames(t *testing.T) {
	c := newTestConn(t)

	c.sendMu.Lock()
	c.sendBuf[7] = &outFrame{data: []byte("x"), sentAt: time.Now()}
	c.inFlight++
	c.sendMu.Unlock()

	c.handleAck(&packet{typ: pktAck, connID: 1, ack: 3})

	c.sendMu.Lock()
	_, kept := c.sendBuf[7]
	inFlight := c.inFlight
	c.sendMu.Unlock()

	if !kept {
		t.Error("frame 7 was removed by an ACK for 3")
	}
	if inFlight != 1 {
		t.Errorf("inFlight = %d, want 1", inFlight)
	}
}

// TestRejectsOutOfRangeFragment: a peer must not be able to grow the
// reassembly map with arbitrary (total, index) values. Each bogus datagram
// used to add a permanent entry, so memory grew with whatever the remote sent.
func TestRejectsOutOfRangeFragment(t *testing.T) {
	c := newTestConn(t)

	cases := []struct {
		name  string
		total uint32
		index uint32
	}{
		{"zero total", 0, 0},
		{"index equals total", 4, 4},
		{"index far beyond total", 2, 1 << 30},
		{"total above cap", maxFragments + 1, 0},
		{"absurd total", 1 << 31, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := make([]byte, 8+16)
			putUint32(body[0:4], tc.total)
			putUint32(body[4:8], tc.index)
			c.handleData(&packet{
				typ: pktData, connID: c.connID, seq: 0, payload: body,
			})

			c.recvMu.Lock()
			n := len(c.recvParts)
			c.recvMu.Unlock()
			if n != 0 {
				t.Errorf("reassembly map grew to %d entries from a bogus fragment", n)
			}
		})
	}
}

// TestAcceptsValidFragment is the control: in-range fragments still reassemble.
func TestAcceptsValidFragment(t *testing.T) {
	c := newTestConn(t)

	body := make([]byte, 8+4)
	putUint32(body[0:4], 1) // total
	putUint32(body[4:8], 0) // index
	copy(body[8:], "data")

	c.handleData(&packet{typ: pktData, connID: c.connID, seq: 0, payload: body})

	select {
	case msg := <-c.inbox:
		if string(msg) != "data" {
			t.Errorf("got %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("valid single-fragment message was not delivered")
	}
}
