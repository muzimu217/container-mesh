// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

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
