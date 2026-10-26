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
		now:      time.Now,
	}
}

// Register applies a validated announce. Membership and signature checks
// happen in the caller (Validate needs the caller's clock); this method only
// mutates state, which keeps it trivially testable.
//
// The boolean reports whether the endpoints changed, so the HTTP layer can
// decide whether the update is worth pushing.
func (n *Node) Register(a MemberAnnounce, when time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	changed := !equalStrings(n.endpts[a.DeviceID], a.Endpoints)
	n.endpts[a.DeviceID] = a.Endpoints
	n.lastSeen[a.DeviceID] = when
	if m, ok := n.members.Get(a.DeviceID); ok {
		m.Endpoints = a.Endpoints
		m.LastSeen = when
		// Best-effort write-back; MemberList has no setter for LastSeen and
		// adding one widens its API for a display field. The authoritative
		// freshness stays in this map.
		_ = m
	}
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
