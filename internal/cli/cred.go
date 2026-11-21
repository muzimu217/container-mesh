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
//
// It also says WHICH ROLE the state directory plays, because "there is no
// roster here" means two different things:
//
//   - a FRESH ANCHOR: no member has ever joined, so nothing has been
//     written. A revoke must still work there — that is the case where a
//     credential leaked before anyone used it.
//   - a MEMBER DEVICE: the roster is the ANCHOR's file, and a member's
//     directory holds only its own identity. Saying "no credentials
//     recorded" there is a lie: the network has members, this device
//     just cannot see the roster.
//
// The state directory tells them apart: a member has a discovery.json (it
// joined something), an anchor does not.
type stateRole int

const (
	// roleUnknown: the directory does not look like either.
	roleUnknown stateRole = iota
	// roleAnchor: this is a discovery node's state directory.
	roleAnchor
	// roleMember: this is a member device's state directory.
	roleMember
)

func stateRoleFor(dir string) stateRole {
	// The ACCOUNT FILE is the criterion, and it is the exact one.
	//
	// `cmesh account create` is what writes it, and only the account
	// owner does that. The roster commands ARE account-owner commands:
	// a member device has no account.json and never will, so it can
	// never own a roster.
	//
	// The first criterion (a discovery.json in the directory) looked
	// equivalent and was NOT: anybody can run `join` against a state
	// directory, and on the ANCHOR that is a natural debugging move. The
	// anchor's own directory would then carry a discovery.json and be
	// mistaken for a member's — so the revoke would refuse to run on the
	// very machine that owns the roster.
	//
	// (Found by the external tester, from a code reading.)
	if _, err := os.Stat(identity.IdentityPath(dir, identity.AccountFileName)); err == nil {
		return roleAnchor
	}
	return roleMember
}

func membersPathFor(state string) (string, error) {
	dir, err := stateDir(state)
	if err != nil {
		return "", err
	}
	return identity.IdentityPath(dir, identity.MembersFileName), nil
}

// noRosterHere is the message for a state directory that has no roster
// because it is a MEMBER's directory.
//
// The roster belongs to the discovery node. A member device can see the
// member view (it is in the snapshot) but it cannot read or change the
// roster, so a command that reads the roster must say so rather than
// report an empty network.
func noRosterHere(dir string) string {
	return fmt.Sprintf(
		"%s has no roster: the member list belongs to the discovery node, and this directory is a member's own state "+
			"(device.json + discovery.json). Run the command on the anchor, where members.json lives.",
		dir)
}

// runCredList prints every credential the network knows, with the number of
// devices each one brought in. A rotation starts with knowing what is out
// there.
//
// It also prints the REVOKED credentials. They are not in the delegation
// list (a revoked credential's delegation is gone), so before this the
// operator had no CLI way to see what had been turned off — they had to
// read the JSON. A rotation you cannot inspect is a rotation you cannot
// trust.
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
	// Two cases of "no roster here", and they must not be confused:
	// a fresh anchor (no member has ever joined — a revoke must still
	// work there) and a MEMBER's directory (the roster is the anchor's
	// file; saying "no credentials recorded" there would be a lie about
	// a network that has members).
	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	rosterThere := true
	if _, statErr := os.Stat(path); statErr != nil {
		rosterThere = false
		if stateRoleFor(dir) == roleMember {
			return ExitError{Code: 1, Err: errors.New(noRosterHere(dir))}
		}
	}
	m, err := identity.LoadMembersOrEmpty(path, "")
	if err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("%s: %w", path, err)}
	}
	now := time.Now().UTC()
	list := m.DelegationList()
	revoked := m.RevokedCredentials()
	if len(list) == 0 && len(revoked) == 0 {
		if !rosterThere {
			fmt.Fprintf(os.Stderr, "no roster yet in %s (this anchor has never had a member; nothing to list)\n", dir)
			return nil
		}
		fmt.Fprintf(os.Stderr, "no credentials recorded in %s\n", path)
		return nil
	}
	if len(list) > 0 {
		fmt.Fprintf(os.Stderr, "credentials in %s\n", path)
		for _, d := range list {
			state := "valid"
			switch {
			case now.After(d.ExpiresAt):
				state = "EXPIRED"
			case now.Before(d.IssuedAt):
				state = "not yet valid"
			}
			// A credential that is on the REVOCATION list is not
			// "valid", whatever its expiry says. It used to be printed
			// twice — once here as "valid" and once in the revoked
			// section — and the operator had to decide which line to
			// believe.
			for _, r := range revoked {
				if r == d.ID {
					state = "REVOKED"
					break
				}
			}
			fmt.Fprintf(os.Stderr, "  %-16s members=%-3d quota=%-4d expires=%s  %s\n",
				d.ID, m.DelegatedCount(d.DelegatorPub), d.MaxDevices,
				d.ExpiresAt.Local().Format(time.RFC3339), state)
		}
	}
	if len(revoked) > 0 {
		fmt.Fprintf(os.Stderr, "revoked credentials in %s\n", path)
		for _, id := range revoked {
			fmt.Fprintf(os.Stderr, "  %-16s revoked (no device can enrol with it)\n", id)
		}
	}
	return nil
}

// runCredRevoke turns a credential off — whichever of the TWO states it is
// in — and takes its members out when it has any.
//
// This is the owner's "换密码" switch. The credential can be:
//
//  1. already used: its delegation record is on the roster (at least one
//     device enrolled with it). RevokeDelegation kicks every device that
//     joined under it AND puts the id on the revocation list, so the old
//     string cannot bring the delegation back by being presented again —
//     its account signature would still verify, and without the list the
//     enroll path would happily re-write the record.
//  2. unused: no delegation on the roster — the anchor only learns a
//     credential's id at enroll time, so a leaked-but-unused credential
//     (the D77 gap) and a typo'd id look identical here, and BOTH go onto
//     the revocation list. Listing a typo harms nothing: no credential will
//     ever present that id. The old hard failure would have left the
//     genuinely-leaked case unrevokable until its year ran out.
//
// Devices approved by the account itself are untouched in both states, and
// so are members of other credentials — a rotation is scoped to one
// credential. There is no un-revoke: a listed id stays refused (re-issuing
// means a new credential with a new id).
func runCredRevoke(args []string) error {
	fs := flag.NewFlagSet("cred revoke", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	credID := fs.String("cred-id", "", "the credential id to revoke (see `cmesh cred list` or the id `cred create` printed)")
	// NOTE: no passphrase flag, and none is needed. The revoke acts on
	// the roster FILE, not on the account key — so the operator does not
	// have to hand the account secret to a script that only turns a
	// credential off. (The external tester noticed the missing flag and
	// reported it as a rough edge; it is deliberate, and the help text
	// now says so.)
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
	// A fresh anchor has no roster yet, and a revoke must work there: the
	// case that matters most ("the credential leaked before anyone used
	// it") is exactly the state a newly installed anchor is in.
	//
	// But on a MEMBER's directory there is nothing to revoke: the roster
	// is the anchor's file. Saying "no such file" there sends the
	// operator looking for a missing file instead of telling them where
	// the roster actually lives.
	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if _, statErr := os.Stat(path); statErr != nil && stateRoleFor(dir) == roleMember {
		return ExitError{Code: 1, Err: errors.New(noRosterHere(dir))}
	}
	m, err := identity.LoadMembersOrEmpty(path, "")
	if err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("%s: %w", path, err)}
	}

	_, onRoster := m.DelegationByID(*credID)
	var removed []identity.DeviceID
	alreadyListed := false
	if onRoster {
		// State 1: devices came in on it — kick them; the id lands on the
		// revocation list in the same mutation.
		removed, err = m.RevokeDelegation(*credID)
		if err != nil {
			return ExitError{Code: 1, Err: err}
		}
	} else {
		// State 2: nobody has enrolled with it (or the id never existed) —
		// pre-revoke it. Both succeed, on purpose.
		newlyListed, err := m.RevokeCredential(*credID)
		if err != nil {
			return ExitError{Code: 1, Err: err}
		}
		alreadyListed = !newlyListed
	}
	// The MERGED save, not the plain one.
	//
	// The CLI's roster is a SNAPSHOT: it was loaded when the command
	// started, and the anchor writes the roster from its own in-memory
	// copy every time something changes. A plain save writes the
	// snapshot back — so a revocation the anchor recorded in the
	// meantime (and the revoked-signer set that goes with it) is
	// ERASED, and the devices that revocation removed come back.
	//
	// That is the race kuku-sandbox-01 reproduced with an ordered
	// test: a revoke and an enrolment issued together → the revoke
	// reported "1 device(s) left the network" and the roster went back
	// UP. The merged save keeps what the file already records.
	if err := m.SaveMembersMerged(path); err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("save %s: %w", path, err)}
	}

	// What happened, in the operator's terms: the credential is dead, and
	// HERE is exactly what the command did about it.
	fmt.Fprintf(os.Stderr, "credential %s revoked.\n", *credID)
	if onRoster {
		if len(removed) == 0 {
			fmt.Fprintf(os.Stderr, "  no devices were enrolled under it.\n")
		} else {
			fmt.Fprintf(os.Stderr, "  %d device(s) left the network:\n", len(removed))
			for _, id := range removed {
				fmt.Fprintf(os.Stderr, "    %s\n", id)
			}
		}
		fmt.Fprintf(os.Stderr, "  its id is on the revocation list: even the original string cannot enrol anything with it again.\n")
	} else {
		// The honest wording.
		//
		// "no delegation on the roster" means the roster has no
		// DELEGATION RECORD for this id — it does NOT mean no device
		// enrolled with it: a device that enrolled under the credential
		// leaves its delegation record behind when the record is
		// removed, and the roster can still hold the device.
		//
		// The old wording ("no device has enrolled with it yet") was
		// false in exactly the case that matters: the credential was
		// revoked and the devices are STILL on the roster, because
		// there was no delegation record to remove them by.
		fmt.Fprintf(os.Stderr, "  no delegation record on the roster for this id.\n")
		fmt.Fprintf(os.Stderr, "  devices that enrolled under it, if any, stay on the roster — a revocation without a\n")
		fmt.Fprintf(os.Stderr, "  delegation record cannot name them. Remove them by hand (or re-issue the credential).\n")
		if alreadyListed {
			fmt.Fprintf(os.Stderr, "  it was already on the revocation list and stays there; nothing changed.\n")
		} else {
			fmt.Fprintf(os.Stderr, "  its id went onto the revocation list: any future enrolment presenting a credential\n")
			fmt.Fprintf(os.Stderr, "  with this id is refused, signature and expiry notwithstanding.\n")
		}
	}
	fmt.Fprintf(os.Stderr, "  the discovery node picks the change up from %s within a second (it hot-reloads the file).\n", path)
	return nil
}
