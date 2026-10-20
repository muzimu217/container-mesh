// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"bytes"
	"testing"
)

// testReceiver drives the receive path directly, with no sockets and no
// goroutines. The reassembly bugs this package exists to prevent are all
// deterministic, so they must be tested deterministically: a probabilistic
// socket test can only say "did not happen this run".
type testReceiver struct {
	r         *receiver
	seq       uint32
	delivered [][]byte
}

func newTestReceiver(t *testing.T, cfg Config) *testReceiver {
	t.Helper()
	cfg.fill()
	tr := &testReceiver{}
	tr.r = newReceiver(1, cfg.RecvWindow, cfg.MaxReasmBytes, func(m []byte) {
		tr.delivered = append(tr.delivered, m)
	})
	return tr
}

// fragments splits msg into packets carrying msgID. The returned packets are in
// fragment order, each with a fresh sequence number, exactly as Send would
// produce them.
func (tr *testReceiver) fragments(msgID uint32, msg []byte) []packet {
	fragCount := (len(msg) + MaxPayload - 1) / MaxPayload
	out := make([]packet, 0, fragCount)
	for i := 0; i < fragCount; i++ {
		lo := i * MaxPayload
		hi := lo + MaxPayload
		if hi > len(msg) {
			hi = len(msg)
		}
		data := makeDataPacket(1, tr.seq, msgID, uint32(i), uint32(fragCount), uint32(len(msg)), msg[lo:hi])
		tr.seq++
		p, err := decodePacket(data)
		if err != nil {
			panic(err)
		}
		out = append(out, p)
	}
	return out
}

// feed delivers one packet and fails the test if the receive path rejects it.
func (tr *testReceiver) feed(t *testing.T, p packet) {
	t.Helper()
	if _, err := tr.r.handleData(p); err != nil {
		t.Fatalf("handleData: %v", err)
	}
}

// feedAll delivers packets in the given order.
func (tr *testReceiver) feedAll(t *testing.T, ps []packet) {
	t.Helper()
	for _, p := range ps {
		tr.feed(t, p)
	}
}

// entries reports the current reassembly table size, for the memory-bound test.
func (tr *testReceiver) entries() int {
	_, _, n, _ := tr.r.stats()
	return n
}

// pad builds a message of fragCount fragments: full fragments except the last.
func pad(marker byte, fragCount int) []byte {
	size := (fragCount-1)*MaxPayload + 8
	b := make([]byte, size)
	for i := range b {
		b[i] = marker
	}
	return b
}

// TestNoCrossMessageAssembly is the core regression.
//
// The legacy layer, given message 8 missing fragment 2 and then all of message
// 9, delivered "B0B1B2B3" as message 8: fragments of 9 filled the hole in 8 and
// len(map)==total then declared 8 complete. Here message 9 must sit in its own
// reassembly slot and wait for 8 to finish, and the bytes handed upwards must
// never be a mixture of the two.
func TestNoCrossMessageAssembly(t *testing.T) {
	tr := newTestReceiver(t, DefaultConfig())
	// Pretend messages 0..7 were already delivered: this is the reported
	// scenario (message 8 is the one that loses a fragment).
	tr.r.nextMsgID = 8

	msgA := pad('A', 4)
	msgB := pad('B', 4)
	fragsA := tr.fragments(8, msgA)
	fragsB := tr.fragments(9, msgB)

	// Message 8 arrives without fragment 2.
	tr.feed(t, fragsA[0])
	tr.feed(t, fragsA[1])
	tr.feed(t, fragsA[3])
	if len(tr.delivered) != 0 {
		t.Fatalf("delivered %q before message 8 was complete", desc(tr.delivered[0]))
	}

	// Message 9 arrives in full while message 8 still has a hole.
	tr.feedAll(t, fragsB)
	if len(tr.delivered) != 0 {
		t.Fatalf("delivered %q while message 8 was incomplete: message 9 was "+
			"delivered out of order", desc(tr.delivered[0]))
	}

	// The missing fragment closes the gap; both messages must come out in
	// order, byte for byte.
	tr.feed(t, fragsA[2])

	if len(tr.delivered) != 2 {
		t.Fatalf("delivered %d messages, want 2", len(tr.delivered))
	}
	if !bytes.Equal(tr.delivered[0], msgA) {
		t.Fatalf("first delivery is %q..., want message 8 (A): this is the "+
			"cross-message assembly bug", desc(tr.delivered[0]))
	}
	if !bytes.Equal(tr.delivered[1], msgB) {
		t.Fatalf("second delivery is %q..., want message 9 (B)", desc(tr.delivered[1]))
	}
}

// TestShortMessageCannotCompleteStaleMap covers the second reported failure
// shape: message 8 (4 fragments) has only 0 and 1, then message 9 (2
// fragments) arrives complete. The legacy layer delivered "B0A1" — a message
// stitched from two messages because recvTotal was a single field that the
// newer, shorter message overwrote.
func TestShortMessageCannotCompleteStaleMap(t *testing.T) {
	tr := newTestReceiver(t, DefaultConfig())
	tr.r.nextMsgID = 8

	long := pad('A', 4)
	short := pad('B', 2)
	fragsLong := tr.fragments(8, long)
	fragsShort := tr.fragments(9, short)

	tr.feed(t, fragsLong[0])
	tr.feed(t, fragsLong[1])
	tr.feedAll(t, fragsShort)

	if len(tr.delivered) != 0 {
		t.Fatalf("delivered %q (len %d) while message 8 was missing two "+
			"fragments: a short message completed a stale map",
			desc(tr.delivered[0]), len(tr.delivered[0]))
	}

	// Completing message 8 must deliver 8 and then 9, both intact.
	tr.feed(t, fragsLong[2])
	tr.feed(t, fragsLong[3])

	if len(tr.delivered) != 2 {
		t.Fatalf("delivered %d messages, want 2", len(tr.delivered))
	}
	if !bytes.Equal(tr.delivered[0], long) {
		t.Fatalf("first delivery is %q..., want message 8", desc(tr.delivered[0]))
	}
	if !bytes.Equal(tr.delivered[1], short) {
		t.Fatalf("second delivery is %q... (len %d), want message 9 (len %d)",
			desc(tr.delivered[1]), len(tr.delivered[1]), len(short))
	}
}

// TestStaleDuplicateDoesNotPoisonNext replays a fragment of an already
// delivered message between the fragments of the next one. The legacy layer
// produced "A0B1" here: the stale fragment overwrote the new message's first
// slot because the map was keyed by fragment index alone.
func TestStaleDuplicateDoesNotPoisonNext(t *testing.T) {
	tr := newTestReceiver(t, DefaultConfig())

	msgA := pad('A', 2)
	msgB := pad('B', 2)
	fragsA := tr.fragments(0, msgA)
	fragsB := tr.fragments(1, msgB)

	tr.feedAll(t, fragsA)
	if len(tr.delivered) != 1 || !bytes.Equal(tr.delivered[0], msgA) {
		t.Fatalf("setup: message A was not delivered intact")
	}

	// Replay A's first fragment, then send B.
	tr.feed(t, fragsA[0])
	tr.feed(t, fragsB[0])
	tr.feed(t, fragsB[1])

	if len(tr.delivered) != 2 {
		t.Fatalf("delivered %d messages, want 2", len(tr.delivered))
	}
	if !bytes.Equal(tr.delivered[1], msgB) {
		t.Fatalf("message B is %q... (len %d), want all-B (len %d): a stale "+
			"fragment poisoned the next message",
			desc(tr.delivered[1]), len(tr.delivered[1]), len(msgB))
	}
}

// TestWholeMessageDeliveredOnce replays every fragment of a delivered message.
// Without a delivery watermark the message would be handed up twice, which at
// the layer above means an AEAD counter mismatch and a dropped session.
func TestWholeMessageDeliveredOnce(t *testing.T) {
	tr := newTestReceiver(t, DefaultConfig())

	msg := pad('X', 4)
	frags := tr.fragments(0, msg)

	tr.feedAll(t, frags)
	if len(tr.delivered) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(tr.delivered))
	}

	// Full replay, twice, in and out of order.
	tr.feedAll(t, frags)
	for i := len(frags) - 1; i >= 0; i-- {
		tr.feed(t, frags[i])
	}
	if len(tr.delivered) != 1 {
		t.Fatalf("delivered %d messages after replaying the whole message, "+
			"want 1: duplicates must be suppressed", len(tr.delivered))
	}
}

// TestFragCountMismatchRejected sends a fragment with the same msgID but a
// different fragCount. Accepting it would let a peer redefine the shape of an
// existing reassembly slot, which is how a mixture of two messages becomes
// indistinguishable from one.
func TestFragCountMismatchRejected(t *testing.T) {
	tr := newTestReceiver(t, DefaultConfig())

	msg := pad('A', 4)
	frags := tr.fragments(0, msg)
	tr.feed(t, frags[0])
	tr.feed(t, frags[1])

	// Same msgID, half the fragment count, consistent-looking body.
	impostor := tr.fragments(0, pad('B', 2))

	tr.feedAll(t, impostor)

	tr.r.mu.Lock()
	ra := tr.r.reasm[0]
	gotCount, gotLen := uint32(0), uint32(0)
	if ra != nil {
		gotCount, gotLen = ra.fragCount, ra.msgLen
	}
	tr.r.mu.Unlock()

	if ra == nil {
		t.Fatal("reassembly slot for msgID 0 disappeared")
	}
	if gotCount != 4 {
		t.Errorf("fragCount became %d, want 4: the impostor redefined the slot", gotCount)
	}
	if gotLen != uint32(len(msg)) {
		t.Errorf("msgLen became %d, want %d", gotLen, len(msg))
	}
	if len(tr.delivered) != 0 {
		t.Fatalf("delivered %q, want nothing", desc(tr.delivered[0]))
	}

	// The original message must still complete correctly afterwards.
	tr.feed(t, frags[2])
	tr.feed(t, frags[3])
	if len(tr.delivered) != 1 || !bytes.Equal(tr.delivered[0], msg) {
		t.Fatal("the original message did not survive the mismatched fragment")
	}
}

// TestReassemblyMemoryBounded floods the receiver with msgIDs far outside the
// window. The reassembly table must not grow with what the peer sends: that is
// the difference between a bounded implementation and one a single hostile
// datagram can use to exhaust memory.
func TestReassemblyMemoryBounded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RecvWindow = 64
	tr := newTestReceiver(t, cfg)

	// Well beyond the window, in ascending order.
	for id := uint32(0); id < 5000; id++ {
		msg := pad('Z', 2)
		frags := tr.fragments(id, msg)
		tr.feed(t, frags[0])
	}

	if n := tr.entries(); n > cfg.RecvWindow {
		t.Fatalf("reassembly table holds %d entries, want <= %d (RecvWindow)",
			n, cfg.RecvWindow)
	}
	if n := tr.entries(); n != cfg.RecvWindow {
		// Exactly the window is expected: msgIDs 0..63 are all inside it and
		// each got one fragment.
		t.Logf("reassembly table holds %d entries (window %d)", n, cfg.RecvWindow)
	}
	if got := len(tr.delivered); got != 0 {
		t.Fatalf("delivered %d messages from single-fragment partials, want 0", got)
	}

	// The byte cap is the second bound; with 64 entries of 8 bytes plus
	// overhead it must also hold.
	_, _, _, bytes := tr.r.stats()
	if bytes > cfg.MaxReasmBytes {
		t.Fatalf("reassembly holds %d bytes, cap is %d", bytes, cfg.MaxReasmBytes)
	}
}

// TestAckRangesTrackHoles checks the acknowledgement state that drives fast
// retransmit: a lost sequence number must leave a gap between reported runs,
// while everything after it is reported as received.
//
// The encoding reports runs from the highest downward, so a single hole in the
// middle of a message produces exactly two runs: the tail above the hole and the
// contiguous prefix below it. A cumulative-only ACK could report neither without
// waiting for the hole to fill.
func TestAckRangesTrackHoles(t *testing.T) {
	msg := pad('S', 10)

	// Lose fragment 3 (sequence 3) by never feeding it.
	tr := newTestReceiver(t, DefaultConfig())
	frags := tr.fragments(0, msg)
	for i, p := range frags {
		if i == 3 {
			continue
		}
		tr.feed(t, p)
	}

	a := tr.r.ackSnapshot()
	if a.una != 3 {
		t.Errorf("una = %d, want 3 (the first missing sequence)", a.una)
	}
	// Sequences 4..9 arrived, so there must be one run covering them.
	if a.n != 1 {
		t.Fatalf("reported %d runs, want 1 (seq 4..9)", a.n)
	}
	first := a.una + uint32(a.ranges[0].gap)
	last := first + uint32(a.ranges[0].length)
	if first != 4 || last != 9 {
		t.Errorf("run covers %d..%d, want 4..9", first, last)
	}
	for seq := uint32(4); seq <= 9; seq++ {
		if !a.contains(seq) {
			t.Errorf("ack does not report seq %d as received", seq)
		}
	}
	if a.contains(3) {
		t.Error("ack reports the lost sequence 3 as received")
	}

	// A datagram arriving in the hole advances una over the contiguous run and
	// collapses the report to a single empty block.
	tr.feed(t, frags[3])
	a2 := tr.r.ackSnapshot()
	if a2.una != 10 {
		t.Errorf("after filling the hole una = %d, want 10", a2.una)
	}
	if a2.n != 0 {
		t.Errorf("after filling the hole %d runs are still reported, want 0", a2.n)
	}
	if len(tr.delivered) != 1 || !bytes.Equal(tr.delivered[0], msg) {
		t.Error("filling the hole did not complete the message")
	}
}
