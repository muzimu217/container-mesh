// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// ---------------------------------------------------------------------------
// A RELAY THAT IS REFUSED MUST LEAVE A TRACE ON THE ANCHOR.
//
// The requester learns WHY (the answer carries the reason and the
// class), but until now the ANCHOR said nothing: a relay refused at 3am
// left no trace on the node that refused it.
//
// The operator's only clue was the requester's error message on a
// DIFFERENT machine — which is exactly the information the trunk exists
// to centralise, thrown away.
//
// This is the same family as the teardown reason (which IS logged): a
// failure the anchor knows about and does not say.
// ---------------------------------------------------------------------------

// TestARefusedRelaySaysWhyOnTheAnchor is the regression test.
//
// The refusal must be reported on the anchor's own log, with the class
// (so the operator can tell "the peer was refused" from "the node is
// full") and the requester's identity (so the operator can tell WHICH
// one).
func TestARefusedRelaySaysWhyOnTheAnchor(t *testing.T) {
	f := newTrunkFixture(t)

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: NewJoinerTable(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// A requester that is NOT a member of the view: the anchor must
	// refuse it, and must say so.
	outsider, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	req, reqPeer := newPipeSessionPair()
	done := make(chan error, 1)
	go func() {
		done <- trunk.ServeRequester(context.Background(), req, NewJoinerTable())
	}()

	out := captureRelayLog(t, func() {
		_ = reqPeer.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := writeRelayRecord(reqPeer, relayRecord{
			Type: uint8(wire.FrameMeshRelayOpen),
			From: outsider.ID(),
			To:   f.bob.ID(),
			Hops: 1,
			Kind: KindMsg,
		}); err != nil {
			t.Fatalf("the requester could not ask for a relay: %v", err)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the anchor did not finish serving the refused requester")
		}
	})

	if !strings.Contains(out, "refused") {
		t.Fatalf("the anchor refused a relay and said nothing about it:\n%s", out)
	}
	// The class matters: it tells the operator whether the PEER was
	// refused or the NODE was full.
	if !strings.Contains(out, RelayClassAuth) {
		t.Fatalf("the refusal did not name its class (%s):\n%s", RelayClassAuth, out)
	}
	// And the requester's identity, so the operator can tell WHICH one.
	if !strings.Contains(out, string(outsider.ID())) {
		t.Fatalf("the refusal did not name the requester:\n%s", out)
	}
}
