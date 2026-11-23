// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// ---------------------------------------------------------------------------
// A TRUNK THAT IS STOPPING MUST ADMIT NOTHING AND KEEP NOTHING.
//
// Two gaps, both found by an audit and both confirmed on the tip:
//
//  1. THE ADMISSION HOLE. The stage-1 relay (internal/relay) refused
//     connections once it was stopping, but the TRUNK did not — so a
//     connection that arrived in the window between "the stop was
//     requested" and "the listener closed" was adopted. And a flow
//     adopted AFTER the teardown keeps its stream slot forever,
//     because nothing would close it.
//
//  2. NO SHUTDOWN PATH. CloseAll existed — and NOTHING in production
//     called it. So an anchor that stopped left its live relay flows
//     running, and their stream slots reserved: the NEXT relay on the
//     same anchor was refused for lack of a slot that no longer had a
//     flow behind it.
//
// So the shutdown is ONE call that does both halves: set the flag (no
// new admissions) and tear down what is live (the slots come back).
// ---------------------------------------------------------------------------

// TestTheTrunkShutdownRefusesNewRelaysAndFreesItsSlots is the
// regression test.
//
// It asserts the two halves together, because each alone is not enough:
// a trunk that refuses but keeps is leaking, and a trunk that frees but
// still admits is racing its own teardown.
func TestTheTrunkShutdownRefusesNewRelaysAndFreesItsSlots(t *testing.T) {
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

	// Take a stream slot, the way a live relay does. The reservation is
	// what a leaked flow holds.
	if err := trunk.reserveStream(); err != nil {
		t.Fatalf("the first stream should be admitted: %v", err)
	}
	if got := trunk.FlowCount(); got != 1 {
		t.Fatalf("the trunk holds %d slots, want 1", got)
	}

	// The shutdown.
	trunk.Shutdown()

	// ① The slot comes back: a trunk that is stopping must not keep
	// the slots of the flows it tore down.
	if got := trunk.FlowCount(); got != 0 {
		t.Fatalf("the trunk still holds %d slot(s) after its shutdown: "+
			"the next relay would be refused for a slot that no longer has a flow behind it", got)
	}

	// ② No new relay is admitted.
	if err := trunk.reserveStream(); err == nil {
		t.Fatal("a trunk that is stopping admitted a new stream: " +
			"a flow adopted after the teardown would keep its slot forever")
	}
}

// TestTheTrunkIsNotStoppingBeforeItsShutdown is the other half: the
// guard must not refuse a trunk that is running.
//
// A guard that refuses everything would pass the test above.
func TestTheTrunkIsNotStoppingBeforeItsShutdown(t *testing.T) {
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
	if trunk.isStopping() {
		t.Fatal("a trunk that was never shut down reports itself as stopping")
	}
	if err := trunk.reserveStream(); err != nil {
		t.Fatalf("a running trunk refused a stream: %v", err)
	}
	_ = identity.DeviceID("")
	_ = time.Now
	_ = context.Background
}
