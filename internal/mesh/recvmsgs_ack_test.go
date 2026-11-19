// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"context"
	"encoding/binary"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// The message receiver's ACK is a DELIVERY RECEIPT, and the whole
// protocol hangs on that word.
//
// The sender writes its frames, half-closes, and waits for one line.
// "done" means "I consumed every frame you sent". The sender treats it
// as proof of delivery and reports success; the operator reads
// "delivered 1 message(s)".
//
// So the receiver must answer "done" ONLY when it really did read every
// frame. Answering "done" after a frame it REFUSED or never received is
// a false delivery receipt: the sender reports a message delivered that
// the target never saw. That is worse than an outright failure, because
// the failure is silent.
//
// The bug this file pins: the receiver's read loop used to `break` on ANY
// read error and then write "done" — so a frame over the cap, a frame
// truncated mid-body, or a reset connection all produced "done" and a
// successful-looking send.
//
// THE HARD PART is telling a normal end apart from a broken frame.
//
// The sender's normal half-close (the FIN that ends the message stream)
// and a truncated frame both surface as an error from a plain read. Worse,
// io.ReadFull reports io.EOF for BOTH "the stream ended before the
// header" and "the stream ended before the body" — so a receiver that
// only checks `err == io.EOF` still answers "done" to a frame whose
// body never arrived. The distinction has to be made where the header
// and the body are read separately.
//
// The failure line is "err: <reason>" — the same convention the file
// handler already uses ("err: receiver has no inbox configured"). It
// cannot be confused with the receipt: "done" and "err:" share no
// prefix, so a sender that accepts only "done" can never mistake a
// failure for a delivery.

// wantMsgErrPrefix is the prefix the receiver's FAILURE line must carry.
//
// It is written as a literal here on purpose: this file pins the WIRE
// FORMAT, so the expectation must not be taken from the implementation
// it is testing (a test that reads the constant it is checking cannot
// fail when the constant changes).
const wantMsgErrPrefix = "err:"

// frameLengthPrefix builds the 4-byte big-endian length prefix of a
// length-prefixed frame, without the body.
func frameLengthPrefix(n int) []byte {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(n))
	return l[:]
}

// msgRecvHarness drives receiveMsgs over an in-memory session pair and
// reports what the receiver wrote back and whether it failed.
type msgRecvHarness struct {
	local  *memSession
	remote *memSession

	mu        sync.Mutex
	delivered []string

	errCh chan error
}

// newMsgRecvHarness starts a receiveMsgs handler on one end of an
// in-memory session pair.
func newMsgRecvHarness(t *testing.T) *msgRecvHarness {
	t.Helper()
	local, remote := newMemSessionPair()
	h := &msgRecvHarness{local: local, remote: remote, errCh: make(chan error, 1)}
	go func() {
		h.errCh <- receiveMsgs(
			identity.DeviceID("peer00000000000"),
			bufio.NewReader(remote),
			remote,
			func(from identity.DeviceID, text string) {
				h.mu.Lock()
				h.delivered = append(h.delivered, text)
				h.mu.Unlock()
			})
	}()
	return h
}

// waitErr waits for the receiver to finish and returns its error.
func (h *msgRecvHarness) waitErr(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.errCh:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the receiver never finished")
		return nil
	}
}

// ack reads the receiver's answer line.
func (h *msgRecvHarness) ack(t *testing.T) string {
	t.Helper()
	return h.local.readLine(t, 4096)
}

// deliveredMessages returns the texts the receiver handed to onMsg.
func (h *msgRecvHarness) deliveredMessages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.delivered))
	copy(out, h.delivered)
	return out
}

// assertFailedAck checks the two things a failure answer must satisfy:
// it is NOT the success receipt, and it says what went wrong.
func assertFailedAck(t *testing.T, ack string) {
	t.Helper()
	if strings.HasPrefix(ack, "done") {
		t.Fatalf("the receiver answered %q — that is the DELIVERY RECEIPT for a frame it never read", ack)
	}
	if !strings.HasPrefix(ack, wantMsgErrPrefix) {
		t.Fatalf("the receiver answered %q; want a %q line", ack, wantMsgErrPrefix)
	}
}

// TestReceiveMsgsRefusesAnOverCapFrame: a frame over the cap must not
// produce a delivery receipt.
//
// The receiver reads the 4-byte length prefix, sees a length above
// MaxMsgBytes, and refuses the frame. It must NOT answer "done" (the
// frame was never delivered), it must NOT hand anything to onMsg, and it
// must report a failure so the sender can tell the operator.
func TestReceiveMsgsRefusesAnOverCapFrame(t *testing.T) {
	h := newMsgRecvHarness(t)

	if _, err := h.local.Write(frameLengthPrefix(MaxMsgBytes + 1)); err != nil {
		t.Fatal(err)
	}
	// The sender half-closes, exactly as SendMsg does.
	if err := h.local.Close(); err != nil {
		t.Fatal(err)
	}

	if err := h.waitErr(t); err == nil {
		t.Fatal("the receiver accepted an over-cap frame; it must report a failure")
	}
	assertFailedAck(t, h.ack(t))
	if got := h.deliveredMessages(); len(got) != 0 {
		t.Fatalf("the receiver delivered %q; it must deliver nothing", got)
	}
}

// TestReceiveMsgsAnswersDoneOnANormalHalfClose: the normal end of the
// message protocol must keep its receipt.
//
// This is the behaviour the whole protocol rests on, and several
// existing tests depend on it. A fix that made the receiver answer
// "err:" at the end of every message stream would be worse than the bug
// it fixed: every send would look like a failure.
func TestReceiveMsgsAnswersDoneOnANormalHalfClose(t *testing.T) {
	h := newMsgRecvHarness(t)

	if err := writeFrame(h.local, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := h.local.Close(); err != nil {
		t.Fatal(err)
	}

	if err := h.waitErr(t); err != nil {
		t.Fatalf("a normal half-close must not be an error: %v", err)
	}
	if ack := h.ack(t); ack != "done" {
		t.Fatalf("the receiver answered %q, want %q", ack, "done")
	}
	got := h.deliveredMessages()
	if len(got) != 1 || got[0] != "first" {
		t.Fatalf("the receiver delivered %q, want [%q]", got, "first")
	}
}

// TestReceiveMsgsRefusesATruncatedFrame: a frame whose body was cut
// short must not produce a delivery receipt.
//
// The length prefix promised N bytes and fewer than N arrived. The
// sender is gone; the message is NOT delivered, and "done" would be a
// lie.
//
// The partial body is the case that makes io.ReadFull report
// io.ErrUnexpectedEOF, so it is the easy half of this test.
func TestReceiveMsgsRefusesATruncatedFrame(t *testing.T) {
	h := newMsgRecvHarness(t)

	// A header promising 100 bytes, followed by only 10 of them.
	raw := frameLengthPrefix(100)
	raw = append(raw, []byte("0123456789")...)
	if _, err := h.local.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := h.local.Close(); err != nil {
		t.Fatal(err)
	}

	if err := h.waitErr(t); err == nil {
		t.Fatal("the receiver accepted a truncated frame; it must report a failure")
	}
	assertFailedAck(t, h.ack(t))
	if got := h.deliveredMessages(); len(got) != 0 {
		t.Fatalf("the receiver delivered %q; it must deliver nothing", got)
	}
}

// TestReceiveMsgsRefusesAFrameWithNoBodyAtAll is the case that a naive
// `err == io.EOF` check gets WRONG.
//
// io.ReadFull reports io.EOF — not io.ErrUnexpectedEOF — when the body
// is COMPLETELY absent and the stream then ends. So a receiver that
// only asks "was the error io.EOF?" cannot tell this apart from the
// sender's normal half-close, and answers "done" for a frame whose
// body never arrived.
//
// The test exists to keep that distinction sharp.
func TestReceiveMsgsRefusesAFrameWithNoBodyAtAll(t *testing.T) {
	h := newMsgRecvHarness(t)

	// A header promising 100 bytes and NOTHING else: the sender hung up
	// between the header and the body.
	if _, err := h.local.Write(frameLengthPrefix(100)); err != nil {
		t.Fatal(err)
	}
	if err := h.local.Close(); err != nil {
		t.Fatal(err)
	}

	if err := h.waitErr(t); err == nil {
		t.Fatal("the receiver accepted a frame with no body; it must report a failure")
	}
	assertFailedAck(t, h.ack(t))
	if got := h.deliveredMessages(); len(got) != 0 {
		t.Fatalf("the receiver delivered %q; it must deliver nothing", got)
	}
}

// TestReceiveMsgsRefusesAPartialHeader: even the length prefix must be
// complete. A partial prefix is a broken frame, not a close.
func TestReceiveMsgsRefusesAPartialHeader(t *testing.T) {
	h := newMsgRecvHarness(t)

	if _, err := h.local.Write([]byte{0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	if err := h.local.Close(); err != nil {
		t.Fatal(err)
	}

	if err := h.waitErr(t); err == nil {
		t.Fatal("the receiver accepted a partial frame header; it must report a failure")
	}
	assertFailedAck(t, h.ack(t))
	if got := h.deliveredMessages(); len(got) != 0 {
		t.Fatalf("the receiver delivered %q; it must deliver nothing", got)
	}
}

// TestMsgFailureLineNeverCollidesWithDone: the failure line and the
// delivery receipt must not be confusable, in either direction.
//
// This is a property of the WIRE FORMAT, so it is pinned with literals:
// the receiver's failure line and the receipt are two different strings,
// and neither is a prefix of the other.
func TestMsgFailureLineNeverCollidesWithDone(t *testing.T) {
	if strings.HasPrefix(wantMsgErrPrefix, "done") {
		t.Fatalf("the failure line %q must not look like the receipt", wantMsgErrPrefix)
	}
	if strings.HasPrefix("done", wantMsgErrPrefix) {
		t.Fatalf("the receipt must not look like the failure line %q", wantMsgErrPrefix)
	}
}

// TestCheckMsgAck: the SENDER's verdict on the receiver's answer.
//
// The old code was `if !bytes.HasPrefix(ack, []byte("done")) { return
// error }`, which already rejected a non-"done" answer — but it read
// the answer with a 16-BYTE cap, so a failure line carrying a reason
// ("err: frame of 65537 bytes exceeds the 65536 byte cap") was cut to
// "line too long" and the operator lost the reason.
//
// These cases pin the judgement itself.
func TestCheckMsgAck(t *testing.T) {
	t.Run("the receipt is success", func(t *testing.T) {
		if err := checkMsgAck([]byte("done")); err != nil {
			t.Fatalf("the receipt must be accepted: %v", err)
		}
	})
	t.Run("an err line is a failure that carries the reason", func(t *testing.T) {
		err := checkMsgAck([]byte("err: mesh: frame of 65537 bytes exceeds the 65536 byte cap"))
		if err == nil {
			t.Fatal("an err line must be a failure")
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("the failure must carry the receiver's reason: %v", err)
		}
	})
	t.Run("an unknown answer is a failure", func(t *testing.T) {
		if err := checkMsgAck([]byte("something else")); err == nil {
			t.Fatal("an unknown answer must be a failure")
		}
	})
	t.Run("an empty answer is a failure", func(t *testing.T) {
		if err := checkMsgAck(nil); err == nil {
			t.Fatal("an empty answer must be a failure")
		}
	})
}

// TestSendMsgReadsAFailureLineWithItsReason: the sender must be able to
// read the receiver's REASON, not just "the answer was too long".
//
// The ack read used to be capped at 16 bytes, which is enough for the
// four-byte receipt and nothing else: every failure line was reported
// as "line too long", hiding the only information the operator needed.
func TestSendMsgReadsAFailureLineWithItsReason(t *testing.T) {
	// A receiver's failure line, with a reason.
	reason := "err: mesh: frame of 65537 bytes exceeds the 65536 byte cap"
	if len(reason) <= 16 {
		t.Fatalf("the test's reason must be longer than the old 16-byte cap: %q", reason)
	}
	// The sender reads the answer with the cap SendMsg uses.
	ack, err := readLine(bufio.NewReader(strings.NewReader(reason+"\n")), MaxHelloBytes)
	if err != nil {
		t.Fatalf("the failure line could not be read: %v", err)
	}
	if string(ack) != reason {
		t.Fatalf("the failure line read back as %q, want %q", ack, reason)
	}
	// And the OLD cap would have thrown the reason away.
	if _, err := readLine(bufio.NewReader(strings.NewReader(reason+"\n")), 16); err == nil {
		t.Fatalf("a 16-byte cap must NOT have read the whole reason %q", reason)
	}
}

// TestReceiveMsgsOverCapOverTheRealDirectPath: the same thing over the
// real QUIC direct path, and the shape the operator actually hits.
//
// A peer that puts a frame over the cap on the wire must get a FAILURE
// answer, not "done". Before the fix the receiver answered "done" and
// the sender reported a delivery that never happened.
func TestReceiveMsgsOverCapOverTheRealDirectPath(t *testing.T) {
	f := newDirectFixture(t)
	delivered := make(chan string, 4)
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
		OnMessage: func(from identity.DeviceID, text string) {
			delivered <- text
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	link, err := DialDirect(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        l.BoundAddrs(),
		Kind:             KindMsg,
		HandshakeTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	// Put a frame over the cap on the wire. SendMsg would never do this
	// (it refuses an over-long message locally), so the frame is written
	// by hand — but a peer that does not apply that guard is exactly
	// what the receiver has to survive.
	if _, err := link.Session().Write(frameLengthPrefix(MaxMsgBytes + 1)); err != nil {
		t.Fatal(err)
	}
	if err := link.Session().Close(); err != nil {
		t.Fatal(err)
	}

	ack, err := readLine(link.br, MaxHelloBytes)
	if err != nil {
		t.Fatalf("the receiver never answered: %v", err)
	}
	assertFailedAck(t, string(ack))
	select {
	case text := <-delivered:
		t.Fatalf("the target received %q; it must receive nothing", text)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSendMsgReportsSuccessOnTheNormalPath: the fix must not break the
// ordinary send. The receiver's "done" is still the success answer.
func TestSendMsgReportsSuccessOnTheNormalPath(t *testing.T) {
	f := newDirectFixture(t)
	delivered := make(chan string, 4)
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
		OnMessage: func(from identity.DeviceID, text string) {
			delivered <- text
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	if err := SendMsg(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        l.BoundAddrs(),
		HandshakeTimeout: 10 * time.Second,
	}, []string{"hello"}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	select {
	case text := <-delivered:
		if text != "hello" {
			t.Fatalf("the target received %q, want %q", text, "hello")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the message never arrived")
	}
}
