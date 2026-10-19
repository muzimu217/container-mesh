// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package secure

import (
	"strings"
	"testing"
)

// TestGenerateCodeEntropy checks the generated code is long enough that the
// derived room label is not worth attacking offline, and that it survives
// normalization unchanged.
func TestGenerateCodeEntropy(t *testing.T) {
	code, err := GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	norm := NormalizeCode(code)
	if len(norm) < 26 {
		t.Errorf("normalized length = %d, want >= 26 characters", len(norm))
	}
	if norm != strings.ToLower(norm) {
		t.Errorf("code is not lowercase: %q", norm)
	}
	// Normalization must be idempotent, or the two sides would derive
	// different room labels.
	if NormalizeCode(code) != NormalizeCode(norm) {
		t.Error("normalization is not idempotent")
	}
}

// TestGenerateCodeIsUnique: two calls must not collide, which would put
// unrelated sessions in the same room.
func TestGenerateCodeIsUnique(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		c, err := GenerateCode()
		if err != nil {
			t.Fatal(err)
		}
		if seen[c] {
			t.Fatalf("duplicate code generated: %q", c)
		}
		seen[c] = true
	}
}

// TestGenerateCodeAvoidsAmbiguousCharacters keeps the code transcribable: a
// user reading it aloud must not confuse 0/O or 1/l/I.
func TestGenerateCodeAvoidsAmbiguousCharacters(t *testing.T) {
	for i := 0; i < 32; i++ {
		c, err := GenerateCode()
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"0", "o", "1", "l", "i"} {
			if strings.Contains(c, bad) {
				t.Fatalf("code %q contains ambiguous character %q", c, bad)
			}
		}
	}
}

// TestGeneratedCodeProducesUsableRoom: the generated code must work with the
// room derivation, and different codes must not share a room.
func TestGeneratedCodeProducesUsableRoom(t *testing.T) {
	a, err := GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	if RoomID(a) == RoomID(b) {
		t.Error("two generated codes produced the same room")
	}
	if len(RoomID(a)) != 32 {
		t.Errorf("room id length = %d, want 32", len(RoomID(a)))
	}
}
