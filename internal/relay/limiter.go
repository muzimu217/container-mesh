// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// ipLimiter bounds how fast one source address may open connections and how
// many it may hold at once. It exists because the relay endpoint is public and
// unauthenticated by design: the room label is not a credential, so without a
// limit anyone can create rooms until MaxRooms is exhausted or repeatedly
// displace a legitimate waiter.
type ipLimiter struct {
	perMin int
	maxCon int

	mu    sync.Mutex
	state map[string]*ipState
	// now is injectable so tests need not sleep.
	now func() time.Time
}

type ipState struct {
	// tokens is a simple bucket refilled at perMin/60 per second.
	tokens   float64
	last     time.Time
	inflight int
	seen     time.Time
}

func newIPLimiter(perMin, maxCon int) *ipLimiter {
	if perMin <= 0 {
		perMin = 120
	}
	if maxCon <= 0 {
		maxCon = 16
	}
	return &ipLimiter{perMin: perMin, maxCon: maxCon, state: make(map[string]*ipState), now: time.Now}
}

// acquire reserves one connection slot for ip. It returns false when the IP is
// over its rate or concurrency budget.
func (l *ipLimiter) acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	st, ok := l.state[ip]
	if !ok {
		st = &ipState{tokens: float64(l.perMin), last: now}
		l.state[ip] = st
	}

	// Refill: perMin tokens per minute.
	elapsed := now.Sub(st.last).Seconds()
	if elapsed > 0 {
		st.tokens += elapsed * float64(l.perMin) / 60.0
		if st.tokens > float64(l.perMin) {
			st.tokens = float64(l.perMin)
		}
		st.last = now
	}
	st.seen = now

	if st.inflight >= l.maxCon {
		return false
	}
	if st.tokens < 1 {
		return false
	}
	st.tokens--
	st.inflight++
	return true
}

// release frees a slot taken by acquire.
func (l *ipLimiter) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, ok := l.state[ip]; ok && st.inflight > 0 {
		st.inflight--
	}
}

// sweep drops state for addresses that have been idle for a while, so the map
// cannot grow without bound on a long-running relay.
func (l *ipLimiter) sweep(maxIdle time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-maxIdle)
	for ip, st := range l.state {
		if st.inflight == 0 && st.seen.Before(cutoff) {
			delete(l.state, ip)
		}
	}
}

// clientIP extracts the address to rate-limit on. X-Forwarded-For is honoured
// because the normal deployment puts the relay behind nginx/Caddy; without it
// every client would appear to come from the proxy and share one budget.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Left-most entry is the original client; the rest are proxies.
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return trimSpace(xff[:i])
			}
		}
		return trimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return trimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}
