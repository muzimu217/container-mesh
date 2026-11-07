// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"sync"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// Node is the in-memory discovery service. Any member can host one.
//
// It is deliberately not an HTTP server here: the relay package mounts this
// behind /mesh/* and reuses its own rate limiting and TLS. Keeping the state
// machine separate from the transport means the tests exercise membership
// semantics without any socket.
type Node struct {
	mu      sync.Mutex
	members *identity.MemberList
	// lastSeen and endpoints mirror the member list; they live in a parallel
	// map because MemberList is the identity package's verified view (who
	// belongs), while freshness and addresses are the discovery layer's view
	// (who is reachable right now).
	lastSeen map[identity.DeviceID]time.Time
	endpts   map[identity.DeviceID][]string

	// subs are the connected members receiving pushes.
	subs map[identity.DeviceID]chan struct{}

	// streams counts live members-stream connections per device. It is the
	// fast liveness path (R-2.2's "transport-level break detection"): when a
	// member's process dies — kill -9, cable pull, destroyed netns — the
	// kernel closes its socket and the node sees the stream end within
	// seconds, an order of magnitude ahead of the heartbeat timeout, which
	// remains only as the fallback for "connection alive, process hung".
	streams map[identity.DeviceID]int

	// now is injectable for tests.
	now func() time.Time
}

// NewNode builds a discovery node over an existing member list. The list is
// the source of truth for membership; the node only adds liveness and
// addresses on top.
func NewNode(members *identity.MemberList) *Node {
	return &Node{
		members:  members,
		lastSeen: make(map[identity.DeviceID]time.Time),
		endpts:   make(map[identity.DeviceID][]string),
		subs:     make(map[identity.DeviceID]chan struct{}),
		streams:  make(map[identity.DeviceID]int),
		now:      time.Now,
	}
}

// AttachStream records that id has an open members-stream connection.
func (n *Node) AttachStream(id identity.DeviceID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.streams[id]++
}

// DetachStream removes one members-stream connection for id. When it was the
// last one, the device's reachability expires IMMEDIATELY and subscribers
// are pushed the change — this is the fast-offline path that makes P-5's
// ≤10 s bound achievable for kill -9 style deaths (the heartbeat timeout
// only has to catch the hung-process case it was designed for).
//
// Reconnect overlap is safe: a member that re-dials before its old socket's
// death is noticed holds count 2; the old stream's detach leaves 1 and the
// device stays online.
func (n *Node) DetachStream(id identity.DeviceID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.streams[id] > 1 {
		n.streams[id]--
		return
	}
	delete(n.streams, id)
	if _, wasOnline := n.lastSeen[id]; wasOnline {
		delete(n.lastSeen, id)
		delete(n.endpts, id)
		n.notifyAll()
	}
}

// Register applies an announce AFTER the caller has validated it.
//
// The node re-checks membership itself (the cheap check — one map lookup)
// because an unauthenticated Register is the single worst hole in this layer:
// any caller that forgets Validate turns the node into an injection point
// that pushes attacker-chosen endpoints to every subscriber. The expensive
// checks (signature, timestamp, endpoint shapes) stay in the caller's
// Validate call; membership is duplicated here as a structural backstop.
//
// The boolean reports whether the endpoints changed, so the HTTP layer can
// decide whether the update is worth pushing.
func (n *Node) Register(a MemberAnnounce, when time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	// Structural backstop: refuse to record state for a non-member even if
	// the caller skipped Validate (external security review, high finding).
	if !n.members.Contains(a.DeviceID) {
		return false
	}
	changed := !equalStrings(n.endpts[a.DeviceID], a.Endpoints)
	// Copy: the announce's Endpoints slice belongs to the caller (it may be a
	// buffer the transport reuses), so storing the reference would let a later
	// message silently rewrite the recorded state.
	n.endpts[a.DeviceID] = append([]string(nil), a.Endpoints...)
	n.lastSeen[a.DeviceID] = when
	if changed {
		n.notifyAll()
	}
	return changed
}

// Endpoints returns a copy of a device's current candidate addresses.
func (n *Node) Endpoints(id identity.DeviceID) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.endpts[id]...)
}

// LastSeen reports when a device last announced.
func (n *Node) LastSeen(id identity.DeviceID) (time.Time, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t, ok := n.lastSeen[id]
	return t, ok
}

// Subscribe registers a member for change notifications. The returned
// channel is closed on every membership or endpoint change; the subscriber
// re-reads the state it needs. One channel per change keeps the protocol
// trivial and drops nothing.
func (n *Node) Subscribe(id identity.DeviceID) (<-chan struct{}, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	// A second Subscribe for the same device closes the first channel rather
	// than silently orphaning it: the old subscriber's select would otherwise
	// block forever on a channel nobody signals (external review finding).
	if old, ok := n.subs[id]; ok {
		close(old)
		delete(n.subs, id)
	}
	// Buffered so notifyAll never blocks on a slow subscriber; a dropped
	// signal is fine because the next one re-triggers the read.
	ch := make(chan struct{}, 1)
	n.subs[id] = ch
	unsub := func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.subs[id] == ch {
			delete(n.subs, id)
		}
	}
	return ch, unsub
}

func (n *Node) notifyAll() {
	for id, ch := range n.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
		_ = id
	}
}

// Sweep drops members whose heartbeat timed out and reports which were
// dropped. The caller (relay) pushes the change. Sweep does NOT remove them
// from the identity MemberList — membership and liveness are different
// things, and a member whose laptop slept for two minutes should not have to
// be re-approved. Only the reachability view expires.
func (n *Node) Sweep() []identity.DeviceID {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	var dropped []identity.DeviceID
	for id, t := range n.lastSeen {
		if now.Sub(t) > MemberTimeout {
			delete(n.lastSeen, id)
			delete(n.endpts, id)
			dropped = append(dropped, id)
		}
	}
	if len(dropped) > 0 {
		n.notifyAll()
	}
	return dropped
}

// SetClock replaces the time source (tests).
func (n *Node) SetClock(f func() time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.now = f
}

// SetMembers replaces the membership source. This is the approval path: an
// operator adds a device's certificate to the list and hands the new list to
// the node; the swap itself pushes a change to every subscriber, because a
// membership change is exactly what they are watching for. The node does not
// merge or diff — it swaps wholesale, and Register's own membership backstop
// immediately starts consulting the new list.
func (n *Node) SetMembers(m *identity.MemberList) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.members = m
	n.notifyAll()
}

// OnlineSnapshot lists currently-online members with their endpoints.
type OnlineMember struct {
	DeviceID  identity.DeviceID
	Endpoints []string
	LastSeen  time.Time
}

// Snapshot returns who is online right now, in stable (sorted) order.
func (n *Node) Snapshot() []OnlineMember {
	n.mu.Lock()
	defer n.mu.Unlock()
	ids := make([]identity.DeviceID, 0, len(n.lastSeen))
	for id := range n.lastSeen {
		ids = append(ids, id)
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	out := make([]OnlineMember, 0, len(ids))
	for _, id := range ids {
		out = append(out, OnlineMember{
			DeviceID:  id,
			Endpoints: append([]string(nil), n.endpts[id]...),
			LastSeen:  n.lastSeen[id],
		})
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
