// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/mesh"
)

// ---------------------------------------------------------------------------
// THE WIRING TEST: THE ANCHOR'S STOP MUST ACTUALLY STOP THE TRUNK.
//
// The trunk's Shutdown() was added in round 96, with a unit test of its own
// (TestTheTrunkShutdownRefusesNewRelaysAndFreesItsSlots). That test is
// green, and the capability is real.
//
// It was still not enough, and the reason is a shape this project has now
// hit three times:
//
//   THE CAPABILITY EXISTS, THE CAPABILITY'S OWN TEST IS GREEN, AND NOBODY
//   CALLS IT.
//
// The symptom is identical to "the capability does not exist", so a green
// unit test of the capability says nothing about the product. What says
// something is a test on the CALLER.
//
// This file is that test. It drives the anchor's REAL stop path — the
// watcher the signal goroutine runs, and the shutdown function it calls —
// and asserts on the trunk's own observable state afterwards:
//
//   ① the trunk is STOPPING (it admits nothing new), and
//   ② the trunk holds NO SLOTS (the slots came back).
//
// Both halves are checked on the REAL trunk through its own state, not
// through a mock: a mock would happily record the call, so the test would
// pass even if the real trunk were not the one being stopped.
//
// If the anchor's stop path stops calling trunk.Shutdown(), this test goes
// red — which is the point. It is written to catch "nobody calls it", NOT
// to prove "it is called right now".
// ---------------------------------------------------------------------------

// stopStubSession is a mesh.Session that never delivers anything.
//
// The trunk's splice reads from BOTH legs in its own goroutines, so the
// stub has to be a session that blocks on a read until it is closed —
// otherwise the flow would end on its own and the test would measure the
// wrong thing.
type stopStubSession struct {
	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

func newStopStubSession() *stopStubSession {
	return &stopStubSession{done: make(chan struct{})}
}

func (s *stopStubSession) Read(p []byte) (int, error) {
	<-s.done
	return 0, io.EOF
}

func (s *stopStubSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func (s *stopStubSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	return nil
}

func (s *stopStubSession) SetReadDeadline(t time.Time) error  { return nil }
func (s *stopStubSession) SetWriteDeadline(t time.Time) error { return nil }

// newStopWiringTrunk builds a live trunk with TWO members besides itself,
// so a relay between them is legal.
//
// The relay to or from the trunk itself is refused on purpose ("the trunk
// does not relay on its own behalf"), so the flow this test opens needs two
// OTHER members.
func newStopWiringTrunk(t *testing.T) (*mesh.RelayTrunk, identity.DeviceID, identity.DeviceID) {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	ids := make([]identity.DeviceID, 0, 3)
	for i := 0; i < 3; i++ {
		dev, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		cert, err := acc.IssueCert(dev, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := members.Add(*cert); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, dev.ID())
	}
	trunk, err := mesh.NewRelayTrunk(mesh.RelayTrunkConfig{
		Self:    ids[0],
		Members: mesh.MemberListAdapter{List: members},
		Enabled: true,
		Joiners: mesh.NewJoinerTable(),
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (mesh.Session, error) {
			return newStopStubSession(), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return trunk, ids[1], ids[2]
}

// TestTheAnchorsStopActuallyStopsTheRelayTrunk is the wiring test.
//
// It is deliberately about the CALLER: the anchor's stop path is invoked,
// and the trunk's state afterwards is what is asserted.
func TestTheAnchorsStopActuallyStopsTheRelayTrunk(t *testing.T) {
	trunk, from, to := newStopWiringTrunk(t)

	// A trunk that carries one relay holds one slot.
	//
	// The slot is taken through the PUBLIC flow API, so the test does not
	// depend on the trunk's internals to arrange the starting state.
	req := newStopStubSession()
	if _, err := trunk.OpenFlow(context.Background(), mesh.RelayOpen{
		From: from,
		To:   to,
		Kind: "ping",
	}, req); err != nil {
		t.Fatalf("the trunk refused to open a relay between two members: %v", err)
	}
	if got := trunk.FlowCount(); got != 1 {
		t.Fatalf("the trunk holds %d slot(s) before the stop, want 1", got)
	}

	// THE ANCHOR'S STOP — the real watcher, the real shutdown function.
	//
	// The watcher is what the signal goroutine runs, so a stop that does
	// not reach the trunk is caught HERE and not only at the signal.
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		watchDiscoveryStop(ctx, func() {}, nil, trunk)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("the anchor's stop watcher did not return within 15s")
	}

	// ① The trunk is stopping: no new relay is admitted.
	//
	// This is the observable an operator would see: the node is going
	// down and it must not take on new work in the meantime.
	if _, err := trunk.OpenFlow(context.Background(), mesh.RelayOpen{
		From: from,
		To:   to,
		Kind: "ping",
	}, newStopStubSession()); err == nil {
		t.Fatal("the anchor stopped, but the trunk still admitted a relay: " +
			"the stop path does not actually stop the trunk")
	}

	// ② The slots came back: a trunk that is stopping must not hold the
	// slots of the work it tore down.
	if got := trunk.FlowCount(); got != 0 {
		t.Fatalf("the trunk still holds %d slot(s) after the anchor's stop: "+
			"the next relay would be refused for a slot that no longer has a flow behind it", got)
	}
}

// TestTheAnchorsStopWithNoTrunkIsHarmless covers the other half of the
// wiring: an anchor that runs WITHOUT a relay trunk must still stop
// cleanly.
//
// A stop path that assumes a trunk exists would panic on every anchor that
// does not relay — and the trunk is OFF by default.
func TestTheAnchorsStopWithNoTrunkIsHarmless(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		watchDiscoveryStop(ctx, func() {}, nil, nil)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("the anchor's stop watcher did not return within 15s")
	}
}
