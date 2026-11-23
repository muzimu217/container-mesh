// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// ---------------------------------------------------------------------------
// The PAYLOAD CAP, and the mixed-version deployment.
//
// THE PROBLEM.
//
// The binary frame carries its payload verbatim, so the SAME 1 MiB record
// budget holds a third more payload than the JSON envelope did: the cap
// rose from 786 KiB to about 1 MiB (measured on the real host: 1,047,552
// payload bytes where the JSON one held 785,408, +33.38%).
//
// But the anchor, on a MIXED-VERSION relay, has to TRANSLATE: it decodes
// the binary record and re-encodes it as a JSON envelope, and the JSON
// envelope is a THIRD BIGGER (base64). So a record at the binary cap
// becomes 1,397,966 bytes — over the 1,048,576-byte record cap.
//
// And the anchor cannot fix it by splitting the record: the end-to-end
// seal puts the GCM nonce at the FRONT of the ciphertext and the
// authentication tag at the BACK, covering the whole plaintext, so half
// a sealed blob does not authenticate.
//
// So the WRITER has to write less. And the writer cannot know how much
// less, because the far leg's format is not its business — it is the
// ANCHOR's.
//
// THE ANSWER.
//
// The anchor knows the whole path: it dialled the target, so it knows the
// target's leg's format, and it knows the requester's leg's format. It
// computes the cap that survives the WHOLE path — the smallest cap of
// any leg the record has to fit on — and tells the writer, in the record
// the anchor already writes to it: the ANSWER for the requester, the
// OPEN for the target.
//
// A writer that gets no cap uses its own leg's default, which is what
// makes this safe against an OLD anchor: an old anchor is JSON-only, so
// the requester's leg is JSON, so the requester's default IS the JSON
// cap, and nothing needs to be said.
//
// THESE TESTS PIN THE CONSEQUENCE, not the mechanism.
// ---------------------------------------------------------------------------

// TestAMixedVersionRelayCarriesAMaximumSizedRecord is the regression
// test for the file-transfer defect.
//
// The shape is the one the file sender produces: ONE write of 1 MiB,
// which on a binary leg is ONE record at the binary cap. Translated to
// JSON it does not fit.
//
// The relay must carry it.
func TestAMixedVersionRelayCarriesAMaximumSizedRecord(t *testing.T) {
	f := newTrunkFixture(t)
	// The requester's leg speaks the binary frame; the target's leg
	// speaks JSON — the shape of a new requester on a new anchor
	// reaching an old target.
	reqEnd, reqPeer := newALPNPipePair(RelayALPN2)
	tgtEnd, tgtPeer := newALPNPipePair(RelayALPN)

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
	// The formats come from the legs' negotiated ALPNs, not from a
	// parameter: this is the production path.
	flowID, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, reqEnd)
	if err != nil {
		t.Fatal(err)
	}
	_ = flowID

	// The OPEN record the anchor wrote to the target carries the cap.
	_ = tgtPeer.SetReadDeadline(time.Now().Add(10 * time.Second))
	cBr := bufio.NewReader(tgtPeer)
	open, err := readRelayRecordAs(cBr, RelaySpliceFormatJSON)
	if err != nil {
		t.Fatalf("the target received no open record: %v", err)
	}
	if open.MaxPayload == 0 || int(open.MaxPayload) > maxRelayPlaintextJSON {
		t.Fatalf("the open record's cap is %d bytes, want the JSON cap (%d): the target's leg "+
			"speaks JSON, so a bigger record would not fit on it",
			open.MaxPayload, maxRelayPlaintextJSON)
	}

	// The requester writes what the file sender writes: one 1 MiB chunk.
	//
	// It goes out through the REAL requester session, because the cap
	// lives THERE: the requester is the node that decides how big a
	// record is, and the anchor's answer is what tells it.
	//
	// A requester that ignored the cap would write one 1,048,484-byte
	// record, and the anchor's translation of it would be 1,397,966
	// bytes — over the record cap, and the relay would die mid-transfer.
	e2e, _ := newEstablishedPair(t, f)
	rs := &relaySession{
		conn:   reqPeer,
		e2e:    e2e,
		target: f.bob.ID(),
		rec:    newFramedRecordReader(bufio.NewReader(reqEnd), RelaySpliceFormatBinary),
		format: RelaySpliceFormatBinary,
		// The cap the anchor stated.
		maxPayload: open.MaxPayload,
	}
	const chunk = 1 << 20
	go func() {
		_, _ = rs.Write(make([]byte, chunk))
	}()

	total := 0
	for i := 0; i < 2; i++ {
		rec, err := readRelayRecordAs(cBr, RelaySpliceFormatJSON)
		if err != nil {
			t.Fatalf("record %d never arrived at the old target: %v", i, err)
		}
		total += len(rec.Data)
	}
	// The target receives the SEALED bytes: the plaintext plus the
	// seal's overhead (a 12-byte nonce and a 16-byte tag) on each
	// record. The cap is on the PLAINTEXT, so the sealed total is the
	// plaintext plus the overhead of the records it was split into.
	want := chunk + e2eSealOverhead*2
	if total != want {
		t.Fatalf("the target received %d bytes, want %d", total, want)
	}
}

// TestTheAnchorTellsTheRequesterHowMuchItMayWrite pins the cap's
// journey.
//
// The requester's leg is binary (it may write the big cap) and the
// target's leg is JSON (it may only receive the small one), so the
// anchor must tell the requester to write no more than the small one —
// otherwise the anchor's own translation overflows the record cap.
//
// The cap travels in the ANSWER, which the anchor writes before the
// requester writes anything.
func TestTheAnchorTellsTheRequesterHowMuchItMayWrite(t *testing.T) {
	f := newTrunkFixture(t)
	// The requester's leg speaks the binary frame; the target's leg
	// speaks JSON.
	trunkSide, aSide := newALPNPipePair(RelayALPN2)
	tRead, _ := newALPNPipePair(RelayALPN)

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
	_ = writeRelayRecordAs(aSide, relayRecord{
		Type: uint8(wire.FrameMeshRelayOpen),
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, RelaySpliceFormatBinary)

	served := make(chan error, 1)
	go func() {
		served <- trunk.ServeRequesterWithFirstFormat(context.Background(), trunkSide, mustNotDial{},
			bufio.NewReader(trunkSide), nil, RelaySpliceFormatBinary)
	}()

	_ = aSide.SetReadDeadline(time.Now().Add(5 * time.Second))
	ans, err := readRelayRecordAs(bufio.NewReader(aSide), RelaySpliceFormatBinary)
	if err != nil {
		t.Fatalf("the requester received no answer: %v", err)
	}
	if !ans.Grant {
		t.Fatalf("the relay was refused: %s (%s)", ans.Reason, ans.Class)
	}
	if ans.MaxPayload == 0 {
		t.Fatal("the answer carries no cap: the requester does not know how much it may " +
			"write, and a maximum-sized write would overflow the record on the target's leg")
	}
	if ans.MaxPayload > maxRelayPlaintextJSON {
		t.Fatalf("the answer's cap is %d bytes, but the target's leg speaks JSON and can only "+
			"carry %d bytes of payload; the requester would write a record that does not fit",
			ans.MaxPayload, maxRelayPlaintextJSON)
	}
}

// TestTheAnchorTellsTheTargetHowMuchItMayWrite is the other direction.
//
// The target's leg speaks the binary frame and the requester's leg
// speaks JSON, so the TARGET is the one that must write less: its
// binary record would become a JSON envelope on the requester's leg.
//
// The cap travels in the OPEN record, which the anchor writes to the
// target before anything else.
func TestTheAnchorTellsTheTargetHowMuchItMayWrite(t *testing.T) {
	f := newTrunkFixture(t)
	// The requester's leg speaks JSON; the target's leg speaks the binary
	// frame. The TARGET is the one that must write less.
	reqEnd, _ := newALPNPipePair(RelayALPN)
	tgtEnd, tgtPeer := newALPNPipePair(RelayALPN2)

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
	// The formats come from the legs' negotiated ALPNs.
	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, reqEnd); err != nil {
		t.Fatal(err)
	}

	_ = tgtPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
	open, err := readRelayRecordAs(bufio.NewReader(tgtPeer), RelaySpliceFormatBinary)
	if err != nil {
		t.Fatalf("the target received no open record: %v", err)
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the target's first record is type %d, want an open record", open.Type)
	}
	if open.MaxPayload == 0 {
		t.Fatal("the open record carries no cap: the target does not know how much it may " +
			"write, and a maximum-sized write would overflow the record on the requester's leg")
	}
	if open.MaxPayload > maxRelayPlaintextJSON {
		t.Fatalf("the open record's cap is %d bytes, but the requester's leg speaks JSON and can "+
			"only carry %d bytes of payload; the target would write a record that does not fit",
			open.MaxPayload, maxRelayPlaintextJSON)
	}
}

// TestTheCapSurvivesTheTranslation pins the cap's travel through the
// anchor.
//
// The anchor writes the OPEN and the ANSWER in the LEG's format, so on a
// mixed-version relay the cap is TRANSLATED like any other field: it is
// decoded out of the binary record and re-encoded into the JSON one.
//
// If it did not survive, the requester would fall back to its own leg's
// default — the big binary cap — and the overflow would come back.
func TestTheCapSurvivesTheTranslation(t *testing.T) {
	// The binary form carries the cap.
	rec := relayRecord{
		Type:       uint8(wire.FrameMeshRelayAnswer),
		Grant:      true,
		MaxPayload: maxRelayPlaintextJSON,
	}
	body, err := writeRelayRecordAsBytes(rec, RelaySpliceFormatBinary)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := DecodeBinaryRelayFrame(body)
	if err != nil {
		t.Fatal(err)
	}
	if frame.MaxPayload != maxRelayPlaintextJSON {
		t.Fatalf("the binary form carries the cap as %d, want %d", frame.MaxPayload, maxRelayPlaintextJSON)
	}
	// And the JSON form carries the same number.
	jsonBody, err := writeRelayRecordAsBytes(rec, RelaySpliceFormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(jsonBody, []byte(`"maxp"`)) {
		t.Fatalf("the JSON envelope does not carry the cap: %s", jsonBody)
	}
	// And a JSON reader gets the same number back.
	got, err := readRelayRecordAs(bufio.NewReader(bytes.NewReader(
		append([]byte{0, 0, 0, byte(len(jsonBody))}, jsonBody...))), RelaySpliceFormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxPayload != maxRelayPlaintextJSON {
		t.Fatalf("the JSON envelope carries the cap as %d, want %d", got.MaxPayload, maxRelayPlaintextJSON)
	}
}

// writeRelayRecordAsBytes encodes one record in the given format and
// returns its BODY (the bytes inside the length prefix).
func writeRelayRecordAsBytes(rec relayRecord, format RelaySpliceFormat) ([]byte, error) {
	switch format {
	case RelaySpliceFormatBinary:
		frame, err := relayFrameFromRecord(rec)
		if err != nil {
			return nil, err
		}
		return EncodeBinaryRelayFrame(frame)
	default:
		return json.Marshal(rec)
	}
}

// alpnPipeSession is a pipe session that KNOWS which protocol version its
// connection negotiated.
//
// The plain test double cannot say, so the trunk falls back to the
// default format — which is the binary one. That is right for a
// same-version test, but it makes a MIXED-version test impossible to
// state: there would be no way to say "this leg speaks JSON".
//
// This double says so, exactly the way a QUIC session does: it answers
// the optional ALPN capability with the ALPN the leg negotiated. It is
// the same seam the production code uses (see quicSession.ALPN), so a
// test that drives it drives the real decision.
type alpnPipeSession struct {
	*pipeSession
	alpn string
}

// ALPN reports the negotiated application protocol.
func (p *alpnPipeSession) ALPN() string { return p.alpn }

// newALPNPipePair builds two connected sessions that both report the
// given ALPN.
func newALPNPipePair(alpn string) (*alpnPipeSession, *alpnPipeSession) {
	a, b := newPipeSessionPair()
	return &alpnPipeSession{pipeSession: a, alpn: alpn},
		&alpnPipeSession{pipeSession: b, alpn: alpn}
}

// TestTheCapIsEightBytesBigEndianInTheBinaryMeta pins the binary
// form's shape.
//
// The cap rides in the META BLOCK, LAST, and it is there only when it
// is not zero. The meta block's own length says how long it is, so no
// flag bit is needed — which matters, because the flags byte has all
// eight bits taken by the other optional fields.
//
// A peer that does not know about the cap ignores the trailing bytes:
// the block's length tells it where the block ends, and it simply does
// not read them. That is what makes the cap SAFE TO ADD: an old peer
// reads the record correctly and ignores a field it has never heard of.
func TestTheCapIsEightBytesBigEndianInTheBinaryMeta(t *testing.T) {
	rec := relayRecord{
		Type:       uint8(wire.FrameMeshRelayData),
		Data:       []byte("payload"),
		MaxPayload: 0x0102030405060708,
	}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	hdr, meta, _, err := SplitRelayFrame(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) < 8 {
		t.Fatalf("the meta block is %d bytes, want at least 8 (the cap)", len(meta))
	}
	tail := meta[len(meta)-8:]
	var got uint64
	for _, b := range tail {
		got = got<<8 | uint64(b)
	}
	if got != 0x0102030405060708 {
		t.Fatalf("the cap in the meta block is %#016x, want %#016x", got, uint64(0x0102030405060708))
	}
	// And the header says how long the block is, so the cap's position is
	// unambiguous.
	if int(hdr.MetaLen) != len(meta) {
		t.Fatalf("the header says the meta block is %d bytes, the block is %d", hdr.MetaLen, len(meta))
	}
}

// TestARecordWithNoCapHasNoTrailingMetaBytes pins the other half.
//
// A zero cap is ABSENT, not present-and-zero — the same rule the JSON
// envelope's omitempty applies, and the same rule every other optional
// field follows. So a record with no cap has a meta block with no
// trailing eight bytes, and an old peer cannot tell the difference.
func TestARecordWithNoCapHasNoTrailingMetaBytes(t *testing.T) {
	rec := relayRecord{Type: uint8(wire.FrameMeshRelayData), Data: []byte("payload")}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	_, meta, _, err := SplitRelayFrame(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) != 0 {
		t.Fatalf("a record with no cap has a %d byte meta block, want 0: %x", len(meta), meta)
	}
}

// TestTheCapFitsInTheFrameWithoutGrowingTheHeader pins that the cap
// costs no header space.
//
// The fixed header stays 24 bytes: the cap is a VARIABLE field, and it
// goes in the meta block, where the other variable fields live. The
// frame's whole point is that the anchor can learn the type and the
// payload's length from 24 bytes — and a header that grew would cost
// that.
func TestTheCapFitsInTheFrameWithoutGrowingTheHeader(t *testing.T) {
	if relayFrameHeaderSize != 24 {
		t.Fatalf("the fixed header is %d bytes, want 24: the cap must not grow it", relayFrameHeaderSize)
	}
}

// TestTheCapIsNotMistakenForAPayload pins the decode side.
//
// The cap is in the META block and the payload is after it. A decoder
// that put the cap in the wrong place would read the payload's length
// wrong — and the anchor would copy the wrong number of bytes.
func TestTheCapIsNotMistakenForAPayload(t *testing.T) {
	payload := bytes.Repeat([]byte{0x5A}, 1000)
	rec := relayRecord{
		Type:       uint8(wire.FrameMeshRelayData),
		Data:       payload,
		MaxPayload: 123456,
	}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	hdr, _, pl, err := SplitRelayFrame(body)
	if err != nil {
		t.Fatal(err)
	}
	if int(hdr.PayloadLen) != len(payload) {
		t.Fatalf("the header says the payload is %d bytes, want %d", hdr.PayloadLen, len(payload))
	}
	if !bytes.Equal(pl, payload) {
		t.Fatal("the payload came back different")
	}
	// And the big-endian reading is the one the code uses.
	var be [8]byte
	binary.BigEndian.PutUint64(be[:], 123456)
	if be[0] != 0x00 || be[7] != 0x40 {
		t.Fatalf("the big-endian encoding is %x", be[:])
	}
}

// TestAMixedVersionRelayCarriesAFileEndToEnd is the whole defect, end
// to end.
//
// A new requester on a new anchor, reaching an OLD target. The
// requester writes the file sender's 1 MiB chunk; the anchor
// translates; the old target reads JSON.
//
// Before the cap existed, the requester wrote one 1,048,484-byte
// record, the anchor's translation of it was 1,397,966 bytes, the
// record cap refused it, and the relay died mid-file.
//
// This test drives the REAL serving path (ServeRequesterWithFirst,
// which is what the QUIC listener calls) and the REAL requester session
// (relaySession, which is what OpenRelay hands back).
func TestAMixedVersionRelayCarriesAFileEndToEnd(t *testing.T) {
	f := newTrunkFixture(t)
	// The requester's leg speaks the binary frame; the target's leg
	// speaks JSON.
	trunkSide, reqPeer := newALPNPipePair(RelayALPN2)
	tgtSide, tgtPeer := newALPNPipePair(RelayALPN)

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tgtSide, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The end-to-end session the requester will seal with.
	e2e, peer := newEstablishedPair(t, f)

	// The requester opens the relay.
	_ = writeRelayRecordAs(reqPeer, relayRecord{
		Type: uint8(wire.FrameMeshRelayOpen),
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
		Kind: KindFile,
	}, RelaySpliceFormatBinary)

	served := make(chan error, 1)
	go func() {
		served <- trunk.ServeRequesterWithFirstFormat(context.Background(), trunkSide, mustNotDial{},
			bufio.NewReader(trunkSide), nil, RelaySpliceFormatBinary)
	}()

	// The requester reads the answer, which carries the cap.
	_ = reqPeer.SetReadDeadline(time.Now().Add(10 * time.Second))
	reqBr := bufio.NewReader(reqPeer)
	ans, err := readRelayRecordAs(reqBr, RelaySpliceFormatBinary)
	if err != nil {
		t.Fatalf("the requester received no answer: %v", err)
	}
	if !ans.Grant {
		t.Fatalf("the relay was refused: %s (%s)", ans.Reason, ans.Class)
	}
	if ans.MaxPayload == 0 || int(ans.MaxPayload) > maxRelayPlaintextJSON {
		t.Fatalf("the answer's cap is %d bytes, want the JSON cap (%d)", ans.MaxPayload, maxRelayPlaintextJSON)
	}

	// The requester writes the file sender's chunk.
	rs := &relaySession{
		conn:       reqPeer,
		e2e:        e2e,
		target:     f.bob.ID(),
		rec:        newFramedRecordReader(bufio.NewReader(trunkSide), RelaySpliceFormatBinary),
		format:     RelaySpliceFormatBinary,
		maxPayload: ans.MaxPayload,
	}
	const chunk = 1 << 20
	payload := make([]byte, chunk)
	for i := range payload {
		payload[i] = byte(i)
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := rs.Write(payload)
		writeDone <- err
	}()

	// The old target reads the JSON records and opens them.
	_ = tgtPeer.SetReadDeadline(time.Now().Add(20 * time.Second))
	tgtBr := bufio.NewReader(tgtPeer)
	if _, err := readRelayRecordAs(tgtBr, RelaySpliceFormatJSON); err != nil {
		t.Fatalf("the target received no open record: %v", err)
	}
	got := make([]byte, 0, chunk)
	for len(got) < chunk {
		rec, err := readRelayRecordAs(tgtBr, RelaySpliceFormatJSON)
		if err != nil {
			t.Fatalf("the target stopped at %d of %d bytes: %v", len(got), chunk, err)
		}
		plain, err := peer.Open(rec.Data)
		if err != nil {
			t.Fatalf("the target could not open the payload: %v", err)
		}
		got = append(got, plain...)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("the requester's write failed: %v", err)
	}
	if len(got) != chunk {
		t.Fatalf("the target received %d bytes, want %d", len(got), chunk)
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("the payload differs at byte %d: got %d, want %d", i, got[i], payload[i])
		}
	}
}
