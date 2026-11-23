// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

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
	//
	// It is a VERDICT: the request itself is wrong, so presenting the
	// same credentials to another relay would be wrong too.
	RelayClassRefused = "refused"
	// RelayClassUnavailable is the answer of a request this trunk cannot
	// carry RIGHT NOW — the target has not joined it, or the target's
	// connection to it is down.
	//
	// It is its OWN class because it is a LOCAL condition, not a verdict:
	// the requester IS a member and the request WAS well formed — this
	// particular trunk simply cannot reach that target. Another relay may
	// well reach it, so the requester must be allowed to try it.
	//
	// Before this class existed, the trunk answered "cannot reach the
	// target" with RelayClassRefused, which the path layer treats as a
	// verdict and therefore STOPS on. The consequence was that the
	// fallback chain worked exactly one hop: the second relay was never
	// tried, which contradicts D73 ("中继只兜底").
	RelayClassUnavailable = "unavailable"
	// RelayClassDisabled is the answer of a request that arrived while
	// the trunk's switch was off.
	RelayClassDisabled = "disabled"
	// RelayClassQuota is the answer of a request that arrived while the
	// trunk's quota was full.
	//
	// It is its OWN class because it is a LOCAL condition, not a verdict:
	// the requester IS a member and the request WAS well formed — this
	// trunk is simply carrying as much as it may. Another relay may well
	// say yes, so the requester must be allowed to try it.
	RelayClassQuota = "quota"
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
	// Kind is the session kind the target must serve: "ping", "dial",
	// "msg", "file", … It is the SAME kind the requester would have used
	// on a direct link, so the target's handler is the one it always
	// runs.
	//
	// It travels in the request because the trunk opens the session to
	// the TARGET, and the target's listener dispatches on the kind in
	// its hello. Without it the trunk would have to guess.
	Kind string `json:"kind,omitempty"`
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
	// Quota bounds what this node carries for others (P-7). Zero means
	// the defaults.
	Quota RelayQuota
	// Dial opens a session to a member. It is the trunk's ability to
	// REACH the target: the anchor dials the target over the member
	// view's endpoints.
	//
	// It is a seam, not a convenience: the trunk's authorization decisions
	// must be testable without a network, and a test that has to stand up
	// three QUIC listeners to prove "a revoked device's stream is torn
	// down" is a test that cannot be written.
	//
	// The session it returns must be a DEDICATED one for this flow: the
	// trunk writes the relay's OPEN record onto it and splices the
	// requester's stream against it. A session that is shared with other
	// traffic would mix two relays' records together.
	Dial func(ctx context.Context, target identity.DeviceID, kind string) (Session, error)
	// Joiners is the table of members that have JOINED this trunk.
	//
	// A joined member is reachable over the connection it opened towards
	// the trunk — which is the ONLY way a pure-outbound member (no
	// inbound port at all) can be reached. When it is set, the trunk
	// prefers it over Dial.
	Joiners *JoinerTable
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
	dial    func(ctx context.Context, target identity.DeviceID, kind string) (Session, error)
	joiners *JoinerTable
	quota   RelayQuota

	mu    sync.RWMutex
	state trunkState
	// stopping says the trunk is shutting down. Once it is set, no NEW
	// relay is admitted: a connection that arrives in the window between
	// the stop and the listener's close would otherwise be adopted, and
	// the trunk would have no way to close it — because a hijacked
	// socket is no longer in net/http's view.
	//
	// This is the same hole the stage-1 relay had (internal/relay), and
	// the same fix: refuse at the top of the handler.
	stopping bool

	// flows are the live relays this trunk carries. Guarded by mu.
	flows map[uint64]*relayFlow
	// reserved counts the stream slots taken, including the ones being
	// set up. It is the quota's live counter. Guarded by mu.
	reserved int
	// bucketTokens is the node's rate budget that has not been spent yet,
	// and bucketTime is when it was last refilled.
	//
	// The budget belongs to the node, not to a flow: P-7 caps the
	// throughput of a NODE ("单节点被持续转发时，吞吐 ≤ 50 MiB/s"). A
	// per-flow budget would let twenty flows each carry the full cap, so
	// the node would forward twenty times what the requirement allows.
	// Guarded by mu.
	//
	// It is a TOKEN BUCKET, not a fixed one-second window, and the
	// difference is not cosmetic. The fixed window counted everything a
	// transfer pushed into its first second, so a file larger than the
	// cap could NEVER be relayed — the rate limit had become a FILE SIZE
	// limit. Measured on the real machine: 49 MiB went through, 50 MiB
	// failed, every time, and raising the cap made 64 MiB succeed.
	//
	// The bucket refills continuously, so the cap is a rate: a
	// hundred-megabyte transfer takes two seconds and is allowed,
	// while a node asked to forward a hundred megabytes in one second is
	// not.
	bucketTokens float64
	bucketTime   time.Time
	// nextFlow hands out relay session ids. Guarded by mu.
	nextFlow uint64
	// revoker is called when a live flow must be torn down because one
	// of its two ends stopped being a member. It is set by the owner of
	// the trunk's runtime; nil means "no runtime installed".
	revoker func(uint64)
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
	t := &RelayTrunk{
		self:    cfg.Self,
		members: cfg.Members,
		dial:    cfg.Dial,
		joiners: cfg.Joiners,
		quota:   cfg.Quota.withDefaults(),
		state:   trunkOff,
		flows:   make(map[uint64]*relayFlow),
	}
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
		// The reason goes with the refusal: "not a member" and
		// "certificate lapsed" are the same verdict but they are not the
		// same PROBLEM, and only the second one has a remedy.
		return denyRelay(RelayClassAuth, ErrAuth,
			"mesh: relay trunk: the requester %s is not a member of this view: %v", open.From, err)
	}
	if _, err := t.members.KeyFor(open.To); err != nil {
		return denyRelay(RelayClassAuth, ErrAuth,
			"mesh: relay trunk: the target %s is not a member of this view: %v", open.To, err)
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

// ---------------------------------------------------------------------------
// The relay PROTOCOL.
//
// The trunk is a member that forwards between two OTHER members. It has to
// speak to both sides, and both sides have to speak to it. This section
// defines that conversation.
//
// THE WIRE SEQUENCE (one relay session, A → B → C, B is the trunk):
//
//   A                                        B (trunk)                     C
//   |------ FrameMeshRelayOpen ----------->|                               |
//   |                                        |--- dial C, open a stream -->|
//   |                                        |<-- FrameMeshRelayHello -----|
//   |                                        |   (C's E2E hello, signed)   |
//   |<-- FrameMeshRelayHello (C's) ---------|                               |
//   |   (A verifies C's signature)          |                               |
//   |------ FrameMeshRelayHello (A's) ---->|--- forward verbatim -------->|
//   |                                        |<-- FrameMeshRelayData ------|
//   |<-- FrameMeshRelayData (sealed) --------|                               |
//   |   (A opens it, gets plaintext)        |                               |
//
// WHAT THE TRUNK FORWARDS: the FrameMeshRelayHello and the
// FrameMeshRelayData frames, VERBATIM. It never parses the payload of a
// data frame, never opens it, and never holds a key for it.
//
// WHY THE HELLO IS FORWARDED VERBATIM RATHER THAN TERMINATED: the hello is
// the end-to-end handshake, and it is between A and C. If the trunk
// terminated it, the trunk would choose the session key — and then it
// would know the plaintext. So the trunk is a byte pipe for the handshake
// too. That is why the hello is SIGNED: a pipe that does not understand
// the content can still be prevented from tampering with it, because A
// verifies C's signature and vice versa.
//
// The relay frames are LENGTH-PREFIXED inside the session (the same
// framing the rest of the mesh uses), NOT the wire.Frame header: the
// relay session is one QUIC stream, and the frames ride it as records.
// ---------------------------------------------------------------------------

// relayProtoID identifies the relay protocol on the wire.
const relayProtoID = "cmesh-relay/1"

// maxRelayRecord bounds one relay record (the envelope plus its opaque
// payload). It matches the wire package's 1 MiB frame cap, so a relayed
// record can never be bigger than a direct one.
//
// It bounds BOTH formats: the binary frame (a 24-byte header, a meta
// block and the payload) and the JSON envelope (base64, so a third
// bigger). See maxRelayPlaintext.
const maxRelayRecord = 1 << 20

// maxRelayPlaintext is the largest plaintext that fits in ONE relay
// record.
//
// THE SWITCH CHANGED THIS NUMBER, and it changed it upwards.
//
// A relay record used to be a JSON envelope, and JSON carries a []byte as
// base64: four bytes of wire for every three bytes of payload. The
// end-to-end seal adds a 12-byte nonce and a 16-byte tag, so the
// plaintext that fit was roughly three quarters of the record limit.
//
// The binary frame carries the payload VERBATIM, so the SAME 1 MiB record
// budget holds a third more: the cap is now the record limit less the
// 24-byte header and the seal's overhead. Measured on the real host, the
// binary record holds 1,047,552 payload bytes where the JSON one held
// 785,408 (+33.38%).
//
// It matters because the file sender writes 1 MiB chunks (fileChunkSize),
// and 1 MiB did NOT fit the old cap: the record came out at 1398158 bytes
// against a 1048576-byte limit, so every file bigger than about a
// megabyte failed through the relay with "record too large" — while the
// same file went through the direct link without complaint.
//
// The margin below is deliberately generous: a record that is one byte
// over the limit is a hard failure, and the exact size depends on the
// seal's overhead.
const maxRelayPlaintext = maxRelayRecord - relayFrameHeaderSize - e2eSealOverhead - 64

// maxRelayPlaintextJSON is the largest plaintext that fits in ONE JSON
// relay record.
//
// It is the OLD cap, and it is still the cap of the JSON path — which is
// the fallback for a mixed-version deployment. A JSON record is bigger
// than its payload by a third, so the binary cap would overflow it and
// the record limit would refuse it.
const maxRelayPlaintextJSON = maxRelayRecord*3/4 - 1024

// e2eSealOverhead is what the end-to-end seal adds to a plaintext: the
// 12-byte nonce in front of the ciphertext and the 16-byte GCM tag
// behind it.
const e2eSealOverhead = 12 + 16

// maxRelayPlaintextFor is the largest plaintext that fits in ONE relay
// record of the given format.
//
// It is the format's own cap: the binary frame carries its payload
// verbatim, the JSON envelope carries it as base64, so the same record
// budget holds a third more of one than the other.
func maxRelayPlaintextFor(format RelaySpliceFormat) int {
	if format == RelaySpliceFormatJSON {
		return maxRelayPlaintextJSON
	}
	return maxRelayPlaintext
}

// maxRelayPayloadForLegs is the largest plaintext that survives the
// WHOLE path of a relay.
//
// A relay is two legs: the writer's and the reader's. A record written
// on the first leg is READ on the second one, and on a mixed-version
// relay the anchor has to TRANSLATE it — which changes its size, because
// the JSON envelope is a third bigger than the binary frame (base64).
//
// So the record has to fit on BOTH legs:
//
//   - the leg the writer writes on, in that leg's format;
//   - the leg the reader reads on, in THAT leg's format — because the
//     anchor re-encodes the record for it.
//
// The cap is the smaller of the two. On a same-version relay both legs
// speak the same format and the cap is that format's cap. On a mixed
// one, it is the JSON cap — the smaller of the two — because a JSON leg
// is somewhere on the path.
//
// WHY THE SMALLER CAP AND NOT A SPLIT. The anchor cannot split an
// oversized record: the end-to-end seal puts the GCM nonce at the front
// of the ciphertext and the authentication tag at the back, covering
// the whole plaintext, so half a sealed blob does not authenticate. The
// WRITER has to write less, and the writer cannot know how much less —
// the far leg's format is not its business. So the anchor, which knows
// the whole path, computes this and tells the writer (see
// relayRecord.MaxPayload).
func maxRelayPayloadForLegs(writerFormat, readerFormat RelaySpliceFormat) uint64 {
	a := maxRelayPlaintextFor(writerFormat)
	b := maxRelayPlaintextFor(readerFormat)
	if b < a {
		a = b
	}
	return uint64(a)
}

// sealAndWriteRelayData seals the plaintext and writes it as relay
// records, SPLITTING it when it does not fit in one.
//
// The split is safe. The end-to-end seal is counter-based — a fresh
// nonce per message — so every piece is sealed and opened on its own,
// and the business layer above reads a STREAM: it sees the same bytes in
// the same order, only in more pieces.
//
// THE FORMAT IS THE BINARY ONE, and that is the switch's endpoint half.
//
// The requester endpoint is one of the THREE processes the switch has to
// land on: the anchor's per-record cost was measured at 1140 µs, the
// target's at 1345 µs and the REQUESTER's at 1778 µs — co-equal, and the
// requester the most expensive of the three. A trunk that spoke the
// binary format while the requester spoke JSON would have to decode and
// re-encode every record anyway, so the anchor's win would be 1.85×
// instead of the measured 96%.
//
// It returns how many bytes were written.
func sealAndWriteRelayData(w io.Writer, e2e *E2ESession, p []byte) (int, error) {
	return sealAndWriteRelayDataAs(w, e2e, p, RelaySpliceFormatBinary, 0)
}

// sealAndWriteRelayDataAs seals the plaintext and writes it as relay
// records in the given format.
//
// The format parameter exists for the ONE case where the requester has to
// speak the old format: a requester that reached an OLD anchor (the
// anchor negotiated container-mesh-relay/1, so it speaks JSON). The
// requester learns the format from the ALPN it negotiated, and the
// anchor's answer says which one it is — so a requester on an old anchor
// keeps working after the switch.
//
// The maxPayload parameter is the cap the ANCHOR stated in its answer
// (or in its open record to a target). Zero means "no cap was stated",
// and the writer then uses its own format's cap.
//
// WHY THE ANCHOR'S CAP AND NOT JUST THE WRITER'S OWN. On a
// mixed-version relay the anchor TRANSLATES the record, and the JSON
// envelope is a third bigger than the binary frame — so a record the
// writer was allowed to write on its own leg does not fit on the far
// one. The anchor is the only node that knows both legs, so it states
// the cap that survives the whole path. See
// maxRelayPayloadForLegs.
func sealAndWriteRelayDataAs(w io.Writer, e2e *E2ESession, p []byte, format RelaySpliceFormat, maxPayload uint64) (int, error) {
	total := len(p)
	// The cap depends on the format: a JSON record is a third bigger
	// than its payload (base64), so the JSON cap is the OLD one. And the
	// anchor's cap, when it stated one, is SMALLER than either — it is
	// the cap that survives the far leg as well.
	limit := maxRelayPlaintextFor(format)
	if maxPayload != 0 && int(maxPayload) < limit {
		limit = int(maxPayload)
	}
	for len(p) > 0 {
		n := len(p)
		if n > limit {
			n = limit
		}
		sealed, err := e2e.Seal(p[:n])
		if err != nil {
			return total - len(p), err
		}
		if err := writeRelayRecordAs(w, relayRecord{
			Type: uint8(wire.FrameMeshRelayData),
			Data: sealed,
		}, format); err != nil {
			return total - len(p), err
		}
		p = p[n:]
	}
	return total, nil
}

// relayHandshakeTimeout bounds the end-to-end handshake that happens
// THROUGH the trunk.
//
// It exists because a handshake has no natural end of its own: the
// requester waits for the target's hello, and the target only produces
// it when the trunk has routed the session to it. If any of those steps
// stalls, the requester would block in a socket read forever — the
// caller's context does not interrupt a blocking read. A bounded
// handshake turns a stall into an error the caller can report.
const relayHandshakeTimeout = 30 * time.Second

// relayRecord is the envelope of one relay protocol message.
//
// The DATA field is OPAQUE to the trunk: it is the end-to-end ciphertext.
// The trunk moves it and counts it; it does not read it.
type relayRecord struct {
	// Type is the frame type: FrameMeshRelayOpen, FrameMeshRelayHello,
	// FrameMeshRelayData, FrameMeshRelayClose, FrameMeshRelayJoin.
	Type uint8 `json:"t"`
	// Session is the relay session id. Zero for the open record (the
	// session id does not exist yet) and for the join record.
	Session uint64 `json:"s,omitempty"`
	// From and To name the two ends of the relay. They are set on the
	// open record only.
	From identity.DeviceID `json:"from,omitempty"`
	To   identity.DeviceID `json:"to,omitempty"`
	// Kind is the session kind the target must serve.
	Kind string `json:"kind,omitempty"`
	// Hops is the hop budget.
	Hops int `json:"hops,omitempty"`
	// Hello is the end-to-end handshake message (a signed JSON blob).
	Hello []byte `json:"hello,omitempty"`
	// Data is the opaque payload — the end-to-end ciphertext.
	Data []byte `json:"data,omitempty"`
	// Close marks the end of the relay.
	Close bool `json:"close,omitempty"`
	// Reason explains a close or a refusal.
	Reason string `json:"reason,omitempty"`
	// Class is the answer's class, on a refusal.
	Class string `json:"class,omitempty"`
	// Grant says whether a request was granted.
	Grant bool `json:"grant,omitempty"`
	// MaxPayload is the largest PLAINTEXT the writer of this relay may
	// put in ONE record.
	//
	// It is zero when the record says nothing about it, and the writer
	// then uses its own leg's default. It is NOT zero on the records the
	// anchor writes — the ANSWER to a requester and the OPEN to a target
	// — because the anchor is the only node that knows the WHOLE path.
	//
	// WHY THE WRITER CANNOT KNOW IT ALONE. The binary frame carries its
	// payload verbatim, so the same 1 MiB record budget holds a third
	// more than the JSON envelope did: the cap rose from 786 KiB to
	// about 1 MiB. But on a MIXED-VERSION relay the anchor TRANSLATES,
	// and the JSON envelope is a third BIGGER (base64) — so a record at
	// the binary cap becomes 1,397,966 bytes, over the 1,048,576-byte
	// record cap.
	//
	// The anchor cannot fix that by splitting the record: the end-to-end
	// seal puts the GCM nonce at the front of the ciphertext and the
	// authentication tag at the back, covering the whole plaintext, so
	// half a sealed blob does not authenticate. The WRITER has to write
	// less, and the writer cannot know how much less — the far leg's
	// format is not its business.
	//
	// So the anchor computes the cap that survives the whole path and
	// states it here. See maxRelayPayloadForLegs.
	MaxPayload uint64 `json:"maxp,omitempty"`
}

// writeRelayRecord writes one relay record in the DEFAULT format.
//
// The default is the BINARY frame, and that is the switch.
//
// The relay record used to be a JSON envelope, so the anchor had to
// decode every record it forwarded (json.Unmarshal + base64 decode) and
// re-encode it (json.Marshal + base64 encode) — 36.27% of the anchor's
// CPU in the measured profile (base64 21.69% of it), and 27.76% of the
// target endpoint's CPU too.
//
// The binary frame puts the type, the meta length and the payload length
// in a FIXED 24-byte header, so the anchor can learn everything it needs
// from 24 bytes and then copy the rest. The measured relay throughput
// went from 33.3–36.5 MiB/s (the JSON product) to 65.9–67.8 MiB/s (the
// binary frame, spliced) — +96%.
//
// The JSON path is still there, behind writeRelayRecordAs, because a
// mixed-version deployment needs it.
func writeRelayRecord(w io.Writer, rec relayRecord) error {
	return writeRelayRecordAs(w, rec, RelaySpliceFormatBinary)
}

// writeRelayRecordAs writes one relay record in the given format.
//
// The format is a property of the LEG it travels on, and the leg's format
// comes from the ALPN that leg's connection negotiated.
func writeRelayRecordAs(w io.Writer, rec relayRecord, format RelaySpliceFormat) error {
	var body []byte
	switch format {
	case RelaySpliceFormatBinary:
		frame, err := relayFrameFromRecord(rec)
		if err != nil {
			return err
		}
		body, err = EncodeBinaryRelayFrame(frame)
		if err != nil {
			return err
		}
	case RelaySpliceFormatJSON:
		var err error
		body, err = json.Marshal(rec)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("mesh: relay: unknown record format (%d)", uint8(format))
	}
	if len(body) > maxRelayRecord {
		return fmt.Errorf("mesh: relay: record too large (%d bytes)", len(body))
	}
	return writeFrame(w, body)
}

// relayRecordReader yields relay records.
//
// It exists because the records do not always come off a byte stream: on
// the target's side they are ROUTED to the right session by the member's
// join loop, so a session handler reads them from a channel, not from the
// socket.
type relayRecordReader interface {
	// nextRecord returns the next record, or an error when the stream
	// is over.
	nextRecord() (relayRecord, error)
}

// framedRecordReader reads the records off a byte stream.
//
// The framing is length-prefixed (the same framing the rest of the mesh
// uses), so a record boundary is unambiguous. The BODY is the binary
// relay frame by default; the JSON envelope is still readable through
// the format field, and that is the mixed-version fallback.
//
// The reader is readMsgFrame, NOT plain readFrame — and the difference is
// data integrity, not style. io.ReadFull returns io.EOF for "nothing was
// read", so a record whose header promised bytes and whose body never
// arrived read as io.EOF, which every caller above takes for "the sender
// is done sending". A truncated record was therefore forwarded as a
// GRACEFUL end: the tail was dropped silently and the far side was told
// the message was complete. readMsgFrame reads the header and the body
// separately and reports cleanEnd ONLY for an EOF at a record boundary
// (the same lesson the message path learned first).
type framedRecordReader struct {
	br *bufio.Reader
	// format is the record format of this leg. The zero value means
	// the DEFAULT (the binary frame) — a zero value that meant "JSON"
	// would silently keep the old wire format alive in every caller
	// that did not set it.
	format RelaySpliceFormat
	// binarySet says the format was chosen EXPLICITLY.
	//
	// It exists because the binary format is the zero value of
	// RelaySpliceFormat (it is the first constant), so "not set" and
	// "set to binary" would be the same value — and a leg that must
	// speak JSON (a requester that reached an old anchor) could not be
	// told so.
	binarySet bool
}

// newFramedRecordReader builds a reader for a leg in the given format.
func newFramedRecordReader(br *bufio.Reader, format RelaySpliceFormat) framedRecordReader {
	return framedRecordReader{br: br, format: format, binarySet: true}
}

// legFormat reports the record format of this leg.
//
// It is the DEFAULT (binary) unless the format was chosen explicitly.
func (f framedRecordReader) legFormat() RelaySpliceFormat {
	if !f.binarySet {
		return RelaySpliceFormatBinary
	}
	return f.format
}

// nextRecord reads one record off the stream.
//
// A clean end comes back as io.EOF (what every caller checks for);
// anything else is a broken stream and must TEAR the flow down, never
// masquerade as a finish.
func (f framedRecordReader) nextRecord() (relayRecord, error) {
	return readRelayRecordAs(f.br, f.legFormat())
}

// readRelayRecord reads one relay record off a byte stream, in the
// DEFAULT format.
func readRelayRecord(br *bufio.Reader) (relayRecord, error) {
	return readRelayRecordAs(br, RelaySpliceFormatBinary)
}

// readRelayRecordAs reads one relay record off a byte stream in the
// given format.
//
// A record in the WRONG format is a loud failure, never a misreading:
// the binary frame's tag (0xC7) is not a byte any JSON parser can start
// a document with, and a JSON envelope starts with '{', which the binary
// decoder refuses. So a member and an anchor that disagree about the
// format break the relay at the first record, with a message that says
// which format was expected.
func readRelayRecordAs(br *bufio.Reader, format RelaySpliceFormat) (relayRecord, error) {
	body, cleanEnd, err := readMsgFrame(br, maxRelayRecord)
	if err != nil {
		return relayRecord{}, err
	}
	if cleanEnd {
		return relayRecord{}, io.EOF
	}
	switch format {
	case RelaySpliceFormatBinary:
		frame, err := DecodeBinaryRelayFrame(body)
		if err != nil {
			return relayRecord{}, fmt.Errorf("mesh: relay: bad record: %w", err)
		}
		return recordFromRelayFrame(frame), nil
	case RelaySpliceFormatJSON:
		var rec relayRecord
		if err := json.Unmarshal(body, &rec); err != nil {
			return relayRecord{}, fmt.Errorf("mesh: relay: bad record: %w", err)
		}
		return rec, nil
	default:
		return relayRecord{}, fmt.Errorf("mesh: relay: unknown record format (%d)", uint8(format))
	}
}

// ---------------------------------------------------------------------------
// The DATA plane.
//
// The control plane above decides WHETHER this node will carry a relay.
// The data plane below does the carrying: it opens a session to the target,
// splices the two byte streams, and counts what it moves.
//
// THE THREE PROPERTIES THIS SECTION HAS TO GET RIGHT:
//
//  1. Authorization is RE-CHECKED before the bytes move. The control plane
//     checked the membership when the request arrived; by the time the
//     trunk dials the target, the answer may be stale. A device revoked
//     in between must NOT get its traffic carried — and, worse, must not
//     get the TARGET's identity leaked to it. So the forwarding path
//     re-asks the member view for BOTH ends, in the same order the
//     control plane used (requester first, so a revoked requester's
//     request never causes a lookup of the target).
//
//  2. The trunk sees only CIPHERTEXT. The end-to-end layer (see
//     session.go) lives ABOVE the transport, so the bytes the trunk
//     forwards are already sealed for the two endpoints. The trunk's
//     splice is a byte pipe, exactly like the stage-1 pairing relay —
//     with one difference: here it knows WHO it is carrying for.
//
//  3. The quota is counted on the bytes the trunk ACTUALLY FORWARDS, not
//     on the connection's total traffic. A trunk that is also busy with
//     its own business must not have its own traffic counted against
//     the relay budget — that would let the trunk lock ITSELF out.
// ---------------------------------------------------------------------------

// The P-7 quota (docs/REQUIREMENTS.md:241): "单节点被持续转发时，吞吐
// ≤ 50 MiB/s、流数 ≤ 20，且自身业务不受影响".
//
// The two dimensions are SEPARATE because they protect different things:
// the stream count protects the trunk's file-descriptor and memory budget,
// the byte rate protects its bandwidth.
const (
	// DefaultRelayMaxStreams is the P-7 concurrent-relay limit.
	DefaultRelayMaxStreams = 20
	// DefaultRelayBytesPerSec is the P-7 forwarding-rate limit, in bytes
	// per second. 50 MiB/s.
	DefaultRelayBytesPerSec = 50 << 20
	// relayTeardownGrace is how long the trunk waits for the SECOND
	// direction of a relay to finish once the first one has ended.
	//
	// It exists because a half-close is legitimate: a request/response
	// protocol sends its request, half-closes, and then waits for the
	// answer. So the first direction ending does NOT mean the relay is
	// over — and the other direction must be given time to deliver that
	// answer.
	//
	// But the other direction can also be stuck: a target that accepted
	// the connection and then said nothing. Then the wait would be
	// forever, and the relay would hold a stream slot (the trunk's
	// quota) for a relay nobody is using.
	//
	// So the wait is BOUNDED. Ten seconds is generous for an answer
	// (the transport's own idle timeout is ten seconds too) and short
	// enough that a stuck relay does not hold a quota slot for long.
	relayTeardownGrace = 10 * time.Second
)

// RelayQuota bounds what one node carries for others.
//
// The zero value means "the defaults": a trunk that is constructed without
// a quota still has one, because an unbounded relay is exactly the public
// relay the switch is meant to prevent.
type RelayQuota struct {
	// MaxStreams is how many relays may be live at once. The 21st is
	// refused with ErrQuota; the 20 already running are NOT killed (a
	// 2 GB transfer in progress must not be thrown away).
	MaxStreams int
	// MaxBytesPerSec is the forwarding rate limit.
	MaxBytesPerSec int
}

// withDefaults fills in the defaults for the unset fields.
func (q RelayQuota) withDefaults() RelayQuota {
	if q.MaxStreams <= 0 {
		q.MaxStreams = DefaultRelayMaxStreams
	}
	if q.MaxBytesPerSec <= 0 {
		q.MaxBytesPerSec = DefaultRelayBytesPerSec
	}
	return q
}

// relayFlow is one live relay: the requester's stream and the target's
// stream, spliced together.
//
// The two sides are NOT symmetric in ownership. The requester's side is a
// session the flow OWNS: it is the QUIC stream the requester opened on
// its connection to the trunk, and closing the flow closes it. The
// target's side is a DEDICATED stream opened towards the target for this
// flow alone — never the target's long-lived join connection. That is
// what makes several relays to the same target possible at once, and what
// stops one relay's end from tearing the target's join down (defect E of
// the round-61 real-machine report: the old code closed the join
// connection, so the target could receive exactly ONE relay, ever).
type relayFlow struct {
	id   uint64
	from identity.DeviceID
	to   identity.DeviceID

	// req is the session to the requester (A), tgt the session to the
	// target (C). Both are owned by the flow.
	req Session
	tgt Session
	// reqBr and tgtBr are the BUFFERED READERS over req and tgt.
	//
	// They are carried in the flow on purpose: a bufio.Reader may have
	// read AHEAD of the record the caller consumed, and those bytes live
	// only in that reader's buffer. The splice must therefore use the
	// SAME reader, or the next record would be silently lost.
	reqBr *bufio.Reader
	tgtBr *bufio.Reader
	// reqFormat and tgtFormat are the record formats of the two legs.
	//
	// THE FORMAT IS PER LEG, and it comes from the ALPN that leg's
	// connection negotiated. The record format is an A↔C protocol — the
	// anchor is a pipe — so the two legs MUST agree for the verbatim
	// splice to be legal: if the requester writes binary frames and the
	// target expects JSON, copying the bytes hands the target garbage.
	//
	// So the two formats are carried on the flow, and the forwarding path
	// branches on whether they are EQUAL:
	//
	//	equal   → the FAST PATH: read the 24-byte header, copy the bytes.
	//	unequal → the FALLBACK: decode the record and re-encode it in
	//	          the far leg's format. That is the slow path, and it is
	//	          what a mixed-version deployment runs on.
	//
	// The fallback is a TRANSLATION, not a refusal: a mixed-version
	// deployment must keep working, because the window between the
	// anchor's upgrade and the last target's upgrade is exactly as long
	// as the operator's rollout. See
	// TestAMixedVersionRelayIsCarriedByTranslation.
	reqFormat RelaySpliceFormat
	tgtFormat RelaySpliceFormat
	// total is the lifetime byte count, for the trunk's statistics.
	total int64
	// done is closed when the flow ends, and doneOnce guards it.
	//
	// The trunk's serving path WAITS on it: the requester's connection
	// must stay open for as long as the flow carries bytes. Returning
	// from the serving function while the flow was still alive closed
	// the requester's QUIC connection under it, and the requester never
	// even got to read the grant (defect A of the round-61 real-machine
	// report).
	done     chan struct{}
	doneOnce sync.Once
}

// finish marks the flow as ended. It is safe to call more than once.
func (f *relayFlow) finish() {
	f.doneOnce.Do(func() { close(f.done) })
}

// RelayFramePayload is the decoded FrameMeshRelayData payload.
//
// It is deliberately TINY: the trunk must be able to move a frame without
// parsing the business payload inside it. Only the session id (to route
// the frame) and the opaque bytes travel as structured data.
type RelayFramePayload struct {
	// Session is the relay session id. It is what lets several relays
	// share one connection without their bytes getting mixed up.
	Session uint64 `json:"session"`
	// Data is the opaque payload. For the data plane it is the sealed
	// (encrypted) bytes of the end-to-end session.
	Data []byte `json:"data,omitempty"`
	// Close says this frame ends the relay.
	Close bool `json:"close,omitempty"`
	// Reason explains a close.
	Reason string `json:"reason,omitempty"`
}

// EncodeRelayFrame encodes a relay data frame payload.
func EncodeRelayFrame(p RelayFramePayload) ([]byte, error) {
	return json.Marshal(p)
}

// DecodeRelayFrame decodes a relay data frame payload.
func DecodeRelayFrame(b []byte) (RelayFramePayload, error) {
	var p RelayFramePayload
	if err := json.Unmarshal(b, &p); err != nil {
		return RelayFramePayload{}, fmt.Errorf("mesh: relay trunk: bad relay frame payload: %w", err)
	}
	return p, nil
}

// RelayFrame builds the frame that carries one chunk of a relayed session.
func RelayFrame(session uint64, data []byte) (*wire.Frame, error) {
	payload, err := EncodeRelayFrame(RelayFramePayload{Session: session, Data: data})
	if err != nil {
		return nil, err
	}
	return &wire.Frame{Type: wire.FrameMeshRelayData, Stream: uint32(session), Payload: payload}, nil
}

// RelayCloseFrame builds the frame that ends a relayed session.
func RelayCloseFrame(session uint64, reason string) (*wire.Frame, error) {
	payload, err := EncodeRelayFrame(RelayFramePayload{Session: session, Close: true, Reason: reason})
	if err != nil {
		return nil, err
	}
	return &wire.Frame{Type: wire.FrameMeshRelayClose, Stream: uint32(session), Payload: payload}, nil
}

// RelayJoin is the decoded FrameMeshRelayJoin payload: a member telling
// the anchor "I am connected and willing to receive relayed sessions".
type RelayJoin struct {
	// Device is the member announcing itself.
	Device identity.DeviceID `json:"device"`
}

// EncodeRelayJoin encodes a join announcement.
func EncodeRelayJoin(j RelayJoin) ([]byte, error) {
	return json.Marshal(j)
}

// DecodeRelayJoin decodes a join announcement.
func DecodeRelayJoin(b []byte) (RelayJoin, error) {
	var j RelayJoin
	if err := json.Unmarshal(b, &j); err != nil {
		return RelayJoin{}, fmt.Errorf("mesh: relay trunk: bad relay join payload: %w", err)
	}
	return j, nil
}

// RelayJoinFrame builds the frame that announces a member to the anchor.
func RelayJoinFrame(j RelayJoin) (*wire.Frame, error) {
	payload, err := EncodeRelayJoin(j)
	if err != nil {
		return nil, err
	}
	return &wire.Frame{Type: wire.FrameMeshRelayJoin, Payload: payload}, nil
}

// dialTarget opens a DEDICATED session to the target.
//
// The JOINERS table is preferred over the plain dialer: a member that has
// joined the trunk is reachable over the connection it opened, which is
// the only way a PURE-OUTBOUND member (no inbound port at all) can be
// reached. The plain dialer is the fallback for a member that has
// endpoints of its own.
//
// The session is dedicated: it belongs to ONE flow. The trunk writes
// that flow's OPEN record onto it and splices the requester's stream
// against it — so two relays to the same target can never mix their
// records together.
func (t *RelayTrunk) dialTarget(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
	if t.joiners != nil {
		if sess, err := t.joiners.Open(ctx, target); err == nil {
			return sess, nil
		}
	}
	if t.dial == nil {
		return nil, t.unreachableTarget(target)
	}
	return t.dial(ctx, target, kind)
}

// unreachableTarget explains why the trunk cannot open a session to a
// member.
//
// The wording matters more than it looks. The old text — "this node
// cannot reach <target> right now" — is true and says nothing, and an
// operator cannot tell "the network is down" from "the target never
// joined the trunk". On the real machine the second case looked like a
// TRANSFER hang: the requester waited for the target's hello until the
// handshake timed out, so the symptom pointed at the transport while
// the cause was one step earlier.
//
// So the refusal names the cause and the remedy.
func (t *RelayTrunk) unreachableTarget(target identity.DeviceID) error {
	return fmt.Errorf("%w: mesh: relay trunk: the target %s has not joined this trunk "+
		"(a pure-outbound member must join it with --relay-trunk <an address it can reach>, "+
		"and if that address is a NAT-mapped public one the reply may not find its way back)",
		ErrRelayUnavailable, target)
}

// dialTargetWith opens a DEDICATED session to the target, falling back to
// the caller's own joiner table.
//
// The caller's table is the fallback rather than the trunk's because the
// trunk's table and the caller's table are the same table in the
// shipped wiring — but a caller that serves a requester with a table of
// its own must not silently lose it.
func (t *RelayTrunk) dialTargetWith(ctx context.Context, target identity.DeviceID, kind string, dialer RelayJoiner) (Session, error) {
	if t.joiners != nil {
		if sess, err := t.joiners.Open(ctx, target); err == nil {
			return sess, nil
		}
	}
	if dialer != nil {
		if sess, err := dialer.Open(ctx, target); err == nil {
			return sess, nil
		}
	}
	if t.dial == nil {
		// The joiners table is the ONLY way a pure-outbound member is
		// reachable, so its being empty is the whole story. Say so.
		return nil, t.unreachableTarget(target)
	}
	return t.dial(ctx, target, kind)
}

// OpenFlow opens a relay flow for the given request.
//
// It is the DATA plane's entry point, and it is where the authorization is
// RE-CHECKED (the control plane's check may be stale by now) and where the
// quota is applied.
//
// The order of the checks:
//
//  1. Re-check that the REQUESTER is still a member. If it is not, the
//     target is not even looked up — a revoked device must not cause the
//     trunk to touch a third party's identity.
//  2. Re-check that the TARGET is still a member.
//  3. The switch must still be on.
//  4. The quota must have room: both the stream count and the rate.
//
// The quota is checked LAST on purpose. A quota refusal is a LOCAL
// condition (this node is busy), not a verdict on the requester — so it
// must not be reported in a way that makes the requester think it was
// rejected on membership grounds.
func (t *RelayTrunk) OpenFlow(ctx context.Context, open RelayOpen, req Session) (uint64, error) {
	reqFormat, err := relayFormatOfSession(req)
	if err != nil {
		return 0, err
	}
	return t.openFlow(ctx, open, req, reqFormat, nil)
}

// OpenFlowWithFormats opens a relay flow with an EXPLICIT format for
// each leg.
//
// It is the seam the mixed-version tests drive: the two legs' formats
// come from the ALPNs their connections negotiated, and a test that wants
// a mixed-version relay says so here.
//
// A nil tgtFormat means "ask the target's session", which is what the
// production path does.
func (t *RelayTrunk) OpenFlowWithFormats(ctx context.Context, open RelayOpen, req Session, reqFormat RelaySpliceFormat, tgtFormat RelaySpliceFormat) (uint64, error) {
	return t.openFlow(ctx, open, req, reqFormat, &tgtFormat)
}

// openFlow opens a relay flow, with the two legs' record formats.
//
// The formats are per LEG, and they are the whole version negotiation:
// the record format is an A↔C protocol, and each leg's format comes
// from the ALPN that leg's connection negotiated.
//
// A nil tgtFormat means "ask the target session" — the production path,
// where the target's connection reports its own ALPN.
func (t *RelayTrunk) openFlow(ctx context.Context, open RelayOpen, req Session, reqFormat RelaySpliceFormat, tgtFormat *RelaySpliceFormat) (uint64, error) {
	// ① the requester is still a member
	if _, err := t.members.KeyFor(open.From); err != nil {
		return 0, fmt.Errorf("%w: mesh: relay trunk: the requester %s is no longer a member of this view",
			ErrAuth, open.From)
	}
	// ② the target is still a member
	if _, err := t.members.KeyFor(open.To); err != nil {
		return 0, fmt.Errorf("%w: mesh: relay trunk: the target %s is no longer a member of this view",
			ErrAuth, open.To)
	}
	// ③ the switch is still on
	if !t.Enabled() {
		return 0, fmt.Errorf("%w: mesh: relay trunk: this node does not relay (the trunk switch is off)",
			ErrRelayTrunkDisabled)
	}
	if t.dial == nil && t.joiners == nil {
		// The trunk has no way to reach ANY member. That is a
		// misconfiguration, and it is worth naming as one — the old
		// wording ("this node cannot reach members") reads like a
		// network fact.
		return 0, fmt.Errorf("%w: mesh: relay trunk: this node has no way to reach members "+
			"(no joiners table and no dialer installed: the trunk cannot open a session to a member)",
			ErrRelayUnavailable)
	}
	// ④ the quota has room
	if err := t.reserveStream(); err != nil {
		return 0, err
	}
	// ⑤ the NODE is not already over its rate quota.
	//
	// The rate window is the node's, so a node that is already forwarding
	// at its cap must not take on another flow: the existing flows would
	// be torn down one after another by the per-flow check, and the node
	// would be doing work it has no budget for. Refusing here is the
	// P-7 behaviour the requirement asks for — "超限只拒新请求".
	if over := t.overRateQuota(); over {
		t.releaseStream()
		return 0, fmt.Errorf("%w: mesh: relay trunk: this node is already at its rate quota",
			ErrQuota)
	}
	// Open the session to the target. If this fails, the reservation must
	// be released — otherwise a failing target would leak a stream slot
	// per attempt, and the trunk would eventually refuse every relay for
	// reasons that have nothing to do with the quota.
	tgt, err := t.dialTarget(ctx, open.To, open.Kind)
	if err != nil {
		t.releaseStream()
		return 0, fmt.Errorf("mesh: relay trunk: cannot reach %s: %w", open.To, err)
	}

	// The TARGET's leg format.
	tgtFmt := RelaySpliceFormatBinary
	if tgtFormat != nil {
		tgtFmt = *tgtFormat
	} else {
		tgtFmt, err = relayFormatOfSession(tgt)
		if err != nil {
			_ = tgt.Close()
			t.releaseStream()
			return 0, err
		}
	}

	// The OPEN record towards the TARGET. The target has no way of knowing
	// who is asking for it: it sees a stream on the connection it opened
	// towards the trunk, and the requester's identity travels in this
	// record. Without it the target cannot pick the right key for the
	// end-to-end handshake, so the handshake can never complete (defect C
	// of the round-61 real-machine report).
	//
	// IT ALSO CARRIES THE PAYLOAD CAP for the whole path.
	//
	// The target is a writer too — it answers through the relay — and on
	// a mixed-version relay its record has to fit on the REQUESTER's leg
	// as well as on its own. The target cannot know the requester's leg's
	// format; the anchor dialled both ends, so the anchor does.
	//
	// See maxRelayPayloadForLegs.
	//
	// It is written in the TARGET's format — the target is the one that
	// has to read it.
	capForTarget := maxRelayPayloadForLegs(tgtFmt, reqFormat)
	//
	// It is written in the TARGET's format — the target is the one that
	// has to read it.
	//
	// It is written in the TARGET's format — the target is the one that
	// has to read it.
	if err := writeRelayRecordAs(tgt, relayRecord{
		Type:       uint8(wire.FrameMeshRelayOpen),
		From:       open.From,
		To:         open.To,
		Kind:       open.Kind,
		Hops:       open.Hops,
		MaxPayload: capForTarget,
	}, tgtFmt); err != nil {
		_ = tgt.Close()
		t.releaseStream()
		return 0, fmt.Errorf("mesh: relay trunk: tell %s who is calling: %w", open.To, err)
	}

	t.mu.Lock()
	// ★ THE FLOW MUST NOT BE ADOPTED AFTER THE TEARDOWN. (Round 98.)
	//
	// The `stopping` flag is checked in reserveStream — but that is a
	// DIFFERENT MOMENT from here. The slot is reserved first, then the
	// target's session is opened (which can take seconds, up to the dial
	// budget), and only NOW is the flow registered.
	//
	// Shutdown() walks the live flows and then zeroes the reserved
	// counter. So a flow that reaches this line after that walk is
	// neither torn down (the walk already passed) nor accounted for
	// (its slot was handed back) — it lives forever, invisible to the
	// stream-limit quota, and it holds two sessions open.
	//
	// The check belongs HERE, at the moment the flow becomes visible,
	// because that is the moment it can be adopted. reserveStream's
	// check covers a different window.
	if t.stopping {
		t.mu.Unlock()
		_ = req.Close()
		_ = tgt.Close()
		t.releaseStream()
		return 0, fmt.Errorf("%w: mesh: relay trunk: this node is shutting down", ErrRelayUnavailable)
	}
	t.nextFlow++
	id := t.nextFlow
	flow := &relayFlow{
		id:        id,
		from:      open.From,
		to:        open.To,
		req:       req,
		tgt:       tgt,
		reqBr:     bufio.NewReader(req),
		tgtBr:     bufio.NewReader(tgt),
		reqFormat: reqFormat,
		tgtFormat: tgtFmt,
		done:      make(chan struct{}),
	}
	t.flows[id] = flow
	t.mu.Unlock()

	// The splice runs in its own goroutine: it lives as long as both
	// streams are open, and the caller (the frame loop) must keep serving
	// frames meanwhile.
	go t.splice(flow)
	return id, nil
}

// reserveStream takes one of the trunk's stream slots.
//
// "Only refuse NEW requests, never kill the running ones" is the hard
// requirement (docs/architecture/MESH-DESIGN.md:37): a 2 GB transfer in
// progress must not be thrown away because the trunk is busy.
//
// The reservation is counted SEPARATELY from the live flows, because a
// slot is taken BEFORE the target session is opened and released if that
// fails. Counting only the live flows would let N concurrent attempts all
// pass the check at once and then overflow the limit.
func (t *RelayTrunk) reserveStream() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	// A TRUNK THAT IS STOPPING TAKES NO MORE WORK.
	//
	// The flag is checked HERE as well as at the top of the handler,
	// because the two are not the same moment: the handler's check
	// happens when the connection arrives, and this one happens when the
	// slot is taken — and between them the trunk can be told to stop.
	//
	// A slot taken after the shutdown would keep itself forever,
	// because the teardown has already walked the live flows.
	if t.stopping {
		return fmt.Errorf("%w: mesh: relay trunk: this node is shutting down", ErrRelayUnavailable)
	}
	if t.reserved >= t.quota.MaxStreams {
		return fmt.Errorf("%w: mesh: relay trunk: this node is already carrying %d relay(s) (the limit)",
			ErrQuota, t.quota.MaxStreams)
	}
	t.reserved++
	return nil
}

// releaseStream gives a stream slot back.
//
// It is the error path: the target session could not be opened, so the
// slot must not leak. (A leaked slot would eventually make the trunk
// refuse every relay, for reasons that have nothing to do with the quota
// — the quota would be exhausted by failures.)
func (t *RelayTrunk) releaseStream() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reserved > 0 {
		t.reserved--
	}
}

// closeFlow tears a flow down and frees its slot.
//
// It closes BOTH of the flow's own sessions, and only those.
//
// The target's session used to be the target's long-lived JOIN connection,
// and closing it here meant the target could receive exactly ONE relay
// ever: the first flow's end took its whole inbox away with it (defect E
// of the round-61 real-machine report). The target's side is now a
// session opened for THIS flow alone (see relayFlow), so closing it ends
// this flow and nothing else.
//
// This is the GRACEFUL end: the two sessions are half-closed (a FIN), so
// the bytes the peer has not read yet are still delivered. That matters
// for the delivery ack: the trunk writes the ack and closes the flow in
// the same breath, and an ABORT (a RESET_STREAM) would discard the ack
// before the requester ever read it. Use closeFlowForced for the teardown
// that must not be polite.
func (t *RelayTrunk) closeFlow(id uint64, reason string) {
	t.tearDownFlow(id, reason, false)
}

// closeFlowForced tears a flow down and frees its slot, ABORTING both
// sessions.
//
// It is the teardown for the cases where the peer must not be able to
// keep the flow alive: a revocation, a quota refusal, a hard peer
// failure. A half close asks the peer to stop; an abort makes it stop.
// A revoked device that ignored the FIN must not be able to keep
// streaming through this trunk.
func (t *RelayTrunk) closeFlowForced(id uint64, reason string) {
	t.tearDownFlow(id, reason, true)
}

func (t *RelayTrunk) tearDownFlow(id uint64, reason string, forced bool) {
	// THE REASON IS THE OPERATOR'S ONLY CLUE, and it used to be dropped
	// on the floor: every caller passes one ("relay peer failed",
	// "a member of this relay was revoked", "the trunk is shutting
	// down", …) and this function never looked at it.
	//
	// So a relay could die for ANY reason and the anchor's log would
	// show nothing at all. The operator's only clue was the peer's
	// side, and a peer that had vanished had no side.
	//
	// The reason is a FIXED STRING chosen by this package — it carries
	// no payload, no peer text and no error text, so it cannot leak
	// business content into the log. (That is the same rule the rest
	// of this file follows: report the error CLASS, not its text.)
	//
	// A normal end is not a death: the relay finished, and saying so
	// on every completed relay would drown the log on a busy anchor.
	// So only the ABNORMAL teardowns are reported — the forced ones,
	// and the graceful ones whose reason is not the ordinary "the
	// relay finished".
	if reason != "" && reason != "relay finished" {
		kind := "graceful"
		if forced {
			kind = "forced"
		}
		fmt.Fprintf(relayLogWriter(),
			"mesh: relay trunk: the relay %d was torn down (%s): %s\n", id, kind, reason)
	}
	t.mu.Lock()
	flow, ok := t.flows[id]
	if ok {
		delete(t.flows, id)
		if t.reserved > 0 {
			t.reserved--
		}
	}
	t.mu.Unlock()
	if !ok {
		return
	}
	flow.finish()
	if forced {
		_ = abortSession(flow.req)
		_ = abortSession(flow.tgt)
		return
	}
	_ = flow.req.Close()
	_ = flow.tgt.Close()
}

// FlowCount reports how many relays this trunk is carrying.
//
// It is the quota's live counter and the test's window into the trunk.
// It counts the RESERVED slots, so a relay that is being set up counts
// too — otherwise N concurrent attempts would all pass the limit.
func (t *RelayTrunk) FlowCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.reserved
}

// FlowBytes reports how many payload bytes the flow has forwarded.
//
// It is the quota's accounting, exposed so a test can check that the
// FAST PATH charges the same number the slow path does: the rate budget
// is charged on the payload bytes, and the binary header carries the
// same number as the decoded record's Data length did.
func (t *RelayTrunk) FlowBytes(id uint64) int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	flow, ok := t.flows[id]
	if !ok {
		return 0
	}
	return flow.total
}

// CloseAll tears down every live relay and frees the slots.
//
// It is the shutdown path: an anchor that is stopping must not leave
// relayed sessions hanging on either end.
// Shutdown stops the trunk: no new relay is admitted, and every live
// one is torn down.
//
// It used to have no way to be called: CloseAll existed but nothing in
// the production path called it, so an anchor that stopped left its live
// relay flows running — and their stream slots reserved. A second relay
// on the same anchor would then be refused for lack of a slot that no
// longer had a flow behind it.
//
// So the shutdown is ONE call that does both halves: set the flag (no
// new admissions) and tear down what is live (the slots come back).
func (t *RelayTrunk) Shutdown() {
	t.mu.Lock()
	t.stopping = true
	t.mu.Unlock()
	t.CloseAll()
	// The RESERVED slots are released too.
	//
	// A slot is reserved BEFORE the flow exists (the target's session
	// has to be opened first), so CloseAll — which walks the live
	// flows — cannot see a reservation that has no flow behind it yet.
	// Those are exactly the slots a shutdown must hand back: the
	// teardown is the end of the node's work, so every slot it held is
	// free.
	t.mu.Lock()
	t.reserved = 0
	t.mu.Unlock()
}

// isStopping reports whether the trunk is shutting down.
func (t *RelayTrunk) isStopping() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.stopping
}

// CloseAll tears down every live relay and gives their stream slots
// back.
func (t *RelayTrunk) CloseAll() {
	t.mu.RLock()
	ids := make([]uint64, 0, len(t.flows))
	for id := range t.flows {
		ids = append(ids, id)
	}
	t.mu.RUnlock()
	for _, id := range ids {
		t.closeFlowForced(id, "the trunk is shutting down")
	}
}

// splice forwards the records between the requester and the target until
// either side ends.
//
// It is a pipe for the RECORDS, and for the records' PAYLOAD it is
// opaque: the trunk reads the envelope (so it can count the bytes and
// route the session) but it never opens the payload. The payload is the
// end-to-end ciphertext, and the trunk holds no key for it.
//
// The half-close semantics of Session are preserved: when one side ends,
// the trunk closes the other side's stream — so the far end sees the end
// of the session, and the tail of the message is not lost. That is the
// whole reason Session documents Close() as a HALF close.
//
// IT ENDS WHEN EITHER SIDE ENDS. The old code waited for BOTH
// directions to finish, and a peer that closes its send side and then
// waits for the reply would leave the other direction blocked in a read
// forever: the flow never ended, its quota slot was never given back,
// and the relay stayed counted as live (defect E of the round-61
// real-machine report: "the first flow succeeds, then immediately
// 'cannot reach <target>'" was the visible symptom of a flow that never
// let go).
//
// Closing the flow ends the other direction: the flow's teardown closes
// both of its sessions, which unblocks the reader on the other side.
// splice carries the two directions of a flow until BOTH of them are done.
//
// The two directions end at DIFFERENT times, and that is the whole point.
// Every request/response protocol in this codebase does the same thing:
// `cmesh msg`, `cmesh run` and `cmesh send-to` all send their request,
// HALF-close (FIN), and only THEN wait for the answer. So the request
// direction ends FIRST, while the answer is still on its way.
//
// The splice used to treat the first end as the end of the flow: it
// closed both of the flow's sessions, so the answer — still travelling on
// the other direction — was discarded. The symptom on the real machine was
// that the message reached the target but the DELIVERY ACK never came
// back, and the requester saw EOF instead.
//
// The rule is therefore: one direction ending HALF-CLOSES the far side
// (the far side may still have something to say), and the flow is over
// only when both directions are done.
func (t *RelayTrunk) splice(flow *relayFlow) {
	// The MIXED-VERSION line, once per relay.
	//
	// It is here, at the top of the splice, and not per record: a line
	// per record would drown the log on a busy anchor — at the measured
	// 65.9–67.8 MiB/s the record rate is in the tens of thousands per
	// second — and the fact it reports (the two legs disagree) does not
	// change during the relay.
	//
	// The operator's reading of the line is "the rollout is not finished":
	// one of the two ends of this relay is still on the old format, so
	// this relay is running on the slow path and carrying less than it
	// could.
	//
	// THE LINE NAMES BOTH FORMATS rather than saying "the JSON path",
	// because the mixed case has TWO shapes and only one of them is
	// "the JSON path":
	//
	//	requester on JSON, target on binary → the anchor reads JSON and
	//	                                   writes binary frames
	//	requester on binary, target on JSON → the anchor reads binary
	//	                                   frames and writes JSON
	//
	// A line that said "running on the JSON path" for a relay that WRITES
	// binary frames would be wrong, and an operator who reads it would
	// go looking in the wrong place. Naming the two formats also says
	// WHICH end is the old one — which is the end to upgrade.
	if flow.reqFormat != flow.tgtFormat {
		fmt.Fprintf(relayLogWriter(),
			"mesh: relay trunk: the relay from %s to %s is running on the slow path: "+
				"the requester's leg speaks %s and the target's leg speaks %s; "+
				"the anchor has to decode and re-encode every record until both ends are upgraded\n",
			flow.from, flow.to, flow.reqFormat, flow.tgtFormat)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	// ★ THE TEARDOWN MUST NOT DEPEND ON BOTH DIRECTIONS FINISHING.
	// (Round 98.)
	//
	// The close used to run AFTER wg.Wait(), so ONE blocked direction
	// kept the flow — and its stream slot — alive forever.
	//
	// The shape that does it: the requester half-closes (its direction
	// sees EOF and ends), and the TARGET never answers. The direction
	// that reads the target is then blocked on a read that will never
	// return, so wg.Wait() never returns, so the flow is never closed.
	//
	// A `defer` alone does NOT fix that: the deferred close still waits
	// for splice() to RETURN, and splice() is blocked in wg.Wait().
	//
	// So the wait itself is bounded. When ONE direction has finished,
	// the other gets a grace period to finish on its own — long enough
	// for the answer to a request that was sent before the half-close
	// (the ordinary request/response shape) — and if it does not, the
	// flow is torn down FORCED, which aborts both sessions and releases
	// the slot.
	//
	// The reason is a fixed string, like every other reason in this
	// file: it names a class, not a peer's text.
	finished := make(chan struct{}, 2)
	var once sync.Once
	markFinished := func() { once.Do(func() { finished <- struct{}{} }) }

	go func() {
		defer wg.Done()
		defer markFinished()
		t.forwardRecords(flow, flow.reqBr, flow.tgt, flow.reqFormat, flow.tgtFormat)
	}()
	go func() {
		defer wg.Done()
		defer markFinished()
		t.forwardRecords(flow, flow.tgtBr, flow.req, flow.tgtFormat, flow.reqFormat)
	}()

	// Wait for the first direction to end.
	<-finished

	// Give the other direction a bounded grace period.
	otherDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(otherDone)
	}()
	select {
	case <-otherDone:
		// Both directions finished: the ordinary end.
	case <-time.After(relayTeardownGrace):
		// One side is stuck. The flow must not survive it: the slot is
		// the trunk's quota, and a quota slot held by a dead relay is a
		// quota slot nobody can use.
		t.closeFlowForced(flow.id, "the relay peer did not finish")
		<-otherDone
	}
	t.closeFlow(flow.id, "relay finished")
}

// forwardRecords forwards the records from one side to the other.
//
// THE TWO PATHS.
//
// When the two legs speak the SAME format — the normal case, and the one
// the whole switch is about — the records are forwarded VERBATIM: the
// anchor reads the 24-byte fixed header, learns the payload's length
// from it, and copies the bytes. It never decodes the payload, and it
// holds no key for it.
//
// When the two legs DISAGREE — a mixed-version relay — the anchor has to
// TRANSLATE: it decodes the record and re-encodes it in the far leg's
// format. That is the old (slow) path, and it is the price of a
// deployment that is only half upgraded.
//
// The measured difference, on the real host, same build, same lab, 128 MB
// bench, 3 reps each, link=relay:
//
//	JSON decode + re-encode            33.3 – 36.5 MiB/s
//	binary frame, decode + re-encode   59.4 – 61.6 MiB/s  (+77%)
//	binary frame, byte-copy            65.9 – 67.8 MiB/s  (+96%)
//
// So the fast path is not a micro-optimisation: it is the difference
// between a relay that carries 34 MiB/s and one that carries 66.
//
// WHAT IT COUNTS is the SIZE of the payload it carries. The count is on
// the bytes the trunk ACTUALLY FORWARDS. The trunk's own business traffic
// does not pass through here, so it never counts against the relay budget
// — that is the "自身业务不受影响" half of P-7.
//
// The rate quota is checked BEFORE the write, so an over-rate flow is
// refused rather than silently truncated: the flow's streams are closed
// and the peers learn about it.
func (t *RelayTrunk) forwardRecords(flow *relayFlow, br *bufio.Reader, to Session, from, toFormat RelaySpliceFormat) {
	// THE FAST PATH is the one that matters, and it is legal only when
	// the two legs both speak the BINARY FRAME: a verbatim copy is a copy
	// of the record AS IT IS, and the far leg has to be able to read it.
	//
	// The splicer is built ONCE, here, and used for every record of this
	// direction. Building it per record would put an allocation back on
	// the hot path — and the whole point of the splice is that it
	// allocates NOTHING per record (measured: 957 ns per 32 KiB record,
	// zero allocations, against 114,369 ns and twelve allocations for
	// the JSON path).
	//
	// WHY THE CONDITION IS "BOTH BINARY" AND NOT "THE LEGS AGREE".
	//
	// The splicer refuses two JSON legs on purpose: there is nothing to
	// splice, the decode-and-re-encode path already forwards that format
	// and has always done so. That refusal is a NORMAL ANSWER, not a
	// fault — and the first version of this switch read it as a fault
	// and tore the flow down.
	//
	// The symptom was a relay that died at its first record on the one
	// deployment shape that has to keep working: a member that negotiated
	// the OLD ALPN on both legs (an old member on a new anchor, or two
	// old members on an old anchor). Since the anchor is upgraded FIRST,
	// that shape exists for the whole window between the anchor's
	// upgrade and the last member's upgrade — so tearing it down would
	// take the relay feature down for exactly as long as the rollout
	// took. See TestARelayWhoseTwoLegsBothSpeakJSONStillWorks.
	//
	// So the rule is: the splicer is the BINARY fast path, and EVERY
	// other shape — two JSON legs, or two legs that disagree — goes down
	// the decode-and-re-encode path, which is the old path and which has
	// always worked.
	var splicer *RelaySplicer
	if from == RelaySpliceFormatBinary && toFormat == RelaySpliceFormatBinary {
		var err error
		splicer, err = NewRelaySplicer(from, toFormat)
		if err != nil {
			// Unreachable in practice: the two legs both speak the binary
			// frame, which is the one shape the splicer accepts. But if it
			// ever happened, the relay must not carry a record in a format
			// the far side cannot read.
			t.closeFlowForced(flow.id, "the relay could not be spliced")
			return
		}
		splicer.Quota = &flowQuota{flow: flow, trunk: t}
		splicer.Done = flow.done
	}
	for {
		if splicer != nil {
			if done := t.forwardOneVerbatim(flow, br, to, splicer); done {
				return
			}
			continue
		}
		if done := t.forwardOneTranslated(flow, br, to, from, toFormat); done {
			return
		}
	}
}

// forwardOneVerbatim forwards ONE record without decoding it.
//
// It is the fast path, and it is where the measured win lives.
//
// The record's length comes from the FIXED HEADER, not from a scan, and
// the header says how long the meta block and the payload are. So the
// anchor knows how many bytes to copy before it copies the first one.
//
// It returns true when this direction is OVER (the peer finished, or the
// flow was torn down), and false when the caller should read the next
// record.
func (t *RelayTrunk) forwardOneVerbatim(flow *relayFlow, br *bufio.Reader, to Session, splicer *RelaySplicer) bool {
	hdr, err := splicer.ForwardOne(br, to)
	if err != nil {
		if errors.Is(err, io.EOF) {
			// The sender is DONE SENDING — a half close, not a
			// failure. The far side may still have an answer to give,
			// so the far side is half-closed too and this direction ends
			// here.
			//
			// This is the case every request/response protocol
			// produces: "send the request, FIN, wait for the answer".
			_ = to.Close()
			return true
		}
		// A hard failure: the peer is GONE, not merely finished.
		// Nothing can be salvaged, so the flow is torn down — and
		// tearing it down releases the other direction, which would
		// otherwise wait for a peer that will never answer.
		t.closeFlowForced(flow.id, "relay peer failed")
		return true
	}
	if hdr.Type == uint8(wire.FrameMeshRelayClose) {
		// The sender is done. The close record has been forwarded
		// already, so the far side knows. This direction ends with a
		// HALF close: the far side may still have something to say.
		_ = to.Close()
		return true
	}
	return false
}

// forwardOneTranslated forwards ONE record, decoding it and re-encoding
// it in the far leg's format.
//
// It is the MIXED-VERSION fallback: the anchor has to understand the
// record to change its format, and understanding the record is exactly
// what the fast path avoids. So this path is the old path, and it is
// SLOW — that is the honest cost of a half-upgraded deployment.
//
// It returns true when this direction is OVER, and false when the caller
// should read the next record.
func (t *RelayTrunk) forwardOneTranslated(flow *relayFlow, br *bufio.Reader, to Session, from, toFormat RelaySpliceFormat) bool {
	rec, err := readRelayRecordAs(br, from)
	if err != nil {
		if errors.Is(err, io.EOF) {
			_ = to.Close()
			return true
		}
		t.closeFlowForced(flow.id, "relay peer failed")
		return true
	}
	if rec.Type == uint8(wire.FrameMeshRelayClose) {
		// The sender is done. Forward the close and stop: the flow
		// is over on this side. If the close itself cannot be written,
		// this direction is dead in the same way a data write would be —
		// tear the flow down explicitly rather than leaving the far
		// direction to depend on the peer's failure mode.
		if err := writeRelayRecordAs(to, rec, toFormat); err != nil {
			t.closeFlowForced(flow.id, "relay close forward failed")
			return true
		}
		_ = to.Close()
		return true
	}
	// The rate budget is a THROTTLE, not a kill switch. chargeBytes
	// reports how long the node needs to refill; the forwarder waits
	// that long and tries again. The old behaviour tore the flow down
	// the moment the bucket ran dry — a transfer faster than the cap
	// DIED at the cap, which is the exact "file size limit" behaviour
	// the bucket was introduced to remove. P-7 is a sustained-RATE
	// requirement: a hundred megabytes at a 50 MiB/s cap takes two
	// seconds and arrives whole.
	for {
		wait := t.chargeBytes(flow, int64(len(rec.Data)))
		if wait == 0 {
			break
		}
		select {
		case <-time.After(wait):
			// Refilled (maybe — another direction may have drunk
			// from the same bucket, so loop and re-check).
		case <-flow.done:
			// The flow ended while this direction waited; nothing
			// left to forward.
			return true
		}
	}
	if err := writeRelayRecordAs(to, rec, toFormat); err != nil {
		// The write failed, so this direction cannot go on. Tear the
		// flow down EXPLICITLY: the other direction must not be left
		// waiting for a flow that is already over. (On the real stream
		// a dead write usually means dead reads too, so the other
		// direction unblocks on its own — but "usually" is not a
		// teardown path, and the quota slot must not depend on the
		// peer's failure mode.)
		//
		// THE REASON IS LOGGED, because on the TRANSLATED path the
		// failure has a cause an operator can act on: the record the
		// writer sent does not fit in the far leg's format. The JSON
		// envelope is a third bigger than the binary frame, so a writer
		// that ignored the cap the anchor stated would produce exactly
		// this — and without the line the operator sees a relay that
		// died for no stated reason.
		fmt.Fprintf(relayLogWriter(),
			"mesh: relay trunk: the relay from %s to %s could not forward a record "+
				"the %s leg cannot hold: %v; the writer is not honouring the payload cap\n",
			flow.from, flow.to, toFormat, err)
		t.closeFlowForced(flow.id, "relay forwarding write failed")
		return true
	}
	return false
}

// flowQuota is the rate budget the splicer charges against.
//
// It is the trunk's own budget, reached through the same accounting the
// translated path uses, so the two paths charge IDENTICALLY: a relay
// that falls back to the slow path does not get a different quota.
type flowQuota struct {
	flow  *relayFlow
	trunk *RelayTrunk
}

// Reserve charges payloadLen payload bytes against the budget.
//
// The splice asks once and waits if the bucket is dry; the wait is the
// trunk's own throttling, so it is re-asked rather than trusted (the
// flow's other direction may have drunk from the same bucket).
func (q *flowQuota) Reserve(payloadLen int64) (time.Duration, error) {
	for {
		wait := q.trunk.chargeBytes(q.flow, payloadLen)
		if wait == 0 {
			return 0, nil
		}
		// The wait must be INTERRUPTIBLE.
		//
		// The old shape slept first and checked afterwards, so a flow
		// that ended while the splice was waiting for the rate budget
		// kept its forwarding goroutine for the whole wait — up to a
		// full record's worth of refill (about 15 s at a 64 KiB/s
		// cap). The check after the sleep could therefore only report
		// the end LATE; it could never cut the wait short.
		//
		// So the wait is a select on the flow's end and a timer: the
		// goroutine goes away as soon as the flow does, instead of
		// lingering for a budget it will never spend.
		timer := time.NewTimer(wait)
		select {
		case <-q.flow.done:
			timer.Stop()
			return 0, fmt.Errorf("%w: mesh: the flow ended while the relay waited for the rate budget",
				ErrRelaySpliceFlowEnded)
		case <-timer.C:
		}
	}
}

// chargeBytes reserves n bytes against the node's rate budget and reports
// how long the caller must WAIT before they may be forwarded.
//
// Zero means the bytes are budgeted now (and n has been charged). A
// positive duration means the bucket is dry: the caller sleeps for the
// reported time and calls again — the refill may have been drunk by the
// flow's other direction in the meantime, so the answer is re-checked
// rather than trusted.
//
// The budget is the NODE's, not the flow's: P-7 says "单节点被持续转发
// 时，吞吐 ≤ 50 MiB/s" — the unit is the node. A per-flow budget would
// let twenty flows each carry 50 MiB/s, so the node would forward a
// thousand, twenty times the cap the requirement states.
//
// One record larger than a whole second's budget is PACED, not waved
// through: it waits until the bucket could cover it minus one full cap
// (the burst allowance it may spend into debt), and the wait is computed
// against the PRE-charge balance. The first version of this branch
// charged and returned zero unconditionally — which a stream of
// back-to-back oversized records turned into "the cap does not exist at
// all" (measured: 1 MiB through a 64 KiB/s cap in 9 ms). The trap on
// the other side is charging first and then returning the deficit as a
// wait: each retry re-charges, consuming exactly the refill, so the
// record NEVER moves. The wait must be computed before the charge.
func (t *RelayTrunk) chargeBytes(flow *relayFlow, n int64) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.refillBucket(now)
	capPerSec := float64(t.quota.MaxBytesPerSec)
	if capPerSec <= 0 {
		capPerSec = float64(DefaultRelayBytesPerSec)
	}
	if float64(n) > capPerSec {
		// Oversized: the burst allowance is one full cap (the record
		// may spend the bucket to -cap), so the record may move when
		// tokens + cap >= n. Until then, wait — and do NOT charge,
		// or the retry consumes its own refill (livelock).
		if wait := float64(n) - t.bucketTokens - capPerSec; wait > 0 {
			return time.Duration(wait / capPerSec * float64(time.Second))
		}
		t.bucketTokens -= float64(n)
		flow.total += n
		return 0
	}
	if float64(n) <= t.bucketTokens {
		t.bucketTokens -= float64(n)
		flow.total += n
		return 0
	}
	deficit := float64(n) - t.bucketTokens
	return time.Duration(deficit / capPerSec * float64(time.Second))
}

// accountForward charges n bytes against the node's rate window without
// forwarding anything.
//
// It exists so that the node-level accounting can be exercised directly:
// a test (or a future policy) needs to put the node over its cap without
// running a whole transfer.
// refillBucket adds the budget the node earned since the last refill.
//
// Every reader of the bucket must call it: the tokens come back with
// TIME, and a reader that does not refill sees a node that stayed
// saturated forever.
func (t *RelayTrunk) refillBucket(now time.Time) {
	capPerSec := float64(t.quota.MaxBytesPerSec)
	if capPerSec <= 0 {
		capPerSec = float64(DefaultRelayBytesPerSec)
	}
	if t.bucketTime.IsZero() {
		t.bucketTime = now
		t.bucketTokens = capPerSec
		return
	}
	if elapsed := now.Sub(t.bucketTime).Seconds(); elapsed > 0 {
		t.bucketTokens += elapsed * capPerSec
		// The ceiling is the cap PLUS one record's worth, not the cap
		// alone.
		//
		// The oversized-record rule waits until the bucket can cover the
		// record minus one cap (the burst allowance). With a ceiling of
		// exactly one cap, that condition can NEVER hold for a record
		// bigger than two caps — the balance could not reach it — so the
		// forwarder would wait FOREVER and the flow's stream slot would
		// be held for ever.
		//
		// The record size is bounded by maxRelayRecord (1 MiB) and the
		// cap is one operator flag away: a metered node with
		// --relay-max-bytes-per-sec below ~384 KiB/s would have hung on
		// every large file.
		if ceiling := capPerSec + float64(maxRelayRecord); t.bucketTokens > ceiling {
			t.bucketTokens = ceiling
		}
		t.bucketTime = now
	}
}

func (t *RelayTrunk) accountForward(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.refillBucket(now)
	t.bucketTokens -= float64(n)
	if t.bucketTokens < 0 {
		t.bucketTokens = 0
	}
}

// overRateQuota reports whether the node is already at (or over) its rate
// cap in the current window.
func (t *RelayTrunk) overRateQuota() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.refillBucket(time.Now())
	// The node is at its cap when what is left in the bucket is less than
	// ONE FLOW'S FAIR SHARE of it (the cap divided by the number of flows
	// it may carry).
	//
	// "Empty" would be the obvious test, and it is wrong: the bucket
	// refills CONTINUOUSLY, so after the node has spent its whole
	// second's budget there is still a sliver left — enough for a
	// handful of bytes and not enough for a flow. The fair share is the
	// amount a new flow would actually need to be worth starting, so
	// "less than that" is the honest test for "this node is saturated".
	capPerSec := float64(t.quota.MaxBytesPerSec)
	if capPerSec <= 0 {
		capPerSec = float64(DefaultRelayBytesPerSec)
	}
	streams := t.quota.MaxStreams
	if streams <= 0 {
		streams = DefaultRelayMaxStreams
	}
	return t.bucketTokens < capPerSec/float64(streams)
}

// RevokeFlows tears down every live relay whose two ends are not both
// still members.
//
// This is the half the previous round did NOT do: refusing NEW requests
// from a revoked device is not enough, because a relay that was GRANTED
// before the revocation keeps running until it ends on its own.
//
// The check is the same one the authorization path uses, and the order is
// the same too: the requester is checked first, so the target's identity
// is not touched on behalf of a device that is already gone.
func (t *RelayTrunk) RevokeFlows() int {
	t.mu.RLock()
	flows := make([]*relayFlow, 0, len(t.flows))
	for _, f := range t.flows {
		flows = append(flows, f)
	}
	t.mu.RUnlock()

	revoked := 0
	for _, f := range flows {
		if t.stillMember(f.from) && t.stillMember(f.to) {
			continue
		}
		t.closeFlowForced(f.id, "a member of this relay was revoked")
		revoked++
	}
	return revoked
}

// stillMember reports whether id is still in the member view.
//
// It asks the NARROW question — "is it still a member?" — and not the
// wide one KeyFor asks ("is it a member AND is its certificate valid
// AND is there a view?").
//
// The distinction matters for the same reason it matters on the direct
// path: a member whose certificate merely LAPSED is not a revoked
// member. The product renews certificates precisely so that expiry is a
// normal, recoverable event — and a revocation watch that tears down a
// flow because a timer ran out kills a legitimate transfer and tells
// the peer it was revoked.
//
// The two paths must agree: the direct path asks the narrow question
// (see Listener.stillMember), so this one does too. A member whose
// certificate lapsed keeps its direct session AND its relay flow —
// a member whose membership was revoked loses both.
func (t *RelayTrunk) stillMember(id identity.DeviceID) bool {
	if m, ok := t.members.(membershipChecker); ok {
		return m.StillMember(id)
	}
	_, err := t.members.KeyFor(id)
	return err == nil
}

// StartRevocationWatch re-checks the live relays on a ticker.
//
// This is the mechanism that makes a revocation reach an ESTABLISHED
// relay instead of only the next request. The audit is explicit that the
// push of a new member view can take up to a second (the snapshot
// coalescing), so the metric "撤销即时生效" cannot be met by the push
// alone — a re-check has to happen on a timer.
//
// The interval is one second: the requirement is that a revoked device's
// relay stops within a second.
func (t *RelayTrunk) StartRevocationWatch(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Second
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.RevokeFlows()
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// The REQUESTER side.
//
// The requester (A) opens a session to the trunk, asks it to carry a
// session to the target, and then speaks the END-TO-END protocol THROUGH
// the trunk. The trunk never terminates that protocol.
//
// The requester ends up with a Session — the same abstraction the direct
// path produces — so the business layer cannot tell the difference.
//
// THE RECORD FRAMING. The relay session is ONE QUIC stream, and the
// records ride it length-prefixed (the same framing the rest of the mesh
// uses). The requester writes a record, the trunk reads it, and the
// trunk writes what comes back from the target.
//
// The requester's Session interface is therefore NOT the raw stream: a
// Write of 100 bytes has to become ONE record, and a Read has to return
// the plaintext of exactly one record. That is what relaySession below
// does: it buffers a decrypted record's plaintext and hands it out in
// pieces.
// ---------------------------------------------------------------------------

// RelayDialer opens a session to the relay member.
//
// It is the seam that keeps the relay path testable without a network:
// the requester needs a session to the TRUNK (which it dials directly, by
// the trunk's endpoints), and the trunk needs a session to the TARGET
// (which it dials on the requester's behalf).
type RelayDialer func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error)

// RelayRequesterConfig configures the requester side of the relay.
type RelayRequesterConfig struct {
	// Device is the requester's identity.
	Device *identity.Device
	// Resolver resolves members' keys. It must be the LIVE view: the
	// requester resolves the TARGET's key to verify the target's
	// end-to-end hello, and a revoked target must fail that check.
	Resolver MemberResolver
	// Target is the member being reached.
	Target identity.DeviceID
	// Kind is the session kind ("ping", "dial", "msg", "file", …).
	Kind string
	// Dial opens a session to the relay member.
	Dial RelayDialer
	// TargetKey is the target's public key, from the member view. It
	// verifies the target's end-to-end hello signature.
	TargetKey ed25519.PublicKey
	// Relay is the relay member's device ID (for the record).
	Relay identity.DeviceID
}

// relaySession is the requester's view of a relayed session.
//
// It is a Session: the business layer writes plain bytes and reads plain
// bytes, and cannot tell whether the bytes travelled directly or through
// a trunk.
//
// The sealing happens in Write (the plaintext is sealed and sent as one
// record) and the opening happens in Read (the record is read, opened,
// and its plaintext is buffered).
type relaySession struct {
	// conn is the session to the trunk. The records ride it.
	conn Session
	// e2e is the end-to-end session with the target.
	e2e *E2ESession
	// target names the target (for the close record).
	target identity.DeviceID

	// rec reads the records the trunk sends back.
	rec relayRecordReader
	// format is the record format of this leg.
	//
	// It comes from the ALPN the connection to the trunk negotiated, and
	// it is the requester's half of the switch: a requester on a NEW
	// anchor writes binary records, and a requester on an OLD anchor
	// writes JSON ones. The requester cannot know which before it
	// connects — and it does not have to guess, because the ALPN says.
	format RelaySpliceFormat
	// maxPayload is the cap the ANCHOR stated in its answer.
	//
	// It is the largest plaintext that survives the WHOLE path of the
	// relay — the anchor knows the target's leg's format too, and on a
	// mixed-version relay the record has to fit on the target's leg as
	// well as on this one. Zero means the anchor stated no cap, and the
	// writer uses its own leg's default.
	//
	// See relayRecord.MaxPayload and maxRelayPayloadForLegs.
	maxPayload uint64
	// wmu guards the write side: records go out one at a time, and two
	// writers would interleave them.
	wmu sync.Mutex
	// buf holds the plaintext of the record currently being read out.
	buf []byte
	// eof marks the end of the stream.
	eof bool
	// closed marks the session as finished.
	closed bool
}

// Write seals the plaintext and sends it as relay records.
//
// A write that does not fit in ONE record is split (see
// sealAndWriteRelayData): the record framing is the envelope on the
// wire, not a message boundary — the business layer above reads a
// stream, so it sees the same bytes in the same order either way.
func (r *relaySession) Write(p []byte) (int, error) {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if r.closed {
		return 0, errors.New("mesh: relay: the session is closed")
	}
	n, err := sealAndWriteRelayDataAs(r.conn, r.e2e, p, r.format, r.maxPayload)
	if err != nil {
		return n, fmt.Errorf("mesh: relay: write: %w", err)
	}
	return n, nil
}

// Read returns the plaintext of the next record.
//
// The plaintext is buffered: a Read may ask for fewer bytes than the
// record holds, and the rest must survive for the next Read.
func (r *relaySession) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		rec, err := r.rec.nextRecord()
		if err != nil {
			r.eof = true
			return 0, err
		}
		switch rec.Type {
		case uint8(wire.FrameMeshRelayData):
			plain, err := r.e2e.Open(rec.Data)
			if err != nil {
				return 0, err
			}
			r.buf = plain
		case uint8(wire.FrameMeshRelayClose):
			r.eof = true
			return 0, io.EOF
		default:
			return 0, fmt.Errorf("mesh: relay: unexpected record type %d", rec.Type)
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// Close ends the send side of the relayed session.
//
// It sends the close record (so the trunk and the target learn the
// message is over) and then closes the trunk session.
func (r *relaySession) Close() error {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	// The close record is sent UNSEALED: it carries no business
	// content, and the trunk has to be able to act on it (tear the
	// relay down) even though it cannot read the payload.
	if err := writeRelayRecordAs(r.conn, relayRecord{
		Type:   uint8(wire.FrameMeshRelayClose),
		Close:  true,
		Reason: "the sender closed the session",
	}, r.format); err != nil {
		_ = r.conn.Close()
		return err
	}
	return r.conn.Close()
}

// ALPN reports the record format this leg speaks.
//
// It is here so that a leg's format can be asked of it the same way a
// connection's can. Without it, relayFormatOfSession asks, gets no
// answer, and FALLS BACK TO THE BINARY FRAME — so a leg that had
// negotiated the OLD protocol would be read as binary while it writes
// JSON, and the reader would find text where a length prefix belongs.
//
// (The symptom is memorable: "frame of 1937009004 bytes exceeds the
// 1048576 byte cap", where 1937009004 is 0x7374696C — the four ASCII
// bytes "stil" read as a big-endian length. A record's JSON payload
// read as a length prefix.
//
// The byte order is BIG endian (the frame length is written and read
// that way in internal/mesh/direct.go), so the bytes on the wire were
// 73 74 69 6C = "stil". The little-endian reading of the same number
// spells "lits" — a plausible-looking but wrong string to grep for.)
func (r *relaySession) ALPN() string {
	switch r.format {
	case RelaySpliceFormatJSON:
		return RelayALPN
	default:
		return RelayALPN2
	}
}

// SetReadDeadline bounds the pending and future reads.
func (r *relaySession) SetReadDeadline(t time.Time) error {
	return r.conn.SetReadDeadline(t)
}

// SetWriteDeadline bounds the pending and future writes.
func (r *relaySession) SetWriteDeadline(t time.Time) error {
	return r.conn.SetWriteDeadline(t)
}

// Abort ends the relayed session on both sides.
//
// The half close (Close) is the polite end of the send direction. This is
// the hard end: the trunk tears the flow down (a revocation, a quota
// refusal) and the requester must not be able to keep the session alive.
func (r *relaySession) Abort() error {
	r.wmu.Lock()
	r.closed = true
	r.wmu.Unlock()
	return abortSession(r.conn)
}

// OpenRelay opens a relayed session to the target through the trunk.
//
// The sequence is:
//
//  1. Ask the trunk to carry a session (the open record).
//  2. The trunk answers: granted or refused.
//  3. The end-to-end handshake happens THROUGH the trunk: the target's
//     hello comes back, is verified, and the requester's hello goes up.
//
// The refusal of step 2 is classified: an auth failure is a verdict (do
// not try another relay with the same credentials), a quota failure is a
// local condition (another relay may well say yes).
func OpenRelay(ctx context.Context, cfg RelayRequesterConfig) (Session, error) {
	if cfg.Device == nil {
		return nil, errors.New("mesh: relay: needs a device identity")
	}
	if cfg.Resolver == nil {
		return nil, errors.New("mesh: relay: needs a member resolver")
	}
	if cfg.Target == "" {
		return nil, errors.New("mesh: relay: needs a target")
	}
	if cfg.Dial == nil {
		return nil, fmt.Errorf("%w: mesh: relay: no dialer for the relay member", ErrRelayUnavailable)
	}
	if len(cfg.TargetKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("mesh: relay: the target key is malformed (%d bytes)", len(cfg.TargetKey))
	}

	// The end-to-end session comes first: the hello has to be ready
	// before the trunk asks the target for one.
	e2e, err := NewE2ESession(E2EConfig{
		Self:    cfg.Device,
		Peer:    cfg.Target,
		PeerKey: cfg.TargetKey,
		Role:    E2ERoleInitiator,
	})
	if err != nil {
		return nil, err
	}

	// The session to the trunk.
	conn, err := cfg.Dial(ctx, cfg.Relay, nil)
	if err != nil {
		return nil, fmt.Errorf("mesh: relay: cannot reach the relay member: %w", err)
	}
	// The HANDSHAKE is bounded. Without a deadline, a trunk that accepts
	// the connection and then goes quiet leaves the requester blocked in
	// a read forever: the caller's context does not interrupt a blocking
	// socket read, and the requester hangs instead of failing. A failed
	// relay is an error the caller can act on; a hung one is not.
	handshakeDeadline := time.Now().Add(relayHandshakeTimeout)
	_ = conn.SetReadDeadline(handshakeDeadline)
	// The record format of this leg comes from the ALPN the connection
	// to the trunk negotiated.
	//
	// It is the requester's half of the version negotiation, and it is
	// what makes the switch deployable: a requester that reached an OLD
	// anchor negotiated the old ALPN, so it speaks JSON — and the old
	// anchor, which only knows JSON, keeps working.
	format, err := relayFormatOfSession(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mesh: relay: the relay member speaks an unknown protocol: %w", err)
	}
	r := &relaySession{
		conn:   conn,
		e2e:    e2e,
		target: cfg.Target,
		rec:    newFramedRecordReader(bufio.NewReader(conn), format),
		format: format,
	}

	// ① ask the trunk to carry the session.
	open := RelayOpen{
		From: cfg.Device.ID(),
		To:   cfg.Target,
		Hops: 1,
		Kind: cfg.Kind,
	}
	if err := writeRelayRecordAs(conn, relayRecord{
		Type: uint8(wire.FrameMeshRelayOpen),
		From: open.From,
		To:   open.To,
		Hops: open.Hops,
		Kind: open.Kind,
	}, format); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mesh: relay: send the open record: %w", err)
	}

	// ② the trunk's verdict.
	rec, err := r.rec.nextRecord()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mesh: relay: read the trunk's answer: %w", err)
	}
	if rec.Type != uint8(wire.FrameMeshRelayAnswer) {
		_ = conn.Close()
		return nil, fmt.Errorf("mesh: relay: expected an answer record, got frame type %d", rec.Type)
	}
	if !rec.Grant {
		_ = conn.Close()
		return nil, relayRefusal(rec)
	}
	// The ANCHOR'S CAP.
	//
	// The anchor is the only node that knows the whole path — it dialled
	// the target, so it knows the target's leg's format — and on a
	// mixed-version relay the record the requester writes has to fit on
	// the target's leg as well as on this one. So the anchor states the
	// cap in its answer, and the requester honours it.
	//
	// Zero means the anchor stated no cap: an OLD anchor does not know
	// about it, and an old anchor is JSON-only — so the requester's leg
	// is JSON too and the requester's own default IS the JSON cap.
	// Nothing is lost.
	r.maxPayload = rec.MaxPayload

	// ③ the end-to-end handshake, THROUGH the trunk.
	//
	// The ORDER is: the initiator sends its hello FIRST, then reads the
	// target's hello.
	//
	// It has to be that way, and the reason is the session id: the
	// responder derives its session id from the initiator's, so the
	// responder cannot build its hello until it has seen the
	// initiator's. If the initiator waited for the target's hello first,
	// the two sides would deadlock — and, worse, the responder's hello
	// would carry a session id the initiator never chose.
	if err := r.sendHello(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := r.acceptHello(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// The handshake is over: the deadline comes off, so the relayed
	// session is bounded by the caller's own budget from here on.
	_ = conn.SetReadDeadline(time.Time{})
	return r, nil
}

// sendHello sends this end's end-to-end hello through the trunk.
func (r *relaySession) sendHello() error {
	own, err := r.e2e.Hello()
	if err != nil {
		return err
	}
	raw, err := EncodeE2EHello(own)
	if err != nil {
		return err
	}
	if err := writeRelayRecordAs(r.conn, relayRecord{
		Type:  uint8(wire.FrameMeshRelayHello),
		Hello: raw,
	}, r.format); err != nil {
		return fmt.Errorf("mesh: relay: send our hello: %w", err)
	}
	return nil
}

// acceptHello reads the target's end-to-end hello and verifies it.
//
// The target's hello is SIGNED, and the signature is verified against the
// member view's key for that device. That is what makes the handshake
// non-forgeable by the trunk: the trunk holds neither end's private key,
// so it cannot present its own ephemeral key as the target's.
func (r *relaySession) acceptHello() error {
	rec, err := r.rec.nextRecord()
	if err != nil {
		return fmt.Errorf("mesh: relay: read the target's hello: %w", err)
	}
	if rec.Type != uint8(wire.FrameMeshRelayHello) {
		return fmt.Errorf("mesh: relay: expected a hello record, got frame type %d", rec.Type)
	}
	hello, err := DecodeE2EHello(rec.Hello)
	if err != nil {
		return err
	}
	return r.e2e.AcceptHello(hello)
}

// relayRefusal turns the trunk's refusal into an error with the right
// CLASS.
//
// The class matters more than the text: the path layer decides from it
// whether another relay may be tried. A quota refusal is a LOCAL
// condition (this trunk is busy — another one may say yes), an auth
// refusal is a VERDICT (the answer will not change on another relay).
func relayRefusal(rec relayRecord) error {
	reason := rec.Reason
	if reason == "" {
		reason = "the relay refused the session"
	}
	switch rec.Class {
	case RelayClassAuth:
		return fmt.Errorf("%w: mesh: relay: %s", ErrAuth, reason)
	case RelayClassDisabled:
		return fmt.Errorf("%w: mesh: relay: %s", ErrRelayTrunkDisabled, reason)
	case RelayClassRefused:
		return fmt.Errorf("%w: mesh: relay: %s", ErrRefused, reason)
	case RelayClassQuota:
		return fmt.Errorf("%w: mesh: relay: %s", ErrQuota, reason)
	case RelayClassUnavailable:
		// A LOCAL condition, not a verdict: this trunk cannot reach the
		// target, but another relay may. The path layer must therefore
		// CONTINUE to the next candidate rather than stop here.
		return fmt.Errorf("%w: mesh: relay: %s", ErrRelayUnavailable, reason)
	default:
		return fmt.Errorf("%w: mesh: relay: %s", ErrRefused, reason)
	}
}

// ---------------------------------------------------------------------------
// The TRUNK's serving side.
//
// A member that wants to RECEIVE relayed sessions connects to the trunk
// and stays connected (the "join"). The trunk then dials THAT member when
// a relay needs a target — which is what makes a pure-outbound member
// reachable at all: it has no inbound port, so the anchor must come to
// it over the connection it opened.
//
// The trunk's serving side therefore has two jobs:
//
//   - Serve the JOINING member's connection: read its records, and when
//     a relay needs it as the target, open the flow on that connection.
//   - Serve the REQUESTER's connection: answer the open record, and
//     forward the records that come after it.
//
// Both are the same shape: read a record, decide, act.
// ---------------------------------------------------------------------------

// TrunkJoiner is a member that has connected to the trunk and is willing
// to receive relayed sessions.
//
// It is the trunk's handle on the connection: when a relay needs this
// member as the target, the trunk opens a FRESH STREAM on the joiner's
// connection and writes the relay's OPEN record onto it.
//
// A fresh stream per relay is not a detail — it is the reason two things
// work at all:
//
//   - The target's session handler and the join loop do not COMPETE for
//     the same bytes. Before, both read from one bufio.Reader, and the
//     join loop swallowed the end-to-end hello that belonged to the
//     session handler (defect C of the round-61 real-machine report).
//   - Ending one relay does not end the target's membership of the
//     trunk. The join connection outlives every flow on it (defect E of
//     the same report).
type TrunkJoiner struct {
	// Device is the joining member's device ID.
	Device identity.DeviceID
	// sess is the session the joiner is on.
	sess Session
	// br reads the records the joiner sends.
	br *bufio.Reader
	// open opens a FRESH session to this member, for ONE relayed
	// session.
	//
	// It is a seam rather than a field of concrete type so that the
	// trunk's data plane can be tested without a QUIC stack: the pipe
	// based tests supply their own.
	open func(ctx context.Context) (Session, error)
	// format is the record format of this joiner's leg.
	//
	// It comes from the ALPN the join connection negotiated, and every
	// stream the trunk opens on that connection speaks it — the ALPN is
	// a property of the CONNECTION, not of the stream.
	//
	// The zero value is the BINARY frame, which is the default and the
	// fast path.
	format RelaySpliceFormat
	// mu guards the write side.
	mu sync.Mutex
	// closed marks the joiner as gone.
	closed bool
}

// Open opens a fresh session to this member for one relayed session.
func (j *TrunkJoiner) Open(ctx context.Context) (Session, error) {
	j.mu.Lock()
	closed := j.closed
	j.mu.Unlock()
	if closed {
		return nil, errors.New("mesh: relay trunk: the joiner is gone")
	}
	if j.open == nil {
		return nil, errors.New("mesh: relay trunk: this joiner cannot carry another session")
	}
	return j.open(ctx)
}

// Send writes one record to the joiner.
func (j *TrunkJoiner) Send(rec relayRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return errors.New("mesh: relay trunk: the joiner is gone")
	}
	return writeRelayRecordAs(j.sess, rec, j.format)
}

// Close drops the joiner.
func (j *TrunkJoiner) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	return j.sess.Close()
}

// RelayJoiner opens a session to a member that has joined the trunk.
//
// It is the trunk's Dial function: it looks the member up in the joiners
// table and returns a session to it.
//
// The member must be a JOINER (not merely a member with endpoints): the
// trunk reaches a pure-outbound member over the connection that member
// opened, which is the whole point of the join.
type RelayJoiner interface {
	// Open opens a DEDICATED session to the member that has joined. The
	// session belongs to ONE relayed session: the trunk writes that
	// session's OPEN record onto it and splices the requester's stream
	// against it.
	Open(ctx context.Context, id identity.DeviceID) (Session, error)
}

// JoinerTable is the trunk's table of joined members.
//
// It is safe for concurrent use.
type JoinerTable struct {
	mu      sync.RWMutex
	joiners map[identity.DeviceID]*TrunkJoiner
}

// NewJoinerTable builds an empty table.
func NewJoinerTable() *JoinerTable {
	return &JoinerTable{joiners: make(map[identity.DeviceID]*TrunkJoiner)}
}

// Add records a joiner.
func (t *JoinerTable) Add(j *TrunkJoiner) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.joiners[j.Device] = j
}

// Remove drops a joiner.
//
// It removes the joiner ONLY IF THE TABLE STILL HOLDS THAT ONE.
//
// The table is keyed by device id, and a member can reconnect: the new
// connection registers itself, and then the OLD connection's cleanup runs
// and deletes the entry by id — taking the NEW joiner with it. The
// symptom is a member whose own connection is alive and healthy while
// the trunk has forgotten it, so every relay to it fails, and only an
// anchor restart brings it back.
//
// Measured on the real machine: the relay worked for a couple of
// seconds after the target reconnected, then broke permanently, and
// restarting the anchor restored it.
func (t *JoinerTable) Remove(j *TrunkJoiner) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, ok := t.joiners[j.Device]
	if !ok {
		return
	}
	// Only the SAME joiner may remove the entry. A newer connection has
	// already replaced it, and that one is the live one.
	if cur != j {
		return
	}
	delete(t.joiners, j.Device)
}

// Open opens a dedicated session to the member that has joined.
func (t *JoinerTable) Open(ctx context.Context, id identity.DeviceID) (Session, error) {
	t.mu.RLock()
	j, ok := t.joiners[id]
	t.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("mesh: relay trunk: %s has not joined this trunk", id)
	}
	return j.Open(ctx)
}

// Joiner returns the session to the member that has joined.
//
// It is the joiner's LONG-LIVED connection. It must not be spliced: the
// connection carries every relay to that member, so a splice on it would
// mix them together.
func (t *JoinerTable) Joiner(id identity.DeviceID) (Session, error) {
	t.mu.RLock()
	j, ok := t.joiners[id]
	t.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("mesh: relay trunk: %s has not joined this trunk", id)
	}
	return j.sess, nil
}

// Count reports how many members have joined.
func (t *JoinerTable) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.joiners)
}

// ServeJoiner serves one member's join connection.
//
// The member announces itself with a join record and then stays
// connected. The trunk keeps the connection for as long as it lives:
// when a relay needs this member as the target, the trunk opens a FRESH
// STREAM on it (see TrunkJoiner.Open) and writes that relay's records
// there.
//
// The connection ends when the member closes it or the trunk is stopped.
//
// The opener is the seam for opening a fresh stream towards the member.
// It may be nil: a joiner with no opener can still be served, it just
// cannot carry a relayed session — the trunk then refuses the relay with
// "this node cannot reach <target> right now" rather than splicing two
// relays' records together.
func (t *RelayTrunk) ServeJoiner(ctx context.Context, sess Session, table *JoinerTable) error {
	return t.ServeJoinerWithOpener(ctx, sess, table, nil)
}

// ServeJoinerWithOpener serves a join connection that can open fresh
// streams towards the member.
func (t *RelayTrunk) ServeJoinerWithOpener(ctx context.Context, sess Session, table *JoinerTable, open func(context.Context) (Session, error)) error {
	br := bufio.NewReader(sess)
	// The joiner's leg format comes from the ALPN its connection
	// negotiated.
	format, err := relayFormatOfSession(sess)
	if err != nil {
		return fmt.Errorf("mesh: relay trunk: the join connection speaks an unknown protocol: %w", err)
	}
	rec, err := readRelayRecordAs(br, format)
	if err != nil {
		return fmt.Errorf("mesh: relay trunk: read the join record: %w", err)
	}
	if rec.Type != uint8(wire.FrameMeshRelayJoin) {
		return fmt.Errorf("mesh: relay trunk: expected a join record, got frame type %d", rec.Type)
	}
	// The joiner must be a member — the same check the relay path makes.
	// A device that is not a member must not be able to occupy a slot
	// on the trunk.
	if _, err := t.members.KeyFor(rec.From); err != nil {
		return fmt.Errorf("%w: mesh: relay trunk: %s is not a member of this view", ErrAuth, rec.From)
	}
	joiner := &TrunkJoiner{Device: rec.From, sess: sess, br: br, open: open, format: format}
	table.Add(joiner)
	defer func() {
		table.Remove(joiner)
		_ = joiner.Close()
	}()

	// Stay connected until the member goes away. The joiner is torn down
	// by the defer above.
	//
	// A CLEAN end (the member disconnected) and a BROKEN stream (a
	// record that promised bytes that never came) land here the same
	// way — the joiner goes either way — but they must not be equally
	// silent: a broken frame is a member (or a path) corrupting records,
	// and an operator needs the reason in the log.
	for {
		if _, err := readRelayRecordAs(br, format); err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(relayLogWriter(), "mesh: relay trunk: the join connection from %s broke: %v\n", rec.From, err)
			}
			return nil
		}
	}
}

// ServeRequester serves one requester's connection.
//
// The sequence:
//
//  1. The open record arrives. The trunk decides (the SAME state machine
//     as the control plane) and answers.
//  2. On a grant, the trunk opens the flow to the target and forwards
//     the rest of the records.
//
// The forwarding is where the trunk's opacity matters: from here on, the
// records are forwarded VERBATIM. The trunk does not parse the payload,
// does not hold a key, and cannot read the traffic.
//
// The identity PIN for the requester's open record lives in the LISTENER's
// dispatch (serveRequester, next to the join path's identical check), not
// here: the listener is the only place that knows who the connection's
// certificate belongs to. Tests use ServeRequester.
func (t *RelayTrunk) ServeRequester(ctx context.Context, sess Session, dialer RelayJoiner) error {
	br := bufio.NewReader(sess)
	return t.ServeRequesterWithFirst(ctx, sess, dialer, br, nil)
}

// ServeRequesterWithFirst serves one requester's connection, taking the
// first record from the reader.
//
// The `first` parameter is the record that was already read, if the
// caller had to peek at it (the listener peeks to tell a joiner from a
// requester). Passing nil means "read it here".
//
// IT BLOCKS FOR THE WHOLE RELAY. That is the point: the caller is the
// listener, and the listener's contract is "this function owns the
// connection until it is done with it". Returning as soon as the grant
// was written closed the requester's QUIC connection while the relay was
// still carrying its bytes, so the requester never even got to read the
// grant (defect A of the round-61 real-machine report).
//
// The record format is ASKED of the leg, not assumed. This function used
// to hardcode the binary frame — and a leg that had negotiated the old
// protocol was then read as binary while it wrote JSON: the reader found
// text where a length prefix belongs. That is the same shape as the
// round-81 defect (a relayed session could not report its own format,
// so the question came back empty and the caller fell back to the
// binary frame — the most dangerous of the two defaults).
//
// So the format comes from the ALPN the leg negotiated, exactly as it
// does on the production path. A leg that cannot say (the test pipes)
// gets the binary frame, which is the default the same-version
// deployment wants.
func (t *RelayTrunk) ServeRequesterWithFirst(ctx context.Context, sess Session, dialer RelayJoiner, br *bufio.Reader, first *relayRecord) error {
	format, err := relayFormatOfSession(sess)
	if err != nil {
		return fmt.Errorf("mesh: relay trunk: the requester's leg format: %w", err)
	}
	return t.ServeRequesterWithFirstFormat(ctx, sess, dialer, br, first, format)
}

// ServeRequesterWithFirstFormat serves one requester's connection with
// an EXPLICIT record format for this leg.
//
// The format is the requester's leg's format, and it comes from the ALPN
// the requester's connection negotiated. It is the whole version
// negotiation: a requester that reached an OLD anchor negotiated the old
// ALPN, so it speaks JSON, and the anchor must speak JSON back to it —
// including the ANSWER record, which is the one record the requester
// cannot do without.
func (t *RelayTrunk) ServeRequesterWithFirstFormat(ctx context.Context, sess Session, dialer RelayJoiner, br *bufio.Reader, first *relayRecord, format RelaySpliceFormat) error {
	var rec relayRecord
	var err error
	if first != nil {
		rec = *first
	} else {
		rec, err = readRelayRecordAs(br, format)
		if err != nil {
			return fmt.Errorf("mesh: relay trunk: read the open record: %w", err)
		}
	}
	if rec.Type != uint8(wire.FrameMeshRelayOpen) {
		return fmt.Errorf("mesh: relay trunk: expected an open record, got frame type %d", rec.Type)
	}
	open := RelayOpen{From: rec.From, To: rec.To, Hops: rec.Hops, Kind: rec.Kind}

	// The SAME decision the control plane makes. The order inside Decide
	// is the security property, and it is not repeated here.
	ans, verdict := t.Decide(open)
	if !ans.Grant {
		// THE ANCHOR LOGS THE REFUSAL TOO.
		//
		// The requester learns WHY (the answer carries the reason and
		// the class), but until now the ANCHOR said nothing: a relay
		// refused at 3am left no trace on the node that refused it.
		//
		// The operator's only clue was the requester's error message
		// on a different machine — which is exactly the information the
		// trunk exists to centralise, thrown away.
		//
		// The reason is the package's OWN fixed string (the same one
		// the requester receives), so no peer text reaches the log.
		fmt.Fprintf(relayLogWriter(),
			"mesh: relay trunk: the relay from %s to %s was refused (%s): %s\n",
			open.From, open.To, ans.Class, ans.Reason)
		_ = writeRelayRecordAs(sess, relayRecord{
			Type:   uint8(wire.FrameMeshRelayAnswer),
			Grant:  false,
			Reason: ans.Reason,
			Class:  ans.Class,
		}, format)
		return verdict
	}

	// The quota is checked AFTER the authorization: a quota refusal is a
	// local condition, and the requester must be able to try another
	// relay.
	//
	// The RATE budget is checked here too, on the path that actually
	// grants relays. It used to live in OpenFlow — which has no
	// production caller — so a node whose forwarding budget was spent
	// still GRANTED new relays, and a saturated node granted work it was
	// about to choke on. The recorded decision is the opposite: when the
	// node is saturated it refuses NEW requests with the quota class (a
	// LOCAL condition), so the requester tries another relay and the
	// running transfers keep the node's remaining budget.
	if err := t.reserveStream(); err != nil {
		// The anchor logs the quota refusal too — same reasoning as the
		// authorization refusal above: a node that says "I am full"
		// must be visible in the node's own log, not only in the
		// requester's.
		fmt.Fprintf(relayLogWriter(),
			"mesh: relay trunk: the relay from %s to %s was refused (%s): this node is carrying as many relays as it may\n",
			open.From, open.To, RelayClassQuota)
		_ = writeRelayRecordAs(sess, relayRecord{
			Type:   uint8(wire.FrameMeshRelayAnswer),
			Grant:  false,
			Reason: "this node is carrying as many relays as it may",
			Class:  RelayClassQuota,
		}, format)
		return err
	}
	if t.overRateQuota() {
		t.releaseStream()
		_ = writeRelayRecordAs(sess, relayRecord{
			Type:   uint8(wire.FrameMeshRelayAnswer),
			Grant:  false,
			Reason: "this node's forwarding budget is spent; try another relay",
			Class:  RelayClassQuota,
		}, format)
		// The requester's session ends with the refusal: without this
		// the requester reads the answer and then waits for the idle
		// timeout (10 s) before it learns the relay is done.
		_ = sess.Close()
		return fmt.Errorf("%w: mesh: relay trunk: the forwarding budget is spent", ErrQuota)
	}

	// Open a DEDICATED session to the target.
	//
	// Dedicated, not the target's join connection: the join connection
	// carries every relay to that target, and the records of two relays
	// must never interleave. It is also what makes ending one relay not
	// end the target's membership of the trunk (defect E of the
	// round-61 real-machine report).
	tgt, err := t.dialTargetWith(ctx, open.To, open.Kind, dialer)
	if err != nil {
		t.releaseStream()
		// The CAUSE goes to the requester.
		//
		// The bare "this node cannot reach <target> right now" was the
		// worst kind of message: it is true, and it says nothing. The
		// real reason is usually something an operator can act on — the
		// member's certificate lapsed, the join connection is not up yet,
		// the target has no endpoints. The requester is already known to
		// be a member (the authorization ran before this point), so
		// telling it the cause leaks nothing to a stranger.
		_ = writeRelayRecordAs(sess, relayRecord{
			Type:   uint8(wire.FrameMeshRelayAnswer),
			Grant:  false,
			Reason: fmt.Sprintf("this node cannot reach %s right now: %v", open.To, err),
			// A LOCAL condition: this trunk cannot reach the target, but
			// another relay may. It used to be RelayClassRefused, which
			// the path layer treats as a verdict — so the fallback chain
			// stopped here and the second relay was never tried.
			Class: RelayClassUnavailable,
		}, format)
		// The session ends with the refusal: the requester reads the
		// answer and then waits for the idle timeout otherwise.
		_ = sess.Close()
		return err
	}

	// The TARGET's leg format comes from the ALPN that leg's connection
	// negotiated — and it is INDEPENDENT of the requester's.
	//
	// This is the mixed-version case the switch has to survive: a new
	// requester on a new anchor (the requester's leg speaks the binary
	// frame) reaching an OLD target (the target's leg speaks JSON). The
	// two formats disagree, so the anchor has to TRANSLATE — and it says
	// so, once, in the log.
	tgtFormat, err := relayFormatOfSession(tgt)
	if err != nil {
		_ = tgt.Close()
		t.releaseStream()
		_ = writeRelayRecordAs(sess, relayRecord{
			Type:   uint8(wire.FrameMeshRelayAnswer),
			Grant:  false,
			Reason: fmt.Sprintf("this node cannot reach %s right now: %v", open.To, err),
			Class:  RelayClassUnavailable,
		}, format)
		_ = sess.Close()
		return err
	}

	// The OPEN record towards the TARGET. It is what tells the target
	// WHO is asking: the target resolves the requester's key from the
	// member view and verifies the requester's end-to-end hello against
	// it. Without this record the target has no idea whose handshake it
	// is looking at, and the handshake can never complete (defect C of
	// the round-61 real-machine report).
	//
	// IT ALSO CARRIES THE PAYLOAD CAP for the whole path: the target
	// answers through the relay, and on a mixed-version relay its record
	// has to fit on the REQUESTER's leg as well as on its own. The
	// target cannot know the requester's leg's format; the anchor dialled
	// both ends, so the anchor does.
	//
	// See maxRelayPayloadForLegs.
	//
	// It is written in the TARGET's format — the target is the one that
	// has to read it.
	capForTarget := maxRelayPayloadForLegs(tgtFormat, format)
	if err := writeRelayRecordAs(tgt, relayRecord{
		Type:       uint8(wire.FrameMeshRelayOpen),
		From:       open.From,
		To:         open.To,
		Kind:       open.Kind,
		Hops:       open.Hops,
		MaxPayload: capForTarget,
	}, tgtFormat); err != nil {
		_ = tgt.Close()
		t.releaseStream()
		_ = writeRelayRecordAs(sess, relayRecord{
			Type:   uint8(wire.FrameMeshRelayAnswer),
			Grant:  false,
			Reason: fmt.Sprintf("this node cannot reach %s right now: %v", open.To, err),
			// Same LOCAL condition as the dial failure above, and the
			// same class: another relay may reach the target. This used
			// to be RelayClassRefused — a verdict — so a target stream
			// that broke between the dial and the first write stopped the
			// multi-relay fallback chain exactly the way the dial failure
			// once did.
			Class: RelayClassUnavailable,
		}, format)
		// The session ends with the refusal: the requester reads the
		// answer and then waits for the idle timeout otherwise.
		_ = sess.Close()
		return fmt.Errorf("mesh: relay trunk: tell %s who is calling: %w", open.To, err)
	}

	// The flow is registered so the quota counts it and the revocation
	// watch can tear it down.
	t.mu.Lock()
	t.nextFlow++
	id := t.nextFlow
	flow := &relayFlow{
		id:        id,
		from:      open.From,
		to:        open.To,
		req:       sess,
		tgt:       tgt,
		reqBr:     br,
		tgtBr:     bufio.NewReader(tgt),
		reqFormat: format,
		tgtFormat: tgtFormat,
		done:      make(chan struct{}),
	}
	t.flows[id] = flow
	t.mu.Unlock()
	// NOTE: the flow is NOT closed by a defer here. The splice owns it
	// from this point on: it ends when both sides are done, and it frees
	// the slot then. A defer would close the target's stream the moment
	// this function returned — before the handshake had even crossed.

	// The grant.
	//
	// IT CARRIES THE PAYLOAD CAP for the whole path.
	//
	// The requester writes its records on ITS leg, and on a mixed-version
	// relay the anchor has to re-encode them for the TARGET's leg — and
	// the JSON envelope is a third bigger than the binary frame. So a
	// record the requester was allowed to write on its own leg may not
	// fit on the target's.
	//
	// The requester cannot know the target's leg's format; the anchor
	// dialled both ends, so the anchor does. See
	// maxRelayPayloadForLegs.
	capForRequester := maxRelayPayloadForLegs(format, tgtFormat)
	if err := writeRelayRecordAs(sess, relayRecord{
		Type:       uint8(wire.FrameMeshRelayAnswer),
		Grant:      true,
		Session:    id,
		MaxPayload: capForRequester,
	}, format); err != nil {
		t.closeFlowForced(id, "the grant could not be written")
		return err
	}

	// From here on the trunk is a BYTE PIPE. The two end-to-end hellos
	// cross through it, and so does the traffic: the trunk never ends
	// the end-to-end protocol, it only carries the bytes.
	//
	// The records are self-delimiting (length-prefixed), so a blind copy
	// cannot mix two records up — the receiving side finds the record
	// boundaries on its own.
	go t.splice(flow)

	// Wait for the relay to finish. The caller's connection must stay
	// open for exactly as long as the relay carries bytes: the requester
	// is still reading records (its hello, its payload, the close) on
	// this very session. Returning here would close it underneath them.
	select {
	case <-flow.done:
		return nil
	case <-ctx.Done():
		t.closeFlowForced(id, "the trunk's context ended")
		return ctx.Err()
	}
}

// relayTargetSession is the TARGET's view of a relayed session.
//
// It mirrors relaySession (the requester's view): the business layer
// writes plain bytes and reads plain bytes, and the sealing happens
// underneath.
//
// The difference is the direction of the roles: the target is the
// RESPONDER of the end-to-end handshake, so it opens what the requester
// sealed and seals what it sends back.
type relayTargetSession struct {
	// conn is the session to the trunk. The records ride it.
	conn Session
	// e2e is the end-to-end session with the requester.
	e2e *E2ESession

	// rec reads the records the requester sends through the trunk.
	rec relayRecordReader
	// format is the record format of this leg.
	//
	// It comes from the ALPN the connection to the trunk negotiated, and
	// it is the TARGET's half of the switch: the target's per-record
	// cost was measured at 1345 µs — co-equal with the anchor's — so a
	// target that still spoke JSON would have kept half the relay's
	// total CPU on the JSON path.
	format RelaySpliceFormat
	// maxPayload is the cap the ANCHOR stated in the OPEN record.
	//
	// It is the largest plaintext that survives the WHOLE path of the
	// relay: the anchor knows the requester's leg's format too, and on a
	// mixed-version relay the record the target writes has to fit on the
	// requester's leg as well as on this one. Zero means the anchor
	// stated no cap, and the target uses its own leg's default.
	//
	// See relayRecord.MaxPayload and maxRelayPayloadForLegs.
	maxPayload uint64
	// wmu guards the write side.
	wmu sync.Mutex
	// buf holds the plaintext of the record currently being read out.
	buf []byte
	// eof marks the end of the stream.
	eof bool
	// closed marks the session as finished.
	closed bool
}

// Write seals the plaintext and sends it as one relay record.
func (t *relayTargetSession) Write(p []byte) (int, error) {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if t.closed {
		return 0, errors.New("mesh: relay target: the session is closed")
	}
	n, err := sealAndWriteRelayDataAs(t.conn, t.e2e, p, t.format, t.maxPayload)
	if err != nil {
		return n, fmt.Errorf("mesh: relay target: write: %w", err)
	}
	return n, nil
}

// Read returns the plaintext of the next record.
func (t *relayTargetSession) Read(p []byte) (int, error) {
	if len(t.buf) == 0 {
		if t.eof {
			return 0, io.EOF
		}
		rec, err := t.rec.nextRecord()
		if err != nil {
			t.eof = true
			return 0, err
		}
		switch rec.Type {
		case uint8(wire.FrameMeshRelayData):
			plain, err := t.e2e.Open(rec.Data)
			if err != nil {
				return 0, err
			}
			t.buf = plain
		case uint8(wire.FrameMeshRelayClose):
			t.eof = true
			return 0, io.EOF
		default:
			return 0, fmt.Errorf("mesh: relay target: unexpected record type %d", rec.Type)
		}
	}
	n := copy(p, t.buf)
	t.buf = t.buf[n:]
	return n, nil
}

// Close ends the send side of the relayed session.
func (t *relayTargetSession) Close() error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if err := writeRelayRecordAs(t.conn, relayRecord{
		Type:   uint8(wire.FrameMeshRelayClose),
		Close:  true,
		Reason: "the target closed the session",
	}, t.format); err != nil {
		_ = t.conn.Close()
		return err
	}
	return t.conn.Close()
}

func (t *relayTargetSession) SetReadDeadline(ts time.Time) error {
	return t.conn.SetReadDeadline(ts)
}

func (t *relayTargetSession) SetWriteDeadline(ts time.Time) error {
	return t.conn.SetWriteDeadline(ts)
}

// ALPN reports the record format this leg speaks.
//
// The target's leg format comes from the ALPN the connection to the
// trunk negotiated (see ServeRelayTarget), so the target can say it —
// and saying it closes the last hole in the "ask the leg" question.
//
// Without this method the question comes back EMPTY, and the caller
// falls back to the binary frame — the most dangerous of the two
// defaults. That is the same shape as the round-81 defect (a relayed
// session could not report its own format, so a leg that had
// negotiated the old protocol was read as binary while it wrote JSON,
// and the reader found text where a length prefix belongs).
//
// So every type that can be handed back as a Session says its format.
// The capability is asked for by type assertion, and a session that
// cannot say (the test pipes) falls back to the binary frame.
func (t *relayTargetSession) ALPN() string {
	switch t.format {
	case RelaySpliceFormatJSON:
		return RelayALPN
	default:
		return RelayALPN2
	}
}

// Abort ends the target's side of the relayed session on both sides.
//
// The half close (Close) is the polite end of the send direction. This is
// the hard end, and it is what a teardown needs: the trunk must be able
// to cut the target's session off, not merely ask it to stop.
func (t *relayTargetSession) Abort() error {
	t.wmu.Lock()
	t.closed = true
	t.wmu.Unlock()
	return abortSession(t.conn)
}

// relayFormatReader is the OPTIONAL way to ask a record reader which
// format it reads.
//
// It mirrors the other optional capabilities in this package: the
// capability is asked for by type assertion, and a reader that does not
// have it is not broken.
type relayFormatReader interface {
	// legFormat reports the record format this reader reads.
	legFormat() RelaySpliceFormat
}

// ServeRelayTarget performs the TARGET's half of the end-to-end
// handshake and returns the session the business layer should serve.
//
// The sequence:
//
//  1. The target sends its end-to-end hello (signed, so the requester
//     can verify it against the member view).
//  2. The target reads the requester's hello and verifies it the same
//     way.
//  3. The target gets a Session — plain bytes in and out — which is
//     what the business handler consumes.
//
// The target holds the end-to-end key. The trunk does not — that is the
// whole point of the layer.
//
// THE RECORD FORMAT is the target's leg's format, and it comes from the
// ALPN the connection to the trunk negotiated. The reader and the writer
// must agree on it — the trunk wrote the OPEN record in that format, so
// a reader in another format would fail on the first record.
//
// A disagreement is a LOUD failure, not a guess: the target refuses the
// session rather than misreading the records.
func ServeRelayTarget(sess Session, src relayRecordReader, cfg RelayTargetConfig) (Session, error) {
	if cfg.Device == nil {
		return nil, errors.New("mesh: relay target: needs a device identity")
	}
	if len(cfg.RequesterKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("mesh: relay target: the requester's key is malformed (%d bytes)", len(cfg.RequesterKey))
	}
	format, err := relayFormatOfSession(sess)
	if err != nil {
		return nil, fmt.Errorf("mesh: relay target: the trunk speaks an unknown protocol: %w", err)
	}
	if r, ok := src.(relayFormatReader); ok {
		if got := r.legFormat(); got != format {
			return nil, fmt.Errorf("mesh: relay target: the record reader speaks %s but the connection "+
				"negotiated %s; the records on this leg cannot be read", got, format)
		}
	}
	e2e, err := NewE2ESession(E2EConfig{
		Self:    cfg.Device,
		Peer:    cfg.Requester,
		PeerKey: cfg.RequesterKey,
		Role:    E2ERoleResponder,
	})
	if err != nil {
		return nil, err
	}
	// ① the REQUESTER's hello comes first.
	//
	// The order is forced by the session id: the responder adopts the
	// initiator's session id, so it cannot build its own hello until it
	// has seen the initiator's. Reading first is therefore not a
	// convention — it is a requirement of the derivation.
	rec, err := src.nextRecord()
	if err != nil {
		return nil, fmt.Errorf("mesh: relay target: read the requester's hello: %w", err)
	}
	if rec.Type != uint8(wire.FrameMeshRelayHello) {
		return nil, fmt.Errorf("mesh: relay target: expected a hello record, got frame type %d", rec.Type)
	}
	peerHello, err := DecodeE2EHello(rec.Hello)
	if err != nil {
		return nil, err
	}
	if err := e2e.AcceptHello(peerHello); err != nil {
		return nil, err
	}
	// ② our hello, now that the session id is settled.
	hello, err := e2e.Hello()
	if err != nil {
		return nil, err
	}
	raw, err := EncodeE2EHello(hello)
	if err != nil {
		return nil, err
	}
	if err := writeRelayRecordAs(sess, relayRecord{
		Type:  uint8(wire.FrameMeshRelayHello),
		Hello: raw,
	}, format); err != nil {
		return nil, fmt.Errorf("mesh: relay target: send our hello: %w", err)
	}
	// ③ the business layer gets a plain-bytes session.
	return &relayTargetSession{conn: sess, e2e: e2e, rec: src, format: format, maxPayload: cfg.MaxPayload}, nil
}

// RelayTargetConfig configures the target's end of a relayed session.
type RelayTargetConfig struct {
	// Device is the target's identity.
	Device *identity.Device
	// Requester is the member that opened the relayed session.
	Requester identity.DeviceID
	// RequesterKey is the requester's public key, from the member view.
	// It verifies the requester's end-to-end hello signature.
	RequesterKey ed25519.PublicKey
	// MaxPayload is the cap the ANCHOR stated in the OPEN record: the
	// largest plaintext that survives the WHOLE path of the relay.
	//
	// It is zero when the anchor stated no cap — an OLD anchor does not
	// know about it — and the target then uses its own leg's default.
	//
	// The target is a writer: it answers through the relay, and on a
	// mixed-version relay the record it writes is re-encoded for the
	// REQUESTER's leg, which may be a JSON one — and a JSON envelope is a
	// third bigger than the binary frame. So the target has to write
	// less than its own leg would allow, and only the anchor knows that.
	//
	// See relayRecord.MaxPayload and maxRelayPayloadForLegs.
	MaxPayload uint64
}

// ---------------------------------------------------------------------------
// The RELAY LISTENER.
//
// The trunk needs a listener of its own: a member that wants to be
// relayed THROUGH connects to it and speaks the relay protocol. The
// direct-link listener cannot serve it, because the direct link's hello
// protocol has no room for "I want to be a relay target".
//
// The ALPN separates the two protocols: a direct-link client must not be
// able to land on the relay protocol, and a relay client must not be able
// to land on the direct link's kind dispatch. Separate ALPNs make the two
// protocols unreachable from each other.
//
// BUT THE ALPN DOES NOT NEED A SEPARATE PORT. ALPN is negotiated PER
// CONNECTION, so one UDP socket and one quic.Listener can serve both
// protocols: the listener advertises both ALPNs, and the negotiated
// protocol decides which one a connection speaks (D76). The old comment
// here said "one socket can serve only one ALPN" — that was WRONG, and
// it is why an operator had to open a SECOND UDP port in their security
// group for the trunk to work at all.
//
// A dedicated relay listener still exists for the deployments that WANT a
// separate port (--relay-port), and it is the DEFAULT-free path: the
// direct listener serves the relay protocol too when a trunk is
// configured on it.
// ---------------------------------------------------------------------------

// RelayALPN is the ALPN of the mesh relay protocol.
//
// It is SEPARATE from directALPN ("container-mesh-direct/1") on purpose:
// a peer that speaks the direct-link protocol must not be able to reach
// the relay protocol, and vice versa. The TLS handshake fails on an
// ALPN mismatch, so the isolation is enforced by the transport.
//
// The separation is by PROTOCOL, not by PORT: the same socket may offer
// both ALPNs, and each connection ends up on one of them (D76).
const RelayALPN = "container-mesh-relay/1"

// relayAdvertisedALPNs is the list every relay-protocol endpoint
// advertises.
//
// THE ORDER IS LOAD-BEARING, and it is the same list for the server and
// the client.
//
// Go's ALPN negotiation is SERVER-FIRST: it walks the SERVER's list in
// order and takes the first entry the client also offers (measured over
// crypto/tls in docs/design/RELAY-FRAME.md §4.3, and re-measured over
// QUIC in TestTheALPNNegotiationUnderQUICIsServerFirst). So:
//
//   - The ANCHOR (the server) must advertise the NEW ALPN FIRST, or it
//     negotiates the OLD protocol with a member that could have spoken
//     the new one — and the relay keeps running on JSON for ever.
//   - The MEMBER (the client) must advertise BOTH, because a member
//     that offers only the new ALPN cannot reach an OLD anchor at all:
//     the handshake fails with "no application protocol" (measured;
//     TestAClientThatOffersOnlyTheNewALPNCannotReachAnOldAnchor).
//
// The two rules together give a rolling upgrade that never breaks:
//
//	new member → new anchor   /2  binary  (the fast path)
//	new member → old anchor   /1  JSON    (the fallback)
//	old member → new anchor   /1  JSON    (the fallback)
//	old member → old anchor   /1  JSON
//
// And they give the DEPLOYMENT ORDER: the anchor is upgraded FIRST. An
// old anchor cannot read a binary record at all, so a new member on an
// old anchor dies at the first record — the one case the ALPN cannot
// save, because the old anchor offers only the old ALPN and the new
// member has to honour it.
//
// The list is the SAME for the server and the client on purpose: it is
// one fact about the protocol, stated once.
func relayAdvertisedALPNs() []string {
	return []string{RelayALPN2, RelayALPN}
}

// isRelayALPN reports whether an ALPN names the relay protocol.
//
// There are TWO of them now — the old one and the new one — and the
// listener's dispatch has to recognise both: a connection that negotiated
// either of them speaks the relay protocol.
//
// It is NOT enough to recognise only the new one: an old member that
// connected to a new anchor negotiated the OLD ALPN, and the anchor has
// to serve it on the relay path (the JSON fallback), not on the
// direct-link path.
func isRelayALPN(alpn string) bool {
	return alpn == RelayALPN || alpn == RelayALPN2
}

// relayFormatForALPN says which record format a connection that
// negotiated the given ALPN speaks.
//
// It is the bridge between the ALPN and the record format, and it is the
// ONLY place the two are related: the ALPN is the version negotiation,
// and the negotiated protocol names the record format.
//
// An ALPN this code does not know is a REFUSAL, not a guess: the
// negotiated protocol names a record format this node cannot speak, and
// guessing would put the node on a forwarding path the peer is not on.
func relayFormatForALPN(alpn string) (RelaySpliceFormat, error) {
	return RelaySpliceFormatForALPN(alpn)
}

// ALPN reports the application protocol the connection negotiated.
//
// It is an OPTIONAL capability, on purpose: the Session interface is a
// byte stream, and "which protocol version did this connection agree
// on" is a property of the CONNECTION, not of the stream. A transport
// that cannot answer it simply does not implement this method, and the
// caller falls back to the default format.
//
// The method is declared HERE rather than in session.go because the
// record format is what needs it: session.go knows about streams, and
// this is the first place the relay protocol's version negotiation
// reaches the transport.
func (s quicSession) ALPN() string {
	if s.conn == nil {
		return ""
	}
	return s.conn.ConnectionState().TLS.NegotiatedProtocol
}

// alpnSession is the OPTIONAL way to ask a session which protocol
// version its connection negotiated.
//
// It mirrors the shape of the other optional capabilities in this
// package (sessionAborter, lossReporter): the capability is asked for by
// type assertion, and a transport that does not have it is not broken —
// it simply falls back to the default.
type alpnSession interface {
	// ALPN reports the negotiated application protocol, or "" when the
	// session cannot say.
	ALPN() string
}

// sessionALPN reports the ALPN a session's connection negotiated, or ""
// when the session cannot say.
//
// A pipe (the test double) cannot say, and "" means "no information" —
// not "the old protocol". The caller treats it as the default format.
func sessionALPN(sess Session) string {
	if a, ok := sess.(alpnSession); ok {
		return a.ALPN()
	}
	return ""
}

// relayFormatOfSession reports the record format a session's leg speaks.
//
// The format comes from the ALPN the leg's connection negotiated, and
// the default is the BINARY frame — so a transport that cannot report
// its ALPN (the test pipes) gets the fast path, which is what a
// same-version deployment wants.
//
// An ALPN that is neither relay protocol is a REFUSAL: the connection
// negotiated something this node does not know how to speak, and guessing
// would put the relay on a forwarding path the peer is not on.
func relayFormatOfSession(sess Session) (RelaySpliceFormat, error) {
	alpn := sessionALPN(sess)
	if alpn == "" {
		return RelaySpliceFormatBinary, nil
	}
	return relayFormatForALPN(alpn)
}

// RelayListenerConfig configures the relay listener.
type RelayListenerConfig struct {
	// Device is the trunk's own identity. Its key IS the TLS
	// certificate.
	Device *identity.Device
	// Members resolves the peers' keys for certificate pinning. It
	// must be the LIVE view: a revoked device must not be able to
	// open a relay session.
	Members MemberResolver
	// Addrs are the UDP endpoints to bind (host:port, port 0 = auto).
	Addrs []string
	// Trunk is the relay trunk that serves the sessions. It may be nil
	// at construction time and installed later.
	Trunk *RelayTrunk
	// Joiners is the table of members that have joined this trunk.
	Joiners *JoinerTable
}

// RelayHandler serves relay-protocol connections.
//
// It is the part of the relay listener that does NOT depend on the
// socket: given a QUIC connection that negotiated the relay ALPN, it
// drives the protocol on it.
//
// It exists as its own type because a relay connection does not have to
// arrive on a socket of its own. ALPN is negotiated PER CONNECTION, so
// one UDP socket and one quic.Listener can serve BOTH the direct-link
// protocol and the relay protocol — the negotiated protocol decides which
// one a connection speaks (D76). The direct listener therefore hands
// its relay-ALPN connections to a RelayHandler, and the dedicated relay
// listener hands its connections to the same handler.
type RelayHandler struct {
	trunk   *RelayTrunk
	joiners *JoinerTable
}

// NewRelayHandler builds the handler that serves relay connections.
//
// The trunk and the joiner table may be nil at construction time and
// installed later — the handler reads them per connection, so an anchor
// that starts its trunk before its listener is still correct.
func NewRelayHandler(trunk *RelayTrunk, joiners *JoinerTable) *RelayHandler {
	if joiners == nil {
		joiners = NewJoinerTable()
	}
	return &RelayHandler{trunk: trunk, joiners: joiners}
}

// Joiners returns the handler's joiner table.
func (h *RelayHandler) Joiners() *JoinerTable {
	if h == nil {
		return nil
	}
	return h.joiners
}

// ServeConn drives one accepted relay connection.
//
// The peer's identity comes from its pinned device certificate (mTLS),
// the same trust anchor the direct path uses.
//
// The first record decides what the connection IS: a join record makes
// the peer a joiner (a member that is willing to RECEIVE relayed
// sessions), an open record makes it a requester (a member that wants a
// session carried).
//
// THE CONNECTION OUTLIVES THE FUNCTION for a REQUESTER. The relay it
// carries is still running when the grant was written, so the requester
// is still reading records on this very connection. Closing the
// connection when this function returns — which the old code did, with a
// `defer conn.CloseWithError(0, "bye")` — killed the relay underneath
// the requester, and the requester never even got to read the grant
// (defect A of the round-61 real-machine report).
//
// So the connection is closed by whoever OWNS the relay's lifetime: the
// flow does it when the relay ends. The connection is only closed here
// on the error paths, where no relay was started.
func (h *RelayHandler) ServeConn(conn *quic.Conn) {
	peerID := identity.DeviceID(conn.ConnectionState().TLS.PeerCertificates[0].Subject.CommonName)
	st, err := conn.AcceptStream(context.Background())
	if err != nil {
		_ = conn.CloseWithError(0, "bye")
		return
	}
	sess := quicSession{st: st, conn: conn}
	trunk := h.trunk
	if trunk == nil {
		trunk = InstalledRelayTrunk()
	}
	if trunk == nil {
		_ = st.Close()
		_ = conn.CloseWithError(0, "bye")
		return
	}
	// A TRUNK THAT IS STOPPING MUST NOT ADMIT ANY MORE CONNECTIONS.
	//
	// The order in a shutdown is: set the stopping flag, close the
	// listener, then tear down the live flows. A connection that arrives
	// in between would otherwise be adopted — and a flow adopted after
	// the teardown would keep its stream slot forever, because nothing
	// would close it.
	//
	// The same hole the stage-1 relay had, and the same fix: refuse at
	// the top of the handler, before anything is adopted.
	if trunk.isStopping() {
		_ = st.Close()
		_ = conn.CloseWithError(0, "this relay trunk is shutting down")
		return
	}
	// The REQUESTER's leg format comes from the ALPN this connection
	// negotiated. It is the whole version negotiation: the peer offered
	// [new, old] and the anchor took the first one it also offered, so
	// the negotiated protocol names the record format this leg speaks.
	reqFormat, err := relayFormatOfSession(sess)
	if err != nil {
		_ = st.Close()
		_ = conn.CloseWithError(0, "bye")
		return
	}
	// Peek at the first record to tell a joiner from a requester.
	br := bufio.NewReader(sess)
	rec, err := readRelayRecordAs(br, reqFormat)
	if err != nil {
		_ = st.Close()
		_ = conn.CloseWithError(0, "bye")
		return
	}
	switch rec.Type {
	case uint8(wire.FrameMeshRelayJoin):
		// The joiner announces ITSELF; the record carries its device
		// ID. It must match the certificate's CN — otherwise a member
		// could claim to be somebody else.
		if rec.From != peerID {
			_ = st.Close()
			_ = conn.CloseWithError(0, "bye")
			return
		}
		h.serveJoiner(trunk, conn, sess, br, peerID, reqFormat)
	case uint8(wire.FrameMeshRelayOpen):
		h.serveRequester(trunk, conn, sess, br, &rec, peerID, reqFormat)
	default:
		_ = st.Close()
		_ = conn.CloseWithError(0, "bye")
	}
}

// serveJoiner serves a member that has joined the trunk.
//
// The joiner's connection is LONG-LIVED: it carries every relay the
// trunk sends to that member, one FRESH STREAM per relay. The opener
// below is how a new relay's stream is created.
func (h *RelayHandler) serveJoiner(trunk *RelayTrunk, conn *quic.Conn, sess Session, br *bufio.Reader, peerID identity.DeviceID, format RelaySpliceFormat) {
	open := func(ctx context.Context) (Session, error) {
		st, err := conn.OpenStreamSync(ctx)
		if err != nil {
			return nil, fmt.Errorf("mesh: relay trunk: open a stream to %s: %w", peerID, err)
		}
		return quicSession{st: st, conn: conn}, nil
	}
	// The joiner carries the record format of its leg. It is the format
	// the JOIN connection negotiated, and every stream the trunk opens
	// on that connection speaks it — the ALPN is a property of the
	// CONNECTION, not of the stream.
	joiner := &TrunkJoiner{Device: peerID, sess: sess, br: br, open: open, format: format}
	h.joiners.Add(joiner)
	defer func() {
		h.joiners.Remove(joiner)
		_ = joiner.Close()
	}()
	// The joiner stays connected until it goes away. The trunk reaches
	// it over this session when a relay needs it as the target. A broken
	// stream is not the same event as a clean disconnect and must not be
	// equally silent (see the join loop in ServeJoinerWithOpener).
	for {
		if _, err := readRelayRecordAs(br, format); err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(relayLogWriter(), "mesh: relay trunk: the join connection from %s broke: %v\n", peerID, err)
			}
			return
		}
	}
}

// serveRequester serves a member that wants a session carried.
//
// The open record was already read (the listener peeks at it to tell a
// joiner from a requester), so it is handed over rather than re-read.
//
// The record's From is PINNED to the connection's certificate first — the
// same pin the join path applies in the dispatch switch above. The open
// record's From field is attacker-controlled bytes: without the pin, any
// member could ask this trunk to carry a relay "on behalf of" a different
// member, and the trunk would re-check THAT member's authorization and
// touch the target's identity for a request its real device never made.
// The end-to-end handshake would still fail (the trunk holds no key), so
// this is unauthorized work and a third-party identity lookup rather than
// impersonation — but "the requester is checked first" is only a security
// property if the requester is the connection.
//
// The connection is NOT closed here: the relay is still running. The
// flow's own teardown closes the requester's session, and this
// connection goes with it.
func (h *RelayHandler) serveRequester(trunk *RelayTrunk, conn *quic.Conn, sess Session, br *bufio.Reader, first *relayRecord, peerID identity.DeviceID, format RelaySpliceFormat) {
	// The pin is UNCONDITIONAL on this path: if the caller had to peek at
	// the record to tell a joiner from a requester, that record is what
	// gets pinned; if it did not, read the record HERE and pin it. A nil
	// first used to skip the check silently — the record would then be
	// served past the pin without anyone vouching for it.
	if first == nil {
		rec, err := readRelayRecordAs(br, format)
		if err != nil {
			_ = sess.Close()
			return
		}
		first = &rec
	}
	if first.From != peerID {
		_ = writeRelayRecordAs(sess, relayRecord{
			Type:   uint8(wire.FrameMeshRelayAnswer),
			Grant:  false,
			Reason: "the open record does not name this connection's device",
			Class:  RelayClassAuth,
		}, format)
		// The SESSION closes, but the CONNECTION does not.
		//
		// Closing the connection right after writing the answer throws
		// the answer away: quic-go discards the in-flight record, and
		// the requester reads a transport error instead of the refusal.
		// The refusal's CLASS is the whole point of the pin — it is what
		// tells the path layer "this is an authentication verdict, stop
		// here" rather than "the network failed, try another relay".
		//
		// The session's close is enough: the requester reads the
		// refusal and then sees the end of the stream, and the
		// connection's own idle timeout reclaims it.
		_ = sess.Close()
		return
	}
	_ = trunk.ServeRequesterWithFirstFormat(context.Background(), sess, h.joiners, br, first, format)
}

// RelayListener accepts relay-protocol connections.
type RelayListener struct {
	lns  []*quic.Listener
	cfg  RelayListenerConfig
	stop chan struct{}
}

// NewRelayListener binds the relay listener's endpoints.
func NewRelayListener(ctx context.Context, cfg RelayListenerConfig) (*RelayListener, error) {
	if cfg.Device == nil || cfg.Members == nil {
		return nil, errors.New("mesh: relay listener needs a device and a member resolver")
	}
	if len(cfg.Addrs) == 0 {
		return nil, errors.New("mesh: relay listener needs at least one listen address")
	}
	if cfg.Joiners == nil {
		cfg.Joiners = NewJoinerTable()
	}
	cert, err := cfg.Device.TLSCertificate()
	if err != nil {
		return nil, err
	}
	pin := func(id identity.DeviceID, pub ed25519.PublicKey) error {
		want, err := cfg.Members.KeyFor(id)
		if err != nil {
			return err
		}
		if !ed25519KeysEqual(pub, want) {
			return fmt.Errorf("mesh: %s certificate key does not match the member view", id)
		}
		return nil
	}
	tlsCfg, err := deviceTLS(&cert, pin)
	if err != nil {
		return nil, err
	}
	// The ALPN is what separates the relay protocol from the direct
	// link. A peer that offers the wrong one fails the handshake.
	//
	// The relay protocol's own list is [NEW, OLD] — see
	// relayAdvertisedALPNs for why the ORDER is load-bearing.
	tlsCfg.NextProtos = relayAdvertisedALPNs()

	l := &RelayListener{cfg: cfg, stop: make(chan struct{})}
	for _, a := range cfg.Addrs {
		udpAddr, err := net.ResolveUDPAddr("udp", a)
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("mesh: relay listener: bad listen address %q: %w", a, err)
		}
		conn, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("mesh: relay listener: cannot bind %s (address in use?): %w", a, err)
		}
		ln, err := quic.Listen(conn, tlsCfg, quicConfig())
		if err != nil {
			_ = conn.Close()
			l.Close()
			return nil, fmt.Errorf("mesh: relay listener: quic listen on %s: %w", a, err)
		}
		l.lns = append(l.lns, ln)
	}
	for _, ln := range l.lns {
		go l.acceptLoop(ln)
	}
	return l, nil
}

// BoundAddrs reports the resolved local endpoints.
func (l *RelayListener) BoundAddrs() []string {
	out := make([]string, 0, len(l.lns))
	for _, ln := range l.lns {
		out = append(out, ln.Addr().String())
	}
	return out
}

// Close shuts the relay listener down.
func (l *RelayListener) Close() {
	if l == nil {
		return
	}
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	for _, ln := range l.lns {
		_ = ln.Close()
	}
}

// acceptLoop accepts relay connections until the listener closes.
func (l *RelayListener) acceptLoop(ln *quic.Listener) {
	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			select {
			case <-l.stop:
				return
			default:
			}
			return
		}
		go l.handle(conn)
	}
}

// handle drives one accepted relay connection.
//
// It delegates to the shared RelayHandler: the same handler serves the
// relay connections that arrive on the DIRECT listener's socket (when
// they negotiated the relay ALPN), so there is exactly one
// implementation of the relay protocol.
func (l *RelayListener) handle(conn *quic.Conn) {
	l.handler().ServeConn(conn)
}

// handler returns the listener's relay handler.
func (l *RelayListener) handler() *RelayHandler {
	return NewRelayHandler(l.cfg.Trunk, l.cfg.Joiners)
}

// ---------------------------------------------------------------------------
// The JOINER's side.
//
// A member that wants to RECEIVE relayed sessions connects to the trunk
// and stays connected. The trunk then reaches that member over the
// connection the member opened.
//
// This is what makes a PURE-OUTBOUND member (a container behind NAT, with
// no inbound port at all) reachable through the trunk: the member dials
// the anchor — which it can always do, since it can reach the discovery
// node — and the anchor comes to it over that connection.
// ---------------------------------------------------------------------------

// RelayJoinConfig configures the joiner's side of the trunk.
type RelayJoinConfig struct {
	// Device is the joining member's identity.
	Device *identity.Device
	// Resolver resolves members' keys from the LIVE view.
	Resolver MemberResolver
	// Trunk is the trunk's device ID. The dial pins against it.
	Trunk identity.DeviceID
	// Endpoints are the trunk's relay endpoints.
	Endpoints []string
	// OnSession is called when the trunk routes a relayed session to
	// this member. It receives the session's Session (plain bytes in
	// and out), the requester's device ID, and the session KIND.
	//
	// The kind matters: the target's business layer dispatches on it
	// (ping, msg, file, …), and it arrives in the OPEN record the
	// trunk wrote. A target that ignored it would serve every relayed
	// session with the same handler — a ping answered by the message
	// handler, for instance.
	//
	// It is a callback rather than an inline handler because the
	// member's business layer (the messages, the files, the pings)
	// lives in the direct path's session dispatch, and it must be able
	// to serve a RELAYED session the same way.
	OnSession func(sess Session, requester identity.DeviceID, kind string) error
}

// reportRelaySessionFailure writes the LOCAL account of a failed
// relayed session.
//
// It exists as a named function so the behaviour is testable on its
// own: the chain-level path (a full relayed session with an end-to-end
// handshake) has too many moving parts for a reliable regression test
// — a failure in the handshake reads as "no report" and looks exactly
// like the defect this guards.
//
// A nil error means the session SUCCEEDED, and then nothing is printed:
// a member that relays hundreds of sessions must not produce a line per
// session.
// relayLog is where the trunk's own diagnostics go.
//
// It is held in an atomic so that a test can REPLACE it without a data
// race. The obvious version — a plain package-level variable that the
// test assigns — races with the background goroutines that write to it
// (the join loops), and the race detector caught exactly that: the
// -race run of this package failed intermittently, with the victim
// rotating between whichever test happened to be running when the race
// fired.
//
// With an atomic, a reader always sees either the old sink or the new
// one, never a torn one, and the assignment itself is a synchronising
// operation.
//
// Production behaviour is unchanged: the sink IS os.Stderr.
var relayLog atomic.Value // io.Writer

func init() { relayLog.Store(io.Writer(os.Stderr)) }

// relayLogWriter returns the current diagnostics sink.
func relayLogWriter() io.Writer {
	w, _ := relayLog.Load().(io.Writer)
	if w == nil {
		return os.Stderr
	}
	return w
}

func reportRelaySessionFailure(requester, kind string, err error) {
	line := relaySessionFailureLine(requester, kind, err)
	if line == "" {
		return
	}
	fmt.Fprint(relayLogWriter(), line)
}

// relaySessionFailureLine is the report's TEXT, separated from where it
// is written.
//
// The separation is what makes the report testable without a data
// race: capturing os.Stderr (or any package-level sink) by swapping it
// races with the background goroutines that write to it — the race
// detector caught exactly that, and the -race run of this package failed
// intermittently with the victim rotating between tests.
//
// So the text is produced by a pure function and the caller decides
// where it goes. The test asserts the TEXT; production writes it to
// stderr.
func relaySessionFailureLine(requester, kind string, err error) string {
	if err == nil {
		return ""
	}
	// The reason is CLASSIFIED, not quoted.
	//
	// The handler's error carries the data it handled — the same text
	// writeMsgFailure sends to the peer — and this line goes to the
	// trunk's own log, which is a different trust domain from the peer's
	// stream. Quoting the error verbatim would carry the payload across
	// that boundary: a message body ending up in the anchor's log file.
	//
	// So the report says WHAT KIND of failure it was, and the peer
	// still receives the detailed reason (the handler's own job).
	return fmt.Sprintf("mesh: relay trunk: the session from %s (%s) failed (%T)\n",
		requester, kind, err)
}

// JoinRelayTrunk connects a member to a relay trunk and stays connected.
//
// The connection is re-established if it drops: the anchor is the one
// node in the network this member can always reach, so it is the natural
// place to reconnect to.
//
// IT DOES NOT RETRY BY ITSELF. The old version looped forever inside this
// function, so the caller's error handling — "relay trunk join failed
// (%v); retrying" — was DEAD CODE and a member that could never join
// failed SILENTLY (defect G of the round-61 real-machine report). The
// retry is the caller's job: it has the log line, and it knows whether a
// failure is worth retrying.
func JoinRelayTrunk(ctx context.Context, cfg RelayJoinConfig) error {
	if cfg.Device == nil || cfg.Resolver == nil {
		return errors.New("mesh: relay join needs a device and a member resolver")
	}
	if cfg.Trunk == "" {
		return errors.New("mesh: relay join needs the trunk's device id")
	}
	if len(cfg.Endpoints) == 0 {
		return fmt.Errorf("%w: mesh: no relay trunk endpoint to dial", ErrNoRoute)
	}
	return joinRelayTrunkOnce(ctx, cfg)
}

// joinRelayTrunkOnce makes one connection to the trunk and serves it.
//
// The connection carries EVERY relay the trunk sends to this member: the
// join record goes on the first stream, and each relayed session gets
// its OWN stream. That is what keeps the session handler and the
// dispatch loop from competing for the same bytes (defect C of the
// round-61 real-machine report).
func joinRelayTrunkOnce(ctx context.Context, cfg RelayJoinConfig) error {
	link, err := DialRelaySession(ctx, cfg.Device, cfg.Resolver, cfg.Trunk, cfg.Endpoints)
	if err != nil {
		return err
	}
	defer link.Close()
	sess := link.Session()

	// The joiner's leg format comes from the ALPN the connection
	// negotiated. It is the format of every stream the trunk opens on
	// this connection — the ALPN is a property of the CONNECTION.
	format, err := relayFormatOfSession(sess)
	if err != nil {
		return fmt.Errorf("mesh: relay join: the trunk speaks an unknown protocol: %w", err)
	}

	// The join record.
	if err := writeRelayRecordAs(sess, relayRecord{
		Type: uint8(wire.FrameMeshRelayJoin),
		From: cfg.Device.ID(),
	}, format); err != nil {
		return fmt.Errorf("mesh: relay join: send the join record: %w", err)
	}

	// The trunk routes relayed sessions to this connection, one FRESH
	// STREAM each. The first stream is the one the join record went on
	// (the trunk reads the join record from it); every later stream
	// starts with an OPEN record naming the requester.
	conn := link.qc
	if conn == nil {
		return errors.New("mesh: relay join: the relay connection cannot carry extra sessions")
	}
	go func() {
		for {
			st, err := conn.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go serveRelayedSession(quicSession{st: st, conn: conn}, cfg)
		}
	}()

	// The first stream: the join record was written on it, and it stays
	// open as the connection's keepalive. Nothing else is read from it.
	// A BROKEN stream (records that promised bytes that never came) is
	// not the same event as a clean disconnect; say which one happened.
	br := bufio.NewReader(sess)
	for {
		if _, err := readRelayRecordAs(br, format); err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(relayLogWriter(), "mesh: relay trunk: the join stream broke: %v\n", err)
			}
			return err
		}
	}
}

// serveRelayedSession serves one relayed session that the trunk opened
// with this member as the target.
//
// The requester's identity comes from the open record the trunk sends;
// the requester's key is resolved from the member view, and the
// requester's hello is verified against it.
//
// The record format is the JOIN CONNECTION's format — the ALPN is a
// property of the connection, and every stream the trunk opens on it
// speaks the same one.
func serveRelayedSession(sess Session, cfg RelayJoinConfig) {
	br := bufio.NewReader(sess)
	format, err := relayFormatOfSession(sess)
	if err != nil {
		reportRelaySessionFailure("", "", err)
		_ = sess.Close()
		return
	}
	open, err := readRelayRecordAs(br, format)
	if err != nil {
		// The failures BEFORE the handler are reported too: they are
		// precisely the "truncated or interrupted transfer invisible on
		// the receiving side" cases — and a TRUNCATED OPEN RECORD is
		// the most likely truncation in the protocol, because it is the
		// first thing on the wire.
		reportRelaySessionFailure("", "", err)
		_ = sess.Close()
		return
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		// Not a relay session: this stream is not ours. Drop it.
		reportRelaySessionFailure("", "", errors.New("the stream is not a relay session"))
		_ = sess.Close()
		return
	}
	// The requester must be a member — the same check the trunk made.
	// It is made AGAIN here, because the member view on the target may
	// be fresher than the one on the trunk.
	key, err := cfg.Resolver.KeyFor(open.From)
	if err != nil {
		reportRelaySessionFailure(string(open.From), open.Kind, err)
		_ = sess.Close()
		return
	}
	tsess, err := ServeRelayTarget(sess, newFramedRecordReader(br, format), RelayTargetConfig{
		Device:       cfg.Device,
		Requester:    open.From,
		RequesterKey: key,
		// The ANCHOR'S CAP, from the OPEN record. The target writes
		// through the relay too, and its record has to fit on the
		// REQUESTER's leg as well as on this one — and only the anchor
		// knows both legs' formats.
		MaxPayload: open.MaxPayload,
	})
	if err != nil {
		// The end-to-end handshake failed — the other end of the
		// session died or was not who it claimed to be.
		reportRelaySessionFailure(string(open.From), open.Kind, err)
		_ = sess.Close()
		return
	}
	if cfg.OnSession != nil {
		// The handler's error is the ONLY account of why the session
		// failed, and it used to be dropped on the floor. That made a
		// truncated or interrupted transfer invisible on the RECEIVING
		// side: the partial file was deleted and no file appeared, but
		// nothing said why — the reason was written to the PEER (which
		// may be the one that died) and the local log stayed silent.
		//
		// The handler has already told the peer; this is the LOCAL
		// account. The report is on the trunk's own log, one line, and
		// it names the session so an operator can match it with the
		// requester's report.
		reportRelaySessionFailure(string(open.From), open.Kind, cfg.OnSession(tsess, open.From, open.Kind))
	}
	// The target's side of the flow ENDS here.
	//
	// The direct path does the same thing (see Listener.handle: it closes
	// the stream when the handler returns), and the relay path used to
	// skip it. That asymmetry was the leak: the trunk's splice waits for
	// BOTH directions to end, and the target's direction could only end
	// if the target closed its side. It did not, so the flow never ended,
	// the trunk never freed its slot, and after twenty relays the trunk
	// refused every new one ("carrying as many relays as it may") — a
	// relay that had worked perfectly well, and then stopped, for a
	// reason that had nothing to do with the network.
	//
	// The close is a HALF close: the requester still gets the bytes the
	// handler wrote (the delivery ack) before it sees the end.
	_ = tsess.Close()
}
