// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// The end-to-end proof of the team credential: ONE command, a device that
// was never signed by the account owner, joins the network and becomes
// visible to the other members.
//
// The test runs the real pieces: a real relay with the enrollment handler
// installed, a real credential, and the real client path.

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/relay"
)

// TestJoinWithCredentialAdmitsDevice: a device that has never been signed
// by the account owner joins with a credential, and the anchor's roster
// knows it.
func TestJoinWithCredentialAdmitsDevice(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	// The account issues a credential that points at the anchor below.
	// The address is only known after the server starts, so the credential
	// is created first and re-issued with the real address.
	srv := relay.New(relay.DefaultConfig())
	members := identity.NewMemberList(acc.ID())
	srv.EnableDiscovery(members)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.StopDiscovery()

	cred, err := credential.Create(acc, []string{ts.URL}, time.Hour, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	// The credential must be a single string the member can be handed.
	credStr, err := credential.Encode(cred)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := credential.Decode(credStr)
	if err != nil {
		t.Fatalf("the encoded credential does not decode: %v", err)
	}
	cred = parsed

	// The anchor admits devices that present the credential.
	membersPath := filepath.Join(t.TempDir(), "members.json")
	if err := members.SaveMembers(membersPath); err != nil {
		t.Fatal(err)
	}
	installEnrollHandler(srv, membersPath, members, acc.PublicKey())

	// A device with no identity yet: the join must create one.
	dir := t.TempDir()
	dev, err := ensureDeviceIdentity(dir, "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(identity.IdentityPath(dir, identity.DeviceFileName)); err != nil {
		t.Fatalf("the device identity was not created: %v", err)
	}

	// The client half: mint and enroll.
	if err := enrollWithCredential(context.Background(), cred, dev); err != nil {
		t.Fatalf("enrollment failed: %v", err)
	}

	// The device must be in the anchor's roster...
	if _, ok := members.Get(dev.ID()); !ok {
		t.Fatal("the enrolled device is not in the roster")
	}
	// ...with the delegation recorded, or no member could verify it...
	if _, ok := members.DelegationFor(cred.Public.DelegatorPub); !ok {
		t.Fatal("the delegation was not recorded")
	}
	// ...and the roster must survive a reload from disk (the node reloads
	// the file every couple of seconds; a member that is not on disk would
	// vanish from the network).
	reloaded, err := identity.LoadMembers(membersPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Get(dev.ID()); !ok {
		t.Fatal("the enrolled device did not survive the roster reload")
	}
	if _, ok := reloaded.DelegationFor(cred.Public.DelegatorPub); !ok {
		t.Fatal("the delegation did not survive the roster reload")
	}
}

// TestEnrollRefusedWhenNodeLacksAccountKey: a node that cannot verify a
// credential must not answer enrollment requests at all.
func TestEnrollRefusedWhenNodeLacksAccountKey(t *testing.T) {
	srv := relay.New(relay.DefaultConfig())
	members := identity.NewMemberList("acct")
	srv.EnableDiscovery(members)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.StopDiscovery()

	// No handler installed: the route is not even mounted.
	resp, err := ts.Client().Post(ts.URL+"/mesh/enroll", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatalf("a node without an enrollment handler answered /mesh/enroll with %d", resp.StatusCode)
	}
}

// TestCredentialJoinRejectsBothInputs: the two ways in are exclusive.
func TestCredentialJoinRejectsBothInputs(t *testing.T) {
	dir := t.TempDir()
	err := Run([]string{"join", "--state", dir, "--cred", "cmesh1.aa.bb", "some-invite"})
	if err == nil {
		t.Fatal("join accepted both a credential and an invite")
	}
	var _ discovery.EnrollRequest
}
