// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

// Tests for the roster's credential revocation list: the anchor-side set of
// credential ids that must never enrol (or re-enrol) a device, whatever the
// credential's account signature or expiry says.
//
// Why the roster holds it: the delegation record — the anchor's only other
// knowledge of a credential — is written at ENROLL time, so "leaked but not
// yet used" left nothing for RevokeDelegation to remove, and the leaked
// string stayed live for its whole TTL (a year by default). The revocation
// list is the missing half: it can name a credential nobody has ever
// presented, and it outlives the delegation record so a rotation cannot be
// undone by re-presenting the old string.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// revocationDelegation builds a well-shaped delegation for the roster.
func revocationDelegation(t *testing.T, m *MemberList, id string) Delegation {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Delegation{
		Version:      1,
		ID:           id,
		AccountID:    m.AccountID,
		DelegatorPub: pub,
		IssuedAt:     time.Now().UTC(),
		ExpiresAt:    time.Now().UTC().Add(24 * time.Hour),
		MaxDevices:   10,
	}
}

// TestPreRevokingAnUnusedCredentialRecordsIt: an id can go on the
// revocation list with no delegation record behind it — that IS the
// pre-revocation feature — and the listing must be idempotent and must
// survive a save/reload round trip (the anchor learns a revoke from the
// FILE, via its hot reload or a restart).
func TestPreRevokingAnUnusedCredentialRecordsIt(t *testing.T) {
	m := NewMemberList("acct")

	newly, err := m.RevokeCredential("cred-leaked")
	if err != nil {
		t.Fatalf("pre-revoking an unused credential failed: %v", err)
	}
	if !newly {
		t.Fatal("the first revocation was reported as not-new")
	}
	if !m.CredentialRevoked("cred-leaked") {
		t.Fatal("the revoked id is not reported as revoked")
	}
	if m.CredentialRevoked("cred-other") {
		t.Fatal("an unrelated id is reported as revoked")
	}

	// Idempotent: revoking again changes nothing and bumps no version —
	// an operator double-running the command must not churn the roster
	// version every member orders its view by.
	v := m.CurrentVersion()
	newly, err = m.RevokeCredential("cred-leaked")
	if err != nil {
		t.Fatalf("re-revoking the same id failed: %v", err)
	}
	if newly {
		t.Fatal("re-revoking reported the id as newly listed")
	}
	if m.CurrentVersion() != v {
		t.Fatal("re-revoking bumped the roster version")
	}

	// Persistence: the file is the anchor's source of truth.
	path := filepath.Join(t.TempDir(), MembersFileName)
	if err := m.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.CredentialRevoked("cred-leaked") {
		t.Fatal("the revocation did not survive the save/reload round trip")
	}
}

// TestRevokingADelegationAlsoListsTheCredential: kicking the enrolled
// members is only half of a rotation. The credential's account signature is
// still valid after the delegation is dropped, so if the id is NOT on the
// revocation list, the enroll path finds no delegation, writes a fresh one,
// and the holder of the old string walks straight back in. The id must go
// on the list as part of RevokeDelegation itself.
func TestRevokingADelegationAlsoListsTheCredential(t *testing.T) {
	m := NewMemberList("acct")
	d := revocationDelegation(t, m, "cred-one")
	if err := m.AddDelegation(d); err != nil {
		t.Fatal(err)
	}
	devPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Add(DeviceCert{
		DeviceID:  IDFromKey(devPub),
		DevicePub: devPub,
		AccountID: "acct",
		IssuedAt:  time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(time.Hour),
		SignerPub: d.DelegatorPub,
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := m.RevokeDelegation("cred-one")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("the revocation removed %d member(s), want 1", len(removed))
	}
	if !m.CredentialRevoked("cred-one") {
		t.Fatal("the revoked delegation's id is not on the revocation list: the old credential could re-enrol")
	}

	path := filepath.Join(t.TempDir(), MembersFileName)
	if err := m.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.CredentialRevoked("cred-one") {
		t.Fatal("the revocation listing did not survive the save/reload round trip")
	}
}

// TestARevokedCredentialCannotBeReAuthorised: defence in depth at the data
// structure. The enroll path consults the revocation list before anything
// else, but no OTHER code path should be able to quietly re-install a
// delegation for a revoked id either — the roster itself refuses.
func TestARevokedCredentialCannotBeReAuthorised(t *testing.T) {
	m := NewMemberList("acct")
	d := revocationDelegation(t, m, "cred-x")
	if _, err := m.RevokeCredential("cred-x"); err != nil {
		t.Fatal(err)
	}
	if err := m.AddDelegation(d); err == nil {
		t.Fatal("a delegation for a revoked credential id was accepted")
	}
	if m.CredentialRevoked("cred-x") && len(m.DelegationList()) != 0 {
		t.Fatal("the refused delegation leaked into the roster anyway")
	}
}

// TestAMembersFileWithoutARevocationListLoads: rosters written before the
// revocation list existed have no revoked_creds field. They must load
// unchanged, behave unchanged, and save back without the field (so a
// pre-feature anchor reading the same file is none the wiser).
func TestAMembersFileWithoutARevocationListLoads(t *testing.T) {
	// The exact shape the old SaveMembers wrote: no revoked_creds key.
	old := []byte(`{
  "account_id": "acct",
  "version": 4,
  "members": {},
  "delegations": {}
}`)
	path := filepath.Join(t.TempDir(), MembersFileName)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMembers(path)
	if err != nil {
		t.Fatalf("an old-format member list refused to load: %v", err)
	}
	if m.AccountID != "acct" || m.CurrentVersion() != 4 {
		t.Fatalf("the old list loaded wrong: account %s v%d", m.AccountID, m.CurrentVersion())
	}
	if m.CredentialRevoked("anything") {
		t.Fatal("an empty revocation list reported an id as revoked")
	}

	// And a freshly saved roster (empty revocation set) must not grow the
	// field either — omitempty, so the file stays readable by old anchors.
	m2 := NewMemberList("acct")
	d := revocationDelegation(t, m2, "cred-keep")
	if err := m2.AddDelegation(d); err != nil {
		t.Fatal(err)
	}
	path2 := filepath.Join(t.TempDir(), MembersFileName)
	if err := m2.SaveMembers(path2); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "revoked_creds") {
		t.Fatal("a roster with an empty revocation list grew a revoked_creds key: old anchors would still load it, but the file shape drifted")
	}
	if _, err := LoadMembers(path2); err != nil {
		t.Fatalf("the saved roster does not load: %v", err)
	}
}

// TestRevokedCredentialsListIsStable: the listing is sorted, so status
// output and tests are reproducible instead of map-order random.
func TestRevokedCredentialsListIsStable(t *testing.T) {
	m := NewMemberList("acct")
	for _, id := range []string{"cred-c", "cred-a", "cred-b"} {
		if _, err := m.RevokeCredential(id); err != nil {
			t.Fatal(err)
		}
	}
	got := m.RevokedCredentials()
	want := []string{"cred-a", "cred-b", "cred-c"}
	if len(got) != len(want) {
		t.Fatalf("the listing is %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the listing is %v, want %v", got, want)
		}
	}
}

// TestSavingDoesNotMarshalTheMutex: SaveMembers marshals a MemberList value;
// this pins that the snapshot copy carries every persisted field. It reads
// the file back as raw JSON and checks all three collections are there.
func TestSavingCarriesAllPersistedCollections(t *testing.T) {
	m := NewMemberList("acct")
	d := revocationDelegation(t, m, "cred-d")
	if err := m.AddDelegation(d); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RevokeCredential("cred-gone"); err != nil {
		t.Fatal(err)
	}
	devPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Add(DeviceCert{
		DeviceID:  IDFromKey(devPub),
		DevicePub: devPub,
		AccountID: "acct",
		IssuedAt:  time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(time.Hour),
		SignerPub: d.DelegatorPub,
	}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), MembersFileName)
	if err := m.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"account_id", "version", "members", "delegations", "revoked_creds"} {
		if _, ok := doc[field]; !ok {
			t.Fatalf("the saved roster is missing %q: the field would be lost across a restart", field)
		}
	}
}
