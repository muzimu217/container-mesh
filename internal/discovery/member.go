// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package discovery implements the mesh's discovery layer: who is a member,
// where they can be reached, and how that information propagates.
//
// Division of trust, restated from MESH-DESIGN §2.2 because it decides two
// choices below:
//
//   - The discovery node holds the member list in plaintext IN MEMORY and
//     must itself be a member. It has to read the list to verify signatures
//     and push updates. The constraint this buys is that whoever runs
//     discovery is trusted; a deployment that cannot trust any machine does
//     not run one.
//   - The node never carries user data. A compromise exposes membership
//     (IDs, addresses, public keys) — not traffic, and not any private key.
//
// This package imports identity (for member types) and wire (frame types);
// it must not import mesh or e2e.
package discovery

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// HeartbeatInterval is how often a client announces itself.
const HeartbeatInterval = 30 * time.Second

// MemberTimeout is how long the node holds a member after its last heartbeat.
// This is the FALLBACK liveness signal: the fast path is transport-level
// break detection (R-2.2, DESIGN.md §2.2), which fires in <=10 s. The
// heartbeat timeout only catches "connection alive but process hung".
const MemberTimeout = 90 * time.Second

// SweepInterval is how often the node checks for timeouts.
const SweepInterval = 15 * time.Second

// MemberAnnounce is what a device sends on every heartbeat: who I am and
// where I can be reached.
//
// Endpoints are candidate addresses, public and private. The receiver tries
// them in order when dialing; nothing here is authoritative.
type MemberAnnounce struct {
	DeviceID  identity.DeviceID `json:"device_id"`
	Endpoints []string          `json:"endpoints"`
	Timestamp time.Time         `json:"timestamp"`
}

// Errors from announce validation. Each is distinct so the node can answer
// with the reason rather than a blanket 403.
var (
	// ErrAnnounceNotMember: the device is not in the member list. This is the
	// R-2.4 gate — a stranger who learns the discovery node's address still
	// cannot inject itself, because it cannot obtain membership.
	ErrAnnounceNotMember = errors.New("discovery: device is not a member")
	// ErrAnnounceBadSignature: the signature does not verify against the
	// device key recorded in the member list.
	ErrAnnounceBadSignature = errors.New("discovery: announce signature invalid")
	// ErrAnnounceStale: the timestamp is too far from now. Bounds replay of a
	// captured announce (which would otherwise let a removed device keep a
	// stale presence alive).
	ErrAnnounceStale = errors.New("discovery: announce timestamp outside window")
	// ErrAnnounceBadEndpoint: an endpoint is not host:port.
	ErrAnnounceBadEndpoint = errors.New("discovery: malformed endpoint")
)

// AnnounceClockSkew bounds how far an announce timestamp may sit from the
// receiver's clock. Five seconds absorbs normal NTP drift without letting a
// replayed announce pass as fresh indefinitely.
const AnnounceClockSkew = 5 * time.Second

// MaxEndpoints bounds the candidate list. A device with hundreds of
// candidates is either broken or hostile; both are rejected rather than
// stored.
const MaxEndpoints = 16

// Validate performs every check the node does before an announce updates
// state. It is exported so the client side can pre-check its own announce
// (cheaper than a round trip to be told).
//
// membership supplies the recorded device key; nil key means not a member.
func (a MemberAnnounce) Validate(memberKey ed25519.PublicKey, sig []byte, now time.Time) error {
	if now.Sub(a.Timestamp) > AnnounceClockSkew || a.Timestamp.Sub(now) > AnnounceClockSkew {
		return ErrAnnounceStale
	}
	if len(a.Endpoints) > MaxEndpoints {
		return fmt.Errorf("%w: %d endpoints, max %d", ErrAnnounceBadEndpoint, len(a.Endpoints), MaxEndpoints)
	}
	seen := make(map[string]bool, len(a.Endpoints))
	for _, e := range a.Endpoints {
		if !validEndpoint(e) {
			return fmt.Errorf("%w: %q", ErrAnnounceBadEndpoint, e)
		}
		if seen[e] {
			return fmt.Errorf("%w: duplicate %q", ErrAnnounceBadEndpoint, e)
		}
		seen[e] = true
	}
	if memberKey == nil {
		return ErrAnnounceNotMember
	}
	if !identity.VerifyDevice(memberKey, a.signable(), sig) {
		return ErrAnnounceBadSignature
	}
	return nil
}

// signable is the exact byte range the signature covers. Marshaling the
// struct without the signature (which lives outside it) is deterministic:
// every field has a fixed JSON shape.
func (a MemberAnnounce) signable() []byte {
	// Re-marshal through a fixed field order to avoid depending on struct
	// field order staying stable across refactors.
	b, _ := jsonMarshal(struct {
		DeviceID  identity.DeviceID `json:"device_id"`
		Endpoints []string          `json:"endpoints"`
		Timestamp time.Time         `json:"timestamp"`
	}{a.DeviceID, a.Endpoints, a.Timestamp})
	return b
}

// Sign produces the signature over signable with the device key.
func (a MemberAnnounce) Sign(dev *identity.Device) []byte {
	return dev.Sign(a.signable())
}

// validEndpoint checks the host:port shape. It deliberately does NOT
// resolve: whether an address is reachable is the dialer's problem, and DNS
// here would make validation depend on a network the discovery node should
// not need for a syntactic check.
func validEndpoint(e string) bool {
	if e == "" || len(e) > 253 {
		return false
	}
	host, port, err := net.SplitHostPort(e)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return false
	}
	// Reject anything with a scheme or path: an endpoint is not a URL, and
	// accepting "ws://x/y" here would smuggle a second syntax in.
	if strings.ContainsAny(e, "/?#") {
		return false
	}
	return true
}

// Invite is the join token. It carries NO secret: only the account ID (so
// the joiner knows which network), a discovery node address, and the
// account public key (so the joiner can verify the certificate it is about
// to be asked for).
//
// Actually joining still requires an existing member's approval (R-1.5):
// possession of an invite lets you ASK, not ENTER.
type Invite struct {
	AccountID      identity.DeviceID `json:"account_id"`
	AccountPub     ed25519.PublicKey `json:"account_pub"`
	DiscoveryAddrs []string          `json:"discovery_addrs"`
	IssuedAt       time.Time         `json:"issued_at"`
}

// EncodeInvite renders the invite as a shareable string.
func EncodeInvite(inv Invite) (string, error) {
	v := url.Values{}
	v.Set("acct", string(inv.AccountID))
	v.Set("k", string(inv.AccountPub))
	for _, a := range inv.DiscoveryAddrs {
		v.Add("disc", a)
	}
	v.Set("at", inv.IssuedAt.UTC().Format(time.RFC3339))
	return "cmesh://join?" + v.Encode(), nil
}

// DecodeInvite parses an invite string. Malformed input yields an error
// rather than a partial invite.
func DecodeInvite(s string) (Invite, error) {
	if !strings.HasPrefix(s, "cmesh://join?") {
		return Invite{}, errors.New("discovery: not a cmesh invite")
	}
	u, err := url.Parse(s)
	if err != nil {
		return Invite{}, err
	}
	q := u.Query()
	var inv Invite
	inv.AccountID = identity.DeviceID(q.Get("acct"))
	if !inv.AccountID.Valid() {
		return Invite{}, errors.New("discovery: invite has a malformed account id")
	}
	k := q.Get("k")
	if k == "" {
		return Invite{}, errors.New("discovery: invite has no account key")
	}
	if len(k) != ed25519.PublicKeySize {
		return Invite{}, errors.New("discovery: invite account key has wrong length")
	}
	inv.AccountPub = ed25519.PublicKey(k)
	for _, a := range q["disc"] {
		if !validEndpoint(a) {
			return Invite{}, fmt.Errorf("%w: %q", ErrAnnounceBadEndpoint, a)
		}
		inv.DiscoveryAddrs = append(inv.DiscoveryAddrs, a)
	}
	if len(inv.DiscoveryAddrs) == 0 {
		return Invite{}, errors.New("discovery: invite has no discovery address")
	}
	at := q.Get("at")
	if at == "" {
		return Invite{}, errors.New("discovery: invite has no issue time")
	}
	inv.IssuedAt, err = time.Parse(time.RFC3339, at)
	if err != nil {
		return Invite{}, errors.New("discovery: invite issue time is not RFC3339")
	}
	return inv, nil
}
