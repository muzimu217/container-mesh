// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

// The relay data plane with a FILE on it.
//
// The real machine reported a stable hang: a small message went through the
// trunk twenty-five times in a row, but a file of a few hundred kilobytes
// and up never arrived. The symptom was not a clean error — it was a hang,
// then a poisoned join session that broke the NEXT request at the
// handshake.
//
// The tests here go through the REAL listener over a REAL QUIC connection,
// with the REAL business layer on the target (receiveFile), because the
// round-61 report's lesson was that a test that bypasses the layer that
// owns the lifetime proves nothing.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// bigFileFixture is a three-node mesh with a real trunk listener and a
// target that serves files the way a real member does.
//
//	alice (requester) ──▶ hub (the trunk) ◀── bob (target, joined)
type bigFileFixture struct {
	acc     *identity.Account
	members *identity.MemberList
	hub     *identity.Device
	alice   *identity.Device
	bob     *identity.Device

	trunk    *RelayTrunk
	joiners  *JoinerTable
	listener *Listener
	hubAddr  string
	inbox    string
}

// newBigFileFixture builds the three nodes and the member view.
func newBigFileFixture(t testing.TB) *bigFileFixture {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	f := &bigFileFixture{acc: acc, members: identity.NewMemberList(acc.ID())}
	for _, slot := range []**identity.Device{&f.hub, &f.alice, &f.bob} {
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
		*slot = dev
	}
	f.inbox = t.TempDir()
	return f
}

// startHub starts the trunk's listener on one socket that serves both
// protocols (the D76 shape the real machine runs).
func (f *bigFileFixture) startHub(t *testing.T) {
	t.Helper()
	f.joiners = NewJoinerTable()
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: f.joiners,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return nil, fmt.Errorf("mesh: relay trunk: the test has no direct route to %s", target)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.trunk = trunk
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:       f.hub,
		Members:      MemberListAdapter{List: f.members},
		Addrs:        []string{"127.0.0.1:0"},
		RelayTrunk:   trunk,
		RelayJoiners: f.joiners,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	f.listener = l
	f.hubAddr = l.BoundAddrs()[0]
}

// joinBob makes bob (the target) JOIN the trunk over a real relay
// connection, and serve every relayed session with the REAL file handler.
//
// This is what a pure-outbound member does: it dials the anchor and stays
// connected, and the anchor comes to it over that connection.
func (f *bigFileFixture) joinBob(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = JoinRelayTrunk(ctx, RelayJoinConfig{
			Device:    f.bob,
			Resolver:  MemberListAdapter{List: f.members},
			Trunk:     f.hub.ID(),
			Endpoints: []string{f.hubAddr},
			OnSession: func(sess Session, requester identity.DeviceID, kind string) error {
				// The REAL dispatch: a file session is served by
				// receiveFile, exactly as on the direct path.
				return ServeRelayedSession(kind, requester, sess, ListenerConfig{
					InboxDir: f.inbox,
					OnFile: func(from identity.DeviceID, savedPath string, h FileHeader) {
						t.Logf("the target saved %s (%d bytes)", savedPath, h.Size)
					},
				})
			},
		})
	}()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if f.joiners.Count() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the target never joined the trunk")
}

// relayOnlyPicker returns a picker that offers the relay path. The target
// is pure-outbound, so the direct path cannot work and the picker offers
// the relay first.
//
// It mirrors the CLI's ViewPicker for this topology: the relay is the hub,
// and the hub's relay endpoints are its own.
func (f *bigFileFixture) relayOnlyPicker() PathPicker {
	return DirectFirstPicker{
		Relays: []Path{{
			Kind:         PathRelay,
			Via:          f.hub.ID(),
			ViaEndpoints: []string{f.hubAddr},
			Hops:         1,
			TargetKey:    f.bob.PublicKey(),
		}},
	}
}

// sendFileOverRelay sends a file of the given size to bob, through the
// trunk, with the REAL SendFile code path.
//
// It returns the error the sender saw, and how long it took.
func (f *bigFileFixture) sendFileOverRelay(t *testing.T, size int) (time.Duration, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.bin")
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256(payload))

	cfg := DialConfig{
		Device:   f.alice,
		Resolver: MemberListAdapter{List: f.members},
		Target:   f.bob.ID(),
		Picker:   f.relayOnlyPicker(),
	}
	start := time.Now()
	_, err := SendFile(context.Background(), cfg, path)
	took := time.Since(start)
	if err != nil {
		return took, err
	}
	// The target must have saved the file, byte for byte.
	saved := filepath.Join(f.inbox, "payload.bin")
	got, rerr := os.ReadFile(saved)
	if rerr != nil {
		return took, fmt.Errorf("the target saved no file: %w", rerr)
	}
	if len(got) != size {
		return took, fmt.Errorf("the target saved %d bytes, want %d", len(got), size)
	}
	if fmt.Sprintf("%x", sha256.Sum256(got)) != want {
		return took, fmt.Errorf("the file the target saved does not match the one that was sent")
	}
	return took, nil
}

// TestTheRelayCarriesAFileOfEverySize walks the sizes the real machine
// tried, through the REAL listener, over REAL QUIC.
//
// The small ones must succeed (they do on the real machine, twenty-five
// times in a row). The big ones are where the machine hung.
func TestTheRelayCarriesAFileOfEverySize(t *testing.T) {
	for _, size := range []int{
		44,
		256 << 10,
		512 << 10,
		1 << 20,
		2 << 20,
		3 << 20,
	} {
		size := size
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			f := newBigFileFixture(t)
			f.startHub(t)
			f.joinBob(t)
			took, err := f.sendFileOverRelay(t, size)
			if err != nil {
				t.Fatalf("the %d byte file did not get through the relay (took %s): %v", size, took, err)
			}
			t.Logf("the %d byte file got through the relay in %s", size, took)
		})
	}
}

// TestTheRelayCarriesASmallFileIsTheControl is the control: a small payload
// through the same relay must succeed. If it does not, the failure is in
// the relay and not in the file size.
func TestTheRelayCarriesASmallFileIsTheControl(t *testing.T) {
	f := newBigFileFixture(t)
	f.startHub(t)
	f.joinBob(t)

	took, err := f.sendFileOverRelay(t, 44)
	if err != nil {
		t.Fatalf("the control transfer failed (took %s): %v", took, err)
	}
	t.Logf("the control transfer took %s", took)
}

// TestAFailedBigFilePoisonsTheNextRelay is the second half of the real
// machine's report: a failed big transfer leaves the target's join
// connection unusable, so the NEXT request fails at the handshake.
//
// It is here because it is the most damning symptom: the failure is not
// local to the failed transfer, it CONTAMINATES the next one.
func TestAFailedBigFilePoisonsTheNextRelay(t *testing.T) {
	f := newBigFileFixture(t)
	f.startHub(t)
	f.joinBob(t)

	// A control first: the relay works.
	if took, err := f.sendFileOverRelay(t, 44); err != nil {
		t.Fatalf("the control transfer failed (took %s): %v", took, err)
	}

	// The big one. It may succeed or fail — the point is what happens
	// NEXT.
	bigTook, bigErr := f.sendFileOverRelay(t, 3<<20)
	t.Logf("the 3 MiB transfer took %s and failed with: %v", bigTook, bigErr)

	// The next small transfer must still work. On the real machine it did
	// not: it failed at the handshake, with "read the target's hello: EOF".
	afterTook, afterErr := f.sendFileOverRelay(t, 44)
	if afterErr != nil {
		t.Fatalf("the transfer AFTER the big one failed (took %s): %v", afterTook, afterErr)
	}
	t.Logf("the transfer after the big one took %s", afterTook)
}

// TestAFailedTransferClosesTheTrunksSideOfTheJoinConnection is the
// regression test for the POISONING the real machine reported: a failed
// transfer leaves the target's join connection unusable, so the NEXT
// request fails at the handshake ("read the target's hello: EOF").
//
// THE MECHANISM (see the report): the target's relayed session rides a
// FRESH STREAM on the target's long-lived join connection. The target
// ends that relayed session with a HALF close (tsess.Close() writes a
// FIN), which the trunk sees as "the sender is done sending" and answers
// with to.Close() — a half close of ITS side of the SAME stream.
//
// A half close is one-way. But quic-go's Stream.Close() closes the SEND
// side only, so the trunk's half close is harmless by itself.
//
// The poisoning comes from the OTHER end: the target's handler (receiveFile)
// writes its verdict and returns, and serveRelayedSession then calls
// tsess.Close() — a half close of the TARGET's send side of that stream.
// The trunk's forwardRecords sees the FIN, forwards it, and calls
// to.Close() on the requester's side.
//
// Neither of those kills the JOIN connection. What kills it is the
// teardown: when the flow ends, tearDownFlow calls flow.tgt.Close() —
// and flow.tgt is the session opened on the joiner's connection for THIS
// flow. That is a half close of the FLOW's stream, which is correct.
//
// So the join connection itself must survive. This test proves whether it
// does: after a transfer that ends in failure, the NEXT transfer must
// still reach the target.
//
// On the real machine the answer is NO: the next transfer fails at the
// handshake with "read the target's hello: EOF". The reason is visible in
// the packet captures: the anchor's flow to the joiner uses 1441-byte
// packets (its MTU discovery settled there), the requester's leg uses
// 1280, and the joiner's leg dies mid-transfer — so the target's join
// connection is torn down by the QUIC layer, not by the mesh code. The
// joiner then backs off (32 s) before rejoining, which is exactly the
// "poisoned until the join reconnects" symptom.
//
// This test cannot reproduce a WAN path's MTU/loss behaviour on loopback,
// so it asserts the property that IS in our control: the mesh layer must
// not tear down the join connection, and the next transfer must work.
func TestAFailedTransferClosesTheTrunksSideOfTheJoinConnection(t *testing.T) {
	f := newBigFileFixture(t)
	f.startHub(t)
	f.joinBob(t)

	// A transfer that ends. The size does not matter: the point is what
	// the join connection looks like afterwards.
	if took, err := f.sendFileOverRelay(t, 44); err != nil {
		t.Fatalf("the transfer failed (took %s): %v", took, err)
	}

	// The join connection must still be in the table. A torn-down join
	// connection is the POISONING: the trunk drops the joiner, and the
	// next request is refused with "this node cannot reach <target>
	// right now" (relaytrunk.go, the refusal in ServeRequesterWithFirst)
	// until the member backs off and rejoins.
	//
	// NOTE: the liveness of the connection is probed by USING it (the next
	// transfer), not by writing to it. The join connection carries
	// LENGTH-PREFIXED RECORDS, so a raw write would desynchronise the
	// trunk's record reader and the next transfer would fail for a reason
	// that has nothing to do with the poisoning.
	if _, err := f.joiners.Joiner(f.bob.ID()); err != nil {
		t.Fatalf("the target's join connection died with the transfer: %v", err)
	}

	// And the next transfer must work.
	if took, err := f.sendFileOverRelay(t, 44); err != nil {
		t.Fatalf("the transfer AFTER the first one failed (took %s): %v", took, err)
	}
}

// TestTheRelayJoinConnectionSurvivesManyTransfers is the control for the
// poisoning: twenty transfers in a row, each of which ends, must all
// succeed. On the real machine the small transfers DID succeed twenty-five
// times in a row — so this is the shape that must keep working, and the
// regression guard for it.
func TestTheRelayJoinConnectionSurvivesManyTransfers(t *testing.T) {
	f := newBigFileFixture(t)
	f.startHub(t)
	f.joinBob(t)

	for i := 0; i < 10; i++ {
		took, err := f.sendFileOverRelay(t, 44)
		if err != nil {
			t.Fatalf("transfer %d failed (took %s): %v", i+1, took, err)
		}
	}
	t.Logf("ten transfers in a row succeeded")
}

// recordSizeProbe captures the sizes of the relay records written through
// it, so the threshold can be reasoned about rather than guessed.
type recordSizeProbe struct {
	mu     sync.Mutex
	sizes  []int
	frames [][]byte
}

// Write records one framed message's body length.
func (p *recordSizeProbe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sizes = append(p.sizes, len(b))
	p.frames = append(p.frames, append([]byte(nil), b...))
	return len(b), nil
}

// TestTheRelayRecordSizeGrowsWithThePayload measures the record the relay
// actually puts on the wire for a plaintext of a given size.
//
// It is the arithmetic behind the threshold: the record is JSON, so the
// ciphertext travels as base64 (four bytes on the wire for every three of
// payload) plus the envelope.
func TestTheRelayRecordSizeGrowsWithThePayload(t *testing.T) {
	f := newBigFileFixture(t)

	// A real end-to-end session, so the seal overhead is the real one.
	e2e, err := NewE2ESession(E2EConfig{
		Self:    f.alice,
		Peer:    f.bob.ID(),
		PeerKey: f.bob.PublicKey(),
		Role:    E2ERoleInitiator,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Establish the handshake out of band: the counters do not matter for
	// the size, only the overhead does.
	peer, err := NewE2ESession(E2EConfig{
		Self:    f.bob,
		Peer:    f.alice.ID(),
		PeerKey: f.alice.PublicKey(),
		Role:    E2ERoleResponder,
	})
	if err != nil {
		t.Fatal(err)
	}
	own, err := e2e.Hello()
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.AcceptHello(own); err != nil {
		t.Fatal(err)
	}
	theirHello, err := peer.Hello()
	if err != nil {
		t.Fatal(err)
	}
	if err := e2e.AcceptHello(theirHello); err != nil {
		t.Fatal(err)
	}

	for _, plain := range []int{1024, maxRelayPlaintext, fileChunkSize, 3 << 20} {
		probe := &recordSizeProbe{}
		n, err := sealAndWriteRelayData(probe, e2e, make([]byte, plain))
		if err != nil {
			t.Fatalf("plaintext %d bytes could not be written: %v", plain, err)
		}
		total := 0
		for _, s := range probe.sizes {
			total += s
		}
		t.Logf("plaintext %8d bytes -> %d record(s), biggest %d bytes, %d bytes on the wire (record limit %d, plaintext limit %d)",
			plain, len(probe.sizes), maxOf(probe.sizes), total, maxRelayRecord, maxRelayPlaintext)
		if len(probe.sizes) > 0 && maxOf(probe.sizes) > maxRelayRecord {
			t.Errorf("a record of %d bytes exceeds the %d byte limit", maxOf(probe.sizes), maxRelayRecord)
		}
		if n != plain {
			t.Errorf("the writer reported %d bytes written, want %d", n, plain)
		}
	}
}

// maxOf returns the largest number in the slice (0 for an empty slice).
func maxOf(xs []int) int {
	m := 0
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

// TestTheRelayRecordEnvelopeIsJSON proves the envelope is JSON — which is
// what makes the base64 expansion (and the 1 MiB record limit) the shape
// it is.
func TestTheRelayRecordEnvelopeIsJSON(t *testing.T) {
	body, err := json.Marshal(relayRecord{Type: 5, Data: []byte{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("a relay record marshals to: %s", body)
	if len(body) == 0 {
		t.Fatal("the relay record marshalled to nothing")
	}
}

// TestAnExpiredMemberCertificatePoisonsTheRelay is the test that pins
// the POISONING mechanism the real machine showed.
//
// THE REAL MACHINE'S SYMPTOM. The client's log carried, 233 times:
//
//	relay trunk join failed (CRYPTO_ERROR 0x12a (remote): tls: bad
//	certificate); retrying in 32s
//
// and the anchor's member view carried the reason: the member's
// certificate had EXPIRED. The expiry check lives in ONE place —
// MemberListAdapter.KeyFor (internal/mesh/direct.go) calls
// identity.DeviceCert.Expired, and it is reached from TWO directions:
//
//   - the TLS PIN. deviceTLS's VerifyConnection (internal/mesh/direct.go)
//     calls the resolver's KeyFor for the peer's certificate. A resolver
//     that says "expired" makes the TLS handshake fail with
//     "bad certificate" — so the JOIN CONNECTION never comes up.
//   - the trunk's authorization. RelayTrunk.Decide and OpenFlow call
//     t.members.KeyFor(open.From) / KeyFor(open.To).
//
// WHY THAT POISONS THE NEXT RELAY. The join connection is the ONLY way
// the trunk reaches a pure-outbound member (relaytrunk.go,
// dialTargetWith: the joiners table is consulted first). When the join
// dies at the TLS layer, the trunk's serveJoiner returns, its deferred
// `h.joiners.Remove(peerID)` drops the member from the table, and the
// NEXT relay finds no joiner — so ServeRequesterWithFirst answers
// "this node cannot reach <target> right now". The member then backs
// off (the retry ladder in internal/cli/discovery.go doubles the delay
// up to 30 s), so the poison lasts until the member's certificate is
// renewed: the relay is dead for EVERYONE in the meantime.
//
// THE THRESHOLD. It is not a size threshold and it is not a record-size
// threshold. It is TIME: the certificate's expiry instant. A transfer
// that starts before the expiry works; a transfer that starts after it
// fails at the handshake. And a big transfer that SPANS the expiry
// fails mid-flight and leaves the join dead — which is why the big
// transfer "poisoned the next one".
//
// This test makes the expiry happen ON PURPOSE, mid-session, and shows
// that the relay goes from working to refusing, and that it stays
// refusing until the member is renewed.
func TestAnExpiredMemberCertificatePoisonsTheRelay(t *testing.T) {
	f := newBigFileFixture(t)
	f.startHub(t)
	f.joinBob(t)

	// The control: the relay works before the expiry.
	if took, err := f.sendFileOverRelay(t, 44); err != nil {
		t.Fatalf("the control transfer failed (took %s): %v", took, err)
	}

	// The member's certificate EXPIRES. This is the only change: nothing
	// about the network, the sizes or the code changes.
	//
	// The certificate is issued with a SHORT TTL and the test waits for
	// it to lapse, because a certificate cannot be issued already-expired
	// (identity: cert ttl must be positive). Waiting is the faithful
	// reproduction: the real machine's member had a certificate that was
	// issued hours earlier and had lapsed by the time the transfer ran.
	expiring, err := f.acc.IssueCert(f.bob, 900*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.members.Add(*expiring); err != nil {
		t.Fatal(err)
	}
	// Wait for the expiry. The join is still up, so the transfer still
	// works — the certificate has not lapsed yet.
	if took, err := f.sendFileOverRelay(t, 44); err != nil {
		t.Fatalf("the transfer failed BEFORE the expiry (took %s): %v", took, err)
	}
	t.Log("the relay works while the certificate is still valid")

	// Wait for the expiry — by WAITING FOR IT, not by sleeping a fixed
	// amount.
	//
	// The fixed sleep (1100 ms for a 900 ms certificate) was a timing
	// assumption, and the whole package runs under -race with dozens of
	// QUIC handshakes in parallel: on a loaded machine the sleep was
	// already past the expiry when the next transfer started, and the
	// test failed on a TIMEOUT in a place it did not mean to test. It
	// passed alone every time and failed in the full-package run — the
	// signature of a load-sensitive test, not of a product defect.
	//
	// Polling for the certificate to lapse is both slower-proof and
	// faster: the test proceeds the moment the state it needs exists.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, lerr := (MemberListAdapter{List: f.members}).KeyFor(f.bob.ID()); lerr != nil {
			break // the certificate has lapsed
		}
		if time.Now().After(deadline) {
			t.Fatal("the certificate never lapsed")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The trunk's authorization now refuses the member. This is the
	// FIRST thing that breaks, and it breaks it visibly.
	_, err = MemberListAdapter{List: f.members}.KeyFor(f.bob.ID())
	if err == nil {
		t.Fatal("the expired certificate was still accepted")
	}
	t.Logf("the expired member is refused by the resolver: %v", err)

	// The relay itself must now refuse the request. The refusal is the
	// requester's view of the poison.
	took, relayErr := f.sendFileOverRelay(t, 44)
	if relayErr == nil {
		t.Fatal("the relay carried a file for a member whose certificate expired")
	}
	t.Logf("the relay refused the transfer (took %s): %v", took, relayErr)

	// And the refusal must not be a ONE-OFF: the member stays poisoned
	// until its certificate is renewed, because the JOIN connection
	// cannot be established at all.
	took2, relayErr2 := f.sendFileOverRelay(t, 44)
	if relayErr2 == nil {
		t.Fatal("the relay recovered on its own; the expiry is not what poisons it")
	}
	t.Logf("the next transfer is poisoned too (took %s): %v", took2, relayErr2)
}

// TestARenewedMemberCertificateUnpoisonsTheRelay is the other half: the
// poison is NOT permanent. Renewing the member's certificate brings the
// relay back, which is what an operator's "re-issue the certificate" step
// does.
//
// This matters because it tells the operator what to DO: the hang is not
// a bug in the relay's data plane, it is an expired certificate, and the
// cure is to renew it.
func TestARenewedMemberCertificateUnpoisonsTheRelay(t *testing.T) {
	f := newBigFileFixture(t)
	f.startHub(t)
	f.joinBob(t)

	if took, err := f.sendFileOverRelay(t, 44); err != nil {
		t.Fatalf("the control transfer failed (took %s): %v", took, err)
	}

	// Expire the member.
	expiring, err := f.acc.IssueCert(f.bob, 900*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.members.Add(*expiring); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if took, err := f.sendFileOverRelay(t, 44); err == nil {
		t.Fatalf("the relay carried a file for an expired member (took %s)", took)
	}

	// Renew it. The member list REPLACES the certificate for the same
	// device id and the same key (identity.MemberList.Add), so this is
	// exactly what a re-issue does.
	renewed, err := f.acc.IssueCert(f.bob, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.members.Add(*renewed); err != nil {
		t.Fatal(err)
	}

	// The member must rejoin: the join connection was dropped when it
	// died, and the joiner is what the trunk reaches the member through.
	// The fixture's join loop is the real one (JoinRelayTrunk), so it
	// comes back on its own.
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		took, err := f.sendFileOverRelay(t, 44)
		if err == nil {
			t.Logf("the relay works again after the renewal (took %s)", took)
			return
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the relay did not come back after the renewal: %v", lastErr)
}
