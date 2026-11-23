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
// The MIXED-VERSION line.
//
// A fallback that is silent is a fallback nobody acts on: the operator
// sees a relay that is slower than it should be and has no way to know
// that the rollout is incomplete. ONE line, naming the two formats, is
// enough.
//
// But the line has to be TRUE. The mixed case has two shapes, and the
// old wording only described one of them:
//
//	requester on JSON, target on binary  → the anchor reads JSON and
//	                                        writes binary frames
//	requester on binary, target on JSON  → the anchor reads binary
//	                                        frames and writes JSON
//
// Both are "the two legs disagree". Neither is "the JSON path" — the
// second one writes JSON but does not read it, and the first one reads
// JSON but does not write it. A line that says "running on the JSON
// path" for a relay that writes BINARY frames is wrong, and an
// operator who reads it will look for the wrong thing.
// ---------------------------------------------------------------------------

// TestTheMixedVersionLineNamesBothFormats pins the line's ACCURACY.
//
// The line has to name the two formats that disagree, because which one
// is the old one is what tells the operator which end to upgrade.
func TestTheMixedVersionLineNamesBothFormats(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  RelaySpliceFormat
		tgt  RelaySpliceFormat
	}{
		{"binary-requester-json-target", RelaySpliceFormatBinary, RelaySpliceFormatJSON},
		{"json-requester-binary-target", RelaySpliceFormatJSON, RelaySpliceFormatBinary},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				}, reqEnd, tc.req, tc.tgt); err != nil {
					t.Fatalf("the trunk refused a mixed-version relay: %v", err)
				}
				_ = writeRelayRecordAs(reqPeer, relayRecord{
					Type: uint8(wire.FrameMeshRelayData), Data: []byte("x"),
				}, tc.req)
				time.Sleep(200 * time.Millisecond)
			})
			if !strings.Contains(out, "JSON") {
				t.Fatalf("the mixed-version relay said nothing about the JSON leg:\n%s", out)
			}
			if !strings.Contains(out, "binary") {
				t.Fatalf("the mixed-version relay did not name the binary leg:\n%s", out)
			}
			// It must not claim the relay is "on the JSON path": in the
			// JSON-requester case the anchor WRITES binary frames, and a
			// line that says otherwise sends the operator looking in the
			// wrong place.
			if strings.Contains(out, "running on the JSON path") {
				t.Fatalf("the line claims the relay runs on the JSON path, which is not "+
					"true of a relay whose target's leg speaks the binary frame:\n%s", out)
			}
		})
	}
}

// TestTheMixedVersionLineIsWrittenOncePerRelay pins the VOLUME.
//
// A line per RECORD would drown the log on a busy anchor: at the
// measured 65.9–67.8 MiB/s the record rate is in the tens of thousands
// per second, and a line each would be a denial of service against the
// operator's disk.
//
// The fact the line reports — the two legs disagree — does not change
// during the relay, so ONE line per relay says everything there is to
// say.
func TestTheMixedVersionLineIsWrittenOncePerRelay(t *testing.T) {
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
	const records = 20
	out := captureRelayLog(t, func() {
		if _, err := trunk.OpenFlowWithFormats(context.Background(), RelayOpen{
			From: f.alice.ID(),
			To:   f.bob.ID(),
			Hops: 1,
		}, reqEnd, RelaySpliceFormatBinary, RelaySpliceFormatJSON); err != nil {
			t.Fatalf("the trunk refused a mixed-version relay: %v", err)
		}
		for i := 0; i < records; i++ {
			_ = writeRelayRecordAs(reqPeer, relayRecord{
				Type: uint8(wire.FrameMeshRelayData), Data: []byte("x"),
			}, RelaySpliceFormatBinary)
		}
		time.Sleep(300 * time.Millisecond)
	})
	lines := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "the requester's leg speaks") {
			lines++
		}
	}
	if lines != 1 {
		t.Fatalf("the mixed-version relay wrote %d lines about its formats, want exactly 1 "+
			"(one per relay, not one per record):\n%s", lines, out)
	}
}

// TestTheTranslationFailureIsLogged pins the operator-visible half of
// the translated path's failure.
//
// On the translated path the record's SIZE changes: the JSON envelope is
// a third bigger than the binary frame. So a writer that ignores the
// cap the anchor stated produces a record the far leg cannot hold, and
// the relay dies.
//
// A relay that dies for no stated reason is a relay an operator cannot
// act on — and the cause here is a MIXED-VERSION DEPLOYMENT, which is
// exactly the thing an operator is trying to get through the rollout
// window. So the line says which leg could not hold the record.
func TestTheTranslationFailureIsLogged(t *testing.T) {
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
	// The requester's leg speaks the binary frame; the target's leg
	// speaks JSON. A binary record at the binary cap does not fit on a
	// JSON leg.
	out := captureRelayLog(t, func() {
		if _, err := trunk.OpenFlowWithFormats(context.Background(), RelayOpen{
			From: f.alice.ID(),
			To:   f.bob.ID(),
			Hops: 1,
		}, reqEnd, RelaySpliceFormatBinary, RelaySpliceFormatJSON); err != nil {
			t.Fatalf("the trunk refused a mixed-version relay: %v", err)
		}
		_ = writeRelayRecordAs(reqPeer, relayRecord{
			Type: uint8(wire.FrameMeshRelayData), Data: make([]byte, maxRelayPlaintext),
		}, RelaySpliceFormatBinary)
		time.Sleep(300 * time.Millisecond)
	})
	if !strings.Contains(out, "could not forward a record") {
		t.Fatalf("the translation failure said nothing in the log; the operator sees a relay "+
			"that died for no stated reason:\n%s", out)
	}
}
