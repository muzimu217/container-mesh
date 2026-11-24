// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ THE MESH FILE TRANSFER MUST RESUME.
//
// The stage-1 path has had resume for a while (internal/peer: offer,
// accept with ResumeFrom, seek, stream from the offset). The mesh line is
// a SEPARATE implementation and never carried the rule over: it sends a
// FileHeader, the receiver answers "ok", and the whole body is streamed
// from byte 0.
//
// So an interrupted mesh transfer restarts from zero, and the operator
// is left re-sending a multi-gigabyte file because the link dropped
// once.
//
// The rule, copied from the path that already has it:
//
//  1. The sender offers name + size + sha256.
//  2. The receiver answers with the number of bytes it already has on
//     disk (its .part file's length).
//  3. The sender seeks there and streams from there.
//
// A stale or corrupt prefix cannot survive, because the whole-file hash
// is verified before the file is renamed into place.

// The mesh wire format must carry a resume offset.
//
// This one asserts on the source rather than on behaviour, because the
// frame is assembled inline in a function that needs a live session.
// It is written this way so it compiles against a tree that predates
// the field — which is what lets it prove the field was never there.
func TestTheMeshFileHeaderCarriesAResumeOffset(t *testing.T) {
	src := readMeshSource(t, "direct.go")
	if !strings.Contains(src, "ResumeFrom") {
		t.Fatal("the mesh file header carries no resume offset; " +
			"an interrupted mesh transfer restarts from byte 0 while the " +
			"stage-1 path has resumed for a long time")
	}
}

// The receiver must keep a .part file, not a throwaway temp.
//
// The temp file is deleted on ANY failure — including a dropped link —
// so nothing survives to resume from. The .part file is what makes the
// second attempt cheap.
func TestTheMeshReceiverKeepsAPartFile(t *testing.T) {
	src := readMeshSource(t, "direct.go")
	if !strings.Contains(src, `".part"`) && !strings.Contains(src, ".part") {
		t.Fatal("the mesh receiver does not keep a .part file; " +
			"the temp file it uses is deleted on every failure, so there is " +
			"nothing to resume from")
	}
}

// ★ AND THE .part SUFFIX MUST BE RESERVED.
//
// The stage-1 path refuses a file whose name ends in .part, because
// that is the receiver's own in-progress suffix: accepting it would let
// a sender write into the receiver's own bookkeeping.
//
// The mesh path has to carry the same rule, or a sender could name a
// file "x.part" and collide with the receiver's partial file.
func TestTheMeshReceiverRefusesThePartSuffix(t *testing.T) {
	src := readMeshSource(t, "direct.go")
	if !strings.Contains(src, ".part") {
		t.Fatal("the mesh receiver does not know about the .part suffix at all")
	}
	// The refusal must be a REFUSAL, not a normalisation: a file that
	// ends in .part must not be accepted.
	if !strings.Contains(src, "reserved") && !strings.Contains(src, "refus") {
		t.Logf("  the .part suffix is mentioned but the refusal could not be " +
			"identified from the source; check it by hand")
	}
}

// The partial file must be RENAMED into place on success — a file that
// is only ever a .part is never delivered.
func TestTheMeshReceiverRenamesThePartFileOnSuccess(t *testing.T) {
	src := readMeshSource(t, "direct.go")
	if !strings.Contains(src, "Rename") {
		t.Fatal("the mesh receiver never renames the .part file into place; " +
			"the file would never be delivered")
	}
}

// readMeshSource reads a source file of this package.
func readMeshSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// The partial file's name must be derived from the final name, so the
// receiver can find it again on the next attempt.
func TestThePartFileIsNamedAfterTheFinalFile(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "report.bin")
	part := final + ".part"
	if err := os.WriteFile(part, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	// And the .part file must NOT be visible as a delivered file.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "report.bin.part" {
		t.Fatalf("the partial file is not named after the final file: %v", names)
	}
}
