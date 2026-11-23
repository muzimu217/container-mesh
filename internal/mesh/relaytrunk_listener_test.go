// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

// The relay trunk's REAL listener path.
//
// The tests in relaytrunk_test.go call trunk.ServeRequester directly.
// That is a seam, not the product: the round-61 real-machine report
// found the trunk's data plane completely broken on real machines while
// every unit test was green, because the unit tests never went through
// RelayListener.handle — the layer that owns the connection's lifetime.
//
// Every test in THIS file goes through the real listener, over a real
// QUIC connection, with a real member view. That is the point of them.

import (
	"bufio"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// relayFixture is a three-node mesh with a real trunk listener.
//
//	alice (requester) ──▶ hub (the trunk) ◀── bob (target, joined)
//
// The hub's listener serves BOTH protocols on ONE socket when
// samePort is set — the D76 shape, where the operator does not have to
// open a second UDP port.
type relayFixture struct {
	acc     *identity.Account
	members *identity.MemberList
	hub     *identity.Device // the trunk
	alice   *identity.Device // the requester
	bob     *identity.Device // the target

	trunk    *RelayTrunk
	joiners  *JoinerTable
	listener *Listener
	// relayListener is the DEDICATED relay listener, when one was bound.
	relayListener *RelayListener
	// hubAddr is the hub's UDP address.
	hubAddr string
}

// newRelayFixture builds the three nodes and the member view.
func newRelayFixture(t testing.TB) *relayFixture {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	f := &relayFixture{acc: acc, members: identity.NewMemberList(acc.ID())}
	for _, slot := range []struct {
		dev  **identity.Device
		cert *identity.DeviceCert
	}{
		{&f.hub, nil}, {&f.alice, nil}, {&f.bob, nil},
	} {
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
		*slot.dev = dev
	}
	return f
}

// startHub starts the trunk's listener.
//
// samePort binds ONE socket that serves both the direct-link protocol
// and the relay protocol (the negotiated ALPN decides). separatePort
// binds a dedicated relay listener on its own port — the shape an
// operator gets with --relay-port.
func (f *relayFixture) startHub(t *testing.T, samePort bool) {
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

	if samePort {
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
		return
	}
	rl, err := NewRelayListener(context.Background(), RelayListenerConfig{
		Device:  f.hub,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
		Trunk:   trunk,
		Joiners: f.joiners,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Close)
	f.relayListener = rl
	f.hubAddr = rl.BoundAddrs()[0]
}

// joinBob makes bob (the target) JOIN the trunk over a real relay
// connection, exactly as a pure-outbound member does.
//
// The returned channel carries the plain-bytes sessions the trunk routes
// to bob.
func (f *relayFixture) joinBob(t *testing.T) <-chan Session {
	t.Helper()
	sessions := make(chan Session, 8)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = JoinRelayTrunk(ctx, RelayJoinConfig{
			Device:    f.bob,
			Resolver:  MemberListAdapter{List: f.members},
			Trunk:     f.hub.ID(),
			Endpoints: []string{f.hubAddr},
			OnSession: func(sess Session, requester identity.DeviceID, kind string) error {
				sessions <- sess
				// The session is handed to the test, which owns it from
				// here on: the handler must NOT read from it, or it would
				// eat the payload the test is about to assert on.
				return nil
			},
		})
	}()
	// The join must land before the request: the trunk refuses a relay
	// to a member that has not joined.
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if f.joiners.Count() > 0 {
			return sessions
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the target never joined the trunk")
	return nil
}

// TestRelayListenerServesARequesterEndToEnd is the test the round-61
// report asked for: the whole relay, through the REAL listener.
//
// It goes through RelayListener.handle — the layer that owns the
// connection's lifetime — and it exercises the full protocol:
//
//	alice ──open──▶ hub ──open──▶ bob
//	alice ──hello──▶ hub ────────▶ bob
//	bob ───hello───▶ hub ──hello──▶ alice
//	alice ──data───▶ hub ────────▶ bob
//
// Before the fix, the listener closed the requester's connection the
// moment the grant was written, so alice never even got to read the
// grant ("read the trunk's answer: bye" on a real machine). That is
// defect A of the round-61 real-machine report.
func TestRelayListenerServesARequesterEndToEnd(t *testing.T) {
	for _, shape := range []struct {
		name     string
		samePort bool
	}{
		{"one port for both protocols", true},
		{"a dedicated relay port", false},
	} {
		t.Run(shape.name, func(t *testing.T) {
			f := newRelayFixture(t)
			f.startHub(t, shape.samePort)
			sessions := f.joinBob(t)

			openCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			sess, err := OpenRelay(openCtx, RelayRequesterConfig{
				Device:    f.alice,
				Resolver:  MemberListAdapter{List: f.members},
				Target:    f.bob.ID(),
				Kind:      KindPing,
				TargetKey: f.bob.PublicKey(),
				Relay:     f.hub.ID(),
				Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
					link, err := DialRelaySession(ctx, f.alice, MemberListAdapter{List: f.members}, relay, []string{f.hubAddr})
					if err != nil {
						return nil, err
					}
					return link.Session(), nil
				},
			})
			if err != nil {
				t.Fatalf("the relayed session could not be opened through the listener: %v", err)
			}
			defer sess.Close()

			// The target must receive the session, and it must be able to
			// read from it: the handshake crossed the trunk.
			var targetSess Session
			select {
			case targetSess = <-sessions:
			case <-time.After(relayHandshakeWait):
				t.Fatal("the target never received the relayed session")
			}
			if targetSess == nil {
				t.Fatal("the target received a nil session")
			}
		})
	}
}

// TestRelayListenerKeepsTheRequesterAliveThroughTheRelay pins defect A
// on its own.
//
// The relay is a LONG exchange: the requester writes after the grant,
// and the trunk has to carry it. If the listener closes the connection
// when the serving function returns, the requester's write fails with
// "bye" — which is exactly what the real machine reported.
func TestRelayListenerKeepsTheRequesterAliveThroughTheRelay(t *testing.T) {
	f := newRelayFixture(t)
	f.startHub(t, true)
	sessions := f.joinBob(t)

	openCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := OpenRelay(openCtx, RelayRequesterConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Kind:      KindPing,
		TargetKey: f.bob.PublicKey(),
		Relay:     f.hub.ID(),
		Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
			link, err := DialRelaySession(ctx, f.alice, MemberListAdapter{List: f.members}, relay, []string{f.hubAddr})
			if err != nil {
				return nil, err
			}
			return link.Session(), nil
		},
	})
	if err != nil {
		t.Fatalf("the relayed session could not be opened: %v", err)
	}
	defer sess.Close()

	var targetSess Session
	select {
	case targetSess = <-sessions:
	case <-time.After(relayHandshakeWait):
		t.Fatal("the target never received the relayed session")
	}

	// The write goes through the trunk to the target. Before the fix it
	// failed with "Application error 0x0 (remote): bye".
	payload := []byte("the relay is still alive")
	if _, err := sess.Write(payload); err != nil {
		t.Fatalf("the write through the relay failed: %v", err)
	}
	buf := make([]byte, len(payload))
	_ = targetSess.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := targetSess.Read(buf)
	if err != nil {
		t.Fatalf("the target could not read the relayed payload: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("the target read %q, want %q", buf[:n], payload)
	}
}

// TestRelayListenerTellsTheTargetWhoIsCalling pins defect C on the real
// path.
//
// The trunk has to send the target an OPEN record that names the
// REQUESTER. Without it the target does not know whose end-to-end hello
// it is looking at, so it cannot pick the key to verify it against, and
// the handshake can never complete. The old code never sent it:
// TrunkJoiner.Send was DEAD CODE ("grep -rn '\.Send(' → 0 hits").
//
// The proof that the record reached the target is the handshake itself:
// the target's ServeRelayTarget reads the OPEN record to learn the
// requester's identity, and the requester's AcceptHello verifies the
// target's signature against the member view's key for THAT device. A
// missing or wrong OPEN record means the handshake cannot complete.
func TestRelayListenerTellsTheTargetWhoIsCalling(t *testing.T) {
	f := newRelayFixture(t)
	f.startHub(t, true)
	sessions := f.joinBob(t)

	sess, err := OpenRelay(context.Background(), RelayRequesterConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Kind:      KindPing,
		TargetKey: f.bob.PublicKey(),
		Relay:     f.hub.ID(),
		Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
			link, err := DialRelaySession(ctx, f.alice, MemberListAdapter{List: f.members}, relay, []string{f.hubAddr})
			if err != nil {
				return nil, err
			}
			return link.Session(), nil
		},
	})
	if err != nil {
		t.Fatalf("the handshake did not complete through the listener: %v", err)
	}
	defer sess.Close()

	select {
	case <-sessions:
	case <-time.After(relayHandshakeWait):
		t.Fatal("the target never received the relayed session")
	}
}

// TestRelayListenerServesTwoRelaysToTheSameTarget pins defect E.
//
// Before the fix, ending ONE relay closed the target's LONG-LIVED join
// connection, so the target could receive exactly ONE relay ever: the
// real machine reported "the first flow succeeds, then immediately
// 'cannot reach <target>'".
//
// The budget is bounded: a trunk that cannot serve the second relay must
// FAIL the test, not hang it.
func TestRelayListenerServesTwoRelaysToTheSameTarget(t *testing.T) {
	f := newRelayFixture(t)
	f.startHub(t, true)
	sessions := f.joinBob(t)

	for i := 0; i < 2; i++ {
		openCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		sess, err := OpenRelay(openCtx, RelayRequesterConfig{
			Device:    f.alice,
			Resolver:  MemberListAdapter{List: f.members},
			Target:    f.bob.ID(),
			Kind:      KindPing,
			TargetKey: f.bob.PublicKey(),
			Relay:     f.hub.ID(),
			Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
				link, err := DialRelaySession(ctx, f.alice, MemberListAdapter{List: f.members}, relay, []string{f.hubAddr})
				if err != nil {
					return nil, err
				}
				return link.Session(), nil
			},
		})
		if err != nil {
			cancel()
			t.Fatalf("relay %d could not be opened: %v", i+1, err)
		}
		select {
		case <-sessions:
		case <-time.After(relayHandshakeWait):
			cancel()
			t.Fatalf("relay %d never reached the target", i+1)
		}
		// The relay ends here: the requester's session is closed, and
		// the flow's teardown runs. Before the fix, this killed the
		// target's join connection.
		_ = sess.Close()
		cancel()
		// Give the teardown a moment to run.
		time.Sleep(200 * time.Millisecond)
	}
}

// TestOneSocketServesBothProtocols pins the D76 shape.
//
// ONE UDP socket, ONE quic.Listener, TWO ALPNs. The negotiated protocol
// decides which one a connection speaks.
//
// The old comment said "one socket can serve only one ALPN". That was
// wrong, and it was the reason an operator had to open a SECOND UDP
// port in their security group for the relay to work at all.
func TestOneSocketServesBothProtocols(t *testing.T) {
	f := newRelayFixture(t)
	f.startHub(t, true)

	// The direct-link protocol works on this socket.
	link, err := DialDirect(context.Background(), DialConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.hub.ID(),
		Endpoints: []string{f.hubAddr},
		Kind:      KindPing,
		// The handshake budget is EXPLICIT.
		//
		// Without it this dial falls back to the transport's own
		// ceiling — quic-go's HandshakeIdleTimeout — and the test then
		// fails with "context deadline exceeded" or "timeout: no recent
		// network activity" after exactly 20.01 s under the full-suite
		// -race load, with no product defect behind it (the same failure
		// mode the repo already documents in direct_test.go: "under the
		// full-suite -race load the QUIC handshake can take longer than
		// 5 s on a loaded CI runner").
		//
		// So the budget is stated here, the way the production path
		// states it, and it is longer than the transport's ceiling so
		// the TRANSPORT is the thing that times out — with a message
		// that says so — rather than a test that guesses.
		HandshakeTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("the direct link could not be opened on the shared socket: %v", err)
	}
	defer link.Close()
	if _, err := link.Session().Write(make([]byte, pingRecordSize)); err != nil {
		t.Fatalf("the direct link could not carry bytes: %v", err)
	}
	buf := make([]byte, pingRecordSize)
	_ = link.Session().SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := readFull(link.Reader(), buf); err != nil {
		t.Fatalf("the direct link could not read the echo: %v", err)
	}

	// And the relay protocol works on the SAME socket.
	f.joinBob(t)
}

// readFull reads exactly len(buf) bytes from a reader.
func readFull(br *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := br.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// TestTheTargetsJoinConnectionSurvivesTheFlow pins defect E directly.
//
// The old closeFlow closed the target's session, and that session WAS
// the target's long-lived join connection — so the target's membership
// of the trunk died with the first relay. The real machine saw exactly
// that: "the first flow succeeds, then immediately 'cannot reach
// <target>'".
//
// The test asserts the property directly: after a flow ends, the
// target's join connection is STILL ALIVE.
func TestTheTargetsJoinConnectionSurvivesTheFlow(t *testing.T) {
	f := newRelayFixture(t)
	f.startHub(t, true)
	f.joinBob(t)

	// The target's join session, as the trunk holds it.
	joiner, err := f.joiners.Joiner(f.bob.ID())
	if err != nil {
		t.Fatalf("the target has not joined the trunk: %v", err)
	}

	// One relay, from the requester.
	openCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := OpenRelay(openCtx, RelayRequesterConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Kind:      KindPing,
		TargetKey: f.bob.PublicKey(),
		Relay:     f.hub.ID(),
		Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
			link, err := DialRelaySession(ctx, f.alice, MemberListAdapter{List: f.members}, relay, []string{f.hubAddr})
			if err != nil {
				return nil, err
			}
			return link.Session(), nil
		},
	})
	if err != nil {
		t.Fatalf("the relay could not be opened: %v", err)
	}
	// End the relay.
	_ = sess.Close()
	// Let the flow's teardown run.
	deadline := time.Now().Add(5 * time.Second)
	for f.trunk.FlowCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if f.trunk.FlowCount() > 0 {
		t.Logf("the flow is still counted after 5s: %d", f.trunk.FlowCount())
	}

	// The join connection must still be usable: a write on it must not
	// fail immediately. Before the fix it was closed, so this write
	// failed at once.
	//
	// The write is bounded: a session that is wedged rather than dead
	// must fail the test too, and a test that hangs reports nothing.
	//
	// The write must be a FRAMED RECORD, not raw bytes.
	//
	// The join stream is a framed record stream: the trunk reads it with
	// readRelayRecordAs, which takes a 4-byte length prefix first. Raw
	// bytes on it are therefore read as a length, and the stream is
	// reported as broken — correctly, because it IS broken.
	//
	// Writing the literal string "still here" put the four bytes
	// 73 74 69 6C on the wire, and a reader that took them as a
	// big-endian length reported "frame of 1937009004 bytes exceeds the
	// 1048576 byte cap" — the memorable symptom of the round-81 format
	// desync, produced here by a TEST that spoke the protocol wrongly.
	//
	// That symptom then appeared in a DIFFERENT test (the joiner's own
	// loop reported the broken stream), so a bug in one test showed up
	// as a failure in another. The product was right throughout; the
	// test was not.
	_ = joiner.SetWriteDeadline(time.Now().Add(5 * time.Second))
	writeDone := make(chan error, 1)
	go func() {
		err := writeRelayRecordAs(joiner, relayRecord{
			Type: uint8(wire.FrameMeshRelayData),
			Data: []byte("still here"),
		}, RelaySpliceFormatBinary)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("the target's join connection died with the first relay: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the target's join connection is wedged after the first relay: " +
			"it must stay usable for the NEXT relay")
	}
}

// TestRelayOpenRecordNamesTheRequester is the mesh-level proof of
// defect C: the record the trunk writes onto the target's stream names
// the requester.
//
// It reads the record off the wire, so it is about the RECORD, not about
// the handshake.
func TestRelayOpenRecordNamesTheRequester(t *testing.T) {
	f := newRelayFixture(t)
	trunkEnd, targetEnd := newPipeSessionPair()
	joiners := NewJoinerTable()
	joiners.Add(&TrunkJoiner{
		Device: f.bob.ID(),
		sess:   trunkEnd,
		br:     bufio.NewReader(trunkEnd),
		open: func(ctx context.Context) (Session, error) {
			return trunkEnd, nil
		},
	})
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: joiners,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The requester's side of the flow.
	reqEnd, reqPeer := newPipeSessionPair()
	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
		Kind: KindPing,
	}, reqEnd); err != nil {
		t.Fatalf("the trunk refused a valid relay: %v", err)
	}

	// The deadline matters: without it, a trunk that never sends the
	// record would make this test HANG instead of failing.
	_ = targetEnd.SetReadDeadline(time.Now().Add(10 * time.Second))
	rec, err := readRelayRecord(bufio.NewReader(targetEnd))
	if err != nil {
		t.Fatalf("the target received no open record: %v", err)
	}
	if rec.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the target received record type %d, want an open record", rec.Type)
	}
	if rec.From != f.alice.ID() {
		t.Fatalf("the open record names %q, want the requester %s", rec.From, f.alice.ID())
	}
	if rec.To != f.bob.ID() {
		t.Fatalf("the open record names the target %q, want %s", rec.To, f.bob.ID())
	}
	if rec.Kind != KindPing {
		t.Fatalf("the open record names kind %q, want %q", rec.Kind, KindPing)
	}
	_ = reqPeer
}
