// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/wire"
)

// ---------------------------------------------------------------------------
// The tests of the BYTE SPLICE.
//
// The splice is the second step of the binary relay record: the anchor
// reads a record's fixed header, learns how long the record is, and copies
// exactly those bytes from one leg of the relay to the other — WITHOUT
// decoding the payload.
//
// The questions these tests ask:
//
//  1. Does the far side get the EXACT bytes the near side sent? A splice
//     that changes a byte is worse than no splice.
//  2. Does the anchor touch the payload? It must not: the payload is the
//     end-to-end ciphertext and the anchor holds no key for it.
//  3. What happens when the far side cannot understand the frame? The
//     splice must fail LOUDLY, not forward a record nobody can read.
//  4. What happens when the frame stops in the middle? A truncation must
//     never look like a clean end.
//  5. Does the quota still see the payload bytes? The rate budget is
//     charged on the payload, and the payload length is in the header.
//
// ---------------------------------------------------------------------------

// spliceSink is the FAR side of a splice in a test: it records every byte
// written to it and remembers whether it was half-closed or aborted.
//
// It implements sessionAborter, so the splicer's teardown path can be
// observed: an aborted far side is the "the peer sees a reset" behaviour
// that a truncated record needs.
type spliceSink struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	closed  bool
	aborted bool
	writes  int
}

func (s *spliceSink) Read(p []byte) (int, error) { return 0, io.EOF }

func (s *spliceSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	return s.buf.Write(p)
}

// Close half-closes: the send side ends, the receive side stays open.
func (s *spliceSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Abort ends the session on BOTH sides.
func (s *spliceSink) Abort() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborted = true
	s.closed = true
	return nil
}

func (s *spliceSink) SetReadDeadline(time.Time) error  { return nil }
func (s *spliceSink) SetWriteDeadline(time.Time) error { return nil }

// bytesWritten returns everything the splice wrote to the far side.
func (s *spliceSink) bytesWritten() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, s.buf.Len())
	copy(out, s.buf.Bytes())
	return out
}

// state reports whether the far session was half-closed and whether it
// was aborted.
func (s *spliceSink) state() (closed, aborted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed, s.aborted
}

// spliceQuota is the rate budget of a splice in a test.
//
// It records what it was asked to reserve and hands back the wait times
// the test scripted. A wait of zero means "the bytes are budgeted now".
type spliceQuota struct {
	mu     sync.Mutex
	asked  []int64
	waits  []time.Duration
	calls  int
	failed error
}

// Reserve records the request and returns the wait the test scripted.
func (q *spliceQuota) Reserve(n int64) (time.Duration, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.asked = append(q.asked, n)
	idx := q.calls
	q.calls++
	if q.failed != nil {
		return 0, q.failed
	}
	if idx < len(q.waits) {
		return q.waits[idx], nil
	}
	return 0, nil
}

// askedFor returns the payload sizes the quota was asked to reserve.
func (q *spliceQuota) askedFor() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]int64, len(q.asked))
	copy(out, q.asked)
	return out
}

// spliceWire builds the wire form of one binary relay record: the 4-byte
// length prefix the mesh's framing layer puts in front of every record,
// and then the binary frame itself.
func spliceWire(t *testing.T, rec relayFrame) []byte {
	t.Helper()
	body, err := EncodeBinaryRelayFrame(rec)
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	var out bytes.Buffer
	if err := writeFrame(&out, body); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	return out.Bytes()
}

// TestTheSplicerForwardsARecordVerbatim checks the core property: the far
// side receives EXACTLY the bytes the near side sent.
//
// Not "the same payload" and not "a record that decodes to the same
// thing" — the same BYTES. A splice that re-encodes anything is not a
// splice.
func TestTheSplicerForwardsARecordVerbatim(t *testing.T) {
	payload := bytes.Repeat([]byte{0x7E}, 4096)
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: payload})

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(wire0))

	hdr, err := sp.ForwardOne(br, sink)
	if err != nil {
		t.Fatalf("ForwardOne: %v", err)
	}
	if hdr.Type != uint8(wire.FrameMeshRelayData) {
		t.Fatalf("the header type = %d, want %d", hdr.Type, wire.FrameMeshRelayData)
	}
	if int(hdr.PayloadLen) != len(payload) {
		t.Fatalf("the header payload length = %d, want %d", hdr.PayloadLen, len(payload))
	}
	if got := sink.bytesWritten(); !bytes.Equal(got, wire0) {
		t.Fatalf("the far side got %d bytes, want %d, and they must be identical", len(got), len(wire0))
	}
}

// TestTheSplicerNeverDecodesThePayload checks that the payload is opaque
// to the splicer.
//
// The payload is the end-to-end ciphertext. It is NOT valid base64 and
// NOT valid JSON, so a splice that decoded it would fail on this record.
// The record still has to come out the far side byte for byte.
func TestTheSplicerNeverDecodesThePayload(t *testing.T) {
	// Bytes that no JSON parser and no base64 decoder would accept as a
	// document: 0xFF is not a legal UTF-8 byte and the length is not a
	// multiple of four.
	payload := []byte{0xFF, 0xFE, 0xFD, 0x01, 0x80, 0x00, 0xC7}
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: payload})

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(wire0))
	if _, err := sp.ForwardOne(br, sink); err != nil {
		t.Fatalf("ForwardOne: %v", err)
	}
	if got := sink.bytesWritten(); !bytes.Equal(got, wire0) {
		t.Fatalf("the far side got %d bytes, want %d, and they must be identical", len(got), len(wire0))
	}
}

// TestTheSplicerForwardsAMetaBlockVerbatim checks that the control fields
// travel too.
//
// The meta block carries the fields the CONTROL records use — the from and
// the to, the kind, the reason. The splice must not skip them: they are
// part of the record's bytes.
func TestTheSplicerForwardsAMetaBlockVerbatim(t *testing.T) {
	wire0 := spliceWire(t, relayFrame{
		Type:    uint8(wire.FrameMeshRelayOpen),
		From:    "alice-device",
		To:      "bob-device",
		Kind:    "shell",
		Hops:    3,
		Hello:   []byte(`{"device_id":"alice-device"}`),
		Reason:  "because",
		Class:   "denied",
		Grant:   true,
		Session: 4242,
	})
	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(wire0))
	hdr, err := sp.ForwardOne(br, sink)
	if err != nil {
		t.Fatalf("ForwardOne: %v", err)
	}
	if hdr.MetaLen == 0 {
		t.Fatal("the header says the record carries no meta block")
	}
	if got := sink.bytesWritten(); !bytes.Equal(got, wire0) {
		t.Fatalf("the far side got %d bytes, want %d, and they must be identical", len(got), len(wire0))
	}
}

// TestTheSplicerForwardsSeveralRecordsInSequence checks that the splice
// keeps its place between records.
//
// A relay carries many records over one session, so the splice must read
// one record after the other without losing or duplicating a byte.
func TestTheSplicerForwardsSeveralRecordsInSequence(t *testing.T) {
	var stream bytes.Buffer
	for i := 0; i < 5; i++ {
		stream.Write(spliceWire(t, relayFrame{
			Type: uint8(wire.FrameMeshRelayData),
			Data: bytes.Repeat([]byte{byte(i)}, 100*(i+1)),
		}))
	}
	want := stream.Bytes()

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(want))
	for i := 0; i < 5; i++ {
		if _, err := sp.ForwardOne(br, sink); err != nil {
			t.Fatalf("ForwardOne #%d: %v", i, err)
		}
	}
	if got := sink.bytesWritten(); !bytes.Equal(got, want) {
		t.Fatalf("the far side got %d bytes, want %d", len(got), len(want))
	}
}

// TestTheSplicerRefusesAFrameItCannotUnderstand checks the loud failure.
//
// A JSON record starts with '{' (0x7B). A binary record starts with the
// format tag 0xC7. When the two legs of a relay disagree about the record
// format, the splice MUST refuse — and it must refuse BEFORE writing
// anything, because a half-written record on the far side is a record the
// far side will mis-parse.
func TestTheSplicerRefusesAFrameItCannotUnderstand(t *testing.T) {
	jsonBody, err := json.Marshal(relayRecord{Type: uint8(wire.FrameMeshRelayData), Data: []byte("x")})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var stream bytes.Buffer
	if err := writeFrame(&stream, jsonBody); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(stream.Bytes()))
	_, err = sp.ForwardOne(br, sink)
	if err == nil {
		t.Fatal("the splice forwarded a JSON record: it must refuse a frame it cannot understand")
	}
	if !errors.Is(err, ErrRelaySpliceFormat) {
		t.Fatalf("the splice failed with %v, want an ErrRelaySpliceFormat", err)
	}
	if got := sink.bytesWritten(); len(got) != 0 {
		t.Fatalf("the splice wrote %d bytes before refusing; it must refuse BEFORE writing", len(got))
	}
	// The message has to say what was wrong: the operator's first question
	// is "which format is the peer speaking?".
	if !bytes.Contains([]byte(err.Error()), []byte("JSON")) {
		t.Fatalf("the error %q does not name the JSON format", err)
	}
}

// TestTheSplicerRefusesAFrameWithTheWrongMagicByte checks that a frame
// whose tag is wrong is refused.
//
// The tag is the FORMAT: 0xC7 is not a byte any JSON parser can start a
// document with, so a frame that starts with anything else is either the
// other format or corruption. Either way it must not be forwarded.
func TestTheSplicerRefusesAFrameWithTheWrongMagicByte(t *testing.T) {
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: []byte("x")})
	bad := append([]byte(nil), wire0...)
	bad[4] = 0x42 // the frame's first byte, right after the length prefix

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(bad))
	_, err := sp.ForwardOne(br, sink)
	if err == nil {
		t.Fatal("the splice forwarded a frame with the wrong tag")
	}
	if !errors.Is(err, ErrRelaySpliceFormat) {
		t.Fatalf("the splice failed with %v, want an ErrRelaySpliceFormat", err)
	}
	if got := sink.bytesWritten(); len(got) != 0 {
		t.Fatalf("the splice wrote %d bytes before refusing; it must refuse BEFORE writing", len(got))
	}
}

// TestTheSplicerRefusesAFrameWithAnUnsupportedVersion checks that a frame
// from a future version of the format is refused rather than
// mis-read.
//
// The version byte is there for this: a version this code does not know
// may put its fields somewhere else, and forwarding it verbatim would hand
// the far side a record whose header lies about its own lengths.
func TestTheSplicerRefusesAFrameWithAnUnsupportedVersion(t *testing.T) {
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: []byte("x")})
	bad := append([]byte(nil), wire0...)
	bad[5] = relayFrameVersion + 1

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(bad))
	_, err := sp.ForwardOne(br, sink)
	if err == nil {
		t.Fatal("the splice forwarded a frame of an unknown version")
	}
	if !errors.Is(err, ErrRelaySpliceFormat) {
		t.Fatalf("the splice failed with %v, want an ErrRelaySpliceFormat", err)
	}
	if got := sink.bytesWritten(); len(got) != 0 {
		t.Fatalf("the splice wrote %d bytes before refusing; it must refuse BEFORE writing", len(got))
	}
}

// TestTheSplicerReportsATruncatedRecordInsteadOfACleanEnd checks the
// half-close in the middle of a frame.
//
// The peer announces a record of 4096 bytes and then half-closes after
// 100. That is NOT the end of the conversation — it is a broken stream.
// If the splice reported it as a clean end, the far side would believe
// the relay finished, and the tail of the record would be lost silently.
//
// The error must NOT satisfy errors.Is(err, io.EOF): every "EOF means the
// sender finished" caller in this package would mistake a truncation for
// a graceful finish.
func TestTheSplicerReportsATruncatedRecordInsteadOfACleanEnd(t *testing.T) {
	payload := bytes.Repeat([]byte{0x33}, 4096)
	full := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: payload})
	cut := full[:4+relayFrameHeaderSize+100]

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(cut))
	_, err := sp.ForwardOne(br, sink)
	if err == nil {
		t.Fatal("the splice accepted a truncated record")
	}
	if !errors.Is(err, ErrRelaySpliceTruncated) {
		t.Fatalf("the splice failed with %v, want an ErrRelaySpliceTruncated", err)
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("the truncation %v satisfies errors.Is(err, io.EOF): a truncation must never look like a clean end", err)
	}
}

// TestTheSplicerReportsATruncatedHeaderInsteadOfACleanEnd checks the same
// thing one step earlier: the peer stops inside the 24-byte header.
//
// The header is the first thing the splice reads, so this is the case
// where "the peer said nothing useful and stopped" must still be a
// truncation, not a clean end.
func TestTheSplicerReportsATruncatedHeaderInsteadOfACleanEnd(t *testing.T) {
	full := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: bytes.Repeat([]byte{1}, 100)})
	cut := full[:4+8]

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(cut))
	_, err := sp.ForwardOne(br, sink)
	if err == nil {
		t.Fatal("the splice accepted a truncated header")
	}
	if !errors.Is(err, ErrRelaySpliceTruncated) {
		t.Fatalf("the splice failed with %v, want an ErrRelaySpliceTruncated", err)
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("the truncation %v satisfies errors.Is(err, io.EOF)", err)
	}
}

// TestTheSplicerReportsACleanEndAtARecordBoundary checks the OTHER end of
// the same distinction.
//
// A peer that half-closes between records has finished sending. That is a
// clean end, and it must come back as io.EOF so that the caller
// half-closes the far side (the far side may still have an answer on the
// way).
func TestTheSplicerReportsACleanEndAtARecordBoundary(t *testing.T) {
	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(nil))
	_, err := sp.ForwardOne(br, sink)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("the splice failed with %v, want a clean io.EOF", err)
	}
	if errors.Is(err, ErrRelaySpliceTruncated) {
		t.Fatalf("a clean end was reported as a truncation: %v", err)
	}
}

// TestTheSplicerRefusesAFrameBiggerThanTheCap checks the cap.
//
// The header says how long the record is, and the header comes from the
// peer. A peer that announces four gigabytes must not make the anchor
// wait for four gigabytes: the cap is what bounds what the anchor is
// willing to carry, and it is checked BEFORE anything is read or written.
func TestTheSplicerRefusesAFrameBiggerThanTheCap(t *testing.T) {
	// A hand-built header: a legal tag and version, and a payload length
	// past the relay record cap.
	body := make([]byte, relayFrameHeaderSize)
	body[relayFrameOffMagic] = relayFrameMagic
	body[relayFrameOffVersion] = relayFrameVersion
	body[relayFrameOffType] = uint8(wire.FrameMeshRelayData)
	binary.BigEndian.PutUint32(body[relayFrameOffPayload:], maxRelayRecord+1)
	var stream bytes.Buffer
	if err := writeFrame(&stream, body); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(stream.Bytes()))
	_, err := sp.ForwardOne(br, sink)
	if err == nil {
		t.Fatal("the splice accepted a record past the cap")
	}
	if !errors.Is(err, ErrRelaySpliceTooLarge) {
		t.Fatalf("the splice failed with %v, want an ErrRelaySpliceTooLarge", err)
	}
	if got := sink.bytesWritten(); len(got) != 0 {
		t.Fatalf("the splice wrote %d bytes before refusing; it must refuse BEFORE writing", len(got))
	}
}

// TestTheSplicerRefusesALengthPrefixThatDisagreesWithTheHeader checks the
// record's own consistency.
//
// The framing layer prefixes every record with its length, and the header
// says how long the body is. If the two disagree, the record is
// malformed: the far side would frame the next record at the wrong
// offset, and every record after it would be garbage.
func TestTheSplicerRefusesALengthPrefixThatDisagreesWithTheHeader(t *testing.T) {
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: bytes.Repeat([]byte{9}, 100)})
	// Claim a body that is 100 bytes shorter than it is.
	binary.BigEndian.PutUint32(wire0[:4], uint32(relayFrameHeaderSize))

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(wire0))
	_, err := sp.ForwardOne(br, sink)
	if err == nil {
		t.Fatal("the splice accepted a record whose length prefix disagrees with its header")
	}
	if !errors.Is(err, ErrRelaySpliceFormat) {
		t.Fatalf("the splice failed with %v, want an ErrRelaySpliceFormat", err)
	}
}

// TestTheSplicerChargesTheQuotaOnThePayloadOnly checks the rate budget.
//
// The budget is charged on the PAYLOAD — the bytes the anchor actually
// carries — and not on the envelope around it. The payload length is in
// the header, so the charge needs no decoding and its value is the same
// number the JSON path charged.
func TestTheSplicerChargesTheQuotaOnThePayloadOnly(t *testing.T) {
	payload := bytes.Repeat([]byte{0x21}, 1000)
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: payload})

	quota := &spliceQuota{}
	sp := newTestSplicer(t)
	sp.Quota = quota
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(wire0))
	if _, err := sp.ForwardOne(br, sink); err != nil {
		t.Fatalf("ForwardOne: %v", err)
	}
	asked := quota.askedFor()
	if len(asked) != 1 {
		t.Fatalf("the quota was asked %d times, want once", len(asked))
	}
	if asked[0] != int64(len(payload)) {
		t.Fatalf("the quota was charged %d bytes, want the payload's %d", asked[0], len(payload))
	}
}

// TestTheSplicerWaitsForTheQuotaBeforeWriting checks the throttle.
//
// The rate budget is a THROTTLE, not a kill switch: the splice waits for
// the bucket to refill and then writes. The wait must happen BEFORE the
// write, because a record that is written before it is budgeted is a
// record that gets the node over its cap.
func TestTheSplicerWaitsForTheQuotaBeforeWriting(t *testing.T) {
	payload := bytes.Repeat([]byte{0x21}, 500)
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: payload})

	quota := &spliceQuota{waits: []time.Duration{30 * time.Millisecond, 0}}
	sp := newTestSplicer(t)
	sp.Quota = quota
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(wire0))

	start := time.Now()
	if _, err := sp.ForwardOne(br, sink); err != nil {
		t.Fatalf("ForwardOne: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("the splice wrote the record after %v, want it to wait for the quota", elapsed)
	}
	if got := sink.bytesWritten(); !bytes.Equal(got, wire0) {
		t.Fatalf("the far side got %d bytes, want %d", len(got), len(wire0))
	}
}

// TestTheSplicerStopsWaitingWhenTheFlowEnds checks that a quota wait is
// interruptible.
//
// The relay's flow can end while the splice is waiting for the bucket.
// A wait that cannot be interrupted would hold the flow's goroutine and
// its session slot for as long as the wait lasts.
func TestTheSplicerStopsWaitingWhenTheFlowEnds(t *testing.T) {
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: bytes.Repeat([]byte{1}, 100)})

	quota := &spliceQuota{waits: []time.Duration{time.Hour}}
	done := make(chan struct{})
	sp := newTestSplicer(t)
	sp.Quota = quota
	sp.Done = done
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(wire0))

	go func() {
		time.Sleep(20 * time.Millisecond)
		close(done)
	}()
	start := time.Now()
	_, err := sp.ForwardOne(br, sink)
	if err == nil {
		t.Fatal("the splice waited for the quota even though the flow had ended")
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("the interrupted wait came back as a clean end: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the splice waited %v after the flow ended; the wait must be interruptible", elapsed)
	}
}

// TestTheSplicerClosesTheFarSessionWhenThePeerFinishesSending checks the
// half-close propagation.
//
// "Send the request, FIN, wait for the answer" is what every
// request/response protocol does. So when the near side finishes sending,
// the far side is HALF-CLOSED — not torn down. The far side may still
// have an answer to give.
func TestTheSplicerClosesTheFarSessionWhenThePeerFinishesSending(t *testing.T) {
	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(nil))
	if err := sp.Forward(br, sink); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	closed, aborted := sink.state()
	if !closed {
		t.Fatal("the splice finished without closing the far session")
	}
	if aborted {
		t.Fatal("the splice ABORTED the far session on a clean end; a clean end is a half close")
	}
}

// TestTheSplicerForwardsACloseRecordAndEndsTheDirection checks the close
// record.
//
// The close record is forwarded — the far side has to learn that the
// relay is over — and then the direction ends. The close is a NORMAL
// end, not a failure: the far side is half-closed, not aborted.
func TestTheSplicerForwardsACloseRecordAndEndsTheDirection(t *testing.T) {
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayClose), Reason: "done"})

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(wire0))
	if err := sp.Forward(br, sink); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if got := sink.bytesWritten(); !bytes.Equal(got, wire0) {
		t.Fatalf("the far side got %d bytes, want the close record's %d", len(got), len(wire0))
	}
	closed, aborted := sink.state()
	if !closed {
		t.Fatal("the splice finished without closing the far session")
	}
	if aborted {
		t.Fatal("the splice ABORTED the far session after a close record; a close is a normal end")
	}
}

// TestTheSplicerAbortsTheFarSessionWhenTheRecordIsTruncated checks the
// teardown of a broken stream.
//
// A truncated record means the far side has a PARTIAL record on its wire.
// Half-closing there would tell the far side "the relay finished", and
// the far side would believe the tail of the record was the end of the
// message. So the far side is ABORTED: it sees a reset, not a finish.
func TestTheSplicerAbortsTheFarSessionWhenTheRecordIsTruncated(t *testing.T) {
	full := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: bytes.Repeat([]byte{2}, 4096)})
	cut := full[:4+relayFrameHeaderSize+10]

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(cut))
	err := sp.Forward(br, sink)
	if err == nil {
		t.Fatal("the splice accepted a truncated record")
	}
	_, aborted := sink.state()
	if !aborted {
		t.Fatal("the splice did not abort the far session after a truncation; the far side would read the partial record as the end of the relay")
	}
}

// TestTheSplicerRefusesToSpliceWhenTheTwoLegsDisagree checks the
// compatibility rule.
//
// The relay record format is a protocol BETWEEN THE TWO ENDPOINTS: the
// anchor is a pipe. If the requester's leg speaks one format and the
// target's leg speaks another, the anchor cannot splice — it would have
// to translate, and translating means DECODING, which is the thing the
// splice exists to avoid. So the splicer refuses to be built.
func TestTheSplicerRefusesToSpliceWhenTheTwoLegsDisagree(t *testing.T) {
	_, err := NewRelaySplicer(RelaySpliceFormatBinary, RelaySpliceFormatJSON)
	if err == nil {
		t.Fatal("the splicer was built for two legs that speak different formats")
	}
	if !errors.Is(err, ErrRelaySpliceMismatch) {
		t.Fatalf("the splicer failed with %v, want an ErrRelaySpliceMismatch", err)
	}
}

// TestTheSplicerRefusesToSpliceAJSONOnlyRelay checks the other side of
// the same rule.
//
// If both legs speak JSON there is nothing to splice: the JSON path
// already works, and a splicer that pretended otherwise would be a
// second, divergent implementation of the same wire format.
func TestTheSplicerRefusesToSpliceAJSONOnlyRelay(t *testing.T) {
	_, err := NewRelaySplicer(RelaySpliceFormatJSON, RelaySpliceFormatJSON)
	if err == nil {
		t.Fatal("the splicer was built for a relay that speaks JSON on both legs")
	}
	if !errors.Is(err, ErrRelaySpliceUnsupported) {
		t.Fatalf("the splicer failed with %v, want an ErrRelaySpliceUnsupported", err)
	}
}

// TestTheSplicerDoesNotLeakGoroutinesOrSessions checks that the splice is
// a synchronous unit.
//
// The splice runs inside the relay's own forwarding goroutine, so it must
// not start one of its own: a goroutine per direction per flow is a
// goroutine that has to be joined, and the relay's flow teardown would
// have to know about it.
func TestTheSplicerDoesNotLeakGoroutinesOrSessions(t *testing.T) {
	before := runtime.NumGoroutine()
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: bytes.Repeat([]byte{3}, 1000)})
	for i := 0; i < 50; i++ {
		sp := newTestSplicer(t)
		sink := &spliceSink{}
		br := bufio.NewReader(bytes.NewReader(wire0))
		if _, err := sp.ForwardOne(br, sink); err != nil {
			t.Fatalf("ForwardOne: %v", err)
		}
	}
	after := runtime.NumGoroutine()
	if after > before+1 {
		t.Fatalf("the goroutine count went from %d to %d over 50 splices; the splice must not leak goroutines", before, after)
	}
}

// TestTheSplicerForwardsBothDirectionsOfARelay checks the two-legged
// shape.
//
// A relay is TWO legs: the requester and the target. Both directions run
// at once, on the same splicer type, and they must not interfere — the
// two of them are the race the -race detector is for.
func TestTheSplicerForwardsBothDirectionsOfARelay(t *testing.T) {
	payloadA := bytes.Repeat([]byte{0xAA}, 3000)
	payloadC := bytes.Repeat([]byte{0xCC}, 4000)
	toC := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: payloadA})
	toA := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: payloadC})

	spC := newTestSplicer(t)
	spA := newTestSplicer(t)
	sinkC := &spliceSink{}
	sinkA := &spliceSink{}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		br := bufio.NewReader(bytes.NewReader(toC))
		if _, err := spC.ForwardOne(br, sinkC); err != nil {
			t.Errorf("ForwardOne towards the target: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		br := bufio.NewReader(bytes.NewReader(toA))
		if _, err := spA.ForwardOne(br, sinkA); err != nil {
			t.Errorf("ForwardOne towards the requester: %v", err)
		}
	}()
	wg.Wait()

	if got := sinkC.bytesWritten(); !bytes.Equal(got, toC) {
		t.Fatalf("the target got %d bytes, want %d", len(got), len(toC))
	}
	if got := sinkA.bytesWritten(); !bytes.Equal(got, toA) {
		t.Fatalf("the requester got %d bytes, want %d", len(got), len(toA))
	}
}

// TestTheSplicerCopysAPayloadLargerThanItsScratchBuffer checks that the
// splice's internal buffer is a buffer, not a limit.
//
// The splice copies through a fixed-size scratch buffer so that it
// allocates nothing per record. A record bigger than that buffer must
// still come out whole.
func TestTheSplicerCopysAPayloadLargerThanItsScratchBuffer(t *testing.T) {
	payload := bytes.Repeat([]byte{0x5C}, 300*1024)
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: payload})

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(wire0))
	if _, err := sp.ForwardOne(br, sink); err != nil {
		t.Fatalf("ForwardOne: %v", err)
	}
	if got := sink.bytesWritten(); !bytes.Equal(got, wire0) {
		t.Fatalf("the far side got %d bytes, want %d", len(got), len(wire0))
	}
}

// newTestSplicer builds a splicer for the common case: both legs speak the
// binary format.
func newTestSplicer(t *testing.T) *RelaySplicer {
	t.Helper()
	sp, err := NewRelaySplicer(RelaySpliceFormatBinary, RelaySpliceFormatBinary)
	if err != nil {
		t.Fatalf("NewRelaySplicer: %v", err)
	}
	return sp
}

// failingSink is a far side whose write fails after n bytes.
//
// It models the real failure: the far side's connection broke while the
// record was on its way. The splice must report the failure rather than
// pretend the record went out whole.
type failingSink struct {
	spliceSink
	failAfter int
	written   int
}

func (f *failingSink) Write(p []byte) (int, error) {
	if f.written >= f.failAfter {
		return 0, errors.New("the far side is gone")
	}
	n := len(p)
	if f.written+n > f.failAfter {
		n = f.failAfter - f.written
	}
	f.written += n
	_, _ = f.spliceSink.Write(p[:n])
	return n, nil
}

// TestTheSplicerReportsAFailedWriteToTheFarSide checks that a broken far
// side is reported.
//
// The write happens after the budget is charged and after the header has
// gone out, so the far side may be holding a PARTIAL record. The splice
// has to say so: the caller tears the flow down on an error, and a
// silent success would leave the far side waiting for the rest of a
// record that is never coming.
func TestTheSplicerReportsAFailedWriteToTheFarSide(t *testing.T) {
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: bytes.Repeat([]byte{4}, 500)})

	sp := newTestSplicer(t)
	sink := &failingSink{failAfter: 10}
	br := bufio.NewReader(bytes.NewReader(wire0))
	_, err := sp.ForwardOne(br, sink)
	if err == nil {
		t.Fatal("the splice reported success although the far side refused the write")
	}
	if got := sink.written; got >= len(wire0) {
		t.Fatalf("the far side took %d bytes, want fewer than the record's %d", got, len(wire0))
	}
}
