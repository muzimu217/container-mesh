// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallPayloadCreatesVersionedLayout(t *testing.T) {
	root := t.TempDir()
	if err := installPayload(root, "v1.2.3", []byte("first")); err != nil {
		t.Fatal(err)
	}
	assertInstallLink(t, filepath.Join(root, "current"), filepath.Join("versions", "v1.2.3"))
	assertInstallLink(t, filepath.Join(root, "cmesh"), filepath.Join("current", "cmesh"))
	got, err := os.ReadFile(filepath.Join(root, "current", "cmesh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("installed payload = %q, want first", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".cmesh-install-root")); err != nil {
		t.Fatalf("managed install marker missing: %v", err)
	}
}

func TestInstallPayloadSwitchesCurrentWithoutTouchingState(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "identity.enc"), []byte("secret envelope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installPayload(root, "v1", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := installPayload(root, "v2", []byte("new")); err != nil {
		t.Fatal(err)
	}
	assertInstallLink(t, filepath.Join(root, "current"), filepath.Join("versions", "v2"))
	for _, want := range []string{"v1", "v2"} {
		if _, err := os.Stat(filepath.Join(root, "versions", want, "cmesh")); err != nil {
			t.Fatalf("version %s missing after upgrade: %v", want, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(state, "identity.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret envelope" {
		t.Fatal("runtime state was changed by binary installation")
	}
}

func TestUninstallRetainsStateUnlessPurged(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "identity.enc"), []byte("secret envelope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installPayload(root, "v1", []byte("binary")); err != nil {
		t.Fatal(err)
	}
	if err := uninstall(root, state, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "identity.enc")); err != nil {
		t.Fatalf("normal uninstall removed state: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("normal uninstall removed the root itself: %v", err)
	}
	if err := installPayload(root, "v2", []byte("binary")); err != nil {
		t.Fatal(err)
	}
	if err := uninstall(root, state, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("purge left install root behind: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("purge left state behind: %v", err)
	}
}

func TestChecksumForRejectsMalformedDigest(t *testing.T) {
	asset := "cmesh-v1-linux-amd64"
	if got := checksumFor([]byte(strings.Repeat("a", 63)+"  "+asset+"\n"), asset); got != "" {
		t.Fatalf("accepted short checksum %q", got)
	}
}

// ---------------------------------------------------------------------------
// THE ROOT GUARDS. (Round 98)
//
// The purge branch used to call os.RemoveAll(state) with NO check on the
// state path, and Go's os.RemoveAll has no root failsafe. So
//
//	CMESH_STATE=/ cmesh-install --uninstall --purge
//
// deleted the filesystem root. It was not theoretical: it happened on a
// host running production web services, during the audit that found it.
//
// These tests are the guard's own red→green. They use paths that RESOLVE
// to the root without LOOKING like the root — "/..", "/.", "//" — because
// the string comparison the shell installer used is bypassed by exactly
// those.
// ---------------------------------------------------------------------------

// TestPurgeRefusesTheFilesystemRoot is the regression test for the
// incident.
//
// ★★ THIS TEST MUST NEVER BE ABLE TO DELETE THE MACHINE. ★★
//
// The first version of this test called uninstall(root, "/", true)
// directly. That is correct ONLY while the guard holds — and when the
// guard was removed to prove the test catches it, the test performed
// os.RemoveAll("/") on the machine running it, for 72 seconds.
//
// A regression test that damages the machine when the regression is
// present is not a regression test; it is a second incident waiting for
// the next person who removes a line to check that the test goes red.
// The lesson is the same one the production incident taught, and it
// applies to the test as much as to the code:
//
// A TEST THAT EXERCISES A DESTRUCTIVE PATH MUST NEVER REACH THE REAL
// PATH. IT MUST PROVE THE REFUSAL AT THE DECISION POINT.
//
// So the test exercises refuseDangerousPath — the function that MAKES
// the decision — and never calls uninstall() with a root-shaped path.
// The wiring between them (that uninstall actually calls it) is checked
// by TestPurgeRefusesTheFilesystemRootThroughUninstall, which uses a
// path that is SAFE to delete but still exercises the same branch.
func TestPurgeRefusesTheFilesystemRoot(t *testing.T) {
	// Every one of these resolves to the filesystem root.
	for _, bad := range []string{
		"/",
		"//",
		"/.",
		"/..",
		"/tmp/..",
	} {
		if err := refuseDangerousPath(bad, "runtime state directory"); err == nil {
			t.Fatalf("the guard accepted the state path %q: it resolves to the filesystem root "+
				"and os.RemoveAll would delete the machine", bad)
		}
	}

	// And a state path that sits DIRECTLY in the root: "${HOME:-}/.cmesh"
	// is exactly that when HOME is unset.
	if err := refuseDangerousPath("/.cmesh-should-never-be-created", "runtime state directory"); err == nil {
		t.Fatal("the guard accepted a state path directly in the filesystem root")
	}
}

// TestPurgeRefusesTheFilesystemRootThroughUninstall checks the WIRING:
// that uninstall() actually consults the guard.
//
// It must not point the tool at a real root. It uses a path that the
// guard refuses for the DIRECT-CHILD reason (a directory directly in the
// filesystem root) — which is a refusal the guard makes on its own
// account, and which is safe because the path does not exist and the
// refusal happens BEFORE any removal.
//
// A test that asserts "uninstall refuses X" only proves anything if
// uninstall really calls the guard; and a test that points uninstall at
// "/" proves it by risking the machine.
func TestPurgeRefusesTheFilesystemRootThroughUninstall(t *testing.T) {
	root := t.TempDir()
	if err := installPayload(root, "v1", []byte("binary")); err != nil {
		t.Fatal(err)
	}

	// "/.cmesh-should-never-be-created" does not exist, and it sits
	// directly in the filesystem root, so the guard refuses it — before
	// any removal is attempted. If the wiring were missing, the purge
	// would try to remove that (non-existent) path, which is harmless
	// here — and the next assertion below is what actually fails.
	err := uninstall(root, "/.cmesh-should-never-be-created", true)
	if err == nil {
		t.Fatal("the purge accepted a state path directly in the filesystem root: " +
			"uninstall() is not consulting the guard")
	}

	// And the install root must have survived: the refusal happens
	// BEFORE any removal.
	if _, err := os.Stat(filepath.Join(root, "current")); err != nil {
		t.Fatalf("the install root was damaged by a refused purge: %v", err)
	}
}

// TestPurgeStillWorksForARealStateDirectory is the other half: the guard
// must not refuse the paths it is supposed to allow.
//
// A guard that refuses everything would pass the test above.
func TestPurgeStillWorksForARealStateDirectory(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(t.TempDir(), "nested", "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := installPayload(root, "v1", []byte("binary")); err != nil {
		t.Fatal(err)
	}
	if err := uninstall(root, state, true); err != nil {
		t.Fatalf("the purge refused an ordinary state directory: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("purge left the state behind: %v", err)
	}
}

// TestTheInstallRootIsRefusedBeforeAnythingIsFetched pins the install
// branch's guard.
//
// The two installers ship side by side, and the shell one refuses the
// filesystem root on the install branch. If the Go one does not, the
// safety depends on which installer the operator happened to use.
func TestTheInstallRootIsRefusedBeforeAnythingIsFetched(t *testing.T) {
	for _, bad := range []string{"/", "//", "/.", "/.."} {
		if err := refuseRootOnly(bad, "install root"); err == nil {
			t.Fatalf("the install branch accepted the install root %q: it resolves to the filesystem root", bad)
		}
	}
	// A directory that sits directly in the root is a LEGITIMATE install
	// root ("/cmesh" is one in the field) — the install branch only adds
	// files under it.
	if err := refuseRootOnly("/cmesh", "install root"); err != nil {
		t.Fatalf("the install branch refused a legitimate install root: %v", err)
	}
}

func assertInstallLink(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	if got != want {
		t.Fatalf("link %s = %q, want %q", path, got, want)
	}
}
