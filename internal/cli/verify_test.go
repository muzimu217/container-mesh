// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// Tests for `cmesh verify`.
//
// The property that matters: a file that does NOT match the published
// digest must fail loudly and non-zero, because the whole point of the
// command is to be the step a user does NOT skip.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyAcceptsMatchingDigest(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("binary payload")
	target := filepath.Join(dir, "cmesh-latest-linux-amd64")
	if err := os.WriteFile(target, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	sums := filepath.Join(dir, "SHA256SUMS")
	// sha256sum's format: digest, two spaces, name.
	if err := os.WriteFile(sums, []byte(
		"0000000000000000000000000000000000000000000000000000000000000000  other\n"+
			digest+"  cmesh-latest-linux-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := Run([]string{"verify", "--file", target, "--checksums", sums}); err != nil {
			t.Fatalf("verify rejected a matching file: %v", err)
		}
	})
	if !strings.Contains(out, "ok") {
		t.Fatalf("verify output does not report success: %q", out)
	}
}

func TestVerifyRejectsMismatchedDigest(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cmesh-latest-linux-amd64")
	if err := os.WriteFile(target, []byte("binary payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	sums := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sums, []byte(
		"0000000000000000000000000000000000000000000000000000000000000000  cmesh-latest-linux-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Run([]string{"verify", "--file", target, "--checksums", sums})
	if err == nil {
		t.Fatal("verify accepted a file whose digest does not match")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("the failure does not say what went wrong: %v", err)
	}
}

func TestVerifyRejectsUnknownNameAndMissingArguments(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cmesh")
	if err := os.WriteFile(target, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	sums := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sums, []byte("deadbeef  somethingelse\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"verify", "--file", target, "--checksums", sums}); err == nil {
		t.Fatal("verify accepted a file the manifest does not list")
	}
	if err := Run([]string{"verify"}); err == nil {
		t.Fatal("verify accepted a call with no file")
	}
	if err := Run([]string{"verify", "--file", target}); err == nil {
		t.Fatal("verify accepted a call with nothing to verify against")
	}
}
