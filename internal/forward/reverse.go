// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package forward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/muzimu217/container-mesh/internal/mux"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// ErrUnknownListener is returned when a reverse stream names a listener this
// side never registered.
var ErrUnknownListener = errors.New("forward: unknown reverse listener")

// ReverseForward is the requester's view of one -R mapping.
type ReverseForward struct {
	spec Spec
}

// Spec reports the mapping that was requested.
func (r *ReverseForward) Spec() Spec { return r.spec }

// PrepareReverse records the local targets a -R mapping will serve.
//
// It does not bind anything: the binding happens on the peer. What this side
// keeps is the ID -> target table, so an inbound reverse stream can be served.
// Registering the same ID twice replaces the target, which is what a reconnect
// does.
func (m *Manager) PrepareReverse(specs []Spec) error {
	for _, sp := range specs {
		if !sp.Reverse {
			return errors.New("forward: PrepareReverse called with a -L spec")
		}
		m.mu.Lock()
		m.reverse[sp.ID] = sp.Target
		m.mu.Unlock()
	}
	return nil
}

// ReverseSpecs turns -R specs into the wire form the peer needs.
func ReverseSpecs(specs []Spec) ([]wire.ForwardListenSpec, error) {
	out := make([]wire.ForwardListenSpec, 0, len(specs))
	for _, sp := range specs {
		host, portStr, err := net.SplitHostPort(sp.Bind)
		if err != nil {
			return nil, fmt.Errorf("forward: %q: %w", sp.Bind, err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("forward: %q: %w", sp.Bind, err)
		}
		out = append(out, wire.ForwardListenSpec{ID: sp.ID, Host: host, Port: port})
	}
	return out, nil
}

// ServeReverse handles one inbound reverse stream.
//
// The preamble has already been read by Handle (it needs the kind to route), so
// it is passed in rather than re-read.
//
// The rule that makes -R safe: the preamble's address selects a listener ID, and
// the ID is looked up in this side's own configuration. The peer cannot name an
// address we dial. A miss is a refusal, not a fallback.
func (m *Manager) serveReverseParsed(ctx context.Context, st Stream, p wire.Preamble) {
	defer st.Close()

	if !m.cfg.AcceptReverse {
		m.reject(st, wire.StatusDenied, "reverse forwarding is disabled (--accept-reverse)")
		return
	}

	m.mu.Lock()
	target, ok := m.reverse[p.Addr]
	m.mu.Unlock()
	if !ok {
		m.log.Info("reverse stream for an unknown listener", "id", p.Addr)
		m.reject(st, wire.StatusDenied, fmt.Sprintf("unknown reverse listener %q", p.Addr))
		return
	}

	if m.bucket != nil && !m.bucket.Allow() {
		m.reject(st, wire.StatusTooMany, "stream rate limit reached")
		return
	}

	// The target is from local configuration, so it is authorised by
	// construction: there is no allow-list check because the operator wrote
	// this line on this machine.
	conn, err := m.dialConfigured(ctx, target)
	if err != nil {
		m.log.Info("reverse dial failed", "target", target, "err", err)
		m.reject(st, wire.StatusDialFail, "local target unreachable")
		return
	}
	defer conn.Close()

	if err := wire.WriteStatus(st, wire.StatusOK, ""); err != nil {
		return
	}
	m.log.Info("reverse forward established", "listener", p.Addr, "target", target)

	if err := pipe(st, conn, m.cfg.Limits.PipeDrain); err != nil && !isClosedErr(err) {
		m.log.Debug("reverse pipe ended", "target", target, "err", err)
	}
}

// dialConfigured dials a target that came from this side's own configuration.
func (m *Manager) dialConfigured(ctx context.Context, target string) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, m.cfg.Limits.DialTimeout)
	defer cancel()
	return m.dialer.DialContext(dialCtx, "tcp", target)
}

// BindReverseListeners binds the listeners a peer asked for and returns one
// status per spec.
//
// Every spec is validated and checked against policy before anything is bound:
// a bind is a side effect on this host, and a refused request must not produce
// one.
func (m *Manager) BindReverseListeners(provider Provider, specs []wire.ForwardListenSpec) []wire.ForwardListenStatus {
	out := make([]wire.ForwardListenStatus, 0, len(specs))
	if !m.cfg.AcceptReverse {
		for _, sp := range specs {
			out = append(out, wire.ForwardListenStatus{
				ID: sp.ID, Status: wire.StatusDenied,
				Message: "reverse forwarding is disabled (--accept-reverse)",
			})
		}
		return out
	}
	for _, sp := range specs {
		out = append(out, m.bindOne(provider, sp))
	}
	return out
}

// bindOne applies policy and, if it passes, binds one listener.
func (m *Manager) bindOne(provider Provider, sp wire.ForwardListenSpec) wire.ForwardListenStatus {
	if err := wire.ValidateForwardListen(sp); err != nil {
		return wire.ForwardListenStatus{ID: sp.ID, Status: wire.StatusBadRequest, Message: err.Error()}
	}
	if !m.cfg.ListenPorts.Contains(sp.Port) {
		return wire.ForwardListenStatus{
			ID: sp.ID, Status: wire.StatusDenied,
			Message: fmt.Sprintf("port %d is not in --allow-listen-port", sp.Port),
		}
	}
	// A non-loopback bind is a much bigger permission: it exposes the port to
	// the network, not just to local processes. It must be asked for twice —
	// once by the port whitelist and once by --listen-all.
	if !isLoopback(sp.Host) && !m.cfg.ListenAll {
		return wire.ForwardListenStatus{
			ID: sp.ID, Status: wire.StatusDenied,
			Message: fmt.Sprintf("binding %s needs --listen-all on this side", sp.Host),
		}
	}

	addr := net.JoinHostPort(sp.Host, strconv.Itoa(sp.Port))

	// Re-registering the same ID is what a reconnect does. The old listener
	// must be closed BEFORE the new bind, not after: two listeners cannot hold
	// one port, so binding first would always fail with "address already in
	// use" and leave the peer unable to re-register after any reconnect.
	m.mu.Lock()
	old := m.revListeners[sp.ID]
	m.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return wire.ForwardListenStatus{
			ID: sp.ID, Status: wire.StatusBindFail,
			Message: fmt.Sprintf("cannot bind %s: %v", addr, err),
		}
	}

	rl := &reverseListener{id: sp.ID, addr: addr, ln: ln, mgr: m, provider: provider}
	m.mu.Lock()
	if m.revListeners == nil {
		m.revListeners = make(map[string]*reverseListener)
	}
	m.revListeners[sp.ID] = rl
	m.mu.Unlock()

	go rl.serve()
	m.log.Info("reverse listener bound", "id", sp.ID, "addr", addr)
	return wire.ForwardListenStatus{ID: sp.ID, Status: wire.StatusOK}
}

// reverseListener is one listener hosted on this side at the peer's request.
type reverseListener struct {
	id       string
	addr     string
	ln       net.Listener
	mgr      *Manager
	provider Provider

	closeOnce sync.Once
	closeErr  error
}

// serve accepts connections and hands each to the peer over a new reverse
// stream.
//
// The direction is the mirror of -L: an application connected to *our* port, so
// we must tell the peer which listener fired and let it dial its own target.
func (rl *reverseListener) serve() {
	for {
		conn, err := rl.ln.Accept()
		if err != nil {
			select {
			case <-rl.mgr.Done():
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			rl.mgr.log.Info("reverse listener accept failed", "id", rl.id, "err", err)
			return
		}
		go rl.handle(conn)
	}
}

// handle serves one connection accepted on a reverse listener.
func (rl *reverseListener) handle(conn net.Conn) {
	defer conn.Close()
	m := rl.mgr

	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Limits.HandshakeTimeout)
	defer cancel()

	ms, err := rl.provider.Current(ctx)
	if err != nil {
		m.log.Info("no session for reverse connection", "id", rl.id, "err", err)
		return
	}
	st, err := ms.Open(ctx, mux.KindForward)
	if err != nil {
		m.log.Info("could not open reverse stream", "id", rl.id, "err", err)
		return
	}
	defer st.Close()

	_ = st.SetDeadline(time.Now().Add(m.cfg.Limits.HandshakeTimeout))
	// The address is the listener ID, never a target: the peer looks it up in
	// its own configuration.
	if err := wire.WritePreamble(st, wire.Preamble{
		Version: wire.PreambleVersion,
		Kind:    wire.KindReverse,
		Addr:    rl.id,
	}); err != nil {
		return
	}
	status, reason, err := wire.ReadStatus(st)
	if err != nil {
		m.log.Info("no status for reverse stream", "id", rl.id, "err", err)
		return
	}
	if status != wire.StatusOK {
		m.log.Warn("peer refused reverse forward",
			"id", rl.id, "status", status.String(), "reason", reason)
		return
	}
	_ = st.SetDeadline(time.Time{})

	m.log.Debug("reverse connected", "id", rl.id, "from", conn.RemoteAddr())
	if err := pipe(st, conn, m.cfg.Limits.PipeDrain); err != nil && !isClosedErr(err) {
		m.log.Debug("reverse pipe ended", "id", rl.id, "err", err)
	}
}

// isLoopback reports whether host names a loopback address.
func isLoopback(host string) bool {
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// A hostname is not accepted for a bind: resolving it would let DNS decide
	// which interface we expose, which is the opposite of an explicit choice.
	return host == "localhost"
}

// CloseReverseListeners unbinds everything registered by a peer.
func (m *Manager) CloseReverseListeners() {
	m.mu.Lock()
	ls := m.revListeners
	m.revListeners = nil
	m.mu.Unlock()
	for _, rl := range ls {
		_ = rl.Close()
	}
}

// Close stops the listener.
func (rl *reverseListener) Close() error {
	rl.closeOnce.Do(func() {
		rl.closeErr = rl.ln.Close()
	})
	return rl.closeErr
}
