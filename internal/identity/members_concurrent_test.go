package identity

import (
	"crypto/ed25519"
	"path/filepath"
	"testing"
	"time"
)

// TestARevocationSurvivesAConcurrentEnrolment is the regression test
// for the race kuku-sandbox-01 found and reproduced in order.
//
// The scenario, measured on his lab:
//
//	$ cmesh cred revoke --state <anchor> --cred-id KxQDPvIbClCE &      # revoke
//	$ cmesh join --cred "<the same credential>" --state <new> ...       # enrol, at the same time
//	credential KxQDPvIbClCE revoked.
//	  1 device(s) left the network:
//	    yps42bglyschxurd
//	enrolled as device novsbcu672uyot6a        ← the REVOKED credential was accepted
//	membership reloaded from file: 3 member(s) ← the roster went back UP
//
// The mechanism: the anchor holds the roster in memory and reloads the
// file on a 2-second tick. `cred revoke` writes the FILE from another
// process, so the in-memory list is up to two seconds BEHIND, and every
// write-back of the in-memory list puts the stale members back.
//
// The save path already knew half of this: SaveMembersMerged merges the
// file's REVOCATION LIST into the in-memory list, so the revocation
// itself survives (that was the earlier real-machine defect: two revoked
// ids, one enrolment, and the revocation list was gone).
//
// But the MEMBERS were not merged. The revocation also REMOVES the
// devices that enrolled under that credential — and those devices were
// still in the in-memory list, so the next write-back put them back.
// The revocation's whole effect — the devices leaving the network — was
// undone by the next enrolment.
//
// The rule the fix encodes: the REVOCATION is the authority on who is no
// longer a member. A device whose certificate was signed by a key that
// no longer has a delegation on the roster was removed by a revocation,
// and it must not come back because the in-memory list still had it.

func TestARevocationSurvivesAConcurrentEnrolment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)
	acc, delegPub, delegPriv, delegID := newCredentialFixture(t)

	// The roster as the anchor holds it IN MEMORY: the delegation and
	// the device that enrolled under it.
	roster := NewMemberList(acc.ID())
	addDelegation(t, roster, acc, delegPub, delegID)
	cert := delegatedCert(t, acc, delegPub, delegPriv)
	if err := roster.Add(*cert); err != nil {
		t.Fatal(err)
	}

	// The operator revokes the credential FROM ANOTHER PROCESS: the file
	// loses the delegation AND the member, and gains the revocation —
	// while the anchor's in-memory list still has both.
	onDisk := NewMemberList(acc.ID())
	addDelegation(t, onDisk, acc, delegPub, delegID)
	if err := onDisk.Add(*cert); err != nil {
		t.Fatal(err)
	}
	if err := onDisk.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	removed, err := onDisk.RevokeDelegation(delegID)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("the revoke removed %d devices, want 1", len(removed))
	}
	if err := onDisk.SaveMembers(path); err != nil {
		t.Fatal(err)
	}

	// Now the anchor's in-memory list saves — which is what the enrol
	// path does (SaveMembersMerged). The revoked device must NOT come
	// back.
	if err := roster.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}

	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Contains(cert.DeviceID) {
		t.Fatalf("the revoked device %s is back on the roster after a save: "+
			"the revocation's effect (the devices leaving the network) was undone "+
			"by the next write-back", cert.DeviceID)
	}
	if !after.CredentialRevoked(delegID) {
		t.Fatalf("the revocation of %s did not survive the save", delegID)
	}
}

// TestAMemberThatJustEnrolledIsNotDropped is the guard on the fix's
// scope.
//
// The in-memory list is RIGHT about who joined: a device that enrolled
// a moment ago has its delegation on the roster and must not be dropped
// by the merge. (A first version of the fix dropped every member the
// file did not have, and it broke the enrol path outright — the
// regression suite caught it.)
func TestAMemberThatJustEnrolledIsNotDropped(t *testing.T) {
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

	// A merged save with nothing to merge: the member stays.
	if err := roster.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}
	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Contains(cert.DeviceID) {
		t.Fatalf("the member that just enrolled was dropped by the merged save: " +
			"the in-memory list is the authority on who JOINED")
	}
}

// TestADirectlyApprovedMemberIsNeverDropped is the second guard.
//
// The account's own directly-approved devices are signed by the ACCOUNT
// key, not by a credential's delegator key. A revocation must not touch
// them: the account is not a credential, and revoking one credential
// never revokes the account.
func TestADirectlyApprovedMemberIsNeverDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)
	acc, delegPub, _, delegID := newCredentialFixture(t)

	roster := NewMemberList(acc.ID())
	addDelegation(t, roster, acc, delegPub, delegID)
	// A device the ACCOUNT approved directly.
	directDev, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	direct, err := acc.IssueCert(directDev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := roster.Add(*direct); err != nil {
		t.Fatal(err)
	}
	if err := roster.SaveMembers(path); err != nil {
		t.Fatal(err)
	}

	// A credential is revoked from another process.
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

	if err := roster.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}
	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Contains(direct.DeviceID) {
		t.Fatalf("the account's directly approved device %s was dropped by a "+
			"credential revocation: revoking one credential never revokes the account",
			direct.DeviceID)
	}
}

// TestAConcurrentEnrolmentDoesNotResurrectARevokedCredential is the
// other half of the same race, from the credential's side.
//
// The revoked credential must not be able to enrol again while the
// anchor's memory is still behind the file.
func TestAConcurrentEnrolmentDoesNotResurrectARevokedCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)
	acc, _, _, _ := newCredentialFixture(t)

	roster := NewMemberList(acc.ID())
	if err := roster.SaveMembers(path); err != nil {
		t.Fatal(err)
	}

	// The operator revokes a credential id that never enrolled anybody.
	const credID = "KxQDPvIbClCE"
	onDisk, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onDisk.RevokeCredential(credID); err != nil {
		t.Fatal(err)
	}
	if err := onDisk.SaveMembers(path); err != nil {
		t.Fatal(err)
	}

	// The anchor's memory is behind: it does not know about the
	// revocation yet.
	if roster.CredentialRevoked(credID) {
		t.Fatal("the in-memory list already knows the revocation; the test " +
			"does not reproduce the window")
	}

	// The enrolment path merges the file's revocations and saves. After
	// that, the in-memory list MUST know the revocation — otherwise the
	// next admission would still accept the credential.
	if err := roster.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}
	if !roster.CredentialRevoked(credID) {
		t.Fatalf("the merged save did not bring the revocation into memory: "+
			"the anchor would still admit %s", credID)
	}

	// And the file still has it.
	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.CredentialRevoked(credID) {
		t.Fatalf("the revocation of %s did not survive the save", credID)
	}
}

// TestTheMergeKeepsTheFileAccountID is the guard on the merge itself:
// merging must not lose the account id the file carries.
func TestTheMergeKeepsTheFileAccountID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)
	acc, _, _, _ := newCredentialFixture(t)

	// The FILE carries the account id (as an anchor's file always does).
	onDisk := NewMemberList(acc.ID())
	if err := onDisk.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	// The in-memory list has NO account id (the shape a fresh anchor's
	// operator-built list has: it was made from a credential, and the
	// credential's delegation did not carry the account id).
	roster := NewMemberList("")

	if err := roster.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}
	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.AccountID != acc.ID() {
		t.Fatalf("the account id is %q, want %q: a merged save must not "+
			"write back an empty account id (the anchor would refuse its own roster)",
			after.AccountID, acc.ID())
	}
}

// ---------------------------------------------------------------------------
// The fixture.

// newCredentialFixture makes an account, a delegation key pair, and a
// delegation id — the shape of a team credential.
func newCredentialFixture(t *testing.T) (*Account, ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	acc, err := GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	delegPub, delegPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return acc, delegPub, delegPriv, "KxQDPvIbClCE"
}

// addDelegation records a delegation on a roster.
func addDelegation(t *testing.T, m *MemberList, acc *Account, delegPub ed25519.PublicKey, delegID string) {
	t.Helper()
	if err := m.AddDelegation(Delegation{
		ID: delegID, AccountID: acc.ID(), DelegatorPub: delegPub,
		MaxDevices: 5, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// delegatedCert mints a device whose certificate was signed by the
// delegation key — a device that enrolled under a team credential.
func delegatedCert(t *testing.T, acc *Account, delegPub ed25519.PublicKey, delegPriv ed25519.PrivateKey) *DeviceCert {
	t.Helper()
	dev, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := SignDelegatedCert(delegPub, delegPriv, acc.ID(), dev.PublicKey(), time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
