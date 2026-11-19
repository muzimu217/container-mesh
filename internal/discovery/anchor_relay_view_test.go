// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery_test

import (
	"context"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestTheAnchorsOwnRelayEndpointsReachTheView pins that the relay
// endpoints an ANCHOR publishes reach the member view.
//
// This is the same-port form (D76): the trunk shares the direct-link
// port, so the anchor's relay endpoints ARE its direct endpoints. A
// requester looks for a trunk by its RELAY endpoints (ViewPicker), so if
// the anchor's own relay endpoints never reach the view, the anchor is
// not a relay candidate and the relay path is unreachable.
//
// The real-machine acceptance of round 62 found exactly this: the anchor
// logged "the relay protocol shares the direct-link port", but the view
// showed no relay endpoints at all, so no requester could ever find the
// trunk.
func TestTheAnchorsOwnRelayEndpointsReachTheView(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	node := discovery.NewNode(members)

	now := time.Now().UTC()
	// The anchor announces the SAME address for both: the trunk shares the
	// direct-link port (D76).
	shared := []string{"175.24.139.148:18090"}
	node.Register(discovery.MemberAnnounce{
		DeviceID:       dev.ID(),
		Endpoints:      shared,
		RelayEndpoints: shared,
		Timestamp:      now,
	}, now)

	snap := discovery.BuildSnapshot(members, node, now)
	mv, ok := snap.Members[dev.ID()]
	if !ok {
		t.Fatal("the anchor is not in the member view")
	}
	if len(mv.RelayEndpoints) != 1 || mv.RelayEndpoints[0] != shared[0] {
		t.Fatalf("the member view carries relay endpoints %v, want %v", mv.RelayEndpoints, shared)
	}
	_ = context.Background()
}

// TestTheRelayEndpointsAloneAreEnoughToTriggerAPush pins that a member
// whose RELAY endpoints appear while its DIRECT endpoints stay the same
// still pushes the new view.
//
// This is the same-port form (D76): the trunk shares the direct-link
// port, so the anchor's direct endpoints never change — its relay
// endpoints appear for the first time when the trunk is switched on.
//
// The change test compared only the direct endpoints, so it said "nothing
// changed", the view was not pushed, and every requester kept a snapshot
// with no relay endpoint. No requester could find the trunk, so the relay
// path was unreachable — while the node's OWN state was already correct.
//
// The real-machine acceptance of round 62 found exactly this, and it is
// the same shape as the bug it fixed: a rule that looked at only part of
// the state.
func TestTheRelayEndpointsAloneAreEnoughToTriggerAPush(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	node := discovery.NewNode(members)

	// A subscriber that is woken on every view change.
	ch, unsub := node.Subscribe(dev.ID())
	defer unsub()

	now := time.Now().UTC()
	direct := []string{"175.24.139.148:18090"}

	// The first announce: direct endpoints only, no trunk yet.
	node.Register(discovery.MemberAnnounce{
		DeviceID:  dev.ID(),
		Endpoints: direct,
		Timestamp: now,
	}, now)
	<-ch

	// The second announce: the SAME direct endpoints, plus the relay
	// endpoints that appear when the trunk is switched on.
	node.Register(discovery.MemberAnnounce{
		DeviceID:       dev.ID(),
		Endpoints:      direct,
		RelayEndpoints: direct,
		Timestamp:      now.Add(time.Second),
	}, now.Add(time.Second))

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("the relay endpoints appeared but no view was pushed: every requester " +
			"kept a snapshot with no relay endpoint, so no requester could find the trunk")
	}
}
