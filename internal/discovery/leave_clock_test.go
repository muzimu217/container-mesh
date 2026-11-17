// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery_test

import (
	"context"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
)

// TestDepartureSurvivesAClockThatWentBackwards pins that a member whose
// clock jumped backwards still gets its departure honoured.
//
// The departure names the presence session it ends, and the node ignores a
// departure that names an OLDER session. The session reference is the
// timestamp of the member's own announce.
//
// The node's reference only moves FORWARD: a replayed old announce must
// not roll it back, or a legitimate departure would look stale. A member's
// clock can jump backwards between two announces, and then the member's own
// last announce carries a SMALLER timestamp than the one the node already
// recorded.
//
// If the member stored its last announce's raw timestamp, the two sides
// would disagree: the node holds the newer value, the departure names the
// older one, and the node reads that as a departure from a session that is
// no longer current — and ignores it. The member would then stay "online"
// in everyone's view until the 90 s presence timeout, which is exactly the
// budget the departure exists to beat.
//
// So both sides keep the LARGEST timestamp they have seen, and the
// departure names that.
//
// The test drives the real client (not a hand-built announce), because the
// client's own bookkeeping is half of the rule.
func TestDepartureSurvivesAClockThatWentBackwards(t *testing.T) {
	s := newStack(t, 2)
	peer := s.clientFor(t, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = peer.Run(ctx) }()
	waitFor(t, 5*time.Second, "peer snapshot", func() bool { return peer.Snapshot() != nil })

	leaver := s.devices[0]

	// The member announces at T, under a normal clock.
	announced := time.Now()
	postAnnounce(t, s, leaver, discovery.MemberAnnounce{
		DeviceID:  leaver.ID(),
		Endpoints: []string{"8.8.8.5:7000"},
		Timestamp: announced,
	})
	waitView(t, peer, leaver.ID(), true, 5*time.Second, "leaver visible online")

	// Its clock jumps backwards: the next announce carries a timestamp
	// EARLIER than the one the node already recorded. The node keeps the
	// newer reference (that is the point of "only forward"), so the two
	// sides would disagree — unless the member keeps the larger value too.
	rewound := announced.Add(-2 * time.Second)
	postAnnounce(t, s, leaver, discovery.MemberAnnounce{
		DeviceID:  leaver.ID(),
		Endpoints: []string{"8.8.8.5:7000"},
		Timestamp: rewound,
	})

	// The departure names the session the member believes it is in. The
	// member's own bookkeeping decides that value — it must be the larger
	// one, the same as the node's.
	start := time.Now()
	leave := discovery.MemberAnnounce{DeviceID: leaver.ID(), Timestamp: time.Now()}
	leave.Leave = true
	leave.Session = announced.UnixNano()
	postAnnounce(t, s, leaver, leave)

	waitView(t, peer, leaver.ID(), false, leaveBudget,
		"leaver offline after a departure under a rewound clock")
	t.Logf("converged in %s", time.Since(start))
}

// TestDepartureForAnOlderSessionIsStillIgnored keeps the other half of the
// rule: a departure that names a session OLDER than the one the node is
// currently in must be ignored, because the member has reconnected.
//
// This is what the session reference exists for: without it, a departure
// that was in flight when the member reconnected would take the NEW
// connection offline, reporting a live member as gone.
func TestDepartureForAnOlderSessionIsStillIgnored(t *testing.T) {
	s := newStack(t, 2)
	peer := s.clientFor(t, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = peer.Run(ctx) }()
	waitFor(t, 5*time.Second, "peer snapshot", func() bool { return peer.Snapshot() != nil })

	leaver := s.devices[0]
	first := time.Now()
	postAnnounce(t, s, leaver, discovery.MemberAnnounce{
		DeviceID:  leaver.ID(),
		Endpoints: []string{"8.8.8.5:7000"},
		Timestamp: first,
	})
	waitView(t, peer, leaver.ID(), true, 5*time.Second, "leaver visible online")

	// The member reconnects: a LATER announce, so the node's reference
	// moves forward.
	second := first.Add(time.Second)
	postAnnounce(t, s, leaver, discovery.MemberAnnounce{
		DeviceID:  leaver.ID(),
		Endpoints: []string{"8.8.8.5:7000"},
		Timestamp: second,
	})

	// A departure that names the OLD session arrives late. It must be
	// ignored: the member is in the newer session and is still there.
	leave := discovery.MemberAnnounce{DeviceID: leaver.ID(), Timestamp: time.Now()}
	leave.Leave = true
	leave.Session = first.UnixNano()
	postAnnounce(t, s, leaver, leave)

	// The member must still be online. A stale departure changes nothing,
	// so there is NO view update to wait for — the snapshot is polled
	// directly. (Waiting on the update channel would hang: the point of
	// the rule is that nothing happens.)
	time.Sleep(500 * time.Millisecond)
	snap := peer.Snapshot()
	if snap == nil {
		t.Fatal("the peer has no snapshot")
	}
	if !memberOnline(snap, leaver.ID()) {
		t.Fatal("the member was taken offline by a stale departure")
	}
}
