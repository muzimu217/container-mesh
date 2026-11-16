// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// candidateView builds a member view with the given device IDs. Members
// with an endpoint are dialable; members without one are not.
func candidateView(t *testing.T, withEndpoint, withoutEndpoint []string) *discovery.MemberSnapshot {
	t.Helper()
	snap := &discovery.MemberSnapshot{Members: map[identity.DeviceID]discovery.MemberView{}}
	for _, id := range withEndpoint {
		snap.Members[identity.DeviceID(id)] = discovery.MemberView{
			Endpoints: []string{"127.0.0.1:7000"},
			Online:    true,
			LastSeen:  time.Now(),
		}
	}
	for _, id := range withoutEndpoint {
		snap.Members[identity.DeviceID(id)] = discovery.MemberView{
			Online:   true,
			LastSeen: time.Now(),
		}
	}
	return snap
}

// TestRelayCandidatesAreDeterministic: the candidate order must be the
// same on every call and on every run. The relay path picks the first
// candidate that works — if the order wobbled, two peers would relay
// through different members for no reason, and the behaviour could not be
// reproduced from a bug report.
//
// The view is a Go map, whose iteration order is randomised by the
// runtime. A candidate list built by walking the map would therefore be
// non-deterministic — that is exactly what this test pins.
func TestRelayCandidatesAreDeterministic(t *testing.T) {
	snap := candidateView(t,
		[]string{"cccccccccccccccc", "aaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbb"},
		nil,
	)
	self := identity.DeviceID("self111111111111")
	target := identity.DeviceID("target1111111111")

	first := RelayCandidates(snap, target, self)
	for i := 0; i < 20; i++ {
		got := RelayCandidates(snap, target, self)
		if len(got) != len(first) {
			t.Fatalf("call %d returned %d candidates, first call returned %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("call %d returned %v, first call returned %v", i, got, first)
			}
		}
	}
	// And the order is the device ID order, not the map's.
	want := []identity.DeviceID{"aaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbb", "cccccccccccccccc"}
	if len(first) != len(want) {
		t.Fatalf("got %v, want %v", first, want)
	}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("got %v, want %v (device ID order)", first, want)
		}
	}
}

// TestRelayCandidatesExcludeSelfAndTarget: a member cannot relay to
// itself, and the target is never its own relay — the direct path is the
// target's own business.
func TestRelayCandidatesExcludeSelfAndTarget(t *testing.T) {
	snap := candidateView(t,
		[]string{"self111111111111", "target1111111111", "relay11111111111"},
		nil,
	)
	got := RelayCandidates(snap, identity.DeviceID("target1111111111"), identity.DeviceID("self111111111111"))
	if len(got) != 1 {
		t.Fatalf("got %v, want exactly the third member", got)
	}
	if got[0] != identity.DeviceID("relay11111111111") {
		t.Fatalf("got %v, want [relay11111111111]", got)
	}
}

// TestRelayCandidatesSkipEndpointless: a member that announced no
// endpoint cannot be dialed, so it cannot be a relay either — picking it
// would burn an attempt on a guaranteed failure.
func TestRelayCandidatesSkipEndpointless(t *testing.T) {
	snap := candidateView(t,
		[]string{"relay11111111111"},
		[]string{"noendpoint111111"},
	)
	got := RelayCandidates(snap, identity.DeviceID("target1111111111"), identity.DeviceID("self111111111111"))
	if len(got) != 1 || got[0] != identity.DeviceID("relay11111111111") {
		t.Fatalf("got %v, want only the member with an endpoint", got)
	}
}

// TestRelayCandidatesEmptyView: no view, no candidates — and no panic.
func TestRelayCandidatesEmptyView(t *testing.T) {
	if got := RelayCandidates(nil, identity.DeviceID("target1111111111"), identity.DeviceID("self111111111111")); len(got) != 0 {
		t.Fatalf("a nil view must yield no candidates, got %v", got)
	}
	snap := &discovery.MemberSnapshot{Members: map[identity.DeviceID]discovery.MemberView{}}
	if got := RelayCandidates(snap, identity.DeviceID("target1111111111"), identity.DeviceID("self111111111111")); len(got) != 0 {
		t.Fatalf("an empty view must yield no candidates, got %v", got)
	}
}
