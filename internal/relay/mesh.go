// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

// Mesh discovery endpoints, mounted on the same process and mux as the
// stage-1 pairing relay. The discovery node is a member-run rendezvous for
// membership, not a data plane: it holds the member list in plaintext in
// memory (it must, to verify signatures and push views) and never carries
// user traffic (MESH-DESIGN §2.2).
//
// Two endpoints, both authenticated by membership (R-2.4: a stranger who
// learns the address can neither register a presence nor read the roster):
//
//	POST /mesh/register   signed announce (heartbeat)  → 200 / 4xx
//	GET  /mesh/members    member-authenticated WS      → full-snapshot pushes
//
// Logging discipline (T-4.2): mesh handlers log device IDs and outcomes
// only — never endpoints, never signatures.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wsconn"
)

// maxMeshRegisterBytes bounds the register body. An announce is a device ID,
// ≤16 endpoints and a signature; anything beyond a few KiB is not an announce,
// it is an abuse vector. Cap it before the decoder sees it.
const maxMeshRegisterBytes = 64 << 10

// reasonCode maps a rejection to a short stable token for logs. The error
// text itself is for the CLIENT's benefit (actionable errors, O-4); logs get
// the token because the text can carry endpoint strings or timestamps
// (T-4.2, review F6).
func reasonCode(err error) string {
	switch {
	case errors.Is(err, discovery.ErrAnnounceNotMember):
		return "not-member"
	case errors.Is(err, discovery.ErrAnnounceBadSignature):
		return "bad-signature"
	case errors.Is(err, discovery.ErrAnnounceStale):
		return "stale-timestamp"
	case errors.Is(err, discovery.ErrAnnounceBadEndpoint):
		return "bad-endpoint"
	default:
		return "rejected"
	}
}

// safeID sanitizes a client-supplied device ID for logging: valid IDs pass
// as-is (16 lowercase base32 chars); anything else is truncated to 32
// printable characters so a hostile ID cannot inject newlines or megabytes
// into the log (review F6).
func safeID(id identity.DeviceID) string {
	s := id.String()
	if id.Valid() {
		return s
	}
	clean := make([]rune, 0, 32)
	for _, r := range s {
		if len(clean) >= 32 {
			break
		}
		if r >= 0x20 && r != 0x7f {
			clean = append(clean, r)
		}
	}
	return string(clean) + "-invalid"
}

// EnableDiscovery mounts the mesh discovery endpoints on this relay and
// starts the liveness sweep. Call it before HTTPServer()/ListenAndServe so
// the routes land in the mux. The returned node is the caller's handle for
// approvals (SetMembers) and direct state inspection in tests.
//
// The relay keeps its own reference to the member list because announce and
// stream authentication must consult the SAME list the node enforces; they
// are swapped together by SetMeshMembers.
func (s *Server) EnableDiscovery(members *identity.MemberList) *discovery.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disco != nil {
		return s.disco
	}
	s.disco = discovery.NewNode(members)
	s.discoMembers = members
	stop := make(chan struct{})
	s.discoStop = stop
	go func() {
		t := time.NewTicker(discovery.SweepInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				// Reachability-only expiry: a member whose laptop slept is
				// dropped from the ONLINE view, never from membership.
				if dropped := s.disco.Sweep(); len(dropped) > 0 {
					s.log.Info("mesh sweep dropped offline members", "count", len(dropped))
				}
			case <-stop:
				return
			}
		}
	}()
	return s.disco
}

// StopDiscovery halts the sweep loop (tests). Discovery endpoints already
// registered in the mux keep working.
func (s *Server) StopDiscovery() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.discoStop != nil {
		close(s.discoStop)
		s.discoStop = nil
	}
}

// SetMeshMembers replaces the member list everywhere it is consulted: the
// node's enforcement view and both endpoints' authentication lookups. This
// is the approval path for `join` — the operator adds a cert, reloads, and
// every subscriber is pushed the new view.
func (s *Server) SetMeshMembers(m *identity.MemberList) {
	s.mu.Lock()
	s.discoMembers = m
	s.mu.Unlock()
	s.disco.SetMembers(m)
}

// MeshMembers returns the member list the mesh endpoints currently enforce.
// Exposed so CLI-side adapters (the serve-discovery direct-link listener's
// certificate pinning) resolve against the LIVE list across hot reloads,
// not against a snapshot captured at startup.
func (s *Server) MeshMembers() *identity.MemberList {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.discoMembers
}

// DiscoNode returns the discovery node behind the /mesh/* endpoints (nil
// when discovery is not enabled). Diagnostics and tests use it to observe
// liveness bookkeeping (stream counts, reachability) directly.
func (s *Server) DiscoNode() *discovery.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disco
}

// KickStream closes every members-stream connection of a device, forcing it
// to reconnect and re-authenticate. Operations use it to refresh a member's
// view on demand (its endpoints may have changed); tests use it to simulate
// the server-side cleanup of a slow consumer.
func (s *Server) KickStream(id identity.DeviceID) int {
	s.mu.Lock()
	conns := make([]*wsconn.Conn, 0, len(s.streams[id]))
	for c := range s.streams[id] {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return len(conns)
}

// meshSnapshot wires the mux routes. Called from Handler().
func (s *Server) meshSnapshotMux(mux *http.ServeMux) {
	mux.HandleFunc("/mesh/register", s.handleMeshRegister)
	mux.HandleFunc("/mesh/members", s.handleMeshMembers)
}

// meshMemberKey returns the device key recorded for id in the CURRENT member
// list, nil when the device is not a member. An expired certificate is
// reported as not-a-member: membership is what the list records, but a lapsed
// cert must not keep authenticating past its TTL even if the removal has not
// happened yet.
func (s *Server) meshMemberKey(id identity.DeviceID) (ed25519.PublicKey, error) {
	s.mu.Lock()
	m := s.discoMembers
	s.mu.Unlock()
	if m == nil {
		return nil, discovery.ErrAnnounceNotMember
	}
	mm, ok := m.Get(id)
	if !ok {
		return nil, discovery.ErrAnnounceNotMember
	}
	if time.Now().UTC().After(mm.Cert.ExpiresAt) {
		return nil, fmt.Errorf("%w: certificate expired at %s",
			discovery.ErrAnnounceNotMember, mm.Cert.ExpiresAt.Format(time.RFC3339))
	}
	return mm.Cert.DevicePub, nil
}

func (s *Server) handleMeshRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// The relay is unauthenticated at the pairing layer, so per-IP budgets
	// are the first thing a flood hits — same policy as /ws, one limiter.
	ip := s.limiter.clientIP(r)
	if !s.limiter.acquire(ip) {
		s.log.Warn("mesh register rate limited", "ip", ip)
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	defer s.limiter.release(ip)

	var req discovery.RegisterRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMeshRegisterBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "malformed register body", http.StatusBadRequest)
		return
	}

	key, err := s.meshMemberKey(req.Announce.DeviceID)
	if err == nil {
		err = req.Announce.Validate(key, req.Sig, time.Now())
	}
	if err != nil {
		// Log a sanitized device ID and a stable reason TOKEN — the raw
		// error text can embed attacker-supplied endpoint strings or expiry
		// timestamps, which T-4.2 keeps out of logs (review F6).
		s.log.Warn("mesh register rejected", "device", safeID(req.Announce.DeviceID), "reason", reasonCode(err))
		status := http.StatusUnauthorized
		if !errors.Is(err, discovery.ErrAnnounceNotMember) &&
			!errors.Is(err, discovery.ErrAnnounceBadSignature) &&
			!errors.Is(err, discovery.ErrAnnounceStale) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}

	changed := s.disco.Register(req.Announce, time.Now())
	s.log.Info("mesh register", "device", req.Announce.DeviceID, "changed", changed)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "changed": changed})
}

// authenticateMeshMember checks the stream credentials: a member-signed
// binding of MeshAuthContext + device ID + timestamp. Same skew window as
// announces; a captured header set cannot outlive its timestamp, and a
// signature made for a register body cannot satisfy this context.
func (s *Server) authenticateMeshMember(r *http.Request) (identity.DeviceID, error) {
	id := identity.DeviceID(r.Header.Get(discovery.HeaderMeshDevice))
	ts := r.Header.Get(discovery.HeaderMeshTime)
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get(discovery.HeaderMeshSig))
	if err != nil {
		return "", fmt.Errorf("malformed signature encoding")
	}
	if !id.Valid() {
		return "", fmt.Errorf("%w: malformed device id", discovery.ErrAnnounceNotMember)
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return "", fmt.Errorf("%w: malformed timestamp", discovery.ErrAnnounceStale)
	}
	now := time.Now()
	if now.Sub(t) > discovery.AnnounceClockSkew || t.Sub(now) > discovery.AnnounceClockSkew {
		return "", fmt.Errorf("%w: timestamp outside window", discovery.ErrAnnounceStale)
	}
	key, err := s.meshMemberKey(id)
	if err != nil {
		return "", err
	}
	signable := discovery.MeshAuthContext + "\x00" + id.String() + "\x00" + ts
	if !identity.VerifyDevice(key, []byte(signable), sig) {
		return "", fmt.Errorf("%w: stream signature invalid", discovery.ErrAnnounceBadSignature)
	}
	return id, nil
}

func (s *Server) handleMeshMembers(w http.ResponseWriter, r *http.Request) {
	id, err := s.authenticateMeshMember(r)
	if err != nil {
		// Reject BEFORE upgrade: a non-member must not hold a socket, even
		// one that would receive nothing. Reason is logged as a token, not
		// raw error text (T-4.2, review F6).
		s.log.Warn("mesh members rejected", "device", safeID(id), "reason", reasonCode(err))
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	conn, err := wsconn.Upgrade(w, r)
	if err != nil {
		s.log.Warn("mesh members upgrade failed", "err", err)
		return
	}
	// The members stream IS the member's liveness signal (issue #8 #7), so
	// its keepalive is tighter than the pairing relay's: 3 s pings prove
	// liveness three times inside the 10 s idle window — the P-5 acceptance
	// numbers (same as the direct-link QUIC memo). Members are tens; the
	// ping cost is negligible.
	conn.EnableLiveness(wsconn.Liveness{PingEvery: 3 * time.Second, IdleTimeout: 10 * time.Second})

	s.streamMeshMembers(r.Context(), conn, id)
}

// streamMeshMembers pushes full snapshots: one immediately, then one per
// change signal. Every push re-reads the current member list — SetMeshMembers
// may swap it at any moment, and a stream opened before an approval must
// still deliver the post-approval view. The stream doubles as the member's
// fast liveness signal: when it dies (kill -9 closes the socket), the node
// expires the device immediately instead of waiting out the heartbeat
// timeout (P-5's ≤10 s; issue #8 finding #7). Returns when the connection
// dies or the request context is cancelled; deferred unsub + detach prevent
// subscriber/liveness leaks either way.
func (s *Server) streamMeshMembers(ctx context.Context, conn *wsconn.Conn, id identity.DeviceID) {
	ch, unsub := s.disco.Subscribe(id)
	defer unsub()
	s.disco.AttachStream(id)
	defer s.disco.DetachStream(id)

	// Track the connection per device so KickStream can force a reconnect.
	s.mu.Lock()
	if s.streams[id] == nil {
		s.streams[id] = make(map[*wsconn.Conn]struct{})
	}
	s.streams[id][conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams[id], conn)
		s.mu.Unlock()
	}()

	// Drain the read side: the server only SENDS on this stream, so without
	// a reader the peer's close (FIN/close-frame) sits unseen until the next
	// ping tick fails — up to a full ping period of extra "offline" lag, or
	// forever if nobody ever reads. Draining makes a graceful client close
	// surface in one RTT; a hard kill surfaces via the ping/idle deadline
	// above. Discarding whatever arrives is correct: members never send
	// data on this stream (red check: removing this goroutine makes
	// TestStreamCloseExpiresMemberFast red — the push only fires after the
	// ping period instead of immediately).

	// Shared between the monitor goroutine and the send closure: the last
	// snapshot size drives the queue-depth cap's dynamic scaling.
	var lastPushLen int64

	go func() {
		for {
			if _, err := conn.Recv(); err != nil {
				return
			}
		}
	}()

	// Send-queue monitor (issue #8, netns-probe root-cause): the kernel
	// sndbuf absorbs megabytes of backlog, so app-layer write duration
	// stays in microseconds and neither the slow-push kick nor the write
	// deadline can ever fire. Measure the KERNEL queue directly (SIOCOUTQ):
	// two consecutive checks above the cap means the consumer is not
	// draining — kick so it reconnects onto a fresh pipe instead of
	// draining stale snapshots for a minute.
	//
	// The cap is DYNAMIC: max(256 KiB, 2× the last snapshot size). A 44 KB
	// snapshot kicks at 256 KiB (genuinely stuck), but a 1 MB snapshot
	// legitimately needs more kernel buffer — a fixed cap would live-lock
	// on large memberships because the reconnect gets the same snapshot
	// and immediately hits the cap again (netns-probe round 16).
	go func() {
		sys := conn.SysConn()
		if sys == nil {
			return
		}
		const (
			outqBase       = 256 << 10 // 256 KiB floor
			outqChecks     = 2         // consecutive breaches before the kick
			outqCheckEvery = 2 * time.Second
		)
		breaches := 0
		t := time.NewTicker(outqCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			n, err := socketOutq(sys)
			if err != nil {
				// Log once, loudly: a dormant monitor is exactly how the
				// first implementation failed silently for a whole round
				// (wrong syscall => ENOPROTOOPT => return on first tick,
				// netns-probe round 7).
				s.log.Warn("mesh members: send-queue monitor unavailable; queue-depth kick disabled",
					"device", id, "err", err)
				return
			}
			cap := int64(outqBase)
			if l := atomic.LoadInt64(&lastPushLen); l > 0 {
				cap = outqBase + 2*l
			}
			if int64(n) > cap {
				breaches++
				if breaches >= outqChecks {
					s.log.Warn("mesh members: send queue over cap; kicking slow consumer",
						"device", id, "outq", n, "cap", cap, "consecutive", breaches)
					_ = conn.Close()
					return
				}
				continue
			}
			breaches = 0
		}
	}()

	// pushWriteDeadline bounds each snapshot write. A consumer that cannot
	// keep up must be KICKED, not buffered forever: an unbounded Send jams
	// the stream (kernel buffer, then gorilla's buffer, then every keepalive
	// write behind it), which both hides the peer's death from liveness and
	// freezes this member's view mid-stream — the exact failure shape the
	// issue #8 retest reported (megabytes stuck in kernel queues). Kicking
	// is safe: the client's reconnect loop redials, authenticates, and
	// receives a fresh snapshot on arrival.
	const pushWriteDeadline = 5 * time.Second
	// slowPushKick: a write that SUCCEEDS but takes this long means deep
	// queueing (the peer's app stalled while kernel auto-tuning absorbs).
	// Every snapshot is a FULL view, so everything queued ahead of it is
	// already stale — delivering megabytes of history delays the newest
	// state by the drain time, which is exactly how a kill event sat 90s+
	// behind a 3.7MB backlog before the member's view updated (issue #8,
	// oh-no-agent). Kick on slow success: the reconnect gets the fresh view
	// immediately instead of draining stale bytes.
	const slowPushKick = 750 * time.Millisecond

	// pushCoalesceBytes is the queue depth above which a new push is NOT
	// queued. Snapshots are FULL views, so a newer one supersedes every
	// older one — queuing both only delays the fresh state behind stale
	// bytes. When the kernel queue already holds more than a snapshot's
	// worth, the push is deferred and retried shortly; the retry carries
	// the CURRENT state, so nothing is lost but latency (issue #8,
	// netns-probe round 7: a kill event queued behind 4 MB of history
	// arrived 13.6s late).
	const pushCoalesceBytes = 32 << 10
	const pushCoalesceRetry = 500 * time.Millisecond

	// errPushCoalesced means "the queue is deep; try again shortly".
	var errPushCoalesced = errors.New("mesh: push coalesced")

	send := func() error {
		s.mu.Lock()
		members := s.discoMembers
		s.mu.Unlock()
		snap := discovery.BuildSnapshot(members, s.disco, time.Now())
		b, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		// Queue-depth check before writing: if the socket already holds
		// pending bytes, this snapshot would sit behind them. Defer instead.
		if sys := conn.SysConn(); sys != nil {
			if q, qerr := socketOutq(sys); qerr == nil && q > pushCoalesceBytes {
				return errPushCoalesced
			}
		}
		_ = conn.SetWriteDeadline(time.Now().Add(pushWriteDeadline))
		started := time.Now()
		err = conn.Send(b)
		// Record the snapshot size for the queue-depth monitor: the cap
		// scales with the last snapshot so a large membership list gets a
		// proportionally larger buffer before a kick fires.
		atomic.StoreInt64(&lastPushLen, int64(len(b)))
		if err == nil {
			if d := time.Since(started); d > slowPushKick {
				err = fmt.Errorf("push slow (%s): kicking slow consumer", d.Round(time.Millisecond))
			}
		}
		return err
	}

	if err := send(); err != nil {
		s.log.Info("mesh members: initial snapshot failed", "device", id, "err", err)
		_ = conn.Close()
		return
	}
	for {
		select {
		case <-ch:
			// Push with coalescing: a deep queue defers this push and
			// retries shortly, so the fresh state lands promptly instead of
			// behind stale snapshots.
			for {
				err := send()
				if err == errPushCoalesced {
					timer := time.NewTimer(pushCoalesceRetry)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-conn.Closed():
						timer.Stop()
						return
					case <-timer.C:
					}
					continue
				}
				if err != nil {
					s.log.Info("mesh members: push failed", "device", id, "err", err)
					_ = conn.Close()
					return
				}
				break
			}
		case <-conn.Closed():
			return
		case <-ctx.Done():
			return
		}
	}
}
