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
// A RELAY THAT DIES MUST SAY WHY.
//
// The audit found that tearDownFlow took a `reason` and never used it.
// Every caller passes one — "relay peer failed", "a member of this
// relay was revoked", "the trunk is shutting down", "the grant could
// not be written" — and the function dropped it on the floor.
//
// The consequence: a relay could die for ANY reason and the anchor's
// log would show NOTHING. The operator's only clue was the peer's
// side, and a peer that had vanished had no side.
//
// This is the same class as the product's other "silent death" bugs
// (a truncated record read as a clean end; a refused connection closed
// with "bye" and code 0): the failure is real, and nothing says so.
//
// The reason is a FIXED STRING chosen by this package. It carries no
// payload, no peer text and no error text, so it cannot leak business
// content into the log — the same rule the rest of this file follows
// (report the error CLASS, not its text).
// ---------------------------------------------------------------------------

// TestATornDownRelaySaysWhy is the regression test.
//
// The teardown reason must reach the log. The test tears a flow down
// with a reason and asserts the reason is in the log.
func TestATornDownRelaySaysWhy(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	out := captureRelayLog(t, func() {
		trunk.closeFlowForced(4242, "a member of this relay was revoked")
	})

	if !strings.Contains(out, "a member of this relay was revoked") {
		t.Fatalf("the relay was torn down and the log did not say why:\n%s", out)
	}
	if !strings.Contains(out, "4242") {
		t.Fatalf("the log did not name the relay that was torn down:\n%s", out)
	}
}

// TestAFinishedRelayDoesNotSayWhy pins the VOLUME.
//
// The ordinary end of a relay is not a death. At the measured
// 65.9–67.8 MiB/s the relay rate is in the tens of thousands per
// second, and a line for each would be a denial of service against the
// operator's disk.
//
// So the NORMAL end stays quiet, and only the abnormal teardowns are
// reported.
func TestAFinishedRelayDoesNotSayWhy(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	out := captureRelayLog(t, func() {
		trunk.closeFlow(4243, "relay finished")
	})

	if strings.Contains(out, "torn down") {
		t.Fatalf("a relay that finished normally was reported as a teardown:\n%s", out)
	}
}

// TestTheTeardownReasonCarriesNoPeerText pins the OTHER half of the
// rule: the reason must be the package's own fixed string, never
// something a peer chose.
//
// A peer's text could carry business content (a file name, a message
// body). The log is not the place for it. So the reason is a constant
// from this file, and the test asserts that the teardown line carries
// the reason but not anything from the peers.
func TestTheTeardownReasonCarriesNoPeerText(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	out := captureRelayLog(t, func() {
		trunk.closeFlowForced(4244, "relay peer failed")
	})

	if !strings.Contains(out, "relay peer failed") {
		t.Fatalf("the teardown did not report its reason:\n%s", out)
	}
	// The peers' device IDs are not in the reason, and the teardown
	// line must not invent them.
	if strings.Contains(out, string(f.alice.ID())) || strings.Contains(out, string(f.bob.ID())) {
		t.Fatalf("the teardown line carries a peer identity, which the reason is not "+
			"supposed to contain:\n%s", out)
	}
}

// TestTheTrunkSaysWhyWhenThePeerFails is the END-TO-END shape: a relay
// whose peer dies must produce a line, without the test reaching into
// the trunk's internals.
//
// The peer here is a pipe that is closed under the relay's feet. The
// relay's forwarding loop then fails, and the failure must reach the
// log.
func TestTheTrunkSaysWhyWhenThePeerFails(t *testing.T) {
	f := newTrunkFixture(t)
	reqEnd, reqPeer := newPipeSessionPair()
	tgtEnd, _ := newPipeSessionPair()

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

	out := captureRelayLog(t, func() {
		if _, err := trunk.OpenFlowWithFormats(context.Background(), RelayOpen{
			From: f.alice.ID(),
			To:   f.bob.ID(),
			Hops: 1,
		}, reqEnd, RelaySpliceFormatBinary, RelaySpliceFormatBinary); err != nil {
			t.Fatalf("the trunk refused the relay: %v", err)
		}
		// The requester sends a record the leg cannot read: a JSON
		// envelope on a leg that negotiated the binary frame.
		//
		// That is a HARD failure (the splice refuses it with
		// ErrRelaySpliceFormat), not the graceful end a half close
		// would produce — and a hard failure is what must be reported.
		if err := writeRelayRecordAs(reqPeer, relayRecord{
			Type: uint8(wire.FrameMeshRelayData),
			Data: []byte("a payload in the wrong format"),
		}, RelaySpliceFormatJSON); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
	})

	if !strings.Contains(out, "torn down") {
		t.Fatalf("a relay whose peer sent an unreadable record left no trace on the anchor:\n%s", out)
	}
	if !strings.Contains(out, "relay peer failed") {
		t.Fatalf("the teardown did not say WHY the relay died:\n%s", out)
	}
}
