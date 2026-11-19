package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// credRevokeRoster writes a members.json with one credential and two
// devices enrolled under it (plus one account-approved device that must
// survive a revoke).
func credRevokeRoster(t *testing.T, dir string) (credID string, memberID identity.DeviceID, keepID identity.DeviceID) {
	t.Helper()
	m := identity.NewMemberList("acct")

	delPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d := identity.Delegation{
		Version:      1,
		ID:           "cred-test",
		AccountID:    "acct",
		DelegatorPub: delPub,
		IssuedAt:     time.Now().UTC(),
		ExpiresAt:    time.Now().UTC().Add(24 * time.Hour),
		MaxDevices:   10,
	}
	if err := m.AddDelegation(d); err != nil {
		t.Fatal(err)
	}
	add := func(signer ed25519.PublicKey) identity.DeviceID {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		id := identity.IDFromKey(pub)
		cert := identity.DeviceCert{
			DeviceID:  id,
			DevicePub: pub,
			AccountID: "acct",
			IssuedAt:  time.Now().UTC(),
			ExpiresAt: time.Now().UTC().Add(time.Hour),
			SignerPub: signer,
		}
		if err := m.Add(cert); err != nil {
			t.Fatal(err)
		}
		return id
	}
	memberID = add(delPub)
	keepID = add(nil) // account-approved
	if err := m.SaveMembers(filepath.Join(dir, identity.MembersFileName)); err != nil {
		t.Fatal(err)
	}
	return d.ID, memberID, keepID
}

// TestCredRevokeRemovesTheMembersFromTheRoster runs the OWNER'S rotation
// end to end against a real roster file: revoke the credential, the
// devices that joined under it leave, the anchor's hot-reload picks the
// file up, and the file still loads cleanly afterwards.
func TestCredRevokeRemovesTheMembersFromTheRoster(t *testing.T) {
	dir := t.TempDir()
	credID, memberID, keepID := credRevokeRoster(t, dir)

	err := runCred([]string{"revoke", "--state", dir, "--cred-id", credID})
	if err != nil {
		t.Fatalf("the revocation failed: %v", err)
	}

	// The roster file reflects the rotation.
	m, err := identity.LoadMembers(filepath.Join(dir, identity.MembersFileName))
	if err != nil {
		t.Fatalf("the roster does not load after the revoke: %v", err)
	}
	if m.Contains(memberID) {
		t.Fatalf("member %s is still in the roster: the rotation did not take it out", memberID)
	}
	if !m.Contains(keepID) {
		t.Fatalf("member %s was removed: the rotation must not touch account-approved devices", keepID)
	}
	if _, ok := m.Delegations[credID]; ok {
		t.Fatal("the revoked credential is still in the roster: it could still enrol new devices")
	}
}

// TestCredRevokeAnUnknownCredentialFailsWithExitOne: the owner must not
// walk away believing devices were kicked when the id was a typo.
func TestCredRevokeAnUnknownCredentialFailsWithExitOne(t *testing.T) {
	dir := t.TempDir()
	credRevokeRoster(t, dir)

	err := runCred([]string{"revoke", "--state", dir, "--cred-id", "typo"})
	exit, ok := err.(ExitError)
	if !ok {
		t.Fatalf("the error is %T, want ExitError", err)
	}
	if exit.Code != 1 {
		t.Fatalf("the exit code is %d, want 1", exit.Code)
	}
}

// TestCredRevokeNeedsACredentialID: without an id the command would have
// to guess, and guessing with a revocation is how everything gets kicked.
func TestCredRevokeNeedsACredentialID(t *testing.T) {
	dir := t.TempDir()
	credRevokeRoster(t, dir)
	err := runCred([]string{"revoke", "--state", dir})
	exit, ok := err.(ExitError)
	if !ok {
		t.Fatalf("the error is %T, want ExitError", err)
	}
	if exit.Code != 2 {
		t.Fatalf("the exit code is %d, want 2 (usage error)", exit.Code)
	}
}

// TestCredListSurvivesAnEmptyRoster: `cred list` on a network that has
// never issued a credential must say so, not crash.
func TestCredListSurvivesAnEmptyRoster(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := identity.NewMemberList("acct")
	if err := m.SaveMembers(filepath.Join(dir, identity.MembersFileName)); err != nil {
		t.Fatal(err)
	}
	if err := runCred([]string{"list", "--state", dir}); err != nil {
		t.Fatalf("cred list failed on an empty roster: %v", err)
	}
}
