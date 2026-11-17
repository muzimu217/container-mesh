// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery_test

// P-5b acceptance, end to end over the real stack (relay + discovery node +
// members streams): a member that broadcasts an explicit LEAVE must show up
// as offline in every other member's view within 3 s.
//
// The tests below drive the whole product through exported APIs only.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// leaveBudget is the P-5b convergence budget: from the departure to the
// other members' view saying offline.
const leaveBudget = 3 * time.Second

// postAnnounce sends a signed announce (or LEAVE) to the discovery node.
func postAnnounce(t *testing.T, s *meshStack, dev *identity.Device, a discovery.MemberAnnounce) {
	t.Helper()
	body, err := json.Marshal(discovery.RegisterRequest{Announce: a, Sig: a.Sign(dev)})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(s.ts.URL+"/mesh/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("register rejected: HTTP %d: %s", resp.StatusCode, string(b))
	}
}

// memberOnline reports what a member's view says about id.
func memberOnline(snap *discovery.MemberSnapshot, id identity.DeviceID) bool {
	if snap == nil {
		return false
	}
	mv, ok := snap.Members[id]
	return ok && mv.Online
}

// waitView waits for a member's view to say online/offline for id, using the
// client's update channel for synchronisation (no polling sleep) and a
// monotonic budget.
func waitView(t *testing.T, cl *discovery.Client, id identity.DeviceID, wantOnline bool, budget time.Duration, what string) {
	t.Helper()
	deadline := time.After(budget)
	for {
		select {
		case <-cl.Updates():
			if memberOnline(cl.Snapshot(), id) == wantOnline {
				return
			}
		case <-deadline:
			t.Fatalf("%s: view did not report online=%v within %s", what, wantOnline, budget)
		}
	}
}

// TestLeaveBroadcastUpdatesPeerViewWithinBudget is the P-5b acceptance test.
//
// The leaver here announces by hand and never opens a members stream, so the
// ONLY thing that can turn it offline in the peer's view is the LEAVE
// message itself — the transport-level path cannot contribute.
func TestLeaveBroadcastUpdatesPeerViewWithinBudget(t *testing.T) {
	s := newStack(t, 2)
	peer := s.clientFor(t, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = peer.Run(ctx) }()
	waitFor(t, 5*time.Second, "peer snapshot", func() bool { return peer.Snapshot() != nil })

	leaver := s.devices[0]
	announced := time.Now()
	postAnnounce(t, s, leaver, discovery.MemberAnnounce{
		DeviceID:  leaver.ID(),
		Endpoints: []string{"8.8.8.5:7000"},
		Timestamp: announced,
	})
	waitView(t, peer, leaver.ID(), true, 5*time.Second, "leaver visible online")

	// The explicit departure. The clock starts BEFORE the broadcast: the
	// budget covers the whole path (departure → anchor → view push →
	// the peer's view), not just the wait.
	start := time.Now()
	leave := discovery.MemberAnnounce{DeviceID: leaver.ID(), Timestamp: time.Now()}
	leave.Leave = true
	leave.Session = announced.UnixNano()
	postAnnounce(t, s, leaver, leave)

	waitView(t, peer, leaver.ID(), false, leaveBudget, "leaver offline after LEAVE")
	elapsed := time.Since(start)
	if elapsed > leaveBudget {
		t.Fatalf("LEAVE convergence took %s, budget is %s", elapsed, leaveBudget)
	}
	t.Logf("LEAVE broadcast converged in %s (budget %s)", elapsed, leaveBudget)
}

// TestClientLeaveConvergesPeerViewWithinBudget is the same acceptance test
// driven through the client API the CLI uses: Client.Leave must broadcast
// the LEAVE and take the client out of the view.
func TestClientLeaveConvergesPeerViewWithinBudget(t *testing.T) {
	s := newStack(t, 2)
	peer := s.clientFor(t, 1, nil)
	leaver := s.clientFor(t, 0, []string{"8.8.8.5:7000"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = peer.Run(ctx) }()
	go func() { _ = leaver.Run(ctx) }()
	waitFor(t, 5*time.Second, "peer snapshot", func() bool { return peer.Snapshot() != nil })
	waitView(t, peer, s.devices[0].ID(), true, 5*time.Second, "leaver visible online")

	lctx, lcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer lcancel()
	start := time.Now()
	if err := leaver.Leave(lctx); err != nil {
		t.Fatalf("leave failed: %v", err)
	}

	waitView(t, peer, s.devices[0].ID(), false, leaveBudget, "leaver offline after Client.Leave")
	elapsed := time.Since(start)
	if elapsed > leaveBudget {
		t.Fatalf("Client.Leave convergence took %s, budget is %s", elapsed, leaveBudget)
	}
	t.Logf("Client.Leave converged in %s (budget %s)", elapsed, leaveBudget)
}

// TestLostLeaveStillConverges is the degradation requirement: LEAVE is an
// accelerator, not the only mechanism. When the LEAVE never reaches the
// node, the member must still be marked offline — through the members
// stream the client closes on its way out — inside the P-5 bound.
func TestLostLeaveStillConverges(t *testing.T) {
	s := newStack(t, 2)
	peer := s.clientFor(t, 1, nil)

	// The leaver's HTTP path drops every LEAVE: the network is broken for
	// the departure message only, the members stream is unaffected.
	leaver, err := newTestClient(discovery.ClientConfig{
		Addrs:      []string{testDiscoveryURL(s.ts.URL)},
		Device:     s.devices[0],
		AccountID:  s.acc.ID(),
		AccountPub: []byte(s.acc.PublicKey()),
		Endpoints:  []string{"8.8.8.5:7000"},
		HTTPClient: &http.Client{Transport: dropLeaveTransport{base: http.DefaultTransport}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = peer.Run(ctx) }()
	go func() { _ = leaver.Run(ctx) }()
	waitFor(t, 5*time.Second, "peer snapshot", func() bool { return peer.Snapshot() != nil })
	waitView(t, peer, s.devices[0].ID(), true, 5*time.Second, "leaver visible online")

	lctx, lcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer lcancel()
	start := time.Now()
	if err := leaver.Leave(lctx); err == nil {
		t.Fatal("the dropped LEAVE must be reported as an error")
	}

	waitView(t, peer, s.devices[0].ID(), false, leaveBudget, "leaver offline after a lost LEAVE")
	elapsed := time.Since(start)
	if elapsed > leaveBudget {
		t.Fatalf("fallback convergence took %s, budget is %s", elapsed, leaveBudget)
	}
	t.Logf("lost LEAVE still converged in %s (budget %s)", elapsed, leaveBudget)
}

// dropLeaveTransport drops every register request that carries a LEAVE.
type dropLeaveTransport struct{ base http.RoundTripper }

func (t dropLeaveTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		if bytes.Contains(b, []byte(`"leave":true`)) {
			return nil, errors.New("simulated network failure: the LEAVE is lost")
		}
	}
	return t.base.RoundTrip(r)
}

// TestRepeatedLeaveKeepsViewOffline: the departure may be broadcast more
// than once. It must stay harmless — no error, no flap, no lost state.
func TestRepeatedLeaveKeepsViewOffline(t *testing.T) {
	s := newStack(t, 2)
	peer := s.clientFor(t, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = peer.Run(ctx) }()
	waitFor(t, 5*time.Second, "peer snapshot", func() bool { return peer.Snapshot() != nil })

	leaver := s.devices[0]
	announced := time.Now()
	postAnnounce(t, s, leaver, discovery.MemberAnnounce{
		DeviceID:  leaver.ID(),
		Endpoints: []string{"8.8.8.5:7000"},
		Timestamp: announced,
	})
	waitView(t, peer, leaver.ID(), true, 5*time.Second, "leaver visible online")

	for i := 0; i < 3; i++ {
		leave := discovery.MemberAnnounce{DeviceID: leaver.ID(), Timestamp: time.Now()}
		leave.Leave = true
		leave.Session = announced.UnixNano()
		postAnnounce(t, s, leaver, leave)
	}
	waitView(t, peer, leaver.ID(), false, leaveBudget, "leaver offline after repeated LEAVEs")

	// The member must STAY offline: a repeated LEAVE may not resurrect it.
	time.Sleep(200 * time.Millisecond)
	if memberOnline(peer.Snapshot(), leaver.ID()) {
		t.Fatal("a repeated LEAVE must not bring the member back online")
	}
}

// TestStaleLeaveKeepsReconnectedMemberOnline is the reconnect race: a LEAVE
// that belongs to a presence session the member already left must not mark
// its NEW connection offline.
func TestStaleLeaveKeepsReconnectedMemberOnline(t *testing.T) {
	s := newStack(t, 2)
	node := s.srv.DiscoNode()
	peer := s.clientFor(t, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = peer.Run(ctx) }()
	waitFor(t, 5*time.Second, "peer snapshot", func() bool { return peer.Snapshot() != nil })

	leaver := s.devices[0]
	first := time.Now()
	postAnnounce(t, s, leaver, discovery.MemberAnnounce{
		DeviceID:  leaver.ID(),
		Endpoints: []string{"8.8.8.5:7000"},
		Timestamp: first,
	})
	waitView(t, peer, leaver.ID(), true, 5*time.Second, "leaver visible online")

	// The member reconnects: a NEW presence session is announced.
	second := first.Add(time.Second)
	postAnnounce(t, s, leaver, discovery.MemberAnnounce{
		DeviceID:  leaver.ID(),
		Endpoints: []string{"8.8.8.5:7000"},
		Timestamp: second,
	})

	// A late LEAVE from the OLD session arrives.
	late := discovery.MemberAnnounce{DeviceID: leaver.ID(), Timestamp: second.Add(time.Second)}
	late.Leave = true
	late.Session = first.UnixNano()
	postAnnounce(t, s, leaver, late)

	if _, online := node.LastSeen(leaver.ID()); !online {
		t.Fatal("a stale LEAVE must not mark the reconnected member offline")
	}
	if !memberOnline(peer.Snapshot(), leaver.ID()) {
		t.Fatal("the peer view must still show the reconnected member online")
	}
}
