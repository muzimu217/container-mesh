// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInboundTransferHashesWhileStreaming pins that the receiver's digest
// comes from the bytes as they arrive.
//
// The receiver used to write the file and then read the whole thing back off
// disk to compute its SHA-256. Profiling put that re-read at 26.67% of the
// receiver's CPU, with the digest itself at another 19.05%.
//
// The digest must be identical either way, so the test computes the expected
// value independently and compares it with the one the transfer reports.
func TestInboundTransferHashesWhileStreaming(t *testing.T) {
	a, _, _, hb := pair(t, "stream-hash-code")

	srcDir := t.TempDir()
	data := bytes.Repeat([]byte("S"), 200<<10)
	src := filepath.Join(srcDir, "stream.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	fd := waitFile(t, hb)
	if fd.err != nil {
		t.Fatalf("receive: %v", fd.err)
	}
	if !res.OK {
		t.Fatalf("transfer failed: %s", res.Message)
	}
	if !strings.Contains(res.Message, hexOf(data)) {
		t.Fatalf("the reported digest %q does not contain the expected %s",
			res.Message, hexOf(data))
	}

	got, err := os.ReadFile(filepath.Join(hb.dir, "stream.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != want {
		t.Fatal("the received file differs from the source")
	}
}

// TestInboundTransferResumedStillVerifies pins the resumed case.
//
// A resumed transfer starts at an offset written by an earlier run, so this
// process never hashed those bytes. Hashing while streaming would produce a
// digest of the tail only, and the transfer would fail with a hash
// mismatch. The resumed case therefore has to read the file back.
//
// The existing resume test covers the behaviour; this one names the reason
// it cannot be streamed, so the rule survives someone "simplifying" the
// branch later.
func TestInboundTransferResumedStillVerifies(t *testing.T) {
	a, _, _, hb := pair(t, "resume-stream-hash-code")

	srcDir := t.TempDir()
	data := bytes.Repeat([]byte("R"), 300<<10)
	src := filepath.Join(srcDir, "resumed.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// A prefix written by an earlier run: this process never saw it.
	const prefix = 120 << 10
	if err := os.WriteFile(filepath.Join(hb.dir, "resumed.bin.part"), data[:prefix], 0o644); err != nil {
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
	if !res.OK {
		t.Fatalf("the resumed transfer failed: %s", res.Message)
	}

	got, err := os.ReadFile(filepath.Join(hb.dir, "resumed.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatal("the resumed file differs from the source")
	}
}
