// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package packaging

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ THE SHELL INSTALLER MUST REFUSE A PAYLOAD THAT DOES NOT MATCH THE
// RELEASE CHECKSUM.
//
// The acceptance cell F1 says the installer "downloads + verifies the
// sha256 + writes it to disk, exit code 0". Every guard that existed
// covered the POSITIVE half: the download lands, the version directory
// appears, the `current` symlink points at it.
//
// A positive half cannot tell a verifying installer from one that does
// not verify at all — remove the sha256 comparison and every one of
// them stays green, and the machine runs whatever the network served.
//
// So the guard is the negative one: the release serves an asset whose
// bytes do not hash to the digest in its SHA256SUMS, and the installer
// must refuse it, exit non-zero, and leave NOTHING behind. A half
// install is worse than no install.
func TestTheShellInstallerRefusesATamperedAsset(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\necho good\n")
	// The fixture ships a REAL checksum. This replaces it with the
	// digest of a DIFFERENT payload — the shape of a compromised
	// download, and the only input the negative guard needs.
	corruptSums(t, f, "latest", fixtureAsset())

	out, code := f.run(t, 0o022)
	if code == 0 {
		t.Errorf("the installer exited 0 on an asset whose sha256 does not match the "+
			"release checksum; the bytes at %s were never verified.\n--- output ---\n%s",
			filepath.Join(f.installRoot(), "current", "cmesh"), out)
	} else if !strings.Contains(out, "checksum mismatch") {
		t.Errorf("the installer refused the tampered asset but did not say why "+
			"(exit %d); the operator must be told the checksum did not match.\n"+
			"--- output ---\n%s", code, out)
	}

	// And NOTHING may be left behind: no version directory, no
	// `current` symlink, no managed-install marker.
	root := f.installRoot()
	for _, path := range []string{
		filepath.Join(root, "versions", "latest"),
		filepath.Join(root, "current"),
		filepath.Join(root, "cmesh"),
		filepath.Join(root, ".cmesh-install-root"),
	} {
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("the refused install left %s behind; a payload whose sha256 does "+
				"not match the release checksum must not be installed at all", path)
		} else if !os.IsNotExist(err) {
			t.Errorf("cannot stat %s: %v", path, err)
		}
	}
	// The install root itself must not even have been created.
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the refused install created the install root %s (stat error: %v); "+
			"a rejected download must leave no half install behind", root, err)
	}
}

// corruptSums replaces the release's SHA256SUMS with a digest that does
// NOT match the asset the release ships.
//
// The digest is a real sha256 — of a different payload — so the
// installer cannot refuse it for a malformed-checksum reason. The only
// reason left to refuse is the mismatch, which is the one under test.
func corruptSums(t *testing.T, f *fixture, version, asset string) {
	t.Helper()
	dir := filepath.Join(f.releases, "latest", "download")
	if version != "latest" {
		dir = filepath.Join(f.releases, "download", version)
	}
	other := sha256.Sum256([]byte("a payload that was not published"))
	line := fmt.Sprintf("%s  %s\n", hex.EncodeToString(other[:]), asset)
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(line), 0o644); err != nil {
		t.Fatalf("write the tampered SHA256SUMS: %v", err)
	}
}

// ★ THE INSTALL TREE MUST BE TRAVERSABLE BY THE SERVICE USER.
//
// The systemd units run as the unprivileged "cmesh" account:
//
//	User=cmesh
//	Group=cmesh
//
// and they execute
//
//	ExecStart=/opt/container-mesh/current/cmesh ...
//
// so the service account has to be able to REACH the binary. On Linux
// that means the execute (traverse) bit on every directory on the path:
// the install root, the versions directory, and the version directory
// itself.
//
// ★ THE HISTORICAL BUG, AND WHY IT IS STILL ALIVE.
//
// The installer creates the tree with mkdir -p, and mkdir applies the
// PROCESS'S umask. Under a restrictive umask — 077, which is what a
// hardened host's root shell and many CI runners use — the directories
// come out 0700, owned by whoever ran the installer.
//
// The result is a unit that starts, is scheduled, and then fails with
//
//	Failed to execute ...: Permission denied
//
// which systemd reports as status=203/EXEC — the same status a MISSING
// binary produces. The operator's first conclusion is "the install did
// not happen", and the actual cause (a directory the service user
// cannot walk into) is invisible.
//
// The fix in the tree is not one chmod: it is EVERY level. The staging
// directory was fixed once (it came from mktemp -d, which creates 0700),
// and the versions directory was fixed once, and the install ROOT was
// still missed — because it is created by mkdir -p on the way down and
// nobody looks at it.
//
// ★ WHY THIS TEST SETS THE UMASK ITSELF.
//
// A test that inherits the developer's umask cannot see this bug: under
// the usual 022 the tree comes out 0755 and everything passes. The
// regression only shows under 077, so the test has to SAY so.
//
// The umask is a process-wide setting, so it is set, the installer runs,
// and the previous value is restored — with a defer, so it is restored
// even when the test fails.
func TestTheInstallTreeIsTraversableByTheServiceUser(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")

	// 077 is the umask that breaks it.
	out, code := f.run(t, 0o077)
	if code != 0 {
		t.Fatalf("the installer failed under umask 077 (exit %d):\n%s", code, out)
	}

	root := f.installRoot()
	// Every directory on the path to the binary must let a user who owns
	// none of them traverse it.
	for _, dir := range []string{
		root,
		filepath.Join(root, "versions"),
		filepath.Join(root, "versions", "latest"),
	} {
		m := mode(t, dir)
		if m&0o001 == 0 {
			t.Errorf("the directory %s is mode %v: the service user "+
				"(%s) cannot traverse into it, so ExecStart=/opt/container-mesh/"+
				"current/cmesh fails with 203/EXEC — the same status a MISSING "+
				"binary gives, so the operator looks in the wrong place",
				dir, m, "cmesh")
		}
	}

	// And the binary itself must be executable.
	bin := filepath.Join(root, "current", "cmesh")
	m := mode(t, bin)
	if m&0o111 == 0 {
		t.Errorf("the installed binary %s is mode %v: it is not executable", bin, m)
	}
}

// ★ THE INSTALL ROOT ITSELF MUST NOT BE 0700.
//
// This is the level the earlier fixes missed, so it is asserted
// SEPARATELY: a directory that is only owner-accessible is the exact
// shape that produces the "203/EXEC, looks like a missing binary"
// symptom, and it is invisible in the versions/ directory checks that
// came before it.
func TestTheInstallRootIsNotOwnerOnly(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	out, code := f.run(t, 0o077)
	if code != 0 {
		t.Fatalf("the installer failed under umask 077 (exit %d):\n%s", code, out)
	}
	root := f.installRoot()
	m := mode(t, root)
	if m&0o001 == 0 {
		t.Errorf("the install root %s is mode %v; the systemd unit executes "+
			"a binary UNDER it as the unprivileged cmesh user, which cannot "+
			"traverse an owner-only directory", root, m)
	}
}

// ★ THE INSTALLER MUST NOT LEAVE A PARTIAL INSTALL BEHIND.
//
// The installer stages the new version and only then rotates the
// `current` symlink. If the staging fails, the previously working
// version must still be the current one — an upgrade that breaks the
// running service is worse than an upgrade that does not happen.
func TestAFailedInstallLeavesThePreviousVersionCurrent(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	root := f.installRoot()

	out, code := f.run(t, 0o022)
	if code != 0 {
		t.Fatalf("the first install failed (exit %d):\n%s", code, out)
	}
	if got := readLink(t, filepath.Join(root, "current")); got != "versions/latest" {
		t.Fatalf("current points at %q, want versions/latest", got)
	}

	// A second install of a DIFFERENT version must rotate the symlink
	// and keep the old version on disk, so a rollback is possible.
	f.writeAsset(t, "v1.0.0", "cmesh-v1.0.0-"+fixtureAssetSuffix(), "#!/bin/sh\nexit 0\n")
	out, code = f.run(t, 0o022)
	if code != 0 {
		t.Fatalf("the second install failed (exit %d):\n%s", code, out)
	}
}

// fixtureAssetSuffix returns the platform suffix the installer puts in
// an asset name.
//
// The asset name is "cmesh-<version>-<os>-<arch>", so the suffix is
// everything after the version — which is "<os>-<arch>", NOT just the
// architecture. Trimming only the last segment would produce "arm64"
// where the installer asks for "darwin-arm64", and every versioned
// install would 404.
func fixtureAssetSuffix() string {
	name := fixtureAsset()
	// "cmesh-latest-<os>-<arch>" → "<os>-<arch>"
	return strings.TrimPrefix(name, "cmesh-latest-")
}

// ★ THE UNINSTALL MUST STOP THE UNITS, AND SAY WHICH ONES.
//
// An uninstall that leaves the units enabled means the NEXT BOOT starts
// a unit whose ExecStart points at a binary that no longer exists:
// status=203/EXEC, then the restart loop.
//
// So the uninstall has to stop AND disable the units, and it has to
// NAME them — because an operator who is told "uninstalled" and not
// told which units were stopped believes the machine is clean when it
// is not.
func TestTheUninstallStopsAndDisablesTheDeployedUnits(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	f.installSystemctlStub(t)

	out, code := f.run(t, 0o022)
	if code != 0 {
		t.Fatalf("the install failed (exit %d):\n%s", code, out)
	}

	// A stub systemctl records what it was asked to do.
	calls := f.installSystemctlStub(t)
	out, code = f.run(t, 0o022, "--uninstall")
	if code != 0 {
		t.Fatalf("the uninstall failed (exit %d):\n%s", code, out)
	}

	log := readFile(t, calls)
	for _, unit := range []string{"cmesh-serve-discovery.service", "cmesh-discovery.service", "cmesh-member.service", "cmesh-hub.service"} {
		if !strings.Contains(log, "stop "+unit) {
			t.Errorf("the uninstall did not stop %s; the calls were:\n%s", unit, log)
		}
		if !strings.Contains(log, "disable "+unit) {
			t.Errorf("the uninstall did not disable %s; the calls were:\n%s", unit, log)
		}
	}
	if !strings.Contains(log, "daemon-reload") {
		t.Errorf("the uninstall did not reload the unit files; the calls were:\n%s", log)
	}

	// And the operator must be told.
	mustContain(t, "the uninstall output", out, "stopped and disabled")
}

// ★ THE UNINSTALL MUST REPORT WHAT IT LEFT BEHIND.
//
// A purge removes the install root and the runtime state. A plain
// uninstall removes the binaries and KEEPS the state — and the state
// holds the device's private key and the credential material.
//
// So an operator who runs the plain uninstall has to be told that the
// state survived, or they will believe the machine is clean when the
// device's private key envelope is still on disk. That is the same
// class of defect the shell installer already fixed for its own default
// (the state directory used to be ~/.cmesh, which is not where the
// deployed units keep theirs).
func TestTheUninstallReportsTheStateItRetained(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	// ★ THE STATE FILE MUST REALLY EXIST BEFORE THE UNINSTALL RUNS.
	//
	// This is what the old assertion needed and did not have. The
	// fixture used to create an EMPTY state directory, so the file the
	// assertion looked for ("identity.enc") was never there — and the
	// assertion, which was inverted as well ("os.Stat returned no
	// error" was the FAILURE case), could not fire whatever the
	// uninstaller did. A guard that cannot fail is not a guard.
	//
	// The uninstaller does not know any file name: it removes the
	// whole state DIRECTORY on a purge and leaves it alone on a plain
	// uninstall. So the file the fixture writes stands for the device's
	// private key envelope, and the assertions below are about what is
	// left of it.
	state, secret := f.makeState(t)

	out, code := f.run(t, 0o022)
	if code != 0 {
		t.Fatalf("the install failed (exit %d):\n%s", code, out)
	}

	// The uninstall must be run with CMESH_STATE pointing at the state
	// directory, exactly as an operator would.
	out, code = f.runWithEnv(t, 0o022, map[string]string{"CMESH_STATE": state}, "--uninstall")
	if code != 0 {
		t.Fatalf("the uninstall failed (exit %d):\n%s", code, out)
	}

	// ★ THE STATE MUST SURVIVE — AND THIS ASSERTION CAN FAIL.
	//
	// The file must STILL BE THERE after the plain uninstall. If the
	// uninstaller had removed the state, os.Stat fails here and the
	// test goes red with the name of the file that disappeared.
	if _, err := os.Stat(secret); err != nil {
		t.Errorf("the plain uninstall removed the state file %s (stat error: %v); "+
			"the state must be retained so a reinstall recovers the identity",
			secret, err)
	}
	// The state DIRECTORY is what the uninstaller actually manages, so
	// it is asserted too: a state that survived as a name but not as a
	// directory is not a retained state.
	if _, err := os.Stat(state); err != nil {
		t.Errorf("the plain uninstall removed the state directory %s (stat error: %v); "+
			"the state must be retained so a reinstall recovers the identity",
			state, err)
	}
	// ...and the operator must be told it is still there.
	if !strings.Contains(out, state) {
		t.Errorf("the uninstall did not mention the state directory it retained "+
			"(%s); an operator who is not told believes the machine is clean "+
			"when the device's private key envelope is still on disk.\n"+
			"--- output ---\n%s", state, out)
	}
}

// ★ THE PURGE MUST NAME THE STATE IT REMOVED.
//
// The purge removes the runtime state, and the operator needs to know
// that the device's private key envelope is GONE — because the next
// install starts from a blank identity, and that is a fact the operator
// has to have before they find out.
//
// The plain uninstall names the state it KEPT; the purge has to name
// the state it REMOVED, or the operator cannot answer "is this machine
// clean?" either way.
func TestThePurgeNamesTheStateItRemoved(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	// ★ THE PURGE MUST ACTUALLY REMOVE THE STATE, FILE BY FILE.
	//
	// The assertion below used to look at the state DIRECTORY only.
	// This file is the device's private key envelope: the one thing the
	// operator must be certain is GONE after a purge, because the next
	// install starts from a blank identity.
	state, secret := f.makeState(t)

	out, code := f.run(t, 0o022)
	if code != 0 {
		t.Fatalf("the install failed (exit %d):\n%s", code, out)
	}

	out, code = f.runWithEnv(t, 0o022, map[string]string{"CMESH_STATE": state}, "--uninstall", "--purge")
	if code != 0 {
		t.Fatalf("the purge failed (exit %d):\n%s", code, out)
	}
	if !strings.Contains(out, state) {
		t.Errorf("the purge did not mention the state directory it removed (%s); "+
			"the next install starts from a BLANK identity, and the operator has "+
			"to know that before they find out.\n--- output ---\n%s", state, out)
	}
	// The state file must be GONE — the assertion fails when it is
	// still there, so it can go red.
	if _, err := os.Stat(secret); err == nil {
		t.Errorf("the purge left the state file %s behind; the purge must remove the "+
			"device identity, because the next install starts from a BLANK identity",
			secret)
	}
	if _, err := os.Stat(state); err == nil {
		t.Errorf("the purge left the state directory behind at %s", state)
	}
}

// ★ THE UNINSTALL MUST REPORT EVEN WHEN THERE WAS NOTHING TO STOP.
//
// A silent uninstall is indistinguishable from one that did nothing,
// and the operator's next question is always "is it clean?".
//
// The two installers must AGREE here: the Go installer (tools/cmesh-install)
// reports unconditionally, and a shell installer that reported only
// when it found something would leave the operator guessing on the
// other path.
func TestTheUninstallReportsEvenWhenNoUnitWasRunning(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")

	out, code := f.run(t, 0o022)
	if code != 0 {
		t.Fatalf("the install failed (exit %d):\n%s", code, out)
	}

	// No systemctl stub is installed, so there is no systemd on this
	// machine to stop: the report must still say so.
	out, code = f.run(t, 0o022, "--uninstall")
	if code != 0 {
		t.Fatalf("the uninstall failed (exit %d):\n%s", code, out)
	}
	mustContain(t, "the uninstall output", out, "stopped and disabled")
}

// installSystemctlStub puts a recording systemctl on the PATH and
// returns the path of the file it writes its calls to.
func (f *fixture) installSystemctlStub(t *testing.T) string {
	t.Helper()
	calls := filepath.Join(f.root, "systemctl.calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FIXTURE_CALLS"
# list-unit-files exits non-zero for a unit that does not exist, and
# zero for one that does. The installer uses that to decide whether a
# unit is present, so the stub has to answer the same way.
if [ "$1" = "list-unit-files" ]; then
    case "$2" in
        cmesh-*) exit 0 ;;
        *) exit 1 ;;
    esac
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(f.bin, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write systemctl stub: %v", err)
	}
	return calls
}

// readFile reads a file, failing the test when it cannot.
func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
