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

// ★ A VERSION STRING MUST NOT BE ABLE TO WALK OUT OF `versions/`.
//
// `safeVersion` allowed "." as a character, so ".." and "." passed it.
// The version is then joined into a path:
//
//	bin  = root/versions/<version>/cmesh
//	link = root/versions/<version>
//
// With version=".." both COLLAPSE out of the versions directory:
//
//	bin  = root/cmesh      ← the "current" symlink, not a version
//	link = root            ← the install root itself
//
// So `cmesh-install --rollback ..` would rotate `current` to point at
// the install ROOT, and the documented layout
// (<install>/current -> versions/<version>) would be destroyed. The
// next start would then run whatever `root/cmesh` is — the symlink to
// the version that is being replaced.
//
// ★ IT IS NOT A SHELL INJECTION. (The security scanner flagged line 676
// as one; it is not.) Nothing here reaches a shell: every process spawn
// in this tool is `exec.Command(prog, args...)`, which calls execve
// directly with an argv list, so a value containing ";" or "$(...)" is
// passed as a LITERAL argument. The defect is a PATH one, and it is
// worth fixing on its own terms.

// TestTheVersionCannotWalkOutOfTheVersionsDirectory pins the rule.
func TestTheVersionCannotWalkOutOfTheVersionsDirectory(t *testing.T) {
	for _, v := range []string{"..", ".", "...", "...."} {
		if safeVersion(v) {
			t.Errorf("★ THE DEFECT: safeVersion accepted %q, and the version is "+
				"joined into root/versions/<version>/cmesh — so it collapses OUT of "+
				"the versions directory (with \"..\" the binary path becomes "+
				"root/cmesh and the symlink target becomes the install root itself)",
				v)
		}
	}

	// The real version names must still be accepted.
	for _, v := range []string{"v1.0.0", "v1.0.0-rc1", "1.0.0+build.5", "v2-rc_1"} {
		if !safeVersion(v) {
			t.Errorf("safeVersion rejected a legitimate version %q", v)
		}
	}
}

// TestTheRollbackTargetStaysUnderVersions is the behavioural half: the
// path that is actually used must stay inside the versions directory.
//
// It also proves the refusal happens BEFORE anything is touched — a
// rollback that half-succeeds leaves the service pointing at nothing.
func TestTheRollbackTargetStaysUnderVersions(t *testing.T) {
	root := t.TempDir()
	versions := filepath.Join(root, "versions")
	if err := os.MkdirAll(filepath.Join(versions, "v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cmesh-install-root"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A "current" symlink, so the rollback has something to rotate.
	if err := os.Symlink(filepath.Join(versions, "v1.0.0"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	// A binary for the good version. It is not a real cmesh, so the
	// compatibility probe refuses it — which is fine: the point is
	// that the PATH check fires FIRST.
	bin := filepath.Join(versions, "v1.0.0", "cmesh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The traversal must be refused, and nothing may be rotated.
	err := rollback(root, "..")
	if err == nil {
		t.Fatal("★ THE DEFECT: the rollback accepted version \"..\"")
	}
	if !strings.Contains(err.Error(), "unsafe version") {
		t.Fatalf("the refusal must name the version check, got: %v", err)
	}

	// The symlink must not have moved.
	target, lerr := os.Readlink(filepath.Join(root, "current"))
	if lerr != nil {
		t.Fatal(lerr)
	}
	if target != filepath.Join(versions, "v1.0.0") {
		t.Fatalf("★ THE DEFECT: the refused rollback still rotated \"current\" to %q", target)
	}
}

// ★ THE ROLLBACK MUST NOT EXECUTE A BINARY IT CANNOT VERIFY.
//
// The compatibility probe RUNS the binary (it asks it for "--help"), so
// the digest has to be checked BEFORE that call. An unverified binary
// must never be executed, not even to ask it a question.
//
// ★ TWO CASES, AND BOTH MUST BE REFUSED:
//  1. there is no recorded digest at all (the version was installed by
//     the SHELL installer, or long ago, or by hand);
//  2. the recorded digest does not match the bytes on disk (something
//     swapped the file after it was installed).
func TestTheRollbackRefusesABinaryItCannotVerify(t *testing.T) {
	stageVersion := func(t *testing.T, payload []byte) (string, string) {
		t.Helper()
		root := t.TempDir()
		versions := filepath.Join(root, "versions")
		if err := os.MkdirAll(filepath.Join(versions, "v1"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".cmesh-install-root"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(versions, "v1", "cmesh"), payload, 0o755); err != nil {
			t.Fatal(err)
		}
		return root, versions
	}

	writeDigest := func(t *testing.T, versions string, payload []byte) {
		t.Helper()
		sum := sha256.Sum256(payload)
		if err := os.WriteFile(filepath.Join(versions, "v1", digestFileName),
			[]byte(hex.EncodeToString(sum[:])+"  cmesh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("no-recorded-digest", func(t *testing.T) {
		root, _ := stageVersion(t, []byte("binary"))
		err := rollback(root, "v1")
		if err == nil {
			t.Fatal("★ THE DEFECT: the rollback accepted a version with no recorded digest")
		}
		if !strings.Contains(err.Error(), "no recorded digest") {
			t.Fatalf("the refusal must say why, got: %v", err)
		}
	})

	t.Run("the-digest-does-not-match", func(t *testing.T) {
		root, versions := stageVersion(t, []byte("the real binary"))
		// A digest of something ELSE — the file was swapped after the
		// install.
		writeDigest(t, versions, []byte("a different binary"))
		err := rollback(root, "v1")
		if err == nil {
			t.Fatal("★ THE DEFECT: the rollback accepted a binary whose digest does not match")
		}
		if !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("the refusal must say why, got: %v", err)
		}
	})

	t.Run("a-matching-digest-is-not-the-blocker", func(t *testing.T) {
		// The positive control: a version whose digest matches must
		// NOT be refused FOR THAT REASON. (It may be refused later for
		// another reason — the payload is not a real cmesh, so running
		// it fails with "exec format error" — and that is fine: the
		// point is that the DIGEST is not the blocker.)
		payload := []byte("the real binary")
		root, versions := stageVersion(t, payload)
		writeDigest(t, versions, payload)
		err := rollback(root, "v1")
		if err != nil && (strings.Contains(err.Error(), "no recorded digest") ||
			strings.Contains(err.Error(), "does not match the digest")) {
			t.Fatalf("a matching digest must not be the reason for the refusal: %v", err)
		}
	})
}
