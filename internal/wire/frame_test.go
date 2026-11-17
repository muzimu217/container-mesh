// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package wire

import (
	"bytes"
	"errors"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []*Frame{
		{Type: FrameMsg, Payload: []byte("hello")},
		{Type: FrameFileChunk, Stream: 7, Flags: 3, Payload: bytes.Repeat([]byte{0xAB}, 4096)},
		{Type: FramePing},
	}
	for _, want := range cases {
		enc, err := want.Encode(nil)
		if err != nil {
			t.Fatalf("%s: encode: %v", want.Type, err)
		}
		got, err := DecodeFrameBytes(enc)
		if err != nil {
			t.Fatalf("%s: decode: %v", want.Type, err)
		}
		if got.Type != want.Type || got.Stream != want.Stream || got.Flags != want.Flags {
			t.Errorf("%s: header mismatch: %+v", want.Type, got)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("%s: payload mismatch", want.Type)
		}
	}
}

func TestDecodeRejectsOversize(t *testing.T) {
	// Craft a header claiming a payload above MaxPayload.
	hdr := make([]byte, HeaderSize)
	hdr[0] = byte(FrameMsg)
	hdr[6], hdr[7], hdr[8], hdr[9] = 0x00, 0x20, 0x00, 0x00 // 2 MiB
	if _, err := DecodeFrameBytes(hdr); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestDecodeRejectsShortFrame(t *testing.T) {
	if _, err := DecodeFrameBytes([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error for short frame")
	}
}

func TestDecodeRejectsLengthMismatch(t *testing.T) {
	f := &Frame{Type: FrameMsg, Payload: []byte("abcdef")}
	enc, err := f.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeFrameBytes(enc[:len(enc)-1]); err == nil {
		t.Fatal("expected error for truncated frame")
	}
}

func TestChunkRoundTrip(t *testing.T) {
	off, data, err := DecodeChunk(EncodeChunk(1234, []byte("payload")))
	if err != nil {
		t.Fatal(err)
	}
	if off != 1234 {
		t.Errorf("offset = %d", off)
	}
	if string(data) != "payload" {
		t.Errorf("data = %q", data)
	}
}

func TestDecodeChunkRejectsShort(t *testing.T) {
	if _, _, err := DecodeChunk([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error")
	}
}

func TestUint64RoundTrip(t *testing.T) {
	v, err := DecodeUint64(EncodeUint64(1 << 40))
	if err != nil {
		t.Fatal(err)
	}
	if v != 1<<40 {
		t.Errorf("got %d", v)
	}
	if _, err := DecodeUint64([]byte{1}); err == nil {
		t.Fatal("expected error for short payload")
	}
}

func TestFrameTypeString(t *testing.T) {
	for _, tt := range []FrameType{FrameHello, FrameMsg, FrameFileChunk, FrameFileResult} {
		if tt.String() == "" {
			t.Errorf("%d has no string", tt)
		}
	}
	if FrameType(200).String() == "" {
		t.Error("unknown type should still render")
	}
}

// TestExistingFrameNumbersAreStable pins the wire numbers of the frames
// that already existed.
//
// The relay trunk frames are APPENDED, and appending is only safe while
// the older numbers stay where they are: the type byte is what a peer
// dispatches on, so renumbering one of these turns a "hello" into a
// "file-offer" for every peer built against an earlier release.
func TestExistingFrameNumbersAreStable(t *testing.T) {
	for _, tt := range []struct {
		got  FrameType
		want uint8
	}{
		{FrameHello, 1},
		{FrameMsg, 2},
		{FrameFileOffer, 3},
		{FrameFileAccept, 4},
		{FrameFileReject, 5},
		{FrameFileChunk, 6},
		{FrameFileAck, 7},
		{FrameFileDone, 8},
		{FrameFileResult, 9},
		{FrameError, 10},
		{FramePing, 11},
		{FramePong, 12},
		{FrameP2POffer, 13},
		{FrameP2PAnswer, 14},
		{FrameMuxStart, 15},
		{FrameStreamData, 16},
		{FrameForwardListen, 17},
		{FrameForwardListenResult, 18},
	} {
		if uint8(tt.got) != tt.want {
			t.Errorf("%s = %d, want %d: an existing frame number moved", tt.got, uint8(tt.got), tt.want)
		}
	}
}

// TestMeshRelayFrameTypes pins the two relay trunk frames.
//
// They are the control frames of the mesh's own relay (the trunk): one
// asks the relay member to open a session on somebody's behalf, the other
// answers. Both need a readable name — the frame type is what shows up in
// a log line when a trunk request goes wrong — and both must be distinct
// from every other frame, because the dispatch is a byte compare.
func TestMeshRelayFrameTypes(t *testing.T) {
	if got := FrameMeshRelayOpen.String(); got != "mesh-relay-open" {
		t.Errorf("FrameMeshRelayOpen.String() = %q, want %q", got, "mesh-relay-open")
	}
	if got := FrameMeshRelayAnswer.String(); got != "mesh-relay-answer" {
		t.Errorf("FrameMeshRelayAnswer.String() = %q, want %q", got, "mesh-relay-answer")
	}

	all := []FrameType{
		FrameHello, FrameMsg, FrameFileOffer, FrameFileAccept, FrameFileReject,
		FrameFileChunk, FrameFileAck, FrameFileDone, FrameFileResult, FrameError,
		FramePing, FramePong, FrameP2POffer, FrameP2PAnswer, FrameMuxStart,
		FrameStreamData, FrameForwardListen, FrameForwardListenResult,
		FrameMeshRelayOpen, FrameMeshRelayAnswer,
	}
	seen := make(map[FrameType]bool, len(all))
	for _, f := range all {
		if seen[f] {
			t.Errorf("frame type %d (%s) is used twice", uint8(f), f)
		}
		seen[f] = true
	}
}

// TestMeshRelayFramesRoundTrip checks that the two new frames survive the
// binary encoding like every other frame: the trunk control messages ride
// the same header as the rest of the protocol.
func TestMeshRelayFramesRoundTrip(t *testing.T) {
	for _, want := range []*Frame{
		{Type: FrameMeshRelayOpen, Stream: 3, Payload: []byte(`{"from":"a","to":"b"}`)},
		{Type: FrameMeshRelayAnswer, Payload: []byte(`{"grant":false,"reason":"nope"}`)},
	} {
		enc, err := want.Encode(nil)
		if err != nil {
			t.Fatalf("%s: encode: %v", want.Type, err)
		}
		got, err := DecodeFrameBytes(enc)
		if err != nil {
			t.Fatalf("%s: decode: %v", want.Type, err)
		}
		if got.Type != want.Type || got.Stream != want.Stream {
			t.Errorf("%s: header mismatch: %+v", want.Type, got)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("%s: payload mismatch", want.Type)
		}
	}
}
