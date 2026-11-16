// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"
)

// TestBenchFrameRoundTrip checks that the reusable frame writer and reader
// produce exactly the same wire bytes as the original one-shot helpers, and
// that a reused reader buffer never leaks stale bytes between frames.
func TestBenchFrameRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xA5}, 4096)

	var wire bytes.Buffer
	w := &benchFrameWriter{}
	if err := w.writeData(nopStream{&wire}, benchTagData, payload); err != nil {
		t.Fatalf("writeData: %v", err)
	}
	if err := w.writeControl(nopStream{&wire}, benchTagEnd); err != nil {
		t.Fatalf("writeControl: %v", err)
	}

	// The wire form must be length-prefixed: 4-byte big-endian body length,
	// then the tag byte, then the payload.
	raw := wire.Bytes()
	if len(raw) != 4+1+len(payload)+4+1 {
		t.Fatalf("wire length = %d, want %d", len(raw), 4+1+len(payload)+4+1)
	}
	if got := binary.BigEndian.Uint32(raw[:4]); got != uint32(1+len(payload)) {
		t.Fatalf("data frame length = %d, want %d", got, 1+len(payload))
	}
	if raw[4] != benchTagData {
		t.Fatalf("data tag = %d, want %d", raw[4], benchTagData)
	}
	if !bytes.Equal(raw[5:5+len(payload)], payload) {
		t.Fatal("data payload mismatch")
	}
	if got := binary.BigEndian.Uint32(raw[5+len(payload) : 5+len(payload)+4]); got != 1 {
		t.Fatalf("control frame length = %d, want 1", got)
	}
	if raw[5+len(payload)+4] != benchTagEnd {
		t.Fatalf("control tag = %d, want %d", raw[5+len(payload)+4], benchTagEnd)
	}

	br := bufio.NewReader(bytes.NewReader(raw))
	r := &benchFrameReader{}
	body, err := r.next(br)
	if err != nil {
		t.Fatalf("next data frame: %v", err)
	}
	if body[0] != benchTagData {
		t.Fatalf("data tag = %d, want %d", body[0], benchTagData)
	}
	if !bytes.Equal(body[1:], payload) {
		t.Fatal("data payload mismatch on read")
	}

	body, err = r.next(br)
	if err != nil {
		t.Fatalf("next control frame: %v", err)
	}
	if body[0] != benchTagEnd {
		t.Fatalf("control tag = %d, want %d", body[0], benchTagEnd)
	}
}

// TestBenchFrameReaderNoStaleBytes makes sure that shrinking the next frame
// after a large one does not expose bytes left over from the previous frame.
func TestBenchFrameReaderNoStaleBytes(t *testing.T) {
	big := bytes.Repeat([]byte{0x11}, 2048)

	var wire bytes.Buffer
	w := &benchFrameWriter{}
	if err := w.writeData(nopStream{&wire}, benchTagData, big); err != nil {
		t.Fatal(err)
	}
	if err := w.writeControl(nopStream{&wire}, benchTagEnd); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(bytes.NewReader(wire.Bytes()))
	r := &benchFrameReader{}
	body, err := r.next(br)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 1+len(big) {
		t.Fatalf("first frame length = %d, want %d", len(body), 1+len(big))
	}
	body, err = r.next(br)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 {
		t.Fatalf("second frame length = %d, want 1 (stale bytes leaked)", len(body))
	}
	if body[0] != benchTagEnd {
		t.Fatalf("second frame tag = %d, want %d", body[0], benchTagEnd)
	}
}

// TestBenchFrameReaderRejectsBadLength keeps the original frame validation.
func TestBenchFrameReaderRejectsBadLength(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    uint32
	}{
		{"zero", 0},
		{"oversized", benchFrameMax + 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw bytes.Buffer
			var header [4]byte
			binary.BigEndian.PutUint32(header[:], tc.n)
			raw.Write(header[:])
			r := &benchFrameReader{}
			if _, err := r.next(bufio.NewReader(&raw)); err == nil {
				t.Fatalf("next with length %d: want error", tc.n)
			}
		})
	}
}

// nopStream adapts an io.Writer to the *quic.Stream the frame helpers take.
type nopStream struct{ w io.Writer }

func (s nopStream) Write(p []byte) (int, error) { return s.w.Write(p) }

func (s nopStream) Read(p []byte) (int, error) { return 0, io.EOF }

func (s nopStream) Close() error                     { return nil }
func (s nopStream) CancelRead(error)                 {}
func (s nopStream) CancelWrite(error)                {}
func (s nopStream) SetDeadline(time.Time) error      { return nil }
func (s nopStream) SetReadDeadline(time.Time) error  { return nil }
func (s nopStream) SetWriteDeadline(time.Time) error { return nil }
