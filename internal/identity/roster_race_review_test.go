package identity

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The roster's shape and its merge behaviour, checked against the ways
// a roster file can be WRONG.
//
// These came from an adversarial review of the revocation-race fix, and
// they are kept here because each one is a way the file can be
// hand-edited, half-written, or written by a second process — and the
// roster is the anchor's only copy of who is a member.

// TestTheRosterAnOldFileWithoutRevokedSignersLoads: a roster written
// before the field existed must load cleanly.
func TestTheRosterAnOldFileWithoutRevokedSignersLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)
	blob := `{
  "account_id": "acct",
  "version": 3,
  "members": {},
  "delegations": {},
  "revoked_creds": {"old": "2026-01-01T00:00:00Z"}
}`
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMembers(path)
	if err != nil {
		t.Fatalf("an old roster without revoked_signers did not load: %v", err)
	}
	if len(m.RevokedSigners) != 0 {
		t.Fatalf("expected no revoked signers, got %v", m.RevokedSigners)
	}
}

// TestTheRosterWithAnEmptyRevokedSignersArrayLoads: a hand-written
// roster with "revoked_signers": [] must load.
//
// This is the SAME class of bug that "revoked_creds": [] caused (an
// external tester hand-wrote it and broke every cred command), and the
// shape repair must cover the field added later — otherwise the anchor
// refuses its own roster.
func TestTheRosterWithAnEmptyRevokedSignersArrayLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)
	blob := `{
  "account_id": "acct",
  "version": 1,
  "members": {},
  "delegations": {},
  "revoked_signers": []
}`
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMembers(path)
	if err != nil {
		t.Fatalf("a hand-written roster with an empty revoked_signers array did not load: %v", err)
	}
	if len(m.RevokedSigners) != 0 {
		t.Fatalf("expected no revoked signers, got %v", m.RevokedSigners)
	}
}

// TestTheRosterKeepsAMemberOfAnExpiredDelegation: EXPIRED is not
// REVOKED. A delegation that simply ran out leaves its members alone.
func TestTheRosterKeepsAMemberOfAnExpiredDelegation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)
	acc, delegPub, delegPriv, delegID := newCredentialFixture(t)
	roster := NewMemberList(acc.ID())
	addDelegation(t, roster, acc, delegPub, delegID)
	cert := delegatedCert(t, acc, delegPub, delegPriv)
	if err := roster.Add(*cert); err != nil {
		t.Fatal(err)
	}
	if err := roster.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	if err := roster.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}
	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Contains(cert.DeviceID) {
		t.Fatal("a member of a merely EXPIRED delegation was dropped by the merged save")
	}
}

// TestTheRosterDropsOnlyTheRevokedCredentialsMembers: the drop must
// be exactly scoped — the revoked credential's members go, and every
// other member stays.
func TestTheRosterDropsOnlyTheRevokedCredentialsMembers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)
	acc, delegPubA, delegPrivA, delegIDA := newCredentialFixture(t)
	_, delegPubB, delegPrivB, _ := newCredentialFixture(t)
	const delegIDB = "OTHER-CRED-ID"

	roster := NewMemberList(acc.ID())
	addDelegation(t, roster, acc, delegPubA, delegIDA)
	if err := roster.AddDelegation(Delegation{
		ID: delegIDB, AccountID: acc.ID(), DelegatorPub: delegPubB,
		MaxDevices: 5, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	certA := delegatedCert(t, acc, delegPubA, delegPrivA)
	certB := delegatedCert(t, acc, delegPubB, delegPrivB)
	if err := roster.Add(*certA); err != nil {
		t.Fatal(err)
	}
	if err := roster.Add(*certB); err != nil {
		t.Fatal(err)
	}
	if err := roster.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	onDisk, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := onDisk.RevokeDelegation(delegIDA)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("revoke removed %d, want 1", len(removed))
	}
	if err := onDisk.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	if err := roster.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}
	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Contains(certA.DeviceID) {
		t.Fatalf("the revoked credential's member %s came back", certA.DeviceID)
	}
	if !after.Contains(certB.DeviceID) {
		t.Fatalf("the OTHER (never revoked) credential's member %s was dropped", certB.DeviceID)
	}
}

// TestTheRosterVersionMovesForwardWhenAMemberIsDropped: the drop must
// bump the version.
//
// The version is what orders the updates — a client ignores a snapshot
// whose version is not newer than the one it has. A drop that did not
// bump it wrote the roster's version BACKWARDS (the file was already at
// a higher one), every client treated the snapshot as stale and
// IGNORED it, and the revocation never reached the network even though
// it was in the file.
func TestTheRosterVersionMovesForwardWhenAMemberIsDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)
	acc, delegPub, delegPriv, delegID := newCredentialFixture(t)
	roster := NewMemberList(acc.ID())
	addDelegation(t, roster, acc, delegPub, delegID)
	cert := delegatedCert(t, acc, delegPub, delegPriv)
	if err := roster.Add(*cert); err != nil {
		t.Fatal(err)
	}
	if err := roster.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	onDisk, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onDisk.RevokeDelegation(delegID); err != nil {
		t.Fatal(err)
	}
	if err := onDisk.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	fileVersion, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := roster.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}
	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.CurrentVersion() < fileVersion.CurrentVersion() {
		t.Fatalf("the roster's version went BACKWARDS: file v%d, written v%d — "+
			"every client would treat the snapshot as stale and ignore it",
			fileVersion.CurrentVersion(), after.CurrentVersion())
	}
}

// TestTheRosterRevokedSignerKeyEncodingIsConsistent: the key encoding
// the set uses must be the one the certificates already use, or the
// drop would never match a member.
func TestTheRosterRevokedSignerKeyEncodingIsConsistent(t *testing.T) {
	acc, delegPub, delegPriv, _ := newCredentialFixture(t)
	cert := delegatedCert(t, acc, delegPub, delegPriv)
	encoded := base64.StdEncoding.EncodeToString(cert.SignerPub)
	blob, err := json.Marshal(cert)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), encoded) {
		t.Fatalf("the certificate's JSON does not carry the base64 signer key %q — "+
			"the roster's revoked-signer set would never match a member", encoded)
	}
}

// TestTheRosterRevokedSignersSurviveASaveRoundTrip: the set is
// persisted, so a restart keeps the revocation's effect.
func TestTheRosterRevokedSignersSurviveASaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)
	acc, delegPub, _, delegID := newCredentialFixture(t)
	m := NewMemberList(acc.ID())
	addDelegation(t, m, acc, delegPub, delegID)
	if _, err := m.RevokeDelegation(delegID); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	m2, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.RevokedSigners) != 1 {
		t.Fatalf("the revoked signers did not survive the round trip: %v", m2.RevokedSigners)
	}
}

// TestTheRosterSurvivesConcurrentSavesAndRevokes: the roster is
// written by the anchor AND by the operator's CLI, from two processes.
// The race detector must stay quiet.
func TestTheRosterSurvivesConcurrentSavesAndRevokes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)
	acc, delegPub, delegPriv, delegID := newCredentialFixture(t)
	m := NewMemberList(acc.ID())
	addDelegation(t, m, acc, delegPub, delegID)
	cert := delegatedCert(t, acc, delegPub, delegPriv)
	if err := m.Add(*cert); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = m.SaveMembersMerged(path)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = m.SaveMembers(path)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_, _ = m.RevokeDelegation(delegID)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = m.CredentialRevoked(delegID)
			_ = m.Contains(cert.DeviceID)
			_ = m.IDs()
		}
	}()
	wg.Wait()
}
