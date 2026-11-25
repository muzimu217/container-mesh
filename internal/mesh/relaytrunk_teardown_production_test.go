// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// ★ THE PRODUCTION PATH MUST NOT ADOPT A FLOW AFTER THE TEARDOWN.
// (Round 107.)
//
// `openFlow` — the TEST-ONLY entry point — has had the `stopping` guard
// since Round 98, and `TestAFlowIsNotAdoptedAfterTheTeardown` pins it.
// But the PRODUCTION path (`ServeRequesterWithFirstFormat`, which is
// what the listener actually calls) did NOT have it.
//
// So every existing regression test drove the guarded path and the
// unguarded one was never covered. This is the same shape as the
// "capability with no caller" lesson: the guard existed, was tested,
// and the production path simply did not use it.
//
// The consequence, measured by driving the production path:
//
//  1. the serve goroutine stays blocked on flow.done forever;
//  2. the flow stays in t.flows forever, holding two sessions and a
//     splice goroutine;
//  3. FlowCount() reads 0 while the flow table holds 1 — because
//     Shutdown() zeroed `reserved` — so the quota counter and the real
//     number of live flows are out of step.
//
// This test drives the PRODUCTION entry point on purpose. A test of
// `openFlow` would pass whether or not this defect exists.

// openRecordSession is a requester session whose first read delivers the
// open record, and then goes quiet.
type openRecordSession struct {
	*silentSession
	first []byte
	done  bool
}

func (s *openRecordSession) Read(p []byte) (int, error) {
	if !s.done {
		s.done = true
		n := copy(p, s.first)
		return n, nil
	}
	return s.silentSession.Read(p)
}

func TestTheProductionPathDoesNotAdoptAFlowAfterTheTeardown(t *testing.T) {
	f := newTrunkFixture(t)

	// The trunk's dialer blocks until the test releases it. That is the
	// window: the slot is reserved, the target's session is being
	// opened, and the shutdown happens IN THAT WINDOW.
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

	// ★ THE PRODUCTION ENTRY POINT. The requester writes the open
	// record; the trunk reads it, dials the target, and registers the
	// flow.
	from, to := f.alice.ID(), f.bob.ID()
	var buf bytes.Buffer
	if err := writeRelayRecordAs(&buf, relayRecord{
		Type: uint8(wire.FrameMeshRelayOpen),
		From: from,
		To:   to,
		Kind: "ping",
		Hops: 1,
	}, RelaySpliceFormatBinary); err != nil {
		t.Fatal(err)
	}

	req := &openRecordSession{silentSession: newRequesterSession(), first: buf.Bytes()}
	done := make(chan error, 1)
	go func() {
		done <- trunk.ServeRequester(context.Background(), req, NewJoinerTable())
	}()

	// Wait until the trunk is inside the dial window.
	select {
	case <-dialing:
	case <-time.After(10 * time.Second):
		t.Fatal("the trunk never reached the dial window")
	}

	// The teardown, in that window.
	trunk.Shutdown()

	// Now let the dial finish, so the trunk reaches the registration
	// line — the moment the flow can be adopted.
	close(release)

	// The serve MUST return. Without the guard it stays blocked on
	// flow.done forever.
	select {
	case err := <-done:
		if err == nil {
			t.Log("the serve returned without an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("★ THE DEFECT: the production serve never returned after " +
			"Shutdown() — the relay flow was ADOPTED after the teardown, so the " +
			"serve goroutine is blocked on flow.done forever")
	}

	// The flow table must be empty, and the quota counter must agree
	// with it.
	trunk.mu.Lock()
	live := len(trunk.flows)
	trunk.mu.Unlock()
	if live != 0 {
		t.Fatalf("★ THE DEFECT: after Shutdown() the trunk holds %d flow(s) in its "+
			"table; the flow was adopted after the teardown and will never be closed", live)
	}
	if n := trunk.FlowCount(); n != 0 {
		t.Fatalf("★ THE DEFECT: FlowCount() reads %d after Shutdown(), so the quota "+
			"counter and the real number of live flows are out of step", n)
	}
}
