// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package secure

import (
	"strings"
	"testing"
)

// TestRoomIDDoesNotRevealCode is the core property behind the MITM fix: the
// relay receives the room label, so the label must not be the code.
func TestRoomIDDoesNotRevealCode(t *testing.T) {
	code := "correct-horse-battery"
	room := RoomID(code)

	if strings.Contains(room, NormalizeCode(code)) {
		t.Fatalf("room %q contains the normalized code", room)
	}
	if strings.Contains(room, code) {
		t.Fatalf("room %q contains the raw code", room)
	}
	// Fixed width, hex, opaque.
	if len(room) != 32 {
		t.Errorf("room length = %d, want 32 hex chars", len(room))
	}
	for _, r := range room {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			t.Errorf("room contains non-hex character %q", r)
		}
	}
}

// TestRoomIDIsStableAndNormalized: both sides must land in the same room
// regardless of how they typed the code.
func TestRoomIDIsStableAndNormalized(t *testing.T) {
	variants := []string{
		"Correct Horse Battery",
		"correct-horse-battery",
		"  CORRECT_HORSE_BATTERY  ",
		"correct--horse--battery",
	}
	want := RoomID(variants[0])
	for _, v := range variants[1:] {
		if got := RoomID(v); got != want {
			t.Errorf("RoomID(%q) = %q, want %q", v, got, want)
		}
	}
	if RoomID("different-code") == want {
		t.Error("different codes produced the same room")
	}
}

// TestShortAuthMatchesAcrossPeers is what makes the out-of-band check work:
// both ends must compute the same digits from the shared session key.
func TestShortAuthMatchesAcrossPeers(t *testing.T) {
	a, b := completedPair(t, "sas-check-code")

	sasA, sasB := a.ShortAuth(), b.ShortAuth()
	if sasA != sasB {
		t.Fatalf("SAS differs: A=%s B=%s", sasA, sasB)
	}
	if len(sasA) != 6 {
		t.Errorf("SAS %q is not 6 digits", sasA)
	}
	for _, r := range sasA {
		if r < '0' || r > '9' {
			t.Errorf("SAS %q contains non-digit %q", sasA, r)
		}
	}
	if fpA, fpB := a.Fingerprint(), b.Fingerprint(); fpA != fpB {
		t.Errorf("fingerprint differs: A=%s B=%s", fpA, fpB)
	}
}

// TestShortAuthDiffersForDifferentSessions: the SAS is a per-session value,
// not a constant derived from the code.
func TestShortAuthDiffersForDifferentSessions(t *testing.T) {
	a1, b1 := completedPair(t, "same-code-here")
	a2, b2 := completedPair(t, "same-code-here")

	// Two independent handshakes with the same code must not share a key.
	if a1.Fingerprint() == a2.Fingerprint() {
		t.Error("two sessions with the same code produced the same fingerprint")
	}
	// But within a session both sides agree.
	if a1.ShortAuth() != b1.ShortAuth() || a2.ShortAuth() != b2.ShortAuth() {
		t.Error("SAS mismatch within a session")
	}
}

// TestShortAuthIsStableForSession: repeated calls return the same value (it is
// cached, not re-derived with fresh randomness).
func TestShortAuthIsStableForSession(t *testing.T) {
	a, _ := completedPair(t, "stable-sas-code")
	first := a.ShortAuth()
	for i := 0; i < 5; i++ {
		if got := a.ShortAuth(); got != first {
			t.Fatalf("call %d returned %q, want %q", i, got, first)
		}
	}
}

// completedPair returns two sessions built from a full handshake.
func completedPair(t *testing.T, code string) (*Session, *Session) {
	t.Helper()
	ha, hb := runHandshake(t, code)
	sa, err := NewSession(ha)
	if err != nil {
		t.Fatalf("A session: %v", err)
	}
	sb, err := NewSession(hb)
	if err != nil {
		t.Fatalf("B session: %v", err)
	}
	return sa, sb
}
