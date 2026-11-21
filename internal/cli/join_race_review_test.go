// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// The revocation race, seen from the JOINER's side.
//
// The race was found by an external tester (kuku-sandbox-01) with an
// ordered reproduction: a `cred revoke` and a `join --cred` issued at
// the same time → the revoke reported "1 device(s) left the network"
// and the roster went back UP.
//
// The fix's own mechanism — the roster's revoked-signer set, merged on
// every write-back — has a failure mode of its own: the anchor's enrol
// handler answers the joiner BEFORE the merged save runs, so a device
// whose credential was revoked in the last couple of seconds is
// admitted, told "enrolled", and then dropped by its own save.
//
// These tests are the end-to-end proof that the two cannot happen at
// once: whatever the anchor ANSWERS must be true of the roster.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
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

// TestTheJoinerIsNeverToldItJoinedWhenTheRosterDoesNotHaveIt is the
// end-to-end proof.
//
// The anchor's enrol handler answers the joiner over the REAL HTTP
// handler, and then the merged save runs. If the answer says "enrolled"
// and the roster does not have the device, the joiner believes it joined
// the network and it did not — the worst possible outcome of a
// revocation race: silent.
func TestTheJoinerIsNeverToldItJoinedWhenTheRosterDoesNotHaveIt(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	srv := relay.New(relay.DefaultConfig())
	members := identity.NewMemberList(acc.ID())
	srv.EnableDiscovery(members)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.StopDiscovery()

	cred, err := credential.Create(acc, []string{testDiscoveryURL(ts.URL)}, time.Hour, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := cred.PublicPart()
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

	membersPath := filepath.Join(t.TempDir(), "members.json")
	// The anchor's in-memory list holds the delegation (an earlier enrol).
	if err := members.AddDelegation(*cred.Delegation()); err != nil {
		t.Fatal(err)
	}
	if err := members.SaveMembers(membersPath); err != nil {
		t.Fatal(err)
	}
	installEnrollHandler(srv, membersPath, members, acc.PublicKey())

	// The operator revokes the credential FROM ANOTHER PROCESS, while the
	// anchor's memory is still behind the file.
	onDisk, err := identity.LoadMembers(membersPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onDisk.RevokeDelegation(cred.Public.ID); err != nil {
		t.Fatal(err)
	}
	if err := onDisk.SaveMembers(membersPath); err != nil {
		t.Fatal(err)
	}

	// The joiner posts the enrollment. The answer is what the operator
	// and the joiner both see.
	body, err := json.Marshal(discovery.EnrollRequest{Credential: pub, Cert: *cert})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/mesh/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res discovery.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}

	// The roster is the truth.
	after, err := identity.LoadMembers(membersPath)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK && !after.Contains(cert.DeviceID) {
		t.Fatalf("the anchor answered %q for device %s, but the roster does not have it: "+
			"the joiner believes it joined the network, and it did not",
			res.Message, cert.DeviceID)
	}
	if !res.OK && after.Contains(cert.DeviceID) {
		t.Fatalf("the anchor refused the enrollment (%q) but the roster HAS the device %s",
			res.Message, cert.DeviceID)
	}
}

// TestTheJoinCommandSucceedsOnlyIfTheDeviceIsOnTheRoster is the same
// property through the JOIN command's own client, so the joiner's whole
// flow is exercised: enroll, then announce.
func TestTheJoinCommandSucceedsOnlyIfTheDeviceIsOnTheRoster(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	srv := relay.New(relay.DefaultConfig())
	members := identity.NewMemberList(acc.ID())
	srv.EnableDiscovery(members)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.StopDiscovery()

	cred, err := credential.Create(acc, []string{testDiscoveryURL(ts.URL)}, time.Hour, 5, now)
	if err != nil {
		t.Fatal(err)
	}

	membersPath := filepath.Join(t.TempDir(), "members.json")
	if err := members.AddDelegation(*cred.Delegation()); err != nil {
		t.Fatal(err)
	}
	if err := members.SaveMembers(membersPath); err != nil {
		t.Fatal(err)
	}
	installEnrollHandler(srv, membersPath, members, acc.PublicKey())

	// The operator revokes from another process.
	onDisk, err := identity.LoadMembers(membersPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onDisk.RevokeDelegation(cred.Public.ID); err != nil {
		t.Fatal(err)
	}
	if err := onDisk.SaveMembers(membersPath); err != nil {
		t.Fatal(err)
	}

	// The joiner enrolls.
	dir := t.TempDir()
	dev, err := ensureDeviceIdentity(dir, "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = enrollWithCredentialUsing(ctx, cred, dev, testEnrollmentTransport(t, ts.URL))

	after, lerr := identity.LoadMembers(membersPath)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if err == nil && !after.Contains(dev.ID()) {
		t.Fatalf("the join reported success but the device %s is not on the roster", dev.ID())
	}
}

// TestTheRosterNeverCarriesARevokedCredentialsDevice: the anchor's
// next write-back must not put a revoked credential's device back.
func TestTheRosterNeverCarriesARevokedCredentialsDevice(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cred, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 5, now)
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

	dir := t.TempDir()
	membersPath := filepath.Join(dir, "members.json")
	members := identity.NewMemberList(acc.ID())
	if err := members.AddDelegation(*cred.Delegation()); err != nil {
		t.Fatal(err)
	}
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	if err := members.SaveMembers(membersPath); err != nil {
		t.Fatal(err)
	}

	// The operator revokes from another process.
	onDisk, err := identity.LoadMembers(membersPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onDisk.RevokeDelegation(cred.Public.ID); err != nil {
		t.Fatal(err)
	}
	if err := onDisk.SaveMembers(membersPath); err != nil {
		t.Fatal(err)
	}

	// The anchor's next write-back.
	if err := members.SaveMembersMerged(membersPath); err != nil {
		t.Fatal(err)
	}
	after, err := identity.LoadMembers(membersPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Contains(cert.DeviceID) {
		t.Fatalf("the device %s that enrolled under the revoked credential %s is on the roster",
			cert.DeviceID, cred.Public.ID)
	}
	if _, err := os.Stat(membersPath); err != nil {
		t.Fatal(err)
	}
}
