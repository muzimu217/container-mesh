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
			"usage: cmesh cred create --disc host:port [--ttl 24h] [--max-devices 10] | " +
				"cmesh cred list --state DIR | cmesh cred revoke --state DIR --cred-id ID")}
	}
	switch args[0] {
	case "create":
		return runCredCreate(args[1:])
	case "list":
		return runCredList(args[1:])
	case "revoke":
		return runCredRevoke(args[1:])
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
	fmt.Fprintf(os.Stderr, "  credential id: %s  (keep it: `cmesh cred revoke --cred-id %s` turns this credential off)\n",
		cred.Public.ID, cred.Public.ID)
	fmt.Fprintf(os.Stderr, "  valid for   : %s\n", cred.Public.ExpiresAt.Local().Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "  device quota: %d\n", cred.Public.MaxDevices)
	fmt.Fprintf(os.Stderr, "  discovery   : %s\n", strings.Join(cred.Public.DiscoveryAddrs, ", "))
	fmt.Fprintf(os.Stderr, "\nThis string is a SECRET: whoever holds it can join devices to the network\n")
	fmt.Fprintf(os.Stderr, "until it expires or the quota runs out. Hand it over like a password.\n\n")
	fmt.Println(s)
	return nil
}

// membersPathFor resolves the roster file inside a state directory.
//
// The roster is the discovery node's file, and the anchor hot-reloads it —
// so a revoke here takes effect on the anchor without a restart, which is
// the whole point: a leaked password must be turnable NOW.
func membersPathFor(state string) (string, error) {
	dir, err := stateDir(state)
	if err != nil {
		return "", err
	}
	return identity.IdentityPath(dir, identity.MembersFileName), nil
}

// runCredList prints every credential the network knows, with the number of
// devices each one brought in. A rotation starts with knowing what is out
// there.
func runCredList(args []string) error {
	fs := flag.NewFlagSet("cred list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	path, err := membersPathFor(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	m, err := identity.LoadMembers(path)
	if err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("%s: %w", path, err)}
	}
	now := time.Now().UTC()
	list := m.DelegationList()
	if len(list) == 0 {
		fmt.Fprintf(os.Stderr, "no credentials recorded in %s\n", path)
		return nil
	}
	fmt.Fprintf(os.Stderr, "credentials in %s\n", path)
	for _, d := range list {
		state := "valid"
		switch {
		case now.After(d.ExpiresAt):
			state = "EXPIRED"
		case now.Before(d.IssuedAt):
			state = "not yet valid"
		}
		fmt.Fprintf(os.Stderr, "  %-16s members=%-3d quota=%-4d expires=%s  %s\n",
			d.ID, m.DelegatedCount(d.DelegatorPub), d.MaxDevices,
			d.ExpiresAt.Local().Format(time.RFC3339), state)
	}
	return nil
}

// runCredRevoke turns a credential off and takes its members out.
//
// This is the owner's "换密码" switch: every device that joined under the
// named credential leaves the network on the next roster reload, and the
// credential can no longer be used to enrol anything. Devices approved by
// the account itself are untouched, and so are members of other
// credentials — a rotation is scoped to one credential.
func runCredRevoke(args []string) error {
	fs := flag.NewFlagSet("cred revoke", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	credID := fs.String("cred-id", "", "the credential id to revoke (see `cmesh cred list` or the id `cred create` printed)")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if *credID == "" {
		return ExitError{Code: 2, Err: errors.New(
			"cred revoke needs --cred-id ID (see `cmesh cred list`, or the id `cred create` printed)")}
	}
	path, err := membersPathFor(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	m, err := identity.LoadMembers(path)
	if err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("%s: %w", path, err)}
	}
	removed, err := m.RevokeDelegation(*credID)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	if err := m.SaveMembers(path); err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("save %s: %w", path, err)}
	}
	// What happened, in the operator's terms: the credential is dead, and
	// HERE is who left the network because of it.
	fmt.Fprintf(os.Stderr, "credential %s revoked.\n", *credID)
	if len(removed) == 0 {
		fmt.Fprintf(os.Stderr, "  no devices were enrolled under it.\n")
	} else {
		fmt.Fprintf(os.Stderr, "  %d device(s) left the network:\n", len(removed))
		for _, id := range removed {
			fmt.Fprintf(os.Stderr, "    %s\n", id)
		}
	}
	fmt.Fprintf(os.Stderr, "  the discovery node picks the change up from %s within a second (it hot-reloads the file).\n", path)
	return nil
}
