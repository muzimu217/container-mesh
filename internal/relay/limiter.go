// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

import (
	"fmt"
	"net"
	"net/http"
	"strings"
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
	// trusted holds the CIDRs whose forwarding headers are believed. Empty
	// means the socket address is always used.
	trusted []*net.IPNet
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

// setTrustedProxies parses the CIDR list.
func (l *ipLimiter) setTrustedProxies(cidrs []string) error {
	var nets []*net.IPNet
	for _, c := range cidrs {
		c = trimSpace(c)
		if c == "" {
			continue
		}
		// Accept both "10.0.0.0/8" and a bare address ("127.0.0.1").
		if !strings.Contains(c, "/") {
			ip := net.ParseIP(c)
			if ip == nil {
				return fmt.Errorf("relay: bad trusted proxy %q", c)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			c = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("relay: bad trusted proxy %q: %w", c, err)
		}
		nets = append(nets, n)
	}
	l.mu.Lock()
	l.trusted = nets
	l.mu.Unlock()
	return nil
}

// isTrustedPeer reports whether the request came from a configured proxy.
func (l *ipLimiter) isTrustedPeer(remoteIP string) bool {
	ip := net.ParseIP(remoteIP)
	if ip == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, n := range l.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP extracts the address to rate-limit on.
//
// Forwarding headers are attacker-controlled: any client can send
// "X-Forwarded-For: 1.2.3.4" and claim a fresh identity, which would give it a
// new budget on every request and grow the limiter map without bound. They are
// therefore only consulted when the request actually arrives from a configured
// trusted proxy; otherwise the socket address is authoritative.
func (l *ipLimiter) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !l.isTrustedPeer(host) {
		return host
	}

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
