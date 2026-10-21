// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package forward maps local TCP ports to services reachable from the peer, and
// vice versa, over the multiplexed session.
//
// The security model in one paragraph: a peer that completed the PAKE knows the
// code, which makes it authenticated but not trusted. Everything it sends is
// attacker-controlled, so every forwarded stream passes four gates in order —
// is forwarding enabled at all, is the target in the allow-list, is the rate
// within limits, and only then do we dial. The order is a security property:
// dialling first and checking afterwards would let a refused request still
// produce an outbound connection, which is a port scanner and an amplifier.
package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/muzimu217/container-mesh/internal/mux"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// Stream is the slice of a multiplexed stream the forwarder needs.
type Stream interface {
	io.ReadWriteCloser
	SetDeadline(t time.Time) error
	Kind() byte
}

// Mux is the slice of a multiplexed session the forwarder needs. Keeping it an
// interface lets the whole package be tested with a fake, with no yamux and no
// network.
type Mux interface {
	Open(ctx context.Context, kind byte) (Stream, error)
	NumStreams() int
}

// Provider supplies the session that is currently live.
//
// The node reconnects and rebuilds the session on every drop, so a forwarder
// must never cache one: it asks for the current session per connection.
type Provider interface {
	Current(ctx context.Context) (Mux, error)
}

// Dialer connects to a target. It is an interface so tests can prove policy
// runs before any dial happens.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// Config configures a Manager.
type Config struct {
	// Accept serves -L streams arriving from the peer. Off by default.
	Accept bool
	// AcceptReverse hosts listeners the peer asked for (-R). Off by default:
	// binding a port is a categorically bigger permission than dialling one.
	AcceptReverse bool
	// ListenAll permits a -R request to bind a non-loopback address.
	ListenAll bool

	Allow       AllowList // zero value denies everything
	ListenPorts PortSet   // zero value denies every -R port
	Limits      Limits

	// Dialer is swapped in tests. Nil means a real TCP dialler.
	Dialer Dialer
	Logger *slog.Logger
}

// Manager owns the forwarding policy and the per-stream handling.
type Manager struct {
	cfg Config
	log *slog.Logger

	gate   *pendingGate
	bucket *tokenBucket

	dialer Dialer

	mu     sync.Mutex
	locals []*LocalForward
	// reverse maps a -R listener ID to the local target it serves. The ID comes
	// from the peer's preamble, but the target comes from local configuration:
	// that separation is what makes -R structurally safe.
	reverse      map[string]string
	revListeners map[string]*reverseListener
	closed       bool
	closeCh      chan struct{}
}

// New builds a Manager. It applies the policy defaults but binds nothing.
func New(cfg Config) *Manager {
	cfg.Limits = cfg.Limits.normalise()
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	d := cfg.Dialer
	if d == nil {
		d = &net.Dialer{}
	}
	m := &Manager{
		cfg:     cfg,
		log:     log,
		gate:    newPendingGate(cfg.Limits.MaxPending),
		dialer:  d,
		closeCh: make(chan struct{}),
		reverse: make(map[string]string),
	}
	if cfg.Limits.MaxStreamsPerSec > 0 {
		// Burst equals the per-second rate: a legitimate client opening a
		// handful of connections at once should not be refused, while a
		// scanner's sustained rate is still capped.
		m.bucket = newTokenBucket(float64(cfg.Limits.MaxStreamsPerSec), float64(cfg.Limits.MaxStreamsPerSec))
	}
	return m
}

// Serve is the kind dispatcher handed to mux.Session.Serve.
//
// It runs on its own goroutine per stream (mux.Serve guarantees that), so it may
// block for the life of the connection.
func (m *Manager) Serve(ctx context.Context, st Stream) {
	switch st.Kind() {
	case mux.KindForward:
		// -L and -R are two different permissions and must be gated
		// separately: --accept means "you may dial targets I allow",
		// --accept-reverse means "you may ask me to bind a port". Checking only
		// --accept here would refuse every reverse stream, which is exactly the
		// bug this comment prevents.
		if !m.cfg.Accept && !m.cfg.AcceptReverse {
			// Do not even read the preamble. Answering immediately means a peer
			// without permission cannot use us as a timing or reachability
			// oracle for targets of its choosing, and a malformed preamble
			// never reaches the parser.
			m.reject(st, wire.StatusDenied,
				"forwarding is disabled (needs --accept or --accept-reverse)")
			return
		}
		m.Handle(ctx, st)
	case mux.KindControl:
		// The control stream belongs to peer.Session; if it reaches here the
		// session wiring is wrong. Close it quietly rather than answering with
		// a forward status, which would confuse the peer's frame decoder.
		m.log.Warn("control stream reached the forwarder; closing", "stream", streamID(st))
		_ = st.Close()
	default:
		m.reject(st, wire.StatusBadRequest, fmt.Sprintf("unknown stream kind %d", st.Kind()))
	}
}

// Handle serves one inbound forward stream: read the preamble, check policy,
// dial, answer with a status, then pipe.
func (m *Manager) Handle(ctx context.Context, st Stream) {
	defer st.Close()

	// Bound the preamble read. A stream that never sends one would otherwise
	// hold a semaphore slot and 2x the yamux window for the life of the
	// session, which is free to a hostile peer.
	if err := st.SetDeadline(time.Now().Add(m.cfg.Limits.HandshakeTimeout)); err != nil {
		m.log.Debug("could not set handshake deadline", "err", err)
	}
	p, err := wire.ReadPreamble(st)
	if err != nil {
		m.reject(st, wire.StatusBadRequest, err.Error())
		return
	}
	// Clear the deadline: the stream may now legitimately sit idle (an SSH
	// session with no typing).
	_ = st.SetDeadline(time.Time{})

	if p.Kind == wire.KindDirect && !m.cfg.Accept {
		// The peer may only have --accept-reverse; a direct stream still needs
		// --accept, because dialling targets is the bigger permission.
		m.reject(st, wire.StatusDenied, "direct forwarding is disabled (--accept)")
		return
	}

	if p.Kind == wire.KindReverse {
		// A reverse stream names one of our listeners; the reverse path owns
		// the lookup and the dial. It has already consumed the preamble, so it
		// gets the parsed value rather than re-reading the stream.
		m.serveReverseParsed(ctx, st, p)
		return
	}

	// Rate limit before policy, because a scanner is cheap to refuse and we do
	// not want it consuming resolver work either.
	if m.bucket != nil && !m.bucket.Allow() {
		m.reject(st, wire.StatusTooMany, "stream rate limit reached")
		return
	}

	// Policy first, dial second. Reversing these would turn a refused request
	// into a real outbound connection, giving the peer a port scanner whose
	// answers are in the status codes and the timing.
	ip, err := m.cfg.Allow.Permits(ctx, p.Addr)
	if err != nil {
		m.log.Info("denied forward request", "target", p.Addr, "reason", err)
		m.reject(st, wire.StatusDenied, err.Error())
		return
	}

	target, err := m.dialApproved(ctx, ip, p.Addr)
	if err != nil {
		m.log.Info("forward dial failed", "target", p.Addr, "err", err)
		m.reject(st, wire.StatusDialFail, "target unreachable")
		return
	}
	defer target.Close()

	if err := wire.WriteStatus(st, wire.StatusOK, ""); err != nil {
		return
	}
	m.log.Info("forward established", "target", p.Addr, "stream", streamID(st))

	if err := pipe(st, target, m.cfg.Limits.PipeDrain); err != nil && !isClosedErr(err) {
		m.log.Debug("forward pipe ended", "target", p.Addr, "err", err)
	}
}

// dialApproved dials exactly the authorised IP.
//
// The port comes from the original request string, but the host is the pinned
// address Permits returned. Dialling the hostname again would reopen the
// check-then-dial window that Permits exists to close.
func (m *Manager) dialApproved(ctx context.Context, ip net.IP, requested string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(requested)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, m.cfg.Limits.DialTimeout)
	defer cancel()
	return m.dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(ip.String(), port))
}

// reject writes a status and closes. The write is best-effort: the peer may
// already be gone, and that is not worth logging as a failure.
func (m *Manager) reject(st Stream, s wire.Status, reason string) {
	_ = st.SetDeadline(time.Now().Add(2 * time.Second))
	_ = wire.WriteStatus(st, s, reason)
	_ = st.Close()
}

// AddLocal installs one -L listener.
//
// The listener is created once and survives reconnects. Rebuilding it per
// session would briefly unbind the port, which both drops connections and races
// any other process trying to take it.
func (m *Manager) AddLocal(sp Spec) (*LocalForward, error) {
	lf, err := newLocalForward(sp, m)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = lf.Close()
		return nil, errors.New("forward: manager is closed")
	}
	m.locals = append(m.locals, lf)
	m.mu.Unlock()
	return lf, nil
}

// Close shuts down every listener this manager owns.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.closeCh)
	locals := m.locals
	m.locals = nil
	m.mu.Unlock()

	var errs []error
	for _, lf := range locals {
		if err := lf.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	m.CloseReverseListeners()
	return errors.Join(errs...)
}

// Done is closed when the manager shuts down.
func (m *Manager) Done() <-chan struct{} { return m.closeCh }

// streamID is a best-effort identifier for logs.
func streamID(st Stream) string {
	if s, ok := st.(interface{ ID() uint32 }); ok {
		return fmt.Sprintf("%d", s.ID())
	}
	return "?"
}
