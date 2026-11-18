// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// Path selection.
//
// A session is established over a PATH. v1.1 knows two kinds: a direct
// link between the two members, and a one-hop relay through a third
// member. The picker decides which paths are worth trying, in which
// order; the path layer then walks that order and stops at the first one
// that opens.
//
// The compatibility rule is absolute: with no picker configured, the dial
// path is byte-for-byte what it was before this abstraction existed. That
// is why dialViaPicker short-circuits to the direct dial instead of
// synthesising a path list — there is no second code path to keep in sync.

// PathKind names the transport a session runs over.
type PathKind string

const (
	// PathDirect is a direct link between the two members.
	PathDirect PathKind = "direct"
	// PathRelay is a one-hop relay through the member named by Path.Via.
	PathRelay PathKind = "relay"
)

// Path is one candidate way to reach the target.
type Path struct {
	// Kind is the transport this path uses.
	Kind PathKind
	// Via is the relay member for a relay path (empty for a direct one).
	Via identity.DeviceID
	// Endpoints are the addresses to try for a direct path (empty = use
	// the dial config's own endpoints).
	Endpoints []string
	// ViaEndpoints are the addresses of the RELAY member for a relay path.
	//
	// A relay path has two address segments — caller→relay and
	// relay→target — and they live in different network domains (the
	// relay may be on a private network the caller reaches through a
	// tunnel, the target may be reachable only from the relay's side).
	// They cannot share one list, so the relay's addresses live here.
	// Without them a relay path could not even name the node it meant to
	// go through.
	ViaEndpoints []string
	// Hops is the remaining hop budget for this path. One hop is the
	// shape the trunk carries today; the field is here so a multi-hop
	// path does not have to change the path structure when it arrives.
	// Zero means "unset", which reads as one hop.
	Hops int
	// TargetKey is the target's public key, from the member view.
	//
	// A relay path needs it and a direct path does not: the direct path
	// gets its key from the TLS pinning (the peer presents its
	// certificate and the pin checks it against the view), but the
	// RELAYED session's end-to-end handshake is verified by the
	// requester against the view directly — there is no certificate
	// involved, because the relay terminates TLS on both hops.
	//
	// Without this key the requester could not verify the target's
	// signed hello, and an unverified handshake is worth nothing: the
	// relay could then present its own key as the target's.
	TargetKey ed25519.PublicKey
}

// PathPicker names the paths worth trying for a target, best first.
//
// It returns an ordered list rather than a single path, because the
// choice is only knowable by trying: "the direct path is unreachable" is
// a fact the picker cannot see in advance. An empty list means "no path".
type PathPicker interface {
	Pick(ctx context.Context, target identity.DeviceID, kind string) ([]Path, error)
}

// DirectFirstPicker is the shipped picker: try the direct path first,
// then the relay candidates in the order it was given.
//
// It carries no state, so the same value always yields the same order —
// the determinism the relay path needs (R-3.2 asks for a deterministic
// first usable relay, not a clever one).
type DirectFirstPicker struct {
	// Direct are the target's endpoints for the direct path. Empty means
	// "use the endpoints from the dial config".
	Direct []string
	// Relays are the relay candidates, in preference order.
	Relays []Path
}

// Pick returns the direct path first, then the relay candidates.
func (p DirectFirstPicker) Pick(ctx context.Context, target identity.DeviceID, kind string) ([]Path, error) {
	out := make([]Path, 0, 1+len(p.Relays))
	if len(p.Direct) > 0 {
		out = append(out, Path{Kind: PathDirect, Endpoints: append([]string(nil), p.Direct...)})
	} else {
		out = append(out, Path{Kind: PathDirect})
	}
	for _, r := range p.Relays {
		if r.Kind != PathRelay {
			// A picker must not smuggle a second direct path in: the
			// dial config owns the target's endpoints.
			r.Kind = PathRelay
		}
		out = append(out, r)
	}
	return out, nil
}

// pathDialer establishes a session over one path. It is the seam that
// keeps the path logic testable without a network.
type pathDialer func(ctx context.Context, cfg DialConfig, p Path) (*Link, error)

// tryPaths walks the candidate paths in order and returns the first link
// that opens.
//
// The fallback rule is the whole point of the error classes:
//
//   - Unreachable / NoRoute → try the next path. The target may be
//     reachable another way.
//   - Auth / Refused → STOP. The peer was reached and the credentials or
//     the request were rejected; the answer does not change on another
//     path, and retrying leaks the attempt.
//   - Unclassified → try the next path (the conservative choice: an
//     unknown failure is not evidence of a wrong credential).
//
// The returned error is the LAST failure, with its class intact.
func tryPaths(ctx context.Context, cfg DialConfig, paths []Path, dial pathDialer) (*Link, Path, error) {
	if len(paths) == 0 {
		return nil, Path{}, fmt.Errorf("%w: mesh: no path to %s", ErrNoRoute, cfg.Target)
	}
	var lastErr error
	var lastPath Path
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, lastPath, err
		}
		link, err := dial(ctx, cfg, p)
		if err == nil {
			return link, p, nil
		}
		classified := classifyDialError(err)
		lastErr = classified
		// Remember which path failed, so the caller can report it. The CLI
		// says which path it took, and "the relay did not work either" is
		// only meaningful with the path in hand.
		lastPath = p
		if errors.Is(classified, ErrAuth) || errors.Is(classified, ErrRefused) {
			// A rejection is a decision, not a network fault. Do not
			// present the same credentials to another path.
			return nil, p, classified
		}
	}
	return nil, lastPath, lastErr
}

// DialViaPicker establishes a session, letting the configured picker
// choose the path.
//
// With no picker, it dials direct exactly as before. With a picker, it
// walks the picker's paths. The returned PathKind tells the caller which
// path was used, so the CLI can report the path it actually took instead
// of guessing it from the outcome.
//
// It is the EXPORTED entry point for the CLI: the internal callers
// (SendMsg, SendFile, SendRun, RunBench) do not care which path was
// taken, but `cmesh mesh ping` does — it prints "link=relay".
func DialViaPicker(ctx context.Context, cfg DialConfig) (*Link, PathKind, error) {
	return dialViaPicker(ctx, cfg)
}

// DialViaPickerPath is DialViaPicker, and it also reports the PATH that
// was used — not just its kind.
//
// The CLI prints "link=relay via=<deviceID>", and "relay" alone does not
// say WHICH trunk carried the traffic — which is the first thing an
// operator wants to know when a ping is slow.
func DialViaPickerPath(ctx context.Context, cfg DialConfig) (*Link, Path, error) {
	if cfg.Picker == nil {
		link, err := DialDirect(ctx, cfg)
		if err != nil {
			return nil, Path{}, err
		}
		return link, Path{Kind: PathDirect}, nil
	}
	if _, err := cfg.Resolver.KeyFor(cfg.Target); err != nil {
		return nil, Path{}, fmt.Errorf("%w: %w", ErrAuth, err)
	}
	paths, err := cfg.Picker.Pick(ctx, cfg.Target, cfg.Kind)
	if err != nil {
		return nil, Path{}, err
	}
	link, used, err := tryPaths(ctx, cfg, paths, dialPath)
	if err != nil {
		return nil, used, err
	}
	return link, used, nil
}

// dialViaPicker is the internal implementation. See DialViaPicker.
func dialViaPicker(ctx context.Context, cfg DialConfig) (*Link, PathKind, error) {
	if cfg.Picker == nil {
		link, err := DialDirect(ctx, cfg)
		return link, PathDirect, err
	}
	// The membership check comes BEFORE the picker is asked. The picker
	// hands the target's identity to a relay, and a device that is not a
	// member — a revoked one, or one that was never a member — must not
	// have its identity sent anywhere. DialDirect makes this check
	// internally; here it has to happen first, because the picker runs
	// before DialDirect would.
	if _, err := cfg.Resolver.KeyFor(cfg.Target); err != nil {
		return nil, PathDirect, fmt.Errorf("%w: %w", ErrAuth, err)
	}
	paths, err := cfg.Picker.Pick(ctx, cfg.Target, cfg.Kind)
	if err != nil {
		return nil, PathDirect, err
	}
	link, used, err := tryPaths(ctx, cfg, paths, dialPath)
	if err != nil {
		// Report the path that actually failed. The CLI prints link= from
		// this value, so answering "direct" when the relay is what broke
		// hides which route is at fault.
		if used.Kind != "" {
			return nil, used.Kind, err
		}
		return nil, PathDirect, err
	}
	return link, used.Kind, nil
}

// dialPath establishes a session over one path.
//
// The direct path is a mutually-authenticated QUIC link. The relay path
// opens a RELAYED session through a trunk member: the requester asks the
// trunk to carry a session to the target, and then speaks the end-to-end
// protocol THROUGH the trunk.
//
// The business layer only ever sees a Session, so it cannot tell the
// difference between the two.
func dialPath(ctx context.Context, cfg DialConfig, p Path) (*Link, error) {
	switch p.Kind {
	case PathDirect:
		if len(p.Endpoints) > 0 {
			cfg.Endpoints = append([]string(nil), p.Endpoints...)
		}
		return DialDirect(ctx, cfg)
	case PathRelay:
		return dialViaRelay(ctx, cfg, p)
	default:
		return nil, fmt.Errorf("%w: mesh: unknown path kind %q", ErrNoRoute, p.Kind)
	}
}

// dialViaRelay opens a session to the target THROUGH a relay member.
//
// The steps:
//
//  1. Resolve the target's key from the member view. The relayed
//     session's end-to-end handshake is verified against that key — the
//     relay terminates TLS on both hops, so there is no certificate to
//     pin. A target that is not a member fails HERE, before the relay
//     ever hears the target's identity.
//  2. Open a session to the RELAY member (by its endpoints).
//  3. Ask the relay to carry a session to the target, and complete the
//     end-to-end handshake through it.
//
// The error classes are deliberate:
//
//   - A relay that is not a member → ErrAuth. A verdict: the answer
//     will not change on another relay.
//   - A relay whose switch is off → ErrRelayTrunkDisabled. A local
//     condition: another relay may well say yes, so the caller may try.
//   - A relay whose quota is full → ErrQuota. Also a local condition.
//   - A relay that cannot be reached → ErrUnreachable. A network fact.
//
// The wording matters as much as the class: the log says WHICH relay was
// attempted, so an operator reading it knows where to look.
func dialViaRelay(ctx context.Context, cfg DialConfig, p Path) (*Link, error) {
	if cfg.Resolver == nil {
		return nil, fmt.Errorf("%w: mesh: the relay path needs a member resolver", ErrRelayUnavailable)
	}
	// ① the target's key, from the LIVE member view.
	wantKey, err := cfg.Resolver.KeyFor(cfg.Target)
	if err != nil {
		// The target is not a (current) member. This is an auth-class
		// failure: the target's identity must not be handed to a relay
		// that would then try to reach it.
		return nil, fmt.Errorf("%w: %w", ErrAuth, err)
	}
	if len(p.TargetKey) == 0 {
		// The picker did not supply the target's key. The member view
		// is the authority, and it was consulted above — so the path's
		// key is only a convenience. Use the view's.
		p.TargetKey = wantKey
	}

	// ② the relay's endpoints.
	if len(p.ViaEndpoints) == 0 {
		return nil, fmt.Errorf("%w: mesh: the relay member %s announced no endpoint to dial",
			ErrRelayUnavailable, p.Via)
	}
	// The relay must be a member too: a node that is not a member is
	// not allowed to carry this network's traffic.
	if _, err := cfg.Resolver.KeyFor(p.Via); err != nil {
		return nil, fmt.Errorf("%w: mesh: the relay member %s is not a member of this view: %w",
			ErrAuth, p.Via, err)
	}

	kind := cfg.Kind
	if kind == "" {
		kind = KindDial
	}

	// ③ open the relayed session.
	//
	// relayLink is the QUIC link to the relay member. It is captured
	// here so the relayed link can release it: nothing else owns it.
	var relayLink *Link
	sess, err := OpenRelay(ctx, RelayRequesterConfig{
		Device:    cfg.Device,
		Resolver:  cfg.Resolver,
		Target:    cfg.Target,
		Kind:      kind,
		TargetKey: p.TargetKey,
		Relay:     p.Via,
		Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
			// The session to the relay member is a QUIC link, and it is
			// owned by the RELAYED link below — so that closing the
			// relayed link also releases the connection to the relay
			// and the socket it opened.
			//
			// It is the RELAY protocol's session, not the direct link's:
			// the two have different ALPNs, so the trunk's relay port
			// cannot be confused with the direct-link port.
			link, err := DialRelaySession(ctx, cfg.Device, cfg.Resolver, relay, p.ViaEndpoints)
			if err != nil {
				return nil, err
			}
			relayLink = link
			return link.Session(), nil
		},
	})
	if err != nil {
		if relayLink != nil {
			_ = relayLink.Close()
		}
		return nil, classifyDialError(err)
	}
	// The relayed link owns the relay link: closing it releases both.
	return &Link{
		sess: sess,
		br:   bufio.NewReader(sess),
		closer: func() error {
			_ = sess.Close()
			if relayLink != nil {
				return relayLink.Close()
			}
			return nil
		},
	}, nil
}

// ViewPicker is the picker that reads the LIVE member view.
//
// It is what the CLI uses: the direct path first (the owner's ruling —
// "中继只兜底：大文件走直连，直连失败再走中继"), and then the relay
// candidates, in the deterministic device-ID order (R-3.2).
//
// The relay candidates are the members that have announced an endpoint,
// are not the target, and are not the requester itself. That is what
// RelayCandidates computes — the picker only adds the ADDRESSES, because
// a relay path without an address cannot be dialled.
//
// The picker does NOT decide whether a relay is willing: that is the
// trunk's own decision, and it may refuse. A refusal is a local
// condition, and the path layer moves on to the next candidate.
type ViewPicker struct {
	// View is the live member snapshot. It is read at PICK time, not
	// captured at construction: the view changes (a member leaves, an
	// endpoint changes), and a picker built on a stale view would keep
	// offering paths that cannot work.
	View func() *discovery.MemberSnapshot
	// Self is the requester's own device ID. It is excluded from the
	// relay candidates.
	Self identity.DeviceID
	// TargetKey resolves the target's public key from the view, so the
	// relay path can verify the target's end-to-end hello.
	TargetKey func(identity.DeviceID) (ed25519.PublicKey, error)
}

// Pick returns the direct path first, then the relay candidates.
func (p ViewPicker) Pick(ctx context.Context, target identity.DeviceID, kind string) ([]Path, error) {
	out := []Path{{Kind: PathDirect}}
	view := p.View()
	if view == nil {
		// No view: there is only the direct path. The dialer's own
		// endpoint list is used for it.
		return out, nil
	}
	for _, id := range RelayCandidates(view, target, p.Self) {
		mv, ok := view.Members[id]
		if !ok {
			continue
		}
		// The relay path is dialled on the candidate's RELAY endpoints, not
		// on its direct ones: a relay session speaks its own ALPN and needs
		// its own UDP port, so the direct addresses would fail the ALPN
		// negotiation. A candidate that has announced no relay endpoint
		// cannot serve as a trunk.
		if len(mv.RelayEndpoints) == 0 {
			continue
		}
		path := Path{
			Kind:         PathRelay,
			Via:          id,
			ViaEndpoints: append([]string(nil), mv.RelayEndpoints...),
			Hops:         1,
		}
		if p.TargetKey != nil {
			key, err := p.TargetKey(target)
			if err != nil {
				// The target's key is not available. The relay path
				// cannot verify the target's hello without it, so the
				// relay candidates are dropped. The direct path is
				// still offered — the direct path pins the certificate
				// itself.
				return out, nil
			}
			path.TargetKey = key
		}
		out = append(out, path)
	}
	return out, nil
}
