// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Member is one device's standing in the network.
//
// There is deliberately no Endpoints field. It used to be one, and it was
// ALWAYS empty: nothing in the tree ever wrote it, and the only two
// mentions were the declaration and the renewal path copying the old (also
// empty) value forward.
//
// A field that looks like it holds the member's addresses but never does
// is worse than no field: a reader would conclude "this member has no
// endpoints, so it cannot be dialed" — which is wrong for every member that
// announced one. The addresses a member announced live in the discovery
// node's own view (discovery.Node.endpts), and reach the clients through the
// verified snapshot (discovery.OnlineMember.Endpoints). The member list
// is the IDENTITY layer: who belongs, not where they are right now.
type Member struct {
	Cert     DeviceCert `json:"cert"`
	AddedAt  time.Time  `json:"added_at"`
	LastSeen time.Time  `json:"last_seen"`
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

	// RevokedCreds is the anchor's credential revocation list: credential
	// ids the owner has turned off, mapped to when the revocation happened.
	//
	// It closes D77's last gap. The delegation record — the roster's only
	// other knowledge of a credential — is written at ENROLL time, so a
	// credential that leaked before anyone joined with it had no record to
	// remove and stayed live until its expiry (a year by default). An id on
	// this list is refused at enroll regardless of the credential's
	// signature or expiry, and — because the list outlives the delegation
	// record — a revoked credential's string cannot bring its delegation
	// back by being presented again. The anchor-side set is enough: a
	// revoked credential's devices are already out of the roster (the
	// revocation of a used credential kicks them), and a member only ever
	// verifies certs of CURRENT members against CURRENT delegation records,
	// so the set never needs to reach the members (see the snapshot's
	// Delegations comment for what does travel and why).
	//
	// omitempty: rosters written before the list existed have no field, and
	// an empty list must not grow one — the file shape stays readable by
	// whatever wrote it last.
	RevokedCreds map[string]time.Time `json:"revoked_creds,omitempty"`

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
	// A revoked id never re-enters the roster. The enroll path checks the
	// revocation list before it gets anywhere near here, but this is the
	// data structure's own floor: no caller, present or future, can quietly
	// re-install what the owner turned off (a re-presented credential string
	// carries the SAME id, account-signed and all).
	if _, revoked := m.RevokedCreds[d.ID]; revoked {
		return fmt.Errorf("identity: credential %s was revoked and cannot be authorised again", d.ID)
	}
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

// RevokeDelegation drops an authorisation AND every device that enrolled
// under it.
//
// This is the owner's credential rotation: "除非我自己改密钥，然后其他会掉线".
// A team credential is a key the owner hands out, and turning it must take
// the people who came in on it out of the network — RemoveDelegation only
// stopped NEW enrolments, which meant the holder of a leaked password
// stayed inside even after the password had been turned.
//
// Members approved by the account itself (SignerPub empty) and members of
// OTHER delegations are untouched: a rotation is scoped to one credential.
//
// It returns the device ids it removed, so the caller can say what
// happened instead of leaving the owner to diff the roster by hand.
func (m *MemberList) RevokeDelegation(id string) ([]DeviceID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.Delegations[id]
	if !ok {
		return nil, fmt.Errorf("identity: no delegation %s", id)
	}
	removed := make([]DeviceID, 0, 4)
	for devID, mm := range m.Members {
		if len(mm.Cert.SignerPub) > 0 && mm.Cert.SignerPub.Equal(d.DelegatorPub) {
			removed = append(removed, devID)
			delete(m.Members, devID)
		}
	}
	delete(m.Delegations, id)
	// The id goes on the revocation list as part of the same mutation: the
	// credential's account signature outlives the delegation record, so
	// without this the old string would simply re-enrol (the enroll path
	// would find no delegation and write a fresh one). Kicking the members
	// is half of a rotation; making it non-resurrectable is the other half.
	if m.RevokedCreds == nil {
		m.RevokedCreds = make(map[string]time.Time)
	}
	if _, seen := m.RevokedCreds[id]; !seen {
		m.RevokedCreds[id] = time.Now().UTC()
	}
	m.Version++
	return removed, nil
}

// RevokeCredential records a credential id on the revocation list WITHOUT
// needing a delegation record on the roster.
//
// This is pre-revocation, for the credential that leaked before anyone used
// it: the roster only learns a credential's id at enroll time, so there is
// nothing to kick and nothing to remove — the owner's decision must be
// recorded somewhere, and this is that somewhere. An id on the list is
// refused at enroll forever (there is no un-revoke; re-issuing means a new
// credential with a new id), and AddDelegation refuses to re-authorise it.
//
// Idempotent: listing an already-listed id is a no-op (returns false, no
// version bump), so an operator double-running the command does not churn
// the version every member orders its view by. It returns whether THIS call
// newly listed the id.
func (m *MemberList) RevokeCredential(id string) (bool, error) {
	if id == "" {
		return false, errors.New("identity: credential id required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.RevokedCreds == nil {
		m.RevokedCreds = make(map[string]time.Time)
	}
	if _, seen := m.RevokedCreds[id]; seen {
		return false, nil
	}
	m.RevokedCreds[id] = time.Now().UTC()
	m.Version++
	return true, nil
}

// mergeRevocations adds revocations that are not in the list yet.
//
// It only ever ADDS. A revocation is permanent: the owner's decision to
// turn a key off is not undone by a stale copy of the list, and there is
// no command that un-revokes one.
func (m *MemberList) mergeRevocations(extra map[string]time.Time) {
	if len(extra) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.RevokedCreds == nil {
		m.RevokedCreds = make(map[string]time.Time)
	}
	for id, when := range extra {
		if _, seen := m.RevokedCreds[id]; !seen {
			m.RevokedCreds[id] = when
		}
	}
}

// CredentialRevoked reports whether a credential id is on the revocation
// list. This is the check the enroll path runs FIRST — before signature and
// expiry, which a leaked credential still satisfy.
func (m *MemberList) CredentialRevoked(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.RevokedCreds[id]
	return ok
}

// RevokedCredentials lists the revoked ids, sorted (map order is random; a
// stable order keeps status output and tests reproducible).
func (m *MemberList) RevokedCredentials() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.RevokedCreds))
	for id := range m.RevokedCreds {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// DelegationByID finds a delegation record by its credential id — the same
// id the credential string carries and the revocation list is keyed by.
func (m *MemberList) DelegationByID(id string) (Delegation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.Delegations[id]
	return d, ok
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
			m.Members[c.DeviceID] = Member{Cert: c, AddedAt: old.AddedAt, LastSeen: old.LastSeen}
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
//
// It saves exactly what the list holds. Use SaveMembersMerged where the
// list may be STALE relative to the file (the anchor's own path: the
// operator's revoke lands in the file, and the anchor's in-memory list
// only catches up on its next reload).
func (m *MemberList) SaveMembers(path string) error {
	return m.saveMembers(path, false)
}

// SaveMembersMerged writes the member list and KEEPS the revocations the
// file already records.
//
// The anchor saves the roster from its in-memory list, and that list only
// catches up with the file every couple of seconds. So between "cred
// revoke wrote the file" and "the anchor reloaded it" there is a window
// in which the anchor's live list does not know about the revocation —
// and any save in that window (an enrolment does one) writes the stale
// view back over the file, ERASING the revocation.
//
// Measured on the real machine: two revoked ids were in the file, one
// enrolment happened, and the file came back with no revocation list at
// all. Both credentials could enrol again — a rotation that is not
// atomic is not a rotation.
//
// The file is the external authority here (the operator edits it, the
// CLI writes it), so the file's revocations are merged IN on the way
// out. The merge only ever ADDS: a revocation is permanent, and there
// is no command that un-revokes one.
func (m *MemberList) SaveMembersMerged(path string) error {
	return m.saveMembers(path, true)
}

func (m *MemberList) saveMembers(path string, mergeFileRevocations bool) error {
	if mergeFileRevocations {
		// Read the file's revocations FIRST, before taking the lock: the
		// file is the authority for them, and the in-memory list may be
		// behind it.
		if onDisk, err := LoadMembers(path); err == nil {
			m.mergeRevocations(onDisk.RevokedCreds)
		}
	}
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
	// The revocation list travels with them: a revoke that did not survive
	// the restart was never a revoke, and the enroll path checks this set
	// on every admission.
	if len(m.RevokedCreds) > 0 {
		snapshot.RevokedCreds = make(map[string]time.Time, len(m.RevokedCreds))
		for k, v := range m.RevokedCreds {
			snapshot.RevokedCreds[k] = v
		}
	}
	m.mu.Unlock()
	blob, err := json.MarshalIndent(&snapshot, "", "  ")
	if err != nil {
		return err
	}
	dir := dirOf(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Write-then-rename instead of a plain WriteFile: a crash mid-write
	// must not leave a truncated members.json — the file is the anchor's
	// only copy of the roster AND the revocation list, and a truncated file
	// either refuses to load (a dead anchor) or loads short (members and
	// revocations silently gone). The rename is atomic within the same
	// directory; the temp file is removed on every failure path.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	// The roster must stay READABLE BY THE SERVICE THAT SERVES IT.
	//
	// The roster is written from several places: the anchor's own watcher
	// (as the service user) and the operator's CLI (often as root, from a
	// different shell). A root-written file with 0600 is owned by root, and
	// the anchor — running as its own unprivileged user — then cannot read
	// it. Measured on the real machine: `cmesh cred revoke` (root) wrote the
	// roster, the anchor's reload failed with "permission denied", the
	// anchor kept its stale in-memory roster, and the next save ERASED the
	// revocation. A rotation that the service cannot read is not a
	// rotation.
	//
	// So the new file inherits the OWNERSHIP OF THE DIRECTORY it lives in
	// — the directory is owned by the service user, so the roster stays
	// readable by the service whoever wrote it. The mode stays 0600: the
	// roster is not public, it is SHARED with the one account that has to
	// read it.
	if dirStat, err := os.Stat(dir); err == nil {
		if dirUID, dirGID, ok := ownerOf(dirStat); ok {
			if err := os.Chown(tmpName, dirUID, dirGID); err != nil {
				// Not fatal: the file is still correct, only its owner
				// may be wrong. The caller decides whether that matters;
				// the anchor logs its own read failures.
				return os.Rename(tmpName, path)
			}
		}
	}
	return os.Rename(tmpName, path)
}

// ownerOf reports the uid and gid that own a file, when the platform
// exposes them.
func ownerOf(fi os.FileInfo) (int, int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
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
