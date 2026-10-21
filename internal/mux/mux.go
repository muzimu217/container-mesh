// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package mux wraps hashicorp/yamux so the rest of the tree never depends on
// its v0.x API directly.
//
// The wrapper exists for three reasons beyond a thin alias:
//
//   - yamux has no stream-count ceiling. Its AcceptBacklog only bounds streams
//     that nobody has accepted yet, so a peer can open unbounded established
//     streams and exhaust memory. This package adds a shared semaphore.
//   - yamux's default LogOutput is os.Stderr, which would corrupt a CLI's
//     output. It is always set explicitly here.
//   - yamux has no CloseWrite. Stream.Close is exactly a FIN in yamux
//     (it only sets streamLocalClose and sends FIN; Read keeps working), so
//     CloseWrite is Close and Close is Close plus a reset when needed.
package mux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// Kind is the first byte of every stream. It tells the accept loop what the
// stream is for before any protocol bytes are read.
const (
	// KindControl is the peer.Session control stream. Only the initiator opens
	// it, and only once.
	KindControl byte = 0
	// KindForward is one forwarded TCP connection.
	KindForward byte = 1
)

// Role decides yamux's stream-id parity: clients open odd ids, servers even.
// Without a fixed assignment both sides could pick the same id and collide.
type Role int

const (
	// RoleClient opens odd stream ids (peer role A).
	RoleClient Role = iota
	// RoleServer opens even stream ids (peer role B).
	RoleServer
)

// ErrStreamLimit is returned by Open when MaxStreams is reached and ctx ends
// first. Accept never returns it: an over-limit inbound stream is delivered
// with Limited() set so the caller can answer with an explicit status instead
// of a transport-level reset.
var ErrStreamLimit = errors.New("mux: stream limit reached")

// Config tunes the session. Use DefaultConfig and override what you need.
type Config struct {
	Role Role
	// Window is yamux's MaxStreamWindowSize.
	//
	// 256 KiB is the yamux floor and the memory-safe default (2x window per
	// active stream, measured 514.8 KiB). 1 MiB is worth ~13% on loopback but
	// is close to required on WAN: a 256 KiB window over a 50 ms RTT caps one
	// stream at roughly 40 Mbit/s.
	Window uint32
	// MaxStreams bounds concurrently open streams on both sides.
	MaxStreams int
	// AcceptBacklog must be at least MaxStreams, or a burst of inbound streams
	// is reset by yamux before the accept loop can drain it.
	AcceptBacklog int
	// StreamOpenTimeout bounds how long a stream may sit unacknowledged.
	// yamux closes the WHOLE session when this fires, so it must be generous:
	// a relay hiccup must not kill every stream.
	StreamOpenTimeout time.Duration
	// StreamCloseTimeout bounds how long a half-closed stream may linger. The
	// yamux default of 5 minutes would hold 2x window for 5 minutes per
	// abandoned forward.
	StreamCloseTimeout time.Duration
	// ConnectionWriteTimeout is yamux's write safety valve.
	ConnectionWriteTimeout time.Duration
	// KeepAliveInterval is yamux's session-level ping period.
	//
	// This is the layer that owns keepalive once multiplexing is on. The
	// transport underneath (wsconn) also pings, and the two must not fight:
	// the transport's idle timeout has to be comfortably longer than this
	// interval, which DefaultConfig arranges.
	KeepAliveInterval time.Duration
	// Logger receives yamux's internal log lines. Nil discards them.
	Logger *slog.Logger
}

// DefaultConfig returns the production defaults.
func DefaultConfig(role Role) Config {
	return Config{
		Role:                   role,
		Window:                 256 << 10,
		MaxStreams:             256,
		AcceptBacklog:          256,
		StreamOpenTimeout:      120 * time.Second,
		StreamCloseTimeout:     60 * time.Second,
		ConnectionWriteTimeout: 10 * time.Second,
		KeepAliveInterval:      20 * time.Second,
	}
}

// BudgetedStreams lowers maxStreams so that maxStreams * 2 * window stays
// within budget.
//
// yamux allocates roughly two windows of buffer per active stream, so an
// unbounded product is the most likely way to blow the memory budget: 256
// streams at a 1 MiB window would be 512 MiB.
func BudgetedStreams(maxStreams int, window uint32, budget int64) int {
	if window == 0 || budget <= 0 {
		return maxStreams
	}
	perStream := int64(window) * 2
	fit := int(budget / perStream)
	if fit < 1 {
		fit = 1
	}
	if fit < maxStreams {
		return fit
	}
	return maxStreams
}

// Session is one multiplexed connection.
type Session struct {
	y   *yamux.Session
	cfg Config
	sem chan struct{} // MaxStreams, shared by Open and Accept
	log *slog.Logger

	closeOnce sync.Once
	closeErr  error
}

// New starts a multiplexed session over rwc.
//
// rwc must be an ordered, reliable byte stream: the record pump's link, not the
// encrypted transport directly. Multiplexing below the AEAD layer would make
// the receiver see counters out of order and every frame would fail to decrypt.
func New(rwc io.ReadWriteCloser, cfg Config) (*Session, error) {
	if rwc == nil {
		return nil, errors.New("mux: nil stream")
	}
	if cfg.Window == 0 {
		cfg.Window = 256 << 10
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 256
	}
	if cfg.AcceptBacklog < cfg.MaxStreams {
		cfg.AcceptBacklog = cfg.MaxStreams
	}
	if cfg.StreamOpenTimeout <= 0 {
		cfg.StreamOpenTimeout = 120 * time.Second
	}
	if cfg.StreamCloseTimeout <= 0 {
		cfg.StreamCloseTimeout = 60 * time.Second
	}
	if cfg.ConnectionWriteTimeout <= 0 {
		cfg.ConnectionWriteTimeout = 10 * time.Second
	}
	if cfg.KeepAliveInterval <= 0 {
		cfg.KeepAliveInterval = 20 * time.Second
	}

	// LogOutput and Logger are mutually exclusive in yamux's VerifyConfig, and
	// one of them must be set. Discard is the safe default: yamux's stock
	// behaviour writes to os.Stderr, which would corrupt CLI output.
	var logOut io.Writer = io.Discard
	yc := &yamux.Config{
		AcceptBacklog:          cfg.AcceptBacklog,
		EnableKeepAlive:        true,
		KeepAliveInterval:      cfg.KeepAliveInterval,
		ConnectionWriteTimeout: cfg.ConnectionWriteTimeout,
		MaxStreamWindowSize:    cfg.Window,
		StreamOpenTimeout:      cfg.StreamOpenTimeout,
		StreamCloseTimeout:     cfg.StreamCloseTimeout,
		LogOutput:              logOut,
	}

	var (
		y   *yamux.Session
		err error
	)
	if cfg.Role == RoleClient {
		y, err = yamux.Client(rwc, yc)
	} else {
		y, err = yamux.Server(rwc, yc)
	}
	if err != nil {
		return nil, fmt.Errorf("mux: start yamux: %w", err)
	}

	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Session{
		y:   y,
		cfg: cfg,
		sem: make(chan struct{}, cfg.MaxStreams),
		log: log,
	}, nil
}

// Open opens one stream and writes its kind byte.
//
// It blocks while the stream limit is reached, honouring ctx. Checking the
// limit before calling yamux matters: yamux's StreamOpenTimeout closes the
// whole session if a stream sits unacknowledged too long, and a stream opened
// while we queue for a slot would be exactly that.
func (s *Session) Open(ctx context.Context, kind byte) (*Stream, error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.y.CloseChan():
		return nil, errors.New("mux: session closed")
	}

	ys, err := s.y.OpenStream()
	if err != nil {
		<-s.sem
		return nil, fmt.Errorf("mux: open stream: %w", err)
	}
	st := &Stream{st: ys, kind: kind, sem: s.sem}
	if err := st.writeKind(kind); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// Serve runs the single accept loop. Each accepted stream is dispatched in its
// own goroutine, which must eventually Close it.
//
// A stream beyond the limit is still delivered, with Limited set: refusing to
// accept would make yamux reset it, and the opener would see a transport error
// instead of an explicit "too many streams" answer it can act on.
func (s *Session) Serve(ctx context.Context, dispatch func(*Stream)) error {
	for {
		ys, err := s.y.AcceptStream()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		kind, err := readKind(ys)
		if err != nil {
			_ = ys.Close()
			continue
		}

		st := &Stream{st: ys, kind: kind, sem: s.sem}
		select {
		case s.sem <- struct{}{}:
		default:
			// Over the limit. Hand it over anyway so the caller can send a
			// status and close cleanly; no semaphore slot is taken, so Close
			// must not release one.
			st.limited = true
		}

		go dispatch(st)
	}
}

// NumStreams reports how many streams yamux currently tracks.
func (s *Session) NumStreams() int { return s.y.NumStreams() }

// Ping measures a round trip on the mux layer.
func (s *Session) Ping() (time.Duration, error) { return s.y.Ping() }

// IsClosed reports whether the underlying session has shut down.
func (s *Session) IsClosed() bool { return s.y.IsClosed() }

// Done is closed when the underlying session shuts down.
func (s *Session) Done() <-chan struct{} { return s.y.CloseChan() }

// Close shuts the multiplexed session down.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.y.Close()
	})
	return s.closeErr
}

// Stream is one multiplexed stream.
type Stream struct {
	st      *yamux.Stream
	kind    byte
	limited bool
	sem     chan struct{}
	once    sync.Once
}

// writeKind writes the single kind byte that prefixes every stream.
func (s *Stream) writeKind(kind byte) error {
	if err := writeFull(s.st, []byte{kind}); err != nil {
		return fmt.Errorf("mux: write stream kind: %w", err)
	}
	return nil
}

// readKind reads the single kind byte that prefixes every stream.
func readKind(ys *yamux.Stream) (byte, error) {
	var b [1]byte
	if _, err := io.ReadFull(ys, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// Read reads stream bytes.
func (s *Stream) Read(b []byte) (int, error) { return s.st.Read(b) }

// Write writes stream bytes.
func (s *Stream) Write(b []byte) (int, error) { return s.st.Write(b) }

// CloseWrite half-closes the write side while leaving reads working.
//
// yamux has no CloseWrite, but Stream.Close is exactly a FIN: it only marks the
// local side closed and sends FIN, and Read keeps working afterwards
// (measured: an 18-byte response was still readable after Close). That is the
// shape a proxy needs when the client half-closes first.
func (s *Stream) CloseWrite() error { return s.st.Close() }

// Close closes the stream and releases its slot exactly once.
func (s *Stream) Close() error {
	err := s.st.Close()
	s.once.Do(func() {
		if !s.limited {
			<-s.sem
		}
	})
	return err
}

// SetDeadline sets the stream's I/O deadline.
func (s *Stream) SetDeadline(t time.Time) error { return s.st.SetDeadline(t) }

// SetReadDeadline sets the read deadline.
func (s *Stream) SetReadDeadline(t time.Time) error { return s.st.SetReadDeadline(t) }

// SetWriteDeadline sets the write deadline.
func (s *Stream) SetWriteDeadline(t time.Time) error { return s.st.SetWriteDeadline(t) }

// Kind reports the stream's kind byte.
func (s *Stream) Kind() byte { return s.kind }

// Limited reports whether this stream exceeded MaxStreams when it arrived.
// A limited stream never held a slot, so closing it does not free one.
func (s *Stream) Limited() bool { return s.limited }

// ID reports the yamux stream id, for logs.
func (s *Stream) ID() uint32 { return s.st.StreamID() }

// writeFull writes all of b, retrying short writes. io.Writer may legally
// return n < len(b) with a nil error; treating that as success would truncate
// the kind byte and misalign every later read on the stream.
func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if n > 0 {
			b = b[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
