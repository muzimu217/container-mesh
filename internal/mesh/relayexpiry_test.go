// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// ---------------------------------------------------------------------------
// THE TWO PATHS MUST AGREE ON WHAT "REVOKED" MEANS.
//
// The direct path's revocation watch asks the NARROW question ("is this
// device still in the roster?"), so a member whose certificate merely
// LAPSED keeps its direct session — an expiry is a normal, recoverable
// event, and the product renews certificates precisely so that it is.
//
// The relay trunk's watch used to ask the WIDE question (KeyFor), whose
// error also covers "certificate expired". So the SAME member — expired
// certificate, still a member — kept its direct session and had its
// relay flow torn down, with the peer told it was revoked.
//
// Two paths answering the same question differently is a bug in its own
// right: the operator sees "revoked" in one place and "still connected"
// in another, and neither is wrong on its own terms.
//
// So the relay path asks the narrow question too.
// ---------------------------------------------------------------------------

// TestTheRelayWatchDoesNotTearDownAnExpiredMembersFlow is the
// regression test.
//
// The member's certificate has lapsed but it is STILL in the roster.
// The flow must survive: an expiry is not a revocation.
func TestTheRelayWatchDoesNotTearDownAnExpiredMembersFlow(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Backdate it past its expiry.
	cert.ExpiresAt = time.Now().UTC().Add(-time.Minute)

	members := identity.NewMemberList(acc.ID())
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}

	trunk := &RelayTrunk{members: MemberListAdapter{List: members}}

	// The NARROW question says yes — it is still a member.
	if !trunk.stillMember(dev.ID()) {
		t.Fatal("the relay watch says an expired member is gone: " +
			"the flow would be torn down and the peer told it was revoked, " +
			"while the direct path keeps the same member's session")
	}
}

// TestTheRelayWatchTearsDownARevokedMembersFlow is the OTHER half: a
// member that is genuinely NOT in the roster must lose its flow.
//
// A watch that never tears anything down would pass the test above.
func TestTheRelayWatchTearsDownARevokedMembersFlow(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	members := identity.NewMemberList(acc.ID())
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	trunk := &RelayTrunk{members: MemberListAdapter{List: members}}

	// Still a member.
	if !trunk.stillMember(dev.ID()) {
		t.Fatal("a member in the roster reads as gone")
	}

	// Revoked.
	if err := members.Remove(dev.ID()); err != nil {
		t.Fatal(err)
	}
	if trunk.stillMember(dev.ID()) {
		t.Fatal("a revoked member still reads as a member: " +
			"the relay watch would keep carrying its flow")
	}
}
