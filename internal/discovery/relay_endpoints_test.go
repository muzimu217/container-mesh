// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery_test

import (
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestAnAnnouncedRelayEndpointReachesTheMemberView pins the relay endpoint
// discovery chain end to end.
//
// A relay session speaks its own ALPN, and one UDP socket can serve only
// one ALPN — so the relay listener is a DIFFERENT port from the direct
// one. A requester that wants to reach a target through a trunk therefore
// has to dial the trunk's relay port.
//
// That address has to travel: the anchor publishes it, the node records it,
// and the member view carries it, because the view is what a requester
// reads when it looks for a trunk to relay through.
//
// Without this, the relay path could not be dialled at all on real
// hardware: the picker would offer the trunk's DIRECT endpoints, and the
// ALPN negotiation would fail.
func TestAnAnnouncedRelayEndpointReachesTheMemberView(t *testing.T) {
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
	relayAddr := "198.51.100.9:7001"
	node.Register(discovery.MemberAnnounce{
		DeviceID:       dev.ID(),
		Endpoints:      []string{"198.51.100.9:7000"},
		RelayEndpoints: []string{relayAddr},
		Timestamp:      now,
	}, now)

	// The node holds it.
	if got := node.RelayEndpoints(dev.ID()); len(got) != 1 || got[0] != relayAddr {
		t.Fatalf("the node holds relay endpoints %v, want [%s]", got, relayAddr)
	}

	// And the member view carries it.
	snap := discovery.BuildSnapshot(members, node, now)
	if snap == nil {
		t.Fatal("no snapshot was built")
	}
	mv, ok := snap.Members[dev.ID()]
	if !ok {
		t.Fatal("the member is not in the snapshot")
	}
	if len(mv.RelayEndpoints) != 1 || mv.RelayEndpoints[0] != relayAddr {
		t.Fatalf("the member view carries relay endpoints %v, want [%s]", mv.RelayEndpoints, relayAddr)
	}
	// The direct endpoints are still separate and untouched.
	if len(mv.Endpoints) != 1 || mv.Endpoints[0] != "198.51.100.9:7000" {
		t.Fatalf("the member view's direct endpoints are %v, want [198.51.100.9:7000]", mv.Endpoints)
	}
}

// TestAMemberWithoutATrunkHasNoRelayEndpoints pins that a plain member
// announces no relay endpoint — the field stays empty, and the picker can
// therefore skip it as a trunk candidate.
func TestAMemberWithoutATrunkHasNoRelayEndpoints(t *testing.T) {
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
	node.Register(discovery.MemberAnnounce{
		DeviceID:  dev.ID(),
		Endpoints: []string{"198.51.100.9:7000"},
		Timestamp: now,
	}, now)

	if got := node.RelayEndpoints(dev.ID()); len(got) != 0 {
		t.Fatalf("a plain member holds relay endpoints %v, want none", got)
	}
	snap := discovery.BuildSnapshot(members, node, now)
	mv := snap.Members[dev.ID()]
	if len(mv.RelayEndpoints) != 0 {
		t.Fatalf("the member view carries relay endpoints %v, want none", mv.RelayEndpoints)
	}
}
