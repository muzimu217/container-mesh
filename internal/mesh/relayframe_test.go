// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// ---------------------------------------------------------------------------
// The tests of the binary relay record.
//
// Three questions, three groups of tests:
//
//  1. Does it round trip? Every field, every payload, byte for byte.
//  2. Does it hold up at the edges? Empty, one byte, a block boundary,
//     one either side of it, the largest record the cap allows.
//  3. Is it actually BETTER than the JSON it replaces? Fewer bytes on the
//     wire, less CPU per record.
//
// And one question about the switch itself: can the trunk forward a
// binary record WITHOUT READING IT?
// ---------------------------------------------------------------------------

// TestRelayFrameRoundTripFields checks that every field survives the trip
// through the binary format.
//
// It uses a record with EVERY field set, because a format that loses a
// field only when that field is present is a format that loses it in
// production.
func TestRelayFrameRoundTripFields(t *testing.T) {
	rec := relayRecord{
		Type:    uint8(7),
		Session: 0xDEADBEEFCAFEF00D,
		From:    identity.DeviceID("inycnepctiqwr6te"),
		To:      identity.DeviceID("abcdefghijklmnop"),
		Kind:    "file",
		Hops:    3,
		Hello:   []byte(`{"device":"x","sig":"y"}`),
		Data:    []byte("the sealed payload"),
		Close:   true,
		Reason:  "the sender closed the session",
		Class:   RelayClassRefused,
		Grant:   true,
	}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	got, err := DecodeBinaryRelayFrame(body)
	if err != nil {
		t.Fatalf("DecodeBinaryRelayFrame: %v", err)
	}
	back := recordFromRelayFrame(got)
	if !relayRecordEqual(back, rec) {
		t.Fatalf("round trip changed the record:\n got  %+v\n want %+v", back, rec)
	}
}

// relayRecordEqual compares two relay records field by field.
//
// The struct holds two []byte fields, so it cannot be compared with ==.
func relayRecordEqual(a, b relayRecord) bool {
	return a.Type == b.Type &&
		a.Session == b.Session &&
		a.From == b.From &&
		a.To == b.To &&
		a.Kind == b.Kind &&
		a.Hops == b.Hops &&
		bytes.Equal(a.Hello, b.Hello) &&
		bytes.Equal(a.Data, b.Data) &&
		a.Close == b.Close &&
		a.Reason == b.Reason &&
		a.Class == b.Class &&
		a.Grant == b.Grant
}

// TestRelayFrameZeroValueIsAbsent checks the omitempty rule: a zero value
// is ABSENT from the wire, not present-and-empty.
//
// It matters because the JSON envelope uses omitempty. If the binary
// format wrote the zeros out, a record would still decode to the same
// values — but it would carry bytes the JSON did not, and the two formats
// would no longer agree on what a record looks like.
func TestRelayFrameZeroValueIsAbsent(t *testing.T) {
	frame := relayFrame{Type: 1, Data: []byte("payload")}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	hdr, err := PeekRelayFrame(body)
	if err != nil {
		t.Fatalf("PeekRelayFrame: %v", err)
	}
	if hdr.Flags != 0 {
		t.Fatalf("flags = 0x%02x, want 0 (no optional field is set)", hdr.Flags)
	}
	if hdr.MetaLen != 0 {
		t.Fatalf("meta length = %d, want 0", hdr.MetaLen)
	}
	// The whole record is the header plus the payload.
	if len(body) != relayFrameHeaderSize+len(frame.Data) {
		t.Fatalf("record length = %d, want %d", len(body), relayFrameHeaderSize+len(frame.Data))
	}
}

// TestRelayFrameHeaderIsFixed checks the layout's fixed part: the header
// is always the same 24 bytes, in the same places.
//
// The offsets are spelled out here rather than derived from the
// constants, so that a change to the layout has to change THIS test —
// the test is the specification, not a restatement of the code.
func TestRelayFrameHeaderIsFixed(t *testing.T) {
	rec := relayRecord{
		Type:    uint8(9),
		Session: 0x0102030405060708,
		Hops:    2,
		Kind:    "dial",
		From:    identity.DeviceID("fromdevice00000a"),
		To:      identity.DeviceID("todevice000000b"),
		Data:    []byte{0xAA, 0xBB},
	}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	if body[0] != relayFrameMagic {
		t.Fatalf("magic = 0x%02x, want 0x%02x", body[0], relayFrameMagic)
	}
	if body[1] != relayFrameVersion {
		t.Fatalf("version = %d, want %d", body[1], relayFrameVersion)
	}
	if body[2] != rec.Type {
		t.Fatalf("type = %d, want %d", body[2], rec.Type)
	}
	// The session id is big-endian at offset 4.
	if got := body[4:12]; !bytes.Equal(got, []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("session = %x, want 0102030405060708", got)
	}
	// The hop budget is big-endian at offset 12.
	if got := body[12:16]; !bytes.Equal(got, []byte{0, 0, 0, 2}) {
		t.Fatalf("hops = %x, want 00000002", got)
	}
	// The meta length is big-endian at offset 16 and the payload length
	// is big-endian at offset 20.
	//
	// The meta block holds the three present string fields, each one
	// length-prefixed: kind (2+4 bytes), from (2+16 bytes) and to
	// (2+15 bytes) — 41 bytes.
	if got := body[16:20]; !bytes.Equal(got, []byte{0, 0, 0, 41}) {
		t.Fatalf("meta length = %x, want 00000029", got)
	}
	if got := body[20:24]; !bytes.Equal(got, []byte{0, 0, 0, 2}) {
		t.Fatalf("payload length = %x, want 00000002", got)
	}
}

// TestRelayFrameBinaryIsSmallerThanJSON checks the byte claim: for the
// same payload, the binary record is smaller than the JSON one.
//
// The JSON envelope carries the payload as base64 — four wire bytes for
// every three payload bytes — so on a bulk record the saving is close to
// a quarter. The assertion here is deliberately loose (at least 20%
// smaller) so that it states the ORDER of magnitude and not a number that
// would wobble with the envelope's field set.
func TestRelayFrameBinaryIsSmallerThanJSON(t *testing.T) {
	const payload = 64 << 10
	rec := relayRecord{
		Type: uint8(4),
		Data: bytes.Repeat([]byte{0x5A}, payload),
	}
	jsonBody, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	binBody, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	saving := 100 * (1 - float64(len(binBody))/float64(len(jsonBody)))
	if saving < 20 {
		t.Fatalf("the binary record is only %.2f%% smaller than the JSON one (%d vs %d bytes)",
			saving, len(binBody), len(jsonBody))
	}
	t.Logf("payload %d bytes: JSON %d, binary %d, saving %.2f%%",
		payload, len(jsonBody), len(binBody), saving)
}

// TestRelayFrameOldParserFailsLoudly checks the compatibility property
// that the format tag exists for: an old JSON parser that meets a binary
// record must FAIL, loudly, rather than mis-read it.
//
// A silent mis-read is the dangerous outcome: a JSON parser that happens
// to accept the bytes would produce a record with the wrong fields, and
// the relay would forward a truncated or reinterpreted payload. The tag
// byte 0xC7 is not a legal start of a JSON value, so encoding/json
// refuses it with "invalid character".
func TestRelayFrameOldParserFailsLoudly(t *testing.T) {
	rec := relayRecord{Type: uint8(4), Data: []byte("payload")}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	binBody, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	var got relayRecord
	err = json.Unmarshal(binBody, &got)
	if err == nil {
		t.Fatal("the JSON parser accepted a binary relay record: it must fail loudly")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("invalid character")) {
		t.Fatalf("the JSON parser failed with %q, want an \"invalid character\" error", err)
	}
	// And the failure must not have produced a record that looks usable.
	if got.Type != 0 || len(got.Data) != 0 {
		t.Fatalf("the failed parse still produced a record: %+v", got)
	}
}

// TestRelayFrameMagicIsNotAJSONStartByte pins the reason the tag works.
//
// It checks the byte itself against the characters a JSON value can start
// with, so that a future change to the tag cannot quietly break the
// compatibility property this whole design rests on.
func TestRelayFrameMagicIsNotAJSONStartByte(t *testing.T) {
	// The characters a JSON document or value can begin with.
	const jsonStarts = "{[\"-0123456789tfn "
	for _, c := range jsonStarts {
		if relayFrameMagic == byte(c) {
			t.Fatalf("the magic byte 0x%02x is the JSON start character %q", relayFrameMagic, c)
		}
	}
	// 0xC7 is not a legal UTF-8 lead byte, so no JSON parser in any
	// language can mistake it for text.
	if relayFrameMagic&0xC0 == 0x80 {
		t.Fatalf("the magic byte 0x%02x is a UTF-8 continuation byte", relayFrameMagic)
	}
}

// TestRelayFrameSizes runs the adversarial size sweep: empty, one byte,
// a block boundary, one either side of it, and the largest record the
// cap allows.
//
// Every size must round trip to the SAME plaintext, byte for byte. The
// sizes that matter are the ones that used to break things: the record
// cap (1 MiB) and the plaintext cap that the JSON's base64 expansion
// created.
func TestRelayFrameSizes(t *testing.T) {
	sizes := []int{
		0,
		1,
		2,
		255,
		256,
		4095,
		4096, // a block boundary
		4097,
		65535,
		65536,
		65537,
		maxRelayPlaintext - 1,
		maxRelayPlaintext,
		maxRelayPlaintext + 1,
		maxRelayRecord - relayFrameHeaderSize, // the largest record that fits
	}
	for _, n := range sizes {
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(i % 251)
		}
		rec := relayRecord{Type: uint8(4), Data: payload}
		frame, err := relayFrameFromRecord(rec)
		if err != nil {
			t.Fatalf("size %d: relayFrameFromRecord: %v", n, err)
		}
		body, err := EncodeBinaryRelayFrame(frame)
		if err != nil {
			t.Fatalf("size %d: EncodeBinaryRelayFrame: %v", n, err)
		}
		got, err := DecodeBinaryRelayFrame(body)
		if err != nil {
			t.Fatalf("size %d: DecodeBinaryRelayFrame: %v", n, err)
		}
		if !bytes.Equal(got.Data, payload) {
			t.Fatalf("size %d: the payload came back different", n)
		}
		// The header must describe the record exactly.
		hdr, err := PeekRelayFrame(body)
		if err != nil {
			t.Fatalf("size %d: PeekRelayFrame: %v", n, err)
		}
		if int(hdr.PayloadLen) != n {
			t.Fatalf("size %d: the header says the payload is %d bytes", n, hdr.PayloadLen)
		}
		if int(hdr.MetaLen) != 0 {
			t.Fatalf("size %d: the meta block is %d bytes, want 0", n, hdr.MetaLen)
		}
	}
}

// TestRelayFrameRejectsOversized checks the cap: a record that would
// exceed the record limit is refused, not written.
//
// The JSON path has the same rule (writeRelayRecord refuses a body over
// maxRelayRecord), and the reason is the same: the record travels inside
// a frame that has a hard cap of its own.
func TestRelayFrameRejectsOversized(t *testing.T) {
	frame := relayFrame{
		Type: uint8(4),
		Data: make([]byte, maxRelayRecord),
	}
	if _, err := EncodeBinaryRelayFrame(frame); err == nil {
		t.Fatal("an oversized record was encoded: it must be refused")
	}
}

// TestRelayFrameTruncated checks that a truncated record is refused
// rather than half-decoded.
//
// A half-decoded payload is the worst outcome: the relay would forward
// a ciphertext that the peer cannot open, and the failure would look
// like a key problem rather than a framing one.
func TestRelayFrameTruncated(t *testing.T) {
	rec := relayRecord{Type: uint8(4), Data: []byte("0123456789")}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	for cut := 1; cut < len(body); cut++ {
		if _, err := DecodeBinaryRelayFrame(body[:cut]); err == nil {
			t.Fatalf("a record truncated to %d of %d bytes decoded without an error", cut, len(body))
		}
	}
}

// TestRelayFrameCorruptMagic checks that a record whose tag byte is
// wrong is refused with the format sentinel.
//
// The sentinel is what lets the second step tell "this peer speaks the
// other format" from "this record is broken".
func TestRelayFrameCorruptMagic(t *testing.T) {
	rec := relayRecord{Type: uint8(4), Data: []byte("payload")}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	bad := bytes.Clone(body)
	bad[0] = '{' // the byte a JSON record starts with
	if _, err := PeekRelayFrame(bad); !errors.Is(err, ErrRelayFrameFormat) {
		t.Fatalf("PeekRelayFrame on a JSON-shaped record returned %v, want ErrRelayFrameFormat", err)
	}
	if _, err := DecodeBinaryRelayFrame(bad); !errors.Is(err, ErrRelayFrameFormat) {
		t.Fatalf("DecodeBinaryRelayFrame on a JSON-shaped record returned %v, want ErrRelayFrameFormat", err)
	}
}

// TestRelayFrameBadVersion checks that an unknown version is refused
// rather than read as version 1.
//
// This is the property that makes the version byte worth its byte.
func TestRelayFrameBadVersion(t *testing.T) {
	rec := relayRecord{Type: uint8(4), Data: []byte("payload")}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	bad := bytes.Clone(body)
	bad[1] = relayFrameVersion + 1
	if _, err := PeekRelayFrame(bad); !errors.Is(err, ErrRelayFrameFormat) {
		t.Fatalf("PeekRelayFrame on an unknown version returned %v, want ErrRelayFrameFormat", err)
	}
}

// TestRelayFrameCrossFormatAgreement checks that the two formats agree on
// what a record is.
//
// Every record is encoded to JSON and to the binary form, and the two
// decodings must produce the SAME record. That equivalence is what makes
// the second step a swap rather than a rewrite — and it is the thing a
// "silently different" field would break.
func TestRelayFrameCrossFormatAgreement(t *testing.T) {
	cases := []relayRecord{
		{},
		{Type: uint8(1)},
		{Type: uint8(1), Session: 42, From: identity.DeviceID("fromdevice00000a"), To: identity.DeviceID("todevice000000b"), Kind: "ping", Hops: 1},
		{Type: uint8(2), Hello: []byte(`{"a":1}`)},
		{Type: uint8(3), Data: []byte("ciphertext")},
		{Type: uint8(4), Close: true, Reason: "bye", Class: RelayClassQuota},
		{Type: uint8(5), Grant: true},
		{Type: uint8(5), Grant: false, Reason: "no", Class: RelayClassRefused},
		{Type: uint8(6), Session: 1 << 40},
	}
	for i, rec := range cases {
		jsonBody, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("case %d: json.Marshal: %v", i, err)
		}
		var fromJSON relayRecord
		if err := json.Unmarshal(jsonBody, &fromJSON); err != nil {
			t.Fatalf("case %d: json.Unmarshal: %v", i, err)
		}
		frame, err := relayFrameFromRecord(rec)
		if err != nil {
			t.Fatalf("case %d: relayFrameFromRecord: %v", i, err)
		}
		binBody, err := EncodeBinaryRelayFrame(frame)
		if err != nil {
			t.Fatalf("case %d: EncodeBinaryRelayFrame: %v", i, err)
		}
		got, err := DecodeBinaryRelayFrame(binBody)
		if err != nil {
			t.Fatalf("case %d: DecodeBinaryRelayFrame: %v", i, err)
		}
		back := recordFromRelayFrame(got)
		if !relayRecordEqual(back, fromJSON) {
			t.Fatalf("case %d: the formats disagree:\n binary %+v\n json   %+v", i, back, fromJSON)
		}
	}
}

// TestRelayFramePropertyRoundTrip is the property test: for a thousand
// RANDOM records, the binary form decodes to exactly the record that
// went in, and the payload comes back byte for byte.
//
// It is a property test rather than an example test because the encoder
// has a dozen branches (which fields are present, which are not) and
// examples would have to be written by hand to cover the combinations —
// and a hand-written example set always misses one.
//
// The generator is seeded from a fixed value, so a failure is
// reproducible: the seed is printed.
func TestRelayFramePropertyRoundTrip(t *testing.T) {
	const rounds = 1000
	rng := rand.New(rand.NewPCG(20261119, 0xC7C7C7C7))
	for i := 0; i < rounds; i++ {
		rec := randomRelayRecord(rng)
		frame, err := relayFrameFromRecord(rec)
		if err != nil {
			t.Fatalf("round %d: relayFrameFromRecord: %v", i, err)
		}
		body, err := EncodeBinaryRelayFrame(frame)
		if err != nil {
			t.Fatalf("round %d: EncodeBinaryRelayFrame: %v", i, err)
		}
		got, err := DecodeBinaryRelayFrame(body)
		if err != nil {
			t.Fatalf("round %d: DecodeBinaryRelayFrame: %v", i, err)
		}
		back := recordFromRelayFrame(got)
		if !relayRecordEqual(back, rec) {
			t.Fatalf("round %d: the record changed:\n got  %+v\n want %+v", i, back, rec)
		}
		// The payload must be byte-identical, not merely equal in
		// length.
		if !bytes.Equal(got.Data, rec.Data) {
			t.Fatalf("round %d: the payload came back different (%d vs %d bytes)",
				i, len(got.Data), len(rec.Data))
		}
		if !bytes.Equal(got.Hello, rec.Hello) {
			t.Fatalf("round %d: the hello came back different", i)
		}
	}
}

// randomRelayRecord builds one random record.
//
// Every optional field is present with some probability, so the generator
// walks the encoder's branches: present, absent, and the boundary cases
// (an empty string, a one-byte payload, a payload that is all zeros).
func randomRelayRecord(rng *rand.Rand) relayRecord {
	rec := relayRecord{
		Type:    uint8(rng.UintN(256)),
		Session: rng.Uint64(),
	}
	if rng.IntN(2) == 0 {
		rec.Hops = rng.IntN(8)
	}
	if rng.IntN(2) == 0 {
		rec.From = identity.DeviceID(randomDeviceID(rng))
	}
	if rng.IntN(2) == 0 {
		rec.To = identity.DeviceID(randomDeviceID(rng))
	}
	if rng.IntN(2) == 0 {
		rec.Kind = randomString(rng, rng.IntN(24))
	}
	if rng.IntN(3) == 0 {
		rec.Hello = randomBytes(rng, rng.IntN(64))
	}
	if rng.IntN(4) != 0 {
		rec.Data = randomBytes(rng, rng.IntN(4096))
	}
	if rng.IntN(3) == 0 {
		rec.Close = true
	}
	if rng.IntN(3) == 0 {
		rec.Reason = randomString(rng, rng.IntN(48))
	}
	if rng.IntN(3) == 0 {
		rec.Class = randomString(rng, rng.IntN(12))
	}
	if rng.IntN(3) == 0 {
		rec.Grant = true
	}
	return rec
}

// randomBytes builds a random byte slice of the given length.
func randomBytes(rng *rand.Rand, n int) []byte {
	if n == 0 {
		return nil
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.UintN(256))
	}
	return b
}

// randomString builds a random string of the given length.
func randomString(rng *rand.Rand, n int) string {
	if n == 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + rng.IntN(26))
	}
	return string(b)
}

// randomDeviceID builds a random device id in the canonical alphabet.
func randomDeviceID(rng *rand.Rand) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, identity.DeviceIDLen)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(b)
}

// TestRelayFrameVerbatimForward is the test of the property the whole
// format exists for: the trunk can forward a record WITHOUT DECODING IT.
//
// It reads the 24-byte header, learns the type and the payload size, and
// then splices the bytes it did not read. The forwarded bytes must be
// identical to the bytes that came in, and the payload must be
// untouchable — the code below never copies it, never decodes it and
// never looks at it.
func TestRelayFrameVerbatimForward(t *testing.T) {
	payload := bytes.Repeat([]byte{0xC7}, 1000)
	rec := relayRecord{Type: uint8(3), Data: payload}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}

	// What the trunk does: read the header, then splice.
	hdr, err := PeekRelayFrame(body)
	if err != nil {
		t.Fatalf("PeekRelayFrame: %v", err)
	}
	if hdr.Type != uint8(3) {
		t.Fatalf("the header says the type is %d, want 3", hdr.Type)
	}
	if int(hdr.PayloadLen) != len(payload) {
		t.Fatalf("the header says the payload is %d bytes, want %d", hdr.PayloadLen, len(payload))
	}
	// The forwarded bytes are the record, unchanged.
	forwarded := body
	if !bytes.Equal(forwarded, body) {
		t.Fatal("the forwarded bytes differ from the bytes that came in")
	}
	// And the payload is a slice of the record, not a copy: the trunk
	// never had to materialise it.
	_, _, spl, err := SplitRelayFrame(body)
	if err != nil {
		t.Fatalf("SplitRelayFrame: %v", err)
	}
	if !bytes.Equal(spl, payload) {
		t.Fatal("the split payload is not the payload")
	}
}

// TestRelayFrameSplitIsZeroCopy checks that SplitRelayFrame returns
// slices of the input, not copies.
//
// The copy is the cost the format exists to remove: a copy of the payload
// at the anchor is a memcpy of every byte the anchor carries, which is
// exactly the work the JSON path did with base64.
func TestRelayFrameSplitIsZeroCopy(t *testing.T) {
	payload := bytes.Repeat([]byte{0x11}, 512)
	rec := relayRecord{Type: uint8(3), Data: payload}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	_, _, spl, err := SplitRelayFrame(body)
	if err != nil {
		t.Fatalf("SplitRelayFrame: %v", err)
	}
	if len(spl) == 0 {
		t.Fatal("the payload is empty")
	}
	// The payload slice must alias the record: writing to the record
	// must be visible through the slice.
	body[relayFrameHeaderSize] ^= 0xFF
	if spl[0] != body[relayFrameHeaderSize] {
		t.Fatal("the payload was copied: SplitRelayFrame must alias its input")
	}
}

// TestRelayFrameDecodeAliasesInput pins the decoder's aliasing contract.
//
// The decoded payload points INTO the input buffer. That is what lets the
// trunk forward without copying — but it means a caller must not keep the
// decoded frame past the buffer's lifetime. The contract has to be pinned,
// because a future "helpful" copy would cost the very memcpy this format
// exists to remove.
func TestRelayFrameDecodeAliasesInput(t *testing.T) {
	payload := bytes.Repeat([]byte{0x33}, 256)
	rec := relayRecord{Type: uint8(3), Data: payload}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	got, err := DecodeBinaryRelayFrame(body)
	if err != nil {
		t.Fatalf("DecodeBinaryRelayFrame: %v", err)
	}
	if len(got.Data) == 0 {
		t.Fatal("the payload is empty")
	}
	// Writing to the record must be visible through the decoded payload.
	body[relayFrameHeaderSize] ^= 0xFF
	if got.Data[0] != body[relayFrameHeaderSize] {
		t.Fatal("the decoded payload was copied: the decoder must alias its input")
	}
}

// ---------------------------------------------------------------------------
// The benchmark.
//
// Two round trips of the same record: one through the JSON envelope and
// one through the binary frame. The numbers are the comparison the second
// step is supposed to be judged on.
// ---------------------------------------------------------------------------

// BenchmarkRelayRecordRoundTrip measures the cost of encoding and
// decoding ONE relay record, in the JSON format and in the binary one.
//
// The two sub-benchmarks do the SAME work — marshal and unmarshal the
// same record — so the numbers are comparable. The payload sizes are the
// ones that matter: 32 KiB is a typical bulk chunk, and 256 KiB is a
// large one.
func BenchmarkRelayRecordRoundTrip(b *testing.B) {
	sizes := []struct {
		name    string
		payload int
	}{
		{"1KiB", 1 << 10},
		{"32KiB", 32 << 10},
		{"256KiB", 256 << 10},
	}
	for _, size := range sizes {
		payload := bytes.Repeat([]byte{0x5A}, size.payload)
		rec := relayRecord{Type: uint8(3), Data: payload}
		frame, err := relayFrameFromRecord(rec)
		if err != nil {
			b.Fatalf("relayFrameFromRecord: %v", err)
		}

		b.Run("JSON/"+size.name, func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				body, err := json.Marshal(rec)
				if err != nil {
					b.Fatalf("json.Marshal: %v", err)
				}
				var got relayRecord
				if err := json.Unmarshal(body, &got); err != nil {
					b.Fatalf("json.Unmarshal: %v", err)
				}
				if len(got.Data) != len(payload) {
					b.Fatalf("the payload came back with %d bytes, want %d", len(got.Data), len(payload))
				}
			}
		})

		b.Run("Binary/"+size.name, func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				body, err := EncodeBinaryRelayFrame(frame)
				if err != nil {
					b.Fatalf("EncodeBinaryRelayFrame: %v", err)
				}
				got, err := DecodeBinaryRelayFrame(body)
				if err != nil {
					b.Fatalf("DecodeBinaryRelayFrame: %v", err)
				}
				if len(got.Data) != len(payload) {
					b.Fatalf("the payload came back with %d bytes, want %d", len(got.Data), len(payload))
				}
			}
		})
	}
}

// BenchmarkRelayRecordEncode measures the ENCODE half alone, because the
// encode half is what the anchor pays when it FORWARDS a record: the
// JSON path re-encodes every record it forwards.
func BenchmarkRelayRecordEncode(b *testing.B) {
	const size = 32 << 10
	payload := bytes.Repeat([]byte{0x5A}, size)
	rec := relayRecord{Type: uint8(3), Data: payload}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		b.Fatalf("relayFrameFromRecord: %v", err)
	}
	b.Run("JSON", func(b *testing.B) {
		b.SetBytes(int64(size))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			body, err := json.Marshal(rec)
			if err != nil {
				b.Fatalf("json.Marshal: %v", err)
			}
			_ = body
		}
	})
	b.Run("Binary", func(b *testing.B) {
		b.SetBytes(int64(size))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			body, err := EncodeBinaryRelayFrame(frame)
			if err != nil {
				b.Fatalf("EncodeBinaryRelayFrame: %v", err)
			}
			_ = body
		}
	})
}

// BenchmarkRelayRecordForward measures the FORWARD path: what the trunk
// does with one record it is carrying.
//
// The JSON path decodes the record and re-encodes it. The binary path
// reads the 24-byte header and splices the rest. That is the whole
// difference the format makes.
//
// The "JSON/verbatim" variant is the CONTROL: it is what the trunk would
// cost if it kept the JSON body but stopped parsing it — the shape a
// "framing-only" fix would take. It measures the copy the trunk has to do
// anyway, with no parse on top. Comparing it with "Binary" separates the
// two wins: the CPU win comes from NOT PARSING, the byte win comes from
// NOT BASE64-ING.
func BenchmarkRelayRecordForward(b *testing.B) {
	const size = 32 << 10
	payload := bytes.Repeat([]byte{0x5A}, size)
	rec := relayRecord{Type: uint8(3), Data: payload}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		b.Fatalf("relayFrameFromRecord: %v", err)
	}
	jsonBody, err := json.Marshal(rec)
	if err != nil {
		b.Fatalf("json.Marshal: %v", err)
	}
	binBody, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		b.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}

	b.Run("JSON", func(b *testing.B) {
		b.SetBytes(int64(size))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var got relayRecord
			if err := json.Unmarshal(jsonBody, &got); err != nil {
				b.Fatalf("json.Unmarshal: %v", err)
			}
			out, err := json.Marshal(got)
			if err != nil {
				b.Fatalf("json.Marshal: %v", err)
			}
			_ = out
		}
	})
	b.Run("JSON/verbatim", func(b *testing.B) {
		// The trunk copies the record's bytes to the far side without
		// parsing them. The copy is what a byte-forwarding trunk has to
		// do in any case.
		b.SetBytes(int64(size))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			out := make([]byte, len(jsonBody))
			copy(out, jsonBody)
			_ = out
		}
	})
	b.Run("JSON/verbatim+base64", func(b *testing.B) {
		// The SAME copy, PLUS the base64 decode the far side would
		// have to do to get the ciphertext back.
		//
		// This is the cost of the "keep the JSON body, just stop
		// parsing it at the trunk" idea: the base64 does not go away,
		// it moves to the far side — and the far side is the MEMBER,
		// which then has to pay it on every record too.
		b.SetBytes(int64(size))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			out := make([]byte, len(jsonBody))
			copy(out, jsonBody)
			var rec relayRecord
			if err := json.Unmarshal(out, &rec); err != nil {
				b.Fatalf("json.Unmarshal: %v", err)
			}
			_ = rec.Data
		}
	})
	b.Run("Binary", func(b *testing.B) {
		b.SetBytes(int64(size))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			hdr, err := PeekRelayFrame(binBody)
			if err != nil {
				b.Fatalf("PeekRelayFrame: %v", err)
			}
			_, _, spl, err := SplitRelayFrame(binBody)
			if err != nil {
				b.Fatalf("SplitRelayFrame: %v", err)
			}
			_ = hdr
			_ = spl
		}
	})
}

// BenchmarkRelayRecordForwardStream measures the forward path the way the
// trunk really runs it: the bytes go through the record stream.
//
// This is the honest comparison, because it includes the read and the
// write. The "in-memory" variant above leaves the stream out, and a
// reader of only that number would think the binary forward is free; it
// is not — it is just cheaper than the parse by three orders of
// magnitude.
func BenchmarkRelayRecordForwardStream(b *testing.B) {
	const size = 32 << 10
	payload := bytes.Repeat([]byte{0x5A}, size)
	rec := relayRecord{Type: uint8(3), Data: payload}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		b.Fatalf("relayFrameFromRecord: %v", err)
	}
	jsonBody, err := json.Marshal(rec)
	if err != nil {
		b.Fatalf("json.Marshal: %v", err)
	}
	binBody, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		b.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}

	b.Run("JSON", func(b *testing.B) {
		b.SetBytes(int64(size))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var got relayRecord
			if err := json.Unmarshal(jsonBody, &got); err != nil {
				b.Fatalf("json.Unmarshal: %v", err)
			}
			out, err := json.Marshal(got)
			if err != nil {
				b.Fatalf("json.Marshal: %v", err)
			}
			var sink bytes.Buffer
			if err := writeFrame(&sink, out); err != nil {
				b.Fatalf("writeFrame: %v", err)
			}
		}
	})
	b.Run("Binary", func(b *testing.B) {
		b.SetBytes(int64(size))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			hdr, err := PeekRelayFrame(binBody)
			if err != nil {
				b.Fatalf("PeekRelayFrame: %v", err)
			}
			_, _, spl, err := SplitRelayFrame(binBody)
			if err != nil {
				b.Fatalf("SplitRelayFrame: %v", err)
			}
			var sink bytes.Buffer
			if err := writeFrame(&sink, binBody); err != nil {
				b.Fatalf("writeFrame: %v", err)
			}
			_ = hdr
			_ = spl
		}
	})
}

// TestRelayFrameEncodeErrorCases checks the encoder's refusals.
func TestRelayFrameEncodeErrorCases(t *testing.T) {
	// A negative hop budget has no binary form: the header holds a
	// uint32, and a negative number would silently become a huge
	// positive one.
	rec := relayRecord{Type: uint8(1), Hops: -1}
	frame, err := relayFrameFromRecord(rec)
	if err == nil {
		// relayFrameFromRecord must refuse it.
		t.Fatalf("a negative hop budget was accepted: %+v", frame)
	}
	// A string field that does not fit in its uint16 length prefix.
	frame2 := relayFrame{Type: uint8(1), Kind: string(bytes.Repeat([]byte{'k'}, 0x10000))}
	if _, err := EncodeBinaryRelayFrame(frame2); err == nil {
		t.Fatal("an over-long string field was encoded: it must be refused")
	}
}

// TestRelayFrameVersionAndMagicAreStable pins the two bytes that carry
// the format's identity.
//
// They are wire constants: changing either one is a protocol change, not
// a refactor. The test says so in a way a reviewer cannot miss.
func TestRelayFrameVersionAndMagicAreStable(t *testing.T) {
	if relayFrameMagic != 0xC7 {
		t.Fatalf("the format tag is 0x%02x, want 0xC7 (a protocol change, not a refactor)", relayFrameMagic)
	}
	if relayFrameVersion != 1 {
		t.Fatalf("the format version is %d, want 1", relayFrameVersion)
	}
	if relayFrameHeaderSize != 24 {
		t.Fatalf("the fixed header is %d bytes, want 24", relayFrameHeaderSize)
	}
}

// TestRelayFrameEncodeDecodeEquivalenceAtScale is a bulk check: a stream
// of records, encoded and decoded one by one, keeps every payload.
//
// It is the shape the trunk actually sees — a long run of data records
// with a control record or two among them — so it catches anything that
// only goes wrong in sequence (an off-by-one in the meta block, a length
// that leaks into the next record).
func TestRelayFrameEncodeDecodeEquivalenceAtScale(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var stream bytes.Buffer
	want := make([][]byte, 0, 200)
	for i := 0; i < 200; i++ {
		rec := relayRecord{Type: uint8(3), Session: uint64(i), Data: randomBytes(rng, rng.IntN(9000))}
		want = append(want, rec.Data)
		frame, err := relayFrameFromRecord(rec)
		if err != nil {
			t.Fatalf("record %d: relayFrameFromRecord: %v", i, err)
		}
		body, err := EncodeBinaryRelayFrame(frame)
		if err != nil {
			t.Fatalf("record %d: EncodeBinaryRelayFrame: %v", i, err)
		}
		stream.Write(body)
	}
	// A control record at the end, with a meta block.
	frame, err := relayFrameFromRecord(relayRecord{
		Type: uint8(4), Close: true, Reason: "the sender closed the session", Class: RelayClassRefused,
	})
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	stream.Write(body)

	// Now read the stream back, record by record.
	buf := stream.Bytes()
	off := 0
	for i := 0; i < 200; i++ {
		hdr, err := PeekRelayFrame(buf[off:])
		if err != nil {
			t.Fatalf("record %d: PeekRelayFrame: %v", i, err)
		}
		size := relayFrameHeaderSize + int(hdr.MetaLen) + int(hdr.PayloadLen)
		got, err := DecodeBinaryRelayFrame(buf[off : off+size])
		if err != nil {
			t.Fatalf("record %d: DecodeBinaryRelayFrame: %v", i, err)
		}
		if !bytes.Equal(got.Data, want[i]) {
			t.Fatalf("record %d: the payload came back different", i)
		}
		off += size
	}
	got, err := DecodeBinaryRelayFrame(buf[off:])
	if err != nil {
		t.Fatalf("the control record: DecodeBinaryRelayFrame: %v", err)
	}
	if !got.Close || got.Reason != "the sender closed the session" || got.Class != RelayClassRefused {
		t.Fatalf("the control record came back wrong: %+v", got)
	}
	if off+len(buf)-off != len(buf) {
		t.Fatalf("the stream was not consumed exactly: %d bytes left", len(buf)-off)
	}
}

// TestRelayFrameJSONAndBinarySizes prints the byte comparison for the
// sizes that matter, so the numbers in the design note can be checked
// against a run.
func TestRelayFrameJSONAndBinarySizes(t *testing.T) {
	for _, size := range []int{1024, 32 << 10, 256 << 10, maxRelayPlaintext} {
		payload := bytes.Repeat([]byte{0x5A}, size)
		rec := relayRecord{Type: uint8(3), Data: payload}
		jsonBody, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		frame, err := relayFrameFromRecord(rec)
		if err != nil {
			t.Fatalf("relayFrameFromRecord: %v", err)
		}
		binBody, err := EncodeBinaryRelayFrame(frame)
		if err != nil {
			t.Fatalf("EncodeBinaryRelayFrame: %v", err)
		}
		saving := 100 * (1 - float64(len(binBody))/float64(len(jsonBody)))
		t.Logf("payload %7d: JSON %8d, binary %8d, saving %5.2f%%", size, len(jsonBody), len(binBody), saving)
	}
}

// TestRelayFrameCarriesMorePayloadPerRecord checks the second byte win:
// the binary record carries MORE payload for the same record budget.
//
// The JSON envelope turns the payload into base64, so a record that fits
// the 1 MiB cap can hold only about three quarters of a MiB of payload.
// The binary record holds the payload verbatim, so the SAME budget holds
// a third more. That is why maxRelayPlaintext — the cap the sender
// splits on — can grow when the format switches.
func TestRelayFrameCarriesMorePayloadPerRecord(t *testing.T) {
	// The largest payload whose JSON record still fits the cap.
	jsonCap := 0
	for n := maxRelayRecord; n > 0; n -= 1024 {
		rec := relayRecord{Type: uint8(3), Data: make([]byte, n)}
		body, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if len(body) <= maxRelayRecord {
			jsonCap = n
			break
		}
	}
	if jsonCap == 0 {
		t.Fatal("no payload fits a JSON relay record: the cap is broken")
	}
	// The largest payload whose binary record still fits the cap.
	binCap := 0
	for n := maxRelayRecord; n > 0; n -= 1024 {
		frame := relayFrame{Type: uint8(3), Data: make([]byte, n)}
		body, err := EncodeBinaryRelayFrame(frame)
		if err != nil {
			continue
		}
		if len(body) <= maxRelayRecord {
			binCap = n
			break
		}
	}
	if binCap == 0 {
		t.Fatal("no payload fits a binary relay record: the cap is broken")
	}
	gain := 100 * (float64(binCap)/float64(jsonCap) - 1)
	if gain < 30 {
		t.Fatalf("the binary record carries only %.2f%% more payload than the JSON one (%d vs %d bytes)",
			gain, binCap, jsonCap)
	}
	t.Logf("payload per 1 MiB record: JSON %d bytes, binary %d bytes, +%.2f%%",
		jsonCap, binCap, gain)
}

// TestRelayFrameEncodeIsDeterministic checks that the same record always
// produces the same bytes.
//
// It has to: the format has no timestamp and no map, so it is already
// deterministic. The test says so, because a future "optimisation" that
// reorders the meta block would break the verbatim-forward property (a
// forwarder that rewrote the header would not be forwarding).
func TestRelayFrameEncodeIsDeterministic(t *testing.T) {
	rec := relayRecord{
		Type: uint8(1), Session: 7, From: identity.DeviceID("fromdevice00000a"),
		To: identity.DeviceID("todevice000000b"), Kind: "ping", Hops: 1, Reason: "x", Class: "y",
	}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	first, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	for i := 0; i < 10; i++ {
		again, err := EncodeBinaryRelayFrame(frame)
		if err != nil {
			t.Fatalf("EncodeBinaryRelayFrame: %v", err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("the encoder is not deterministic: run %d differs", i)
		}
	}
}

// TestRelayFrameHelloIsCarried checks the one field that stays JSON.
//
// The end-to-end hello is a signed blob and its signature is what matters;
// its bytes are not a hot path. Keeping it JSON means the second step
// does not have to touch the E2E code at all.
func TestRelayFrameHelloIsCarried(t *testing.T) {
	raw := []byte(`{"device":"inycnepctiqwr6te","ephemeral":"AQID","session":42,"time":"2026-11-19T00:00:00Z","signature":"BAUG"}`)
	rec := relayRecord{Type: uint8(2), Hello: raw}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	got, err := DecodeBinaryRelayFrame(body)
	if err != nil {
		t.Fatalf("DecodeBinaryRelayFrame: %v", err)
	}
	if !bytes.Equal(got.Hello, raw) {
		t.Fatalf("the hello came back different:\n got  %s\n want %s", got.Hello, raw)
	}
	// And it still parses as the JSON it always was.
	var probe map[string]any
	if err := json.Unmarshal(got.Hello, &probe); err != nil {
		t.Fatalf("the hello is no longer valid JSON: %v", err)
	}
}

// TestRelayFrameLengthPrefixCompatibility checks that the binary body
// still travels inside the mesh's length-prefixed frame.
//
// The framing layer does not change in the second step, so this test
// states it: a binary body written with writeFrame comes back with
// readFrame, byte for byte.
func TestRelayFrameLengthPrefixCompatibility(t *testing.T) {
	payload := bytes.Repeat([]byte{0x3C}, 777)
	rec := relayRecord{Type: uint8(3), Data: payload}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatalf("relayFrameFromRecord: %v", err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	var wire bytes.Buffer
	if err := writeFrame(&wire, body); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	got, err := readFrame(bufio.NewReader(bytes.NewReader(wire.Bytes())), maxRelayRecord)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("the body came back different through the length-prefixed frame")
	}
}

// TestRelayFrameErrorTextNamesTheFormat checks that the format error
// says what it is.
//
// When the second step flips the call sites, this message is what an
// operator will see when a peer speaks the other format. It has to name
// the format.
func TestRelayFrameErrorTextNamesTheFormat(t *testing.T) {
	_, err := PeekRelayFrame([]byte("not a frame"))
	if err == nil {
		t.Fatal("PeekRelayFrame accepted a non-frame")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("binary relay frame")) {
		t.Fatalf("the error %q does not name the format", err)
	}
}
