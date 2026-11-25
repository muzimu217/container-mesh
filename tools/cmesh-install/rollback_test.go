// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ THE ROLLBACK FLOOR MUST BE ENFORCED BY THE TOOL.
//
// The shipped systemd units drive the binary with specific flags:
//
//	cmesh serve-discovery --state … --account-pub … --panel-token-file …
//	cmesh join           --state … --cred-file … --relay-trunk-id …
//
// A version that does not accept those flags cannot be driven by the
// units. The symptom is not a clean failure: the unit starts, the
// binary exits with "flag provided but not defined" (status=2), and
// Restart=on-failure restarts it until the start limit stops it — a
// restart loop that fills the journal.
//
// The README has always said the floor is "a rule an operator has to
// know, not a rule the tool enforces". This is that rule, enforced.
//
// ★ WHY THE PROBE EXECUTES THE BINARY.
//
// The question is "does this CLI accept this flag", and the only
// honest way to answer it is to ask the CLI. A substring search over
// the binary's bytes would be a guess: the flag name can appear in
// unrelated text, and a version can carry a flag in its help without
// accepting it.
//
// Running `--help` is also exactly what the unit does, and the binary
// is checksum-verified against the release's SHA256SUMS BEFORE this
// runs — so the thing being executed is the published artifact, not
// something the network could have substituted.
//
// The probe asks for help and never starts the service: `--help` is
// handled by the flag parser, which prints the usage and exits.
//
// ★ THE TESTS BELOW NEVER TOUCH A REAL DEPLOYMENT.
//
// They prove the DECISION: given a binary that serves the units, the
// check ACCEPTS; given one that does not, it REFUSES. The refusal
// happens at the decision point, before anything is activated, so no
// destructive behaviour is ever performed.

// fakeBinary writes a script that mimics `cmesh <sub> --help`.
//
// The flags argument is the set of flags the fake accepts. An empty
// subcommand map means the binary does not have that subcommand at all
// — which is what a version from before the subcommand existed looks
// like.
func fakeBinary(t *testing.T, flags map[string][]string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\n")
	sb.WriteString("sub=\"\"\n")
	sb.WriteString("for a in \"$@\"; do\n")
	sb.WriteString("  case \"$a\" in -*) ;; *) sub=\"$a\"; break ;; esac\n")
	sb.WriteString("done\n")
	sb.WriteString("case \"$sub\" in\n")
	for sub, fs := range flags {
		sb.WriteString("  " + sub + ")\n")
		sb.WriteString("    echo 'Usage of " + sub + ":' >&2\n")
		for _, f := range fs {
			sb.WriteString("    echo '  -" + f + " string' >&2\n")
			sb.WriteString("    echo '        usage text for " + f + "' >&2\n")
		}
		sb.WriteString("    exit 2\n")
		sb.WriteString("    ;;\n")
	}
	sb.WriteString("  *)\n")
	sb.WriteString("    echo 'cmesh: unknown subcommand \"'\"$sub\"'\"' >&2\n")
	sb.WriteString("    exit 2\n")
	sb.WriteString("    ;;\n")
	sb.WriteString("esac\n")

	path := filepath.Join(t.TempDir(), "cmesh")
	if err := os.WriteFile(path, []byte(sb.String()), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

// recordDigest writes the digest of a payload beside it, the way the
// installer does at install time.
//
// A test that SWAPS the binary has to call this: the rollback verifies
// the installed binary against this file before executing it, so a
// stale digest would make the test prove the refusal path instead of
// the behaviour it means to prove.
func recordDigest(t *testing.T, versionDir string, payload []byte) {
	t.Helper()
	sum := sha256.Sum256(payload)
	if err := os.WriteFile(filepath.Join(versionDir, digestFileName),
		[]byte(hex.EncodeToString(sum[:])+"  cmesh\n"), 0o644); err != nil {
		t.Fatalf("record digest: %v", err)
	}
}

// aFakeBinaryServingTheUnits is a binary that accepts every flag the
// shipped units pass.
func aFakeBinaryServingTheUnits(t *testing.T) string {
	t.Helper()
	return fakeBinary(t, map[string][]string{
		"serve-discovery": {
			"account-pub", "addr", "announce", "panel",
			"panel-token-file", "relay-trunk", "state",
		},
		"join": {
			"cred-file", "listen", "relay-trunk", "relay-trunk-id", "state",
		},
	})
}

// ★ THE CHECK MUST ACCEPT A BINARY THAT SERVES THE UNITS.
func TestTheUnitCompatibilityCheckAcceptsABinaryThatServesTheUnits(t *testing.T) {
	bin := aFakeBinaryServingTheUnits(t)
	if err := checkUnitCompatibility(bin); err != nil {
		t.Fatalf("the check refused a binary that serves every flag the units "+
			"pass: %v", err)
	}
}

// ★ THE CHECK MUST REFUSE A BINARY THAT LACKS A FLAG THE UNITS PASS.
//
// This is the rollback floor. A version without --panel-token-file
// (the file-based panel secret) makes the anchor unit die with "flag
// provided but not defined" and restart until the start limit stops it.
func TestTheUnitCompatibilityCheckRefusesABinaryMissingAUnitFlag(t *testing.T) {
	bin := fakeBinary(t, map[string][]string{
		"serve-discovery": {
			// No panel-token-file: this is a version from before the
			// panel secret moved to a file.
			"account-pub", "addr", "announce", "panel",
			"relay-trunk", "state",
		},
		"join": {
			"cred-file", "listen", "relay-trunk", "relay-trunk-id", "state",
		},
	})
	err := checkUnitCompatibility(bin)
	if err == nil {
		t.Fatal("the check accepted a binary that cannot accept --panel-token-file; " +
			"the anchor unit would die with status=2 and restart until the start " +
			"limit stops it")
	}
	if !strings.Contains(err.Error(), "panel-token-file") {
		t.Errorf("the refusal does not name the missing flag: %v; the operator has "+
			"to be told WHICH flag the version cannot accept", err)
	}
}

// ★ THE CHECK MUST REFUSE A BINARY THAT LACKS THE SUBCOMMAND.
//
// A version from before serve-discovery existed does not have the
// subcommand at all — and the unit's ExecStart names it, so the unit
// cannot start at all.
func TestTheUnitCompatibilityCheckRefusesABinaryWithoutTheSubcommand(t *testing.T) {
	bin := fakeBinary(t, map[string][]string{
		"join": {
			"cred-file", "listen", "relay-trunk", "relay-trunk-id", "state",
		},
	})
	err := checkUnitCompatibility(bin)
	if err == nil {
		t.Fatal("the check accepted a binary with no serve-discovery subcommand; " +
			"the anchor unit's ExecStart names that subcommand, so the unit " +
			"cannot start at all")
	}
	if !strings.Contains(err.Error(), "serve-discovery") {
		t.Errorf("the refusal does not name the missing subcommand: %v", err)
	}
}

// ★ THE CHECK MUST REFUSE A BINARY IT CANNOT EXECUTE.
//
// "Cannot tell" is not "compatible". A binary that cannot run at all
// is a broken install, and activating it would take the service down.
func TestTheUnitCompatibilityCheckRefusesABinaryItCannotRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cmesh")
	// Not executable: exec fails.
	if err := os.WriteFile(path, []byte("not a program"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := checkUnitCompatibility(path); err == nil {
		t.Fatal("the check accepted a binary it cannot execute; a broken binary " +
			"must not be activated")
	}
}

// installVersion installs a payload and records its digest, the way the
// real installer does.
//
// ★ THE DIGEST MATTERS NOW. (Round 107.) The rollback verifies the
// installed binary against the digest recorded beside it BEFORE it
// executes the binary, so a test that swaps the binary has to record
// the new digest too — otherwise it would be proving the refusal path
// instead of the behaviour it means to prove.
func installVersion(t *testing.T, root, version string, payload []byte) {
	t.Helper()
	sum := sha256.Sum256(payload)
	if err := installPayloadWithDigest(root, version, payload, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("install %s: %v", version, err)
	}
}

// ★ THE ROLLBACK MUST REFUSE A VERSION THAT IS NOT INSTALLED.
//
// The refusal has to happen BEFORE anything is touched: a rollback that
// half-succeeds leaves the service pointing at nothing.
func TestTheRollbackRefusesAVersionThatIsNotInstalled(t *testing.T) {
	root := installedRoot(t)
	installVersion(t, root, "v2", []byte("new"))
	before := readLink(t, filepath.Join(root, "current"))

	if err := rollback(root, "v-does-not-exist"); err == nil {
		t.Fatal("the rollback accepted a version that is not installed")
	}
	if got := readLink(t, filepath.Join(root, "current")); got != before {
		t.Fatalf("a refused rollback changed `current` from %q to %q; the refusal "+
			"has to happen before anything is touched", before, got)
	}
}

// ★ THE ROLLBACK MUST REFUSE A VERSION THE UNITS CANNOT DRIVE.
//
// This is the whole point: rolling back to a version below the floor
// must be REFUSED, not performed and discovered later as a restart
// loop.
func TestTheRollbackRefusesAVersionTheUnitsCannotDrive(t *testing.T) {
	root := installedRoot(t)
	installVersion(t, root, "v2", []byte("new"))
	// Replace the OLD version's binary with one that cannot serve the
	// units — a version from before the file-based secrets.
	incompatible := fakeBinary(t, map[string][]string{
		"join": {
			"listen", "relay-trunk", "relay-trunk-id", "state",
		},
	})
	payload, err := os.ReadFile(incompatible)
	if err != nil {
		t.Fatalf("read fake binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "versions", "v1", "cmesh"), payload, 0o755); err != nil {
		t.Fatalf("replace binary: %v", err)
	}
	recordDigest(t, filepath.Join(root, "versions", "v1"), payload)
	before := readLink(t, filepath.Join(root, "current"))

	err = rollback(root, "v1")
	if err == nil {
		t.Fatal("the rollback accepted a version the units cannot drive; rolling " +
			"back below the floor is exactly what must be refused")
	}
	if got := readLink(t, filepath.Join(root, "current")); got != before {
		t.Fatalf("a refused rollback changed `current` from %q to %q", before, got)
	}
}

// ★ THE ROLLBACK MUST ACTIVATE A COMPATIBLE VERSION.
//
// The refusal must not be a blanket "no": rolling back to a version
// that CAN serve the units is the normal, supported operation, and it
// has to work.
func TestTheRollbackActivatesACompatibleVersion(t *testing.T) {
	root := installedRoot(t)
	installVersion(t, root, "v2", []byte("new"))
	// Make v1's binary one that serves the units.
	good := aFakeBinaryServingTheUnits(t)
	payload, err := os.ReadFile(good)
	if err != nil {
		t.Fatalf("read fake binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "versions", "v1", "cmesh"), payload, 0o755); err != nil {
		t.Fatalf("replace binary: %v", err)
	}
	recordDigest(t, filepath.Join(root, "versions", "v1"), payload)

	if err := rollback(root, "v1"); err != nil {
		t.Fatalf("the rollback refused a version that CAN serve the units: %v", err)
	}
	if got := readLink(t, filepath.Join(root, "current")); got != "versions/v1" {
		t.Fatalf("after the rollback `current` points at %q, want versions/v1", got)
	}
}

// readLink returns the target of a symlink.
func readLink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	return target
}
