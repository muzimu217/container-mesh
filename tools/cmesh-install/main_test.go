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
