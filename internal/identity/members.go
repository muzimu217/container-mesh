// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Member is one device's standing in the network.
type Member struct {
	Cert      DeviceCert `json:"cert"`
	AddedAt   time.Time  `json:"added_at"`
	Endpoints []string   `json:"endpoints,omitempty"`
	LastSeen  time.Time  `json:"last_seen"`
}

// MemberList is the network's source of truth for who belongs.
//
// Versioning is what makes revocation survivable across partitions: every
// mutation bumps Version, and the version travels with both the push (so
// nodes can order updates they receive out of turn) and every e2e frame (so
// a receiver can detect that a revocation happened while it was cut off).
//
// The disconnect TEST is membership, never version ordering — a Version bump
// from an unrelated change must not tear sessions down mid-propagation
// (MESH-DESIGN §2.1, review note 1).
type MemberList struct {
	AccountID DeviceID            `json:"account_id"`
	Version   uint64              `json:"version"`
	Members   map[DeviceID]Member `json:"members"`
	// Delegations are the account's authorisations for a delegator key to
	// sign device certificates. They travel WITH the roster so every
	// member can verify a delegated certificate: without them, a member
	// that joined under a credential would verify as a forgery and be
	// dropped from every view (M4-CREDENTIAL.md).
	//
	// Keyed by the delegation's own ID, which is also the quota bucket.
	Delegations map[string]Delegation `json:"delegations,omitempty"`

	mu sync.Mutex
}

// NewMemberList creates an empty list bound to an account.
func NewMemberList(account DeviceID) *MemberList {
	return &MemberList{AccountID: account, Members: make(map[DeviceID]Member)}
}

// AddDelegation records an authorisation for a delegator key.
//
// The account signature is NOT checked here: the list has no account public
// key, and it does not verify member certificates either — the party that
// ACCEPTS a delegation (the discovery node, which holds the account key)
// verifies it before calling this. What the list does enforce is the
// structural shape, so a malformed record cannot enter the roster.
func (m *MemberList) AddDelegation(d Delegation) error {
	if d.ID == "" {
		return errors.New("identity: delegation needs an id")
	}
	if len(d.DelegatorPub) != ed25519.PublicKeySize {
		return fmt.Errorf("identity: delegation public key must be %d bytes, got %d", ed25519.PublicKeySize, len(d.DelegatorPub))
	}
	if d.MaxDevices <= 0 {
		return fmt.Errorf("identity: delegation must allow at least one device, got %d", d.MaxDevices)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Delegations == nil {
		m.Delegations = make(map[string]Delegation)
	}
	if old, ok := m.Delegations[d.ID]; ok {
		if !old.DelegatorPub.Equal(d.DelegatorPub) {
			return fmt.Errorf("identity: delegation %s already exists with a different key", d.ID)
		}
	} else {
		m.Version++
	}
	m.Delegations[d.ID] = d
	return nil
}

// RemoveDelegation drops an authorisation. Devices already enrolled under
// it stay in the roster — revoking the delegation stops NEW enrolments, it
// is not a device revocation.
func (m *MemberList) RemoveDelegation(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.Delegations[id]; !ok {
		return fmt.Errorf("identity: no delegation %s", id)
	}
	delete(m.Delegations, id)
	m.Version++
	return nil
}

// DelegationFor finds the authorisation that covers a signing key.
func (m *MemberList) DelegationFor(pub ed25519.PublicKey) (Delegation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.Delegations {
		if d.DelegatorPub.Equal(pub) {
			return d, true
		}
	}
	return Delegation{}, false
}

// DelegationList lists the recorded authorisations, oldest id first (the
// map order is random; a stable order keeps snapshots reproducible).
func (m *MemberList) DelegationList() []Delegation {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Delegation, 0, len(m.Delegations))
	for _, d := range m.Delegations {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DelegatedCount reports how many members were enrolled under a delegator
// key. The quota is counted from the ROSTER, not from a counter: a revoked
// device gives its slot back, and a restarted node recovers the count
// without extra state.
func (m *MemberList) DelegatedCount(pub ed25519.PublicKey) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, mm := range m.Members {
		if len(mm.Cert.SignerPub) > 0 && mm.Cert.SignerPub.Equal(pub) {
			n++
		}
	}
	return n
}

// ErrNotAMember is returned when an operation requires membership the list
// does not record.
var ErrNotAMember = errors.New("identity: device is not a member")

// Add records a member. Idempotent for identical certs; adding a DIFFERENT
// cert for an existing deviceID is refused, because a device ID is derived
// from the device key and two different certs for one ID means one of them
// is lying about its key.
func (m *MemberList) Add(c DeviceCert) error {
	if IDFromKey(c.DevicePub) != c.DeviceID {
		return ErrCertMismatch
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.Members[c.DeviceID]; ok {
		// Same key and account: a re-issue after renewal. Refresh it.
		if old.Cert.DevicePub.Equal(c.DevicePub) && old.Cert.AccountID == c.AccountID {
			m.Members[c.DeviceID] = Member{Cert: c, AddedAt: old.AddedAt, Endpoints: old.Endpoints, LastSeen: old.LastSeen}
			m.Version++
			return nil
		}
		return fmt.Errorf("identity: device %s already exists with a different key", c.DeviceID)
	}
	m.Members[c.DeviceID] = Member{Cert: c, AddedAt: time.Now().UTC()}
	m.Version++
	return nil
}

// Remove revokes a device. This is the whole revocation operation on the
// data structure: bump the version so the push propagates, and from then on
// Contains returns false, which is what the session teardown checks.
func (m *MemberList) Remove(id DeviceID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.Members[id]; !ok {
		return ErrNotAMember
	}
	delete(m.Members, id)
	m.Version++
	return nil
}

// Contains reports membership. This is the ONLY check that may tear down a
// live session (see the type comment).
func (m *MemberList) Contains(id DeviceID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.Members[id]
	return ok
}

// Get returns a copy of a member record.
func (m *MemberList) Get(id DeviceID) (Member, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mm, ok := m.Members[id]
	return mm, ok
}

// Count reports the membership size.
func (m *MemberList) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Members)
}

// CurrentVersion reports the version counter.
func (m *MemberList) CurrentVersion() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Version
}

// IDs lists member IDs in a stable order (sorted), for status output and tests.
func (m *MemberList) IDs() []DeviceID {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DeviceID, 0, len(m.Members))
	for id := range m.Members {
		out = append(out, id)
	}
	// insertion-sort: member counts are tens, not thousands
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// SaveMembers writes the member list. It contains no secrets (certs are
// public), but it is written under the state dir with 0600 anyway so the
// state directory has one permission story.
func (m *MemberList) SaveMembers(path string) error {
	m.mu.Lock()
	snapshot := MemberList{AccountID: m.AccountID, Version: m.Version,
		Members: make(map[DeviceID]Member, len(m.Members))}
	for k, v := range m.Members {
		snapshot.Members[k] = v
	}
	// The delegations travel with the roster: a member that joined under a
	// credential must still be verifiable after a restart, and the quota
	// is counted from the roster.
	if len(m.Delegations) > 0 {
		snapshot.Delegations = make(map[string]Delegation, len(m.Delegations))
		for k, v := range m.Delegations {
			snapshot.Delegations[k] = v
		}
	}
	m.mu.Unlock()
	blob, err := json.MarshalIndent(&snapshot, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, blob, 0o600)
}

// LoadMembers reads a member list written by SaveMembers.
func LoadMembers(path string) (*MemberList, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m MemberList
	if err := json.Unmarshal(blob, &m); err != nil {
		return nil, fmt.Errorf("identity: %s: not a member list: %w", path, err)
	}
	if m.Members == nil {
		m.Members = make(map[DeviceID]Member)
	}
	return &m, nil
}
