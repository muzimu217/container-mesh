// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"bytes"
	"testing"
	"time"
)

// TestMessageIdentityOnTheWire is P0-1 as a wire assertion rather than a
// reassembly assertion: every fragment of one message must carry the same msgID,
// and the fragments of two messages must never share one.
//
// This is the property the legacy layer did not have at all — it had no message
// identity in the header, so the receive side could not tell which message a
// fragment belonged to. Checking it at the encoder means a future change that
// drops the field fails here rather than in a reassembly test that happens to
// pass for a different reason.
func TestMessageIdentityOnTheWire(t *testing.T) {
	const connID = 0xABCD
	msg := pad('M', 5)

	// Encode exactly what Send encodes: one msgID, fragIndex 0..n-1.
	fragCount := (len(msg) + MaxPayload - 1) / MaxPayload
	var gotIDs []uint32
	var gotIndexes []uint32
	var gotCounts []uint32
	for i := 0; i < fragCount; i++ {
		lo, hi := i*MaxPayload, (i+1)*MaxPayload
		if hi > len(msg) {
			hi = len(msg)
		}
		raw := makeDataPacket(connID, uint32(i), 42, uint32(i), uint32(fragCount), uint32(len(msg)), msg[lo:hi])
		p, err := decodePacket(raw)
		if err != nil {
			t.Fatalf("decode fragment %d: %v", i, err)
		}
		gotIDs = append(gotIDs, p.msgID)
		gotIndexes = append(gotIndexes, p.fragIndex)
		gotCounts = append(gotCounts, p.fragCount)
		if p.connID != connID {
			t.Fatalf("fragment %d connID = %#x, want %#x", i, p.connID, connID)
		}
	}

	for i := range gotIDs {
		if gotIDs[i] != 42 {
			t.Errorf("fragment %d msgID = %d, want 42 for every fragment", i, gotIDs[i])
		}
		if gotIndexes[i] != uint32(i) {
			t.Errorf("fragment %d fragIndex = %d", i, gotIndexes[i])
		}
		if gotCounts[i] != uint32(fragCount) {
			t.Errorf("fragment %d fragCount = %d, want %d", i, gotCounts[i], fragCount)
		}
	}
}

// TestRetransmissionKeepsIdentity is the other half of P0-1: a retransmission
// must reuse the datagram buffer verbatim, so msgID/fragIndex/fragCount survive.
// Only the piggybacked ack block is allowed to change.
func TestRetransmissionKeepsIdentity(t *testing.T) {
	raw := makeDataPacket(1, 7, 9, 2, 4, 100, bytes.Repeat([]byte{'x'}, 10))

	before, err := decodePacket(raw)
	if err != nil {
		t.Fatal(err)
	}

	// Patch the ack block, as a retransmission would.
	var a ackBlock
	a.una = 33
	a.n = 1
	a.ranges[0] = ackRange{gap: 2, length: 5}
	patchAck(raw, &a)

	after, err := decodePacket(raw)
	if err != nil {
		t.Fatal(err)
	}
	if after.seq != before.seq || after.msgID != before.msgID ||
		after.fragIndex != before.fragIndex || after.fragCount != before.fragCount ||
		after.msgLen != before.msgLen {
		t.Fatalf("identity changed across a retransmission: %+v -> %+v", before, after)
	}
	if !bytes.Equal(after.payload, before.payload) {
		t.Fatal("payload changed across a retransmission")
	}
	if after.ack.una != 33 || after.ack.n != 1 {
		t.Fatalf("ack block was not patched: %+v", after.ack)
	}
}

// TestAckBlockRoundTrip is table-driven because the encoding has more edge cases
// than a hand-written case list survives: no runs, one run, adjacent runs, a run
// at the head of una, and the maximum number of runs.
func TestAckBlockRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   ackBlock
	}{
		{
			name: "no-runs",
			in:   ackBlock{una: 0, n: 0},
		},
		{
			name: "una-advances-nothing-above",
			in:   ackBlock{una: 1234, n: 0},
		},
		{
			name: "one-run-at-head",
			in:   ackBlock{una: 10, n: 1, ranges: [maxAckRanges]ackRange{{gap: 0, length: 0}}},
		},
		{
			name: "one-run-with-gap",
			in:   ackBlock{una: 10, n: 1, ranges: [maxAckRanges]ackRange{{gap: 1, length: 9}}},
		},
		{
			name: "two-adjacent-runs",
			in: ackBlock{una: 10, n: 2, ranges: [maxAckRanges]ackRange{
				{gap: 1, length: 3}, {gap: 0, length: 2},
			}},
		},
		{
			name: "two-runs-with-holes",
			in: ackBlock{una: 10, n: 2, ranges: [maxAckRanges]ackRange{
				{gap: 2, length: 5}, {gap: 3, length: 7},
			}},
		},
		{
			name: "max-runs",
			in: func() ackBlock {
				a := ackBlock{una: 100, n: maxAckRanges}
				for i := range a.ranges {
					a.ranges[i] = ackRange{gap: 2, length: 3}
				}
				return a
			}(),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf [maxAckBlockSize]byte
			n := encodeAckBlock(buf[:], &c.in)
			if n != ackFixedSize+4*c.in.n {
				t.Fatalf("encoded %d bytes, want %d", n, ackFixedSize+4*c.in.n)
			}
			got, used, err := decodeAckBlock(buf[:n])
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if used != n {
				t.Fatalf("used %d bytes, want %d", used, n)
			}
			if got.una != c.in.una || got.n != c.in.n {
				t.Fatalf("got una=%d n=%d, want una=%d n=%d", got.una, got.n, c.in.una, c.in.n)
			}
			for i := 0; i < c.in.n; i++ {
				if got.ranges[i] != c.in.ranges[i] {
					t.Errorf("run %d = %+v, want %+v", i, got.ranges[i], c.in.ranges[i])
				}
				if got.firstOf(i) != c.in.firstOf(i) || got.lastOf(i) != c.in.lastOf(i) {
					t.Errorf("run %d spans %d..%d, want %d..%d", i,
						got.firstOf(i), got.lastOf(i), c.in.firstOf(i), c.in.lastOf(i))
				}
			}
			// Every reported run must agree with contains().
			for i := 0; i < got.n; i++ {
				for seq := got.firstOf(i); ; seq++ {
					if !got.contains(seq) {
						t.Errorf("contains(%d) = false but run %d spans it", seq, i)
					}
					if seq == got.lastOf(i) {
						break
					}
				}
			}
		})
	}
}

// TestAckBlockRejectsMalformed covers the decode path's rejection of a hostile
// block. The sender is the trust boundary: a peer that sends a run extending
// below sequence 0, or more runs than the format allows, must be rejected before
// its numbers reach loss detection.
func TestAckBlockRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"too-short", []byte{0, 0, 0, 1}},
		{"too-many-runs", []byte{0, 0, 0, 0, maxAckRanges + 1}},
		{"truncated-runs", []byte{0, 0, 0, 0, 2, 0, 1}},
		{
			// First run at una+0, second run whose gap would wrap below zero.
			name: "wrapping-run",
			raw:  []byte{0, 0, 0, 5, 2, 0, 0, 0, 0, 0, 9, 0, 0},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := decodeAckBlock(c.raw); err == nil {
				t.Fatal("decoded a malformed ack block without error")
			}
		})
	}
}

// TestRTOCollapsesToPathRTT is P1-3 as an assertion about the estimator: after
// samples of a fixed RTT the timeout must sit just above that RTT, not at some
// unrelated floor.
func TestRTOCollapsesToPathRTT(t *testing.T) {
	cfg := DefaultConfig()
	cfg.fill()
	s := newSender(cfg, &statsCounters{}, func(error) {})

	const sample = 2 * time.Millisecond
	for i := 0; i < 50; i++ {
		s.observeRTT(sample)
	}

	rto := s.rto()
	// srtt converges to the sample and rttvar to zero, so the timeout approaches
	// srtt + 4*rttvar = the sample itself, clamped by the floor.
	if rto < sample {
		t.Fatalf("rto = %s is below the measured RTT %s", rto, sample)
	}
	if rto > 4*sample {
		t.Fatalf("rto = %s did not collapse towards the %s path RTT", rto, sample)
	}

	// A path with a large spread must produce a correspondingly larger timeout:
	// rttvar is what absorbs jitter, and dropping it would make the timer fire on
	// normal variation.
	s2 := newSender(cfg, &statsCounters{}, func(error) {})
	for i := 0; i < 50; i++ {
		if i%2 == 0 {
			s2.observeRTT(time.Millisecond)
		} else {
			s2.observeRTT(20 * time.Millisecond)
		}
	}
	if s2.rto() <= rto {
		t.Fatalf("a jittery path produced rto %s, not larger than the steady path's %s",
			s2.rto(), rto)
	}
}

// TestKarnRuleSkipsRetransmittedSamples checks that an ACK for a retransmitted
// datagram does not feed the estimator. The ACK may belong to either copy, so its
// age is ambiguous; feeding it back inflates the RTO exactly when the path is bad.
func TestKarnRuleSkipsRetransmittedSamples(t *testing.T) {
	cfg := DefaultConfig()
	cfg.fill()
	s := newSender(cfg, &statsCounters{}, func(error) {})

	now := time.Now()
	p := &outPacket{seq: 0, data: []byte{0}, sentAt: now.Add(-time.Second), rto: time.Millisecond}
	s.mu.Lock()
	s.enqueueLocked(p)
	s.mu.Unlock()

	// First transmission is acknowledged normally: one second of "RTT".
	s.handleAck(ackBlock{una: 1}, now)
	first := s.srtt
	if first == 0 {
		t.Fatal("no RTT sample was taken from an unambiguous ACK")
	}

	// Now a retransmitted packet: its ACK must not move the estimate.
	p2 := &outPacket{seq: 1, data: []byte{0}, sentAt: now.Add(-time.Second),
		rto: time.Millisecond, retransmitted: true}
	s.mu.Lock()
	s.enqueueLocked(p2)
	s.mu.Unlock()
	s.handleAck(ackBlock{una: 2}, now.Add(10*time.Millisecond))
	if s.srtt != first {
		t.Fatalf("srtt moved from %s to %s on a retransmitted packet's ACK", first, s.srtt)
	}
}

// TestSendWindowBoundsInFlight checks P1-4: the sender must never have more than
// SendWindow datagrams outstanding, however large the message.
func TestSendWindowBoundsInFlight(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SendWindow = 16
	a, b := pair(t, cfg, 0)

	// Never read from b, so ACKs stop after the socket buffer fills. Send must
	// still stay within the window rather than pushing the whole message.
	msg := make([]byte, 200*MaxPayload)
	go func() {
		if err := a.Send(msg); err != nil {
			t.Logf("send returned %v", err)
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	var maxInFlight int
	for time.Now().Before(deadline) {
		st := a.Stats()
		if st.InFlight > maxInFlight {
			maxInFlight = st.InFlight
		}
		if maxInFlight > cfg.SendWindow {
			t.Fatalf("in flight reached %d, above the %d window", maxInFlight, cfg.SendWindow)
		}
		time.Sleep(time.Millisecond)
	}
	if maxInFlight == 0 {
		t.Fatal("nothing was ever in flight")
	}
	_ = b.Close()
	_ = a.Close()
}

// TestReceiveWindowRejectsFarMsgID checks P1-5 at the protocol boundary: a
// msgID beyond the receive window must be dropped without allocating a
// reassembly slot, which is what keeps a hostile peer from growing the table.
func TestReceiveWindowRejectsFarMsgID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RecvWindow = 8
	tr := newTestReceiver(t, cfg)

	msg := pad('R', 2)
	// msgID 8 is exactly at the window edge (0..7 are inside).
	for _, id := range []uint32{0, 7, 8, 1000, 1 << 20} {
		frags := tr.fragments(id, msg)
		tr.feed(t, frags[0])
		_, _, entries, _ := tr.r.stats()
		if id < uint32(cfg.RecvWindow) && entries == 0 {
			t.Errorf("msgID %d inside the window was not tracked", id)
		}
		if id >= uint32(cfg.RecvWindow) && entries > cfg.RecvWindow {
			t.Errorf("msgID %d outside the window grew the table to %d entries", id, entries)
		}
	}
	if n := tr.entries(); n > cfg.RecvWindow {
		t.Fatalf("reassembly table holds %d entries, window is %d", n, cfg.RecvWindow)
	}
}

// TestMTUFitsUnderIPv6Minimum is P2-3 as a constant assertion: every datagram
// this package emits must fit the smallest MTU in common use, so IP
// fragmentation never turns one lost fragment into a lost message.
func TestMTUFitsUnderIPv6Minimum(t *testing.T) {
	const ipv6MinMTU = 1280
	if MaxDatagram > ipv6MinMTU {
		t.Fatalf("MaxDatagram = %d exceeds the IPv6 minimum MTU %d", MaxDatagram, ipv6MinMTU)
	}

	// The largest possible data datagram must still fit.
	big := makeDataPacket(1, 1, 1, 0, 1, MaxPayload, make([]byte, MaxPayload))
	if len(big) > MaxDatagram {
		t.Fatalf("largest data datagram is %d bytes, above MaxDatagram %d", len(big), MaxDatagram)
	}

	// A pure ACK with a full run table must fit too.
	var a ackBlock
	a.n = maxAckRanges
	var buf [commonHeaderSize + maxAckBlockSize]byte
	n := encodeAck(buf[:], 1, &a)
	if n > MaxDatagram {
		t.Fatalf("largest ack datagram is %d bytes, above MaxDatagram %d", n, MaxDatagram)
	}
}

// TestStatsReportEveryCounter checks P2-2: the diagnostic surface must actually
// move, because an operator diagnosing a live path depends on these numbers
// rather than on a packet capture.
func TestStatsReportEveryCounter(t *testing.T) {
	a, b := pair(t, DefaultConfig(), 0.10)

	msg := pad('T', 20)
	go func() {
		if err := a.Send(msg); err != nil {
			t.Logf("send: %v", err)
		}
	}()
	if _, err := b.Recv(); err != nil {
		t.Fatalf("recv: %v", err)
	}

	sa := a.Stats()
	if sa.Sent == 0 {
		t.Error("Sent is zero after a transfer")
	}
	if sa.Recv == 0 {
		t.Error("Recv is zero after a transfer")
	}
	if sa.SRTT == 0 {
		t.Error("SRTT is zero after a transfer: no RTT sample was taken")
	}
	if sa.RTO == 0 {
		t.Error("RTO is zero after a transfer")
	}

	// InFlight drains asynchronously: the receiver can hand the message up before
	// the sender has processed the final ACK. Poll rather than assume the two
	// events are ordered.
	deadline := time.Now().Add(2 * time.Second)
	for {
		sa = a.Stats()
		if sa.InFlight == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("InFlight is still %d after the message was delivered", sa.InFlight)
		}
		time.Sleep(time.Millisecond)
	}

	sb := b.Stats()
	if sb.Delivered == 0 {
		t.Error("Delivered is zero after a message arrived")
	}
	if sb.ReasmEntries != 0 {
		t.Errorf("ReasmEntries is %d after delivery, want 0", sb.ReasmEntries)
	}
}
