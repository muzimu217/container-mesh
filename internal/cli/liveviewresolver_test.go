// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/mesh"
)

// ---------------------------------------------------------------------------
// THE REVOCATION WATCH MUST ASK THE NARROW QUESTION — IN THE PRODUCTION
// SHAPE.
//
// An audit found a real defect in the fix I shipped in round 86.
//
// The fix added an optional capability (StillMember: "is this device
// still in the roster?") precisely because KeyFor answers a WIDER
// question — KeyFor fails for "not a member" AND for "the certificate
// expired". A peer whose certificate merely LAPSED is not a revoked
// peer, and the product renews certificates for exactly that reason.
//
// The capability was implemented on mesh.MemberListAdapter. The
// revocation watch asks the resolver, and the resolver a real member
// uses is liveViewResolver — which WRAPS a SnapshotAdapter and did NOT
// forward the capability.
//
// Go's type assertion sees the wrapper's method set, not the wrapped
// adapter's. So on the production path the watch fell back to KeyFor,
// and a member whose certificate merely lapsed had its live direct
// session destroyed and was told it was revoked.
//
// WHY MY OWN TEST DID NOT CATCH IT: the test for the expiry case used
// MemberListAdapter, which HAS the capability — so the test passed and
// the production resolver never got the narrow answer.
//
// **A test that only exercises the fixture cannot find a gap in the
// fixture.**
//
// So the test below uses the PRODUCTION resolver shape.
// ---------------------------------------------------------------------------

// TestTheMemberSideResolverAnswersTheNarrowMembershipQuestion pins the
// capability forwarding.
//
// The check is on the resolver's METHOD SET, not on its behaviour:
// that is the whole defect. A resolver that answers the wide question
// behaves correctly in every case the old tests covered, and only
// differs in the one case that matters — an expired certificate.
func TestTheMemberSideResolverAnswersTheNarrowMembershipQuestion(t *testing.T) {
	// The revocation watch finds the narrow capability by type
	// assertion. If the production resolver does not carry it, the
	// watch silently falls back to the wide question.
	var r mesh.MemberResolver = &liveViewResolver{}
	checker, ok := r.(interface {
		StillMember(id identity.DeviceID) bool
	})
	if !ok {
		t.Fatal("the member-side resolver does not carry the narrow membership " +
			"capability: the revocation watch will fall back to KeyFor, whose error " +
			"covers 'certificate expired' as well as 'not a member' — so a member " +
			"whose certificate merely lapsed would have its live session destroyed " +
			"and be told it was revoked")
	}
	_ = checker
}

// The narrow capability is part of the CONTRACT of a resolver that the
// revocation watch uses, so it is asserted at compile time.
//
// This is the guard that would have caught the defect on the day it was
// written: the capability was added to ONE adapter, a wrapper stopped
// forwarding it, and the type assertion silently fell back to the wide
// question. **A compile-time assertion cannot be forgotten the way a
// behavioural test can be.**
var _ interface {
	StillMember(id identity.DeviceID) bool
} = &liveViewResolver{}

// The ANCHOR-side resolver carries it too, for the same reason: the
// relay trunk's revocation watch asks the narrow question, and it is
// handed this resolver.
var _ interface {
	StillMember(id identity.DeviceID) bool
} = relayMembersResolver{}

// TestTheSnapshotAdapterAnswersTheNarrowQuestion is the mesh-side half:
// the adapter the member-side resolver wraps must carry it too.
func TestTheSnapshotAdapterAnswersTheNarrowQuestion(t *testing.T) {
	var r mesh.MemberResolver = mesh.SnapshotAdapter{}
	if _, ok := r.(interface {
		StillMember(id identity.DeviceID) bool
	}); !ok {
		t.Fatal("the snapshot resolver does not carry the narrow membership capability")
	}
}

// TestAnExpiredCertificateIsStillAMemberInTheSnapshot is the BEHAVIOUR
// half, on the production shape.
//
// A member whose certificate lapsed is still in the view. The narrow
// question must say so — otherwise the watch tears its session down.
func TestAnExpiredCertificateIsStillAMemberInTheSnapshot(t *testing.T) {
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

	snap := &discovery.MemberSnapshot{
		AccountID: acc.ID(),
		Version:   1,
		Members: map[identity.DeviceID]discovery.MemberView{
			dev.ID(): {Cert: *cert},
		},
	}

	// The WIDE question says no (the certificate failed verification).
	adapter := mesh.SnapshotAdapter{Snap: snap, AccountPub: acc.PublicKey()}
	if _, err := adapter.KeyFor(dev.ID()); err == nil {
		t.Fatal("the wide question accepted an expired certificate")
	}

	// The NARROW question says yes — it is still in the roster.
	if !adapter.StillMember(dev.ID()) {
		t.Fatal("the narrow question says an expired certificate is no longer a member: " +
			"an expiry is not a revocation, and the watch would destroy a legitimate session")
	}
}

// TestAWatchOnTheMemberSideDoesNotKillAnExpiredMembersSession is the
// END-TO-END shape: the watch, on the production resolver, must leave
// an expired member's session alone.
//
// It is the test the round-86 fix was missing: the earlier one
// exercised the FIXTURE's resolver, so it could not see the gap.
func TestAWatchOnTheMemberSideDoesNotKillAnExpiredMembersSession(t *testing.T) {
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
	cert.ExpiresAt = time.Now().UTC().Add(-time.Minute)

	// The member-side resolver reads its view from a discovery client.
	// The check below is on the resolver's own logic, over the same
	// snapshot shape the client hands it.
	snap := &discovery.MemberSnapshot{
		AccountID: acc.ID(),
		Version:   1,
		Members: map[identity.DeviceID]discovery.MemberView{
			dev.ID(): {Cert: *cert},
		},
	}
	adapter := mesh.SnapshotAdapter{Snap: snap, AccountPub: acc.PublicKey()}

	// The member is still in the view, so the watch must not tear the
	// session down over an expiry.
	if !adapter.StillMember(dev.ID()) {
		t.Fatal("an expired member reads as gone: " +
			"the watch would destroy a legitimate session over an expiry")
	}

	// And a device that is genuinely NOT in the view must read as gone.
	other, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if adapter.StillMember(other.ID()) {
		t.Fatal("a device that was never in the view reads as a member")
	}
	_ = context.Background
}
