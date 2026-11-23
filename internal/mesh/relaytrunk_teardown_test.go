// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// ---------------------------------------------------------------------------
// THE TEARDOWN MUST NOT DEPEND ON THE PEER. (Round 98)
//
// Two gaps, both found by an audit on the tip, both confirmed here:
//
//  1. A FLOW ADOPTED AFTER THE TEARDOWN. The `stopping` flag was checked
//     where the SLOT is reserved — but the flow is REGISTERED later,
//     after the target's session has been opened (which can take
//     seconds). Shutdown() walks the live flows and then zeroes the
//     reserved counter. So a flow that reaches the registration line
//     after that walk is neither torn down nor accounted for: it lives
//     forever, invisible to the stream-limit quota.
//
//  2. THE SPLICE'S TEARDOWN WAITED FOR BOTH DIRECTIONS. The close ran
//     AFTER wg.Wait(), so one blocked direction kept the flow — and its
//     stream slot — alive.
//
// The rule they share: the teardown must not depend on the peer.
// ---------------------------------------------------------------------------

// silentSession is a session that never delivers anything.
//
// It models the shape that keeps a relay flow alive: the TARGET accepted
// a connection of its own and then said nothing on it.
//
// ★ THE TWO SIDES NEED DIFFERENT SEMANTICS, and getting that wrong is
// what made the first version of this test prove nothing.
//
//   - The REQUESTER's session: when the requester is done sending, the
//     trunk's read on it must see EOF (the requester half-closed). So
//     this session's READ ends when its send side ends.
//
//   - The TARGET's session: the target said nothing and stays that way.
//     The trunk's half-close of the target's SEND side must NOT end the
//     target's READ — on a QUIC stream a FIN on one direction leaves the
//     other direction open, and that is exactly the Session contract's
//     half close.
//
// So the two are separate types, not one type used twice.
type silentSession struct {
	// readEndsOnSendClose makes the READ side end when the send side
	// ends. TRUE for the requester, FALSE for the target.
	readEndsOnSendClose bool
	sendClosed          chan struct{}
	sendOnce            sync.Once
	closed              chan struct{}
	closeOnce           sync.Once
}

func newRequesterSession() *silentSession {
	return &silentSession{
		readEndsOnSendClose: true,
		sendClosed:          make(chan struct{}),
		closed:              make(chan struct{}),
	}
}

func newTargetSession() *silentSession {
	return &silentSession{
		readEndsOnSendClose: false,
		sendClosed:          make(chan struct{}),
		closed:              make(chan struct{}),
	}
}

func (s *silentSession) Read(p []byte) (int, error) {
	if s.readEndsOnSendClose {
		select {
		case <-s.sendClosed:
			return 0, io.EOF
		case <-s.closed:
			return 0, io.EOF
		}
	}
	// The target never says anything: only a full abort ends its read.
	<-s.closed
	return 0, io.EOF
}

func (s *silentSession) Write(p []byte) (int, error) {
	// ★ A HALF-CLOSED SESSION CAN STILL BE WRITTEN TO. (Round 98.)
	//
	// The Session contract says Close "ends the SEND side (a FIN). The
	// receive side stays open." So a session whose send side has ended
	// still accepts writes — that is exactly the request/response shape:
	// "send the request, FIN, wait for the answer", and the ANSWER comes
	// back on the same session.
	//
	// The first version of this double failed the write once the send
	// side closed, which made the trunk tear the flow down ("relay
	// forwarding write failed") instead of letting the second direction
	// carry on. The double was stricter than the protocol, and the test
	// was measuring that.
	select {
	case <-s.closed:
		return 0, errors.New("session closed")
	default:
	}
	return len(p), nil
}

// Close ends the SEND side only. The receive side stays open — that is
// the Session contract's half close.
func (s *silentSession) Close() error {
	s.sendOnce.Do(func() { close(s.sendClosed) })
	return nil
}

// Abort ends BOTH sides.
func (s *silentSession) Abort() error {
	s.closeOnce.Do(func() { close(s.closed) })
	s.sendOnce.Do(func() { close(s.sendClosed) })
	return nil
}

func (s *silentSession) SetReadDeadline(t time.Time) error  { return nil }
func (s *silentSession) SetWriteDeadline(t time.Time) error { return nil }

// TestAFlowIsNotAdoptedAfterTheTeardown is the regression test for gap 1.
//
// It reproduces the exact window: the slot is taken, the target's
// session is being opened, and the shutdown happens IN THAT WINDOW.
func TestAFlowIsNotAdoptedAfterTheTeardown(t *testing.T) {
	f := newTrunkFixture(t)

	// The trunk's dialer blocks until the test releases it. That is the
	// window: the slot is reserved, and the flow is not yet registered.
	release := make(chan struct{})
	dialing := make(chan struct{}, 1)

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: NewJoinerTable(),
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			select {
			case dialing <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return newTargetSession(), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The relay requester is one of the other members (a relay to or
	// from the trunk itself is refused on purpose).
	from, to := f.alice.ID(), f.bob.ID()

	done := make(chan error, 1)
	go func() {
		_, err := trunk.OpenFlow(context.Background(), RelayOpen{
			From: from,
			To:   to,
			Kind: "ping",
		}, newRequesterSession())
		done <- err
	}()

	// Wait until the slot is taken and the dial is in progress.
	select {
	case <-dialing:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay never reached the dial step")
	}
	if got := trunk.FlowCount(); got != 1 {
		t.Fatalf("the trunk holds %d slot(s) while the dial is in flight, want 1", got)
	}

	// THE SHUTDOWN, INSIDE THE WINDOW.
	trunk.Shutdown()

	// Let the dial finish, so the flow reaches the registration line.
	close(release)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a relay was adopted after the trunk's shutdown: " +
				"the flow would live forever, invisible to the stream-limit quota")
		}
		if !errors.Is(err, ErrRelayUnavailable) {
			t.Fatalf("the refused relay failed with %v, want ErrRelayUnavailable", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the relay never returned after the shutdown")
	}

	// AND THE SLOT MUST BE BACK: the shutdown handed it back, and the
	// refused flow must not take it again.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if trunk.FlowCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the trunk still holds %d slot(s) after the shutdown: "+
		"a flow adopted after the teardown keeps its slot forever", trunk.FlowCount())
}

// TestTheSpliceTearsDownEvenWhenOneSideIsSilent is the regression test
// for gap 2.
//
// The requester half-closes and the target never answers. The splice
// used to wait for BOTH directions before it closed the flow — so the
// flow, and its slot, stayed alive.
//
// The QUIC layer's own idle timeout eventually reclaims such a
// connection, but that is the transport's business; the relay's own
// teardown must not wait for the peer.
func TestTheSpliceTearsDownEvenWhenOneSideIsSilent(t *testing.T) {
	f := newTrunkFixture(t)

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: NewJoinerTable(),
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			// The target's session never delivers anything.
			return newTargetSession(), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := newRequesterSession()
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

	// The requester half-closes. The target stays silent.
	_ = req.Close()

	// The flow must be torn down anyway — the teardown must not depend
	// on the peer.
	deadline := time.Now().Add(relayTeardownGrace + 10*time.Second)
	for time.Now().Before(deadline) {
		if trunk.FlowCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the trunk still holds %d slot(s) after the requester half-closed: "+
		"the splice waited for a peer that never answered", trunk.FlowCount())
}
