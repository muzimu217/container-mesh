// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"testing"
)

// Tests for the sackState summary word added in round 15.
//
// The summary exists only to make snapshot cheap in the common case, but it is
// now load-bearing: snapshot returns early when the summary says the ring is
// empty. If the summary ever claims "empty" while a bit is set, an ACK silently
// under-reports what the receiver holds, the sender retransmits data the peer
// already has, and the only symptom is wasted bandwidth — which no other test
// would catch. So the invariant is checked directly.

// TestSackSummaryFitsOneWord pins the assumption the summary field is built on:
// the ring is small enough that a single uint64 describes every word. Raising
// sackRingBits past 4096 would need a second summary level, and this test is the
// thing that would say so instead of the optimisation silently going wrong.
func TestSackSummaryFitsOneWord(t *testing.T) {
	const words = sackRingBits / 64
	if words > 64 {
		t.Fatalf("sackRingBits=%d means %d words, but summary is one uint64: "+
			"add a second summary level", sackRingBits, words)
	}
}

// TestSackSummaryTracksBits checks that summary is exactly the OR of the
// non-empty words, across the operations that can change a word.
func TestSackSummaryTracksBits(t *testing.T) {
	var s sackState
	s = newSackState()

	check := func(step string) {
		t.Helper()
		var want uint64
		for w, word := range s.bits {
			if word != 0 {
				want |= 1 << uint(w)
			}
		}
		if s.summary != want {
			t.Fatalf("%s: summary = %#x, want %#x (derived from bits)", step, s.summary, want)
		}
	}

	check("fresh")

	// Mark a spread of sequences, including ones that land in every word of the
	// ring so the summary has to track several words at once.
	for _, seq := range []uint32{0, 1, 63, 64, 127, 128, 1000, 2000, 4095, 4096, 5000} {
		s.mark(seq)
		check("after mark")
	}

	// Advance the cumulative point across the ring so bits are cleared one word
	// at a time, which is what makes a word's summary bit clear.
	for i := 0; i < 4200; i++ {
		s.mark(s.next)
	}
	check("after draining the ring")
}

// TestSackSnapshotEmptyWhenInOrder is the case the summary optimises: an
// in-order stream leaves no arrivals above the cumulative point, so every
// snapshot is just una.
func TestSackSnapshotEmptyWhenInOrder(t *testing.T) {
	s := newSackState()
	for i := uint32(0); i < 500; i++ {
		s.mark(i)
		if !s.empty() {
			t.Fatalf("after marking seq %d in order, the ring is not empty", i)
		}
		a := s.snapshot()
		if a.n != 0 {
			t.Fatalf("after marking seq %d in order, snapshot reported %d ranges", i, a.n)
		}
		if a.una != i+1 {
			t.Fatalf("after marking seq %d, una = %d, want %d", i, a.una, i+1)
		}
	}
}

// TestSackSnapshotWithHole checks that a single gap is still reported once the
// ring is non-empty: the fast path must not swallow a real out-of-order report.
func TestSackSnapshotWithHole(t *testing.T) {
	s := newSackState()
	s.mark(0) // una advances to 1
	s.mark(2) // 1 is missing
	s.mark(3)

	if s.empty() {
		t.Fatal("ring reports empty with seq 2 and 3 outstanding")
	}
	a := s.snapshot()
	if a.una != 1 {
		t.Fatalf("una = %d, want 1", a.una)
	}
	if a.n != 1 {
		t.Fatalf("snapshot reported %d ranges, want 1", a.n)
	}
	first, last := a.firstOf(0), a.lastOf(0)
	if first != 2 || last != 3 {
		t.Fatalf("range = [%d,%d], want [2,3]", first, last)
	}

	// Filling the hole must empty the ring again, and the summary must follow.
	s.mark(1)
	if !s.empty() {
		t.Fatal("ring is not empty after the hole was filled")
	}
	if a := s.snapshot(); a.n != 0 || a.una != 4 {
		t.Fatalf("after filling the hole: una=%d n=%d, want una=4 n=0", a.una, a.n)
	}
}

// TestSackSnapshotMatchesBruteForce compares snapshot against a direct scan of
// the ring over a pseudo-random arrival pattern. It is the general statement of
// the invariant: the summary must not change what snapshot reports.
func TestSackSnapshotMatchesBruteForce(t *testing.T) {
	s := newSackState()
	// A deterministic LCG, so a failure is reproducible without a seed.
	state := uint32(12345)
	next := func() uint32 {
		state = state*1664525 + 1013904223
		return state
	}

	for step := 0; step < 3000; step++ {
		// Mark a few arrivals, some in order and some ahead, leaving holes.
		for k := 0; k < 3; k++ {
			seq := s.next + next()%64
			s.mark(seq)
		}

		a := s.snapshot()

		// Brute force: the highest set sequence above una is what the block must
		// report, and every set sequence must be inside one of its ranges.
		highest := s.next
		found := false
		for seq := s.next + 1; seq < s.next+sackRingBits; seq++ {
			if s.get(seq) {
				highest = seq
				found = true
			}
		}
		if !found {
			if a.n != 0 {
				t.Fatalf("step %d: no set sequence above una, but snapshot reported %d ranges", step, a.n)
			}
			continue
		}
		if a.n == 0 {
			t.Fatalf("step %d: seq %d is set above una %d but snapshot reported nothing", step, highest, a.una)
		}
		if got := a.lastOf(0); got != highest {
			t.Fatalf("step %d: highest reported %d, want %d", step, got, highest)
		}
		// Every sequence the block claims must actually be set. This is the
		// safety direction: a false positive would let the sender believe data
		// arrived that did not, and it would never retransmit it.
		//
		// The converse (every set sequence is reported) is deliberately not
		// asserted: snapshot stops at maxAckRanges and drops the lowest runs, which
		// costs a slower repair and never a lost datagram (see sackState.snapshot).
		for r := 0; r < a.n; r++ {
			for seq := a.firstOf(r); !seqAfter(seq, a.lastOf(r)); seq++ {
				if !s.get(seq) {
					t.Fatalf("step %d: range %d claims seq %d arrived, but it is not set", step, r, seq)
				}
			}
		}
		// With few enough runs that nothing is truncated, the report must be
		// complete: every set sequence is covered.
		if a.n < maxAckRanges {
			for seq := s.next + 1; seq <= highest; seq++ {
				if s.get(seq) && !a.contains(seq) {
					t.Fatalf("step %d: %d runs (< max %d) but set seq %d is unreported",
						step, a.n, maxAckRanges, seq)
				}
			}
		}
	}
}
