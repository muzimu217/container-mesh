// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery_test

// Client-side and three-node integration tests. This file is an EXTERNAL
// test package because it drives the whole stack (relay + discovery node +
// clients) through exported APIs only, and relay imports discovery.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/relay"
	"github.com/muzimu217/container-mesh/internal/wsconn"
)

type meshStack struct {
	acc     *identity.Account
	devices []*identity.Device
	members *identity.MemberList
	srv     *relay.Server
	ts      *httptest.Server
}

func newStack(t *testing.T, n int) *meshStack {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	s := &meshStack{acc: acc, members: identity.NewMemberList(acc.ID())}
	for i := 0; i < n; i++ {
		dev, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		cert, err := acc.IssueCert(dev, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.members.Add(*cert); err != nil {
			t.Fatal(err)
		}
		s.devices = append(s.devices, dev)
	}
	s.srv = relay.New(relay.DefaultConfig())
	s.srv.EnableDiscovery(s.members)
	s.ts = httptest.NewServer(s.srv.Handler())
	t.Cleanup(s.ts.Close)
	t.Cleanup(s.srv.StopDiscovery)
	return s
}

func (s *meshStack) clientFor(t *testing.T, i int, eps []string) *discovery.Client {
	t.Helper()
	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{s.ts.URL},
		Device:     s.devices[i],
		AccountID:  s.acc.ID(),
		AccountPub: []byte(s.acc.PublicKey()),
		Endpoints:  eps,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func waitFor(t *testing.T, bound time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s not satisfied within %s", what, bound)
}

// TestClientReconnectBackoffResetsAfterHealthyStream pins the fix for the
// root cause netns-probe found in round 7: the reconnect penalty doubled on
// EVERY stream end and was never reset, so a client that the server kicked
// as a slow consumer ended up in a 30-second reconnect gap — and any state
// change landing in that gap arrived 30s late (issue #8 #7).
//
// A healthy stream (one that delivered a verified snapshot) must reset the
// penalty, so repeated kicks cost one backoff step each, not a ladder.
func TestClientReconnectBackoffResetsAfterHealthyStream(t *testing.T) {
	s := newStack(t, 2)
	node := s.srv.DiscoNode()
	if node == nil {
		t.Fatal("discovery node not enabled on the stack relay")
	}

	// Observe the client's reconnect behaviour through the server: each
	// stream attach/detach pair is visible as the stream count changing.
	cl := s.clientFor(t, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	waitFor(t, 5*time.Second, "first snapshot", func() bool { return cl.Snapshot() != nil })

	// The server kicks this client repeatedly (as it would a slow
	// consumer). Each kick must be followed by a prompt reconnect — a
	// laddering penalty would show up as a growing gap.
	gaps := make([]time.Duration, 0, 3)
	for i := 0; i < 3; i++ {
		time.Sleep(200 * time.Millisecond)
		// Force the server to drop this member's stream — the same thing
		// the queue-depth cleanup does to a slow consumer.
		if n := s.srv.KickStream(s.devices[0].ID()); n == 0 {
			t.Fatal("no stream to kick — the client is not connected")
		}
		// The detach is asynchronous (the old handler's deferred
		// DetachStream fires when it sees the close), so `before` sampled
		// immediately after KickStream can still show the old count —
		// `> before` then never becomes true (doubao-cloud-vm-01 round 8
		// diagnosed this exact race). Instead: wait for the detach to land
		// (count drops to 0), then wait for the reconnect (count ≥ 1).
		kickAt := time.Now()
		waitFor(t, 5*time.Second, "detach lands after kick", func() bool {
			return node.StreamCount(s.devices[0].ID()) == 0
		})
		waitFor(t, 8*time.Second, "client reconnects promptly", func() bool {
			return node.StreamCount(s.devices[0].ID()) >= 1
		})
		gaps = append(gaps, time.Since(kickAt))
	}
	t.Logf("reconnect gaps after repeated kicks: %v", gaps)
	// With a resetting penalty every gap is ~1s (the initial backoff). A
	// laddering penalty would make the third gap ≥4s. Use a generous
	// bound (T-2.4: no absolute thresholds in tests) but well below the
	// ladder's 8s+.
	for i, g := range gaps {
		if g > 5*time.Second {
			t.Fatalf("reconnect gap %d was %s — the penalty is laddering instead of resetting", i+1, g)
		}
	}
}

func TestClientAnnouncesAndSeesItselfOnline(t *testing.T) {
	s := newStack(t, 1)
	cl := s.clientFor(t, 0, []string{"203.0.113.5:7000"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	waitFor(t, 5*time.Second, "initial snapshot", func() bool { return cl.Snapshot() != nil })
	waitFor(t, 5*time.Second, "self online after announce", func() bool {
		snap := cl.Snapshot()
		mv, ok := snap.Members[s.devices[0].ID()]
		return ok && mv.Online && len(mv.Endpoints) == 1
	})
	if !cl.Joined() {
		t.Fatal("member device must report Joined after its snapshot")
	}
}

func TestClientRejectsForgedSnapshot(t *testing.T) {
	// A hostile "discovery node": skips every auth check and pushes a
	// snapshot carrying a cert the real account never signed. The client's
	// account-key verification is what must save it (MESH-DESIGN §5.1:
	// the node is trusted to be online, not to be honest).
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	victim, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	forgerAcc, err := identity.GenerateAccount() // NOT the invite's account key
	if err != nil {
		t.Fatal(err)
	}
	forgerDev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	forgerCert, err := forgerAcc.IssueCert(forgerDev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	push := make(chan discovery.MemberSnapshot, 4)
	fake := fakeMembersServer(t, acc.ID(), push)
	defer fake.Close()

	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{fake.URL},
		Device:     victim,
		AccountID:  acc.ID(),
		AccountPub: []byte(acc.PublicKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	push <- discovery.MemberSnapshot{
		AccountID: acc.ID(),
		Version:   1,
		Members: map[identity.DeviceID]discovery.MemberView{
			forgerDev.ID(): {Cert: *forgerCert, Online: true, Endpoints: []string{"6.6.6.6:6666"}},
		},
	}
	waitFor(t, 3*time.Second, "forged-only snapshot processed", func() bool { return cl.Snapshot() != nil })

	// The forged member must NOT exist in the view; an empty applied
	// snapshot is correct behaviour (drop, not reject — one forged cert must
	// not blind the client, review F3).
	snap := cl.Snapshot()
	if _, present := snap.Members[forgerDev.ID()]; present {
		t.Fatalf("forged member present in the view: %+v", snap.Members)
	}
	if cl.Joined() {
		t.Fatal("client must not consider itself joined on a forged-only view")
	}
}

// fakeMembersServer is a hostile-or-test discovery node: no auth checks,
// snapshots pushed verbatim from a channel.
func fakeMembersServer(t *testing.T, accountID identity.DeviceID, push <-chan discovery.MemberSnapshot) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mesh/members" {
			http.NotFound(w, r)
			return
		}
		conn, err := wsconn.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			select {
			case snap := <-push:
				b, err := json.Marshal(snap)
				if err != nil {
					return
				}
				if err := conn.Send(b); err != nil {
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	}))
}

// TestClientKeepsGoodMembersAmongForged pins the F3 fix: a snapshot with one
// forged and one genuine member yields a view with ONLY the genuine one —
// the forged cert is dropped, the honest view survives.
func TestClientKeepsGoodMembersAmongForged(t *testing.T) {
	acc, _ := identity.GenerateAccount()
	victim, _ := identity.Generate()
	forgerAcc, _ := identity.GenerateAccount()
	forgerDev, _ := identity.Generate()
	forgerCert, err := forgerAcc.IssueCert(forgerDev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	honestDev, _ := identity.Generate()
	honestCert, err := acc.IssueCert(honestDev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	push := make(chan discovery.MemberSnapshot, 4)
	fake := fakeMembersServer(t, acc.ID(), push)
	defer fake.Close()

	cl, _ := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{fake.URL},
		Device:     victim,
		AccountID:  acc.ID(),
		AccountPub: []byte(acc.PublicKey()),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	push <- discovery.MemberSnapshot{
		AccountID: acc.ID(),
		Version:   1,
		Members: map[identity.DeviceID]discovery.MemberView{
			forgerDev.ID(): {Cert: *forgerCert, Online: true, Endpoints: []string{"6.6.6.6:6666"}},
			honestDev.ID(): {Cert: *honestCert, Online: true, Endpoints: []string{"203.0.113.7:7000"}},
		},
	}
	waitFor(t, 3*time.Second, "mixed snapshot applied", func() bool {
		snap := cl.Snapshot()
		return snap != nil && len(snap.Members) == 1
	})
	if _, ok := cl.Snapshot().Members[honestDev.ID()]; !ok {
		t.Fatal("honest member dropped along with the forged one")
	}
}

// TestAnnounceSanitizesErrorBody pins the F8 fix: a hostile node's 4xx body
// reaches client logs as printable text — newlines and ANSI escapes become
// spaces, so the error channel cannot forge log lines.
func TestAnnounceSanitizesErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("evil\nFAKE LOG LINE\x1b[31minjected"))
	}))
	defer srv.Close()

	dev, _ := identity.Generate()
	acc, _ := identity.GenerateAccount()
	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{srv.URL},
		Device:     dev,
		AccountID:  acc.ID(),
		AccountPub: []byte(acc.PublicKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = cl.Announce(context.Background())
	if err == nil {
		t.Fatal("announce against rejecting server must fail")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\n\r\x1b") {
		t.Fatalf("error message carries control characters (log injection): %q", msg)
	}
	if !strings.Contains(msg, "FAKE LOG LINE") {
		t.Fatalf("error message lost the (sanitized) server reason: %q", msg)
	}
}

// TestClientIgnoresVersionRegression pins the F1 fix: a replayed PRE-revocation
// snapshot (lower version) must never overwrite the newer view — a malicious
// node's only remaining power is to withhold updates, not to resurrect
// revoked members.
func TestClientIgnoresVersionRegression(t *testing.T) {
	acc, _ := identity.GenerateAccount()
	victim, _ := identity.Generate()
	revoked, _ := identity.Generate()
	revokedCert, err := acc.IssueCert(revoked, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	push := make(chan discovery.MemberSnapshot, 4)
	fake := fakeMembersServer(t, acc.ID(), push)
	defer fake.Close()

	cl, _ := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{fake.URL},
		Device:     victim,
		AccountID:  acc.ID(),
		AccountPub: []byte(acc.PublicKey()),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	// v2: post-revocation world — the revoked device is gone.
	push <- discovery.MemberSnapshot{AccountID: acc.ID(), Version: 2, Members: map[identity.DeviceID]discovery.MemberView{}}
	waitFor(t, 3*time.Second, "v2 applied", func() bool {
		snap := cl.Snapshot()
		return snap != nil && snap.Version == 2
	})

	// Replay: the node pushes the pre-revocation v1 snapshot.
	push <- discovery.MemberSnapshot{
		AccountID: acc.ID(),
		Version:   1,
		Members: map[identity.DeviceID]discovery.MemberView{
			revoked.ID(): {Cert: *revokedCert, Online: true, Endpoints: []string{"6.6.6.6:6666"}},
		},
	}
	time.Sleep(500 * time.Millisecond)
	snap := cl.Snapshot()
	if snap.Version != 2 {
		t.Fatalf("replayed v1 overwrote the view (version now %d)", snap.Version)
	}
	if _, present := snap.Members[revoked.ID()]; present {
		t.Fatal("revoked member resurrected by a replayed snapshot")
	}
}

func TestClientRejectsWrongAccountSnapshot(t *testing.T) {
	s := newStack(t, 1)
	otherAcc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}

	// A node that answers for a DIFFERENT account: the client's account
	// binding (from its invite) must reject the view outright.
	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{s.ts.URL},
		Device:     s.devices[0],
		AccountID:  otherAcc.ID(), // claim a different account than the node serves
		AccountPub: []byte(otherAcc.PublicKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	time.Sleep(500 * time.Millisecond)
	if snap := cl.Snapshot(); snap != nil {
		t.Fatalf("foreign-account snapshot accepted: account %s", snap.AccountID)
	}
}

func TestClientAnnounceSurfacesNotMemberReason(t *testing.T) {
	s := newStack(t, 1)
	// devices[0] is a member; a second device is not in the list.
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := s.acc.IssueCert(stranger, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_ = cert // issued but never added: pending approval

	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{s.ts.URL},
		Device:     stranger,
		AccountID:  s.acc.ID(),
		AccountPub: []byte(s.acc.PublicKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = cl.Announce(context.Background())
	if err == nil {
		t.Fatal("pending-approval announce must be rejected")
	}
	if !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("error must carry the actionable reason (O-4): %v", err)
	}
}

func TestClientFailsOverAcrossAddresses(t *testing.T) {
	s := newStack(t, 1)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	deadURL := dead.URL
	dead.Close() // port now refused

	cl, err := discovery.NewClient(discovery.ClientConfig{
		// Dead first, live second: both announce and stream must land on the
		// second without the caller caring.
		Addrs:      []string{deadURL, s.ts.URL},
		Device:     s.devices[0],
		AccountID:  s.acc.ID(),
		AccountPub: []byte(s.acc.PublicKey()),
		Endpoints:  []string{"203.0.113.5:7000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Announce(context.Background()); err != nil {
		t.Fatalf("announce with failover: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()
	waitFor(t, 5*time.Second, "snapshot via second address", func() bool { return cl.Snapshot() != nil })
}

// TestThreeNodesMutualVisibility is the PR-M2 acceptance scenario: 3 nodes
// announce; each sees all three online. The bound here is generous (in-process
// loopback), the REQUIREMENT (R-2.2 / §5.1) stays ≤10 s in real deployments.
func TestThreeNodesMutualVisibility(t *testing.T) {
	s := newStack(t, 3)
	clients := make([]*discovery.Client, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range clients {
		clients[i] = s.clientFor(t, i, []string{fmt.Sprintf("203.0.113.%d:7000", i+1)})
		go func(cl *discovery.Client) { _ = cl.Run(ctx) }(clients[i])
	}

	for i, cl := range clients {
		i, cl := i, cl
		waitFor(t, 8*time.Second, fmt.Sprintf("node %d sees 3 online members", i), func() bool {
			snap := cl.Snapshot()
			if snap == nil || len(snap.Members) != 3 {
				return false
			}
			for _, mv := range snap.Members {
				if !mv.Online {
					return false
				}
			}
			return true
		})
	}
}

// TestUnapprovedDeviceStaysInvisible pins the R-1.5/R-2.4 boundary: a device
// with no membership is blind (its stream is refused — the roster is members-
// only) and invisible (it appears in no member's view). Approval then flips
// both sides within one push.
func TestUnapprovedDeviceStaysInvisible(t *testing.T) {
	s := newStack(t, 2) // two approved members
	// third device: cert issued, approval pending
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := s.acc.IssueCert(stranger, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	scl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{s.ts.URL},
		Device:     stranger,
		AccountID:  s.acc.ID(),
		AccountPub: []byte(s.acc.PublicKey()),
		Endpoints:  []string{"203.0.113.200:7000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = scl.Run(ctx) }()

	// Blind: every stream attempt is 401'd before upgrade.
	time.Sleep(800 * time.Millisecond)
	if snap := scl.Snapshot(); snap != nil {
		t.Fatalf("unapproved device received a snapshot: %+v", snap.Members)
	}
	if scl.Joined() {
		t.Fatal("unapproved device claims joined")
	}

	memberCl := s.clientFor(t, 0, nil)
	mctx, mcancel := context.WithCancel(context.Background())
	defer mcancel()
	go func() { _ = memberCl.Run(mctx) }()
	waitFor(t, 5*time.Second, "member snapshot", func() bool { return memberCl.Snapshot() != nil })
	time.Sleep(300 * time.Millisecond)
	if snap := memberCl.Snapshot(); len(snap.Members) != 2 {
		t.Fatalf("member view has %d members; the unapproved device leaked in", len(snap.Members))
	}

	// Approval lands (operator adds the cert and hot-swaps the list): the
	// joiner's next reconnect succeeds and BOTH sides see the new state.
	if err := s.members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	s.srv.SetMeshMembers(s.members)

	waitFor(t, 8*time.Second, "joiner sees the roster after approval", func() bool {
		snap := scl.Snapshot()
		return snap != nil && len(snap.Members) == 3 && scl.Joined()
	})
	waitFor(t, 5*time.Second, "member sees the approved joiner", func() bool {
		snap := memberCl.Snapshot()
		if len(snap.Members) != 3 {
			return false
		}
		_, inList := snap.Members[stranger.ID()]
		return inList
	})
}

// TestFirstAnnounceRejectionArmsFastApprovalPoll pins the approval-path
// latency netns-probe quantified in round 20: when the FIRST announce is
// rejected ("not a member" — the normal pre-approval state), the retry
// ticker must arm at the fast cadence immediately, so the joiner's
// endpoints surface within seconds of approval. Run used to pass nil to
// announceRetryDelay, arming the first ticker at a full heartbeat: an
// approval landing in the first window left the new member undialable
// for ~30 s while "is it running with --listen?" pointed troubleshooters
// the wrong way.
//
// The observable is an APPROVED member's pushed snapshot — the stranger
// appears there with endpoints only once its announce has landed, which
// isolates the announce cadence from the joiner's own stream backoff.
func TestFirstAnnounceRejectionArmsFastApprovalPoll(t *testing.T) {
	s := newStack(t, 1)

	// Observer: approved member, stream up before the joiner exists.
	obsCl := s.clientFor(t, 0, []string{"203.0.113.5:7000"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = obsCl.Run(ctx) }()
	waitFor(t, 5*time.Second, "observer snapshot", func() bool { return obsCl.Snapshot() != nil })

	// Joiner: cert issued, approval pending.
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := s.acc.IssueCert(stranger, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	joinCl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{s.ts.URL},
		Device:     stranger,
		AccountID:  s.acc.ID(),
		AccountPub: []byte(s.acc.PublicKey()),
		Endpoints:  []string{"203.0.113.201:7000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	jctx, jcancel := context.WithCancel(context.Background())
	defer jcancel()
	go func() { _ = joinCl.Run(jctx) }()

	// Let the first announce fire and be rejected — the state whose retry
	// delay this test pins.
	time.Sleep(500 * time.Millisecond)

	if err := s.members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	s.srv.SetMeshMembers(s.members)

	// Endpoints must surface on the fast cadence (~3 s), not a heartbeat
	// (30 s). The 8 s bound keeps this red if the first ticker is armed at
	// the heartbeat, and green with margin under -race.
	waitFor(t, 8*time.Second, "joiner endpoints visible after approval (fast poll)", func() bool {
		snap := obsCl.Snapshot()
		if snap == nil {
			return false
		}
		mv, ok := snap.Members[stranger.ID()]
		return ok && len(mv.Endpoints) > 0
	})
}

// TestTransportFailureStillPollsFastBeforeJoin pins the boundary netns-probe
// quantified in round 25: a TRANSPORT-level announce failure (node down, or
// the joiner racing the node's socket) must not cost a full heartbeat while
// the device is unjoined. The old error-text rule fast-polled only
// "not a member" rejections, so a 503/refused left approvals waiting
// 27.1 s — 25× the not-member path. The cadence now keys on joined state:
// ANY pre-join failure retries fast; post-join failures keep the heartbeat
// (a node outage must not turn the whole membership into a 3 s storm).
//
// A 503-ing register endpoint is the transport bucket: the response never
// carries membership semantics, so the pre-fix client armed its next ticker
// at the heartbeat — one attempt in the window. The fixed client must be
// back within ~3 s, and not hammer: bounded above to catch cadence bugs.
func TestTransportFailureStillPollsFastBeforeJoin(t *testing.T) {
	var attempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mesh/register" {
			atomic.AddInt64(&attempts, 1)
			http.Error(w, "transport bucket: service unavailable", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r) // stream dials fail too; irrelevant to this cadence
	}))
	defer srv.Close()

	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{srv.URL},
		Device:     stranger,
		AccountID:  acc.ID(),
		AccountPub: []byte(acc.PublicKey()),
		Endpoints:  []string{"203.0.113.202:7000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	// First announce fires immediately and fails (503). The retry ticker
	// must re-arm at the fast cadence, not the heartbeat: a second attempt
	// within the 6 s bound proves it (the pre-fix code would need 30 s).
	waitFor(t, 6*time.Second, "second announce attempt after transport failure", func() bool {
		return atomic.LoadInt64(&attempts) >= 2
	})
	if n := atomic.LoadInt64(&attempts); n > 4 {
		t.Fatalf("pre-join retries ran at %d attempts in 6s; expected ~3s cadence", n)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestJoinedAnnounceFailuresKeepHeartbeat pins the OTHER half of the D-9b
// cadence, the anti-storm side netns-probe round 27 baselined on the merge
// candidate (gaps 30.001–30.005 s under 503): once JOINED, announce
// failures must keep the heartbeat cadence — a node outage must not turn
// the whole membership into a 3 s request storm (2084 members would go
// from ~70 to ~695 req/s). The heartbeat here is shrunk to 5 s so the same
// property asserts in seconds: post-flip gaps must stay ≈5 s. An inverted
// joined-check (fast when joined) collapses them to ~3 s and fails — the
// symptom would otherwise be silent 10× request volume, not an error.
func TestJoinedAnnounceFailuresKeepHeartbeat(t *testing.T) {
	s := newStack(t, 1)

	var mu sync.Mutex
	failMode := false
	var stamps []time.Time
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/mesh/register" {
			mu.Lock()
			stamps = append(stamps, time.Now())
			mu.Unlock()
			if failMode {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Body:       io.NopCloser(strings.NewReader("stub: upstream unavailable")),
					Header:     http.Header{},
				}, nil
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}

	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{s.ts.URL},
		Device:     s.devices[0],
		AccountID:  s.acc.ID(),
		AccountPub: []byte(s.acc.PublicKey()),
		Endpoints:  []string{"203.0.113.5:7000"},
		Heartbeat:  5 * time.Second,
		HTTPClient: hc,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	// Join for real: announce succeeds, the stream delivers the member view.
	waitFor(t, 8*time.Second, "member joins", func() bool { return cl.Joined() })

	mu.Lock()
	failMode = true
	flipAt := len(stamps)
	mu.Unlock()

	// Collect three post-failure announces (~15 s at the 5 s heartbeat).
	waitFor(t, 25*time.Second, "three announces after failures begin", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(stamps)-flipAt >= 3
	})

	mu.Lock()
	defer mu.Unlock()
	for i := flipAt + 1; i < len(stamps); i++ {
		gap := stamps[i].Sub(stamps[i-1])
		if gap < 4500*time.Millisecond || gap > 7500*time.Millisecond {
			t.Fatalf("post-join announce gap %s (want ≈5s heartbeat, not ~3s fast poll)", gap)
		}
	}
}
