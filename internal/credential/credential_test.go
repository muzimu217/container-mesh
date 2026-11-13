// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package credential_test

// Tests for the team join credential (M4 (a)). They pin the properties the
// design depends on:
//
//   - a credential is verifiable ONLY by the account key;
//   - a tampered credential is rejected;
//   - expiry is enforced;
//   - the secret half (delegator private key) is never part of the signed
//     body, so a verifier never needs it;
//   - a minted certificate verifies under the DELEGATOR key and NOT under
//     the account key — the account never signed it;
//   - the delegation record carries the quota and expiry the credential
//     promised.

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/identity"
)

func testAccount(t *testing.T) *identity.Account {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

// TestCredentialVerifiesAgainstAccountKey pins the basic round trip.
func TestCredentialVerifiesAgainstAccountKey(t *testing.T) {
	acc := testAccount(t)
	now := time.Now().UTC()
	c, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Verify(acc.PublicKey(), now); err != nil {
		t.Fatalf("fresh credential does not verify: %v", err)
	}
	// Another account's key must not verify it.
	other := testAccount(t)
	if err := c.Verify(other.PublicKey(), now); !errors.Is(err, credential.ErrAccountMismatch) {
		t.Fatalf("other account key verified the credential: %v", err)
	}
}

// TestCredentialRoundTripsThroughString pins the transport format: the
// string must decode back to the same halves, and the secret half must
// survive (the joiner cannot mint without it).
func TestCredentialRoundTripsThroughString(t *testing.T) {
	acc := testAccount(t)
	now := time.Now().UTC()
	c, err := credential.Create(acc, []string{"a:1", "b:2"}, time.Hour, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	s, err := credential.Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, credential.Prefix+".") {
		t.Fatalf("encoded credential lacks the %s prefix: %q", credential.Prefix, s)
	}
	if strings.Contains(s, " ") || strings.Contains(s, "\n") {
		t.Fatal("encoded credential must be a single line")
	}
	got, err := credential.Decode(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Public.ID != c.Public.ID {
		t.Fatalf("id changed across the round trip: %q vs %q", got.Public.ID, c.Public.ID)
	}
	if !got.Public.AccountPub.Equal(c.Public.AccountPub) {
		t.Fatal("account public key changed across the round trip")
	}
	if len(got.Secret.DelegatorPriv) != ed25519.PrivateKeySize {
		t.Fatalf("secret half did not survive the round trip: %d bytes", len(got.Secret.DelegatorPriv))
	}
	if err := got.Verify(acc.PublicKey(), now); err != nil {
		t.Fatalf("decoded credential does not verify: %v", err)
	}
}

// TestTamperedCredentialIsRejected: flipping any byte of the signed body
// must break verification.
func TestTamperedCredentialIsRejected(t *testing.T) {
	acc := testAccount(t)
	now := time.Now().UTC()
	c, err := credential.Create(acc, []string{"a:1"}, time.Hour, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	// Widen the quota without re-signing: the classic privilege-escalation
	// attempt against a signed blob.
	c.Public.MaxDevices = 100000
	if err := c.Verify(acc.PublicKey(), now); !errors.Is(err, credential.ErrBadSignature) {
		t.Fatalf("tampered quota verified: %v", err)
	}
}

// TestExpiredCredentialIsRejected pins the expiry.
func TestExpiredCredentialIsRejected(t *testing.T) {
	acc := testAccount(t)
	now := time.Now().UTC()
	c, err := credential.Create(acc, []string{"a:1"}, time.Hour, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Verify(acc.PublicKey(), now.Add(2*time.Hour)); !errors.Is(err, credential.ErrExpired) {
		t.Fatalf("expired credential verified: %v", err)
	}
}

// TestMalformedCredentialIsRejected: garbage must fail cleanly, not panic.
func TestMalformedCredentialIsRejected(t *testing.T) {
	for _, s := range []string{"", "nonsense", "cmesh1.!!!.???", "cmesh2.a.b", "cmesh1."} {
		if _, err := credential.Decode(s); err == nil {
			t.Fatalf("decoded malformed credential %q", s)
		}
	}
}

// TestMintedCertIsDelegatedNotAccountSigned is the core safety property:
// the device certificate minted under a credential verifies against the
// DELEGATOR key, and explicitly NOT against the account key. A verifier
// that only knows the account key must not accept it by accident.
func TestMintedCertIsDelegatedNotAccountSigned(t *testing.T) {
	acc := testAccount(t)
	now := time.Now().UTC()
	c, err := credential.Create(acc, []string{"a:1"}, time.Hour, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	devPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := c.MintCert(devPub, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.SignerPub.Equal(c.Public.DelegatorPub) {
		t.Fatal("minted certificate must name the delegator as its signer")
	}
	if err := cert.VerifySigner(c.Public.DelegatorPub); err != nil {
		t.Fatalf("certificate does not verify under the delegator key: %v", err)
	}
	if err := cert.Verify(acc.PublicKey()); !errors.Is(err, identity.ErrCertDelegated) {
		t.Fatalf("certificate verified under the ACCOUNT key: %v", err)
	}
}

// TestDelegationRecordCarriesPolicy pins that the node-side record keeps
// the quota and expiry the credential promised, and that the account
// signed it.
func TestDelegationRecordCarriesPolicy(t *testing.T) {
	acc := testAccount(t)
	now := time.Now().UTC()
	c, err := credential.Create(acc, []string{"a:1"}, 2*time.Hour, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	d := c.Delegation()
	if d.MaxDevices != 7 {
		t.Fatalf("delegation quota = %d, want 7", d.MaxDevices)
	}
	if !d.ExpiresAt.Equal(c.Public.ExpiresAt) {
		t.Fatalf("delegation expiry %s does not match the credential's %s", d.ExpiresAt, c.Public.ExpiresAt)
	}
	if err := d.Verify(acc.PublicKey(), now); err != nil {
		t.Fatalf("delegation does not verify: %v", err)
	}
	if err := d.Verify(acc.PublicKey(), now.Add(3*time.Hour)); !errors.Is(err, identity.ErrDelegationExpired) {
		t.Fatalf("expired delegation verified: %v", err)
	}
	other := testAccount(t)
	if err := d.Verify(other.PublicKey(), now); err == nil {
		t.Fatal("another account's key verified the delegation")
	}
}

// TestSecretHalfIsNotSigned pins the split: the delegator PRIVATE key must
// not be part of the signed public body, otherwise a verifier (the anchor)
// would need the secret to check the signature — and holding it would let
// the anchor mint members.
func TestSecretHalfIsNotSigned(t *testing.T) {
	acc := testAccount(t)
	now := time.Now().UTC()
	c, err := credential.Create(acc, []string{"a:1"}, time.Hour, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(c.Public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "delegator_priv") {
		t.Fatal("the signed public body must not carry the delegator private key")
	}
	// The encoded string DOES carry it (the joiner needs it) — but the
	// public half alone must be enough to verify.
	pubOnly := credential.Public{}
	if err := json.Unmarshal(blob, &pubOnly); err != nil {
		t.Fatal(err)
	}
	if len(pubOnly.Sig) == 0 {
		t.Fatal("public half carries no signature")
	}
}
