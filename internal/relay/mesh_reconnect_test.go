// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

import (
	"net/http"
	"testing"
	"time"
)

// TestMeshMembersReconnectDoesNotHotPush pins the F-1 fix.
//
// A member that reconnects opens a second members stream. The old stream
// must be orphaned, not left pushing. Before the fix, Subscribe closed the
// old subscription channel, and a closed channel is always ready to receive:
// the old stream's select took its receive branch immediately and forever,
// so it pushed a full snapshot as fast as it could write one. Measured on a
// real anchor: one one-shot command under a member's identity turned that
// member's push rate from 158 B/15 s into 15.8 MB/15 s (1.05 MB/s), with no
// upper bound.
//
// The test asserts the old stream goes quiet: after the second stream
// subscribes, the first one must receive nothing while the member view
// changes.
func TestMeshMembersReconnectDoesNotHotPush(t *testing.T) {
	f := newMeshFixture(t, 2)
	_, ts, _ := newMeshRelay(t, f.members)

	// The first stream for devices[0].
	first := dialMembers(t, ts, f.devices[0], membersAuthHeaders(f.devices[0]))
	if snap := recvSnapshot(t, first); len(snap.Members) != 2 {
		t.Fatalf("initial snapshot: %d members", len(snap.Members))
	}

	// The member reconnects: a second stream for the SAME device. This is
	// what the discovery client does after a stream drop.
	second := dialMembers(t, ts, f.devices[0], membersAuthHeaders(f.devices[0]))
	if snap := recvSnapshot(t, second); len(snap.Members) != 2 {
		t.Fatalf("reconnect snapshot: %d members", len(snap.Members))
	}

	// Now change the member view. The live stream must receive the push.
	resp, body := postRegister(t, ts, signedAnnounce(f.devices[1], []string{"8.8.8.9:7000"}, time.Now()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("announce: HTTP %d (body %s)", resp.StatusCode, body)
	}
	snap := recvSnapshot(t, second)
	if mv, ok := snap.Members[f.devices[1].ID()]; !ok || !mv.Online {
		t.Fatalf("live stream did not get the push: %+v", mv)
	}

	// The orphaned stream must stay silent. Before the fix it received a
	// snapshot as fast as the socket could write one, so a single read with
	// a short deadline would succeed. Now it must time out.
	_ = first.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if msg, err := first.Recv(); err == nil {
		t.Fatalf("orphaned stream still receiving (%d bytes): the old subscription "+
			"channel was closed instead of being signalled, so it spins", len(msg))
	}
}
