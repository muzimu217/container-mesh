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
