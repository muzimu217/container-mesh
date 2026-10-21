// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package wire

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestPreambleRoundTrip checks the four address shapes the design calls out,
// because they are where a naive colon-split breaks.
func TestPreambleRoundTrip(t *testing.T) {
	cases := []Preamble{
		{Version: PreambleVersion, Kind: KindDirect, Addr: "db:5432"},
		{Version: PreambleVersion, Kind: KindDirect, Addr: "127.0.0.1:8080"},
		{Version: PreambleVersion, Kind: KindDirect, Addr: "[::1]:5432"},
		{Version: PreambleVersion, Kind: KindReverse, Addr: "[::1]:9000"},
		{Version: PreambleVersion, Kind: KindReverse, Addr: "127.0.0.1:9000"},
	}
	for _, want := range cases {
		var buf bytes.Buffer
		if err := WritePreamble(&buf, want); err != nil {
			t.Fatalf("write %+v: %v", want, err)
		}
		got, err := ReadPreamble(&buf)
		if err != nil {
			t.Fatalf("read %+v: %v", want, err)
		}
		if got != want {
			t.Fatalf("round trip: got %+v, want %+v", got, want)
		}
		if buf.Len() != 0 {
			t.Fatalf("reader left %d bytes unread", buf.Len())
		}
	}
}

// TestReadPreambleRejects is the rejection table from the design. Every entry is
// something an attacker can send, so a lenient parser is a real bug.
func TestReadPreambleRejects(t *testing.T) {
	// raw builds the wire bytes directly. WritePreamble is deliberately strict,
	// so it cannot be used to synthesise the invalid inputs this test needs;
	// hand-assembling them is also closer to what an attacker actually sends.
	raw := func(version, kind byte, addr string) []byte {
		b := make([]byte, 3+len(addr))
		b[0], b[1], b[2] = version, kind, byte(len(addr))
		copy(b[3:], addr)
		return b
	}
	good := raw(PreambleVersion, KindDirect, "db:5432")

	// helper: patch one byte of the good preamble.
	mutate := func(idx int, v byte) []byte {
		b := append([]byte(nil), good...)
		b[idx] = v
		return b
	}

	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"version 0", mutate(0, 0), "unsupported version"},
		{"version 2", mutate(0, 2), "unsupported version"},
		{"kind 0", mutate(1, 0), "unknown kind"},
		{"kind 3", mutate(1, 3), "unknown kind"},
		{"empty address", mutate(2, 0), "empty address"},
		{"truncated header", good[:2], "reading header"},
		{"truncated address", good[:len(good)-1], "reading address"},
		{"no port", raw(1, KindDirect, "db"), "not host:port"},
		{"empty host", raw(1, KindDirect, ":5432"), "empty host"},
		{"port zero", raw(1, KindDirect, "db:0"), "port 0 out of range"},
		{"port too big", raw(1, KindDirect, "db:65536"), "port 65536 out of range"},
		{"port not a number", raw(1, KindDirect, "db:http"), "not a number"},
		{"nul byte", raw(1, KindDirect, "db:54\x0032"), "control character"},
		{"newline injection", raw(1, KindDirect, "db:5432\nFAKE"), "control character"},
		{"invalid utf8", raw(1, KindDirect, "db:5432\xff"), "not valid UTF-8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ReadPreamble(bytes.NewReader(c.in))
			if err == nil {
				t.Fatalf("accepted invalid preamble %q", c.in)
			}
			if !errors.Is(err, ErrBadPreamble) {
				t.Fatalf("error %v is not ErrBadPreamble", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// TestWritePreambleRejects keeps the writer as strict as the reader: emitting
// something the peer must reject is a bug, not a compatibility feature.
func TestWritePreambleRejects(t *testing.T) {
	cases := []Preamble{
		{Version: 2, Kind: KindDirect, Addr: "db:5432"},
		{Version: 1, Kind: 9, Addr: "db:5432"},
		{Version: 1, Kind: KindDirect, Addr: ""},
		{Version: 1, Kind: KindDirect, Addr: strings.Repeat("a", MaxPreambleAddr+1)},
		{Version: 1, Kind: KindDirect, Addr: "db:0"},
	}
	for _, p := range cases {
		var buf bytes.Buffer
		if err := WritePreamble(&buf, p); err == nil {
			t.Fatalf("wrote invalid preamble %+v", p)
		}
	}
}

// TestStatusRoundTrip covers all six codes and the optional reason.
func TestStatusRoundTrip(t *testing.T) {
	for _, code := range []Status{
		StatusOK, StatusDenied, StatusDialFail,
		StatusTooMany, StatusBadRequest, StatusBindFail,
	} {
		var buf bytes.Buffer
		if err := WriteStatus(&buf, code, "why"); err != nil {
			t.Fatalf("write %v: %v", code, err)
		}
		got, reason, err := ReadStatus(&buf)
		if err != nil {
			t.Fatalf("read %v: %v", code, err)
		}
		if got != code || reason != "why" {
			t.Fatalf("got %v %q, want %v %q", got, reason, code, "why")
		}
	}
}

// TestStatusReasonTruncated proves a long reason is cut rather than rejected:
// a diagnostic string must never fail the handshake.
func TestStatusReasonTruncated(t *testing.T) {
	long := strings.Repeat("x", MaxStatusReason+50)
	var buf bytes.Buffer
	if err := WriteStatus(&buf, StatusDenied, long); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, reason, err := ReadStatus(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(reason) != MaxStatusReason {
		t.Fatalf("reason length %d, want %d", len(reason), MaxStatusReason)
	}
}

// TestStatusReasonTruncatesOnRuneBoundary keeps the truncated string valid
// UTF-8, which matters because the receiver prints it.
func TestStatusReasonTruncatesOnRuneBoundary(t *testing.T) {
	// Multi-byte runes so a naive byte cut would split one.
	long := strings.Repeat("世", MaxStatusReason)
	var buf bytes.Buffer
	if err := WriteStatus(&buf, StatusDenied, long); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, reason, err := ReadStatus(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for i, r := range reason {
		if r == '\uFFFD' {
			t.Fatalf("truncation produced invalid UTF-8 at byte %d", i)
		}
	}
}

// TestReadStatusRejectsUnknownCode guards the decode side.
func TestReadStatusRejectsUnknownCode(t *testing.T) {
	buf := make([]byte, 6)
	buf[3] = 99 // status = 99
	if _, _, err := ReadStatus(bytes.NewReader(buf)); err == nil {
		t.Fatal("accepted unknown status code")
	}
}

// TestValidateForwardListen is the -R spec check. The acceptor applies it before
// binding anything, so every rejection here is one fewer bind attempt an
// attacker can trigger.
func TestValidateForwardListen(t *testing.T) {
	ok := []ForwardListenSpec{
		{ID: "127.0.0.1:9000", Host: "127.0.0.1", Port: 9000},
		{ID: "[::1]:9000", Host: "::1", Port: 9000},
	}
	for _, s := range ok {
		if err := ValidateForwardListen(s); err != nil {
			t.Fatalf("rejected valid spec %+v: %v", s, err)
		}
	}
	bad := []ForwardListenSpec{
		{ID: "", Host: "127.0.0.1", Port: 9000},
		{ID: "127.0.0.1:9000", Host: "127.0.0.1", Port: 0},
		{ID: "127.0.0.1:9000", Host: "127.0.0.1", Port: 70000},
		{ID: "127.0.0.1:9000", Host: "", Port: 9000},
		{ID: "127.0.0.1:9000", Host: "not a host!", Port: 9000},
	}
	for _, s := range bad {
		if err := ValidateForwardListen(s); err == nil {
			t.Fatalf("accepted invalid spec %+v", s)
		}
	}
}

// FuzzReadPreamble is the security test: arbitrary bytes must never panic or
// allocate without bound.
func FuzzReadPreamble(f *testing.F) {
	f.Add([]byte{1, 1, 7, 'd', 'b', ':', '5', '4', '3', '2'})
	f.Add([]byte{0})
	f.Add([]byte{1, 1, 255})
	f.Add([]byte{1, 2, 3, 'a', ':', '1'})
	f.Fuzz(func(t *testing.T, in []byte) {
		// Must not panic. An error is the expected outcome for almost all input.
		_, _ = ReadPreamble(bytes.NewReader(in))
	})
}

// TestReadPreambleBoundedAllocation proves a hostile length byte cannot make us
// allocate more than 255 bytes for the address.
func TestReadPreambleBoundedAllocation(t *testing.T) {
	// Claim 255 bytes but supply none: the read must fail, not block or grow.
	hdr := []byte{1, 1, 255}
	_, err := ReadPreamble(bytes.NewReader(hdr))
	if err == nil {
		t.Fatal("accepted a preamble with a truncated address")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		t.Fatal("error was not wrapped as ErrBadPreamble")
	}
	if !errors.Is(err, ErrBadPreamble) {
		t.Fatalf("error %v is not ErrBadPreamble", err)
	}
}
