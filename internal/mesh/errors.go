// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/quic-go/quic-go"
)

// Error classes for the mesh dial path.
//
// The mesh dial path has exactly one decision point that matters to the
// caller: MAY THIS FAILURE BE RETRIED ON ANOTHER PATH? A target that does
// not answer (unreachable) may be reached through a relay; a target that
// rejected our credentials (auth) may not — the credentials are wrong, and
// retrying them elsewhere just repeats the failure while leaking the
// attempt.
//
// So the dial path classifies its failures into a small, closed set of
// classes. The classification ADDS information; it never rewrites the
// message. The original text stays in the error chain (the operator reads
// it), and the sentinel is reachable with errors.Is (the path layer
// branches on it).
var (
	// ErrAuth: the peer is not who it must be, or refused us on
	// membership grounds. Never retry this on another path.
	ErrAuth = errors.New("mesh: authentication failed")
	// ErrUnreachable: nobody answered. A relay may reach the target.
	ErrUnreachable = errors.New("mesh: peer unreachable")
	// ErrRefused: the peer answered and said no (e.g. no inbox
	// configured). The peer is healthy; the request was rejected.
	ErrRefused = errors.New("mesh: peer refused")
	// ErrNoRoute: there is no path at all — the member view holds no
	// endpoint for the target. This is a routing fact, not a credential
	// problem, and not a "the peer is down" fact either.
	ErrNoRoute = errors.New("mesh: no route to target")
	// ErrQuota: a relay's quota is exhausted (relay side; unused by the
	// direct path).
	ErrQuota = errors.New("mesh: quota exceeded")
	// ErrSessionClosed: the session ended before the exchange finished
	// (relay side; unused by the direct path).
	ErrSessionClosed = errors.New("mesh: session closed")
	// ErrRelayUnavailable: the relay path was chosen but the relay
	// trunk is not built yet, so the attempt could not be made.
	//
	// It has to be its own class. ErrNoRoute is a routing fact — the
	// member view holds no endpoint — and borrowing it would tell the
	// caller to keep walking a list of paths that cannot work either.
	// ErrSessionClosed is a session fact, which is just as wrong: no
	// session was ever opened.
	ErrRelayUnavailable = errors.New("mesh: relay path unavailable")
)

// dialClasses is every sentinel classifyDialError can attach. An error
// that already carries one of them is passed through untouched.
var dialClasses = []error{
	ErrAuth, ErrUnreachable, ErrRefused, ErrNoRoute, ErrQuota, ErrSessionClosed,
	ErrRelayUnavailable,
}

// refusalMarker marks an error whose peer WAS reached and refused the
// session. classifyDialError turns it into ErrRefused.
//
// It exists as a distinct type (rather than wrapping ErrRefused at the
// raise site) so that the classification lives in exactly one place: every
// dial error gets its class from classifyDialError, never from whichever
// function happened to raise it.
type refusalMarker struct {
	reason string
}

func (e *refusalMarker) Error() string { return e.reason }

// newRefused builds a refusal error: the peer answered and declined.
func newRefused(format string, args ...any) error {
	return &refusalMarker{reason: fmt.Sprintf(format, args...)}
}

// classifyDialError attaches an error class to a dial failure without
// touching the message.
//
// Rules, in order:
//
//  1. nil stays nil.
//  2. An error that already carries a class is returned unchanged — the
//     class is decided once, here, and nowhere else.
//  3. A QUIC crypto error (quic.TransportError whose error code is a TLS
//     alert) is an auth failure. This case is the subtle one: quic.Dial
//     returns a NIL error when the peer rejects our certificate, because
//     the rejection only arrives after the handshake starts — it surfaces
//     at the hello write or the ack read. Both of those wrap the
//     TransportError, so errors.As finds it here. The Remote flag is NOT
//     consulted: a LOCAL crypto error (our pin rejected the peer) and a
//     REMOTE one (the peer rejected us) are both auth failures.
//  4. A timeout (context deadline, os.ErrDeadlineExceeded, a net.Error
//     timeout, quic-go's handshake/idle timeouts) is "nobody answered" —
//     unreachable.
//  5. Anything else is returned unchanged: an unclassified error is
//     honest, and inventing a class for it would make the fallback
//     decision arbitrary.
func classifyDialError(err error) error {
	if err == nil {
		return nil
	}
	for _, c := range dialClasses {
		if errors.Is(err, c) {
			return err
		}
	}
	var refusal *refusalMarker
	if errors.As(err, &refusal) {
		return fmt.Errorf("%w: %w", ErrRefused, refusal)
	}
	// A refusal that reached us as plain text (the peer answered "no")
	// is still a refusal. The marker above is the normal route; this
	// catches the same signal arriving from a layer that formats its
	// own message.
	if strings.Contains(err.Error(), "listener refused:") {
		return fmt.Errorf("%w: %w", ErrRefused, err)
	}
	if isCryptoError(err) {
		return fmt.Errorf("%w: %w", ErrAuth, err)
	}
	if isTimeoutError(err) {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	return err
}

// isCryptoError reports whether err is (or wraps) a QUIC transport error
// carrying a TLS alert code. Those are the authentication failures: the
// handshake did not complete because a certificate was not acceptable.
func isCryptoError(err error) bool {
	var te *quic.TransportError
	if !errors.As(err, &te) {
		return false
	}
	return te.ErrorCode.IsCryptoError()
}

// isTimeoutError reports whether err is a "nobody answered in time"
// signal: the deadline expired, or the transport gave up waiting.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}
