// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package relay implements the rendezvous server.
//
// The relay's only job is to introduce two peers that know the same room code
// and then splice their message streams. It cannot read anything the peers
// exchange because every frame is sealed with a key derived from the SPAKE2
// handshake, which the relay never sees.
//
// This is deliberately the same shape as croc's relay, because croc's relay is
// proven to work from the target containers (outbound WebSocket/TCP only).
//
// Robustness: every connection runs WebSocket-level keepalive (see package
// wsconn), so a session whose path dies silently — NAT mapping expired,
// transparent proxy dropped the flow — is torn down within IdleTimeout instead
// of hanging forever. A peer that dies *while parked* (waiting for its
// partner) is handled without any socket probing: when the same code+role
// reconnects, the newcomer evicts the stale waiter ("last writer wins"). The
// room code is the only credential the protocol has, so whoever presents it
// is entitled to the slot; a client that redials is by definition fresher
// than whatever it replaces. Bytes sent by a parked client before its partner
// arrives are legitimate (the handshake may start early) and simply sit in
// the socket buffer until the splice picks them up — nothing here reads them.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/blackevil/container-mesh/internal/wsconn"
)

// Config controls relay behaviour.
type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// Path is the WebSocket endpoint path (default "/ws").
	Path string
	// PairTimeout bounds how long the first peer waits for its partner.
	PairTimeout time.Duration
	// MaxRooms caps concurrent rooms to bound memory.
	MaxRooms int
	// MaxMessageBytes is the per-message ceiling.
	MaxMessageBytes int64
	// PingPeriod is how often the relay pings each client (0 = default).
	PingPeriod time.Duration
	// IdleTimeout closes a client connection with no inbound traffic —
	// including keepalive pongs — for this long (0 = default).
	IdleTimeout time.Duration
	// Logger receives structured events.
	Logger *slog.Logger
}

// DefaultConfig returns a usable configuration.
func DefaultConfig() Config {
	l := wsconn.DefaultLiveness()
	return Config{
		Addr:            ":8080",
		Path:            "/ws",
		PairTimeout:     10 * time.Minute,
		MaxRooms:        1024,
		MaxMessageBytes: 16 << 20,
		PingPeriod:      l.PingEvery,
		IdleTimeout:     l.IdleTimeout,
	}
}

// Server is the rendezvous HTTP server.
type Server struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	rooms map[string]*room
}

// waiter is one peer parked in a room waiting for its partner.
//
// Nothing reads the socket while parked. That is deliberate: gorilla latches
// the first read error and panics after a bounded number of retries, so any
// "probe by reading with a deadline" scheme eventually crashes the relay (both
// variants were observed doing exactly that in a real deployment). Liveness is
// handled by the transport instead — the WebSocket ping ticker closes a
// connection whose pongs stop arriving, and the splice's first read then sees
// the failure immediately. Bytes a client sends before pairing are not lost:
// they sit in the TCP/socket buffer and the splice reads them as soon as it
// starts.
type waiter struct {
	conn *wsconn.Conn
	// paired receives the partner's waiter exactly once.
	paired chan *waiter
	// done is closed when the splice for this connection has finished.
	done chan struct{}
	// evicted is closed when a newer connection with the same role takes the
	// slot; the older waiter gives up instead of being spliced.
	evicted   chan struct{}
	evictOnce sync.Once
	// closed is closed once the transport reports the socket is gone.
	closed chan struct{}
}

func (w *waiter) markEvicted() { w.evictOnce.Do(func() { close(w.evicted) }) }

type room struct {
	id    string
	roles map[string]*waiter
}

// New creates a relay server.
func New(cfg Config) *Server {
	def := DefaultConfig()
	if cfg.Addr == "" {
		cfg.Addr = def.Addr
	}
	if cfg.Path == "" {
		cfg.Path = def.Path
	}
	if cfg.PairTimeout == 0 {
		cfg.PairTimeout = def.PairTimeout
	}
	if cfg.MaxRooms == 0 {
		cfg.MaxRooms = def.MaxRooms
	}
	if cfg.MaxMessageBytes == 0 {
		cfg.MaxMessageBytes = def.MaxMessageBytes
	}
	if cfg.PingPeriod == 0 {
		cfg.PingPeriod = def.PingPeriod
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = def.IdleTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{cfg: cfg, log: cfg.Logger, rooms: make(map[string]*room)}
}

// Handler returns the HTTP mux for the relay.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(s.cfg.Path, s.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "container-mesh relay\nendpoint: %s?room=<code>&role=A|B\n", s.cfg.Path)
	})
	return mux
}

// HTTPServer builds an *http.Server so tests can start it on a random port.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// ListenAndServe runs the relay until the process exits.
func (s *Server) ListenAndServe() error {
	s.log.Info("relay listening", "addr", s.cfg.Addr, "path", s.cfg.Path)
	return s.HTTPServer().ListenAndServe()
}

func (s *Server) liveness() wsconn.Liveness {
	return wsconn.Liveness{PingEvery: s.cfg.PingPeriod, IdleTimeout: s.cfg.IdleTimeout}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	role := r.URL.Query().Get("role")
	if roomID == "" {
		http.Error(w, "missing room", http.StatusBadRequest)
		return
	}
	if role != "A" && role != "B" {
		http.Error(w, "role must be A or B", http.StatusBadRequest)
		return
	}

	c, err := wsconn.Upgrade(w, r)
	if err != nil {
		s.log.Warn("upgrade failed", "err", err)
		return
	}
	c.EnableLiveness(s.liveness())

	partner, self, err := s.join(r.Context(), roomID, role, c)
	if err != nil {
		_ = c.Close()
		s.log.Warn("join failed", "room", roomID, "role", role, "err", err)
		return
	}
	if partner == nil {
		// This peer was the one already waiting; join returned after the
		// splice finished, so there is nothing left to do.
		return
	}
	s.log.Info("paired", "room", roomID, "role", role, "remote", c.RemoteAddr())

	// Exactly one side runs the splice: the peer that completed the room.
	s.splice(roomID, c, partner.conn)
	close(self.done)
}

// join parks the peer until its partner arrives.
//
// A newer connection with the same room+role replaces the parked one
// (last-writer-wins). That is what makes client reconnects recover: a fresh
// dial is by definition more useful than whatever is stuck in the slot, and a
// parked waiter that is actually dead costs nothing to replace.
//
// It returns (partner, self, nil) when the room is now complete, or
// (nil, self, nil) if this peer is the first one and is still waiting.
func (s *Server) join(ctx context.Context, roomID, role string, c *wsconn.Conn) (*waiter, *waiter, error) {
	w := &waiter{
		conn:    c,
		paired:  make(chan *waiter, 1),
		done:    make(chan struct{}),
		evicted: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	other := "B"
	if role == "B" {
		other = "A"
	}

	var partner *waiter
	for {
		s.mu.Lock()
		rm, ok := s.rooms[roomID]
		if !ok {
			if len(s.rooms) >= s.cfg.MaxRooms {
				s.mu.Unlock()
				return nil, nil, errors.New("relay at capacity")
			}
			rm = &room{id: roomID, roles: make(map[string]*waiter, 2)}
			s.rooms[roomID] = rm
		}
		if old, taken := rm.roles[role]; taken {
			delete(rm.roles, role)
			if len(rm.roles) == 0 {
				delete(s.rooms, roomID)
			}
			s.mu.Unlock()
			s.log.Info("replacing parked peer", "room", roomID, "role", role)
			old.markEvicted()
			_ = old.conn.Close()
			continue
		}
		rm.roles[role] = w
		partner = rm.roles[other]
		if partner != nil {
			delete(s.rooms, roomID)
		}
		s.mu.Unlock()
		break
	}

	if partner != nil {
		partner.paired <- w
		return partner, w, nil
	}

	// Watch the socket without consuming its data: when the transport gives up
	// on the connection (pongs stopped arriving), free the room slot instead
	// of holding it until PairTimeout. The wait itself does not read, so the
	// peer's pre-pairing bytes stay in the buffer for the splice.
	go func() {
		<-c.Closed()
		close(w.closed)
	}()

	timer := time.NewTimer(s.cfg.PairTimeout)
	defer timer.Stop()

	select {
	case arriving := <-w.paired:
		// The arriving peer owns the splice. Wait on *its* done channel: when
		// the splice ends it closes that channel, which releases us. Waiting on
		// our own would deadlock because only the splice owner closes it.
		<-arriving.done
		return nil, w, nil
	case <-w.evicted:
		return nil, nil, errors.New("replaced by a newer connection with the same role")
	case <-w.closed:
		s.removeWaiter(roomID, role, w)
		return nil, nil, errors.New("connection closed while waiting for partner")
	case <-timer.C:
		s.removeWaiter(roomID, role, w)
		return nil, nil, fmt.Errorf("timed out after %s waiting for the other peer", s.cfg.PairTimeout)
	case <-ctx.Done():
		s.removeWaiter(roomID, role, w)
		return nil, nil, ctx.Err()
	}
}

// removeWaiter drops a parked peer from its room, deleting the room when it
// becomes empty.
func (s *Server) removeWaiter(roomID, role string, w *waiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.rooms[roomID]
	if !ok {
		return
	}
	if cur.roles[role] == w {
		delete(cur.roles, role)
	}
	if len(cur.roles) == 0 {
		delete(s.rooms, roomID)
	}
}

// splice copies messages between the two peers until either side closes.
//
// The relay is a dumb pipe: it does not parse, buffer or reorder.
func (s *Server) splice(roomID string, a, b *wsconn.Conn) {
	var once sync.Once
	shutdown := func(reason string) {
		once.Do(func() {
			s.log.Info("room closed", "room", roomID, "reason", reason)
			_ = a.Close()
			_ = b.Close()
		})
	}

	done := make(chan struct{}, 2)
	pump := func(src, dst *wsconn.Conn, dir string) {
		defer func() { done <- struct{}{} }()
		for {
			msg, err := src.Recv()
			if err != nil {
				shutdown(dir + " recv: " + err.Error())
				return
			}
			if int64(len(msg)) > s.cfg.MaxMessageBytes {
				shutdown(dir + " message too large")
				return
			}
			if err := dst.Send(msg); err != nil {
				shutdown(dir + " send: " + err.Error())
				return
			}
		}
	}

	go pump(a, b, "A->B")
	pump(b, a, "B->A")
	<-done
	shutdown("pump finished")
}
