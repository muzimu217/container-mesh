// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

import (
	"crypto/ed25519"
	"encoding/base64"
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

	// RevokedSigners is the set of delegator keys whose credential has
	// been turned off, base64-encoded.
	//
	// It exists because a revocation does two things and the list above
	// can only carry one of them. The list records the CREDENTIAL ID (so
	// the credential string can never enrol again); but a revocation also
	// REMOVES the devices that enrolled under that credential — and
	// without this set, the removal cannot be recognised afterwards: the
	// delegation record is deleted with it, so the key that signed those
	// devices is no longer anywhere in the roster.
	//
	// The anchor's in-memory roster can be two seconds behind the file
	// (it hot-reloads on a tick), and every write-back of the in-memory
	// roster put the revoked devices BACK. Measured by kuku-sandbox-01
	// with an ordered reproduction: a revoke and an enrolment issued
	// together → the revoke reported "1 device(s) left the network", the
	// enrolment with the SAME revoked credential was ACCEPTED, and the
	// roster went back UP.
	//
	// So the set is the durable record of "these devices left", and it is
	// merged on every write-back for the same reason the revocation list
	// is: the file is the authority on what was turned off.
	//
	// omitempty, for the same reason as the list above: a roster written
	// before this field existed must not grow one.
	RevokedSigners map[string]string `json:"revoked_signers,omitempty"`

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
	// The delegator KEY goes on the revoked-signer set: the delegation
	// record is about to be deleted, and without this the removal of the
	// devices above could not be recognised afterwards (the key is the
	// only thing that says which devices those were).
	if m.RevokedSigners == nil {
		m.RevokedSigners = make(map[string]string)
	}
	if len(d.DelegatorPub) > 0 {
		m.RevokedSigners[base64.StdEncoding.EncodeToString(d.DelegatorPub)] = id
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

// mergeAccountID adopts the account id from a roster on disk, when this
// list has none.
//
// The account id is what the anchor checks its roster against, and a
// roster written without it is refused ("members file reload refused:
// file is for account X, network is Y"). An empty list — the one a fresh
// anchor's operator builds with `cred revoke` — has no account id of its
// own, so it takes the one the file already records.
func (m *MemberList) mergeAccountID(id DeviceID) {
	if id == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.AccountID == "" {
		m.AccountID = id
	}
}

// mergeRevokedSigners adds the revoked delegator keys that the list does
// not have yet.
//
// Like the revocation list, it only ever ADDS: a revocation is
// permanent, and there is no command that un-revokes one.
func (m *MemberList) mergeRevokedSigners(extra map[string]string) {
	if len(extra) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.RevokedSigners == nil {
		m.RevokedSigners = make(map[string]string)
	}
	for k, v := range extra {
		if _, seen := m.RevokedSigners[k]; !seen {
			m.RevokedSigners[k] = v
		}
	}
}

// dropMembersOfRevokedSigners removes the members whose credential was
// revoked and whose removal the in-memory list has not caught up with.
//
// A revocation does two things: it records the credential id, and it
// REMOVES the devices that enrolled under that credential — the "the
// devices left the network" the operator is told about. The anchor's
// in-memory roster can be two seconds behind the file, so those removed
// devices are still in it, and the next write-back would put them back:
// the revocation would be recorded (that list is merged) while its
// EFFECT was undone (the members were not).
//
// Measured by kuku-sandbox-01 with an ordered reproduction: a revoke and
// an enrolment issued together → the revoke reported "1 device(s) left the
// network", the enrolment with the SAME revoked credential was ACCEPTED,
// and the roster went back UP.
//
// The revoked SIGNER KEY is what identifies those members: the
// delegation record is deleted by the revocation, so the credential id
// alone cannot say which devices were signed under it.
//
// The scope is deliberately narrow, because the in-memory list is
// otherwise RIGHT about who joined: a device that enrolled a moment ago
// is never dropped (its credential is not revoked), and a device the
// ACCOUNT approved directly is never dropped either — the account is not
// a credential.
func (m *MemberList) dropMembersOfRevokedSigners() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Members) == 0 || len(m.RevokedSigners) == 0 {
		return
	}
	dropped := 0
	for id, mem := range m.Members {
		sp := mem.Cert.SignerPub
		if len(sp) == 0 {
			continue // directly approved by the account: not a credential's member
		}
		if _, revoked := m.RevokedSigners[base64.StdEncoding.EncodeToString(sp)]; revoked {
			delete(m.Members, id)
			dropped++
		}
	}
	if dropped == 0 {
		return
	}
	// The DELEGATION RECORD goes too.
	//
	// A revocation deletes it (RevokeDelegation), and the anchor's
	// in-memory copy still has it — so the next write-back would put it
	// back, and the delegation record is exactly what travels in the
	// SNAPSHOT. Every member would then be told that a revoked
	// delegator key is still authorised to sign certificates.
	for id, d := range m.Delegations {
		if len(d.DelegatorPub) == 0 {
			continue
		}
		if _, revoked := m.RevokedSigners[base64.StdEncoding.EncodeToString(d.DelegatorPub)]; revoked {
			delete(m.Delegations, id)
		}
	}
	// The VERSION moves forward.
	//
	// The version is what orders the updates: a client ignores a
	// snapshot whose version is not newer than the one it has. A drop
	// that did not bump it would write the roster's version BACKWARDS
	// (the file was already at a higher one), every client would treat
	// the snapshot as stale and IGNORE it — and the revocation would
	// never reach the network, even though it was in the file.
	m.Version++
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

// SaveMembersMerged writes the member list and KEEPS what the file
// already records: the revocations, the account id, and the MEMBERS the
// file has dropped.
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
// The MEMBERS were the second half of the same defect, and it is the
// one that actually undoes the revocation's EFFECT. A revocation does
// not only record an id: it also REMOVES the devices that enrolled under
// that credential — that is the "the devices left the network" the
// operator is told about. Those devices were still in the anchor's
// in-memory list, so the next write-back put them BACK on the file.
//
// Measured by kuku-sandbox-01 (ordered reproduction): a revoke and an
// enrolment issued together → the revoke reported "1 device(s) left the
// network", the enrolment with the SAME revoked credential was ACCEPTED,
// and the roster went back UP to 3 members.
//
// So the rule is: the FILE is the authority for what the roster
// CONTAINS. A member the file has dropped does not come back because
// the in-memory list still had it.
//
// The merge only ever REMOVES members (the file's view wins) and only
// ever ADDS revocations (a revocation is permanent, and there is no
// command that un-revokes one).
func (m *MemberList) SaveMembersMerged(path string) error {
	return m.saveMembers(path, true)
}

func (m *MemberList) saveMembers(path string, mergeFileRevocations bool) error {
	if mergeFileRevocations {
		// Read the file's revocations FIRST, before taking the lock: the
		// file is the authority for them, and the in-memory list may be
		// behind it.
		//
		// The file's ACCOUNT ID comes along for the same reason: a fresh
		// anchor has no roster, so the list the operator just built has
		// no account id — and writing it back with an empty one would
		// make the anchor refuse its own roster ("members file reload
		// refused: file is for account X, network is Y").
		if onDisk, err := LoadMembers(path); err == nil {
			m.mergeRevocations(onDisk.RevokedCreds)
			m.mergeAccountID(onDisk.AccountID)
			m.mergeRevokedSigners(onDisk.RevokedSigners)
			m.dropMembersOfRevokedSigners()
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
	// The revoked signer keys travel with them: they are what says which
	// devices left the network, and without them the next write-back
	// would put those devices back (see RevokedSigners).
	if len(m.RevokedSigners) > 0 {
		snapshot.RevokedSigners = make(map[string]string, len(m.RevokedSigners))
		for k, v := range m.RevokedSigners {
			snapshot.RevokedSigners[k] = v
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
		// A hand-written roster is a normal thing for an operator to do,
		// and "revoked_creds": [] is what a person writes to mean "no
		// revocations" — but the field is a map, so the roster used to
		// fail to load and EVERY command that reads it became unusable.
		// (Found by the external tester, who wrote one.)
		//
		// The shape is repaired rather than refused: the content of the
		// roster is still the operator's, and a roster that cannot be
		// loaded is a roster that cannot be repaired either.
		if repaired, ok := repairRosterShape(blob); ok {
			if err := json.Unmarshal(repaired, &m); err == nil {
				if m.Members == nil {
					m.Members = make(map[DeviceID]Member)
				}
				return &m, nil
			}
		}
		return nil, fmt.Errorf("identity: %s: not a member list: %w", path, err)
	}
	if m.Members == nil {
		m.Members = make(map[DeviceID]Member)
	}
	return &m, nil
}

// repairRosterShape rewrites the two collections an operator is likely to
// write in the wrong JSON shape: an empty ARRAY where a map belongs.
//
// Only the EMPTY case is repaired: "[]" means "none of them", and a
// non-empty array carries ids that would have to be reinterpreted —
// that is a roster the operator should look at, not one to guess at.
func repairRosterShape(blob []byte) ([]byte, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(blob, &raw); err != nil {
		return nil, false
	}
	changed := false
	// The list covers every map-shaped field of the roster. A field
	// added later must be added here too: a hand-written roster with
	// `"revoked_signers": []` would otherwise fail to load AT ALL, and
	// the anchor would refuse its own roster — the exact defect class
	// this repair exists for (an external tester hand-wrote
	// `"revoked_creds": []` and broke every cred command).
	for _, key := range []string{"revoked_creds", "revoked_signers", "delegations"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			continue // not an array: leave it alone
		}
		if len(arr) != 0 {
			continue // a non-empty array is not something to guess at
		}
		raw[key] = json.RawMessage(`{}`)
		changed = true
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	return out, true
}

// LoadMembersOrEmpty loads the roster, and treats a MISSING roster as an
// empty one.
//
// A fresh anchor has no roster: no member has ever joined, so nothing has
// ever been written. That is the normal state of a newly installed
// anchor, and the operator's commands must work there — `cred revoke`
// in particular, because "a credential that leaked before anyone used
// it" is exactly the case a fresh anchor is in.
//
// The account id is a hint, not a requirement: an empty roster has no
// members to attribute, and the one thing an empty roster must be able
// to carry is a revocation.
//
// A roster that exists but does not parse is still an error: the
// operator must see it, not have it papered over.
func LoadMembersOrEmpty(path, accountID string) (*MemberList, error) {
	m, err := LoadMembers(path)
	if err == nil {
		return m, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	return NewMemberList(DeviceID(accountID)), nil
}
