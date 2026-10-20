// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/muzimu217/container-mesh/internal/wire"
)

// Signaller carries the two signalling frames between peers. The session
// implements it; keeping it an interface lets the negotiation be tested
// without a real relay.
type Signaller interface {
	SendFrame(*wire.Frame) error
	RecvFrame() (*wire.Frame, error)
}

// Result is the outcome of a negotiation.
type Result struct {
	// Conn is the direct transport, nil when punching failed.
	Conn *Conn
	// Socket is the UDP socket hole punching used. It is returned so a caller
	// can run a different transport (QUIC) on the same NAT mapping instead of
	// the built-in one. Ownership passes to the caller when Conn is nil.
	Socket net.PacketConn
	// Remote is the address that answered the punch.
	RemoteAddr *net.UDPAddr
	// Remote is the address that answered.
	Remote *net.UDPAddr
	// RTT is the measured direct round trip.
	RTT time.Duration
	// Reason explains a failure, for logging and diagnostics.
	Reason string
	// Tried lists every candidate and its outcome.
	Tried []PunchAttempt
}

// Options configures a negotiation.
type Options struct {
	// STUNServers are queried for the reflexive candidate.
	STUNServers []string
	// STUNTimeout bounds each STUN query.
	STUNTimeout time.Duration
	// PunchTimeout bounds the whole hole-punch attempt.
	PunchTimeout time.Duration
	// Enabled false skips the attempt entirely (relay-only mode).
	Enabled bool
	// Logger receives progress events.
	Logger *slog.Logger
}

// DefaultOptions returns the standard negotiation settings.
//
// Enabled is false, matching the CLI default: the direct path is an
// experimental accelerator, and on a fast link the user-space UDP transport is
// slower than the relay (measured 13 MiB/s vs 512 MiB/s for a 1 GiB transfer).
// Callers that want it must ask for it explicitly, so the two layers cannot
// disagree about what "default" means.
func DefaultOptions() Options {
	return Options{
		STUNServers: []string{
			"stun.miwifi.com:3478",
			"stun.cloudflare.com:3478",
			"stun.l.google.com:19302",
		},
		STUNTimeout:  2 * time.Second,
		PunchTimeout: 5 * time.Second,
		Enabled:      false,
		Logger:       slog.Default(),
	}
}

// Negotiate tries to establish a direct UDP path.
//
// Both peers run this concurrently. The exchange is:
//
//  1. each side binds a UDP socket and gathers candidates (local + STUN);
//  2. each sends a FrameP2POffer with those candidates;
//  3. each reads the peer's offer and punches toward its candidates;
//  4. whichever side completes first sends FrameP2PAnswer{OK:true, Chosen:...}.
//
// If either side fails, both fall back to the relay — the caller decides that,
// this function only reports.
//
// The signalling runs inside the already-encrypted session, so a malicious
// relay cannot inject candidates to redirect the direct path.
func Negotiate(ctx context.Context, sig Signaller, opts Options) (*Result, error) {
	if !opts.Enabled {
		return &Result{Reason: "p2p disabled"}, nil
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.PunchTimeout <= 0 {
		opts.PunchTimeout = 5 * time.Second
	}

	udp, err := Listen()
	if err != nil {
		return &Result{Reason: "cannot open UDP socket: " + err.Error()}, nil
	}

	sessionID, err := NewConnID()
	if err != nil {
		_ = udp.Close()
		return &Result{Reason: "cannot generate session id: " + err.Error()}, nil
	}

	gatherCtx, cancelGather := context.WithTimeout(ctx, 2*opts.STUNTimeout+time.Second)
	gathered, gerr := Gather(gatherCtx, udp, opts.STUNServers, opts.STUNTimeout)
	cancelGather()
	if gerr != nil && len(gathered.Candidates) == 0 {
		_ = udp.Close()
		return &Result{Reason: "candidate gathering failed: " + gerr.Error()}, nil
	}

	// Exchange candidates. Both sides send first, then read, so neither can
	// deadlock waiting for the other.
	offer := wire.P2POffer{SessionID: sessionID}
	for _, c := range gathered.Candidates {
		offer.Candidates = append(offer.Candidates, wire.P2PCandidate{Addr: c.Addr, Kind: c.Kind})
	}
	payload, _ := json.Marshal(offer)
	if err := sig.SendFrame(&wire.Frame{Type: wire.FrameP2POffer, Payload: payload}); err != nil {
		_ = udp.Close()
		return &Result{Reason: "sending offer: " + err.Error()}, nil
	}

	peerOffer, err := readP2PFrame(sig, wire.FrameP2POffer, opts.PunchTimeout)
	if err != nil {
		_ = udp.Close()
		return &Result{Reason: "reading peer offer: " + err.Error()}, nil
	}
	var theirs wire.P2POffer
	if err := json.Unmarshal(peerOffer.Payload, &theirs); err != nil {
		_ = udp.Close()
		return &Result{Reason: "bad peer offer: " + err.Error()}, nil
	}
	if len(theirs.Candidates) == 0 {
		_ = udp.Close()
		return &Result{Reason: "peer offered no candidates"}, nil
	}

	remoteCands := make([]Candidate, 0, len(theirs.Candidates))
	for _, c := range theirs.Candidates {
		remoteCands = append(remoteCands, Candidate{Addr: c.Addr, Kind: c.Kind})
	}

	// Both sides must punch with the SAME conn id, or each drops the other's
	// probes as stray traffic. The ids are generated independently, so derive
	// the shared value deterministically from both (min is symmetric).
	connID := sessionID
	if theirs.SessionID < connID {
		connID = theirs.SessionID
	}

	punchCtx, cancelPunch := context.WithTimeout(ctx, opts.PunchTimeout)
	punch, perr := Punch(punchCtx, udp, PunchOptions{
		LocalCandidates:  gathered.Candidates,
		RemoteCandidates: remoteCands,
		Timeout:          opts.PunchTimeout,
		ConnID:           connID,
	})
	cancelPunch()

	if perr != nil || punch.Remote == nil {
		reason := "hole punching failed"
		if perr != nil {
			reason += ": " + perr.Error()
		}
		// Tell the peer so it stops waiting instead of burning its own
		// timeout; both then use the relay.
		answer := wire.P2PAnswer{SessionID: sessionID, OK: false}
		if data, err := json.Marshal(answer); err == nil {
			_ = sig.SendFrame(&wire.Frame{Type: wire.FrameP2PAnswer, Payload: data})
		}
		_ = udp.Close()
		return &Result{Reason: reason, Tried: punchTried(punch)}, nil
	}

	// Announce the working address so the peer can send data to it.
	answer := wire.P2PAnswer{SessionID: sessionID, OK: true, Chosen: punch.Remote.String()}
	if data, err := json.Marshal(answer); err == nil {
		_ = sig.SendFrame(&wire.Frame{Type: wire.FrameP2PAnswer, Payload: data})
	}

	// Both sides must switch at the same point, otherwise frames in flight on
	// the relay are lost and the AEAD counter desynchronises (the receiver
	// expects counter N but the sender has already moved on). Wait for the
	// peer's answer before handing the socket to the session.
	if _, err := readP2PFrame(sig, wire.FrameP2PAnswer, opts.PunchTimeout); err != nil {
		_ = udp.Close()
		return &Result{Reason: "peer did not confirm the direct path: " + err.Error()}, nil
	}

	// Clear any deadline left over from gathering/punching; the session's
	// reader must block indefinitely.
	_ = udp.SetReadDeadline(time.Time{})

	// Build the built-in transport as well: --p2p-legacy uses it, and the QUIC
	// path ignores it (the caller closes it).
	conn := NewConn(udp, punch.Remote, connID, DefaultConfig())

	opts.Logger.Info("p2p path established",
		"remote", punch.Remote.String(),
		"rtt", punch.RTT.Round(time.Millisecond).String(),
		"candidates", len(gathered.Candidates))

	return &Result{
		Conn:       conn,
		Socket:     udp,
		RemoteAddr: punch.Remote,
		Remote:     punch.Remote,
		RTT:        punch.RTT,
		Tried:      punchTried(punch),
	}, nil
}

func punchTried(p *PunchResult) []PunchAttempt {
	if p == nil {
		return nil
	}
	return p.Tried
}

// readP2PFrame reads frames until one of the wanted type arrives, skipping
// keepalives and any other traffic that interleaves.
func readP2PFrame(sig Signaller, want wire.FrameType, timeout time.Duration) (*wire.Frame, error) {
	// Read synchronously. An earlier version ran the read in a goroutine and
	// abandoned it on timeout; the abandoned reader stayed blocked inside
	// RecvFrame and then raced the session's read pump for the next frame,
	// which could swallow the first business message.
	//
	// The signalling exchange is strictly sequential (send offer, read
	// answer), so a synchronous read is safe here. The deadline is enforced by
	// the transport: the session's RecvFrame returns an error once the socket
	// is closed, and the caller's own timeout bounds the whole negotiation.
	type res struct {
		f   *wire.Frame
		err error
	}
	ch := make(chan res, 1)
	go func() {
		f, err := sig.RecvFrame()
		ch <- res{f, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		if r.f.Type != want {
			return nil, fmt.Errorf("expected %s, got %s", want, r.f.Type)
		}
		return r.f, nil
	case <-time.After(timeout):
		// The reader is still parked in RecvFrame. It will finish when the
		// session closes, and the caller treats this as a failed negotiation
		// and falls back to the relay. To avoid the double-reader problem the
		// caller must NOT start the read pump while this reader is alive; the
		// session closes on this error path, which releases it.
		return nil, errors.New("timed out waiting for p2p signalling")
	}
}

// LocalAddr reports the socket's local address, for diagnostics.
func (r *Result) LocalAddr() string {
	if r == nil || r.Conn == nil || r.Conn.udp == nil {
		return ""
	}
	return r.Conn.udp.LocalAddr().String()
}

// Describe renders a one-line summary for logs and the CLI.
func (r *Result) Describe() string {
	if r == nil {
		return "p2p: not attempted"
	}
	if r.Conn == nil {
		return fmt.Sprintf("p2p: direct path unavailable (%s); using relay", r.Reason)
	}
	return fmt.Sprintf("p2p: direct path to %s (rtt %s)",
		r.Remote.String(), r.RTT.Round(time.Millisecond))
}
