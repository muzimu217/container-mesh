// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// Tests for credential enrollment (M4 (a), the node's admission decision).
//
// What they pin:
//   - a valid credential + delegator-signed certificate admits the device;
//   - the delegation ends up on the roster (otherwise the device would be
//     invisible to every member, including itself);
//   - a tampered credential, a foreign delegator, a wrong account, an
//     expired credential and an exhausted quota are all refused, each with a
//     reason that points at the actual cause.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// encodePublicForTest renders a public half the way the wire carries it.
func encodePublicForTest(p *credential.Public) (string, error) {
	blob, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(blob), nil
}

// enrollFixture builds an account, a credential and the request a device
// would send.
func enrollFixture(t *testing.T, quota int, ttl time.Duration) (*identity.Account, *credential.Credential, discovery.EnrollRequest, *identity.MemberList) {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cred, err := credential.Create(acc, []string{"mesh.example:7000"}, ttl, quota, now)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := cred.MintCert(dev.PublicKey(), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := cred.PublicPart()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	return acc, cred, discovery.EnrollRequest{Credential: pub, Cert: *cert}, members
}

func TestEnrollAdmitsDelegatedDevice(t *testing.T) {
	acc, _, req, members := enrollFixture(t, 5, time.Hour)

	res := enrollDevice(members, acc.PublicKey(), req, time.Now().UTC())
	if !res.OK {
		t.Fatalf("a valid credential was refused: %s", res.Message)
	}
	if res.DeviceID != req.Cert.DeviceID {
		t.Fatalf("enrolled device %s, want %s", res.DeviceID, req.Cert.DeviceID)
	}
	// The device must be in the roster...
	if _, ok := members.Get(req.Cert.DeviceID); !ok {
		t.Fatal("the enrolled device is not in the roster")
	}
	// ...and the delegation must be too, or every member would drop the
	// device's certificate as a forgery.
	if _, ok := members.DelegationFor(req.Cert.SignerPub); !ok {
		t.Fatal("the delegation was not recorded: the device would be invisible to every member")
	}
	if n := members.DelegatedCount(req.Cert.SignerPub); n != 1 {
		t.Fatalf("delegated member count = %d, want 1", n)
	}
}

func TestEnrollRefusesBadRequests(t *testing.T) {
	now := time.Now().UTC()

	t.Run("tampered credential", func(t *testing.T) {
		acc, cred, req, members := enrollFixture(t, 5, time.Hour)
		// Flip the quota in the public half without re-signing.
		pub, err := credential.DecodePublic(req.Credential)
		if err != nil {
			t.Fatal(err)
		}
		pub.MaxDevices = 1000
		tampered, err := encodePublicForTest(pub)
		if err != nil {
			t.Fatal(err)
		}
		req.Credential = tampered
		res := enrollDevice(members, acc.PublicKey(), req, now)
		if res.OK {
			t.Fatal("a tampered credential was accepted")
		}
		if !strings.Contains(strings.ToLower(res.Message), "rejected") {
			t.Fatalf("the refusal does not say what is wrong: %s", res.Message)
		}
		_ = cred
	})

	t.Run("certificate from another delegator", func(t *testing.T) {
		acc, _, req, members := enrollFixture(t, 5, time.Hour)
		// A second credential has its own delegator key: a certificate
		// signed by THAT key must not pass for the first one.
		otherCred, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 5, now)
		if err != nil {
			t.Fatal(err)
		}
		foreign, err := identity.SignDelegatedCert(otherCred.Public.DelegatorPub,
			otherCred.Secret.DelegatorPriv, acc.ID(), req.Cert.DevicePub, time.Hour, now)
		if err != nil {
			t.Fatal(err)
		}
		req.Cert = *foreign
		res := enrollDevice(members, acc.PublicKey(), req, now)
		if res.OK {
			t.Fatal("a certificate signed by an unauthorised key was accepted")
		}
	})

	t.Run("expired credential", func(t *testing.T) {
		acc, _, req, members := enrollFixture(t, 5, time.Hour)
		res := enrollDevice(members, acc.PublicKey(), req, now.Add(2*time.Hour))
		if res.OK {
			t.Fatal("an expired credential was accepted")
		}
		if !strings.Contains(strings.ToLower(res.Message), "expired") {
			t.Fatalf("the refusal does not mention expiry: %s", res.Message)
		}
	})

	t.Run("quota exhausted", func(t *testing.T) {
		acc, cred, req, members := enrollFixture(t, 1, time.Hour)
		// One device joins first.
		if res := enrollDevice(members, acc.PublicKey(), req, now); !res.OK {
			t.Fatalf("the first device was refused: %s", res.Message)
		}
		// A second one must not fit into a quota of one.
		dev, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		cert, err := cred.MintCert(dev.PublicKey(), time.Hour, now)
		if err != nil {
			t.Fatal(err)
		}
		pub, err := cred.PublicPart()
		if err != nil {
			t.Fatal(err)
		}
		res := enrollDevice(members, acc.PublicKey(),
			discovery.EnrollRequest{Credential: pub, Cert: *cert}, now)
		if res.OK {
			t.Fatal("a device joined beyond the credential's quota")
		}
		if !strings.Contains(strings.ToLower(res.Message), "quota") {
			t.Fatalf("the refusal does not mention the quota: %s", res.Message)
		}
	})

	t.Run("credential for another account", func(t *testing.T) {
		_, _, req, members := enrollFixture(t, 5, time.Hour)
		other, err := identity.GenerateAccount()
		if err != nil {
			t.Fatal(err)
		}
		res := enrollDevice(members, other.PublicKey(), req, now)
		if res.OK {
			t.Fatal("a credential was accepted under another account's key")
		}
	})

	t.Run("no account key configured", func(t *testing.T) {
		_, _, req, members := enrollFixture(t, 5, time.Hour)
		res := enrollDevice(members, nil, req, now)
		if res.OK {
			t.Fatal("a node without the account key admitted a device")
		}
		if !strings.Contains(strings.ToLower(res.Message), "account-pub") {
			t.Fatalf("the refusal does not say how to fix it: %s", res.Message)
		}
	})

	t.Run("certificate not signed by a delegator", func(t *testing.T) {
		acc, _, req, members := enrollFixture(t, 5, time.Hour)
		// An ordinary account-signed certificate: the enrollment path is
		// for delegated certificates only.
		plain, err := acc.IssueCertForPub(req.Cert.DevicePub, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		req.Cert = *plain
		res := enrollDevice(members, acc.PublicKey(), req, now)
		if res.OK {
			t.Fatal("an ordinary account certificate was accepted on the enrollment path")
		}
	})
}
