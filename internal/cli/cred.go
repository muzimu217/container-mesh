// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// The team credential's owner-side command.
//
// `cmesh cred create` hands the account owner ONE string that a member's
// device can join with — no per-device signing, no per-device approval.
// The string is a secret: possession of it is authority, bounded by the
// credential's expiry and device quota.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/identity"
)

func runCred(args []string) error {
	if len(args) == 0 {
		return ExitError{Code: 2, Err: errors.New(
			"usage: cmesh cred create --disc host:port [--ttl 24h] [--max-devices 10]")}
	}
	switch args[0] {
	case "create":
		return runCredCreate(args[1:])
	default:
		return ExitError{Code: 2, Err: fmt.Errorf("unknown cred subcommand %q", args[0])}
	}
}

func runCredCreate(args []string) error {
	fs := flag.NewFlagSet("cred create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("account-passphrase", "", "passphrase for the account file (for scripting)")
	var disc stringSlice
	fs.Var(&disc, "disc", "discovery node address(es), repeatable or comma-separated (host:port)")
	ttl := fs.Duration("ttl", credential.DefaultTTL, "how long the credential stays usable")
	maxDevices := fs.Int("max-devices", credential.DefaultMaxDevices, "how many devices may join with this credential")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if len(disc) == 0 {
		return ExitError{Code: 2, Err: errors.New(
			"cred create needs at least one discovery address: --disc host:port (joiners need to know where the network is)")}
	}
	if *maxDevices <= 0 {
		return ExitError{Code: 2, Err: fmt.Errorf(
			"--max-devices must be positive, got %d (a credential without a quota is a permanent key)", *maxDevices)}
	}

	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	accPath := identity.IdentityPath(dir, identity.AccountFileName)
	if _, err := os.Stat(accPath); err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("%s not found; run cmesh account create first", accPath)}
	}
	phrase, err := readPassphrase(*pass, "account passphrase")
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	acc, err := identity.LoadAccount(accPath, phrase)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	cred, err := credential.Create(acc, []string(disc), *ttl, *maxDevices, time.Now())
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	s, err := credential.Encode(cred)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	// The credential goes to stdout (it is the payload); the warning goes to
	// stderr so a copy-paste of stdout yields only the credential.
	fmt.Fprintf(os.Stderr, "team credential for account %s\n", acc.ID())
	fmt.Fprintf(os.Stderr, "  valid for   : %s\n", cred.Public.ExpiresAt.Local().Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "  device quota: %d\n", cred.Public.MaxDevices)
	fmt.Fprintf(os.Stderr, "  discovery   : %s\n", strings.Join(cred.Public.DiscoveryAddrs, ", "))
	fmt.Fprintf(os.Stderr, "\nThis string is a SECRET: whoever holds it can join devices to the network\n")
	fmt.Fprintf(os.Stderr, "until it expires or the quota runs out. Hand it over like a password.\n\n")
	fmt.Println(s)
	return nil
}
