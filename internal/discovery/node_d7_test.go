// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

// D-7 and the endpoint-oscillation fix (issue #8): endpoints are
// ANNOUNCEMENT state, carried by the member's real listener, and neither a
// stream detach nor an empty announce from a second announcer may wipe them.

import (
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

func d7Fixture(t *testing.T) (*identity.Account, *identity.Device, *identity.MemberList, *Node) {
	t.Helper()
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
	return acc, dev, members, NewNode(members)
}

// TestEmptyAnnounceKeepsEndpoints pins the D-7 server-side guard: a presence
// -only (empty) announce from a second announcer must refresh liveness but
// never blank the endpoints a real listener announced.
func TestEmptyAnnounceKeepsEndpoints(t *testing.T) {
	_, dev, members, node := d7Fixture(t)
	now := time.Now()

	if changed := node.Register(MemberAnnounce{DeviceID: dev.ID(), Endpoints: []string{"10.0.0.5:7000"}, Timestamp: now}, now); !changed {
		t.Fatal("first endpoints announce must count as changed")
	}

	// The one-shot client's presence-only announce (empty endpoints).
	if changed := node.Register(MemberAnnounce{DeviceID: dev.ID(), Timestamp: now.Add(time.Second)}, now.Add(time.Second)); changed {
		t.Fatal("empty announce must not signal an endpoint change")
	}
	if got := node.Endpoints(dev.ID()); len(got) != 1 || got[0] != "10.0.0.5:7000" {
		t.Fatalf("empty announce wiped endpoints: %v", got)
	}

	// Liveness still refreshed by the empty announce.
	if snap := BuildSnapshot(members, node, now.Add(time.Second)); !snap.Members[dev.ID()].Online {
		t.Fatal("empty announce must refresh liveness")
	}

	// A real endpoint update still applies.
	if changed := node.Register(MemberAnnounce{DeviceID: dev.ID(), Endpoints: []string{"10.0.0.6:7000"}, Timestamp: now.Add(2 * time.Second)}, now.Add(2*time.Second)); !changed {
		t.Fatal("endpoint change must count as changed")
	}
	if got := node.Endpoints(dev.ID()); len(got) != 1 || got[0] != "10.0.0.6:7000" {
		t.Fatalf("endpoint update lost: %v", got)
	}
}

// TestDetachStreamKeepsEndpoints pins the oscillation fix: the stream closing
// expires REACHABILITY, not the announced endpoints — they remain dial
// candidates while the member's UDP listener is alive.
func TestDetachStreamKeepsEndpoints(t *testing.T) {
	_, dev, members, node := d7Fixture(t)
	now := time.Now()

	node.Register(MemberAnnounce{DeviceID: dev.ID(), Endpoints: []string{"10.0.0.5:7000"}, Timestamp: now}, now)
	node.AttachStream(dev.ID())
	node.DetachStream(dev.ID())

	if snap := BuildSnapshot(members, node, now.Add(time.Second)); snap.Members[dev.ID()].Online {
		t.Fatal("detach must expire reachability")
	}
	snap := BuildSnapshot(members, node, now.Add(time.Second))
	if got := snap.Members[dev.ID()].Endpoints; len(got) != 1 || got[0] != "10.0.0.5:7000" {
		t.Fatalf("detach must keep announced endpoints for dial-through: %v", got)
	}
}
