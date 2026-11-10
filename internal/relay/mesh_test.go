// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wsconn"
)

// meshFixture builds an account with n devices, all signed in.
type meshFixture struct {
	acc     *identity.Account
	devices []*identity.Device
	members *identity.MemberList
}

func newMeshFixture(t *testing.T, n int) *meshFixture {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	f := &meshFixture{acc: acc, members: identity.NewMemberList(acc.ID())}
	for i := 0; i < n; i++ {
		dev, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		cert, err := acc.IssueCert(dev, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.members.Add(*cert); err != nil {
			t.Fatal(err)
		}
		f.devices = append(f.devices, dev)
	}
	return f
}

func newMeshRelay(t *testing.T, members *identity.MemberList) (*Server, *httptest.Server, *discovery.Node) {
	t.Helper()
	srv := New(DefaultConfig())
	node := srv.EnableDiscovery(members)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.StopDiscovery)
	return srv, ts, node
}

// postRegister sends a register request exactly as the discovery client does.
func postRegister(t *testing.T, ts *httptest.Server, req discovery.RegisterRequest) (*http.Response, string) {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/mesh/register", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func signedAnnounce(dev *identity.Device, eps []string, at time.Time) discovery.RegisterRequest {
	a := discovery.MemberAnnounce{DeviceID: dev.ID(), Endpoints: eps, Timestamp: at}
	return discovery.RegisterRequest{Announce: a, Sig: a.Sign(dev)}
}

func TestMeshRegisterRejectsNonMember(t *testing.T) {
	f := newMeshFixture(t, 1)
	_, ts, node := newMeshRelay(t, f.members)

	// A device that holds a certificate but has NOT been approved into the
	// member list yet — the exact state of a joiner waiting for R-1.5
	// approval. Signed correctly; still must not register.
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.acc.IssueCert(stranger, time.Hour); err != nil {
		t.Fatal(err)
	}
	resp, body := postRegister(t, ts, signedAnnounce(stranger, []string{"203.0.113.5:7000"}, time.Now()))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("non-member announce: HTTP %d, want 401 (body: %s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "not a member") {
		t.Fatalf("rejection reason missing membership: %s", body)
	}
	if got := node.Endpoints(stranger.ID()); len(got) != 0 {
		t.Fatalf("non-member recorded endpoints %v", got)
	}
}

func TestMeshRegisterRejectsBadSignature(t *testing.T) {
	f := newMeshFixture(t, 2)
	_, ts, _ := newMeshRelay(t, f.members)

	// Announce as devices[0] but sign with devices[1]'s key.
	a := discovery.MemberAnnounce{DeviceID: f.devices[0].ID(), Endpoints: []string{"203.0.113.5:7000"}, Timestamp: time.Now()}
	req := discovery.RegisterRequest{Announce: a, Sig: a.Sign(f.devices[1])}
	resp, body := postRegister(t, ts, req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged signature: HTTP %d, want 401 (body: %s)", resp.StatusCode, body)
	}
}

func TestMeshRegisterRejectsStaleTimestamp(t *testing.T) {
	f := newMeshFixture(t, 1)
	_, ts, _ := newMeshRelay(t, f.members)

	old := time.Now().Add(-time.Minute)
	resp, body := postRegister(t, ts, signedAnnounce(f.devices[0], []string{"203.0.113.5:7000"}, old))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale announce: HTTP %d, want 401 (body: %s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "outside window") {
		t.Fatalf("rejection reason missing staleness: %s", body)
	}
}

func TestMeshRegisterRejectsMalformedBody(t *testing.T) {
	f := newMeshFixture(t, 1)
	_, ts, _ := newMeshRelay(t, f.members)
	resp, err := http.Post(ts.URL+"/mesh/register", "application/json", strings.NewReader(`{"announce": "junk"`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body: HTTP %d, want 400", resp.StatusCode)
	}
}

func TestMeshRegisterAcceptsAndUpdatesNode(t *testing.T) {
	f := newMeshFixture(t, 1)
	_, ts, node := newMeshRelay(t, f.members)
	dev := f.devices[0]

	resp, body := postRegister(t, ts, signedAnnounce(dev, []string{"203.0.113.5:7000", "10.0.0.5:7000"}, time.Now()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid announce: HTTP %d (body: %s)", resp.StatusCode, body)
	}
	var out struct {
		OK      bool `json:"ok"`
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || !out.OK {
		t.Fatalf("response body %q: %v", body, err)
	}
	if !out.Changed {
		t.Fatalf("first announce must report changed=true (new endpoints), got %q", body)
	}
	got := node.Endpoints(dev.ID())
	if len(got) != 2 || got[0] != "203.0.113.5:7000" {
		t.Fatalf("node endpoints = %v", got)
	}

	// Heartbeat with identical endpoints: accepted but not worth a push.
	resp, body = postRegister(t, ts, signedAnnounce(dev, []string{"203.0.113.5:7000", "10.0.0.5:7000"}, time.Now()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repeat announce: HTTP %d (body: %s)", resp.StatusCode, body)
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Changed {
		t.Fatalf("identical heartbeat reported changed=%v (body %s)", out.Changed, body)
	}
}

// membersAuthHeaders builds the stream credentials for dev, matching the
// discovery client's scheme.
func membersAuthHeaders(dev *identity.Device) http.Header {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	signable := discovery.MeshAuthContext + "\x00" + dev.ID().String() + "\x00" + ts
	h := http.Header{}
	h.Set(discovery.HeaderMeshDevice, dev.ID().String())
	h.Set(discovery.HeaderMeshTime, ts)
	h.Set(discovery.HeaderMeshSig, base64.StdEncoding.EncodeToString(dev.Sign([]byte(signable))))
	return h
}

func dialMembers(t *testing.T, ts *httptest.Server, dev *identity.Device, hdr http.Header) *wsconn.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/mesh/members"
	conn, err := wsconn.Dial(context.Background(), wsconn.DialOptions{URL: u, HandshakeHeader: hdr})
	if err != nil {
		t.Fatalf("members dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func recvSnapshot(t *testing.T, conn *wsconn.Conn) *discovery.MemberSnapshot {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	msg, err := conn.Recv()
	if err != nil {
		t.Fatalf("snapshot recv: %v", err)
	}
	var snap discovery.MemberSnapshot
	if err := json.Unmarshal(msg, &snap); err != nil {
		t.Fatalf("snapshot decode: %v (%q)", err, msg)
	}
	return &snap
}

func TestMeshMembersRejectsUnauthenticated(t *testing.T) {
	f := newMeshFixture(t, 1)
	_, ts, _ := newMeshRelay(t, f.members)

	// No credentials at all.
	resp, err := http.Get(ts.URL + "/mesh/members")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credentials: HTTP %d, want 401", resp.StatusCode)
	}

	// Non-member device.
	other := newMeshFixture(t, 1)
	resp2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mesh/members", nil)
	for k, vs := range membersAuthHeaders(other.devices[0]) {
		for _, v := range vs {
			resp2.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(resp2)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("non-member stream: HTTP %d, want 401", res.StatusCode)
	}

	// A real member presenting a CORRUPTED signature: the signature check is
	// its own gate (breaking it must turn this red).
	hdr := membersAuthHeaders(f.devices[0])
	hdr.Set(discovery.HeaderMeshSig, base64.StdEncoding.EncodeToString([]byte("forged-signature-bytes")))
	req3, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mesh/members", nil)
	req3.Header = hdr
	res3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	res3.Body.Close()
	if res3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("corrupted member signature: HTTP %d, want 401", res3.StatusCode)
	}
}

func TestMeshMembersPushesSnapshotAndUpdates(t *testing.T) {
	f := newMeshFixture(t, 2)
	_, ts, _ := newMeshRelay(t, f.members)

	conn := dialMembers(t, ts, f.devices[0], membersAuthHeaders(f.devices[0]))

	snap := recvSnapshot(t, conn)
	if len(snap.Members) != 2 {
		t.Fatalf("initial snapshot has %d members, want 2", len(snap.Members))
	}
	// Nobody has announced yet: everyone listed, nobody online.
	for id, mv := range snap.Members {
		if mv.Online {
			t.Fatalf("member %s online before any announce", id)
		}
		// The client-side trust property: every cert verifies against the
		// account key alone (R-2.4 on the receiving side).
		if err := mv.Cert.Verify(f.acc.PublicKey()); err != nil {
			t.Fatalf("cert for %s failed account verification: %v", id, err)
		}
	}

	// devices[1] announces → the node changes → a push with it ONLINE.
	resp, body := postRegister(t, ts, signedAnnounce(f.devices[1], []string{"203.0.113.9:7000"}, time.Now()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("announce: HTTP %d (body %s)", resp.StatusCode, body)
	}
	snap = recvSnapshot(t, conn)
	mv, ok := snap.Members[f.devices[1].ID()]
	if !ok || !mv.Online {
		t.Fatalf("announced member missing or offline in push: %+v", mv)
	}
	if len(mv.Endpoints) != 1 || mv.Endpoints[0] != "203.0.113.9:7000" {
		t.Fatalf("announced endpoints wrong: %v", mv.Endpoints)
	}
}

func TestMeshMembersSeesApprovalReload(t *testing.T) {
	f := newMeshFixture(t, 2)
	srv, ts, _ := newMeshRelay(t, f.members)

	conn := dialMembers(t, ts, f.devices[0], membersAuthHeaders(f.devices[0]))
	if snap := recvSnapshot(t, conn); len(snap.Members) != 2 {
		t.Fatalf("initial snapshot: %d members", len(snap.Members))
	}

	// Approval: a third device's cert joins the list (the operator's file
	// edit), the relay hot-swaps it, and open streams are pushed the new
	// view — this is the R-1.5 join path.
	newDev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := f.acc.IssueCert(newDev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := f.members
	if err := reloaded.Add(*cert); err != nil {
		t.Fatal(err)
	}
	srv.SetMeshMembers(reloaded)

	snap := recvSnapshot(t, conn)
	if len(snap.Members) != 3 {
		t.Fatalf("post-approval snapshot: %d members, want 3", len(snap.Members))
	}
	if _, ok := snap.Members[newDev.ID()]; !ok {
		t.Fatalf("approved device missing from push")
	}
}

func TestMeshSweepDropsOfflineFromView(t *testing.T) {
	f := newMeshFixture(t, 2)
	_, ts, node := newMeshRelay(t, f.members)

	if resp, _ := postRegister(t, ts, signedAnnounce(f.devices[1], []string{"203.0.113.9:7000"}, time.Now())); resp.StatusCode != 200 {
		t.Fatalf("announce failed: %d", resp.StatusCode)
	}
	conn := dialMembers(t, ts, f.devices[0], membersAuthHeaders(f.devices[0]))
	snap := recvSnapshot(t, conn) // initial
	if !snap.Members[f.devices[1].ID()].Online {
		t.Fatalf("announced member should be online in initial snapshot")
	}

	// Advance the clock past MemberTimeout and sweep: reachability expires,
	// membership does not.
	node.SetClock(func() time.Time { return time.Now().Add(2 * discovery.MemberTimeout) })
	node.Sweep()

	snap = recvSnapshot(t, conn)
	mv := snap.Members[f.devices[1].ID()]
	if mv.Online || len(mv.Endpoints) != 0 {
		t.Fatalf("swept member still online with endpoints: %+v", mv)
	}
	if _, still := snap.Members[f.devices[1].ID()]; !still {
		t.Fatalf("sweep must expire reachability, not membership")
	}
}

// TestMeshMembersShortKeyMemberDoesNotPanic pins the F4 fix: a member record
// with a malformed (non-32-byte) device key must produce a clean 401, not an
// ed25519 panic that kills the whole relay process. Add()'s only structural
// check is IDFromKey(DevicePub)==DeviceID, which an attacker can satisfy for
// ANY key length — the length defense has to live in verification itself.
func TestMeshMembersShortKeyMemberDoesNotPanic(t *testing.T) {
	f := newMeshFixture(t, 1)
	short := ed25519.PublicKey("attacker-chosen-key")
	poison := identity.DeviceCert{
		DeviceID:  identity.IDFromKey(short),
		DevicePub: short,
		AccountID: f.acc.ID(),
		IssuedAt:  time.Now().Add(-time.Minute),
		ExpiresAt: time.Now().Add(time.Hour),
		Sig:       []byte("irrelevant-here"),
	}
	if err := f.members.Add(poison); err != nil {
		t.Fatalf("the poisoned record must pass Add's structural check: %v", err)
	}
	_, ts, _ := newMeshRelay(t, f.members)

	// Stream credentials naming the poisoned ID, signed by a key we hold.
	// The failure must be a 401 with the signature reason — reached without
	// panicking, which is the whole point of this test.
	tsStr := time.Now().UTC().Format(time.RFC3339Nano)
	signable := discovery.MeshAuthContext + "\x00" + poison.DeviceID.String() + "\x00" + tsStr
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mesh/members", nil)
	req.Header.Set(discovery.HeaderMeshDevice, poison.DeviceID.String())
	req.Header.Set(discovery.HeaderMeshTime, tsStr)
	req.Header.Set(discovery.HeaderMeshSig, base64.StdEncoding.EncodeToString(f.devices[0].Sign([]byte(signable))))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request with poisoned member record: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("poisoned member stream: HTTP %d, want 401", res.StatusCode)
	}
}

// TestStreamCloseExpiresMemberFast pins the #7 fix (issue #8): a member's
// death (kill -9 closes its socket) must flip it offline in every other
// member's view within seconds — the stream-close is the fast liveness
// signal, NOT the 90s heartbeat timeout. Removing the AttachStream/DetachStream
// pair in streamMeshMembers turns this test red (the snapshot only updates
// after MemberTimeout).
func TestStreamCloseExpiresMemberFast(t *testing.T) {
	f := newMeshFixture(t, 2)
	_, ts, _ := newMeshRelay(t, f.members)

	// devices[1] announces and opens its members stream (what every client
	// does in Run).
	if resp, _ := postRegister(t, ts, signedAnnounce(f.devices[1], []string{"203.0.113.9:7000"}, time.Now())); resp.StatusCode != 200 {
		t.Fatal("announce failed")
	}
	victimConn := dialMembers(t, ts, f.devices[1], membersAuthHeaders(f.devices[1]))
	if snap := recvSnapshot(t, victimConn); !snap.Members[f.devices[1].ID()].Online {
		t.Fatal("victim should be online in its own initial snapshot")
	}

	// devices[0] watches the network.
	obs := dialMembers(t, ts, f.devices[0], membersAuthHeaders(f.devices[0]))
	if snap := recvSnapshot(t, obs); !snap.Members[f.devices[1].ID()].Online {
		t.Fatal("observer should see the victim online initially")
	}

	// Death: the socket closes (kill -9 sends no goodbye; the kernel RSTs).
	start := time.Now()
	_ = victimConn.Close()

	// The observer is pushed the offline flip well within seconds — the
	// bound here is a smoke ceiling (T-2.4), the P-5 line is 10 s.
	_ = obs.SetReadDeadline(time.Now().Add(3 * time.Second))
	snap := recvSnapshot(t, obs)
	if snap.Members[f.devices[1].ID()].Online {
		t.Fatalf("victim still online after stream close (%.1fs)", time.Since(start).Seconds())
	}
	// Endpoints PERSIST the stream close (announcement state ≠ liveness,
	// issue #8 endpoint-oscillation fix): the member's UDP listener is
	// probably still alive, so its advertised address stays a dial candidate.
	if len(snap.Members[f.devices[1].ID()].Endpoints) == 0 {
		t.Fatal("stream close must not blank announced endpoints")
	}
	// Membership itself survives: reachability expired, not membership.
	if _, still := snap.Members[f.devices[1].ID()]; !still {
		t.Fatal("fast expiry must not remove membership")
	}
}

// TestStreamOverlapKeepsOnline covers the reconnect-overlap case: a member
// that re-dials before its old socket's death is noticed stays online.
func TestStreamOverlapKeepsOnline(t *testing.T) {
	f := newMeshFixture(t, 1)
	_, ts, node := newMeshRelay(t, f.members)
	dev := f.devices[0]
	if resp, _ := postRegister(t, ts, signedAnnounce(dev, []string{"203.0.113.5:7000"}, time.Now())); resp.StatusCode != 200 {
		t.Fatal("announce failed")
	}
	node.AttachStream(dev.ID())
	node.AttachStream(dev.ID()) // re-dial before the old one dies
	node.DetachStream(dev.ID()) // old socket's death noticed
	if snap := discovery.BuildSnapshot(f.members, node, time.Now()); !snap.Members[dev.ID()].Online {
		t.Fatal("overlap detach must not expire a member with a live stream")
	}
	node.DetachStream(dev.ID())
	if snap := discovery.BuildSnapshot(f.members, node, time.Now()); snap.Members[dev.ID()].Online {
		t.Fatal("last detach must expire reachability")
	}
}

// TestThrottledConsumerKicked pins the slow-success kick (issue #8,
// oh-no-agent's #7 failure shape): a consumer that reads SLOWLY (app alive
// but starved — e.g. its stderr pipe backed up) lets writes succeed while
// megabytes queue; the queue delays every later push, so a kill event's
// full snapshot sat 90s+ behind 3.7MB of stale history. A push that takes
// longer than slowPushKick must kick EVEN WHEN IT EVENTUALLY SUCCEEDS:
// full snapshots make queued history obsolete, so reconnect-and-refetch
// converges strictly faster than draining.
func TestThrottledConsumerKicked(t *testing.T) {
	f := newMeshFixture(t, 3)
	_, ts, node := newMeshRelay(t, f.members)

	obs := dialMembers(t, ts, f.devices[0], membersAuthHeaders(f.devices[0]))
	recvSnapshot(t, obs) // initial push lands

	// Starved reader: 512 bytes every 200 ms (~2.5 KB/s) — orders of
	// magnitude slower than pushes arrive.
	stopReading := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopReading:
				return
			default:
			}
			_ = obs.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := obs.Recv(); err != nil {
				return // server kicked us — reader's job is done
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	go func() {
		for i := 0; i < 60000; i++ {
			ep := fmt.Sprintf("203.0.113.%d:%d", 1+i%250, 7000+i%1000)
			node.Register(discovery.MemberAnnounce{
				DeviceID:  f.devices[2].ID(),
				Endpoints: []string{ep},
				Timestamp: time.Now(),
			}, time.Now())
		}
	}()

	// The server must close the starved stream within a bounded window once
	// a push runs past slowPushKick (750 ms). Observable from the SERVER
	// side (stream count drops to zero) — the client's own view of the close
	// would be delayed by minutes: it drains its buffered bytes at its
	// throttled pace before ever seeing the FIN, which is exactly why the
	// backlog was so damaging in the first place.
	kicked := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if node.StreamCount(f.devices[0].ID()) == 0 {
			kicked = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	close(stopReading)
	if !kicked {
		t.Fatalf("throttled consumer never kicked (stream count %d) — slow-success pushes can still queue forever",
			node.StreamCount(f.devices[0].ID()))
	}
}

// TestSlowConsumerKicked pins the push-path hardening from the issue #8
// retest: a member stream whose client stops reading must be KICKED by the
// write deadline instead of jamming forever (unbounded Send fills kernel
// + gorilla buffers, blocks every keepalive write behind it, and freezes the
// member's view mid-stream). The kick is safe by design: the client's
// reconnect loop redials and receives a fresh snapshot. Removing the
// SetWriteDeadline in streamMeshMembers turns this test red (the conn stays
// open while megabytes queue).
func TestSlowConsumerKicked(t *testing.T) {
	f := newMeshFixture(t, 3)
	_, ts, node := newMeshRelay(t, f.members)

	// The observer connects and then STOPS READING (its process is wedged).
	obs := dialMembers(t, ts, f.devices[0], membersAuthHeaders(f.devices[0]))
	recvSnapshot(t, obs) // initial push lands in the socket buffer

	// A churn storm: tens of thousands of endpoint changes, each one a
	// legitimate notifyAll → a full-snapshot push toward the wedged reader.
	// ~48 MB total: comfortably past every auto-tuned loopback kernel buffer,
	// so without the write deadline the queue grows for minutes instead of
	// the kick firing in seconds.
	go func() {
		for i := 0; i < 60000; i++ {
			ep := fmt.Sprintf("203.0.113.%d:%d", 1+i%250, 7000+i%1000)
			node.Register(discovery.MemberAnnounce{
				DeviceID:  f.devices[2].ID(),
				Endpoints: []string{ep},
				Timestamp: time.Now(),
			}, time.Now())
		}
	}()

	// The observable for "kicked" is the server-side close surfacing on the
	// client's read: the client here deliberately has no liveness and never
	// reads (that IS the wedge), so its own Closed() cannot fire. The close
	// surfaces only AFTER the client drains whatever is already buffered,
	// so poll: buffered snapshots read clean (nil error) until the kick's
	// close arrives, then Recv errors. Polling also tolerates the race
	// between our wait and the deadline trip (kick observed anywhere from
	// ~5 s in). Without the write deadline the server never closes and this
	// loop runs to the fatal bound.
	kickSeen := false
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		_ = obs.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := obs.Recv()
		if err != nil {
			kickSeen = true
			break
		}
	}
	if !kickSeen {
		t.Fatal("server never kicked the wedged stream — the push path can still jam forever")
	}
}

func TestBuildSnapshotOmitsStaleEndpointsOnly(t *testing.T) {
	f := newMeshFixture(t, 1)
	_, ts, node := newMeshRelay(t, f.members)
	if resp, _ := postRegister(t, ts, signedAnnounce(f.devices[0], []string{"203.0.113.5:7000"}, time.Now())); resp.StatusCode != 200 {
		t.Fatal("announce failed")
	}
	snap := discovery.BuildSnapshot(f.members, node, time.Now())
	if !snap.Members[f.devices[0].ID()].Online {
		t.Fatal("fresh announcer must be online")
	}
	if snap.Members[f.devices[0].ID()].LastSeen.IsZero() {
		t.Fatal("online member must carry LastSeen")
	}
	if fmt.Sprint(snap.Version) != fmt.Sprint(f.members.CurrentVersion()) {
		t.Fatal("snapshot version must track the member list version")
	}
}
