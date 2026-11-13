// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

// Tests for the release builder.
//
// The property that matters most: a FAILED release must leave nothing
// behind. The earlier version copied the binaries, then failed while
// building the first archive (the installer was missing), leaving copied
// binaries and a truncated tar.gz with no SHA256SUMS. That half-product
// passes `gzip -t` and looks like a release to an automated upload step —
// so the failure was invisible downstream (doubao-cloud-vm-01, round 56).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBinary makes a fake built binary.
func writeBinary(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestAssembleRefusesToShipWithoutInstaller: the missing installer must be
// a LOUD failure that names the file.
func TestAssembleRefusesToShipWithoutInstaller(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")

	err := assemble(bin, out, "v1.0.0", filepath.Join(root, "packaging", "install.sh"))
	if err == nil {
		t.Fatal("a release without an installer was produced")
	}
	if !strings.Contains(err.Error(), "install.sh") {
		t.Fatalf("the failure does not name the missing file: %v", err)
	}
}

// TestAssembleProducesCompleteRelease: with the installer present, the
// release has binaries, archives, the installer and a checksum manifest.
func TestAssembleProducesCompleteRelease(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")
	writeBinary(t, bin, "cmesh-v1.0.0-darwin-arm64")
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := assemble(bin, out, "v1.0.0", installer); err != nil {
		t.Fatalf("the release failed: %v", err)
	}
	for _, name := range []string{
		"cmesh-v1.0.0-linux-amd64",
		"cmesh-v1.0.0-linux-amd64.tar.gz",
		"cmesh-v1.0.0-darwin-arm64",
		"cmesh-v1.0.0-darwin-arm64.tar.gz",
		"install.sh",
		"SHA256SUMS",
	} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("the release is missing %s: %v", name, err)
		}
	}
	// The manifest must cover every downloadable artefact.
	sums, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(sums)), "\n")
	// 2 versioned binaries + 2 aliases + 2 archives + install.sh
	if len(lines) != 7 {
		t.Fatalf("the manifest lists %d files, want 7:\n%s", len(lines), sums)
	}
}

// TestAssembleEmitsLatestAliases: the documented default download URL
// (releases/latest/download/cmesh-latest-<os>-<arch>) only resolves when the
// release carries an asset with that literal name (doubao-cloud-vm-01, round
// 56: the URL was documented but nothing produced it).
func TestAssembleEmitsLatestAliases(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")
	writeBinary(t, bin, "cmesh-v1.0.0-windows-amd64.exe")
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := assemble(bin, out, "v1.0.0", installer); err != nil {
		t.Fatalf("the release failed: %v", err)
	}
	for _, alias := range []string{"cmesh-latest-linux-amd64", "cmesh-latest-windows-amd64.exe"} {
		p := filepath.Join(out, alias)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the release is missing the %s alias: %v", alias, err)
			continue
		}
		// The alias must be the SAME binary, not a stub.
		versioned := strings.Replace(alias, "cmesh-latest-", "cmesh-v1.0.0-", 1)
		a, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(out, versioned))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Errorf("the %s alias does not match %s", alias, versioned)
		}
	}
}

// TestAssembleRejectsUnsafeVersion: a version that could escape into a
// path or a command must be refused.
func TestAssembleRejectsUnsafeVersion(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"", "v1.0.0; rm -rf /", "../escape", "v1/0"} {
		if err := assemble(bin, filepath.Join(root, "out"), v, installer); err == nil {
			t.Fatalf("the unsafe version %q was accepted", v)
		}
	}
}
