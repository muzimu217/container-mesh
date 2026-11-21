package discovery

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// aDelegation builds a delegation record with a real (throwaway) key —
// the roster insists on a 32-byte delegator public key.
func aDelegation(t *testing.T, id string, issued, expires time.Time) identity.Delegation {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return identity.Delegation{
		Version:      1,
		ID:           id,
		AccountID:    "acct",
		DelegatorPub: pub,
		IssuedAt:     issued,
		ExpiresAt:    expires,
		MaxDevices:   10,
	}
}

// TestTheSnapshotOmitsExpiredDelegations is the regression test for the
// noise the external tester reported.
//
// The roster keeps a delegation record forever (nothing reaps it), and
// the snapshot used to carry every one of them. A client that receives a
// lapsed delegation rejects it and logs a warning — so EVERY client
// logged that warning on EVERY snapshot, for every lapsed credential,
// forever.
//
// The external tester saw it on a forty-member network carrying four
// stale delegations: four warnings per snapshot, saying nothing an
// operator could act on.
//
// The expired certificates were already left out of the snapshot (a lapsed
// cert proves nothing). The delegations now get the same treatment: what
// travels is what is still usable. The roster keeps the record — this
// only decides what LEAVES the anchor.
func TestTheSnapshotOmitsExpiredDelegations(t *testing.T) {
	members := identity.NewMemberList("acct")

	live := aDelegation(t, "live-cred", time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour))
	lapsed := aDelegation(t, "lapsed-cred", time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-time.Hour))
	if err := members.AddDelegation(live); err != nil {
		t.Fatal(err)
	}
	if err := members.AddDelegation(lapsed); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	snap := BuildSnapshot(members, NewNode(members), now)

	for _, d := range snap.Delegations {
		if d.ID == lapsed.ID {
			t.Fatalf("the snapshot carries the lapsed delegation %s: every client rejects it and logs a warning, once per snapshot, forever", lapsed.ID)
		}
	}
	found := false
	for _, d := range snap.Delegations {
		if d.ID == live.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the snapshot dropped the LIVE delegation: it still authorises new devices")
	}
}

// TestTheRosterStillKeepsTheExpiredDelegation: filtering what TRAVELS
// must not delete what the anchor keeps. The record is how the anchor
// knows a credential once existed, and how a revoke can name it.
func TestTheRosterStillKeepsTheExpiredDelegation(t *testing.T) {
	members := identity.NewMemberList("acct")
	lapsed := aDelegation(t, "lapsed-cred", time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-time.Hour))
	if err := members.AddDelegation(lapsed); err != nil {
		t.Fatal(err)
	}

	snap := BuildSnapshot(members, NewNode(members), time.Now().UTC())
	for _, d := range snap.Delegations {
		if d.ID == lapsed.ID {
			t.Fatal("the snapshot carries the lapsed delegation")
		}
	}

	// The roster still has it.
	if _, ok := members.DelegationByID(lapsed.ID); !ok {
		t.Fatal("the roster dropped the lapsed delegation: the record is what a revoke names")
	}
}
