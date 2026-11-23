// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// ---------------------------------------------------------------------------
// A SLOW FLOW IS NOT A STUCK FLOW. (Round 98.)
//
// The splice's teardown gives the second direction a grace period, so a
// peer that never answers cannot hold a stream slot forever.
//
// The FIRST version of that grace was a WALL-CLOCK deadline: "if the
// other direction has not finished in ten seconds, tear the flow down".
// A wall-clock deadline cannot tell a SLOW transfer from a STUCK one — so
// on a loaded machine it tore down transfers that were perfectly healthy.
//
// And the teardown is not harmless: it writes a FrameMeshRelayClose to
// the target, which ends the target's side of the JOIN connection. That is
// the poisoning the relay's own big-file test exists to catch, and it
// showed up as a 15-second failure that looked like a network problem.
//
// So the deadline is measured from the LAST BYTE. A flow that is moving
// is never torn down, however slow it is.
//
// This test pins that distinction: a flow that keeps moving past the
// grace period must SURVIVE.
// ---------------------------------------------------------------------------

// slowButMovingSession delivers ONE COMPLETE RECORD every now and then.
//
// It models a transfer that is slow because the machine is busy — not a
// peer that has gone away.
//
// ★ IT MUST DELIVER WHOLE RECORDS. (Round 98.) The first version of this
// double delivered a single byte per tick, and that made the test prove
// nothing: the trunk's forwarder reads a LENGTH-PREFIXED record, so
// `io.ReadFull(br, head[:4])` never got its four bytes, the first
// direction never ENDED, and the teardown — the thing under test — was
// never even reached. The test passed with the defect present.
//
// The lesson is the same one the project keeps meeting: a double that
// does not model the protocol measures nothing.
type slowButMovingSession struct {
	interval time.Duration
	// record is what each tick delivers.
	record []byte
	// pending holds the tail of a record the last Read did not fit.
	pending  []byte
	mu       sync.Mutex
	closeOne sync.Once
	closed   chan struct{}
	aborted  chan struct{}
	abortOne sync.Once
}

// newSlowButMovingSession builds a session that delivers one whole relay
// record every interval.
//
// The record is built with the package's OWN encoder AND written through
// the package's OWN framing, so the double cannot drift from the format
// the trunk reads.
//
// ★ THE LENGTH PREFIX MATTERS. (Round 98.) The first version of this
// double delivered the bare frame, without the four-byte length prefix
// that writeFrame puts in front of it. The splicer reads that prefix
// first, so it read the frame's first four bytes as a length, got a
// nonsense number, and reported "the length prefix is truncated" — the
// flow died of a MALFORMED RECORD, not of the teardown the test is
// about. **The test was measuring the wrong failure.**
func newSlowButMovingSession(t *testing.T, interval time.Duration) *slowButMovingSession {
	t.Helper()
	var buf bytes.Buffer
	if err := writeRelayRecordAs(&buf, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: []byte("slow but moving"),
	}, RelaySpliceFormatBinary); err != nil {
		t.Fatal(err)
	}
	return &slowButMovingSession{
		interval: interval,
		record:   buf.Bytes(),
		closed:   make(chan struct{}),
		aborted:  make(chan struct{}),
	}
}

func (s *slowButMovingSession) Read(p []byte) (int, error) {
	s.mu.Lock()
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		s.mu.Unlock()
		return n, nil
	}
	s.mu.Unlock()

	// ★ A HALF CLOSE DOES NOT END THE READ SIDE. (Round 98.)
	//
	// The first version of this double ended its read on `closed` — which
	// the trunk reaches by half-closing the target when the requester's
	// direction ends. So the half close killed the read side, the second
	// direction ended at once, and the flow was torn down by the ORDINARY
	// path — which is not the path under test.
	//
	// The Session contract says it plainly: "Close ends the SEND side (a
	// FIN). The receive side stays open." Only a full abort ends the
	// read side.
	select {
	case <-s.aborted:
		return 0, io.EOF
	default:
	}

	// Wait for the next tick, then deliver the WHOLE record.
	t := time.NewTimer(s.interval)
	defer t.Stop()
	select {
	case <-t.C:
		s.mu.Lock()
		s.pending = s.record
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		s.mu.Unlock()
		return n, nil
	case <-s.aborted:
		return 0, io.EOF
	}
}

func (s *slowButMovingSession) Write(p []byte) (int, error) {
	select {
	case <-s.aborted:
		return 0, errors.New("session aborted")
	case <-s.closed:
		return 0, errors.New("session closed")
	default:
	}
	return len(p), nil
}

// Close ends the send side only.
func (s *slowButMovingSession) Close() error {
	s.closeOne.Do(func() { close(s.closed) })
	return nil
}

// Abort ends both sides.
func (s *slowButMovingSession) Abort() error {
	s.abortOne.Do(func() { close(s.aborted) })
	s.closeOne.Do(func() { close(s.closed) })
	return nil
}

func (s *slowButMovingSession) SetReadDeadline(t time.Time) error  { return nil }
func (s *slowButMovingSession) SetWriteDeadline(t time.Time) error { return nil }

// TestASlowButMovingFlowIsNotTornDown is the regression test.
//
// ★ THE SHAPE, AND WHY IT IS THIS SHAPE. (Round 98.)
//
// The splice has two directions: one reads the REQUESTER and writes the
// target, the other reads the TARGET and writes the requester.
//
// The teardown waits for the FIRST direction to end, then gives the
// second one a grace period. So for the teardown to be exercised at all,
// the first direction must END — and the second one must be the one that
// is slow.
//
// That means the TARGET is the slow-but-moving side (its direction is the
// second one) and the REQUESTER half-closes (so the first direction ends
// on its own).
//
// The first version of this test had it the other way round: the
// requester was the slow one. Then the first direction never ended, the
// teardown was never reached, and the test passed with the defect
// present. **The test was measuring a code path it never entered.**
//
// What the test asserts: the flow keeps moving for LONGER than the grace
// period, and it must SURVIVE. A flow that is carrying bytes is slow,
// not stuck.
func TestASlowButMovingFlowIsNotTornDown(t *testing.T) {
	f := newTrunkFixture(t)

	// The TARGET delivers a record every 200 ms — slow, but moving.
	tgt := newSlowButMovingSession(t, 200*time.Millisecond)
	// The REQUESTER delivers nothing and half-closes: the first
	// direction ends at once.
	//
	// ★ IT MUST BE THE REQUESTER-SHAPED SESSION. (Round 98.) The
	// requester's read side ends when its send side ends — that is the
	// half close — so the trunk's read on it sees EOF and the first
	// direction ends. The target-shaped session's read side stays open,
	// so the direction would never end and the teardown would never be
	// reached.
	req := newRequesterSession()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: NewJoinerTable(),
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tgt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Kind: "ping",
	}, req); err != nil {
		t.Fatalf("the trunk refused to open a relay: %v", err)
	}
	if got := trunk.FlowCount(); got != 1 {
		t.Fatalf("the trunk holds %d slot(s), want 1", got)
	}

	// The requester half-closes: the FIRST direction ends, and the
	// second one (the target's) is left to carry on — slowly.
	//
	// This is the shape that exercises the teardown at all.
	_ = req.Close()

	// Wait LONGER than the grace period. A wall-clock deadline would
	// have torn the flow down by now; an IDLE deadline does not, because
	// the flow is still moving.
	deadline := time.Now().Add(relayTeardownGrace + 3*time.Second)
	for time.Now().Before(deadline) {
		if trunk.FlowCount() == 0 {
			t.Fatal("a flow that was still MOVING bytes was torn down: " +
				"the teardown cannot tell a slow transfer from a stuck one")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
