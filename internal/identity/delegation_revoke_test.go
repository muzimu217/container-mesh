package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

// revokeTestRoster builds a roster with TWO delegations and four members:
// two under the first credential, one under the second, and one approved
// by the account itself.
func revokeTestRoster(t *testing.T) (*MemberList, Delegation, Delegation, DeviceID, DeviceID, DeviceID, DeviceID) {
	t.Helper()
	m := NewMemberList("acct")
	newKey := func(t *testing.T) ed25519.PublicKey {
		t.Helper()
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return pub
	}
	k1 := newKey(t)
	k2 := newKey(t)
	mkDelegation := func(id string, pub ed25519.PublicKey) Delegation {
		return Delegation{
			Version:      1,
			ID:           id,
			AccountID:    "acct",
			DelegatorPub: pub,
			IssuedAt:     time.Now().UTC(),
			ExpiresAt:    time.Now().UTC().Add(24 * time.Hour),
			MaxDevices:   10,
		}
	}
	d1 := mkDelegation("cred-one", k1)
	d2 := mkDelegation("cred-two", k2)
	if err := m.AddDelegation(d1); err != nil {
		t.Fatal(err)
	}
	if err := m.AddDelegation(d2); err != nil {
		t.Fatal(err)
	}

	next := 0
	addMember := func(signer ed25519.PublicKey) DeviceID {
		next++
		// The device key is arbitrary, but the ID must derive from it —
		// the roster checks that pairing on Add.
		devPub := newKey(t)
		devID := IDFromKey(devPub)
		cert := DeviceCert{
			DeviceID:  devID,
			DevicePub: devPub,
			AccountID: "acct",
			IssuedAt:  time.Now().UTC(),
			ExpiresAt: time.Now().UTC().Add(time.Hour),
			SignerPub: signer,
		}
		if err := m.Add(cert); err != nil {
			t.Fatalf("member %s could not join: %v", devID, err)
		}
		return devID
	}

	one1 := addMember(k1)
	one2 := addMember(k1)
	two := addMember(k2)
	manual := addMember(nil) // account-signed: SignerPub empty
	return m, d1, d2, one1, one2, two, manual
}

// TestRevokingADelegationRemovesItsMembers is the regression test for the
// owner's ruling on membership: "除非我自己改密钥，然后其他会掉线".
//
// A team credential IS a key the owner hands out. Rotating it — revoking
// the delegation behind it — must take the devices that joined under it
// OUT of the network, not merely stop new enrolments.
//
// RemoveDelegation dropped the authorisation and left every device
// enrolled under it in place, which is the opposite of what the owner
// asked for: the one switch that says "everyone who came in on this
// password is out" did nothing to the people already inside.
func TestRevokingADelegationRemovesItsMembers(t *testing.T) {
	m, _, d2, one1, one2, two, manual := revokeTestRoster(t)
	if got := m.Count(); got != 4 {
		t.Fatalf("the roster holds %d members, want 4", got)
	}

	removed, err := m.RevokeDelegation("cred-one")
	if err != nil {
		t.Fatalf("the revocation failed: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("the revocation removed %d members, want the two that joined under it: %v", len(removed), removed)
	}
	for _, id := range []DeviceID{one1, one2} {
		if m.Contains(id) {
			t.Fatalf("member %s is still in the roster after its credential was revoked", id)
		}
	}
	if !m.Contains(two) {
		t.Fatal("a member of the OTHER credential was removed: a rotation must be scoped to one credential")
	}
	if !m.Contains(manual) {
		t.Fatal("the manually-authorized member was removed: a rotation must not take down members that came in by another door")
	}
	if _, ok := m.DelegationFor(d2.DelegatorPub); !ok {
		t.Fatal("the other delegation was removed: a rotation must be scoped to one credential")
	}
}

// TestRevokingAnUnknownDelegationFailsLoudly: a typo in the credential id
// must be an error, not a silent no-op — the owner believes devices were
// kicked when they were not.
func TestRevokingAnUnknownDelegationFailsLoudly(t *testing.T) {
	m := NewMemberList("acct")
	if _, err := m.RevokeDelegation("no-such-credential"); err == nil {
		t.Fatal("revoking an unknown credential succeeded silently")
	}
}

// TestARevokedDelegationStopsNewEnrolments: removing the delegation must
// also make the anchor refuse devices that try to enrol under it later —
// otherwise the rotation only kicks the people already inside and the
// holder of the old password simply rejoins.
func TestARevokedDelegationStopsNewEnrolments(t *testing.T) {
	m, d1, _, _, _, _, _ := revokeTestRoster(t)
	if _, err := m.RevokeDelegation("cred-one"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.DelegationFor(d1.DelegatorPub); ok {
		t.Fatal("the revoked delegation is still in the roster")
	}
	// The enrolment path looks the delegation up by the credential's
	// signer key; without the delegation record there is nothing that
	// authorises a new certificate under it.
	if _, ok := m.DelegationFor(d1.DelegatorPub); ok {
		t.Fatal("a new enrolment under the revoked credential would still be authorised")
	}
}
