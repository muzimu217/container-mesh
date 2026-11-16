// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"sort"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// Relay candidate selection.
//
// v1.1 relays through ONE hop only: the initiator opens a session to a
// relay member, and the relay forwards it to the target. A one-hop path
// has no loop, so there is nothing for a hop counter or a split-horizon
// rule to protect against — those belong to multi-hop routing, which is a
// later requirement (R-3.3, path quality / best-path selection).
//
// What v1.1 DOES need is determinism: the relay path picks the first
// candidate that works, so two peers must agree on the candidate ORDER or
// they will relay through different members for no reason, and a bug
// report ("it relayed through X") could not be reproduced.
//
// The order is therefore the device ID order — the one ordering every
// participant computes identically from the same view.

// RelayCandidates returns the members that could relay a session to
// target, in a deterministic order.
//
// A candidate is a member that:
//
//   - is not the initiator itself (a member cannot relay to itself),
//   - is not the target (the target is reached directly, not through
//     itself),
//   - has announced at least one endpoint (a member with no endpoint
//     cannot be dialed, so it cannot be a relay either — naming it would
//     only burn an attempt on a guaranteed failure).
//
// The result is sorted by device ID, so the same view always yields the
// same list, on every member, in every process.
func RelayCandidates(view *discovery.MemberSnapshot, target, self identity.DeviceID) []identity.DeviceID {
	if view == nil || len(view.Members) == 0 {
		return nil
	}
	out := make([]identity.DeviceID, 0, len(view.Members))
	for id, mv := range view.Members {
		if id == target || id == self {
			continue
		}
		if len(mv.Endpoints) == 0 {
			continue
		}
		out = append(out, id)
	}
	// sort.Slice on the device ID (a string) — the map iteration order
	// above is randomised by the runtime and must not leak into the
	// result.
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
