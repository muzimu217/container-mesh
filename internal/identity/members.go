// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

	mu sync.Mutex
}

// NewMemberList creates an empty list bound to an account.
func NewMemberList(account DeviceID) *MemberList {
	return &MemberList{AccountID: account, Members: make(map[DeviceID]Member)}
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
