// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"testing"
)

// TestRecvCounterStartsAtZero documents the signal the client uses to detect a
// pairing that never carried data.
//
// The relay pairs on slot presence, so a peer that died while parked can be
// paired with a live one; the splice then fails on its first read and both
// sides are closed. The relay cannot detect this (reading a parked connection
// starves the splice, and a fired read deadline poisons the connection), so
// the client treats "session ended with zero frames received" as "retry now"
// instead of waiting out a backoff.
func TestRecvCounterStartsAtZero(t *testing.T) {
	a, b, _, hb := pair(t, "zero-frame-code")

	// The hello exchange itself is one frame, so the raw counter is not zero;
	// what matters is that nothing beyond it has arrived.
	if got := a.FramesSinceStart(); got != 0 {
		t.Errorf("fresh session FramesSinceStart = %d, want 0", got)
	}
	if got := b.FramesSinceStart(); got != 0 {
		t.Errorf("fresh session FramesSinceStart = %d, want 0", got)
	}

	// After one message the counter must move, so a session that DID carry
	// traffic is not mistaken for a dead pairing.
	if err := a.SendMessage("hello"); err != nil {
		t.Fatal(err)
	}
	waitMsg(t, hb)
	if got := b.FramesSinceStart(); got == 0 {
		t.Error("FramesSinceStart did not advance after a received message")
	}
}
