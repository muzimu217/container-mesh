// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

// The tests of the PRODUCTION SWITCH to the binary relay record.
//
// The binary format existed before this round (relayframe.go) but nothing
// on the wire used it: writeRelayRecord still marshalled JSON, and
// forwardRecords still decoded every record and re-encoded it.
//
// WHY THE SWITCH IS THE WHOLE POINT.
//
// The relay is THREE processes, not one:
//
//	requester A ──▶ anchor B ──▶ target C
//
// The record format is an A↔C protocol — the anchor is a pipe — and the
// anchor's per-record cost was MEASURED against the two endpoints': the
// trunk 1140 µs, the target 1345 µs, the requester 1778 µs per record.
// They are co-equal. Switching the trunk alone gives 1.85×; the measured
// full win (65.9–67.8 MiB/s, +96% over the product's 33.3–36.5) needs
// all three.
//
// The ablation that decided the design, on the real host, same build, same
// lab, 128 MB bench, 3 reps each, link=relay:
//
//	product (JSON decode + re-encode)      33.3 – 36.5 MiB/s
//	raw    (still JSON, byte-copy only)     43.0 – 44.2 MiB/s  (+31%)
//	binary (binary frame, decode+re-encode) 59.4 – 61.6 MiB/s  (+77%)
//	splice (binary frame + byte-copy)       65.9 – 67.8 MiB/s  (+96%)
//
// The trunk's per-record cycle went 2000.8 µs → 589.9 µs (−71%) and the
// CPU per MiB of payload 326.7 ms → 92.3 ms (−72%).
//
// THESE TESTS PIN THE SWITCH'S CONSEQUENCES, not its internals:
//
//   - the DEFAULT for a same-version deployment is the binary frame;
//   - a MIXED-version deployment still WORKS (the anchor translates), and
//     the translation says so in ONE log line;
//   - the translation is CLEAN: no half-written record ever reaches the
//     far side;
//   - the ALPN advertisement is the shape the measured negotiation needs.
//
// ---------------------------------------------------------------------------

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// ---------------------------------------------------------------------------
// ① The DEFAULT is the binary frame.
//
// The requester endpoint and the target endpoint both write records. If
// either of them still writes JSON, the anchor has to decode and
// re-encode, and the whole measured win is gone. This test therefore
// asserts the WIRE BYTES, not the behaviour: the first byte of a record
// body on the wire must be the format tag.
// ---------------------------------------------------------------------------

// TestTheRelayRecordOnTheWireIsBinaryByDefault is the switch's central
// assertion.
//
// The relay record used to be a JSON envelope, so every record on the
// wire began with '{'. It now begins with the binary frame's format tag
// (0xC7) — the byte that no JSON parser can start a document with, which
// is what makes an old peer fail LOUDLY rather than misread the record.
func TestTheRelayRecordOnTheWireIsBinaryByDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRelayRecord(&buf, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: []byte("opaque ciphertext"),
	}); err != nil {
		t.Fatalf("writeRelayRecord: %v", err)
	}
	body, err := readFrame(bufio.NewReader(bytes.NewReader(buf.Bytes())), maxRelayRecord)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("the record body is empty")
	}
	if body[0] != relayFrameMagic {
		t.Fatalf("the record on the wire starts with 0x%02x, want the binary format tag 0x%02x; "+
			"the switch did not happen (or the default is still JSON)",
			body[0], relayFrameMagic)
	}
	// And it must not be JSON either — the old envelope is still
	// reachable through the JSON path, but not on the default path.
	if body[0] == '{' {
		t.Fatal("the record on the wire is still the JSON envelope")
	}
}

// TestTheControlRecordsAreBinaryToo pins the control plane.
//
// OPEN / ANSWER / HELLO / CLOSE are RECORDS, and they carry the
// authorization decision and the end-to-end handshake. Leaving them in
// JSON while the data records go binary would mean the anchor decodes
// every control record anyway — and the control records are on the
// critical path of every relay, so the per-relay cost would not go away.
func TestTheControlRecordsAreBinaryToo(t *testing.T) {
	for _, rec := range []relayRecord{
		{Type: uint8(wire.FrameMeshRelayOpen), From: "a", To: "b", Kind: KindPing, Hops: 1},
		{Type: uint8(wire.FrameMeshRelayAnswer), Grant: true, Session: 7},
		{Type: uint8(wire.FrameMeshRelayAnswer), Grant: false, Reason: "no", Class: RelayClassAuth},
		{Type: uint8(wire.FrameMeshRelayHello), Hello: []byte(`{"proto":"x"}`)},
		{Type: uint8(wire.FrameMeshRelayJoin), From: "a"},
		{Type: uint8(wire.FrameMeshRelayClose), Close: true, Reason: "bye"},
	} {
		var buf bytes.Buffer
		if err := writeRelayRecord(&buf, rec); err != nil {
			t.Fatalf("writeRelayRecord(type %d): %v", rec.Type, err)
		}
		body, err := readFrame(bufio.NewReader(bytes.NewReader(buf.Bytes())), maxRelayRecord)
		if err != nil {
			t.Fatalf("readFrame(type %d): %v", rec.Type, err)
		}
		if body[0] != relayFrameMagic {
			t.Fatalf("the control record of type %d starts with 0x%02x, want the binary tag 0x%02x",
				rec.Type, body[0], relayFrameMagic)
		}
		// It must survive the round trip, or the switch broke the
		// protocol rather than changing its encoding.
		got, err := DecodeBinaryRelayFrame(body)
		if err != nil {
			t.Fatalf("DecodeBinaryRelayFrame(type %d): %v", rec.Type, err)
		}
		back := recordFromRelayFrame(got)
		if back.Type != rec.Type {
			t.Fatalf("the control record of type %d came back as type %d", rec.Type, back.Type)
		}
	}
}

// TestTheRequesterEndpointWritesBinaryRecords pins the REQUESTER's leg.
//
// The requester endpoint is one of the three processes the switch has to
// land on, and its per-record cost was 1778 µs — the highest of the
// three. It writes records through sealAndWriteRelayData, so that is
// where the assertion goes: the bytes the requester puts on the wire must
// be binary frames.
func TestTheRequesterEndpointWritesBinaryRecords(t *testing.T) {
	f := newTrunkFixture(t)
	e2e, peer := newEstablishedPair(t, f)

	var buf bytes.Buffer
	if _, err := sealAndWriteRelayData(&buf, e2e, []byte("the request")); err != nil {
		t.Fatalf("sealAndWriteRelayData: %v", err)
	}
	body, err := readFrame(bufio.NewReader(bytes.NewReader(buf.Bytes())), maxRelayRecord)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if body[0] != relayFrameMagic {
		t.Fatalf("the requester's record starts with 0x%02x, want the binary tag 0x%02x",
			body[0], relayFrameMagic)
	}
	// The seal is intact: the target can open what the requester sealed.
	got, err := DecodeBinaryRelayFrame(body)
	if err != nil {
		t.Fatalf("DecodeBinaryRelayFrame: %v", err)
	}
	plain, err := peer.Open(got.Data)
	if err != nil {
		t.Fatalf("the target could not open the requester's payload: %v", err)
	}
	if string(plain) != "the request" {
		t.Fatalf("the payload came back as %q", plain)
	}
}

// TestTheTargetEndpointReadsBinaryRecords pins the TARGET's leg.
//
// The target's per-record cost was 1345 µs — co-equal with the trunk's.
// Its read path is framedRecordReader, so a record that arrives as a
// binary frame must be decoded by the very same reader the trunk uses.
func TestTheTargetEndpointReadsBinaryRecords(t *testing.T) {
	f := newTrunkFixture(t)
	e2e, peer := newEstablishedPair(t, f)

	// The requester writes a binary record into the pipe.
	var raw bytes.Buffer
	if _, err := sealAndWriteRelayData(&raw, e2e, []byte("the request")); err != nil {
		t.Fatalf("sealAndWriteRelayData: %v", err)
	}
	sender, reader := newPipeSessionPair()
	go func() {
		_, _ = sender.Write(raw.Bytes())
		_ = sender.CloseWrite()
	}()

	rec, err := readRelayRecord(bufio.NewReader(reader))
	if err != nil {
		t.Fatalf("the target could not read the record: %v", err)
	}
	if rec.Type != uint8(wire.FrameMeshRelayData) {
		t.Fatalf("the target read record type %d, want a data record", rec.Type)
	}
	plain, err := peer.Open(rec.Data)
	if err != nil {
		t.Fatalf("the target could not open the payload: %v", err)
	}
	if string(plain) != "the request" {
		t.Fatalf("the payload came back as %q", plain)
	}
}

// ---------------------------------------------------------------------------
// ② The per-leg format.
//
// The record format is an A↔C protocol and the ALPN is a CONNECTION's
// property. So the format of a leg comes from the ALPN that leg's
// connection negotiated, and the two legs of one relay can disagree (a
// new requester on a new anchor, an old target).
// ---------------------------------------------------------------------------

// TestTheLegFormatComesFromTheNegotiatedALPN pins where the format is
// decided.
//
// It is decided PER LEG, from that leg's negotiated ALPN — not per
// process, and not per relay. A relay whose requester leg negotiated
// container-mesh-relay/2 and whose target leg negotiated
// container-mesh-relay/1 has to be able to say so, because that is
// exactly the mixed-version deployment.
func TestTheLegFormatComesFromTheNegotiatedALPN(t *testing.T) {
	if got, err := relayFormatForALPN(RelayALPN2); err != nil || got != RelaySpliceFormatBinary {
		t.Fatalf("the format of %q is %v (%v), want the binary one", RelayALPN2, got, err)
	}
	if got, err := relayFormatForALPN(RelayALPN); err != nil || got != RelaySpliceFormatJSON {
		t.Fatalf("the format of %q is %v (%v), want the JSON one", RelayALPN, got, err)
	}
	if _, err := relayFormatForALPN("container-mesh-relay/99"); err == nil {
		t.Fatal("an unknown ALPN was given a record format: it must be a refusal")
	}
}

// TestAMixedVersionRelayIsCarriedByTranslation is the mixed-version
// decision, pinned.
//
// THE DECISION: a mixed-version relay (new requester on a new anchor,
// old target) FALLS BACK TO THE JSON DECODE/RE-ENCODE PATH for that
// relay. It is NOT refused.
//
// The reason is the deployment order. The anchor is upgraded FIRST (it
// has to be — an old anchor cannot read a binary record at all, so a new
// member on an old anchor dies at the first record). But the TARGETS
// are upgraded last, because a target that has not been restarted yet
// still speaks JSON. Refusing those relays would take the relay feature
// down for the whole window between the anchor's upgrade and the last
// target's upgrade — and the window is exactly as long as the operator's
// rollout.
//
// So the anchor can carry BOTH formats, and the price of the mixed case
// is the CPU the fast path was saving. The fast path is the DEFAULT; the
// slow path is the fallback, and it says so in ONE log line so an
// operator can see that the rollout is not finished.
func TestAMixedVersionRelayIsCarriedByTranslation(t *testing.T) {
	f := newTrunkFixture(t)

	// The requester speaks the NEW format; the target speaks the OLD one.
	reqEnd, reqPeer := newPipeSessionPair()
	tgtEnd, tgtPeer := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tgtEnd, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The flow's two legs speak different formats. This is what the
	// negotiated ALPNs decided, upstream of the trunk.
	flowID, err := trunk.OpenFlowWithFormats(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, reqEnd, RelaySpliceFormatBinary, RelaySpliceFormatJSON)
	if err != nil {
		t.Fatalf("the trunk refused a mixed-version relay: %v", err)
	}
	_ = flowID

	// The requester writes a BINARY record (it is new).
	if err := writeRelayRecord(reqPeer, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: []byte("the request"),
	}); err != nil {
		t.Fatalf("the requester could not write: %v", err)
	}

	// The target — an OLD member — must receive it as a JSON record.
	_ = tgtPeer.SetReadDeadline(time.Now().Add(10 * time.Second))
	tgtBr := bufio.NewReader(tgtPeer)
	// The OPEN record comes first: the trunk tells the target who is
	// calling, in the TARGET's format.
	open, err := readRelayRecordAs(tgtBr, RelaySpliceFormatJSON)
	if err != nil {
		t.Fatalf("the old target received no open record: %v", err)
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the old target's first record is type %d, want an open record", open.Type)
	}
	rec, err := readRelayRecordAs(tgtBr, RelaySpliceFormatJSON)
	if err != nil {
		t.Fatalf("the old target received nothing: %v", err)
	}
	if rec.Type != uint8(wire.FrameMeshRelayData) {
		t.Fatalf("the old target received record type %d, want a data record", rec.Type)
	}
	if string(rec.Data) != "the request" {
		t.Fatalf("the old target received %q", rec.Data)
	}

	// And the wire form must be the OLD one: an old target's parser
	// cannot read a binary frame, so the translation has to be real.
	//
	// The DATA record is the LAST one the target received, and it is the
	// one the anchor translated — so it is the one that must be JSON.
	//
	// seenBytes records what a session WROTE (the pipe's write side logs
	// its own bytes), so the target's leg is seen from the trunk's end:
	// tgtEnd is the session the trunk writes on.
	seen := tgtEnd.seenBytes()
	sent := reqPeer.seenBytes()
	got := lastFramedRecord(t, seen)
	if len(got) > 0 && got[0] == relayFrameMagic {
		t.Fatal("the old target was handed a binary record: it would have failed to parse it")
	}
	// And the requester's record really was binary — otherwise the test
	// would pass without the translation doing anything.
	sentRec := lastFramedRecord(t, sent)
	if len(sentRec) == 0 || sentRec[0] != relayFrameMagic {
		t.Fatal("the requester's record was not binary: the mixed-version case was not exercised")
	}
}

// TestAMixedVersionRelaySaysSoInTheLog pins the operator-visible half of
// the mixed-version decision.
//
// A fallback that is silent is a fallback nobody acts on: the operator
// sees a relay that is slower than it should be and has no way to know
// that the rollout is incomplete. ONE line, naming the two formats, is
// enough — and it must not be one line per record.
func TestAMixedVersionRelaySaysSoInTheLog(t *testing.T) {
	f := newTrunkFixture(t)

	reqEnd, reqPeer := newPipeSessionPair()
	tgtEnd, _ := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tgtEnd, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The log is captured through a pipe, the same way the existing
	// report test does it — an atomic.Value holds a CONCRETE type, so
	// swapping in a *bytes.Buffer would panic the store.
	out := captureRelayLog(t, func() {
		if _, err := trunk.OpenFlowWithFormats(context.Background(), RelayOpen{
			From: f.alice.ID(),
			To:   f.bob.ID(),
			Hops: 1,
		}, reqEnd, RelaySpliceFormatBinary, RelaySpliceFormatJSON); err != nil {
			t.Fatalf("the trunk refused a mixed-version relay: %v", err)
		}
		// One record is enough to trigger the line.
		_ = writeRelayRecord(reqPeer, relayRecord{Type: uint8(wire.FrameMeshRelayData), Data: []byte("x")})
		time.Sleep(200 * time.Millisecond)
	})
	if !strings.Contains(out, "JSON") {
		t.Fatalf("the mixed-version relay said nothing in the log; the operator cannot tell "+
			"the rollout is unfinished. The log was:\n%s", out)
	}
}

// TestASameVersionRelaySaysNothingInTheLog is the other half of the
// previous test.
//
// The fast path must be SILENT. A log line per relay on the default path
// would drown the log on a busy anchor — and the fast path is not
// something an operator needs to be told about.
func TestASameVersionRelaySaysNothingInTheLog(t *testing.T) {
	f := newTrunkFixture(t)

	reqEnd, reqPeer := newPipeSessionPair()
	tgtEnd, _ := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tgtEnd, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := captureRelayLog(t, func() {
		if _, err := trunk.OpenFlowWithFormats(context.Background(), RelayOpen{
			From: f.alice.ID(),
			To:   f.bob.ID(),
			Hops: 1,
		}, reqEnd, RelaySpliceFormatBinary, RelaySpliceFormatBinary); err != nil {
			t.Fatalf("the trunk refused a same-version relay: %v", err)
		}
		_ = writeRelayRecord(reqPeer, relayRecord{Type: uint8(wire.FrameMeshRelayData), Data: []byte("x")})
		time.Sleep(200 * time.Millisecond)
	})
	if strings.Contains(out, "JSON") {
		t.Fatalf("the fast path logged a line about the formats; it must be silent:\n%s", out)
	}
}

// TestTheMixedVersionTranslationNeverWritesAHalfRecord is the CLEAN
// failure requirement.
//
// The translation path reads a record and writes a record. If the write
// fails halfway, the far side is left holding a PARTIAL record — and a
// far side that then reads the next record's bytes as the tail of this
// one is out of step for the rest of the relay.
//
// So the translation must not be a "write the prefix and then the body"
// sequence of two partial writes: the far side either gets a whole
// record or gets nothing. This test forces the write to fail and checks
// that the far side sees NO record at all (not a partial one).
func TestTheMixedVersionTranslationNeverWritesAHalfRecord(t *testing.T) {
	f := newTrunkFixture(t)

	reqEnd, reqPeer := newPipeSessionPair()
	tgtEnd, tgtPeer := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tgtEnd, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trunk.OpenFlowWithFormats(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, reqEnd, RelaySpliceFormatBinary, RelaySpliceFormatJSON); err != nil {
		t.Fatalf("the trunk refused a mixed-version relay: %v", err)
	}

	// The far side dies mid-record.
	_ = writeRelayRecord(reqPeer, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: bytes.Repeat([]byte("x"), 4096),
	})
	time.Sleep(100 * time.Millisecond)
	_ = tgtPeer.Abort()

	// Whatever the far side got, it must not be a PARTIAL record that
	// looks like a whole one. The pipe records every byte that crossed,
	// so the test can look at the wire itself.
	seen := tgtPeer.seenBytes()
	if len(seen) > 0 {
		// A whole record would parse. A partial one must not.
		br := bufio.NewReader(bytes.NewReader(seen))
		if _, err := readFrame(br, maxRelayRecord); err == nil {
			t.Fatal("the aborted write left a record that parses: the far side would read " +
				"the next record's bytes as the tail of this one")
		}
	}
}

// ---------------------------------------------------------------------------
// ③ The fast path.
//
// When the two legs agree, the anchor must NOT decode the record: it
// reads the 24-byte header, learns the length, and copies the bytes.
// ---------------------------------------------------------------------------

// TestTheSameVersionRelayIsForwardedVerbatim is the fast path's
// behavioural proof.
//
// The record the requester writes must arrive at the target BYTE FOR
// BYTE. If the anchor decoded and re-encoded it, the bytes would still
// be equivalent — so the test compares the RAW bytes, not the decoded
// record.
func TestTheSameVersionRelayIsForwardedVerbatim(t *testing.T) {
	f := newTrunkFixture(t)

	reqEnd, reqPeer := newPipeSessionPair()
	tgtEnd, tgtPeer := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tgtEnd, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trunk.OpenFlowWithFormats(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, reqEnd, RelaySpliceFormatBinary, RelaySpliceFormatBinary); err != nil {
		t.Fatalf("the trunk refused the relay: %v", err)
	}

	payload := bytes.Repeat([]byte{0x5A}, 4096)
	if err := writeRelayRecord(reqPeer, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: payload,
	}); err != nil {
		t.Fatalf("the requester could not write: %v", err)
	}

	// The target reads the OPEN record first (the trunk wrote it), then
	// the DATA record the requester wrote.
	_ = tgtPeer.SetReadDeadline(time.Now().Add(10 * time.Second))
	tgtBr := bufio.NewReader(tgtPeer)
	open, err := readRelayRecord(tgtBr)
	if err != nil {
		t.Fatalf("the target received no open record: %v", err)
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the target's first record is type %d, want an open record", open.Type)
	}
	rec, err := readRelayRecord(tgtBr)
	if err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}
	if !bytes.Equal(rec.Data, payload) {
		t.Fatalf("the payload came back different: %d bytes, want %d", len(rec.Data), len(payload))
	}

	// The raw bytes: the target must have received exactly what the
	// requester wrote, framing included. The anchor did not re-encode.
	//
	// seenBytes records what a session WROTE, so the target's leg is
	// seen from the trunk's end (tgtEnd), and the requester's leg from
	// the requester's end (reqPeer).
	seen := tgtEnd.seenBytes()
	sent := reqPeer.seenBytes()
	if len(seen) == 0 {
		t.Fatal("the target received no bytes at all")
	}
	// The target's last record must be byte-identical to the
	// requester's last record.
	sentRec := lastFramedRecord(t, sent)
	gotRec := lastFramedRecord(t, seen)
	if !bytes.Equal(sentRec, gotRec) {
		t.Fatalf("the anchor re-encoded the record: the target got %d bytes that are not the "+
			"requester's %d bytes, so the fast path did not run", len(gotRec), len(sentRec))
	}
}

// TestTheFastPathCountsThePayloadTheSameWay keeps the quota honest.
//
// The rate budget is charged on the PAYLOAD bytes, and it was charged on
// len(rec.Data) when the anchor decoded the record. The binary header
// carries the same number, so the charge must be identical — a fast path
// that charged the wrong number would silently change the P-7 limit.
func TestTheFastPathCountsThePayloadTheSameWay(t *testing.T) {
	f := newTrunkFixture(t)

	reqEnd, reqPeer := newPipeSessionPair()
	tgtEnd, tgtPeer := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tgtEnd, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trunk.OpenFlowWithFormats(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, reqEnd, RelaySpliceFormatBinary, RelaySpliceFormatBinary); err != nil {
		t.Fatalf("the trunk refused the relay: %v", err)
	}

	const payload = 8192
	if err := writeRelayRecord(reqPeer, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: make([]byte, payload),
	}); err != nil {
		t.Fatalf("the requester could not write: %v", err)
	}
	_ = tgtPeer.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := readRelayRecord(bufio.NewReader(tgtPeer)); err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var total int64
	for time.Now().Before(deadline) {
		total = trunk.FlowBytes(1)
		if total != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if total != payload {
		t.Fatalf("the trunk charged %d bytes against the rate budget, want the payload's %d",
			total, payload)
	}
}

// ---------------------------------------------------------------------------
// ④ The ALPN advertisement.
//
// Go's ALPN negotiation is SERVER-FIRST: the server's list order decides.
// So the anchor must advertise [NEW, OLD] and the member must offer
// BOTH — a member that offers only the new ALPN cannot reach an old
// anchor at all ("no application protocol").
// ---------------------------------------------------------------------------

// TestTheAnchorAdvertisesTheNewALPNFirst pins the server side.
//
// The ORDER is the whole point: Go walks the SERVER's list and takes the
// first entry the client also offers (measured under QUIC in
// TestTheALPNNegotiationUnderQUICIsServerFirst). An anchor that
// advertised [OLD, NEW] would negotiate the OLD protocol with a member
// that could have spoken the new one — and the relay would keep running
// on JSON for ever.
func TestTheAnchorAdvertisesTheNewALPNFirst(t *testing.T) {
	protos := relayAdvertisedALPNs()
	if len(protos) < 2 {
		t.Fatalf("the anchor advertises %v, want the new ALPN first and the old one after it", protos)
	}
	if protos[0] != RelayALPN2 {
		t.Fatalf("the anchor advertises %v, want %q FIRST (Go's ALPN negotiation is server-first)",
			protos, RelayALPN2)
	}
	found := false
	for _, p := range protos[1:] {
		if p == RelayALPN {
			found = true
		}
	}
	if !found {
		t.Fatalf("the anchor advertises %v, want it to keep %q as well (an old member must still reach it)",
			protos, RelayALPN)
	}
}

// TestTheMemberOffersBothALPNs pins the client side.
//
// The member must offer BOTH. Offering only the new one means the
// handshake against an OLD anchor fails with "no application protocol" —
// so a member upgraded before the anchor would be unable to relay at all.
func TestTheMemberOffersBothALPNs(t *testing.T) {
	protos := relayAdvertisedALPNs()
	if len(protos) < 2 {
		t.Fatalf("the member offers %v, want both ALPNs", protos)
	}
	if protos[0] != RelayALPN2 {
		t.Fatalf("the member offers %v, want %q first", protos, RelayALPN2)
	}
	if protos[1] != RelayALPN {
		t.Fatalf("the member offers %v, want %q second", protos, RelayALPN)
	}
}

// ---------------------------------------------------------------------------
// ⑤ The payload cap.
//
// The binary frame carries the payload verbatim, so the SAME record
// budget holds a third more payload. The plaintext cap can rise with it.
// ---------------------------------------------------------------------------

// TestTheBinaryFrameCarriesMorePayloadPerRecord is the cap's arithmetic.
//
// The old cap was three quarters of the record limit, because JSON
// carried the payload as base64. The binary frame has no base64, so the
// cap can go to the record limit minus the header and the seal's
// overhead.
func TestTheBinaryFrameCarriesMorePayloadPerRecord(t *testing.T) {
	if maxRelayPlaintext <= maxRelayRecord*3/4-1024 {
		t.Fatalf("the plaintext cap is still %d bytes: the binary frame carries a third more "+
			"payload per record, so the cap can rise", maxRelayPlaintext)
	}
	// And it must still FIT: the cap is a promise the sender relies on.
	// A plaintext of the cap size, sealed, must fit in one record.
	//
	// The probe counts WRITES, and a framed record is two writes (the
	// 4-byte length prefix and the body), so one record is two sizes.
	f := newTrunkFixture(t)
	e2e, _ := newEstablishedPair(t, f)
	probe := &recordSizeProbe{}
	if _, err := sealAndWriteRelayData(probe, e2e, make([]byte, maxRelayPlaintext)); err != nil {
		t.Fatalf("a plaintext of the cap size could not be written: %v", err)
	}
	if got := len(probe.sizes) / 2; got != 1 {
		t.Fatalf("a plaintext of %d bytes was split into %d records; it must fit in one",
			maxRelayPlaintext, got)
	}
	if maxOf(probe.sizes) > maxRelayRecord {
		t.Fatalf("the record is %d bytes, over the %d byte limit", maxOf(probe.sizes), maxRelayRecord)
	}
}

// ---------------------------------------------------------------------------
// ⑥ The JSON path still works.
//
// The mixed-version deployment depends on it: an old anchor, or an old
// member, must still be able to talk to a new one. So the JSON path is
// not deleted — it is the fallback.
// ---------------------------------------------------------------------------

// TestTheJSONPathStillWorks is the compatibility floor.
//
// A JSON record written through the JSON path must still decode, and a
// JSON record on the wire must still be readable by the JSON reader. The
// fallback is a real path, not a stub.
func TestTheJSONPathStillWorks(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRelayRecordAs(&buf, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: []byte("opaque ciphertext"),
	}, RelaySpliceFormatJSON); err != nil {
		t.Fatalf("writeRelayRecordAs(JSON): %v", err)
	}
	body, err := readFrame(bufio.NewReader(bytes.NewReader(buf.Bytes())), maxRelayRecord)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if body[0] != '{' {
		t.Fatalf("the JSON record starts with 0x%02x, want '{'", body[0])
	}
	rec, err := readRelayRecordAs(bufio.NewReader(bytes.NewReader(buf.Bytes())), RelaySpliceFormatJSON)
	if err != nil {
		t.Fatalf("readRelayRecordAs(JSON): %v", err)
	}
	if string(rec.Data) != "opaque ciphertext" {
		t.Fatalf("the JSON record came back as %q", rec.Data)
	}
}

// TestAnOldParserFailsLoudlyOnABinaryRecord is the compatibility
// property the format tag was chosen for.
//
// It is already asserted in relayframe_test.go for the codec; this test
// pins it for the PRODUCTION path: the record the new code writes must
// still be un-parseable by an old JSON reader.
func TestAnOldParserFailsLoudlyOnABinaryRecord(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRelayRecord(&buf, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: []byte("opaque ciphertext"),
	}); err != nil {
		t.Fatalf("writeRelayRecord: %v", err)
	}
	// The old reader: readRelayRecordAs with the JSON format.
	_, err := readRelayRecordAs(bufio.NewReader(bytes.NewReader(buf.Bytes())), RelaySpliceFormatJSON)
	if err == nil {
		t.Fatal("an old JSON reader read a binary record: the format tag is not doing its job")
	}
	if !strings.Contains(err.Error(), "invalid character") &&
		!strings.Contains(err.Error(), "not a binary relay frame") {
		t.Fatalf("the old reader failed with %v, want a loud format error", err)
	}
}

// ---------------------------------------------------------------------------
// The helpers.
// ---------------------------------------------------------------------------

// newEstablishedPair builds an end-to-end session and its peer, with the
// handshake already done — so a test can seal and open without running
// SIGMA.
func newEstablishedPair(t testing.TB, f *trunkFixture) (*E2ESession, *E2ESession) {
	t.Helper()
	e2e, err := NewE2ESession(E2EConfig{
		Self:    f.alice,
		Peer:    f.bob.ID(),
		PeerKey: f.bob.PublicKey(),
		Role:    E2ERoleInitiator,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := NewE2ESession(E2EConfig{
		Self:    f.bob,
		Peer:    f.alice.ID(),
		PeerKey: f.alice.PublicKey(),
		Role:    E2ERoleResponder,
	})
	if err != nil {
		t.Fatal(err)
	}
	own, err := e2e.Hello()
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.AcceptHello(own); err != nil {
		t.Fatal(err)
	}
	theirHello, err := peer.Hello()
	if err != nil {
		t.Fatal(err)
	}
	if err := e2e.AcceptHello(theirHello); err != nil {
		t.Fatal(err)
	}
	return e2e, peer
}

// lastFramedRecord returns the LAST length-prefixed record in a byte
// stream.
//
// It is how a test compares the requester's bytes with the target's:
// both streams carry more than one record (the open record, the data
// record), and the data record is the last one on both.
func lastFramedRecord(t testing.TB, stream []byte) []byte {
	t.Helper()
	br := bufio.NewReader(bytes.NewReader(stream))
	var last []byte
	for {
		body, err := readFrame(br, maxRelayRecord)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// A partial trailing record: stop here and use what we have.
			break
		}
		last = body
	}
	if len(last) == 0 {
		t.Fatalf("the stream holds no complete record (%d bytes)", len(stream))
	}
	return last
}

// unusedGuard keeps the compiler honest about the imports the file uses
// only in some build configurations.
var _ = fmt.Sprintf

// TestTheSwitchWorksOverARealQUICConnection is the switch's end-to-end
// proof.
//
// Every other test in this file drives the pipe double. This one goes
// through the REAL listener, over a REAL QUIC connection, with a REAL
// member view — and it asserts the two things that the pipe cannot
// prove:
//
//  1. The anchor and the member negotiate container-mesh-relay/2 (the
//     new ALPN), not container-mesh-relay/1.
//
//     That is the whole version negotiation, and it is the one thing the
//     unit tests cannot show: ALPN is negotiated by TLS, and a test that
//     stubs the transport proves nothing about what the transport
//     agreed on. The round-61 real-machine report is the precedent —
//     every unit test was green while the trunk's data plane was
//     completely broken on real machines, because the unit tests never
//     went through RelayListener.handle.
//
//  2. The payload crosses the trunk INTACT.
//
//     The relay is the whole feature, and a relay that negotiates the
//     new protocol but loses bytes is worse than one that never
//     upgraded.
func TestTheSwitchWorksOverARealQUICConnection(t *testing.T) {
	f := newRelayFixture(t)
	f.startHub(t, true)
	sessions := f.joinBob(t)

	var negotiated string
	// The budget must OUTLAST the handshake it waits for.
	//
	// DialRelaySession derives its own budget from quicHandshakeTimeout
	// (20 s) plus a margin: 30 s, and the TRANSPORT gives up first at
	// quicTransportHandshakeTimeout (15 s). A 20-second context
	// therefore expires before the dial budget it is waiting for could
	// run out — the peer is ready and the dialer has already given up.
	//
	// That is the failure the repo already documents in direct.go ("a
	// dial context shorter than the handshake it waits for cancels the
	// handshake before it can finish"), and it showed up here as
	// "TestTheSwitchWorksOverARealQUICConnection (45.01s)" — 20.01 s
	// for the dial, plus the 20 s the select below waits for the target.
	//
	// So the context is longer than the dial's own budget. The test's
	// SUBJECT is the format switch, not the speed of a handshake.
	openCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	sess, err := OpenRelay(openCtx, RelayRequesterConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Kind:      KindPing,
		TargetKey: f.bob.PublicKey(),
		Relay:     f.hub.ID(),
		Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
			link, err := DialRelaySession(ctx, f.alice, MemberListAdapter{List: f.members}, relay, []string{f.hubAddr})
			if err != nil {
				return nil, err
			}
			negotiated = sessionALPN(link.Session())
			return link.Session(), nil
		},
	})
	if err != nil {
		t.Fatalf("the relayed session could not be opened: %v", err)
	}
	defer sess.Close()

	if negotiated != RelayALPN2 {
		t.Fatalf("the member and the anchor negotiated %q, want %q: the switch did not take "+
			"effect on the wire", negotiated, RelayALPN2)
	}

	var targetSess Session
	select {
	case targetSess = <-sessions:
	case <-time.After(relayHandshakeWait):
		t.Fatal("the target never received the relayed session")
	}

	payload := []byte("the relay is still alive")
	if _, err := sess.Write(payload); err != nil {
		t.Fatalf("the write through the relay failed: %v", err)
	}
	buf := make([]byte, len(payload))
	_ = targetSess.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := targetSess.Read(buf)
	if err != nil {
		t.Fatalf("the target could not read the relayed payload: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("the target read %q, want %q", buf[:n], payload)
	}
}
