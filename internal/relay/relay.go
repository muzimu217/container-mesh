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
	"sync/atomic"
	"time"

	"github.com/muzimu217/container-mesh/internal/wsconn"
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
	// MaxConnsPerIP caps concurrent connections from one source IP. It bounds
	// room-squatting and connection floods from a single host (0 = default).
	MaxConnsPerIP int
	// ConnectRatePerMin limits new connections per source IP per minute
	// (0 = default).
	ConnectRatePerMin int
	// TrustedProxies lists CIDRs whose X-Forwarded-For / X-Real-IP headers are
	// believed. Empty means "trust nobody": the client address is always taken
	// from the socket.
	//
	// This matters because those headers are attacker-controlled. A relay that
	// honours them unconditionally lets any client claim a fresh identity per
	// request, which defeats the per-IP limits entirely (measured: 60/60
	// requests through with forged headers versus 5/60 without).
	TrustedProxies []string
	// LimiterSweepInterval bounds how often idle limiter entries are dropped
	// (0 = default). Without it the map grows once per distinct claimed
	// address, which is a remotely triggerable memory leak.
	LimiterSweepInterval time.Duration
	// TLSCert and TLSKey are paths to a certificate and key. When both are
	// set the relay serves HTTPS/WSS directly, which lets it own port 443
	// without a reverse proxy in front.
	//
	// This matters for the deployment that motivated it: a container behind a
	// restrictive egress policy can usually reach 443 and almost nothing else,
	// so terminating TLS here removes both a port requirement and a hop.
	TLSCert string
	TLSKey  string
	// Logger receives structured events.
	Logger *slog.Logger
}

// DefaultConfig returns a usable configuration.
func DefaultConfig() Config {
	l := wsconn.DefaultLiveness()
	return Config{
		Addr:                 ":8080",
		Path:                 "/ws",
		PairTimeout:          10 * time.Minute,
		MaxRooms:             1024,
		MaxMessageBytes:      16 << 20,
		PingPeriod:           l.PingEvery,
		IdleTimeout:          l.IdleTimeout,
		MaxConnsPerIP:        16,
		ConnectRatePerMin:    120,
		LimiterSweepInterval: time.Minute,
	}
}

// Server is the rendezvous HTTP server.
type Server struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	rooms map[string]*room
	// contested tracks rooms where peers are evicting each other in lockstep.
	// It lives on the Server because a room entry is deleted as soon as two
	// peers pair, which is exactly when the race repeats.
	contested map[string]contestState

	limiter *ipLimiter

	// Stats. These are counters rather than derived values because the room
	// map is not a census of activity: an entry is deleted the moment two
	// peers pair, so len(rooms) counts peers still waiting, not sessions
	// running. A dashboard that reported it as "connections" would show zero
	// during exactly the traffic it exists to display.
	startedAt       time.Time
	activeSessions  atomic.Int64
	bytesRelayed    atomic.Int64
	messagesRelayed atomic.Int64
}

// contestState records recent take-overs for one room label.
type contestState struct {
	evictions int
	until     time.Time
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

// maxEvictionsPerRoom is how many take-overs a room tolerates before it starts
// refusing new connections for a moment. Two well-behaved peers need at most a
// couple (one per real reconnect); a higher count means the sides are racing.
const maxEvictionsPerRoom = 4

// contestedWindow is how long a room refuses take-overs after the budget is
// exhausted. Long enough for the clients' backoff to spread out.
const contestedWindow = 5 * time.Second

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
	if cfg.MaxConnsPerIP == 0 {
		cfg.MaxConnsPerIP = def.MaxConnsPerIP
	}
	if cfg.ConnectRatePerMin == 0 {
		cfg.ConnectRatePerMin = def.ConnectRatePerMin
	}
	if cfg.LimiterSweepInterval == 0 {
		cfg.LimiterSweepInterval = def.LimiterSweepInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{
		cfg:       cfg,
		log:       cfg.Logger,
		rooms:     make(map[string]*room),
		contested: make(map[string]contestState),
		limiter:   newIPLimiter(cfg.ConnectRatePerMin, cfg.MaxConnsPerIP),
		startedAt: time.Now(),
	}
}

// Handler returns the HTTP mux for the relay.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(s.cfg.Path, s.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	// /status is JSON for machines, /dashboard is the page for humans. Both are
	// read-only and expose no room labels, addresses or codes.
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/dashboard", s.handleDashboard)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "container-mesh relay\nendpoint: %s?room=<code>&role=A|B\nstatus:   /status\ndashboard: /dashboard\n", s.cfg.Path)
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
//
// When TLSCert and TLSKey are configured it serves WSS directly, so a client
// can use wss://host/ws with no reverse proxy in between.
func (s *Server) ListenAndServe() error {
	s.StartMaintenance()
	hs := s.HTTPServer()
	if s.cfg.TLSCert != "" && s.cfg.TLSKey != "" {
		s.log.Info("relay listening (TLS)", "addr", s.cfg.Addr, "path", s.cfg.Path)
		return hs.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
	}
	s.log.Info("relay listening", "addr", s.cfg.Addr, "path", s.cfg.Path)
	return hs.ListenAndServe()
}

// StartMaintenance begins periodic cleanup of limiter and contest state.
//
// It must run in production: without it the limiter map grows once per distinct
// client address seen and never shrinks, which a remote client can drive by
// opening connections from many addresses (or, before the trusted-proxy fix,
// by forging a new address per request).
func (s *Server) StartMaintenance() {
	interval := s.cfg.LimiterSweepInterval
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			// Keep entries for a few intervals so an active client is never
			// forgotten mid-burst.
			s.limiter.sweep(4 * interval)
			s.sweepContested()
		}
	}()
}

// sweepContested drops expired contest records so the map cannot grow without
// bound on a long-running relay. Called opportunistically from handleWS.
func (s *Server) sweepContested() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for room, st := range s.contested {
		if st.evictions == 0 && now.After(st.until) {
			delete(s.contested, room)
		}
	}
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

	s.sweepContested()

	ip := s.limiter.clientIP(r)
	if !s.limiter.acquire(ip) {
		// The relay is unauthenticated by design (the room label is not a
		// credential), so per-IP budgets are the only thing standing between a
		// single host and a room-exhaustion or squatting flood.
		s.log.Warn("rate limited", "ip", ip, "room", redactRoom(roomID))
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	defer s.limiter.release(ip)

	c, err := wsconn.Upgrade(w, r)
	if err != nil {
		s.log.Warn("upgrade failed", "err", err)
		return
	}
	c.EnableLiveness(s.liveness())

	partner, self, err := s.join(r.Context(), roomID, role, c)
	if err != nil {
		_ = c.Close()
		s.log.Warn("join failed", "room", redactRoom(roomID), "role", role, "err", err)
		return
	}
	if partner == nil {
		// This peer was the one already waiting; join returned after the
		// splice finished, so there is nothing left to do.
		return
	}
	s.log.Info("paired", "room", redactRoom(roomID), "role", role, "remote", c.RemoteAddr())

	// Exactly one side runs the splice: the peer that completed the room.
	// Anything the parked probe captured from either side is replayed first,
	// so a client that spoke before its partner arrived is not dropped.
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
			// Mutual eviction guard. Two peers that redial on similar timers
			// can displace each other indefinitely: A arrives, kicks B; B
			// redials, kicks A; repeat. Once a room has seen too many
			// take-overs, refuse new arrivals for a short window so both
			// clients back off and their retries decorrelate. Without this the
			// room never converges (observed as two sides retrying forever
			// against a healthy relay).
			st := s.contested[roomID]
			if time.Now().Before(st.until) {
				s.mu.Unlock()
				s.log.Warn("room contested; refusing take-over",
					"room", redactRoom(roomID), "role", role, "evictions", st.evictions)
				return nil, nil, errors.New("room is contested; retry after backoff")
			}
			st.evictions++
			if st.evictions >= maxEvictionsPerRoom {
				st.until = time.Now().Add(contestedWindow)
				st.evictions = 0
			}
			s.contested[roomID] = st

			delete(rm.roles, role)
			if len(rm.roles) == 0 {
				delete(s.rooms, roomID)
			}
			s.mu.Unlock()
			s.log.Info("replacing parked peer", "room", redactRoom(roomID), "role", role)
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

// redactRoom shortens a room label for logs. Room labels are derived from the
// shared code (see secure.RoomID); they are not the code itself, but a short
// prefix is enough to correlate log lines and keeps the full label out of
// access logs and log aggregators.
func redactRoom(room string) string {
	const keep = 8
	if len(room) <= keep {
		return room
	}
	return room[:keep] + "…"
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
	s.activeSessions.Add(1)
	defer s.activeSessions.Add(-1)

	var once sync.Once
	shutdown := func(reason string) {
		once.Do(func() {
			s.log.Info("room closed", "room", roomID, "reason", reason)
			_ = a.Close()
			_ = b.Close()
		})
	}

	// forwarded counts messages actually delivered in each direction. It is
	// what distinguishes "the peer died before we ever spoke" (recoverable:
	// put the survivor back in the room) from "the session ran and then
	// ended" (not recoverable: the stream position is lost).
	var (
		forwardedMu sync.Mutex
		forwarded   [2]int
	)

	done := make(chan struct{}, 2)
	pump := func(src, dst *wsconn.Conn, dir string, idx int) {
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
			forwardedMu.Lock()
			forwarded[idx]++
			forwardedMu.Unlock()
			s.messagesRelayed.Add(1)
			s.bytesRelayed.Add(int64(len(msg)))
		}
	}

	go pump(a, b, "A->B", 0)
	pump(b, a, "B->A", 1)
	<-done
	shutdown("pump finished")

	forwardedMu.Lock()
	total := forwarded[0] + forwarded[1]
	forwardedMu.Unlock()

	// A pairing that failed before a single byte moved means one side was
	// already gone when it was paired (a parked peer that died is invisible
	// until its slot is taken over). Both connections are now closed, so the
	// live side cannot be handed back as-is; the useful thing is to tell it to
	// retry immediately instead of waiting out its backoff, which the client
	// does on a normal close anyway.
	if total == 0 {
		s.log.Info("pairing produced no traffic; both sides closed",
			"room", redactRoom(roomID))
	}
}
