// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"bufio"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// stateDir resolves where identity files live. --state overrides the default
// ~/.cmesh, so tests never touch a real home directory.
func stateDir(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, identity.DefaultStateDir), nil
}

// readPassphrase gets the passphrase for encrypting a key.
//
// Priority: --passphrase flag (for scripting; it ends up in shell history and
// ps output, which is why it is not the default), then the CMESH_PASSPHRASE
// environment variable, then an interactive prompt on stdin.
//
// The interactive prompt reads a plain line, not a no-echo ReadPassword: that
// would need golang.org/x/term, and adding a dependency for echo-suppression
// on a v1 command is not justified yet. The passphrase is typed once per
// rare command (init), on the operator's own machine. Revisit when the CLI
// grows a daily-use unlock command.
func readPassphrase(flagVal string, prompt string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if v := os.Getenv("CMESH_PASSPHRASE"); v != "" {
		return v, nil
	}
	fmt.Fprintf(os.Stderr, "%s: ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// runInit implements `cmesh init`: generate this machine's device identity.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("passphrase", "", "passphrase for the identity file (for scripting; prefer the prompt)")
	force := fs.Bool("force", false, "overwrite an existing identity")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}

	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	path := identity.IdentityPath(dir, identity.DeviceFileName)

	if !*force {
		if _, err := os.Stat(path); err == nil {
			return ExitError{Code: 1, Err: fmt.Errorf(
				"%s already exists; rerun with --force to replace it (the old identity will be lost)", path)}
		}
	}

	phrase, err := readPassphrase(*pass, "passphrase for the new identity (min 8 chars)")
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	dev, err := identity.Generate()
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	if err := dev.Save(path, phrase); err != nil {
		return ExitError{Code: 1, Err: err}
	}

	fmt.Fprintf(os.Stderr, "device identity created\n")
	fmt.Fprintf(os.Stderr, "  id       %s\n", dev.ID())
	fmt.Fprintf(os.Stderr, "  file     %s (encrypted, 0600)\n", path)
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "This ID is this machine's name in the mesh. It never changes unless you\n")
	fmt.Fprintf(os.Stderr, "re-run init --force. Give it to an account owner to be signed in:\n")
	fmt.Fprintf(os.Stderr, "  cmesh account device-add\n")
	return nil
}

// runAccount implements the `cmesh account` subcommands.
func runAccount(args []string) error {
	if len(args) == 0 {
		return ExitError{Code: 2, Err: errors.New(
			"account needs a subcommand: create | device-add | show")}
	}
	switch args[0] {
	case "create":
		return runAccountCreate(args[1:])
	case "device-add":
		return runAccountDeviceAdd(args[1:])
	case "authorize":
		return runAccountAuthorize(args[1:])
	case "show":
		return runAccountShow(args[1:])
	default:
		return ExitError{Code: 2, Err: fmt.Errorf(
			"unknown account subcommand %q (known: create | device-add | authorize | show)", args[0])}
	}
}

// runAccountAuthorize signs a REMOTE device's public key into this account.
// This is the approval half of `cmesh join`: the joiner exports its key with
// `cmesh show-pub`, the account owner authorizes it, and the resulting
// certificate file goes to the discovery host (drop it into the certs
// directory there — serve-discovery picks it up within seconds).
func runAccountAuthorize(args []string) error {
	fs := flag.NewFlagSet("account authorize", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("account-passphrase", "", "passphrase for the account file (for scripting)")
	pub := fs.String("pub", "", "the joining device's public key, base64 from `cmesh show-pub`")
	out := fs.String("out", "", "where to write the certificate (default <state>/<deviceID>.cert.json)")
	ttl := fs.Duration("ttl", identity.DefaultCertTTL, "certificate validity")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if *pub == "" {
		return ExitError{Code: 2, Err: errors.New(
			"authorize needs the joining device's public key: --pub <base64> (the joiner prints it via cmesh show-pub)")}
	}
	raw, err := base64.StdEncoding.DecodeString(*pub)
	if err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("--pub is not base64: %w", err)}
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

	cert, err := acc.IssueCertForPub(ed25519.PublicKey(raw), *ttl)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	path := *out
	if path == "" {
		path = identity.IdentityPath(dir, string(cert.DeviceID)+identity.CertSuffix)
	}
	if err := identity.SaveCert(cert, path); err != nil {
		return ExitError{Code: 1, Err: err}
	}

	fmt.Fprintf(os.Stderr, "device authorized into account %s\n", acc.ID())
	fmt.Fprintf(os.Stderr, "  device   %s\n", cert.DeviceID)
	fmt.Fprintf(os.Stderr, "  expires  %s\n", cert.ExpiresAt.Format("2006-01-02"))
	fmt.Fprintf(os.Stderr, "  cert     %s\n", path)
	fmt.Fprintf(os.Stderr, "\nHand %s to the discovery host: drop it into that host's certs directory\n", filepath.Base(path))
	fmt.Fprintf(os.Stderr, "(cmesh serve-discovery adopts it and pushes the new membership within seconds).\n")
	return nil
}

// runShowPub exports this device's public key: the value a joiner hands to
// the account owner for `account authorize`. Public by definition — the
// private half never leaves the device.
func runShowPub(args []string) error {
	fs := flag.NewFlagSet("show-pub", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("device-passphrase", "", "passphrase for the device file (for scripting)")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	devPath := identity.IdentityPath(dir, identity.DeviceFileName)
	if _, err := os.Stat(devPath); err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("%s not found; run cmesh init first", devPath)}
	}
	phrase, err := readPassphrase(*pass, "device passphrase")
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	dev, err := identity.LoadDevice(devPath, phrase)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	fmt.Fprintf(os.Stderr, "device %s public key (base64; give it to the account owner):\n", dev.ID())
	fmt.Println(base64.StdEncoding.EncodeToString(dev.PublicKey()))
	return nil
}

func runAccountCreate(args []string) error {
	fs := flag.NewFlagSet("account create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("passphrase", "", "passphrase for the account file (for scripting)")
	force := fs.Bool("force", false, "overwrite an existing account key")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}

	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	path := identity.IdentityPath(dir, identity.AccountFileName)

	if !*force {
		if _, err := os.Stat(path); err == nil {
			return ExitError{Code: 1, Err: fmt.Errorf(
				"%s already exists; rerun with --force to replace it", path)}
		}
	}
	phrase, err := readPassphrase(*pass, "passphrase for the account key (min 8 chars)")
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	acc, err := identity.GenerateAccount()
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	if err := acc.Save(path, phrase); err != nil {
		return ExitError{Code: 1, Err: err}
	}

	fmt.Fprintf(os.Stderr, "account key created\n")
	fmt.Fprintf(os.Stderr, "  id       %s\n", acc.ID())
	fmt.Fprintf(os.Stderr, "  file     %s\n", path)
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "The account key signs device certificates. Keep it on your most trusted\n")
	fmt.Fprintf(os.Stderr, "machine (or offline); it never touches the data plane.\n")
	return nil
}

// runAccountDeviceAdd signs THIS machine's device into the account held on
// the same machine. That is the common single-operator flow: the account and
// the device live together on the trusted machine, and only the certificate
// travels.
func runAccountDeviceAdd(args []string) error {
	fs := flag.NewFlagSet("account device-add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	devPass := fs.String("device-passphrase", "", "passphrase for the device file")
	accPass := fs.String("account-passphrase", "", "passphrase for the account file")
	ttl := fs.Duration("ttl", identity.DefaultCertTTL, "certificate validity")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}

	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	devPath := identity.IdentityPath(dir, identity.DeviceFileName)
	accPath := identity.IdentityPath(dir, identity.AccountFileName)

	for _, p := range []struct{ path, what string }{{devPath, "device"}, {accPath, "account"}} {
		if _, err := os.Stat(p.path); err != nil {
			return ExitError{Code: 1, Err: fmt.Errorf(
				"%s not found; run cmesh init and cmesh account create first", p.path)}
		}
	}

	devPhrase, err := readPassphrase(*devPass, "device passphrase")
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	dev, err := identity.LoadDevice(devPath, devPhrase)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	accPhrase, err := readPassphrase(*accPass, "account passphrase")
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	acc, err := identity.LoadAccount(accPath, accPhrase)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	cert, err := acc.IssueCert(dev, *ttl)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	certPath := identity.IdentityPath(dir, string(dev.ID())+identity.CertSuffix)
	if err := identity.SaveCert(cert, certPath); err != nil {
		return ExitError{Code: 1, Err: err}
	}

	fmt.Fprintf(os.Stderr, "device signed into account\n")
	fmt.Fprintf(os.Stderr, "  device   %s\n", dev.ID())
	fmt.Fprintf(os.Stderr, "  account  %s\n", acc.ID())
	fmt.Fprintf(os.Stderr, "  expires  %s\n", cert.ExpiresAt.Format("2006-01-02"))
	fmt.Fprintf(os.Stderr, "  cert     %s\n", certPath)
	return nil
}

func runAccountShow(args []string) error {
	fs := flag.NewFlagSet("account show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	// Both spellings are accepted: the account commands use
	// --account-passphrase everywhere except here, and a flag that differs
	// between sibling commands is a trap (doubao-cloud-vm-01 round 56 hit
	// "flag provided but not defined").
	pass := fs.String("account-passphrase", "", "passphrase for the account file")
	passAlias := fs.String("passphrase", "", "alias for --account-passphrase")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	accountPass := *pass
	if accountPass == "" {
		accountPass = *passAlias
	}
	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	accPath := identity.IdentityPath(dir, identity.AccountFileName)
	if _, err := os.Stat(accPath); err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("%s not found; run cmesh account create first", accPath)}
	}
	phrase, err := readPassphrase(accountPass, "account passphrase")
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	acc, err := identity.LoadAccount(accPath, phrase)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	fmt.Fprintf(os.Stderr, "account  %s\n", acc.ID())
	fmt.Fprintf(os.Stderr, "  file   %s\n", accPath)
	// The key lines go to STDOUT: scripts capture stdout (and routinely
	// discard stderr with 2>/dev/null — the retest round's harness did
	// exactly that and passed an empty --account-pub, silently disabling
	// cert verification). Same convention as show-pub: stdout is the
	// copy-paste surface, stderr is the human narration.
	fmt.Printf("pubkey.b64 %s\n", base64.StdEncoding.EncodeToString(acc.PublicKey()))
	fmt.Printf("pubkey.hex %x\n", acc.PublicKey())
	fmt.Fprintf(os.Stderr, "  pubkey.b64 %s (printed on stdout for scripting; both forms are accepted by --account-pub)\n",
		base64.StdEncoding.EncodeToString(acc.PublicKey()))
	return nil
}
