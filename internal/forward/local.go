// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package forward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muzimu217/container-mesh/internal/mux"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// Spec is one -L or -R entry after parsing.
type Spec struct {
	// Reverse marks a -R entry.
	Reverse bool
	// Bind is where we listen (for -L) or where we ask the peer to listen
	// (for -R).
	Bind string
	// Target is what gets dialled.
	Target string
	// ID is Bind normalised; -R matches the peer's stream by this.
	ID string
}

// DefaultBind is where -L listens when the user did not name an address.
//
// Loopback is the safe default: a forward that silently bound 0.0.0.0 would
// expose an internal service to every interface on the host, which is the
// opposite of what someone typing "forward my local port" expects.
const DefaultBind = "127.0.0.1"

// ParseSpec parses "[bind:]port:host:port" right-associatively.
//
// Right-associativity is what makes IPv6 work without a separate syntax: the
// last two fields are always the target, so
//
//	8080:db:5432             -> 127.0.0.1:8080 -> db:5432
//	127.0.0.1:8080:db:5432   -> 127.0.0.1:8080 -> db:5432
//	[::1]:8080:db:5432       -> [::1]:8080     -> db:5432
//	8080:[::1]:5432          -> 127.0.0.1:8080 -> [::1]:5432
//
// are all unambiguous. Fewer than three fields is rejected rather than guessed.
func ParseSpec(s string, reverse bool) (Spec, error) {
	fields, err := splitSpec(s)
	if err != nil {
		return Spec{}, err
	}
	if len(fields) < 3 {
		return Spec{}, fmt.Errorf("forward: %q needs at least port:host:port", s)
	}
	targetHost := fields[len(fields)-2]
	targetPort := fields[len(fields)-1]
	if targetHost == "" {
		return Spec{}, fmt.Errorf("forward: %q has an empty target host", s)
	}
	if _, err := parsePort(targetPort); err != nil {
		return Spec{}, fmt.Errorf("forward: target port %q: %w", targetPort, err)
	}

	local := fields[:len(fields)-2]
	var bindHost, bindPort string
	switch len(local) {
	case 1:
		bindHost, bindPort = DefaultBind, local[0]
	case 2:
		bindHost, bindPort = local[0], local[1]
	default:
		// Three local fields means an unbracketed IPv6 address, which we refuse
		// rather than guess: "::1:8080:db:5432" could be read two ways.
		return Spec{}, fmt.Errorf("forward: %q has too many fields before the target; "+
			"bracket IPv6 addresses like [::1]:8080:db:5432", s)
	}
	port, err := parsePort(bindPort)
	if err != nil {
		return Spec{}, fmt.Errorf("forward: local port %q: %w", bindPort, err)
	}
	if bindHost == "" {
		// An empty host in "[:8080]:..." would bind every interface, which must
		// be an explicit choice, never a typo.
		return Spec{}, fmt.Errorf("forward: %q has an empty bind host; "+
			"use 0.0.0.0 explicitly to bind all interfaces", s)
	}
	// bindHost may already carry its brackets ("[::1]"), because splitSpec keeps
	// them to protect the colons. Stripping before JoinHostPort avoids
	// producing "[[::1]]:8080".
	bind := net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(bindHost, "["), "]"), strconv.Itoa(port))
	return Spec{
		Reverse: reverse,
		Bind:    bind,
		Target:  net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(targetHost, "["), "]"), targetPort),
		ID:      bind,
	}, nil
}

// splitSpec splits on colons while keeping bracketed IPv6 literals intact.
func splitSpec(s string) ([]string, error) {
	var (
		fields  []string
		cur     strings.Builder
		inBrack bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '[':
			if inBrack {
				return nil, fmt.Errorf("forward: %q has nested brackets", s)
			}
			inBrack = true
			cur.WriteByte(c)
		case c == ']':
			if !inBrack {
				return nil, fmt.Errorf("forward: %q has an unmatched ]", s)
			}
			inBrack = false
			cur.WriteByte(c)
		case c == ':' && !inBrack:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if inBrack {
		return nil, fmt.Errorf("forward: %q has an unterminated [", s)
	}
	fields = append(fields, cur.String())
	return fields, nil
}

// LocalForward is one -L mapping: a local listener that opens a forwarded
// stream per accepted connection.
type LocalForward struct {
	spec Spec
	mgr  *Manager
	ln   net.Listener

	closeOnce sync.Once
	closeErr  error
}

// NewLocalForTest exposes the local-forward constructor to the external test
// package, which cannot reach the unexported one. It is in the production file
// rather than a _test.go file because Go does not allow the external test
// package to see declarations from an internal test file.
func NewLocalForTest(sp Spec, m *Manager) (*LocalForward, error) { return newLocalForward(sp, m) }

// newLocalForward binds the listener immediately, so a port conflict is a
// startup error rather than a surprise on the first connection.
func newLocalForward(sp Spec, m *Manager) (*LocalForward, error) {
	if sp.Reverse {
		return nil, errors.New("forward: newLocalForward called with a -R spec")
	}
	ln, err := net.Listen("tcp", sp.Bind)
	if err != nil {
		return nil, fmt.Errorf("forward: listen %s: %w", sp.Bind, err)
	}
	return &LocalForward{spec: sp, mgr: m, ln: ln}, nil
}

// Addr reports the bound address, which matters for ":0" style test listeners.
func (lf *LocalForward) Addr() net.Addr { return lf.ln.Addr() }

// Spec reports the mapping this listener serves.
func (lf *LocalForward) Spec() Spec { return lf.spec }

// Serve accepts connections until the listener closes.
//
// Every accepted connection runs on its own goroutine; the accept loop itself
// never blocks on a dial, so one slow target cannot stall the listener.
func (lf *LocalForward) Serve(ctx context.Context, p Provider) error {
	for {
		conn, err := lf.ln.Accept()
		if err != nil {
			select {
			case <-lf.mgr.Done():
				return nil
			case <-ctx.Done():
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}

		// Bound in-flight connections before spawning: a peer that stops
		// answering would otherwise let accepted sockets pile up without limit.
		if !lf.mgr.gate.Acquire() {
			_ = conn.Close()
			lf.mgr.log.Warn("refusing local connection: pending limit reached",
				"limit", lf.mgr.cfg.Limits.MaxPending)
			continue
		}

		go func() {
			defer lf.mgr.gate.Release()
			lf.handleConn(ctx, conn, p)
		}()
	}
}

// handleConn forwards one local connection.
func (lf *LocalForward) handleConn(ctx context.Context, conn net.Conn, p Provider) {
	defer conn.Close()

	m, err := p.Current(ctx)
	if err != nil {
		lf.mgr.log.Info("no session for local connection", "err", err)
		return
	}

	openCtx, cancel := context.WithTimeout(ctx, lf.mgr.cfg.Limits.HandshakeTimeout)
	defer cancel()
	st, err := m.Open(openCtx, mux.KindForward)
	if err != nil {
		lf.mgr.log.Info("could not open forwarded stream", "err", err)
		return
	}
	defer st.Close()

	// Announce the target, then wait for the verdict. Writing the preamble and
	// the status exchange share the handshake budget: a peer that accepts the
	// stream but never answers must not hold this connection forever.
	_ = st.SetDeadline(time.Now().Add(lf.mgr.cfg.Limits.HandshakeTimeout))
	if err := wire.WritePreamble(st, wire.Preamble{
		Version: wire.PreambleVersion,
		Kind:    wire.KindDirect,
		Addr:    lf.spec.Target,
	}); err != nil {
		return
	}
	status, reason, err := wire.ReadStatus(st)
	if err != nil {
		lf.mgr.log.Info("no status from peer", "target", lf.spec.Target, "err", err)
		return
	}
	if status != wire.StatusOK {
		lf.mgr.log.Warn("peer refused forward",
			"target", lf.spec.Target, "status", status.String(), "reason", reason)
		return
	}
	_ = st.SetDeadline(time.Time{})

	lf.mgr.log.Debug("forward connected", "local", conn.RemoteAddr(), "target", lf.spec.Target)
	if err := pipe(st, conn, lf.mgr.cfg.Limits.PipeDrain); err != nil && !isClosedErr(err) {
		lf.mgr.log.Debug("forward closed", "target", lf.spec.Target, "err", err)
	}
}

// Close stops the listener. Connections already accepted keep running; they
// end when their own pipe finishes or the session dies.
func (lf *LocalForward) Close() error {
	lf.closeOnce.Do(func() {
		lf.closeErr = lf.ln.Close()
	})
	return lf.closeErr
}
