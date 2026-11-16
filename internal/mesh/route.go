// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"errors"
	"fmt"

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

// dialViaPicker establishes a session, letting the configured picker
// choose the path.
//
// With no picker, it dials direct exactly as before. With a picker, it
// walks the picker's paths. The returned PathKind tells the caller which
// path was used, so the CLI can report the path it actually took instead
// of guessing it from the outcome.
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
		return nil, PathDirect, err
	}
	return link, used.Kind, nil
}

// dialPath establishes a session over one path.
//
// Only the direct path is implemented. The relay path is where the
// relay trunk (PR-2) plugs in: at that point this function opens a
// relay route instead of a QUIC stream, and the business layer — which
// only ever sees a Session — cannot tell the difference.
func dialPath(ctx context.Context, cfg DialConfig, p Path) (*Link, error) {
	switch p.Kind {
	case PathDirect:
		if len(p.Endpoints) > 0 {
			cfg.Endpoints = append([]string(nil), p.Endpoints...)
		}
		return DialDirect(ctx, cfg)
	case PathRelay:
		// Not "no route". The relay trunk does not exist yet, so trying
		// another path cannot help either, and ErrNoRoute would tell the
		// caller to keep walking a list. This class stops the walk.
		return nil, fmt.Errorf("%w: mesh: relay paths are not available yet", ErrSessionClosed)
	default:
		return nil, fmt.Errorf("%w: mesh: unknown path kind %q", ErrNoRoute, p.Kind)
	}
}
