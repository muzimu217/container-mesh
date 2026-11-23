// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"context"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// ---------------------------------------------------------------------------
// The DEPLOYMENT MATRIX.
//
// The switch has to be DEPLOYABLE, and the deployment has an order: the
// ANCHOR is upgraded FIRST.
//
// The reason is structural. The record format is a protocol between the
// two ENDPOINTS of a relay — the anchor is a pipe — and the only version
// negotiation an OLD peer takes part in is the ALPN. So:
//
//   - a NEW anchor can carry BOTH formats: it reads whatever the leg
//     negotiated and translates when the two legs disagree;
//   - an OLD anchor can carry only JSON, and it cannot be taught the
//     binary frame.
//
// The four combinations, and what each one must do:
//
//	new member → new anchor   /2  binary  (the fast path)
//	new member → old anchor   /1  JSON    (the fallback)
//	old member → new anchor   /1  JSON    (the fallback)
//	old member → old anchor   /1  JSON
//
// The matrix is about the ANCHOR's legs, and the ALPN matrix in
// relaysplice_compat_test.go is about the CONNECTION. This one is about
// the DATA PLANE: it drives the whole relay and checks that the bytes
// arrive, in the format the far side can read.
//
// The order constraint that follows: upgrade the ANCHORS first, then the
// members. A new member on an old anchor dies at the first record — the
// one case the ALPN cannot save, because the old anchor offers only the
// old ALPN and the new member has to honour it.
// ---------------------------------------------------------------------------

// TestTheDeploymentMatrixCarriesEveryCombination drives the whole relay
// for each of the four combinations.
//
// The point of the test is that EVERY row works. The switch's value is
// the first row (the fast path); its DEPLOYABILITY is the other three.
func TestTheDeploymentMatrixCarriesEveryCombination(t *testing.T) {
	for _, row := range []struct {
		name string
		// reqALPN is the ALPN the requester's leg negotiates.
		reqALPN string
		// tgtALPN is the ALPN the target's leg negotiates.
		tgtALPN string
	}{
		{"new member, new anchor", RelayALPN2, RelayALPN2},
		{"new member, old anchor", RelayALPN, RelayALPN},
		{"old member, new anchor", RelayALPN, RelayALPN},
		{"old member, old anchor", RelayALPN, RelayALPN},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newTrunkFixture(t)
			reqEnd, reqPeer := newALPNPipePair(row.reqALPN)
			tgtEnd, tgtPeer := newALPNPipePair(row.tgtALPN)

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
			if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
				From: f.alice.ID(),
				To:   f.bob.ID(),
				Hops: 1,
			}, reqEnd); err != nil {
				t.Fatalf("the trunk refused the relay: %v", err)
			}

			reqFmt, err := relayFormatForALPN(row.reqALPN)
			if err != nil {
				t.Fatal(err)
			}
			tgtFmt, err := relayFormatForALPN(row.tgtALPN)
			if err != nil {
				t.Fatal(err)
			}

			// The payload.
			payload := []byte("the request")
			if err := writeRelayRecordAs(reqPeer, relayRecord{
				Type: uint8(wire.FrameMeshRelayData),
				Data: payload,
			}, reqFmt); err != nil {
				t.Fatal(err)
			}

			// The target reads it in ITS leg's format.
			_ = tgtPeer.SetReadDeadline(time.Now().Add(10 * time.Second))
			tgtBr := bufio.NewReader(tgtPeer)
			open, err := readRelayRecordAs(tgtBr, tgtFmt)
			if err != nil {
				t.Fatalf("the target received no open record: %v", err)
			}
			if open.Type != uint8(wire.FrameMeshRelayOpen) {
				t.Fatalf("the target's first record is type %d, want an open record", open.Type)
			}
			rec, err := readRelayRecordAs(tgtBr, tgtFmt)
			if err != nil {
				t.Fatalf("the payload never arrived: %v", err)
			}
			if string(rec.Data) != string(payload) {
				t.Fatalf("the payload came back as %q", rec.Data)
			}
		})
	}
}

// TestTheMixedLegsAreCarriedByTranslation pins the ONE case the matrix
// cannot produce: a relay whose two legs DISAGREE.
//
// The matrix's rows are about a DEPLOYMENT, where both ends of the relay
// are the same version or the anchor is old. But a rollout window has a
// third shape: a new requester on a new anchor reaching an OLD target.
//
// That is the case the anchor has to TRANSLATE for, and it is the case
// the deployment order is chosen to keep as SHORT as possible — the
// targets are upgraded last, precisely because they are the ones that
// stay on the old format longest.
func TestTheMixedLegsAreCarriedByTranslation(t *testing.T) {
	for _, row := range []struct {
		name string
		req  string
		tgt  string
	}{
		{"binary requester, JSON target", RelayALPN2, RelayALPN},
		{"JSON requester, binary target", RelayALPN, RelayALPN2},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newTrunkFixture(t)
			reqEnd, reqPeer := newALPNPipePair(row.req)
			tgtEnd, tgtPeer := newALPNPipePair(row.tgt)

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
			if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
				From: f.alice.ID(),
				To:   f.bob.ID(),
				Hops: 1,
			}, reqEnd); err != nil {
				t.Fatalf("the trunk refused the relay: %v", err)
			}

			reqFmt, err := relayFormatForALPN(row.req)
			if err != nil {
				t.Fatal(err)
			}
			tgtFmt, err := relayFormatForALPN(row.tgt)
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte("the request")
			if err := writeRelayRecordAs(reqPeer, relayRecord{
				Type: uint8(wire.FrameMeshRelayData),
				Data: payload,
			}, reqFmt); err != nil {
				t.Fatal(err)
			}
			_ = tgtPeer.SetReadDeadline(time.Now().Add(10 * time.Second))
			tgtBr := bufio.NewReader(tgtPeer)
			if _, err := readRelayRecordAs(tgtBr, tgtFmt); err != nil {
				t.Fatalf("the target received no open record: %v", err)
			}
			rec, err := readRelayRecordAs(tgtBr, tgtFmt)
			if err != nil {
				t.Fatalf("the payload never arrived: %v", err)
			}
			if string(rec.Data) != string(payload) {
				t.Fatalf("the payload came back as %q", rec.Data)
			}
		})
	}
}
