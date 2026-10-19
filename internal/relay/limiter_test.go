// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLimiterCapsConcurrency: one IP cannot hold more than maxCon slots, which
// bounds room-squatting from a single host.
func TestLimiterCapsConcurrency(t *testing.T) {
	l := newIPLimiter(1000, 3)

	for i := 0; i < 3; i++ {
		if !l.acquire("1.2.3.4") {
			t.Fatalf("acquire %d should succeed", i+1)
		}
	}
	if l.acquire("1.2.3.4") {
		t.Fatal("fourth concurrent connection should be refused")
	}
	// Another address has its own budget.
	if !l.acquire("5.6.7.8") {
		t.Fatal("a different IP should not be affected")
	}

	l.release("1.2.3.4")
	if !l.acquire("1.2.3.4") {
		t.Fatal("a freed slot should be reusable")
	}
}

// TestLimiterRateRefillsOverTime: the bucket refills at perMin/60 per second.
func TestLimiterRateRefillsOverTime(t *testing.T) {
	now := time.Unix(0, 0)
	l := newIPLimiter(60, 100) // 60/min = 1/s
	l.now = func() time.Time { return now }

	// Drain the initial budget.
	acquired := 0
	for l.acquire("9.9.9.9") {
		acquired++
		l.release("9.9.9.9")
		if acquired > 200 {
			t.Fatal("bucket never emptied")
		}
	}
	if acquired != 60 {
		t.Errorf("initial budget = %d, want 60", acquired)
	}

	// One second later exactly one more token is available.
	now = now.Add(time.Second)
	if !l.acquire("9.9.9.9") {
		t.Fatal("expected one refilled token")
	}
	l.release("9.9.9.9")
	if l.acquire("9.9.9.9") {
		t.Fatal("only one token should have refilled")
	}
}

// TestLimiterSweepDropsIdleState keeps the map bounded on a long-lived relay.
func TestLimiterSweepDropsIdleState(t *testing.T) {
	now := time.Unix(0, 0)
	l := newIPLimiter(100, 10)
	l.now = func() time.Time { return now }

	l.acquire("idle-ip")
	l.release("idle-ip")

	now = now.Add(time.Hour)
	l.sweep(10 * time.Minute)

	l.mu.Lock()
	_, still := l.state["idle-ip"]
	l.mu.Unlock()
	if still {
		t.Error("idle IP state was not swept")
	}
}

// TestLimiterKeepsBusyState: sweeping must not drop an IP with connections in
// flight, or its concurrency accounting would be lost.
func TestLimiterKeepsBusyState(t *testing.T) {
	now := time.Unix(0, 0)
	l := newIPLimiter(100, 10)
	l.now = func() time.Time { return now }

	l.acquire("busy-ip")
	now = now.Add(time.Hour)
	l.sweep(10 * time.Minute)

	l.mu.Lock()
	st, still := l.state["busy-ip"]
	l.mu.Unlock()
	if !still {
		t.Fatal("busy IP was swept")
	}
	if st.inflight != 1 {
		t.Errorf("inflight = %d, want 1", st.inflight)
	}
}

// TestClientIPHonoursProxyHeaders: behind nginx/Caddy every request comes from
// the proxy, so the limiter must key on the forwarded client address.
func TestClientIPIgnoresForgedHeadersByDefault(t *testing.T) {
	// With no trusted proxies configured, forwarding headers must be ignored:
	// they are attacker-controlled and would otherwise hand every request a
	// fresh rate-limit budget.
	l := newIPLimiter(100, 10)
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.RemoteAddr = "198.51.100.7:1234"
	r.Header.Set("X-Forwarded-For", "10.9.9.9")
	r.Header.Set("X-Real-IP", "10.9.9.8")

	if got := l.clientIP(r); got != "198.51.100.7" {
		t.Errorf("clientIP = %q, want the socket address 198.51.100.7", got)
	}
}

func TestClientIPHonoursHeadersFromTrustedProxyOnly(t *testing.T) {
	l := newIPLimiter(100, 10)
	if err := l.setTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		remote  string
		headers map[string]string
		want    string
	}{
		{"trusted proxy, xff", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "203.0.113.9"}, "203.0.113.9"},
		{"trusted proxy, xff chain", "10.1.2.3:1",
			map[string]string{"X-Forwarded-For": "203.0.113.9, 10.0.0.1"}, "203.0.113.9"},
		{"trusted proxy, x-real-ip", "127.0.0.1:1",
			map[string]string{"X-Real-IP": "198.51.100.7"}, "198.51.100.7"},
		{"untrusted peer, forged xff", "203.0.113.50:1",
			map[string]string{"X-Forwarded-For": "10.9.9.9"}, "203.0.113.50"},
		{"trusted proxy, no headers", "127.0.0.1:1", nil, "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/ws", nil)
			r.RemoteAddr = tc.remote
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := l.clientIP(r); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetTrustedProxiesRejectsBadCIDR(t *testing.T) {
	l := newIPLimiter(100, 10)
	if err := l.setTrustedProxies([]string{"not-an-ip"}); err == nil {
		t.Error("malformed CIDR should be rejected")
	}
}

// TestForgedHeadersCannotBypassLimit is the end-to-end form of the above: a
// client that invents a new address per request must still be throttled.
func TestForgedHeadersCannotBypassLimit(t *testing.T) {
	l := newIPLimiter(5, 100) // 5 connections per minute

	allowed := 0
	for i := 0; i < 60; i++ {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.RemoteAddr = "198.51.100.7:1234"
		// A different claimed address every time.
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("10.9.9.%d", i%250))
		ip := l.clientIP(r)
		if l.acquire(ip) {
			allowed++
		}
	}
	if allowed > 5 {
		t.Errorf("%d of 60 requests allowed, want <= 5: forged headers bypassed the limit", allowed)
	}
}

// TestRedactRoomHidesFullLabel: logs must not carry the complete room label.
func TestRedactRoomHidesFullLabel(t *testing.T) {
	full := "0123456789abcdef0123456789abcdef"
	got := redactRoom(full)
	if len(got) >= len(full) {
		t.Errorf("redactRoom(%q) = %q, expected truncation", full, got)
	}
	if got[:8] != full[:8] {
		t.Errorf("redacted value should keep the prefix for correlation")
	}
	// Short labels pass through unchanged (they cannot be truncated usefully).
	if short := "abc"; redactRoom(short) != short {
		t.Errorf("short room was altered: %q", redactRoom(short))
	}
}
