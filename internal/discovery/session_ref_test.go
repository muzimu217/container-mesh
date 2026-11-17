// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"sync"
	"testing"
	"time"
)

// TestTheSessionReferenceOnlyMovesForward pins the client's own half of
// the departure rule.
//
// The node keeps the LARGEST announce timestamp it has seen from a member,
// so a replayed old announce cannot roll it back. The client must keep the
// same kind of reference, or the two sides disagree.
//
// If the client stored the raw timestamp of its last announce, a clock that
// jumped backwards would leave the client holding a SMALLER value than the
// node's. The departure would then name the older session and the node
// would ignore it as a departure from a session that is no longer current
// — so the member stayed "online" in everyone's view until the presence
// timeout, which is the budget the departure exists to beat.
func TestTheSessionReferenceOnlyMovesForward(t *testing.T) {
	c := &Client{}
	c.lastAnnounceTS.Store(int64(0))

	// A normal announce: the reference takes its timestamp.
	first := time.Now()
	c.recordSession(first)
	if got := c.lastAnnounceTS.Load(); got != first.UnixNano() {
		t.Fatalf("the session reference = %d, want %d", got, first.UnixNano())
	}

	// A clock that jumps backwards: the reference must NOT go back.
	rewound := first.Add(-2 * time.Second)
	c.recordSession(rewound)
	if got := c.lastAnnounceTS.Load(); got != first.UnixNano() {
		t.Fatalf("the session reference moved BACKWARDS to %d after a rewound clock (it was %d)",
			got, first.UnixNano())
	}

	// A later announce moves it forward again.
	later := first.Add(time.Second)
	c.recordSession(later)
	if got := c.lastAnnounceTS.Load(); got != later.UnixNano() {
		t.Fatalf("the session reference = %d, want %d", got, later.UnixNano())
	}
}

// TestTheSessionReferenceIsRaceFreeUnderConcurrentAnnounces pins that
// concurrent announces cannot lose an update.
//
// The reference is advanced with a compare-and-swap loop, so two announces
// racing must not leave the reference behind the larger of the two.
func TestTheSessionReferenceIsRaceFreeUnderConcurrentAnnounces(t *testing.T) {
	c := &Client{}
	c.lastAnnounceTS.Store(int64(0))

	base := time.Now()
	var wg sync.WaitGroup
	for i := 1; i <= 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c.recordSession(base.Add(time.Duration(n) * time.Millisecond))
		}(i)
	}
	wg.Wait()

	want := base.Add(16 * time.Millisecond).UnixNano()
	if got := c.lastAnnounceTS.Load(); got != want {
		t.Fatalf("the session reference = %d, want the largest %d", got, want)
	}
}
