// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package secure

import (
	"bytes"
	"testing"

	"github.com/blackevil/container-mesh/internal/wire"
)

// runHandshake performs a full in-memory PAKE exchange between two roles.
func runHandshake(t *testing.T, code string) (a, b *Handshake) {
	t.Helper()
	ha, err := NewHandshake(code, RoleA)
	if err != nil {
		t.Fatalf("A init: %v", err)
	}
	hb, err := NewHandshake(code, RoleB)
	if err != nil {
		t.Fatalf("B init: %v", err)
	}

	m1, done, err := ha.Step(nil)
	if err != nil || done {
		t.Fatalf("A step1: out=%v done=%v err=%v", m1 != nil, done, err)
	}
	m2, done, err := hb.Step(m1)
	if err != nil || done {
		t.Fatalf("B step1: done=%v err=%v", done, err)
	}
	if _, done, err := ha.Step(m2); err != nil || !done {
		t.Fatalf("A step2: done=%v err=%v", done, err)
	}
	return ha, hb
}

func TestNormalizeCode(t *testing.T) {
	cases := map[string]string{
		"Abc-Def":      "abc-def",
		"  ABC DEF  ":  "abc-def",
		"abc_def":      "abc-def",
		"abc--def":     "abc-def",
		"--abc--def--": "abc-def",
		"ABC123":       "abc123",
		"a b c":        "a-b-c",
	}
	for in, want := range cases {
		if got := NormalizeCode(in); got != want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandshakeDerivesSameKeys(t *testing.T) {
	ha, hb := runHandshake(t, "shared-secret-code")

	ka, err := ha.SessionKey()
	if err != nil {
		t.Fatalf("A key: %v", err)
	}
	kb, err := hb.SessionKey()
	if err != nil {
		t.Fatalf("B key: %v", err)
	}
	if !bytes.Equal(ka, kb) {
		t.Fatal("session keys differ")
	}

	sendA, recvA, err := DeriveKeys(ka, RoleA)
	if err != nil {
		t.Fatal(err)
	}
	sendB, recvB, err := DeriveKeys(kb, RoleB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sendA, recvB) || !bytes.Equal(recvA, sendB) {
		t.Fatal("directional keys do not line up")
	}
	if bytes.Equal(sendA, recvA) {
		t.Fatal("send and recv keys must differ")
	}
}

func TestDifferentCodesProduceDifferentKeys(t *testing.T) {
	ha, _ := runHandshake(t, "code-number-one")
	hb, _ := runHandshake(t, "code-number-two")

	ka, err := ha.SessionKey()
	if err != nil {
		t.Fatal(err)
	}
	kb, err := hb.SessionKey()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ka, kb) {
		t.Fatal("different codes produced the same key")
	}
}

func TestSessionSealOpen(t *testing.T) {
	ha, hb := runHandshake(t, "seal-open-code")
	sa, err := NewSession(ha)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := NewSession(hb)
	if err != nil {
		t.Fatal(err)
	}

	f := &wire.Frame{Type: wire.FrameMsg, Payload: []byte("hello world")}
	sealed, err := sa.SealFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sb.OpenFrame(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(got.Payload) != "hello world" {
		t.Errorf("payload = %q", got.Payload)
	}

	// The reverse direction must work too.
	sealed, err = sb.SealFrame(&wire.Frame{Type: wire.FrameMsg, Payload: []byte("back")})
	if err != nil {
		t.Fatal(err)
	}
	got, err = sa.OpenFrame(sealed)
	if err != nil {
		t.Fatalf("reverse open: %v", err)
	}
	if string(got.Payload) != "back" {
		t.Errorf("reverse payload = %q", got.Payload)
	}
}

func TestSessionRejectsReplay(t *testing.T) {
	ha, hb := runHandshake(t, "replay-code-here")
	sa, err := NewSession(ha)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := NewSession(hb)
	if err != nil {
		t.Fatal(err)
	}

	sealed, err := sa.SealFrame(&wire.Frame{Type: wire.FrameMsg, Payload: []byte("once")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.OpenFrame(sealed); err != nil {
		t.Fatalf("first open: %v", err)
	}
	// Replaying the same ciphertext must fail because the counter advanced.
	if _, err := sb.OpenFrame(sealed); err == nil {
		t.Fatal("replay should be rejected")
	}
}

func TestSessionRejectsTamperedCiphertext(t *testing.T) {
	ha, hb := runHandshake(t, "tamper-code-here")
	sa, err := NewSession(ha)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := NewSession(hb)
	if err != nil {
		t.Fatal(err)
	}

	sealed, err := sa.SealFrame(&wire.Frame{Type: wire.FrameMsg, Payload: []byte("data")})
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 0xFF
	if _, err := sb.OpenFrame(sealed); err == nil {
		t.Fatal("tampered ciphertext should be rejected")
	}
}

func TestHandshakeRejectsShortCode(t *testing.T) {
	if _, err := NewHandshake("abc", RoleA); err == nil {
		t.Fatal("short code should be rejected")
	}
	if _, err := NewHandshake("   ---   ", RoleA); err == nil {
		t.Fatal("code that normalizes to nothing should be rejected")
	}
}

func TestHandshakeRejectsSameRole(t *testing.T) {
	ha, err := NewHandshake("same-role-code", RoleA)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewHandshake("same-role-code", RoleA)
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := ha.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.Step(m); err == nil {
		t.Fatal("two role-A peers must not complete a handshake")
	}
}

func TestSessionCounters(t *testing.T) {
	ha, hb := runHandshake(t, "counter-code-xx")
	sa, err := NewSession(ha)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := NewSession(hb)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		sealed, err := sa.SealFrame(&wire.Frame{Type: wire.FramePing})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sb.OpenFrame(sealed); err != nil {
			t.Fatal(err)
		}
	}
	if sa.SendCounter() != 3 {
		t.Errorf("send counter = %d", sa.SendCounter())
	}
	if sb.RecvCounter() != 3 {
		t.Errorf("recv counter = %d", sb.RecvCounter())
	}
}
