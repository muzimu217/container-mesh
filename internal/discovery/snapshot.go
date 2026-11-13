// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// RegisterRequest is the POST /mesh/register body. Both sides (relay handler
// and discovery client) share this type so the wire shape has exactly one
// definition; a field renamed in one place cannot silently desynchronize the
// other.
type RegisterRequest struct {
	Announce MemberAnnounce `json:"announce"`
	Sig      []byte         `json:"sig"`
}

// MeshAuthContext prefixes the bytes a member signs to prove membership on
// the /mesh/members stream. It binds the signature to this specific purpose,
// so a signature produced for an announce can never be replayed as a stream
// credential or vice versa.
const MeshAuthContext = "cmesh-members-auth"

// MembersAuthHeaders carries the credentials a client presents on
// /mesh/members. The signature covers MeshAuthContext + device ID + the
// RFC3339Nano timestamp in X-Mesh-Time; the receiver enforces the same
// ±AnnounceClockSkew window as announces, so a captured header set dies with
// its timestamp.
const (
	HeaderMeshDevice = "X-Mesh-Device"
	HeaderMeshTime   = "X-Mesh-Time"
	HeaderMeshSig    = "X-Mesh-Sig"
)

// MemberView is one member as the snapshot wire format carries it: the cert
// (which the client re-verifies against the account key it got in the
// invite), the node's record of reachability, and a derived online flag.
//
// Online is derived at snapshot build time, not stored: liveness is the
// node's opinion about a moving target, and every push re-derives it.
type MemberView struct {
	Cert      identity.DeviceCert `json:"cert"`
	Endpoints []string            `json:"endpoints,omitempty"`
	LastSeen  time.Time           `json:"last_seen,omitzero"`
	Online    bool                `json:"online"`
}

// MemberSnapshot is what GET /mesh/members pushes: who belongs (from the
// member list), who is reachable right now (from the node's liveness view).
//
// A snapshot is a whole view, not a delta: member counts are tens, a delta
// protocol would need its own ordering rules, and a client that missed a
// push just waits for the next full one. The version travels so a client can
// ignore stale out-of-order copies.
//
// Delegations travel with it for one reason: a member that joined under a
// team credential holds a certificate signed by a DELEGATOR, and the
// client's trust model only knows the account key. Without the delegation
// record that proves the delegator was authorised, such a member would
// verify as a forgery and be dropped from every view — the device would be
// in the network and invisible to it (M4-CREDENTIAL.md).
type MemberSnapshot struct {
	AccountID   identity.DeviceID                `json:"account_id"`
	Version     uint64                           `json:"version"`
	Members     map[identity.DeviceID]MemberView `json:"members"`
	Delegations []identity.Delegation            `json:"delegations,omitempty"`
}

// BuildSnapshot combines the member list (who belongs) with the node's
// liveness view (who announced recently). Members with no recorded heartbeat
// appear with Online=false and no endpoints: they are part of the network —
// they may receive approval-based services — but are not dialable now.
func BuildSnapshot(members *identity.MemberList, n *Node, now time.Time) *MemberSnapshot {
	snap := &MemberSnapshot{
		AccountID:   members.AccountID,
		Version:     members.CurrentVersion(),
		Members:     make(map[identity.DeviceID]MemberView, members.Count()),
		Delegations: members.DelegationList(),
	}
	for _, id := range members.IDs() {
		mm, ok := members.Get(id)
		if !ok {
			continue // list mutated mid-iteration; next push will be correct
		}
		// Expired certificates are not sent at all: the client would drop
		// them anyway (a lapsed cert no longer proves membership), and
		// sending them first only widens the window where an old cert is
		// treated as a live member (security review F3).
		if now.After(mm.Cert.ExpiresAt) {
			continue
		}
		view := MemberView{Cert: mm.Cert}
		// Endpoints are announcement state and are shown regardless of
		// liveness: a members-stream gap does not mean the member's UDP
		// listener is gone, and a dial attempt against the last advertised
		// address beats a blind "no endpoints" hint (issue #8 — endpoint
		// oscillation fix). Online remains the pure liveness signal (P-5).
		if eps := n.Endpoints(id); len(eps) > 0 {
			view.Endpoints = eps
		}
		if t, seen := n.LastSeen(id); seen && now.Sub(t) <= MemberTimeout {
			view.Online = true
			view.LastSeen = t
		}
		snap.Members[id] = view
	}
	return snap
}
