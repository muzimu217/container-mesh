// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testPass = "correct-horse-battery"

// newDeviceForTest generates a device with a fixed-ish passphrase workflow.
func newDeviceForTest(t *testing.T) *Device {
	t.Helper()
	d, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDeviceIDDeterministicAndWellFormed(t *testing.T) {
	d := newDeviceForTest(t)

	// Same key -> same ID, always.
	again := IDFromKey(d.PublicKey())
	if again != d.ID() {
		t.Fatalf("ID not deterministic: %s vs %s", d.ID(), again)
	}

	// Shape: exactly 16 lowercase base32 characters.
	if len(d.ID()) != DeviceIDLen {
		t.Fatalf("ID length = %d, want %d", len(d.ID()), DeviceIDLen)
	}
	if !d.ID().Valid() {
		t.Fatalf("ID %q failed Valid", d.ID())
	}

	// Different key -> different ID.
	other := newDeviceForTest(t)
	if other.ID() == d.ID() {
		t.Fatal("two different keys produced the same ID")
	}
}

func TestDeviceIDValidRejectsMalformed(t *testing.T) {
	bad := []DeviceID{
		"",                  // empty
		"abc",               // too short
		"abcdefghijklmnopq", // 17 chars
		"ABCDEFGHIJKLMNOp",  // uppercase
		"abcdefghijklmn0",   // 0 is not in the base32 alphabet chosen
		"abcdefghijklmn1",   // 1 is not in the alphabet either
	}
	for _, id := range bad {
		if id.Valid() {
			t.Errorf("ID %q should be invalid", id)
		}
	}
	if !DeviceID("abcdefghijklmnop").Valid() {
		t.Error("a well-formed 16-char lowercase ID was rejected")
	}
}

func TestDeviceSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DeviceFileName)

	d := newDeviceForTest(t)
	if err := d.Save(path, testPass); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := LoadDevice(path, testPass)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ID() != d.ID() {
		t.Fatalf("ID changed across round trip: %s vs %s", got.ID(), d.ID())
	}
	if !got.PublicKey().Equal(d.PublicKey()) {
		t.Fatal("public key changed across round trip")
	}
	// The private key must survive intact: sign with the loaded key and
	// verify against the original public key.
	msg := []byte("round trip")
	if !VerifyDevice(d.PublicKey(), msg, got.Sign(msg)) {
		t.Fatal("signature from reloaded key does not verify")
	}
}

func TestDeviceWrongPassphraseRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DeviceFileName)

	d := newDeviceForTest(t)
	if err := d.Save(path, testPass); err != nil {
		t.Fatal(err)
	}
	_, err := LoadDevice(path, "wrong-passphrase")
	if err == nil {
		t.Fatal("wrong passphrase was accepted")
	}
	// The error must carry a human-readable message, not a raw AEAD failure
	// the CLI would have to string-match.
	if err.Error() == "" {
		t.Fatal("empty error message")
	}
}

func TestDeviceShortPassphraseRefused(t *testing.T) {
	d := newDeviceForTest(t)
	if err := d.Save(filepath.Join(t.TempDir(), "x"), "short"); err == nil {
		t.Fatal("short passphrase was accepted")
	}
}

func TestDeviceFileIsEncryptedAndLockedDown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DeviceFileName)

	d := newDeviceForTest(t)
	if err := d.Save(path, testPass); err != nil {
		t.Fatal(err)
	}

	// Permissions: 0600, because the file holds a private key.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 0600", fi.Mode().Perm())
	}

	// The raw file must not contain the private key in the clear.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	privJSON, _ := json.Marshal(d.priv)
	if containsBytes(raw, privJSON) {
		t.Fatal("private key appears in plaintext in the identity file")
	}
	if containsBytes(raw, []byte(`"priv"`)) {
		t.Fatal("unencrypted payload field name present in identity file")
	}
}

func containsBytes(hay, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// --- account & certificates -------------------------------------------------

func TestCertIssueAndVerify(t *testing.T) {
	acc, err := GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	dev := newDeviceForTest(t)

	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.Verify(acc.PublicKey()); err != nil {
		t.Fatalf("fresh cert failed to verify: %v", err)
	}
}

func TestCertExpired(t *testing.T) {
	acc, _ := GenerateAccount()
	dev := newDeviceForTest(t)

	// A legitimately-signed expired cert must be created by issuing a tiny
	// positive TTL and letting it lapse; mutating ExpiresAt afterwards would
	// break the signature (that is TestCertTampered's job).
	cert, err := acc.IssueCert(dev, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := cert.Verify(acc.PublicKey()); err == nil {
		t.Fatal("expired cert verified")
	}
}

func TestCertNonPositiveTTLError(t *testing.T) {
	acc, _ := GenerateAccount()
	dev := newDeviceForTest(t)
	if _, err := acc.IssueCert(dev, 0); err == nil {
		t.Fatal("zero ttl accepted")
	}
	if _, err := acc.IssueCert(dev, -time.Hour); err == nil {
		t.Fatal("negative ttl accepted")
	}
}

func TestCertTampered(t *testing.T) {
	acc, _ := GenerateAccount()
	dev := newDeviceForTest(t)

	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Extend the validity without re-signing.
	cert.ExpiresAt = cert.ExpiresAt.Add(24 * time.Hour)
	if err := cert.Verify(acc.PublicKey()); err == nil {
		t.Fatal("tampered cert verified")
	}
}

func TestCertSignedByWrongAccount(t *testing.T) {
	signer, _ := GenerateAccount()
	other, _ := GenerateAccount()
	dev := newDeviceForTest(t)

	cert, err := signer.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.Verify(other.PublicKey()); err == nil {
		t.Fatal("cert verified against the wrong account key")
	}
}

func TestCertIDKeyMismatch(t *testing.T) {
	acc, _ := GenerateAccount()
	dev := newDeviceForTest(t)

	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Claim a different device ID than the key derives.
	cert.DeviceID = "aaaaaaaaaaaaaaaa"
	if err := cert.Verify(acc.PublicKey()); err == nil {
		t.Fatal("cert with mismatched ID verified")
	}
}

func TestCertSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev"+CertSuffix)

	acc, _ := GenerateAccount()
	dev := newDeviceForTest(t)
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveCert(cert, path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCert(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Verify(acc.PublicKey()); err != nil {
		t.Fatalf("reloaded cert failed to verify: %v", err)
	}
}

// --- member list ------------------------------------------------------------

func newAccountWithDevice(t *testing.T) (*Account, *Device, DeviceCert) {
	t.Helper()
	acc, err := GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	dev := newDeviceForTest(t)
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return acc, dev, *cert
}

func TestMemberListAddRemoveVersioning(t *testing.T) {
	acc, _, cert := newAccountWithDevice(t)
	ml := NewMemberList(acc.ID())

	if err := ml.Add(cert); err != nil {
		t.Fatal(err)
	}
	v1 := ml.CurrentVersion()
	if v1 != 1 {
		t.Fatalf("version after first add = %d, want 1", v1)
	}
	if !ml.Contains(cert.DeviceID) {
		t.Fatal("member not contained after add")
	}

	if err := ml.Remove(cert.DeviceID); err != nil {
		t.Fatal(err)
	}
	if ml.CurrentVersion() != v1+1 {
		t.Fatalf("version did not bump on remove: %d", ml.CurrentVersion())
	}
	if ml.Contains(cert.DeviceID) {
		t.Fatal("revoked device still contained")
	}

	// Removing a non-member is an error, not a silent no-op: callers use it
	// to detect desync.
	if err := ml.Remove(cert.DeviceID); err == nil {
		t.Fatal("double remove succeeded")
	}
}

func TestMemberListAddConflictingKey(t *testing.T) {
	acc, _, cert := newAccountWithDevice(t)
	ml := NewMemberList(acc.ID())
	if err := ml.Add(cert); err != nil {
		t.Fatal(err)
	}

	// Same ID, different key: the ID is derived from the key, so this pair
	// is internally inconsistent and must be refused.
	forged := cert
	other := newDeviceForTest(t)
	forged.DevicePub = other.PublicKey()
	if err := ml.Add(forged); err == nil {
		t.Fatal("conflicting key for an existing device ID was accepted")
	}
}

func TestMemberListRenewalKeepsMetadata(t *testing.T) {
	acc, dev, cert := newAccountWithDevice(t)
	ml := NewMemberList(acc.ID())
	if err := ml.Add(cert); err != nil {
		t.Fatal(err)
	}

	renewed, err := acc.IssueCert(dev, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := ml.Get(cert.DeviceID)
	if err := ml.Add(*renewed); err != nil {
		t.Fatalf("renewal rejected: %v", err)
	}
	after, _ := ml.Get(cert.DeviceID)
	if !after.AddedAt.Equal(before.AddedAt) {
		t.Fatal("renewal reset AddedAt")
	}
	if ml.Count() != 1 {
		t.Fatalf("renewal changed member count: %d", ml.Count())
	}
}

func TestMemberListSaveLoadRoundtrip(t *testing.T) {
	acc, _, cert := newAccountWithDevice(t)
	ml := NewMemberList(acc.ID())
	if err := ml.Add(cert); err != nil {
		t.Fatal(err)
	}
	v := ml.CurrentVersion()

	path := filepath.Join(t.TempDir(), MembersFileName)
	if err := ml.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentVersion() != v {
		t.Fatalf("version changed across round trip: %d vs %d", got.CurrentVersion(), v)
	}
	if !got.Contains(cert.DeviceID) {
		t.Fatal("membership lost across round trip")
	}
	if got.AccountID != acc.ID() {
		t.Fatal("account changed across round trip")
	}
}

func TestMemberListFileIsLockedDown(t *testing.T) {
	acc, _, cert := newAccountWithDevice(t)
	ml := NewMemberList(acc.ID())
	if err := ml.Add(cert); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), MembersFileName)
	if err := ml.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("member list mode = %o, want 0600", fi.Mode().Perm())
	}
}

// TestMembershipNotVersionGovernsTeardown pins the semantic from review
// note 1: the disconnect test is CONTAINS, not version comparison. A version
// bump from an unrelated change must not look like a revocation.
func TestMembershipNotVersionGovernsTeardown(t *testing.T) {
	acc, _, cert := newAccountWithDevice(t)
	ml := NewMemberList(acc.ID())
	if err := ml.Add(cert); err != nil {
		t.Fatal(err)
	}

	// An unrelated join bumps the version...
	_, _, otherCert := newAccountWithDeviceForAccount(t, acc)
	if err := ml.Add(otherCert); err != nil {
		t.Fatal(err)
	}
	// ...but the original member is still a member, and that is the only
	// thing a session teardown may consult.
	if !ml.Contains(cert.DeviceID) {
		t.Fatal("unrelated version bump caused a member to appear revoked")
	}

	// A real revocation is exactly one thing: absence from the list.
	if err := ml.Remove(cert.DeviceID); err != nil {
		t.Fatal(err)
	}
	if ml.Contains(cert.DeviceID) {
		t.Fatal("revoked member still contained")
	}
}

// newAccountWithDeviceForAccount issues a cert for a fresh device under an
// existing account.
func newAccountWithDeviceForAccount(t *testing.T, acc *Account) (*Account, *Device, DeviceCert) {
	t.Helper()
	dev := newDeviceForTest(t)
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return acc, dev, *cert
}

func TestAccountSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AccountFileName)

	acc, err := GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	if err := acc.Save(path, testPass); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAccount(path, testPass)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != acc.ID() {
		t.Fatalf("account ID changed: %s vs %s", got.ID(), acc.ID())
	}

	// The reloaded account must still be able to issue verifying certs.
	dev := newDeviceForTest(t)
	cert, err := got.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.Verify(acc.PublicKey()); err != nil {
		t.Fatalf("cert from reloaded account failed: %v", err)
	}
}
