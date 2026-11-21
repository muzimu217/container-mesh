// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// The WIRING of `cmesh join --cred-file`, end to end.
//
// The unit tests for the credential's sources call resolveCredential
// directly, so they stay green even when the JOIN itself never passes the
// file through: an independent reviewer deleted the `--cred-file` argument
// from the call in runJoin and every test still passed. A helper test
// proves the helper works; only a test that runs the COMMAND proves the
// command uses it.
//
// So this test runs the real `cmesh join` — a real discovery node, a real
// credential, a real enrollment — with the credential in a FILE, and
// asserts that the join actually used it:
//
//   - the enrollment reached the discovery fixture (the device is in the
//     roster, with the delegation the credential authorises);
//   - the saved configuration names the credential's account.
//
// It also asserts the negative: the credential's secret must not be in the
// saved configuration.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/relay"
)

// TestJoinWithACredentialFileEnrollsAndSavesTheAccount: the whole
// `--cred-file` path, through the real command.
//
// The credential is handed over as a FILE, the way systemd's
// LoadCredential hands it over — the credential never touches the command
// line.
func TestJoinWithACredentialFileEnrollsAndSavesTheAccount(t *testing.T) {
	// The fixture talks to a loopback address, which the outbound
	// address policy refuses by design. The private-deployment switch is
	// the operator's own way of saying "this is my network".
	t.Setenv("CMESH_ALLOW_PRIVATE_DISCOVERY", "1")

	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}

	// The discovery node: a real relay with the real enrollment handler.
	srv := relay.New(relay.DefaultConfig())
	members := identity.NewMemberList(acc.ID())
	srv.EnableDiscovery(members)
	t.Cleanup(srv.StopDiscovery)
	discAddr := freePort(t)
	serveDiscoveryInProcess(t, srv, discAddr)

	membersPath := filepath.Join(t.TempDir(), "members.json")
	if err := members.SaveMembers(membersPath); err != nil {
		t.Fatal(err)
	}
	installEnrollHandler(srv, membersPath, members, acc.PublicKey())

	// The credential names the fixture's address.
	cred, err := credential.Create(acc, []string{discAddr}, time.Hour, 5, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	credStr, err := credential.Encode(cred)
	if err != nil {
		t.Fatal(err)
	}

	// The credential as a FILE — the systemd LoadCredential shape.
	credPath := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(credPath, []byte(credStr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The joiner: no identity at all (the credential path creates one).
	dir := t.TempDir()
	pass := "test-pass-phrase"

	done := make(chan error, 1)
	go func() {
		done <- Run([]string{"join", "--state", dir, "--device-passphrase", pass,
			"--cred-file", credPath, "--once", "--timeout", "30s"})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the join with a credential file failed: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the join with a credential file never returned")
	}

	// The enrollment reached the fixture: the device is in the roster and
	// the delegation the credential authorises is recorded beside it.
	dev, err := identity.LoadDevice(identity.IdentityPath(dir, identity.DeviceFileName), pass)
	if err != nil {
		t.Fatalf("the join did not create a device identity: %v", err)
	}
	if _, ok := members.Get(dev.ID()); !ok {
		t.Fatal("the device never reached the roster: the credential file was not used")
	}
	if _, ok := members.DelegationFor(cred.Public.DelegatorPub); !ok {
		t.Fatal("the delegation was not recorded: the enrollment did not happen through the credential")
	}

	// The saved configuration names the CREDENTIAL's account — the
	// derived fields, not a raw string.
	cfg, err := loadDiscoveryConfig(t, dir)
	if err != nil {
		t.Fatalf("read the saved configuration: %v", err)
	}
	if cfg.AccountID != string(acc.ID()) {
		t.Fatalf("the saved account is %q, want the credential's account %q", cfg.AccountID, acc.ID())
	}
	if len(cfg.DiscoveryAddrs) == 0 {
		t.Fatal("the saved configuration names no discovery address")
	}

	// And the credential's SECRET must not be on disk.
	blob, err := os.ReadFile(filepath.Join(dir, "discovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), credStr) {
		t.Fatalf("the saved configuration carries the credential itself:\n%s", blob)
	}
	// The DELEGATOR PRIVATE KEY is the secret half of the encoded
	// credential. It must not be on disk in any form.
	if strings.Contains(string(blob), "delegator_priv") {
		t.Fatalf("the saved configuration carries the delegator private key:\n%s", blob)
	}
}

// TestJoinWithACredentialFileThatIsEmptyFails: the file source is
// checked before the join does anything, so a broken file never reaches
// the network.
func TestJoinWithACredentialFileThatIsEmptyFails(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(credPath, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Run([]string{"join", "--state", dir, "--device-passphrase", "test-pass",
		"--cred-file", credPath, "--once", "--timeout", "5s"})
	if err == nil {
		t.Fatal("the join accepted an empty credential file")
	}
	if !strings.Contains(err.Error(), credPath) {
		t.Fatalf("the error does not name the broken file: %v", err)
	}
	// And no identity was created: the join refused before it did
	// anything at all.
	if _, err := os.Stat(identity.IdentityPath(dir, identity.DeviceFileName)); !os.IsNotExist(err) {
		t.Fatal("the join created a device identity although the credential file was empty")
	}
}

// TestJoinWithAMissingCredentialFileFails: the same for a file that is
// not there.
func TestJoinWithAMissingCredentialFileFails(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "not-there")

	err := Run([]string{"join", "--state", dir, "--device-passphrase", "test-pass",
		"--cred-file", missing, "--once", "--timeout", "5s"})
	if err == nil {
		t.Fatal("the join accepted a missing credential file")
	}
}

// TestTheInvitePathWorksWithAnAmbientCredential is the end-to-end half of
// the ambient-variable rule.
//
// The invite path is the OTHER way into the network, and it must keep
// working on a host that exports CMESH_CRED globally. This test runs the
// real join with a real invite and an ambient credential, and the join
// must go after the invite — not refuse with "not both".
func TestTheInvitePathWorksWithAnAmbientCredential(t *testing.T) {
	t.Setenv("CMESH_ALLOW_PRIVATE_DISCOVERY", "1")
	t.Setenv("CMESH_CRED", "cmesh1.this-is-an-ambient-credential-nobody-asked-for")

	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := encodeTestInvite(t, acc, "mesh.example:18801")
	if err != nil {
		t.Fatal(err)
	}

	// The joiner has an identity but no approval: the join must WAIT for
	// the approval, which means it must not have refused the combination
	// of a credential and an invite.
	dir := t.TempDir()
	pass := "test-pass-phrase"
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := dev.Save(identity.IdentityPath(dir, identity.DeviceFileName), pass); err != nil {
		t.Fatal(err)
	}

	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	oldHook := joinLifetimeHook
	joinLifetimeHook = func() (context.Context, context.CancelFunc) { return lifeCtx, lifeCancel }
	t.Cleanup(func() {
		joinLifetimeHook = oldHook
		lifeCancel()
	})

	done := make(chan error, 1)
	go func() {
		done <- Run([]string{"join", "--state", dir, "--device-passphrase", pass,
			"--timeout", "3s", inv})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the join succeeded although nothing approved it")
		}
		if strings.Contains(err.Error(), "not both") {
			t.Fatalf("the ambient credential broke the invite path: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the join never returned")
	}
}
