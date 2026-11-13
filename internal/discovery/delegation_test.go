// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

// Tests for delegated members (M4 (a)).
//
// The property that matters most: a device that joined under a team
// credential holds a certificate signed by a DELEGATOR, and the client's
// trust model only knows the account key. Without the delegation record
// travelling with the snapshot, that device verifies as a forgery and is
// dropped from EVERY view — including its own — so it is in the network
// and invisible to it.
//
// This is an internal test: it drives the client's own verification, which
// is the security-critical decision.

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestDelegatedMemberIsVisibleWithItsDelegation: the joiner and an
// unrelated member both see the delegated device.
func TestDelegatedMemberIsVisibleWithItsDelegation(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	// The account issues a team credential.
	cred, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	delegation := cred.Delegation()

	// A device joins under that credential.
	joiner, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := cred.MintCert(joiner.PublicKey(), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	if err := members.AddDelegation(*delegation); err != nil {
		t.Fatal(err)
	}

	// The roster must carry the delegation: without it the member's
	// certificate has no authorised signer.
	d, ok := members.DelegationFor(cred.Public.DelegatorPub)
	if !ok {
		t.Fatal("the roster does not carry the delegation")
	}
	if d.MaxDevices != 5 {
		t.Fatalf("delegation quota = %d, want 5", d.MaxDevices)
	}
	if n := members.DelegatedCount(cred.Public.DelegatorPub); n != 1 {
		t.Fatalf("delegated member count = %d, want 1", n)
	}

	// The snapshot carries it too.
	node := NewNode(members)
	snap := BuildSnapshot(members, node, now)
	if len(snap.Delegations) != 1 {
		t.Fatalf("the snapshot carries %d delegations, want 1", len(snap.Delegations))
	}
	if _, present := snap.Members[joiner.ID()]; !present {
		t.Fatal("the delegated device is missing from the roster")
	}

	// A client that trusts only the account key must STILL see it — the
	// delegation is what authorises the delegator's signature.
	cl := clientForAccount(t, acc)
	if err := cl.verifySnapshot(snap); err != nil {
		t.Fatalf("the client rejected the snapshot: %v", err)
	}
	if _, present := snap.Members[joiner.ID()]; !present {
		t.Fatal("the delegated member was dropped even though its delegation travelled with the snapshot")
	}
}

// clientForAccount builds a client that trusts one account.
func clientForAccount(t *testing.T, acc *identity.Account) *Client {
	t.Helper()
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cl, err := NewClient(ClientConfig{
		Addrs:      []string{"127.0.0.1:1"},
		Device:     dev,
		AccountID:  acc.ID(),
		AccountPub: []byte(acc.PublicKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// TestDelegatedMemberWithoutDelegationIsDropped: the same certificate, but
// with no authorisation to back it, must NOT be trusted — a delegated
// certificate is a forgery unless the network says otherwise.
func TestDelegatedMemberWithoutDelegationIsDropped(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	cred, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	joiner, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := cred.MintCert(joiner.PublicKey(), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}

	members := identity.NewMemberList(acc.ID())
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	// NO delegation recorded.

	node := NewNode(members)
	snap := BuildSnapshot(members, node, now)
	if len(snap.Delegations) != 0 {
		t.Fatalf("the snapshot carries delegations the roster does not have: %d", len(snap.Delegations))
	}

	// A client that trusts only the account key must drop the member: a
	// delegated certificate with no authorisation behind it is a forgery.
	cl := clientForAccount(t, acc)
	if err := cl.verifySnapshot(snap); err != nil {
		t.Fatalf("the client rejected the snapshot: %v", err)
	}
	if _, present := snap.Members[joiner.ID()]; present {
		t.Fatal("a delegated certificate with no delegation was accepted as a member")
	}
}

// TestDelegationQuotaCountsFromRoster pins the quota's source: the roster,
// not a counter — so a revoked device gives its slot back and a restarted
// node recovers the count.
func TestDelegationQuotaCountsFromRoster(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cred, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	delegation := cred.Delegation()
	members := identity.NewMemberList(acc.ID())
	if err := members.AddDelegation(*delegation); err != nil {
		t.Fatal(err)
	}
	if n := members.DelegatedCount(cred.Public.DelegatorPub); n != 0 {
		t.Fatalf("a fresh roster reports %d delegated members, want 0", n)
	}

	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := cred.MintCert(dev.PublicKey(), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	if n := members.DelegatedCount(cred.Public.DelegatorPub); n != 1 {
		t.Fatalf("delegated member count = %d, want 1", n)
	}

	// Revoking the device gives the slot back.
	if err := members.Remove(dev.ID()); err != nil {
		t.Fatal(err)
	}
	if n := members.DelegatedCount(cred.Public.DelegatorPub); n != 0 {
		t.Fatalf("after the device was revoked the count is %d, want 0", n)
	}
}

// TestExpiredDelegationDoesNotAuthorise: an expired delegation must not
// admit new devices.
func TestExpiredDelegationDoesNotAuthorise(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cred, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	delegation := cred.Delegation()
	if err := delegation.Verify(acc.PublicKey(), now); err != nil {
		t.Fatalf("a fresh delegation does not verify: %v", err)
	}
	if err := delegation.Verify(acc.PublicKey(), now.Add(2*time.Hour)); err == nil {
		t.Fatal("an expired delegation still verifies")
	}
}

// TestDelegationMustBeForThisAccount: a delegation signed by another
// account must not verify against this one.
func TestDelegationMustBeForThisAccount(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	other, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cred, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	delegation := cred.Delegation()
	if err := delegation.Verify(other.PublicKey(), now); err == nil {
		t.Fatal("a delegation verified under another account's key")
	}
	var _ ed25519.PublicKey = delegation.DelegatorPub
}
