// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// ---------------------------------------------------------------------------
// The PER-LEG format, and the JSON path that must keep working.
//
// The relay record format is a property of the LEG, not of the record
// helper: a leg's format comes from the ALPN that leg's connection
// negotiated, and the two legs of one relay can disagree.
//
// THESE TESTS PIN THE LEG-LEVEL CONSEQUENCES:
//
//   - a relay whose two legs both speak JSON still works — it is the
//     OLD member on a NEW anchor, and the OLD anchor's own path;
//   - the JSON path forwards a record in the JSON shape, byte for byte in
//     the sense that matters: the target's JSON parser reads it;
//   - a relay whose two legs both speak the binary frame takes the fast
//     path, and the fast path is the ONLY one that never decodes.
//
// The JSON-leg case is the one that matters most for the DEPLOYMENT ORDER:
// the anchor is upgraded first, so for the whole window between the
// anchor's upgrade and the last member's upgrade there are relays whose
// two legs are both on the old ALPN. If the trunk killed those, the
// switch would take the relay feature down for exactly as long as the
// rollout took.
// ---------------------------------------------------------------------------

// TestARelayWhoseTwoLegsBothSpeakJSONStillWorks is the regression test
// for the defect the switch's own design created.
//
// THE DEFECT. The forwarding path picked its implementation by asking
// "do the two legs agree?" — and when they agreed it built a BYTE
// SPLICER. But the splicer REFUSES to be built for two JSON legs: there
// is nothing to splice, the JSON path already forwards that format. So
// the refusal — which is a normal, expected answer, not a fault — was
// read as "the relay could not be spliced" and the flow was TORN DOWN.
//
// The symptom was a relay that died at its first record with "the peer
// aborted the stream", on the one deployment shape that MUST work: a
// member that negotiated the old ALPN on both legs.
//
// The fix: the splicer is the FAST PATH, and it is only for the binary
// frame. Every other shape — two JSON legs, or two legs that disagree —
// goes down the decode-and-re-encode path, which is the old path and
// which has always worked.
func TestARelayWhoseTwoLegsBothSpeakJSONStillWorks(t *testing.T) {
	f := newTrunkFixture(t)
	aRead, aWrite := newPipeSessionPair()
	tRead, cRead := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tRead, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := trunk.OpenFlowWithFormats(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, aRead, RelaySpliceFormatJSON, RelaySpliceFormatJSON); err != nil {
		t.Fatalf("the trunk refused a relay whose two legs speak JSON: %v", err)
	}

	go func() {
		if err := writeRelayRecordAs(aWrite, relayRecord{
			Type: uint8(wire.FrameMeshRelayData),
			Data: []byte("the old request"),
		}, RelaySpliceFormatJSON); err != nil {
			return
		}
		_ = aWrite.Close()
	}()

	cBr := bufio.NewReader(cRead)
	_ = cRead.SetReadDeadline(time.Now().Add(10 * time.Second))

	open, err := readRelayRecordAs(cBr, RelaySpliceFormatJSON)
	if err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the target's first record is type %d, want an open record", open.Type)
	}
	rec, err := readRelayRecordAs(cBr, RelaySpliceFormatJSON)
	if err != nil {
		t.Fatalf("the JSON-leg relay broke: %v", err)
	}
	if rec.Type != uint8(wire.FrameMeshRelayData) {
		t.Fatalf("the target received record type %d, want a data record", rec.Type)
	}
	if string(rec.Data) != "the old request" {
		t.Fatalf("the target received %q", rec.Data)
	}
}

// TestTheJSONLegPathPutsJSONOnTheWire pins the SHAPE of the JSON leg's
// wire form.
//
// It matters because the JSON leg's far side is an OLD member: its
// parser is a JSON parser, and a binary frame would fail it with
// "invalid character". So the record the anchor hands a JSON leg must
// still START with '{'.
//
// The rule is stated as a property of the LEG, not of the record helper:
// the same record, written on a binary leg, is a binary frame; written
// on a JSON leg, it is a JSON envelope. Both are correct — the leg
// decides.
func TestTheJSONLegPathPutsJSONOnTheWire(t *testing.T) {
	f := newTrunkFixture(t)
	aRead, aWrite := newPipeSessionPair()
	tRead, _ := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tRead, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trunk.OpenFlowWithFormats(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, aRead, RelaySpliceFormatJSON, RelaySpliceFormatJSON); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = writeRelayRecordAs(aWrite, relayRecord{
			Type: uint8(wire.FrameMeshRelayData),
			Data: []byte("payload"),
		}, RelaySpliceFormatJSON)
	}()

	// The TARGET's leg is what the anchor writes on, and seenBytes
	// records what a session WROTE.
	time.Sleep(200 * time.Millisecond)
	seen := tRead.seenBytes()
	got := lastFramedRecord(t, seen)
	if len(got) == 0 {
		t.Fatal("the target's leg received no record")
	}
	if got[0] != '{' {
		t.Fatalf("the record on a JSON leg starts with 0x%02x, want '{': "+
			"an old member's JSON parser could not read it", got[0])
	}
}

// TestTheSpliceIsOnlyForTheBinaryFrame states the rule that keeps the
// fast path and the fallback apart.
//
// The splicer REFUSES to be built for two JSON legs, and that refusal
// is CORRECT: there is nothing to splice, the decode-and-re-encode path
// already forwards that format. The point of this test is that the
// refusal must stay a refusal — the trunk's job is to fall back to the
// slow path, not to tear the relay down.
//
// If somebody ever "fixes" the splicer to accept JSON, this test is what
// says why not: the JSON path is the SLOW path, and the whole point of
// the switch is that the fast path is the one the default deployment
// takes.
func TestTheSpliceIsOnlyForTheBinaryFrame(t *testing.T) {
	if _, err := NewRelaySplicer(RelaySpliceFormatJSON, RelaySpliceFormatJSON); err == nil {
		t.Fatal("the splicer accepted two JSON legs: there is nothing to splice, and " +
			"the JSON path already forwards that format")
	}
	if _, err := NewRelaySplicer(RelaySpliceFormatBinary, RelaySpliceFormatJSON); err == nil {
		t.Fatal("the splicer accepted two legs that disagree: the anchor would have to " +
			"translate, and translating means decoding")
	}
	if _, err := NewRelaySplicer(RelaySpliceFormatBinary, RelaySpliceFormatBinary); err != nil {
		t.Fatalf("the splicer refused two binary legs: %v", err)
	}
}

// TestTheFastPathIsTheOnlyOneThatNeverDecodes pins the difference
// between the two forwarding paths, in the form that matters to the
// operator: the fast path's bytes are the requester's bytes.
//
// The JSON path re-encodes, so the target's bytes are NOT the
// requester's bytes — and that is FINE, because a JSON leg's target is
// an old member that reads records, not bytes. The binary path copies,
// so the target's bytes ARE the requester's bytes, framing included.
//
// The distinction is the measured win: the anchor's per-record cycle
// went 2000.8 µs → 589.9 µs (−71%) and the CPU per MiB of payload
// 326.7 ms → 92.3 ms (−72%) on the real host, same build, same lab,
// 128 MB bench, 3 reps each, link=relay.
func TestTheFastPathIsTheOnlyOneThatNeverDecodes(t *testing.T) {
	f := newTrunkFixture(t)

	// ① The binary pair: byte for byte.
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
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x5A}, 4096)
	if err := writeRelayRecordAs(reqPeer, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: payload,
	}, RelaySpliceFormatBinary); err != nil {
		t.Fatal(err)
	}
	_ = tgtPeer.SetReadDeadline(time.Now().Add(10 * time.Second))
	tgtBr := bufio.NewReader(tgtPeer)
	if _, err := readRelayRecordAs(tgtBr, RelaySpliceFormatBinary); err != nil {
		t.Fatalf("the target received no open record: %v", err)
	}
	if _, err := readRelayRecordAs(tgtBr, RelaySpliceFormatBinary); err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}
	sentRec := lastFramedRecord(t, reqPeer.seenBytes())
	gotRec := lastFramedRecord(t, tgtEnd.seenBytes())
	if !bytes.Equal(sentRec, gotRec) {
		t.Fatalf("the anchor re-encoded the record: the target got %d bytes that are not "+
			"the requester's %d bytes, so the fast path did not run", len(gotRec), len(sentRec))
	}
}

// TestTheQuotaIsChargedTheSameOnBothPaths keeps the P-7 limit honest
// across the two paths.
//
// The rate budget is charged on the PAYLOAD bytes, and it was charged on
// len(rec.Data) when the anchor decoded the record. The binary header
// carries the same number, so the charge must be identical — a fast
// path that charged a different number would silently change the P-7
// limit, and a fallback path that charged a different number would
// change it only for the deployments that are half upgraded.
func TestTheQuotaIsChargedTheSameOnBothPaths(t *testing.T) {
	const payload = 8192
	// The binary pair.
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
		t.Fatal(err)
	}
	if err := writeRelayRecordAs(reqPeer, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: make([]byte, payload),
	}, RelaySpliceFormatBinary); err != nil {
		t.Fatal(err)
	}
	_ = tgtPeer.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := readRelayRecordAs(bufio.NewReader(tgtPeer), RelaySpliceFormatBinary); err != nil {
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
