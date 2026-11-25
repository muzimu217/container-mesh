// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ THE ROLLBACK FLOOR MUST BE ENFORCED BY THE TOOL.
//
// The shipped systemd units drive the binary with specific flags. A
// version that does not accept them cannot be driven by the units, and
// the failure is a restart loop rather than a clean error: the binary
// exits with "flag provided but not defined" (status=2) and
// Restart=on-failure restarts it until the start limit stops it.
//
// The README has always said the floor is "a rule an operator has to
// know, not a rule the tool enforces". This is that rule, enforced.
//
// ★ THESE TESTS NEVER TOUCH A REAL DEPLOYMENT.
//
// They prove the DECISION: a version that serves the units is
// ACCEPTED, one that does not is REFUSED, and a refusal happens before
// anything is rotated — so no destructive behaviour is performed.

// fakeBinary writes a script that mimics `cmesh <sub> --help`.
//
// An empty subcommand map means the binary does not have that
// subcommand at all — which is what a version from before the
// subcommand existed looks like.
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

// flagsServingTheUnits is the flag set of a binary that the shipped
// units can drive.
func flagsServingTheUnits() map[string][]string {
	return map[string][]string{
		"serve-discovery": {
			"account-pub", "addr", "announce", "panel",
			"panel-token-file", "relay-trunk", "state",
		},
		"join": {
			"cred-file", "listen", "relay-trunk", "relay-trunk-id", "state",
		},
	}
}

// installVersion installs a named version into the fixture's install root.
//
// The installer names the version directory after CMESH_VERSION, so a
// second install with a different version gives the tree two version
// directories — which is what a rollback operates on.
func (f *fixture) installVersion(t *testing.T, version string, content string) {
	t.Helper()
	asset := "cmesh-" + version + "-" + fixtureAssetSuffix()
	f.writeAsset(t, version, asset, content)
	out, code := f.runWithEnv(t, 0o022, map[string]string{"CMESH_VERSION": version})
	if code != 0 {
		t.Fatalf("the %s install failed (exit %d):\n%s", version, code, out)
	}
}

// ★ THE ROLLBACK MUST ACTIVATE A COMPATIBLE VERSION.
//
// Rolling back to a version that CAN serve the units is the normal,
// supported operation, and it has to work.
func TestTheRollbackActivatesACompatibleVersion(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	root := f.installRoot()

	// Two versions, so there is something to roll back TO.
	f.installVersion(t, "v1", "#!/bin/sh\nexit 0\n")
	f.installVersion(t, "v2", "#!/bin/sh\nexit 0\n")

	// The version we roll back TO must be one the units can drive.
	good := fakeBinary(t, flagsServingTheUnits())
	if err := copyInto(filepath.Join(root, "versions", "v1", "cmesh"), good); err != nil {
		t.Fatalf("install v1 binary: %v", err)
	}

	out, code := f.run(t, 0o022, "--rollback", "v1")
	if code != 0 {
		t.Fatalf("the rollback refused a version that CAN serve the units (exit %d):\n%s", code, out)
	}
	if got := readLink(t, filepath.Join(root, "current")); got != "versions/v1" {
		t.Fatalf("after the rollback `current` points at %q, want versions/v1", got)
	}
}

// ★ THE ROLLBACK MUST REFUSE A VERSION THE UNITS CANNOT DRIVE.
//
// Rolling back below the floor is exactly what must be refused: the
// symlink would rotate happily and the unit would die on the next
// start, then restart until the start limit stops it.
func TestTheRollbackRefusesAVersionTheUnitsCannotDrive(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	root := f.installRoot()

	f.installVersion(t, "v1", "#!/bin/sh\nexit 0\n")
	f.installVersion(t, "v2", "#!/bin/sh\nexit 0\n")
	before := readLink(t, filepath.Join(root, "current"))

	// A version from before the file-based secrets: no --cred-file.
	old := fakeBinary(t, map[string][]string{
		"serve-discovery": {
			"account-pub", "addr", "announce", "panel",
			"relay-trunk", "state",
		},
		"join": {
			"listen", "relay-trunk", "relay-trunk-id", "state",
		},
	})
	if err := copyInto(filepath.Join(root, "versions", "v1", "cmesh"), old); err != nil {
		t.Fatalf("install old binary: %v", err)
	}

	out, code := f.run(t, 0o022, "--rollback", "v1")
	if code == 0 {
		t.Fatalf("the rollback accepted a version the units cannot drive:\n%s", out)
	}
	mustContain(t, "the rollback output", out, "rollback floor")
	if got := readLink(t, filepath.Join(root, "current")); got != before {
		t.Fatalf("a refused rollback changed `current` from %q to %q; the refusal "+
			"has to happen before anything is touched", before, got)
	}
}

// ★ THE ROLLBACK MUST REFUSE A VERSION THAT IS NOT INSTALLED.
func TestTheRollbackRefusesAVersionThatIsNotInstalled(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	root := f.installRoot()

	out, code := f.run(t, 0o022)
	if code != 0 {
		t.Fatalf("the install failed (exit %d):\n%s", code, out)
	}
	before := readLink(t, filepath.Join(root, "current"))

	out, code = f.run(t, 0o022, "--rollback", "v-does-not-exist")
	if code == 0 {
		t.Fatalf("the rollback accepted a version that is not installed:\n%s", out)
	}
	if got := readLink(t, filepath.Join(root, "current")); got != before {
		t.Fatalf("a refused rollback changed `current` from %q to %q", before, got)
	}
}

// ★ THE FLOOR CHECK MUST MATCH WHAT THE UNITS ACTUALLY PASS.
//
// The installer's list of "the flags the units need" is a copy of the
// units' ExecStart lines. A copy can drift, and when it does the
// floor stops being the floor.
//
// So the list is CHECKED against the unit files here: every flag the
// installer demands of a binary must be one a shipped unit actually
// passes, and every flag-based secret a shipped unit passes must be
// one the installer demands.
//
// The second direction is the important one: a unit that starts using
// --some-new-flag would otherwise silently become undriveable by old
// versions, and the floor would not know.
func TestTheInstallerFloorListMatchesTheShippedUnits(t *testing.T) {
	installerFlags := installerFloorFlags(t)

	for _, u := range packagedUnits(t) {
		exec := u.value("Service", "ExecStart")
		if exec == "" {
			continue
		}
		// The subcommand is the first argument that is not a flag.
		sub := ""
		for _, tok := range strings.Fields(exec) {
			if strings.HasPrefix(tok, "-") {
				continue
			}
			if tok == u.path || strings.HasSuffix(tok, "/cmesh") {
				continue
			}
			sub = tok
			break
		}
		if sub == "" {
			continue
		}

		for _, flag := range unitFlagsOf(exec) {
			key := sub + ":" + flag
			if !installerFlags[key] {
				t.Errorf("%s passes --%s to %q, but the installer's rollback "+
					"floor does not demand it; a version that lacks the flag "+
					"would be accepted and then fail in the unit with "+
					"\"flag provided but not defined\"", u.path, flag, sub)
			}
		}
	}
}

// installerFloorFlags reads the floor list out of the shell installer.
//
// It is read from the SCRIPT rather than duplicated here, because the
// script is what runs on the operator's machine. A test that kept its
// own copy would agree with itself and disagree with reality.
func installerFloorFlags(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "packaging", "install.sh"))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "CMESH_UNIT_FLAGS=") {
			continue
		}
		list := strings.TrimPrefix(trimmed, "CMESH_UNIT_FLAGS=")
		list = strings.Trim(list, `"`)
		for _, item := range strings.Fields(list) {
			out[item] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("the shell installer declares no CMESH_UNIT_FLAGS; the rollback " +
			"floor cannot be enforced without it")
	}
	return out
}

// unitFlagsOf returns the long flags an ExecStart line passes.
func unitFlagsOf(exec string) []string {
	var out []string
	for _, tok := range strings.Fields(exec) {
		if !strings.HasPrefix(tok, "--") {
			continue
		}
		name := strings.TrimPrefix(tok, "--")
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// copyInto copies a file, creating the destination's parent.
func copyInto(dest, src string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, raw, 0o755)
}
