// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/muzimu217/container-mesh/internal/wire"
)

// emptySHA256 is the digest of the empty input; a zero-byte transfer must
// produce exactly this.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// TestZeroByteFileArrives is the regression test for a bug found by external
// review: empty files could never be received.
//
// Root cause was conflating "how many bytes do we have" with "where are they":
// for an empty file have == offer.Size == 0 from the start, so the receiver
// took the "already complete" branch and tried to open an empty path.
func TestZeroByteFileArrives(t *testing.T) {
	a, _, _, hb := pair(t, "zero-byte-code")

	src := filepath.Join(t.TempDir(), "empty.bin")
	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	fd := waitFile(t, hb)
	if fd.err != nil {
		t.Fatalf("receive: %v", fd.err)
	}
	if !res.OK || !fd.res.OK {
		t.Fatalf("zero-byte transfer failed: %s / %s", res.Message, fd.res.Message)
	}
	if res.Message != "hash verified: "+emptySHA256 {
		t.Errorf("unexpected digest: %s", res.Message)
	}

	dst := filepath.Join(hb.dir, "empty.bin")
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("size = %d, want 0", fi.Size())
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Errorf("leftover .part after zero-byte transfer: %v", err)
	}
}

// TestZeroByteOverExistingEmptyFile: if the destination already holds a
// zero-byte file, the receiver must verify it in place rather than truncating
// or rewriting it.
func TestZeroByteOverExistingEmptyFile(t *testing.T) {
	a, _, _, hb := pair(t, "zero-existing-code")

	dst := filepath.Join(hb.dir, "already.bin")
	if err := os.WriteFile(dst, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(t.TempDir(), "already.bin")
	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	fd := waitFile(t, hb)
	if fd.err != nil {
		t.Fatalf("receive: %v", fd.err)
	}
	if !res.OK || !fd.res.OK {
		t.Fatalf("transfer failed: %s / %s", res.Message, fd.res.Message)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Size() != 0 {
		t.Fatalf("existing empty file damaged: %v", err)
	}
}

// TestShortFileStillArrives is the control case: the fix must not break
// ordinary small transfers.
func TestShortFileStillArrives(t *testing.T) {
	a, _, _, hb := pair(t, "short-file-code")

	data := make([]byte, 3)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "tiny.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	fd := waitFile(t, hb)
	if !res.OK || fd.err != nil || !fd.res.OK {
		t.Fatalf("short transfer failed: %s / %v", res.Message, fd.err)
	}

	got, err := os.ReadFile(filepath.Join(hb.dir, "tiny.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Error("content mismatch")
	}
}

// TestZeroByteResumeFromEmptyPart: a leftover zero-byte .part must be treated
// as "nothing yet", not as a complete file.
func TestZeroByteResumeFromEmptyPart(t *testing.T) {
	a, _, _, hb := pair(t, "zero-part-code")

	src := filepath.Join(t.TempDir(), "resumed.bin")
	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-create an empty .part, as an interrupted transfer would.
	if err := os.WriteFile(filepath.Join(hb.dir, "resumed.bin.part"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	fd := waitFile(t, hb)
	if !res.OK || fd.err != nil || !fd.res.OK {
		t.Fatalf("transfer failed: %s / %v", res.Message, fd.err)
	}
	if _, err := os.Stat(filepath.Join(hb.dir, "resumed.bin")); err != nil {
		t.Fatalf("final file missing: %v", err)
	}
}

// TestHiddenFileNameRejected documents the deliberate policy that dotfiles are
// refused (protects the receiver's working directory from .bashrc-style
// payloads). The README states this; the test keeps it from changing silently.
func TestHiddenFileNameRejected(t *testing.T) {
	if err := validateName(".env"); err == nil {
		t.Fatal("dotfile should be rejected")
	}
	if err := validateName("normal.txt"); err != nil {
		t.Fatalf("ordinary name rejected: %v", err)
	}
}

var _ = hex.EncodeToString
var _ = wire.FileResult{}
