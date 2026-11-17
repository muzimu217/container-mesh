// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

// P-5b: the explicit LEAVE event (graceful departure). Node-side semantics:
// a LEAVE ends the CURRENT presence session, is idempotent, never touches a
// newer session, and cannot be forged by a non-member.

import (
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestRegularAnnounceSignableBytesUnchanged pins the wire compatibility of
// the LEAVE extension (C-4: no version bump, no broken signatures).
//
// A NORMAL announce must keep the exact signable bytes it had before the
// LEAVE fields existed, so an anchor running the older code still verifies a
// new client's announces — and a new anchor still verifies an old client's.
// Only the LEAVE message itself is new; an old anchor rejects it and the
// client falls back to the transport-level path.
func TestRegularAnnounceSignableBytesUnchanged(t *testing.T) {
	a := MemberAnnounce{
		DeviceID:  identity.DeviceID("ABCDEFGHIJKLMNOP"),
		Endpoints: []string{"1.2.3.4:5555"},
		Timestamp: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
	}
	want := `{"device_id":"ABCDEFGHIJKLMNOP","endpoints":["1.2.3.4:5555"],"timestamp":"2026-11-17T10:00:00Z"}`
	if got := string(a.signable()); got != want {
		t.Fatalf("regular announce signable bytes changed:\n got %s\nwant %s", got, want)
	}
}

// TestLeaveFieldsAreSigned proves the LEAVE fields are covered by the
// signature: flipping them must change the signed bytes, otherwise a
// captured announce could be turned into a departure by anyone in the path.
func TestLeaveFieldsAreSigned(t *testing.T) {
	base := MemberAnnounce{
		DeviceID:  identity.DeviceID("ABCDEFGHIJKLMNOP"),
		Endpoints: []string{"1.2.3.4:5555"},
		Timestamp: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
	}
	leave := base
	leave.Leave = true
	leave.Session = 1763385600000000000
	leave.View = 7
	if string(leave.signable()) == string(base.signable()) {
		t.Fatal("LEAVE fields are not covered by the signature")
	}
}

// TestNodeLeaveMarksMemberOffline is the node-side core of P-5b: an explicit
// departure must end the presence immediately and wake every subscriber so
// the anchor pushes a fresh view.
func TestNodeLeaveMarksMemberOffline(t *testing.T) {
	_, dev, _, node := d7Fixture(t)
	now := time.Now()
	node.Register(MemberAnnounce{DeviceID: dev.ID(), Endpoints: []string{"8.8.8.5:7000"}, Timestamp: now}, now)
	if _, online := node.LastSeen(dev.ID()); !online {
		t.Fatal("device should be online after its announce")
	}

	ch, unsub := node.Subscribe(dev.ID())
	defer unsub()

	leave := MemberAnnounce{DeviceID: dev.ID(), Timestamp: now.Add(time.Second)}
	leave.Leave = true
	leave.Session = now.UnixNano()
	if !node.Register(leave, now.Add(time.Second)) {
		t.Fatal("a LEAVE that ends a live presence must report a change")
	}
	if _, online := node.LastSeen(dev.ID()); online {
		t.Fatal("device must be offline right after the LEAVE")
	}
	// The announced endpoints SURVIVE the departure, exactly like a stream
	// detach: they are what the member's listener last advertised, and
	// blanking them would make an offline member undialable for a whole
	// heartbeat (issue #8 endpoint-oscillation fix).
	if eps := node.Endpoints(dev.ID()); len(eps) != 1 || eps[0] != "8.8.8.5:7000" {
		t.Fatalf("LEAVE must not blank the announced endpoints, got %v", eps)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("LEAVE did not notify the subscribers (no view would be pushed)")
	}
}

// TestNodeLeaveIsIdempotent: a repeated LEAVE must be a harmless no-op —
// the departure may be broadcast more than once (retry, duplicate signal,
// duplicated client), and it must not error, flap or wipe state.
func TestNodeLeaveIsIdempotent(t *testing.T) {
	_, dev, _, node := d7Fixture(t)
	now := time.Now()
	node.Register(MemberAnnounce{DeviceID: dev.ID(), Timestamp: now}, now)

	leave := MemberAnnounce{DeviceID: dev.ID(), Timestamp: now.Add(time.Second)}
	leave.Leave = true
	leave.Session = now.UnixNano()
	if !node.Register(leave, now.Add(time.Second)) {
		t.Fatal("the first LEAVE must end the presence")
	}
	if node.Register(leave, now.Add(2*time.Second)) {
		t.Fatal("a repeated LEAVE must report no change")
	}
	if _, online := node.LastSeen(dev.ID()); online {
		t.Fatal("device must stay offline after a repeated LEAVE")
	}
}

// TestNodeIgnoresLeaveFromOlderSession is the reconnect race: a LEAVE that
// belongs to a presence session the member already left (it reconnected and
// announced a newer session) must NOT mark the new connection offline.
func TestNodeIgnoresLeaveFromOlderSession(t *testing.T) {
	_, dev, _, node := d7Fixture(t)
	first := time.Now()
	node.Register(MemberAnnounce{DeviceID: dev.ID(), Timestamp: first}, first)

	// The member reconnects and announces a NEWER presence session.
	second := first.Add(time.Second)
	node.Register(MemberAnnounce{DeviceID: dev.ID(), Timestamp: second}, second)

	// A late LEAVE from the OLD session arrives.
	late := MemberAnnounce{DeviceID: dev.ID(), Timestamp: second.Add(time.Second)}
	late.Leave = true
	late.Session = first.UnixNano()
	if node.Register(late, second.Add(time.Second)) {
		t.Fatal("a LEAVE from an older session must be ignored")
	}
	if _, online := node.LastSeen(dev.ID()); !online {
		t.Fatal("the reconnected member must stay online")
	}
}

// TestNodeLeaveRefusesNonMember keeps the same structural backstop the
// announce path has: a stranger must not be able to talk a member offline.
func TestNodeLeaveRefusesNonMember(t *testing.T) {
	_, _, _, node := d7Fixture(t)
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	leave := MemberAnnounce{DeviceID: stranger.ID(), Timestamp: time.Now()}
	leave.Leave = true
	if node.Register(leave, time.Now()) {
		t.Fatal("a non-member's LEAVE must be refused")
	}
	if _, ok := node.LastSeen(stranger.ID()); ok {
		t.Fatal("a non-member must not acquire presence state")
	}
}

// TestNodeLeaveKeepsSessionFresh pins that only the newest announce defines
// the current presence session — a replayed OLD announce must not roll the
// session reference backwards (it would make a legitimate LEAVE look stale).
func TestNodeLeaveKeepsSessionFresh(t *testing.T) {
	_, dev, _, node := d7Fixture(t)
	newer := time.Now()
	older := newer.Add(-2 * time.Second)
	node.Register(MemberAnnounce{DeviceID: dev.ID(), Timestamp: newer}, newer)
	node.Register(MemberAnnounce{DeviceID: dev.ID(), Timestamp: older}, older)

	leave := MemberAnnounce{DeviceID: dev.ID(), Timestamp: newer.Add(time.Second)}
	leave.Leave = true
	leave.Session = newer.UnixNano()
	if !node.Register(leave, newer.Add(time.Second)) {
		t.Fatal("the LEAVE of the newest session must be applied")
	}
}
