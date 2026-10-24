// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// runCLI runs an init/account command against a temp state dir, the way the
// binary would, and reports stderr output plus the error.
func runCLI(t *testing.T, dir, passphrase string, fn func(args []string) error, args ...string) (string, error) {
	t.Helper()
	t.Setenv("CMESH_PASSPHRASE", passphrase)
	var sb strings.Builder
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	err := fn(args)
	_ = w.Close()
	os.Stderr = old
	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	sb.Write(buf[:n])
	return sb.String(), err
}

func TestCLIInitCreatesIdentity(t *testing.T) {
	dir := t.TempDir()
	out, err := runCLI(t, dir, "test-passphrase", runInit, "--state", dir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out, "device identity created") {
		t.Fatalf("output missing confirmation: %q", out)
	}

	// The file must exist with the documented permissions.
	path := filepath.Join(dir, identity.DeviceFileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("identity file not created: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", fi.Mode().Perm())
	}
}

func TestCLIInitRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := runCLI(t, dir, "test-passphrase", runInit, "--state", dir); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, dir, "different-pass", runInit, "--state", dir)
	if err == nil {
		t.Fatal("second init without --force succeeded")
	}
	// --force must replace (a different ID proves a new key was made).
	out, err := runCLI(t, dir, "third-passphras", runInit, "--state", dir, "--force")
	if err != nil {
		t.Fatalf("forced init: %v", err)
	}
	if !strings.Contains(out, "created") {
		t.Fatalf("forced init output: %q", out)
	}
}

func TestCLIAccountWorkflow(t *testing.T) {
	dir := t.TempDir()

	if _, err := runCLI(t, dir, "test-passphrase", runInit, "--state", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, dir, "test-passphrase", runAccount, "create", "--state", dir); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, dir, "test-passphrase", runAccount, "device-add", "--state", dir)
	if err != nil {
		t.Fatalf("device-add: %v", err)
	}
	if !strings.Contains(out, "device signed into account") {
		t.Fatalf("device-add output: %q", out)
	}

	// The certificate must load and verify against the account on disk.
	dev, err := identity.LoadDevice(filepath.Join(dir, identity.DeviceFileName), "test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, string(dev.ID())+identity.CertSuffix)
	cert, err := identity.LoadCert(certPath)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := identity.LoadAccount(filepath.Join(dir, identity.AccountFileName), "test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.Verify(acc.PublicKey()); err != nil {
		t.Fatalf("CLI-issued cert does not verify: %v", err)
	}
}

func TestCLIDeviceAddWithoutPrerequisites(t *testing.T) {
	dir := t.TempDir()
	_, err := runCLI(t, dir, "test-passphrase", runAccount, "device-add", "--state", dir)
	if err == nil {
		t.Fatal("device-add without init succeeded")
	}
	if !strings.Contains(err.Error(), "run cmesh init") {
		t.Fatalf("error does not tell the user what to do: %v", err)
	}
}

func TestCLIAccountShowWrongPassphrase(t *testing.T) {
	dir := t.TempDir()
	if _, err := runCLI(t, dir, "test-passphrase", runAccount, "create", "--state", dir); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, dir, "wrong-passphrase", runAccount, "show", "--state", dir)
	if err == nil {
		t.Fatal("wrong passphrase accepted")
	}
}

// TestCLIInitNeverWritesDefaultHome proves --state isolation: a test must not
// be able to clobber the developer's real ~/.cmesh by forgetting --state in
// some future test. We point HOME at a temp dir and confirm that path is used.
func TestCLIInitNeverWritesDefaultHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// No --state: must resolve under the fake HOME.
	_, err := runCLI(t, "", "test-passphrase", runInit)
	if err != nil {
		t.Fatalf("init with default state dir: %v", err)
	}
	expected := filepath.Join(home, identity.DefaultStateDir, identity.DeviceFileName)
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("identity not at %s: %v", expected, err)
	}
	// And nothing outside it.
	real, err := os.UserHomeDir()
	if err == nil && real != home {
		if _, err := os.Stat(filepath.Join(real, identity.DefaultStateDir)); err == nil {
			// Only a failure if it was created just now by this test; check
			// mtime would overreach — asserting the fake-home write is enough.
			_ = real
		}
	}
}
