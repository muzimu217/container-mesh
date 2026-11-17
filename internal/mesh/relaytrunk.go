// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// The relay trunk: the mesh's own relay.
//
// The pairing relay (package internal/relay) joins two peers that share a
// passphrase. It does not know who they are and it does not know where
// anything goes — it splices two byte pipes and steps aside.
//
// The trunk is a different animal. It is a MEMBER of the mesh that
// forwards between two OTHER members, so it knows both identities and it
// has to decide whether it will. That decision is the whole of this slice:
// the control frames (relay open / relay answer) and the authorization
// state machine that answers them.
//
// There is NO data plane here. No bytes are forwarded, no session is
// opened, no quota is counted. A granted answer means "this node is
// willing to carry this relay"; the carrying itself is the next slice.
//
// The rules this state machine follows come from the trunk audit
// (/tmp/trunk-r59.md, sections 2.2 and 3.1) and the owner's ruling
// (docs/decisions.md D73): the relay side is an anchor that has a public
// IP and a switch, and the switch is off unless an operator turned it on.

// MaxRelayHops is how many relay hops a request may still need.
//
// One: the trunk relays between two members directly. Multi-hop routing
// (A→B→C→D) is not built, so a request that still needs more than one hop
// is refused with a clear reason instead of being half-served.
const MaxRelayHops = 1

// The classes a relay answer can carry.
//
// They exist so the requester knows whether to try ANOTHER relay or to
// stop. "auth" and "refused" are verdicts — the request is wrong and
// repeating it elsewhere only repeats the failure while leaking the
// attempt. "disabled" is a local condition — this node does not relay, so
// another node might.
const (
	// RelayClassGrant is the answer of a request the trunk accepted.
	RelayClassGrant = "grant"
	// RelayClassAuth is the answer of a request where the requester or
	// the target is not a member of the trunk's view.
	RelayClassAuth = "auth"
	// RelayClassRefused is the answer of a request that makes no sense as
	// a relay (from or to the trunk itself, a device to itself, a
	// multi-hop request).
	RelayClassRefused = "refused"
	// RelayClassDisabled is the answer of a request that arrived while
	// the trunk's switch was off.
	RelayClassDisabled = "disabled"
)

// RelayOpen is the decoded FrameMeshRelayOpen payload: the request to
// carry a session from one member to another.
type RelayOpen struct {
	// From is the member that wants to reach To.
	From identity.DeviceID `json:"from"`
	// To is the member that is being reached.
	To identity.DeviceID `json:"to"`
	// Hops is how many relay hops the request still needs. Zero means
	// "one hop", which is the only shape the trunk serves today.
	Hops int `json:"hops,omitempty"`
}

// RelayAnswer is the decoded FrameMeshRelayAnswer payload: the trunk's
// verdict on a request.
type RelayAnswer struct {
	// Grant says whether the trunk will carry the relay.
	Grant bool `json:"grant"`
	// Reason is a human readable explanation. It is what the requester
	// shows, so it says what happened rather than what to do about it.
	Reason string `json:"reason,omitempty"`
	// Class says what KIND of answer this is, so the requester can tell
	// a verdict from a local condition.
	Class string `json:"class,omitempty"`
}

// EncodeRelayOpen encodes the payload of a relay request.
func EncodeRelayOpen(open RelayOpen) ([]byte, error) {
	return json.Marshal(open)
}

// DecodeRelayOpen decodes the payload of a relay request.
func DecodeRelayOpen(p []byte) (RelayOpen, error) {
	var open RelayOpen
	if err := json.Unmarshal(p, &open); err != nil {
		return RelayOpen{}, fmt.Errorf("mesh: relay trunk: bad relay open payload: %w", err)
	}
	return open, nil
}

// EncodeRelayAnswer encodes the payload of a relay answer.
func EncodeRelayAnswer(ans RelayAnswer) ([]byte, error) {
	return json.Marshal(ans)
}

// DecodeRelayAnswer decodes the payload of a relay answer.
func DecodeRelayAnswer(p []byte) (RelayAnswer, error) {
	var ans RelayAnswer
	if err := json.Unmarshal(p, &ans); err != nil {
		return RelayAnswer{}, fmt.Errorf("mesh: relay trunk: bad relay answer payload: %w", err)
	}
	return ans, nil
}

// RelayOpenFrame builds the frame that asks a trunk to carry a relay.
func RelayOpenFrame(open RelayOpen) (*wire.Frame, error) {
	payload, err := EncodeRelayOpen(open)
	if err != nil {
		return nil, err
	}
	return &wire.Frame{Type: wire.FrameMeshRelayOpen, Payload: payload}, nil
}

// RelayAnswerFrame builds the frame that carries a trunk's verdict.
func RelayAnswerFrame(ans RelayAnswer) (*wire.Frame, error) {
	payload, err := EncodeRelayAnswer(ans)
	if err != nil {
		return nil, err
	}
	return &wire.Frame{Type: wire.FrameMeshRelayAnswer, Payload: payload}, nil
}

// RelayTrunkConfig configures the relay side of the trunk.
type RelayTrunkConfig struct {
	// Self is this member's own device ID. The trunk refuses to relay on
	// its own behalf or to itself: a node that can reach another member
	// directly does not need the trunk, and a relay to itself is a loop.
	Self identity.DeviceID
	// Members is the member view the trunk decides against. It is the
	// trust anchor: the same view the direct path pins certificates
	// against. It must be the LIVE view, so that a revoked device is
	// refused on its next request.
	Members MemberResolver
	// Enabled turns the trunk on. It is off by default: a node that relays
	// for the whole network without being asked is a public relay.
	Enabled bool
}

// trunkState is the trunk's own state.
type trunkState uint8

const (
	// trunkOff is the default: the node does not relay.
	trunkOff trunkState = iota
	// trunkOn: the node carries relays between members of its view.
	trunkOn
)

// RelayTrunk is the relay side of the mesh's relay: the state machine
// that decides whether this node carries a relay for somebody else.
//
// It is safe for concurrent use.
type RelayTrunk struct {
	self    identity.DeviceID
	members MemberResolver

	mu    sync.RWMutex
	state trunkState
}

// NewRelayTrunk builds a trunk.
//
// The member resolver is required: without a view there is no membership
// question to ask, and a trunk that cannot ask it would relay for anyone.
// The trunk's own identity is required for the same reason — the guards
// that refuse a relay to or from the trunk itself need to know which
// device that is.
func NewRelayTrunk(cfg RelayTrunkConfig) (*RelayTrunk, error) {
	if cfg.Members == nil {
		return nil, errors.New("mesh: relay trunk needs a member resolver")
	}
	if cfg.Self == "" {
		return nil, errors.New("mesh: relay trunk needs its own device id")
	}
	t := &RelayTrunk{self: cfg.Self, members: cfg.Members, state: trunkOff}
	if cfg.Enabled {
		t.state = trunkOn
	}
	return t, nil
}

// Enabled reports whether the trunk is willing to carry relays.
func (t *RelayTrunk) Enabled() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state == trunkOn
}

// SetEnabled turns the trunk on or off.
//
// This is the operator's control switch (D73). It is off until it is
// turned on, and turning it off stops new relays from being accepted.
func (t *RelayTrunk) SetEnabled(on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if on {
		t.state = trunkOn
		return
	}
	t.state = trunkOff
}

// Decide evaluates one relay request.
//
// It returns the answer to send back AND the local verdict. The answer
// carries the class so the requester knows whether to try another relay;
// the error carries the matching sentinel so the local caller can branch
// on it with errors.Is.
//
// The order of the checks is the security property, not a style choice:
//
//  1. The REQUESTER must be in the member view. A device that is not in
//     it — revoked, or never a member — is refused here, and the target is
//     not even looked up. The membership check comes before everything
//     else: that is the lesson of the defect the previous round caught
//     (a revoked device's identity reaching a third party).
//  2. The TARGET must be in the member view. Same reason: a request must
//     not make the trunk work for a pair that is not a pair of members.
//  3. The trunk's switch must be on. This is the anchor's policy, and it
//     is off by default.
//  4. The request must make sense as a relay: not from the trunk, not to
//     the trunk, not from a device to itself, and not a multi-hop request.
//
// The membership verdicts come BEFORE the switch on purpose: the answer
// about a requester must not depend on the local policy, so a revoked
// device gets the same answer whatever the switch says and cannot use the
// difference to learn anything about the trunk.
func (t *RelayTrunk) Decide(open RelayOpen) (RelayAnswer, error) {
	if _, err := t.members.KeyFor(open.From); err != nil {
		return denyRelay(RelayClassAuth, ErrAuth,
			"mesh: relay trunk: the requester %s is not a member of this view", open.From)
	}
	if _, err := t.members.KeyFor(open.To); err != nil {
		return denyRelay(RelayClassAuth, ErrAuth,
			"mesh: relay trunk: the target %s is not a member of this view", open.To)
	}
	if !t.Enabled() {
		return denyRelay(RelayClassDisabled, ErrRelayTrunkDisabled,
			"mesh: relay trunk: this node does not relay (the trunk switch is off)")
	}
	if open.From == open.To {
		return denyRelay(RelayClassRefused, ErrRefused,
			"mesh: relay trunk: a device cannot relay to itself")
	}
	if open.From == t.self {
		return denyRelay(RelayClassRefused, ErrRefused,
			"mesh: relay trunk: the trunk does not relay on its own behalf")
	}
	if open.To == t.self {
		return denyRelay(RelayClassRefused, ErrRefused,
			"mesh: relay trunk: the trunk does not relay to itself")
	}
	hops := open.Hops
	if hops <= 0 {
		// A request that states no budget is a one-hop request.
		hops = 1
	}
	if hops > MaxRelayHops {
		return denyRelay(RelayClassRefused, ErrRefused,
			"mesh: relay trunk: multi-hop relay is not implemented (the request needs %d hops, the trunk carries one)",
			hops)
	}
	return RelayAnswer{Grant: true, Class: RelayClassGrant}, nil
}

// AnswerFrame answers one relay request frame.
//
// The answer frame is returned even when the verdict is a refusal: the
// requester has to learn WHY it was refused. The returned error is the
// local classification (ErrAuth, ErrRefused, ErrRelayTrunkDisabled) — it
// is for the log, not for the peer.
func (t *RelayTrunk) AnswerFrame(f *wire.Frame) (*wire.Frame, error) {
	if f == nil {
		return nil, errors.New("mesh: relay trunk: no frame to answer")
	}
	if f.Type != wire.FrameMeshRelayOpen {
		return nil, fmt.Errorf("mesh: relay trunk: expected a %s frame, got %s",
			wire.FrameMeshRelayOpen, f.Type)
	}
	open, err := DecodeRelayOpen(f.Payload)
	if err != nil {
		return nil, err
	}
	ans, verdict := t.Decide(open)
	out, err := RelayAnswerFrame(ans)
	if err != nil {
		return nil, err
	}
	return out, verdict
}

// denyRelay builds a refusal: the answer the requester receives and the
// local verdict, carrying the same class.
func denyRelay(class string, sentinel error, format string, args ...any) (RelayAnswer, error) {
	reason := fmt.Sprintf(format, args...)
	return RelayAnswer{Grant: false, Reason: reason, Class: class},
		fmt.Errorf("%w: %s", sentinel, reason)
}

// The installed trunk.
//
// The listener that receives a relay request lives in another package
// (internal/cli builds it), and the listener's configuration belongs to
// the direct path — a different concern that must not grow a trunk
// pointer. So the node's trunk is installed once, here, and the data
// plane reads it back.
//
// There is at most one trunk per process: the trunk is a property of the
// NODE (the anchor), not of a session.
var (
	installedMu sync.RWMutex
	installed   *RelayTrunk
)

// SetRelayTrunk installs the process's relay trunk. Passing nil removes it.
func SetRelayTrunk(t *RelayTrunk) {
	installedMu.Lock()
	defer installedMu.Unlock()
	installed = t
}

// InstalledRelayTrunk returns the installed relay trunk, or nil when the
// node does not run one.
func InstalledRelayTrunk() *RelayTrunk {
	installedMu.RLock()
	defer installedMu.RUnlock()
	return installed
}
