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
	"crypto/ed25519"
	"encoding/json"
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

// TestCredentialJoinLeavesUsableConfig: a credential-joined device must be
// able to run every follow-up command (ping, msg, send-to, run).
//
// Those commands rebuild the discovery view from discovery.json, which used
// to store the RAW join string in Invite and re-parse it with DecodeInvite —
// but on this path the raw string is a cmesh1. credential, which
// DecodeInvite cannot read. The device joined successfully and then failed
// everything with "saved invite is malformed" (oh-no-agent, defect B-3): the
// one-command join broke at the second command. The config now carries the
// account and addresses as fields, derived at join time.
func TestCredentialJoinLeavesUsableConfig(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cred, err := credential.Create(acc, []string{"127.0.0.1:18801"}, time.Hour, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	credStr, err := credential.Encode(cred)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	// Exactly what runJoin persists on the credential path.
	inv := inviteFromCredential(cred)
	if _, err := saveDiscoveryConfig(dir, discoveryConfig{
		Invite:         credStr,
		AccountID:      string(inv.AccountID),
		AccountPub:     inv.AccountPub,
		DiscoveryAddrs: inv.DiscoveryAddrs,
		SavedAt:        now,
	}); err != nil {
		t.Fatal(err)
	}

	blob, err := os.ReadFile(filepath.Join(dir, "discovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg discoveryConfig
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatal(err)
	}

	// The raw credential string must NOT be re-parsed as an invite.
	got, err := cfg.invite()
	if err != nil {
		t.Fatalf("a credential-joined device cannot rebuild its discovery view: %v", err)
	}
	if got.AccountID != acc.ID() {
		t.Errorf("rebuilt account = %s, want %s", got.AccountID, acc.ID())
	}
	if len(got.DiscoveryAddrs) != 1 || got.DiscoveryAddrs[0] != "127.0.0.1:18801" {
		t.Errorf("rebuilt discovery addresses = %v, want the credential's own", got.DiscoveryAddrs)
	}
	if len(got.AccountPub) != ed25519.PublicKeySize {
		t.Errorf("rebuilt account key is %d bytes, want %d", len(got.AccountPub), ed25519.PublicKeySize)
	}

	// A config written by an older build has only the raw invite; that path
	// must keep working, or upgrading would strand existing devices.
	raw, err := discovery.EncodeInvite(discovery.Invite{
		AccountID:      acc.ID(),
		AccountPub:     acc.PublicKey(),
		DiscoveryAddrs: []string{"127.0.0.1:18802"},
		IssuedAt:       now,
	})
	if err != nil {
		t.Fatal(err)
	}
	gotLegacy, err := (discoveryConfig{Invite: raw}).invite()
	if err != nil {
		t.Fatalf("an older invite-only config no longer loads: %v", err)
	}
	if gotLegacy.AccountID != acc.ID() {
		t.Errorf("legacy config rebuilt account %s, want %s", gotLegacy.AccountID, acc.ID())
	}
}
