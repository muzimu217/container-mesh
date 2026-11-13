// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// Tests for the team credential's owner-side command.
//
// The properties that matter:
//   - the printed string is a complete, verifiable credential (the owner can
//     hand it over and a member can use it);
//   - it never leaks the account key, and the quota/expiry it promises are
//     the ones it carries;
//   - a credential without a quota or without a discovery address is
//     refused, because both are load-bearing.

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/muzimu217/container-mesh/internal/credential"
)

func TestCredCreatePrintsUsableCredential(t *testing.T) {
	dir := t.TempDir()
	pass := "test-pass-phrase"
	mustRun(t, "init", "--state", dir, "--passphrase", pass)
	mustRun(t, "account", "create", "--state", dir, "--passphrase", pass)

	out := mustRun(t, "cred", "create", "--state", dir,
		"--account-passphrase", pass, "--disc", "127.0.0.1:18090",
		"--ttl", "2h", "--max-devices", "3")

	cred := strings.TrimSpace(out)
	if cred == "" {
		t.Fatal("cred create printed nothing")
	}
	if strings.Contains(cred, " ") || strings.Contains(cred, "\n") {
		t.Fatalf("the credential must be a single line: %q", cred)
	}
	parsed, err := credential.Decode(cred)
	if err != nil {
		t.Fatalf("the printed credential does not decode: %v", err)
	}
	if parsed.Public.MaxDevices != 3 {
		t.Fatalf("quota = %d, want 3", parsed.Public.MaxDevices)
	}
	if len(parsed.Public.DiscoveryAddrs) != 1 || parsed.Public.DiscoveryAddrs[0] != "127.0.0.1:18090" {
		t.Fatalf("discovery addresses = %v", parsed.Public.DiscoveryAddrs)
	}
	// The account public key travels (the node verifies against it); the
	// account PRIVATE key must not. What the credential does carry is the
	// delegator's private key — that is the whole point of the delegation.
	if len(parsed.Secret.DelegatorPriv) != ed25519.PrivateKeySize {
		t.Fatalf("the credential carries no delegator key: %d bytes", len(parsed.Secret.DelegatorPriv))
	}
}

// TestCredCreateRefusesUnboundedCredential pins the guard: a credential
// without a device quota is a permanent key, and one without a discovery
// address cannot tell a joiner where the network is.
func TestCredCreateRefusesUnboundedCredential(t *testing.T) {
	dir := t.TempDir()
	pass := "test-pass-phrase"
	mustRun(t, "init", "--state", dir, "--passphrase", pass)
	mustRun(t, "account", "create", "--state", dir, "--passphrase", pass)

	if err := Run([]string{"cred", "create", "--state", dir,
		"--account-passphrase", pass, "--disc", "127.0.0.1:18090",
		"--max-devices", "0"}); err == nil {
		t.Fatal("a credential with a zero quota was accepted")
	}
	if err := Run([]string{"cred", "create", "--state", dir,
		"--account-passphrase", pass}); err == nil {
		t.Fatal("a credential without a discovery address was accepted")
	}
}

// TestAccountShowAcceptsBothPassphraseFlags pins the round-56 observation:
// `account show` used --passphrase while every sibling command used
// --account-passphrase, so the wrong spelling died with "flag provided but
// not defined". Both must work.
func TestAccountShowAcceptsBothPassphraseFlags(t *testing.T) {
	dir := t.TempDir()
	pass := "test-pass-phrase"
	mustRun(t, "init", "--state", dir, "--passphrase", pass)
	mustRun(t, "account", "create", "--state", dir, "--passphrase", pass)

	for _, flag := range []string{"--passphrase", "--account-passphrase"} {
		out := mustRun(t, "account", "show", "--state", dir, flag, pass)
		if !strings.Contains(out, "pubkey.b64") {
			t.Fatalf("account show with %s printed nothing useful: %q", flag, out)
		}
	}
}
