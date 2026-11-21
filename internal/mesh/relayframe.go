// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// ---------------------------------------------------------------------------
// The BINARY relay record.
//
// WHY THIS EXISTS.
//
// The relay trunk's data plane carries its traffic in JSON envelopes
// (relayRecord, in relaytrunk.go). A JSON envelope encodes a []byte as
// base64, so four bytes of wire carry three bytes of payload, and both
// ends of the trunk PAY FOR THE JSON: the anchor decodes every record it
// forwards (json.Unmarshal + base64 decode) and re-encodes it (json.Marshal
// + base64 encode) before handing it on.
//
// The anchor is the one node in the path that has no business reading the
// payload — the payload is the end-to-end ciphertext and the anchor holds
// neither end's key. Yet the JSON envelope forces it to parse and rebuild
// the payload on every single record. The measured cost on a 32 MiB
// transfer through the anchor was 34.89% of the anchor's CPU in encoding
// alone (json.Unmarshal 23.98% of which base64 9.74%, json.Marshal 10.91%
// of which base64 9.08%).
//
// The fix is a format whose fixed-length header says everything the trunk
// needs (the frame type and the payload length) so that the trunk can
// forward the bytes VERBATIM: read 24 bytes, splice the rest. The
// forwarding path then never touches the payload at all.
//
// THIS FILE IS DEAD CODE ON PURPOSE.
//
// It defines the format and its codec, and it is exercised by its own
// tests. No production call site uses it yet: writeRelayRecord /
// readRelayRecord still speak JSON. The switch is a SEPARATE step, and
// this file exists to make that step a mechanical change instead of a
// design change — and to give the second step a baseline to compare
// against. See docs/design/RELAY-FRAME.md.
//
// THE WIRE SHAPE DOES NOT CHANGE.
//
// A relay record is still carried inside the mesh's length-prefixed frame
// (writeFrame / readFrame: a 4-byte big-endian length followed by the
// body). Only the BODY changes: JSON becomes the binary frame below. So
// the framing layer, the QUIC stream layer and the relay ALPN are all
// untouched by the switch.
//
// ---------------------------------------------------------------------------
// THE LAYOUT.
//
// All integers are big-endian. All lengths are byte counts.
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|    magic      |    version    |     type      |     flags     |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                                                               |
//	+                     session (uint64)                         +
//	|                                                               |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                           hops (uint32)                       |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                          metaLen (uint32)                     |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                        payloadLen (uint32)                   |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                     meta block (metaLen bytes)                |
//	|                     ...                                        |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                     payload (payloadLen bytes)                |
//	|                     ...                                        |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//
// THE FIXED HEADER IS 24 BYTES AND IS ALWAYS PRESENT.
//
//	Type and PayloadLen live in it, so a reader can answer "what is this
//	and how big is the payload" from a single 24-byte read, without
// touching the payload. That is what makes the verbatim forward possible.
//
// THE META BLOCK holds the fields that only the CONTROL records use, each
// one length-prefixed and each one present only when the matching flag
// bit is set:
//
//	kind   : uint16 length + bytes
//	reason : uint16 length + bytes
//	class  : uint16 length + bytes
//	from   : uint16 length + bytes   (the DeviceID, as its own characters)
//	to     : uint16 length + bytes   (the DeviceID, as its own characters)
//	hello  : uint32 length + bytes
//
// The order is fixed, and the flags say which of them are there. A DATA
// record — the hot one, the one that carries the bulk of the bytes — has
// NO meta fields at all, so its meta block is empty and the whole record
// is 24 bytes of header plus the payload.
//
// THE FLAGS BYTE says which optional values are present:
//
//	bit 0 (0x01) Close
//	bit 1 (0x02) Grant
//	bit 2 (0x04) Kind
//	bit 3 (0x08) Reason
//	bit 4 (0x10) Class
//	bit 5 (0x20) From
//	bit 6 (0x40) To
//	bit 7 (0x80) Hello
//
// The presence rule is the same as the JSON's omitempty: a zero value is
// ABSENT, not present-and-empty. So the binary frame and the JSON
// envelope agree on what a zero value means, and a record round-trips
// through either one to the same value.
//
// Hops lives in the fixed header as a uint32, not in the meta block,
// because it is a small number that the control path reads on every open
// record. Zero means "no hop budget stated", which is what the JSON's
// omitempty means by zero.
//
// ---------------------------------------------------------------------------

const (
	// relayFrameMagic is the FORMAT TAG: the first byte of every binary
	// relay record.
	//
	// It is 0xC7 on purpose. A JSON document starts with one of a
	// handful of characters — '{', '[', '"', 't', 'f', 'n', '-', a digit
	// or a space — and 0xC7 is none of them. It is also not a legal
	// UTF-8 lead byte, so no JSON parser, in any language, can mistake
	// it for the start of a JSON value.
	//
	// That matters because the format has to be able to meet an OLD
	// parser. A new anchor that speaks the binary format to an old
	// member (or the other way round) must produce a LOUD failure, not
	// a plausible misreading: the old side must say "invalid character"
	// and give up, never decode a binary record as a JSON record with
	// the wrong fields.
	relayFrameMagic = 0xC7

	// relayFrameVersion is the format version.
	//
	// It is a byte of its own so that a future incompatible change can
	// be DETECTED (and refused with a clear message) instead of being
	// mis-read as version 1.
	relayFrameVersion = 1

	// relayFrameHeaderSize is the size of the fixed header: the bytes
	// that are always there, in the same order, for every record.
	relayFrameHeaderSize = 24
)

// The flag bits of the flags byte.
const (
	relayFrameFlagClose  uint8 = 1 << 0
	relayFrameFlagGrant  uint8 = 1 << 1
	relayFrameFlagKind   uint8 = 1 << 2
	relayFrameFlagReason uint8 = 1 << 3
	relayFrameFlagClass  uint8 = 1 << 4
	relayFrameFlagFrom   uint8 = 1 << 5
	relayFrameFlagTo     uint8 = 1 << 6
	relayFrameFlagHello  uint8 = 1 << 7
)

// The byte offsets of the fixed header's fields.
const (
	relayFrameOffMagic   = 0
	relayFrameOffVersion = 1
	relayFrameOffType    = 2
	relayFrameOffFlags   = 3
	relayFrameOffSession = 4  // 8 bytes
	relayFrameOffHops    = 12 // 4 bytes
	relayFrameOffMeta    = 16 // 4 bytes
	relayFrameOffPayload = 20 // 4 bytes
)

// ErrRelayFrameFormat is returned when a byte slice is not a binary relay
// frame, or is one but is malformed.
//
// It is a sentinel so that the caller can tell "this is not my format"
// (a version mismatch, a peer speaking the other format) from "this is
// my format but it is broken" (a truncated frame).
var ErrRelayFrameFormat = errors.New("mesh: relay: not a binary relay frame")

// relayFrame is the binary form of one relay record.
//
// It mirrors relayRecord field for field, but it is a SEPARATE type on
// purpose: the two formats are independent, and a change to one must not
// be able to change the other by accident. The two converters below are
// the only place where the correspondence is stated.
type relayFrame struct {
	// Type is the frame type: FrameMeshRelayOpen, FrameMeshRelayHello,
	// FrameMeshRelayData, FrameMeshRelayClose, FrameMeshRelayJoin.
	Type uint8
	// Session is the relay session id.
	Session uint64
	// From and To name the two ends of the relay.
	From identity.DeviceID
	To   identity.DeviceID
	// Kind is the session kind the target must serve.
	Kind string
	// Hops is the hop budget. Zero means "not stated".
	Hops uint32
	// Hello is the end-to-end handshake message (a signed JSON blob).
	// It is JSON ON PURPOSE: it is signed over a transcript, and the
	// signature is what matters — its bytes are not a hot path.
	Hello []byte
	// Data is the opaque payload — the end-to-end ciphertext. This is
	// the field the trunk must not have to look at.
	Data []byte
	// Close marks the end of the relay.
	Close bool
	// Reason explains a close or a refusal.
	Reason string
	// Class is the answer's class, on a refusal.
	Class string
	// Grant says whether a request was granted.
	Grant bool
}

// relayFrameHeader is the fixed header of a binary relay record, read on
// its own.
//
// It is what the trunk needs in order to FORWARD A RECORD WITHOUT READING
// IT: Type says whether the record is the end of the relay, and
// PayloadLen says how many bytes of payload are in it (the rate quota is
// charged on those bytes).
type relayFrameHeader struct {
	// Version is the format version.
	Version uint8
	// Type is the frame type.
	Type uint8
	// Flags says which optional values the record carries.
	Flags uint8
	// Session is the relay session id.
	Session uint64
	// Hops is the hop budget.
	Hops uint32
	// MetaLen is the size of the meta block.
	MetaLen uint32
	// PayloadLen is the size of the payload.
	PayloadLen uint32
}

// relayFrameFromRecord builds the binary form of a relay record.
func relayFrameFromRecord(rec relayRecord) (relayFrame, error) {
	if rec.Hops < 0 {
		return relayFrame{}, fmt.Errorf("mesh: relay: the hop budget is negative (%d)", rec.Hops)
	}
	return relayFrame{
		Type:    rec.Type,
		Session: rec.Session,
		From:    rec.From,
		To:      rec.To,
		Kind:    rec.Kind,
		Hops:    uint32(rec.Hops),
		Hello:   rec.Hello,
		Data:    rec.Data,
		Close:   rec.Close,
		Reason:  rec.Reason,
		Class:   rec.Class,
		Grant:   rec.Grant,
	}, nil
}

// recordFromRelayFrame builds a relay record from its binary form.
//
// The inverse of relayFrameFromRecord. It is total: every binary frame
// decodes to SOME record, and the values it cannot know (an absent field)
// come back as the zero value, which is exactly what the JSON's omitempty
// would have produced.
func recordFromRelayFrame(f relayFrame) relayRecord {
	return relayRecord{
		Type:    f.Type,
		Session: f.Session,
		From:    f.From,
		To:      f.To,
		Kind:    f.Kind,
		Hops:    int(f.Hops),
		Hello:   f.Hello,
		Data:    f.Data,
		Close:   f.Close,
		Reason:  f.Reason,
		Class:   f.Class,
		Grant:   f.Grant,
	}
}

// EncodeBinaryRelayFrame encodes one relay record into its binary form.
//
// The result is the BODY of a relay record: it still travels inside the
// mesh's length-prefixed frame, exactly as the JSON body does.
func EncodeBinaryRelayFrame(f relayFrame) ([]byte, error) {
	meta, flags, err := encodeRelayFrameMeta(f)
	if err != nil {
		return nil, err
	}
	total := relayFrameHeaderSize + len(meta) + len(f.Data)
	if total > maxRelayRecord {
		return nil, fmt.Errorf("mesh: relay: record too large (%d bytes)", total)
	}
	out := make([]byte, relayFrameHeaderSize, total)
	out[relayFrameOffMagic] = relayFrameMagic
	out[relayFrameOffVersion] = relayFrameVersion
	out[relayFrameOffType] = f.Type
	out[relayFrameOffFlags] = flags
	binary.BigEndian.PutUint64(out[relayFrameOffSession:relayFrameOffSession+8], f.Session)
	binary.BigEndian.PutUint32(out[relayFrameOffHops:relayFrameOffHops+4], f.Hops)
	binary.BigEndian.PutUint32(out[relayFrameOffMeta:relayFrameOffMeta+4], uint32(len(meta)))
	binary.BigEndian.PutUint32(out[relayFrameOffPayload:relayFrameOffPayload+4], uint32(len(f.Data)))
	out = append(out, meta...)
	out = append(out, f.Data...)
	return out, nil
}

// encodeRelayFrameMeta builds the meta block and the flags byte.
//
// A field is present exactly when it is not the zero value — the same
// rule the JSON envelope's omitempty applies, so the two formats agree on
// what a zero value means.
func encodeRelayFrameMeta(f relayFrame) ([]byte, byte, error) {
	var flags uint8
	var meta []byte
	putString := func(flag uint8, s string) error {
		if s == "" {
			return nil
		}
		if len(s) > 0xFFFF {
			return fmt.Errorf("mesh: relay: a string field is too long (%d bytes)", len(s))
		}
		flags |= flag
		meta = appendUint16(meta, uint16(len(s)))
		meta = append(meta, s...)
		return nil
	}
	if err := putString(relayFrameFlagKind, f.Kind); err != nil {
		return nil, 0, err
	}
	if err := putString(relayFrameFlagReason, f.Reason); err != nil {
		return nil, 0, err
	}
	if err := putString(relayFrameFlagClass, f.Class); err != nil {
		return nil, 0, err
	}
	if err := putString(relayFrameFlagFrom, string(f.From)); err != nil {
		return nil, 0, err
	}
	if err := putString(relayFrameFlagTo, string(f.To)); err != nil {
		return nil, 0, err
	}
	if len(f.Hello) > 0 {
		flags |= relayFrameFlagHello
		meta = appendUint32(meta, uint32(len(f.Hello)))
		meta = append(meta, f.Hello...)
	}
	if f.Close {
		flags |= relayFrameFlagClose
	}
	if f.Grant {
		flags |= relayFrameFlagGrant
	}
	return meta, flags, nil
}

// DecodeBinaryRelayFrame decodes one relay record from its binary form.
//
// THE RETURNED SLICES ALIAS b. f.Data and f.Hello point INTO the input, not
// into fresh copies. That is deliberate — it is what lets the trunk forward
// a payload without copying it — but it means the caller must NOT keep the
// decoded frame past the lifetime of b.
//
// A reader that hands the trunk a buffer it is about to reuse must either
// copy the payload or use the forward path (PeekRelayFrame +
// SplitRelayFrame), which never decodes at all.
func DecodeBinaryRelayFrame(b []byte) (relayFrame, error) {
	hdr, meta, payload, err := SplitRelayFrame(b)
	if err != nil {
		return relayFrame{}, err
	}
	f := relayFrame{
		Type:    hdr.Type,
		Session: hdr.Session,
		Hops:    hdr.Hops,
		Close:   hdr.Flags&relayFrameFlagClose != 0,
		Grant:   hdr.Flags&relayFrameFlagGrant != 0,
	}
	rest := meta
	take := func(flag uint8, width int) ([]byte, error) {
		if hdr.Flags&flag == 0 {
			return nil, nil
		}
		if len(rest) < width {
			return nil, fmt.Errorf("%w: the meta block is truncated", ErrRelayFrameFormat)
		}
		var n uint32
		if width == 2 {
			n = uint32(binary.BigEndian.Uint16(rest[:2]))
		} else {
			n = binary.BigEndian.Uint32(rest[:4])
		}
		rest = rest[width:]
		if uint64(len(rest)) < uint64(n) {
			return nil, fmt.Errorf("%w: the meta block is truncated", ErrRelayFrameFormat)
		}
		out := rest[:n]
		rest = rest[n:]
		return out, nil
	}
	kind, err := take(relayFrameFlagKind, 2)
	if err != nil {
		return relayFrame{}, err
	}
	f.Kind = string(kind)
	reason, err := take(relayFrameFlagReason, 2)
	if err != nil {
		return relayFrame{}, err
	}
	f.Reason = string(reason)
	class, err := take(relayFrameFlagClass, 2)
	if err != nil {
		return relayFrame{}, err
	}
	f.Class = string(class)
	from, err := take(relayFrameFlagFrom, 2)
	if err != nil {
		return relayFrame{}, err
	}
	f.From = identity.DeviceID(from)
	to, err := take(relayFrameFlagTo, 2)
	if err != nil {
		return relayFrame{}, err
	}
	f.To = identity.DeviceID(to)
	hello, err := take(relayFrameFlagHello, 4)
	if err != nil {
		return relayFrame{}, err
	}
	if hello != nil {
		f.Hello = hello
	}
	f.Data = payload
	return f, nil
}

// PeekRelayFrame reads the fixed header of a binary relay record.
//
// It reads NOTHING else: no meta, no payload. That is the whole point —
// the trunk can learn the type and the payload size from 24 bytes and
// then forward the rest without looking at it.
//
// It returns ErrRelayFrameFormat when the bytes are not a binary relay
// frame, and a plain error when the frame is truncated.
func PeekRelayFrame(b []byte) (relayFrameHeader, error) {
	if len(b) < relayFrameHeaderSize {
		return relayFrameHeader{}, fmt.Errorf("%w: short header (%d bytes)", ErrRelayFrameFormat, len(b))
	}
	if b[relayFrameOffMagic] != relayFrameMagic {
		return relayFrameHeader{}, fmt.Errorf("%w: bad magic byte 0x%02x", ErrRelayFrameFormat, b[relayFrameOffMagic])
	}
	version := b[relayFrameOffVersion]
	if version != relayFrameVersion {
		return relayFrameHeader{}, fmt.Errorf("%w: unsupported version %d", ErrRelayFrameFormat, version)
	}
	return relayFrameHeader{
		Version:    version,
		Type:       b[relayFrameOffType],
		Flags:      b[relayFrameOffFlags],
		Session:    binary.BigEndian.Uint64(b[relayFrameOffSession : relayFrameOffSession+8]),
		Hops:       binary.BigEndian.Uint32(b[relayFrameOffHops : relayFrameOffHops+4]),
		MetaLen:    binary.BigEndian.Uint32(b[relayFrameOffMeta : relayFrameOffMeta+4]),
		PayloadLen: binary.BigEndian.Uint32(b[relayFrameOffPayload : relayFrameOffPayload+4]),
	}, nil
}

// SplitRelayFrame splits a binary relay record into its header, its meta
// block and its payload.
//
// The two returned slices are SUBSLICES of b: no copy is made. That is
// what lets the trunk forward a record's payload without touching it.
func SplitRelayFrame(b []byte) (relayFrameHeader, []byte, []byte, error) {
	hdr, err := PeekRelayFrame(b)
	if err != nil {
		return relayFrameHeader{}, nil, nil, err
	}
	need := int64(relayFrameHeaderSize) + int64(hdr.MetaLen) + int64(hdr.PayloadLen)
	if int64(len(b)) != need {
		return relayFrameHeader{}, nil, nil, fmt.Errorf("%w: the frame is %d bytes, the header says %d",
			ErrRelayFrameFormat, len(b), need)
	}
	meta := b[relayFrameHeaderSize : relayFrameHeaderSize+int(hdr.MetaLen)]
	payload := b[relayFrameHeaderSize+int(hdr.MetaLen):]
	return hdr, meta, payload, nil
}

// appendUint16 appends a big-endian uint16.
func appendUint16(dst []byte, v uint16) []byte {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return append(dst, b[:]...)
}

// appendUint32 appends a big-endian uint32.
func appendUint32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}
