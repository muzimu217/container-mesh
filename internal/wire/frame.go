// Package wire defines the binary framing used between two container-mesh peers.
//
// Every frame travels inside an encrypted envelope (see package secure) and is
// carried by the relay as an opaque binary WebSocket message. The relay never
// parses these bytes.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Version is the on-the-wire protocol version. Bump on any incompatible change.
const Version = 1

// MaxPayload is the largest payload a single frame may carry.
const MaxPayload = 1 << 20 // 1 MiB

// FrameType identifies what a frame means.
type FrameType uint8

const (
	// FrameHello is exchanged right after the encrypted channel is up. It
	// carries the protocol version and a human readable peer name.
	FrameHello FrameType = iota + 1
	// FrameMsg is a small user message. Payload is raw UTF-8 text.
	FrameMsg
	// FrameFileOffer announces a file: name, size, sha256.
	FrameFileOffer
	// FrameFileAccept accepts an offer and states where to resume from.
	FrameFileAccept
	// FrameFileReject rejects an offer with a reason string.
	FrameFileReject
	// FrameFileChunk carries file bytes: offset(8) || data.
	FrameFileChunk
	// FrameFileAck reports durable progress: next-offset(8).
	//
	// Reserved. The current transport is TCP-backed WebSocket, which already
	// guarantees ordered, reliable delivery, so per-chunk acknowledgement would
	// add round trips for nothing. The type is defined so a future UDP/QUIC
	// data plane can negotiate real backpressure without a version bump.
	FrameFileAck
	// FrameFileDone tells the receiver all bytes were sent: sha256(32).
	FrameFileDone
	// FrameFileResult reports the receiver's verification result.
	FrameFileResult
	// FrameError reports a protocol level error.
	FrameError
	// FramePing is a keepalive.
	FramePing
	// FramePong answers FramePing.
	FramePong
)

// String makes logs readable.
func (t FrameType) String() string {
	switch t {
	case FrameHello:
		return "hello"
	case FrameMsg:
		return "msg"
	case FrameFileOffer:
		return "file-offer"
	case FrameFileAccept:
		return "file-accept"
	case FrameFileReject:
		return "file-reject"
	case FrameFileChunk:
		return "file-chunk"
	case FrameFileAck:
		return "file-ack"
	case FrameFileDone:
		return "file-done"
	case FrameFileResult:
		return "file-result"
	case FrameError:
		return "error"
	case FramePing:
		return "ping"
	case FramePong:
		return "pong"
	default:
		return fmt.Sprintf("frame(%d)", uint8(t))
	}
}

// HeaderSize is the fixed size of a frame header.
const HeaderSize = 10 // type(1) + flags(1) + stream(4) + length(4)

// Frame is one protocol message.
type Frame struct {
	Type    FrameType
	Flags   uint8
	Stream  uint32
	Payload []byte
}

// ErrFrameTooLarge is returned when a frame claims a payload above MaxPayload.
var ErrFrameTooLarge = errors.New("wire: frame payload too large")

// Encode appends the binary representation of f to dst.
func (f *Frame) Encode(dst []byte) ([]byte, error) {
	if len(f.Payload) > MaxPayload {
		return dst, ErrFrameTooLarge
	}
	var hdr [HeaderSize]byte
	hdr[0] = byte(f.Type)
	hdr[1] = f.Flags
	binary.BigEndian.PutUint32(hdr[2:6], f.Stream)
	binary.BigEndian.PutUint32(hdr[6:10], uint32(len(f.Payload)))
	dst = append(dst, hdr[:]...)
	dst = append(dst, f.Payload...)
	return dst, nil
}

// DecodeFrame reads exactly one frame from r.
func DecodeFrame(r io.Reader) (*Frame, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[6:10])
	if n > MaxPayload {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, n)
	}
	f := &Frame{
		Type:   FrameType(hdr[0]),
		Flags:  hdr[1],
		Stream: binary.BigEndian.Uint32(hdr[2:6]),
	}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// DecodeFrameBytes decodes a frame from a complete byte slice.
func DecodeFrameBytes(b []byte) (*Frame, error) {
	if len(b) < HeaderSize {
		return nil, fmt.Errorf("wire: short frame: %d bytes", len(b))
	}
	n := binary.BigEndian.Uint32(b[6:10])
	// Check the declared size against the limit before comparing it to the
	// actual buffer length, so an attacker-supplied header is rejected on the
	// cheap test first.
	if n > MaxPayload {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, n)
	}
	if int(n) != len(b)-HeaderSize {
		return nil, fmt.Errorf("wire: length mismatch: header %d, body %d", n, len(b)-HeaderSize)
	}
	f := &Frame{
		Type:    FrameType(b[0]),
		Flags:   b[1],
		Stream:  binary.BigEndian.Uint32(b[2:6]),
		Payload: b[HeaderSize:],
	}
	return f, nil
}

// ---- Payload helpers -------------------------------------------------------

// ChunkHeaderSize is the offset prefix of a FrameFileChunk payload.
const ChunkHeaderSize = 8

// EncodeChunk builds a file-chunk payload.
func EncodeChunk(offset int64, data []byte) []byte {
	out := make([]byte, ChunkHeaderSize+len(data))
	binary.BigEndian.PutUint64(out[:ChunkHeaderSize], uint64(offset))
	copy(out[ChunkHeaderSize:], data)
	return out
}

// DecodeChunk splits a file-chunk payload.
func DecodeChunk(p []byte) (offset int64, data []byte, err error) {
	if len(p) < ChunkHeaderSize {
		return 0, nil, errors.New("wire: short chunk payload")
	}
	off := binary.BigEndian.Uint64(p[:ChunkHeaderSize])
	if off > 1<<62 {
		return 0, nil, errors.New("wire: chunk offset out of range")
	}
	return int64(off), p[ChunkHeaderSize:], nil
}

// EncodeUint64 is a convenience for single-value payloads.
func EncodeUint64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// DecodeUint64 is the inverse of EncodeUint64.
func DecodeUint64(p []byte) (uint64, error) {
	if len(p) != 8 {
		return 0, fmt.Errorf("wire: expected 8 byte payload, got %d", len(p))
	}
	return binary.BigEndian.Uint64(p), nil
}

// FileOffer is the decoded FrameFileOffer payload.
type FileOffer struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// FileAccept is the decoded FrameFileAccept payload.
type FileAccept struct {
	ResumeFrom int64 `json:"resume_from"`
}

// FileResult is the decoded FrameFileResult payload.
type FileResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}
