// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// trunkFixture is a small member view plus the devices in it.
type trunkFixture struct {
	acc     *identity.Account
	members *identity.MemberList
	hub     *identity.Device // the trunk itself (the anchor)
	alice   *identity.Device // a member that asks for a relay
	bob     *identity.Device // the target of the relay
	// The certificates, so a test can build a member view (the picker
	// reads the view, not the member list).
	hubCert   identity.DeviceCert
	aliceCert identity.DeviceCert
	bobCert   identity.DeviceCert
}

// newTrunkFixture builds an account with three members: the anchor that
// would run the trunk, a requester, and a target.
func newTrunkFixture(t testing.TB) *trunkFixture {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	f := &trunkFixture{acc: acc, members: identity.NewMemberList(acc.ID())}
	for _, slot := range []struct {
		dev  **identity.Device
		cert *identity.DeviceCert
	}{
		{&f.hub, &f.hubCert},
		{&f.alice, &f.aliceCert},
		{&f.bob, &f.bobCert},
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
		*slot.cert = *cert
	}
	return f
}

// recordingResolver remembers every membership lookup, so a test can prove
// that a refused request touched nothing it had no business touching.
type recordingResolver struct {
	inner  MemberResolver
	looked []identity.DeviceID
}

func (r *recordingResolver) KeyFor(id identity.DeviceID) (ed25519.PublicKey, error) {
	r.looked = append(r.looked, id)
	return r.inner.KeyFor(id)
}

func (r *recordingResolver) saw(id identity.DeviceID) bool {
	for _, got := range r.looked {
		if got == id {
			return true
		}
	}
	return false
}

// TestRelayOpenFromARevokedDeviceIsRefused guards the defect the previous
// round caught: the membership check has to come BEFORE anything that
// touches a third party.
//
// A revoked device is one that WAS a member. Its request must be refused
// as what it is — not a member — and the refusal must not have looked at
// the target at all: a revoked device must not be able to make the trunk
// do work on its behalf, not even a membership lookup.
func TestRelayOpenFromARevokedDeviceIsRefused(t *testing.T) {
	f := newTrunkFixture(t)

	// Mallory joins and is revoked before the request arrives.
	mallory, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := f.acc.IssueCert(mallory, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	if err := f.members.Remove(mallory.ID()); err != nil {
		t.Fatal(err)
	}

	rec := &recordingResolver{inner: MemberListAdapter{List: f.members}}
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: rec,
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ans, err := trunk.Decide(RelayOpen{From: mallory.ID(), To: f.bob.ID(), Hops: 1})
	if err == nil {
		t.Fatal("a revoked device was granted a relay session")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("the refusal is %v, want an auth-class error: a device that is not "+
			"a member must not be told anything else", err)
	}
	if ans.Grant {
		t.Fatal("the answer granted the relay to a revoked device")
	}
	if ans.Class != RelayClassAuth {
		t.Fatalf("the answer class is %q, want %q", ans.Class, RelayClassAuth)
	}
	if rec.saw(f.bob.ID()) {
		t.Fatal("the trunk looked the target up for a revoked device: the membership " +
			"check must come before anything else")
	}
}

// TestRelayOpenToANonMemberIsRefused covers the other half of the same
// rule: the TARGET has to be a member too, and the refusal must not leak
// the target's identity to the requester.
func TestRelayOpenToANonMemberIsRefused(t *testing.T) {
	f := newTrunkFixture(t)
	outsider, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ans, err := trunk.Decide(RelayOpen{From: f.alice.ID(), To: outsider.ID(), Hops: 1})
	if err == nil {
		t.Fatal("a relay to a non-member was granted")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("the refusal is %v, want an auth-class error: the target is not a member", err)
	}
	if ans.Grant {
		t.Fatal("the answer granted the relay to a non-member target")
	}
	if ans.Class != RelayClassAuth {
		t.Fatalf("the answer class is %q, want %q", ans.Class, RelayClassAuth)
	}
}

// TestRelayOpenIsRefusedWhenTheTrunkIsOff pins the anchor's control switch.
//
// The switch is OFF by default (D73): an anchor that relays for the whole
// network without being asked is a public relay. The refusal must be
// distinguishable from an auth failure — "this node does not relay" is a
// policy answer the requester can act on (try another relay), while "you
// are not a member" is a verdict it must not retry.
func TestRelayOpenIsRefusedWhenTheTrunkIsOff(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	ans, err := trunk.Decide(RelayOpen{From: f.alice.ID(), To: f.bob.ID(), Hops: 1})
	if err == nil {
		t.Fatal("the trunk granted a session while it was switched off")
	}
	if !errors.Is(err, ErrRelayTrunkDisabled) {
		t.Fatalf("the refusal is %v, want ErrRelayTrunkDisabled: the requester has to "+
			"be able to tell 'this node does not relay' from 'you are not a member'", err)
	}
	if errors.Is(err, ErrAuth) {
		t.Fatal("the 'trunk off' answer must not carry the auth class: a requester " +
			"must not treat a policy answer as a verdict on its own credentials")
	}
	if ans.Grant {
		t.Fatal("the answer granted the relay while the trunk was off")
	}
	if ans.Class != RelayClassDisabled {
		t.Fatalf("the answer class is %q, want %q", ans.Class, RelayClassDisabled)
	}
	if ans.Reason == "" {
		t.Fatal("the refusal carries no reason: the requester has to show something")
	}
}

// TestRelayOpenIsGrantedWhenTheTrunkIsOn is the happy path: two members of
// the same view, the trunk switched on, one hop.
func TestRelayOpenIsGrantedWhenTheTrunkIsOn(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ans, err := trunk.Decide(RelayOpen{From: f.alice.ID(), To: f.bob.ID(), Hops: 1})
	if err != nil {
		t.Fatalf("a valid relay request was refused: %v", err)
	}
	if !ans.Grant {
		t.Fatal("the answer does not grant a valid relay request")
	}
	if ans.Class != RelayClassGrant {
		t.Fatalf("the answer class is %q, want %q", ans.Class, RelayClassGrant)
	}
}

// TestRelayOpenRefusesSelfAndSameDevice covers the requests that make no
// sense as a relay: from the trunk itself, to the trunk itself, or from a
// device to itself. Relaying to yourself is a loop, and a trunk is not a
// useful relay for itself.
func TestRelayOpenRefusesSelfAndSameDevice(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		open RelayOpen
	}{
		{"from the trunk itself", RelayOpen{From: f.hub.ID(), To: f.bob.ID(), Hops: 1}},
		{"to the trunk itself", RelayOpen{From: f.alice.ID(), To: f.hub.ID(), Hops: 1}},
		{"from a device to itself", RelayOpen{From: f.alice.ID(), To: f.alice.ID(), Hops: 1}},
	} {
		ans, err := trunk.Decide(tt.open)
		if err == nil {
			t.Fatalf("%s: the request was granted", tt.name)
		}
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("%s: the refusal is %v, want ErrRefused", tt.name, err)
		}
		if ans.Grant {
			t.Fatalf("%s: the answer grants the request", tt.name)
		}
		if ans.Class != RelayClassRefused {
			t.Fatalf("%s: the answer class is %q, want %q", tt.name, ans.Class, RelayClassRefused)
		}
	}
}

// TestRelayOpenRefusesAMultiHopRequest keeps the promise that the trunk
// relays between two members and nothing more.
//
// Multi-hop routing (A→B→C→D) is not built yet, so a request that still
// needs more than one hop is refused with a clear reason instead of being
// half-served. A request that does not state its budget is a one-hop
// request.
func TestRelayOpenRefusesAMultiHopRequest(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ans, err := trunk.Decide(RelayOpen{From: f.alice.ID(), To: f.bob.ID(), Hops: MaxRelayHops + 1})
	if err == nil {
		t.Fatal("a multi-hop request was granted: the trunk only relays one hop")
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("the refusal is %v, want ErrRefused", err)
	}
	if ans.Class != RelayClassRefused {
		t.Fatalf("the answer class is %q, want %q", ans.Class, RelayClassRefused)
	}

	// A request that states no budget at all is a one-hop request.
	if _, err := trunk.Decide(RelayOpen{From: f.alice.ID(), To: f.bob.ID()}); err != nil {
		t.Fatalf("a request without a hop budget was refused: %v", err)
	}
}

// TestTrunkSwitchFlipsTheDecision pins the switch as a CONTROL: flipping
// it flips the decision, and the default is off.
func TestTrunkSwitchFlipsTheDecision(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := RelayOpen{From: f.alice.ID(), To: f.bob.ID(), Hops: 1}

	if trunk.Enabled() {
		t.Fatal("the trunk is on by default: an anchor must not relay for the whole " +
			"network unless it was told to")
	}
	if _, err := trunk.Decide(req); !errors.Is(err, ErrRelayTrunkDisabled) {
		t.Fatalf("the request was answered %v while the trunk was off", err)
	}

	trunk.SetEnabled(true)
	if !trunk.Enabled() {
		t.Fatal("the trunk did not switch on")
	}
	if _, err := trunk.Decide(req); err != nil {
		t.Fatalf("the trunk refused a valid request after being switched on: %v", err)
	}

	trunk.SetEnabled(false)
	if trunk.Enabled() {
		t.Fatal("the trunk did not switch off")
	}
	if _, err := trunk.Decide(req); !errors.Is(err, ErrRelayTrunkDisabled) {
		t.Fatalf("the request was answered %v after the trunk was switched off", err)
	}
}

// TestRelayOpenFrameIsAnswered drives the whole control-frame path: the
// request goes out as a frame, comes back as a frame, and the answer
// carries the verdict.
func TestRelayOpenFrameIsAnswered(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	in, err := RelayOpenFrame(RelayOpen{From: f.alice.ID(), To: f.bob.ID(), Hops: 1})
	if err != nil {
		t.Fatal(err)
	}
	if in.Type != wire.FrameMeshRelayOpen {
		t.Fatalf("the request frame type is %v, want %v", in.Type, wire.FrameMeshRelayOpen)
	}
	enc, err := in.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	arrived, err := wire.DecodeFrameBytes(enc)
	if err != nil {
		t.Fatal(err)
	}

	out, err := trunk.AnswerFrame(arrived)
	if err != nil {
		t.Fatalf("a valid relay request was refused: %v", err)
	}
	if out.Type != wire.FrameMeshRelayAnswer {
		t.Fatalf("the answer frame type is %v, want %v", out.Type, wire.FrameMeshRelayAnswer)
	}
	ans, err := DecodeRelayAnswer(out.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !ans.Grant || ans.Class != RelayClassGrant {
		t.Fatalf("the answer is %+v, want a grant", ans)
	}
}

// TestAnswerFrameRefusesAGrantAsWell: the answer frame is returned even
// when the verdict is a refusal — the requester has to learn WHY it was
// refused — and the error tells the local caller what class it was.
func TestAnswerFrameRefusesAGrantAsWell(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	in, err := RelayOpenFrame(RelayOpen{From: f.alice.ID(), To: f.bob.ID(), Hops: 1})
	if err != nil {
		t.Fatal(err)
	}
	out, err := trunk.AnswerFrame(in)
	if !errors.Is(err, ErrRelayTrunkDisabled) {
		t.Fatalf("the verdict is %v, want ErrRelayTrunkDisabled", err)
	}
	if out == nil {
		t.Fatal("no answer frame was returned: the requester cannot learn the reason")
	}
	ans, err := DecodeRelayAnswer(out.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Grant || ans.Class != RelayClassDisabled {
		t.Fatalf("the answer is %+v, want a disabled refusal", ans)
	}
}

// TestAnswerFrameRejectsAForeignFrame: the trunk answers relay requests
// and nothing else. A frame of another type is a protocol error, not a
// relay request, and it must not produce an answer.
func TestAnswerFrameRejectsAForeignFrame(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := trunk.AnswerFrame(&wire.Frame{Type: wire.FrameMsg})
	if err == nil {
		t.Fatal("a foreign frame was answered as a relay request")
	}
	if out != nil {
		t.Fatal("a foreign frame produced an answer frame")
	}
}

// TestRelayOpenAndAnswerCodecRoundTrip pins the payload codec: the
// request and the answer must survive the trip through the wire format.
func TestRelayOpenAndAnswerCodecRoundTrip(t *testing.T) {
	open := RelayOpen{From: "device-a", To: "device-b", Hops: 1}
	payload, err := EncodeRelayOpen(open)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRelayOpen(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got != open {
		t.Fatalf("the open payload round-tripped to %+v, want %+v", got, open)
	}

	ans := RelayAnswer{Grant: false, Reason: "not a member", Class: RelayClassAuth}
	payload, err = EncodeRelayAnswer(ans)
	if err != nil {
		t.Fatal(err)
	}
	gotAns, err := DecodeRelayAnswer(payload)
	if err != nil {
		t.Fatal(err)
	}
	if gotAns != ans {
		t.Fatalf("the answer payload round-tripped to %+v, want %+v", gotAns, ans)
	}

	if _, err := DecodeRelayOpen([]byte("{")); err == nil {
		t.Fatal("a broken open payload decoded without an error")
	}
	if _, err := DecodeRelayAnswer([]byte("{")); err == nil {
		t.Fatal("a broken answer payload decoded without an error")
	}
}

// TestInstalledRelayTrunkIsWhatWasSet pins the process-wide installation
// point that the anchor's --relay-trunk flag uses.
//
// The trunk is a property of the NODE, not of a session, so it is
// installed once and read back by the data plane. Nothing installs one on
// its own: a node that never asked for a trunk must not find one.
func TestInstalledRelayTrunkIsWhatWasSet(t *testing.T) {
	if InstalledRelayTrunk() != nil {
		t.Fatal("a trunk was installed before anything asked for one")
	}
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetRelayTrunk(nil) })

	SetRelayTrunk(trunk)
	if InstalledRelayTrunk() != trunk {
		t.Fatal("the installed trunk is not the one that was set")
	}
	if !InstalledRelayTrunk().Enabled() {
		t.Fatal("the installed trunk is not on: the --relay-trunk switch would not take effect")
	}
}

// TestRelayPathCarriesTheRelayEndpoint pins the two new Path fields.
//
// ViaEndpoints is the address of the RELAY member: a relay path has two
// address segments (caller→relay, relay→target) and the first one had
// nowhere to live, so a relay path could not even name the node it meant
// to go through. Hops is the remaining hop budget, one for a single hop.
//
// The dial path still refuses to pretend: the data plane is not built, so
// the relay path keeps its own error class.
func TestRelayPathCarriesTheRelayEndpoint(t *testing.T) {
	relay := Path{
		Kind:         PathRelay,
		Via:          "relay-one",
		ViaEndpoints: []string{"198.51.100.7:7000"},
		Hops:         1,
	}

	picker := DirectFirstPicker{Relays: []Path{relay}}
	paths, err := picker.Pick(context.Background(), "target", "msg")
	if err != nil {
		t.Fatal(err)
	}
	var got Path
	for _, p := range paths {
		if p.Kind == PathRelay {
			got = p
		}
	}
	if got.Via != relay.Via {
		t.Fatalf("the picker changed the relay member: %+v", got)
	}
	if len(got.ViaEndpoints) != 1 || got.ViaEndpoints[0] != relay.ViaEndpoints[0] {
		t.Fatalf("the picker dropped the relay endpoint: %+v", got)
	}
	if got.Hops != 1 {
		t.Fatalf("the picker dropped the hop budget: %+v", got)
	}

	_, err = dialPath(context.Background(), DialConfig{Target: "target"}, relay)
	if !errors.Is(err, ErrRelayUnavailable) {
		t.Fatalf("the relay path reports %v, want ErrRelayUnavailable: the data plane "+
			"is not built yet and the path must not pretend otherwise", err)
	}
}

// ---------------------------------------------------------------------------
// The DATA plane tests.
//
// The control plane above proves the trunk DECIDES. These prove it CARRIES:
// that the bytes flow, that the authorization is re-checked before they
// flow, that the quota is enforced, and that a revocation reaches an
// ESTABLISHED relay.
// ---------------------------------------------------------------------------

// pipeSession is an in-memory Session for the data plane tests.
//
// It is NOT net.Pipe: the relay's splice depends on the HALF-close
// semantics of Session (Close() = FIN, the receive side stays open), and
// net.Pipe does not implement CloseWrite — a test built on it would
// exercise the wrong branch (see CONVENTIONS.md T-2.8).
//
// Each pipeSession has its OWN inbound queue. A session's Write puts bytes
// on the PEER's queue, and its Read takes them off its own. That is what
// makes a pair of sessions a wire: writing to one is reading from the
// other, and nothing loops back.
//
// CLOSE SEMANTICS OF THIS DOUBLE: Close() is a HALF close — it ends the
// SEND side only, so the peer sees EOF while this end keeps reading. The
// real Session does exactly that (quic.Stream.Close() writes a FIN), and
// the relay's splice relies on it to carry an answer that arrives AFTER
// the request ended. Abort() is the hard end: both sides go down.
//
// What this double still exercises is that the reader drains the bytes
// that are ALREADY QUEUED before it reports EOF, so a message's tail
// cannot be lost. The half-close PROPAGATION is what the relay's splice
// does (it closes the far side when the near side ends), and that is what
// the forwarding test asserts.
type pipeSession struct {
	// in is this session's inbound queue. The peer writes into it.
	in chan []byte
	// peer is the other end. Writing puts bytes on the peer's queue.
	peer *pipeSession
	// closed is closed when this session ends.
	closed    chan struct{}
	closeOnce sync.Once
	// sendClosed is closed when this session ends its SEND side only.
	// The peer sees EOF on it; this end can still READ what the peer
	// sends. That is the HALF close the real Session does
	// (quic.Stream.Close() writes a FIN), and the relay's splice has to
	// survive it: "send the request, FIN, then wait for the answer" is
	// what every request/response protocol does.
	sendClosed chan struct{}
	sendOnce   sync.Once
	// readErr, when set, is returned by the next Read. It is how a test
	// simulates "the peer hung up".
	readErr error
	// readDeadline bounds a blocking Read. The zero value means "no
	// deadline".
	readDeadline time.Time
	mu           sync.Mutex
	// seen records every byte that crossed this session, so a test can
	// assert what the OTHER end could read. It is how the end-to-end
	// encryption test checks that the trunk never held the plaintext: the
	// trunk's own session is inspected for the secret.
	seenMu sync.Mutex
	seen   []byte
}

// record notes the bytes that crossed this session.
func (p *pipeSession) record(b []byte) {
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	p.seen = append(p.seen, b...)
}

// seenBytes returns everything that crossed this session so far.
func (p *pipeSession) seenBytes() []byte {
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	out := make([]byte, len(p.seen))
	copy(out, p.seen)
	return out
}

// newPipeSessionPair builds two connected sessions.
func newPipeSessionPair() (*pipeSession, *pipeSession) {
	a := &pipeSession{in: make(chan []byte, 64), closed: make(chan struct{}), sendClosed: make(chan struct{})}
	b := &pipeSession{in: make(chan []byte, 64), closed: make(chan struct{}), sendClosed: make(chan struct{})}
	a.peer = b
	b.peer = a
	return a, b
}

// Write puts bytes on the wire (into the peer's inbound queue).
//
// A session that has been ABORTED refuses writes DETERMINISTICALLY. The
// check used to live inside the select, next to the delivery case — and
// a select with two ready arms picks at RANDOM, so a write onto an
// aborted session sometimes landed in the peer's buffer anyway. A test
// that needs "the write to the target failed" cannot tolerate a coin
// flip; neither can a protocol (on the real stream a cancelled session
// fails every subsequent write, promptly).
func (p *pipeSession) Write(b []byte) (int, error) {
	select {
	case <-p.closed:
		return 0, errors.New("pipe session closed")
	default:
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	p.record(cp)
	select {
	case p.peer.in <- cp:
		return len(b), nil
	case <-p.closed:
		return 0, errors.New("pipe session closed")
	}
}

// Read takes bytes off the wire.
//
// It drains the bytes that are already queued before reporting EOF, so a
// message's tail is never lost to the end-of-stream signal.
//
// The EOF comes when EITHER side ends: the peer closing its send side is
// what the reader must see (that is the half-close propagation the
// relay's splice depends on), and its own close is the local shutdown.
func (p *pipeSession) Read(b []byte) (int, error) {
	p.mu.Lock()
	readErr := p.readErr
	deadline := p.readDeadline
	p.mu.Unlock()
	if readErr != nil {
		return 0, readErr
	}
	// Drain first: bytes that are already queued must not be lost to
	// the end-of-stream signal.
	select {
	case chunk, ok := <-p.in:
		if !ok {
			return 0, io.EOF
		}
		return copy(b, chunk), nil
	default:
	}
	// The deadline turns "the peer will never send anything" into a
	// prompt failure instead of a hang.
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timeout = timer.C
	}
	// An ABORT outranks a FIN. Abort() closes BOTH of the peer's end
	// signals (the session's close and its send side), and a select with
	// two ready arms picks at random — so a reader could see a graceful
	// EOF for an aborted stream half the time. On the real stream a
	// RESET_STREAM makes the read fail even when a FIN was also sent, so
	// the abort is checked FIRST, deterministically.
	select {
	case <-p.peer.closed:
		// The peer was ABORTED, not half-closed: on the real stream this
		// is a RESET_STREAM, and a reset makes the reader FAIL, not reach
		// a clean end. The double used to report io.EOF here — which made
		// an aborted session indistinguishable from a graceful finish,
		// and a truncation dressed up as a completion could not be seen.
		return 0, errPeerAborted
	default:
	}
	select {
	case chunk, ok := <-p.in:
		if !ok {
			return 0, io.EOF
		}
		return copy(b, chunk), nil
	case <-p.closed:
		return 0, io.EOF
	case <-timeout:
		return 0, os.ErrDeadlineExceeded
	case <-p.peer.sendClosed:
		// The peer HALF-closed: it will send nothing more, but it is
		// still listening. This is the end of the request, not the end
		// of the conversation.
		//
		// RE-CHECK the abort: this reader may have gone to sleep while
		// only the FIN was visible, and the abort then landed — a select
		// blocked on two ready arms picks at random, so without the
		// re-check an aborted stream would read as a graceful end about
		// half the time. Abort closes the session channel FIRST, so a
		// send-side close that was not an abort never has the session
		// channel closed behind it.
		select {
		case <-p.peer.closed:
			return 0, errPeerAborted
		default:
		}
		return p.drainThenEOF(b)
	}
}

// errPeerAborted is what a reader sees when the PEER's session was
// aborted (a RESET_STREAM on the real wire).
var errPeerAborted = errors.New("pipe session: the peer aborted the stream")

// drainThenEOF hands back the bytes that are already queued and then
// reports the end of the stream.
func (p *pipeSession) drainThenEOF(b []byte) (int, error) {
	select {
	case chunk, ok := <-p.in:
		if !ok {
			return 0, io.EOF
		}
		return copy(b, chunk), nil
	default:
	}
	return 0, io.EOF
}

// Close ends the SEND side of the session.
//
// It is a HALF close: the peer sees the end of this direction, and this
// end keeps its receive side open. The real Session is a half close
// (quic.Stream.Close() writes a FIN), and the relay's splice DEPENDS on
// it: a request that ends before its answer arrives must not take the
// answer down with it.
//
// The double used to close both sides at once, which is not what the real
// session does — and the difference is invisible until the half-close
// protocols are exercised.
func (p *pipeSession) Close() error {
	p.CloseWrite()
	return nil
}

// Abort ends the session on BOTH sides.
//
// The half close above is what a FIN means. A TEARDOWN is different: the
// peer must not be able to keep the session alive by ignoring the FIN,
// and this end's pending reads must fail instead of waiting for a peer
// that is being cut off.
func (p *pipeSession) Abort() error {
	p.closeOnce.Do(func() { close(p.closed) })
	p.CloseWrite()
	return nil
}

// CloseWrite ends the SEND side only.
//
// The peer sees EOF — nothing more will arrive on this direction — while
// this end keeps its receive side open. The real Session is a half close
// (quic.Stream.Close() writes a FIN and the receive side stays open), and
// the relay's splice depends on it: a request that ends before its answer
// arrives must not take the answer down with it.
func (p *pipeSession) CloseWrite() error {
	p.sendOnce.Do(func() { close(p.sendClosed) })
	return nil
}

// SetReadDeadline arms a read deadline.
//
// The pipe used to ignore it, which meant a test that waited for a
// record that would never arrive HUNG instead of failing. A test that
// hangs is a test that never reports: the red it was supposed to show
// never showed up. The deadline turns "the record never came" into a
// prompt, actionable failure.
func (p *pipeSession) SetReadDeadline(t time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readDeadline = t
	return nil
}

func (p *pipeSession) SetWriteDeadline(t time.Time) error { return nil }

// failRead makes the next Read return err (simulating a hung-up peer).
func (p *pipeSession) failRead(err error) {
	p.mu.Lock()
	p.readErr = err
	p.mu.Unlock()
}

// TestTrunkOpenFlowForwardsBytes: the relay actually carries data.
//
// This is the base case of the whole data plane: A writes, the trunk
// forwards, C receives. Without it the trunk is a state machine that
// grants permissions and moves nothing.
//
// The splice forwards RECORDS (the relay protocol's envelope), so the
// test writes a record and reads a record — not raw bytes. The payload
// it carries is opaque to the trunk, which is the point: the trunk
// forwards it without opening it.
func TestTrunkOpenFlowForwardsBytes(t *testing.T) {
	f := newTrunkFixture(t)
	// The relay has TWO hops, and each hop is a pair of sessions:
	//
	//   A --- aRead ---> [trunk] ---> tRead --- C
	//
	// The trunk READS from aRead and WRITES to tRead. C reads from the
	// other end of the tRead pair. A writes to the other end of the
	// aRead pair.
	aRead, aWrite := newPipeSessionPair()
	tRead, cRead := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			if target != f.bob.ID() {
				t.Fatalf("the trunk dialed %s, want %s", target, f.bob.ID())
			}
			return tRead, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	id, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
		Kind: KindPing,
	}, aRead)
	if err != nil {
		t.Fatalf("the trunk refused a valid relay: %v", err)
	}
	if id == 0 {
		t.Fatal("the flow got no session id")
	}

	// A sends a record; C must receive the SAME record, verbatim.
	go func() {
		_ = writeRelayRecord(aWrite, relayRecord{
			Type: uint8(wire.FrameMeshRelayData),
			Data: []byte("ciphertext the trunk cannot read"),
		})
	}()

	// The FIRST record the target sees is the OPEN record: it is what
	// tells the target who is calling. Without it the target has no way
	// of knowing whose end-to-end hello it is looking at (defect C of the
	// round-61 real-machine report).
	cBr := bufio.NewReader(cRead)
	open, err := readRelayRecord(cBr)
	if err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the target received record type %d, want an open record", open.Type)
	}
	if open.From != f.alice.ID() {
		t.Fatalf("the open record names %q, want the requester %s", open.From, f.alice.ID())
	}
	if open.Kind != KindPing {
		t.Fatalf("the open record names kind %q, want %q", open.Kind, KindPing)
	}

	rec, err := readRelayRecord(cBr)
	if err != nil {
		t.Fatalf("the target received no data record: %v", err)
	}
	if rec.Type != uint8(wire.FrameMeshRelayData) {
		t.Fatalf("the target received record type %d, want a data record", rec.Type)
	}
	if string(rec.Data) != "ciphertext the trunk cannot read" {
		t.Fatalf("the target received %q: the trunk must forward the payload VERBATIM", rec.Data)
	}
}

// TestTrunkOpenFlowRechecksMembership: the authorization is RE-CHECKED
// before the bytes move.
//
// The control plane checked the membership when the request arrived. The
// data plane must check it AGAIN, because the request may sit in a queue
// while a device gets revoked — and a revoked device's traffic must not
// be carried, and its identity must not cause the target to be touched.
func TestTrunkOpenFlowRechecksMembership(t *testing.T) {
	f := newTrunkFixture(t)
	req, tgt := newPipeSessionPair()
	dialed := false
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			dialed = true
			return tgt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The requester is revoked AFTER the control plane granted it.
	if err := f.members.Remove(f.alice.ID()); err != nil {
		t.Fatal(err)
	}

	_, err = trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, req)
	if err == nil {
		t.Fatal("a revoked requester was granted a data flow")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("the refusal is %v, want an auth-class error", err)
	}
	if dialed {
		t.Fatal("the trunk dialled the target for a revoked requester: the membership " +
			"check must come before anything that touches a third party")
	}
}

// TestTrunkOpenFlowRefusesWhenTheSwitchIsOff: the anchor's switch governs
// the DATA plane too, not just the control plane.
//
// A trunk that is switched off must not carry traffic even if it granted
// an answer a moment earlier.
func TestTrunkOpenFlowRefusesWhenTheSwitchIsOff(t *testing.T) {
	f := newTrunkFixture(t)
	req, tgt := newPipeSessionPair()
	dialed := false
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			dialed = true
			return tgt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	trunk.SetEnabled(false)

	_, err = trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, req)
	if err == nil {
		t.Fatal("the trunk opened a flow while switched off")
	}
	if !errors.Is(err, ErrRelayTrunkDisabled) {
		t.Fatalf("the refusal is %v, want ErrRelayTrunkDisabled", err)
	}
	if dialed {
		t.Fatal("the trunk dialled the target while switched off")
	}
}

// TestTrunkQuotaRefusesTheTwentyFirstStream: P-7's stream limit.
//
// The 21st concurrent relay must be refused with ErrQuota, and the 20
// that are already running must NOT be killed — a 2 GB transfer in
// progress must not be thrown away because the trunk is busy.
func TestTrunkQuotaRefusesTheTwentyFirstStream(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			_, tgt := newPipeSessionPair()
			return tgt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fill the quota.
	for i := 0; i < DefaultRelayMaxStreams; i++ {
		req, _ := newPipeSessionPair()
		if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
			From: f.alice.ID(),
			To:   f.bob.ID(),
			Hops: 1,
		}, req); err != nil {
			t.Fatalf("stream %d was refused: %v", i, err)
		}
	}
	if got := trunk.FlowCount(); got != DefaultRelayMaxStreams {
		t.Fatalf("the trunk carries %d streams, want %d", got, DefaultRelayMaxStreams)
	}

	// The 21st must be refused with the quota class.
	req, _ := newPipeSessionPair()
	_, err = trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, req)
	if err == nil {
		t.Fatal("the 21st concurrent relay was granted: the P-7 stream limit is 20")
	}
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("the refusal is %v, want ErrQuota: the quota sentinel has to be reachable "+
			"with errors.Is so the caller can tell 'this node is busy' from 'you are not a member'", err)
	}
	// And the running ones must still be there.
	if got := trunk.FlowCount(); got != DefaultRelayMaxStreams {
		t.Fatalf("the trunk now carries %d streams, want %d: a quota refusal must NOT kill "+
			"the relays that are already running", got, DefaultRelayMaxStreams)
	}
	trunk.CloseAll()
}

// TestTrunkQuotaDoesNotCountItsOwnBusiness: P-7's "自身业务不受影响".
//
// The quota counts the bytes the trunk FORWARDS. The trunk's own traffic
// does not pass through the splice, so it must not be counted — otherwise
// a busy anchor would lock ITSELF out of relaying.
func TestTrunkQuotaDoesNotCountItsOwnBusiness(t *testing.T) {
	f := newTrunkFixture(t)
	_, tgt := newPipeSessionPair()
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Quota: RelayQuota{
			MaxStreams:     DefaultRelayMaxStreams,
			MaxBytesPerSec: 1024, // deliberately tiny
		},
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tgt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The trunk writes to itself: that is its own business traffic, and
	// it does not go through the splice. A drain goroutine keeps the
	// pipe from filling up.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		buf := make([]byte, 4096)
		for {
			if _, err := tgt.peer.Read(buf); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		_, _ = tgt.Write(make([]byte, 1024))
	}
	// Give the writes a moment to land.
	time.Sleep(50 * time.Millisecond)

	// A relay must still be grantable: the trunk's own traffic did not
	// eat the relay budget.
	req2, tgt2 := newPipeSessionPair()
	t.Cleanup(func() {
		_ = req2.Close()
		_ = tgt2.Close()
	})
	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, req2); err != nil {
		t.Fatalf("the trunk's own traffic consumed the relay quota: %v", err)
	}
}

// TestTrunkRevocationTearsDownAnEstablishedRelay: the half the previous
// round did NOT do.
//
// Refusing a NEW request from a revoked device is not enough: a relay
// that was GRANTED before the revocation keeps running until it ends on
// its own. The revocation must reach the ESTABLISHED relay.
func TestTrunkRevocationTearsDownAnEstablishedRelay(t *testing.T) {
	f := newTrunkFixture(t)
	req, tgt := newPipeSessionPair()
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tgt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, req); err != nil {
		t.Fatal(err)
	}
	if got := trunk.FlowCount(); got != 1 {
		t.Fatalf("the trunk carries %d flows, want 1", got)
	}

	// The requester is revoked.
	if err := f.members.Remove(f.alice.ID()); err != nil {
		t.Fatal(err)
	}

	// The established relay must be torn down.
	revoked := trunk.RevokeFlows()
	if revoked != 1 {
		t.Fatalf("RevokeFlows tore down %d relays, want 1: a revoked device's "+
			"ESTABLISHED relay must be torn down, not just its next request", revoked)
	}
	if got := trunk.FlowCount(); got != 0 {
		t.Fatalf("the trunk still carries %d flows after the revocation", got)
	}
}

// TestTrunkRevocationIsScoped: revoking one device must not kill the
// relays of the others.
func TestTrunkRevocationIsScoped(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			_, tgt := newPipeSessionPair()
			return tgt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two relays, both involving alice.
	for i := 0; i < 2; i++ {
		req, _ := newPipeSessionPair()
		if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
			From: f.alice.ID(),
			To:   f.bob.ID(),
			Hops: 1,
		}, req); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.members.Remove(f.alice.ID()); err != nil {
		t.Fatal(err)
	}
	if revoked := trunk.RevokeFlows(); revoked != 2 {
		t.Fatalf("RevokeFlows tore down %d relays, want 2", revoked)
	}
}

// ---------------------------------------------------------------------------
// The relay PROTOCOL tests.
//
// These pin the conversation between the three parties, and the one
// property that matters most: the trunk forwards the end-to-end handshake
// and the ciphertext VERBATIM, so it cannot read either.
// ---------------------------------------------------------------------------

// TestRelayRecordRoundTrip pins the envelope's wire shape.
func TestRelayRecordRoundTrip(t *testing.T) {
	rec := relayRecord{
		Type:    uint8(wire.FrameMeshRelayData),
		Session: 42,
		Data:    []byte("opaque ciphertext"),
	}
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var got relayRecord
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != rec.Type || got.Session != rec.Session {
		t.Fatalf("the record round-tripped to %+v, want %+v", got, rec)
	}
	if string(got.Data) != string(rec.Data) {
		t.Fatalf("the opaque payload did not survive: %q", got.Data)
	}
}

// TestRelayFramePayloadCodec pins the data-frame payload codec.
func TestRelayFramePayloadCodec(t *testing.T) {
	p := RelayFramePayload{Session: 7, Data: []byte("ciphertext"), Close: true, Reason: "bye"}
	raw, err := EncodeRelayFrame(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRelayFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Session != p.Session || got.Close != p.Close || got.Reason != p.Reason {
		t.Fatalf("the payload round-tripped to %+v, want %+v", got, p)
	}
	if string(got.Data) != string(p.Data) {
		t.Fatalf("the opaque payload did not survive: %q", got.Data)
	}
	if _, err := DecodeRelayFrame([]byte("{")); err == nil {
		t.Fatal("a broken payload decoded without an error")
	}
}

// TestRelayFrameTypesAreDistinct pins that the relay frames are not
// confused with each other: the trunk dispatches on the type byte, and a
// collision would mix a data frame into a close.
func TestRelayFrameTypesAreDistinct(t *testing.T) {
	data, err := RelayFrame(1, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if data.Type != wire.FrameMeshRelayData {
		t.Fatalf("the data frame type is %v, want %v", data.Type, wire.FrameMeshRelayData)
	}
	closeFrame, err := RelayCloseFrame(1, "bye")
	if err != nil {
		t.Fatal(err)
	}
	if closeFrame.Type != wire.FrameMeshRelayClose {
		t.Fatalf("the close frame type is %v, want %v", closeFrame.Type, wire.FrameMeshRelayClose)
	}
	if data.Type == closeFrame.Type {
		t.Fatal("the data and close frames share a type")
	}
	join, err := RelayJoinFrame(RelayJoin{Device: "abcdefghijklmnop"})
	if err != nil {
		t.Fatal(err)
	}
	if join.Type != wire.FrameMeshRelayJoin {
		t.Fatalf("the join frame type is %v, want %v", join.Type, wire.FrameMeshRelayJoin)
	}
}

// TestRelayJoinCodecRoundTrip pins the join announcement's codec.
func TestRelayJoinCodecRoundTrip(t *testing.T) {
	j := RelayJoin{Device: "abcdefghijklmnop"}
	raw, err := EncodeRelayJoin(j)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRelayJoin(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != j {
		t.Fatalf("the join round-tripped to %+v, want %+v", got, j)
	}
	if _, err := DecodeRelayJoin([]byte("{")); err == nil {
		t.Fatal("a broken join decoded without an error")
	}
}

// ---------------------------------------------------------------------------
// The END-TO-END relay session.
//
// This is the headline requirement: a real A→B→C relayed session, where
// A and C both reach the anchor B, and C receives what A sent.
//
// It exercises the WHOLE chain: the requester's open record, the trunk's
// authorization, the end-to-end handshake THROUGH the trunk, and the
// payload moving as ciphertext.
// ---------------------------------------------------------------------------

// joinedTarget is a test double for a member that has JOINED the trunk.
//
// It is what a pure-outbound member looks like from the trunk's side: a
// long-lived connection plus the ability to open a FRESH session for each
// relayed session. The fresh session is the whole point — before, the
// trunk spliced against the join connection itself, and one relay's end
// took the target's membership of the trunk down with it (defect E of the
// round-61 real-machine report).
type joinedTarget struct {
	device identity.DeviceID
	// sessions receives the TARGET end of each fresh session, so the
	// test can serve it.
	sessions chan *pipeSession
	// open holds the trunk end of the last fresh session, so the test
	// can inspect what crossed it.
	trunkEnds chan *pipeSession
}

func newJoinedTarget(device identity.DeviceID) *joinedTarget {
	return &joinedTarget{
		device:    device,
		sessions:  make(chan *pipeSession, 8),
		trunkEnds: make(chan *pipeSession, 8),
	}
}

// open is the TrunkJoiner opener: one fresh session per relayed session.
func (j *joinedTarget) open(ctx context.Context) (Session, error) {
	trunkEnd, targetEnd := newPipeSessionPair()
	j.trunkEnds <- trunkEnd
	j.sessions <- targetEnd
	return trunkEnd, nil
}

// serveOne serves the next relayed session: it reads the OPEN record
// (which names the requester) and then performs the target's half of the
// end-to-end handshake.
//
// It returns the plain-bytes session and the requester.
func (j *joinedTarget) serveOne(t *testing.T, cfg RelayTargetConfig) (Session, identity.DeviceID, error) {
	t.Helper()
	var targetEnd *pipeSession
	select {
	case targetEnd = <-j.sessions:
	case <-time.After(10 * time.Second):
		t.Fatal("the trunk never opened a session towards the target")
		return nil, "", nil
	}
	br := bufio.NewReader(targetEnd)
	open, err := readRelayRecord(br)
	if err != nil {
		t.Fatalf("the target received no open record: %v", err)
		return nil, "", err
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the target received record type %d, want an open record", open.Type)
		return nil, "", err
	}
	sess, err := ServeRelayTarget(targetEnd, framedRecordReader{br: br}, cfg)
	if err != nil {
		return nil, open.From, err
	}
	return sess, open.From, nil
}

// TestRelaySessionCarriesAPayloadEndToEnd is the headline test.
//
// The chain:
//
//	A --(open record)--> B (trunk) --(dial C)--> C
//	A --(hello, signed)--> B --> C
//	C --(hello, signed)--> B --> A
//	A --(sealed payload)--> B --> C
//
// C must receive A's plaintext, and B must not be able to read it.
func TestRelaySessionCarriesAPayloadEndToEnd(t *testing.T) {
	f := newTrunkFixture(t)

	// The trunk's connection to A.
	trunkToA, aSide := newPipeSessionPair()

	joiners := NewJoinerTable()
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: joiners,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The trunk serves A's connection in the background.
	go func() {
		_ = trunk.ServeRequester(context.Background(), trunkToA, joiners)
	}()

	// C has joined the trunk (a pure-outbound member has no inbound
	// port, so the anchor comes to it over the connection it opened).
	target := newJoinedTarget(f.bob.ID())
	joiners.Add(&TrunkJoiner{
		Device: f.bob.ID(),
		sess:   trunkToA, // unused by the flow; the opener is what matters
		br:     bufio.NewReader(trunkToA),
		open:   target.open,
	})

	// C serves its end of the relayed session in the background.
	targetDone := make(chan error, 1)
	go func() {
		sess, _, err := target.serveOne(t, RelayTargetConfig{
			Device:       f.bob,
			Requester:    f.alice.ID(),
			RequesterKey: f.alice.PublicKey(),
		})
		if err != nil {
			targetDone <- err
			return
		}
		// Read the payload C is supposed to receive.
		buf := make([]byte, 256)
		n, err := sess.Read(buf)
		if err != nil {
			targetDone <- err
			return
		}
		targetDone <- fmt.Errorf("payload=%q", buf[:n])
	}()

	// A opens the relayed session.
	// A's connection to the trunk is the OTHER end of trunkToA.
	aConn := aSide
	sess, err := OpenRelay(context.Background(), RelayRequesterConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Kind:      KindPing,
		TargetKey: f.bob.PublicKey(),
		Relay:     f.hub.ID(),
		Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
			return aConn, nil
		},
	})
	if err != nil {
		t.Fatalf("the relayed session could not be opened: %v", err)
	}

	// A sends a payload.
	payload := []byte("the quick brown fox jumps over the lazy dog")
	if _, err := sess.Write(payload); err != nil {
		t.Fatalf("the payload could not be written: %v", err)
	}

	select {
	case res := <-targetDone:
		if res == nil {
			t.Fatal("the target received nothing")
		}
		want := fmt.Sprintf("payload=%q", payload)
		if res.Error() != want {
			t.Fatalf("the target got %v, want %v", res, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the target never received the payload")
	}
}

// TestRelaySessionTheTrunkCannotReadThePayload is the R-3.5 acceptance
// criterion, on the REAL relay path (not on the E2E primitive).
//
// The trunk's own session is the pipe between A and C. A test that reads
// the bytes on that pipe is reading exactly what the trunk sees.
func TestRelaySessionTheTrunkCannotReadThePayload(t *testing.T) {
	f := newTrunkFixture(t)

	trunkToA, aSide := newPipeSessionPair()

	joiners := NewJoinerTable()
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: joiners,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = trunk.ServeRequester(context.Background(), trunkToA, joiners)
	}()
	target := newJoinedTarget(f.bob.ID())
	joiners.Add(&TrunkJoiner{
		Device: f.bob.ID(),
		sess:   trunkToA,
		br:     bufio.NewReader(trunkToA),
		open:   target.open,
	})

	targetDone := make(chan string, 1)
	go func() {
		sess, _, err := target.serveOne(t, RelayTargetConfig{
			Device:       f.bob,
			Requester:    f.alice.ID(),
			RequesterKey: f.alice.PublicKey(),
		})
		if err != nil {
			targetDone <- "ERR: " + err.Error()
			return
		}
		buf := make([]byte, 256)
		n, err := sess.Read(buf)
		if err != nil {
			targetDone <- "ERR: " + err.Error()
			return
		}
		targetDone <- string(buf[:n])
	}()

	sess, err := OpenRelay(context.Background(), RelayRequesterConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Kind:      KindPing,
		TargetKey: f.bob.PublicKey(),
		Relay:     f.hub.ID(),
		Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
			return aSide, nil
		},
	})
	if err != nil {
		t.Fatalf("the relayed session could not be opened: %v", err)
	}

	secret := []byte("TOP SECRET: the merger price is 42 million")
	if _, err := sess.Write(secret); err != nil {
		t.Fatalf("the payload could not be written: %v", err)
	}

	var got string
	select {
	case got = <-targetDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the target never received the payload")
	}
	if got != string(secret) {
		t.Fatalf("the target received %q, want %q", got, secret)
	}

	// The part this test is NAMED for: the TRUNK must not be able to read
	// the payload. The trunk's own side of the relay is inspected for the
	// secret — if the bytes that crossed the trunk contained the
	// plaintext, the relay would be a reader of the traffic and R-3.5
	// would not hold.
	//
	// Checking only that the target received the right bytes would not
	// prove this at all: a relay that forwarded the plaintext would make
	// that check pass too.
	//
	// The target's side is the session the trunk opened for THIS relay —
	// the same session the trunk spliced against.
	var trunkToC *pipeSession
	select {
	case trunkToC = <-target.trunkEnds:
	case <-time.After(5 * time.Second):
		t.Fatal("the trunk never opened a session towards the target")
	}
	for _, side := range []struct {
		name string
		sess *pipeSession
	}{
		{"the trunk's side toward the requester", trunkToA},
		{"the trunk's side toward the target", trunkToC},
	} {
		crossed := side.sess.seenBytes()
		if bytes.Contains(crossed, secret) {
			t.Fatalf("%s carried the plaintext: the trunk can read the payload", side.name)
		}
	}
}

// TestTrunkRateQuotaIsCountedPerNode pins that the rate quota is the
// NODE's, not each flow's.
//
// P-7 (docs/REQUIREMENTS.md): "单节点被持续转发时，吞吐 ≤ 50 MiB/s、流数
// ≤ 20". The unit is the NODE. A per-flow window lets twenty flows each
// carry 50 MiB/s, so the node forwards a thousand — twenty times the cap
// the requirement states.
//
// The window counter therefore belongs to the trunk, not to the flow.
func TestTrunkRateQuotaIsCountedPerNode(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Quota: RelayQuota{
			MaxStreams:     20,
			MaxBytesPerSec: 64 << 10, // 64 KiB/s: small enough to hit
		},
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			_, tgt := newPipeSessionPair()
			return tgt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Two flows, each carrying half the node's per-second budget. Neither
	// flow alone is over the cap; together they are.
	limit := trunk.quota.MaxBytesPerSec
	for i := 0; i < 2; i++ {
		req, _ := newPipeSessionPair()
		if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
			From: f.alice.ID(),
			To:   f.bob.ID(),
			Hops: 1,
		}, req); err != nil {
			t.Fatalf("flow %d was refused: %v", i+1, err)
		}
		// Each flow pushes half the node's budget through.
		trunk.accountForward(int64(limit / 2))
	}
	// The third flow must be refused: the NODE is already over its cap.
	req, _ := newPipeSessionPair()
	_, err = trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, req)
	if err == nil {
		t.Fatal("a third flow was granted although the node was already over its rate quota")
	}
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("the refusal is %v, want the quota class", err)
	}
}

// ---------------------------------------------------------------------------
// The PICKER wiring.
//
// The coordinator's finding was that the picker had NO production call
// site: cfg.Picker was never set, so dialViaPicker short-circuited to
// the direct dial and the relay path had zero producers. These tests pin
// that the picker is now wired AND that wiring it does not break the
// direct path.
// ---------------------------------------------------------------------------

// TestDirectFirstPickerStillPrefersTheDirectPath pins the order.
//
// "中继只兜底：大文件走直连，直连失败再走中继" (D73): the direct path
// comes FIRST. A picker that offered the relay first would put every
// ping through a trunk — the opposite of the owner's ruling.
func TestDirectFirstPickerStillPrefersTheDirectPath(t *testing.T) {
	picker := DirectFirstPicker{Relays: []Path{
		{Kind: PathRelay, Via: "relay00000000000", ViaEndpoints: []string{"198.51.100.7:7000"}},
	}}
	paths, err := picker.Pick(context.Background(), "target000000000", KindPing)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("the picker returned no paths")
	}
	if paths[0].Kind != PathDirect {
		t.Fatalf("the first path is %q, want the direct path: the relay is only a fallback", paths[0].Kind)
	}
	var relays int
	for _, p := range paths[1:] {
		if p.Kind == PathRelay {
			relays++
		}
	}
	if relays != 1 {
		t.Fatalf("the picker offered %d relay paths, want 1", relays)
	}
}

// TestViewPickerOffersTheRelayCandidates pins that the picker names the
// relay candidates AT ALL.
//
// Without this, the relay path has zero producers and the whole data
// plane is unreachable — which is exactly the state the coordinator
// found.
func TestViewPickerOffersTheRelayCandidates(t *testing.T) {
	f := newTrunkFixture(t)
	// The hub is the relay candidate: it is a member with an endpoint,
	// and it is neither the requester nor the target.
	view := &discovery.MemberSnapshot{
		AccountID: f.acc.ID(),
		Members: map[identity.DeviceID]discovery.MemberView{
			f.alice.ID(): {Cert: f.aliceCert, Endpoints: []string{"10.0.0.1:7000"}},
			f.bob.ID():   {Cert: f.bobCert, Endpoints: []string{"10.0.0.2:7000"}},
			// The hub is a trunk: it accepts RELAYS on its own port,
			// which is not its direct-link port. A relay session speaks
			// its own ALPN, so a requester has to dial the relay
			// endpoint (D76: in the default shape the two share one
			// socket, and the ALPN decides).
			f.hub.ID(): {
				Cert:           f.hubCert,
				Endpoints:      []string{"10.0.0.3:7000"},
				RelayEndpoints: []string{"10.0.0.3:7001"},
			},
		},
	}
	picker := ViewPicker{
		View: func() *discovery.MemberSnapshot { return view },
		Self: f.alice.ID(),
		TargetKey: func(id identity.DeviceID) (ed25519.PublicKey, error) {
			return f.bob.PublicKey(), nil
		},
	}
	paths, err := picker.Pick(context.Background(), f.bob.ID(), KindPing)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 2 {
		t.Fatalf("the picker offered %d path(s), want the direct path plus the relay candidate: "+
			"without a relay path the whole relay data plane has no producer", len(paths))
	}
	if paths[0].Kind != PathDirect {
		t.Fatalf("the first path is %q, want the direct path", paths[0].Kind)
	}
	var relay *Path
	for i := range paths {
		if paths[i].Kind == PathRelay {
			relay = &paths[i]
		}
	}
	if relay == nil {
		t.Fatal("the picker offered no relay path")
	}
	if relay.Via != f.hub.ID() {
		t.Fatalf("the relay candidate is %s, want %s", relay.Via, f.hub.ID())
	}
	// The relay path is dialled on the trunk's RELAY endpoint, not on its
	// direct one: the two speak different ALPNs on different ports, and
	// the direct address would fail the ALPN negotiation.
	if len(relay.ViaEndpoints) != 1 || relay.ViaEndpoints[0] != "10.0.0.3:7001" {
		t.Fatalf("the relay path has no usable endpoint: %+v", relay)
	}
	if len(relay.TargetKey) != ed25519.PublicKeySize {
		t.Fatal("the relay path carries no target key: the requester could not verify " +
			"the target's signed hello")
	}
}

// TestViewPickerSkipsMembersWithoutARelayEndpoint pins that a member
// with no relay endpoint is not offered as a trunk.
//
// A relay session speaks its own ALPN and needs its own UDP port, so the
// candidate's DIRECT endpoints cannot carry it: dialling them fails the
// ALPN negotiation. A member that has announced no relay endpoint
// therefore cannot serve as a trunk, and offering it would put a path in
// the list that cannot work.
func TestViewPickerSkipsMembersWithoutARelayEndpoint(t *testing.T) {
	f := newTrunkFixture(t)
	view := &discovery.MemberSnapshot{
		AccountID: f.acc.ID(),
		Members: map[identity.DeviceID]discovery.MemberView{
			f.alice.ID(): {Cert: f.aliceCert, Endpoints: []string{"10.0.0.1:7000"}},
			f.bob.ID():   {Cert: f.bobCert, Endpoints: []string{"10.0.0.2:7000"}},
			// The hub has a direct endpoint but NO relay endpoint: it
			// does not run a trunk.
			f.hub.ID(): {Cert: f.hubCert, Endpoints: []string{"10.0.0.3:7000"}},
		},
	}
	picker := ViewPicker{
		View: func() *discovery.MemberSnapshot { return view },
		Self: f.alice.ID(),
		TargetKey: func(id identity.DeviceID) (ed25519.PublicKey, error) {
			return f.bob.PublicKey(), nil
		},
	}
	paths, err := picker.Pick(context.Background(), f.bob.ID(), KindPing)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if p.Kind == PathRelay {
			t.Fatalf("a member without a relay endpoint was offered as a trunk: %+v", p)
		}
	}
}

// TestViewPickerExcludesTheRequesterAndTheTarget: a member cannot relay
// to itself, and the target is reached directly.
func TestViewPickerExcludesTheRequesterAndTheTarget(t *testing.T) {
	f := newTrunkFixture(t)
	view := &discovery.MemberSnapshot{
		AccountID: f.acc.ID(),
		Members: map[identity.DeviceID]discovery.MemberView{
			f.alice.ID(): {Cert: f.aliceCert, Endpoints: []string{"10.0.0.1:7000"}},
			f.bob.ID():   {Cert: f.bobCert, Endpoints: []string{"10.0.0.2:7000"}},
			f.hub.ID():   {Cert: f.hubCert, Endpoints: []string{"10.0.0.3:7000"}},
		},
	}
	picker := ViewPicker{
		View:      func() *discovery.MemberSnapshot { return view },
		Self:      f.alice.ID(),
		TargetKey: func(id identity.DeviceID) (ed25519.PublicKey, error) { return f.bob.PublicKey(), nil },
	}
	paths, err := picker.Pick(context.Background(), f.bob.ID(), KindPing)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if p.Kind != PathRelay {
			continue
		}
		if p.Via == f.alice.ID() {
			t.Fatal("the picker offered the requester as its own relay")
		}
		if p.Via == f.bob.ID() {
			t.Fatal("the picker offered the target as a relay")
		}
	}
}

// TestDialViaPickerWithANilPickerIsUnchanged pins the BACKWARD
// COMPATIBILITY.
//
// With no picker, the dial must be byte-for-byte what it was before the
// path abstraction existed. This is the rule that keeps every existing
// caller safe.
func TestDialViaPickerWithANilPickerIsUnchanged(t *testing.T) {
	f := newDirectFixture(t)
	link, kind, err := dialViaPicker(context.Background(), DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.aline.ID(),
		Endpoints: nil,
	})
	if err == nil {
		t.Fatal("the dial succeeded with no endpoints")
	}
	if kind != PathDirect {
		t.Fatalf("the reported path is %q, want the direct path", kind)
	}
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("the failure is %v, want ErrNoRoute", err)
	}
	_ = link
}

// TestPickerWiringDoesNotBreakTheDirectPath is the REGRESSION GUARD for
// the picker wiring.
//
// The coordinator's finding was that the picker had no production call
// site. Wiring it in is necessary — but it is also RISKY: once
// cfg.Picker is set, dialViaPicker walks tryPaths instead of short-
// circuiting to the direct dial, so a bug in the relay path would turn
// "the direct path works" into "the whole command fails".
//
// This test therefore does the thing that matters: with a picker that
// OFFERS relay candidates, a target that is directly reachable must
// still be reached DIRECTLY, and the reported path must be the direct
// one. The relay candidates are never tried.
func TestPickerWiringDoesNotBreakTheDirectPath(t *testing.T) {
	f := newDirectFixture(t)
	l := startListener(t, f)

	// A view with a third member as a relay candidate, so the picker
	// DOES offer a relay path.
	hub, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	hubCert, err := f.acc.IssueCert(hub, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	view := &discovery.MemberSnapshot{
		AccountID: f.members.AccountID,
		Members: map[identity.DeviceID]discovery.MemberView{
			f.aline.ID(): {Endpoints: l.BoundAddrs()},
			f.bob.ID():   {},
			hub.ID():     {Cert: *hubCert, Endpoints: []string{"127.0.0.1:1"}},
		},
	}
	picker := ViewPicker{
		View:      func() *discovery.MemberSnapshot { return view },
		Self:      f.bob.ID(),
		TargetKey: func(id identity.DeviceID) (ed25519.PublicKey, error) { return f.aline.PublicKey(), nil },
	}

	link, used, err := dialViaPicker(context.Background(), DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.aline.ID(),
		Endpoints: l.BoundAddrs(),
		Kind:      KindPing,
		Picker:    picker,
	})
	if err != nil {
		t.Fatalf("the direct path failed with a picker wired: %v — wiring the picker must not "+
			"break the path that already worked", err)
	}
	defer link.Close()
	if used != PathDirect {
		t.Fatalf("the session took the %q path, want the direct one: the direct path was reachable", used)
	}

	// And the session actually works.
	stats, err := PingExchangeStats(context.Background(), link, 1, 0)
	if err != nil {
		t.Fatalf("the ping over the direct path failed: %v", err)
	}
	if stats.Received != 1 {
		t.Fatalf("the ping got %d replies, want 1", stats.Received)
	}
}

// TestTrunkQuotaRateLimitSlowsAnOverRateFlow: P-7's throughput limit.
//
// The limit is a SUSTAINED RATE, and exceeding it SLOWS a running
// transfer to the cap — it must not kill it. The forward loop used to
// tear an over-rate flow down at the first dry bucket, which made the
// cap a kill switch: a transfer faster than the cap DIED instead of
// being slowed, and the test asserted exactly that. The test's own
// comment said the opposite ("not kill the ones already running") —
// the assertion and the requirement had drifted apart, and the
// requirement is what TestTheRateQuotaThrottlesInsteadOfKilling now
// pins in the forward loop.
//
// What is asserted HERE is the shape of the throttle end to end: all
// the bytes arrive, the flow ends CLEANLY (a forwarded FIN, not a
// teardown), and the cap actually held — the transfer took at least the
// time the deficit implies.
func TestTrunkQuotaRateLimitSlowsAnOverRateFlow(t *testing.T) {
	f := newTrunkFixture(t)
	aRead, aWrite := newPipeSessionPair()
	tRead, cRead := newPipeSessionPair()

	const capPerSec = 4 << 10 // 4 KiB/s
	const records = 8
	const size = 1 << 10 // 1 KiB each: 8 KiB total, 4 s of budget at the cap

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Quota: RelayQuota{
			MaxStreams:     DefaultRelayMaxStreams,
			MaxBytesPerSec: capPerSec,
		},
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tRead, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, aRead); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	go func() {
		for i := 0; i < records; i++ {
			if err := writeRelayRecord(aWrite, relayRecord{
				Type: uint8(wire.FrameMeshRelayData),
				Data: make([]byte, size),
			}); err != nil {
				return
			}
		}
		_ = aWrite.Close()
	}()

	// Every record arrives — the cap slowed the transfer, it did not cut
	// it — and the stream ends with the requester's FIN.
	br := bufio.NewReader(cRead)
	deadline := time.Now().Add(30 * time.Second)
	open, err := readRelayRecord(br)
	if err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the target's first record is type %d, want the open record", open.Type)
	}
	for i := 0; i < records; i++ {
		_ = cRead.SetReadDeadline(deadline)
		rec, err := readRelayRecord(br)
		if err != nil {
			t.Fatalf("record %d of %d never arrived: %v — the cap killed a transfer it should have slowed", i+1, records, err)
		}
		if rec.Type == uint8(wire.FrameMeshRelayClose) {
			t.Fatalf("record %d of %d was followed by a CLOSE: the quota tore the flow down", i+1, records)
		}
	}
	_ = cRead.SetReadDeadline(deadline)
	if _, err := readRelayRecord(br); !errors.Is(err, io.EOF) {
		t.Fatalf("after all the data the target saw %v, want the forwarded FIN", err)
	}

	// The cap HELD: the first second's budget (the full bucket) covers
	// the first 4 records; the remaining 4 KiB must have taken about a
	// second. A floor with slack, because the assertion is "slower than
	// the wire", not a stopwatch.
	if elapsed := time.Since(start); elapsed < 700*time.Millisecond {
		t.Fatalf("the whole transfer took %s: the cap did not slow it at all", elapsed)
	}
}

// TestTrunkRevocationWatchTearsDownAnEstablishedRelay pins the MECHANISM
// that makes a revocation reach an established relay.
//
// The audit is explicit that the member-view PUSH can take up to a
// second, so the "撤销即时生效" metric cannot be met by the push alone: a
// re-check has to happen on a timer. This test drives that timer.
func TestTrunkRevocationWatchTearsDownAnEstablishedRelay(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			_, tgt := newPipeSessionPair()
			return tgt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := newPipeSessionPair()
	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, req); err != nil {
		t.Fatal(err)
	}
	if got := trunk.FlowCount(); got != 1 {
		t.Fatalf("the trunk carries %d flows, want 1", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The watch interval is short here so the test does not have to wait
	// a full second; the production interval is one second.
	trunk.StartRevocationWatch(ctx, 20*time.Millisecond)

	// The requester is revoked.
	if err := f.members.Remove(f.alice.ID()); err != nil {
		t.Fatal(err)
	}

	// The established relay must be torn down within the watch interval
	// (plus slack for the tick).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if trunk.FlowCount() == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the revocation watch did not tear the established relay down")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTrunkWithoutADialerRefusesWithAnActionableReason pins defect D.
//
// The anchor built its trunk WITHOUT a dialer, so the trunk's data plane
// was DEAD in that deployment: OpenFlow answered "no dialer installed"
// and every relay to a member that had not joined was refused.
//
// The trunk must either have a way to reach members (a dialer, or the
// joiners table) or refuse with a reason that says so.
func TestTrunkWithoutADialerRefusesWithAnActionableReason(t *testing.T) {
	f := newTrunkFixture(t)
	req, _ := newPipeSessionPair()
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, req)
	if err == nil {
		t.Fatal("a trunk with no way to reach members opened a flow")
	}
	if !errors.Is(err, ErrRelayUnavailable) {
		t.Fatalf("the refusal is %v, want ErrRelayUnavailable", err)
	}
	if !strings.Contains(err.Error(), "dialer") {
		t.Fatalf("the refusal %q does not say what is missing", err)
	}
}

// TestTrunkWithADialerReachesAMemberThatHasNotJoined pins defect D from
// the other side: with a dialer, the trunk reaches a member that has NOT
// joined it.
//
// That is the whole point of the dialer: a member with endpoints of its
// own does not have to join the trunk to be relayed through it.
func TestTrunkWithADialerReachesAMemberThatHasNotJoined(t *testing.T) {
	f := newTrunkFixture(t)
	req, tgt := newPipeSessionPair()
	dialed := false
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			dialed = true
			return tgt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
		Kind: KindPing,
	}, req); err != nil {
		t.Fatalf("a trunk with a dialer refused a reachable member: %v", err)
	}
	if !dialed {
		t.Fatal("the trunk did not dial the member that has not joined")
	}
}

// TestJoinRelayTrunkReturnsInsteadOfRetryingForever pins defect G.
//
// The old JoinRelayTrunk looped forever INSIDE itself, so the caller's
// "relay trunk join failed (%v); retrying" line was DEAD CODE and a
// member that could never join failed SILENTLY.
//
// One call must produce one answer: an error the caller can log.
func TestJoinRelayTrunkReturnsInsteadOfRetryingForever(t *testing.T) {
	f := newTrunkFixture(t)
	// An endpoint nothing listens on: the join must FAIL, and fail
	// promptly.
	deadline := time.Now().Add(20 * time.Second)
	err := JoinRelayTrunk(context.Background(), RelayJoinConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Trunk:     f.hub.ID(),
		Endpoints: []string{"127.0.0.1:1"},
	})
	if err == nil {
		t.Fatal("the join to a dead trunk succeeded")
	}
	if elapsed := time.Since(deadline); elapsed > 20*time.Second {
		t.Fatalf("the join took %s to fail: it must return, not retry forever", elapsed)
	}
}

// TestJoinRelayTrunkRefusesAnEmptyEndpointList pins the other half of
// defect G: the join must say what is missing rather than spin.
func TestJoinRelayTrunkRefusesAnEmptyEndpointList(t *testing.T) {
	f := newTrunkFixture(t)
	err := JoinRelayTrunk(context.Background(), RelayJoinConfig{
		Device:   f.alice,
		Resolver: MemberListAdapter{List: f.members},
		Trunk:    f.hub.ID(),
	})
	if err == nil {
		t.Fatal("the join with no endpoint succeeded")
	}
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("the refusal is %v, want ErrNoRoute", err)
	}
}
