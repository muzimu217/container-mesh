// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package forward

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeResolver returns canned answers so a test can simulate DNS changing
// between the check and the dial.
type fakeResolver struct {
	answers map[string][]string
	err     error
	calls   int
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	addrs, ok := f.answers[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	out := make([]net.IPAddr, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, net.IPAddr{IP: net.ParseIP(a)})
	}
	return out, nil
}

// TestAllowListZeroValueDeniesEverything is the load-bearing default: a
// forgotten configuration must not become an open proxy.
func TestAllowListZeroValueDeniesEverything(t *testing.T) {
	var al AllowList
	if !al.Empty() {
		t.Fatal("zero AllowList reports non-empty")
	}
	if _, err := al.Permits(context.Background(), "db:5432"); !errors.Is(err, ErrDenied) {
		t.Fatalf("zero AllowList permitted a target: %v", err)
	}
}

// TestAllowListEmptyHostsDeny proves an explicitly empty list is still "deny
// none", never "allow all".
func TestAllowListEmptyHostsDeny(t *testing.T) {
	ports, err := ParsePortSet([]string{"5432"})
	if err != nil {
		t.Fatal(err)
	}
	al, err := NewAllowList(context.Background(), nil, ports, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := al.Permits(context.Background(), "db:5432"); !errors.Is(err, ErrDenied) {
		t.Fatalf("empty host list permitted a target: %v", err)
	}
}

// TestAllowListPinsHostnameAtStartup is the DNS-rebinding guard: the second
// lookup must not be able to change what gets dialled.
func TestAllowListPinsHostnameAtStartup(t *testing.T) {
	res := &fakeResolver{answers: map[string][]string{"db.internal": {"10.0.0.5"}}}
	ports, _ := ParsePortSet([]string{"5432"})
	al, err := NewAllowList(context.Background(), []string{"db.internal"}, ports, res)
	if err != nil {
		t.Fatal(err)
	}
	if res.calls != 1 {
		t.Fatalf("resolver called %d times at construction, want 1", res.calls)
	}

	ip, err := al.Permits(context.Background(), "db.internal:5432")
	if err != nil {
		t.Fatalf("permits: %v", err)
	}
	if !ip.Equal(net.ParseIP("10.0.0.5")) {
		t.Fatalf("permitted %v, want the pinned 10.0.0.5", ip)
	}

	// Now the attacker flips DNS to their own address. The allow-list must keep
	// returning the pinned IP, because that is what gets dialled.
	res.answers["db.internal"] = []string{"6.6.6.6"}
	ip2, err := al.Permits(context.Background(), "db.internal:5432")
	if err != nil {
		t.Fatalf("second permits: %v", err)
	}
	if !ip2.Equal(net.ParseIP("10.0.0.5")) {
		t.Fatalf("after rebinding, permitted %v; the pinned address must win", ip2)
	}
}

// TestAllowListRejectsRebindToUnlistedIP is the complementary case: a request
// for a hostname that now resolves somewhere unlisted must be denied outright.
func TestAllowListRejectsRebindToUnlistedIP(t *testing.T) {
	res := &fakeResolver{answers: map[string][]string{"evil.example": {"6.6.6.6"}}}
	ports, _ := ParsePortSet([]string{"5432"})
	al, err := NewAllowList(context.Background(), []string{"10.0.0.5"}, ports, res)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := al.Permits(context.Background(), "evil.example:5432"); !errors.Is(err, ErrDenied) {
		t.Fatalf("permitted an unlisted resolution: %v", err)
	}
}

// TestAllowListRefusesAllAddresses is ARCHITECTURE-SDWAN §8: --allow-any is
// never a feature.
func TestAllowListRefusesAllAddresses(t *testing.T) {
	ports, _ := ParsePortSet([]string{"5432"})
	for _, host := range []string{"0.0.0.0/0", "::/0"} {
		if _, err := NewAllowList(context.Background(), []string{host}, ports, &fakeResolver{}); err == nil {
			t.Fatalf("accepted %q as an allow entry", host)
		}
	}
}

// TestAllowListPortMatching covers the port set, including ranges and the
// empty-set denial.
func TestAllowListPortMatching(t *testing.T) {
	ports, err := ParsePortSet([]string{"5432", "6000-6002"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []int{5432, 6000, 6001, 6002} {
		if !ports.Contains(p) {
			t.Fatalf("port %d should be allowed", p)
		}
	}
	for _, p := range []int{5431, 5433, 5999, 6003, 80} {
		if ports.Contains(p) {
			t.Fatalf("port %d should be denied", p)
		}
	}

	empty, err := ParsePortSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Contains(5432) {
		t.Fatal("empty port set allowed a port")
	}
	if !empty.Empty() {
		t.Fatal("empty port set does not report Empty")
	}
}

// TestParsePortSetRejects checks the parse errors, since a silently ignored
// entry would widen the policy in the operator's mind but not in reality.
func TestParsePortSetRejects(t *testing.T) {
	bad := [][]string{
		{"0"}, {"65536"}, {"-1"}, {"abc"}, {"6000-"}, {"-6000"},
		{"7000-6000"}, {"6000-abc"},
	}
	for _, entries := range bad {
		if _, err := ParsePortSet(entries); err == nil {
			t.Fatalf("accepted %v", entries)
		}
	}
}

// TestAllowListPermitsIPLiteral covers the case where the peer sends an IP
// rather than a name, which needs no resolution at all.
func TestAllowListPermitsIPLiteral(t *testing.T) {
	ports, _ := ParsePortSet([]string{"5432"})
	al, err := NewAllowList(context.Background(), []string{"10.0.0.5"}, ports, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	ip, err := al.Permits(context.Background(), "10.0.0.5:5432")
	if err != nil {
		t.Fatalf("permits: %v", err)
	}
	if !ip.Equal(net.ParseIP("10.0.0.5")) {
		t.Fatalf("got %v", ip)
	}
	if _, err := al.Permits(context.Background(), "10.0.0.6:5432"); !errors.Is(err, ErrDenied) {
		t.Fatalf("permitted an unlisted IP: %v", err)
	}
}

// TestAllowListCIDR checks the literal-CIDR path that PR #3 extends.
func TestAllowListCIDR(t *testing.T) {
	ports, _ := ParsePortSet([]string{"5432"})
	al, err := NewAllowList(context.Background(), []string{"10.0.0.0/24"}, ports, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := al.Permits(context.Background(), "10.0.0.7:5432"); err != nil {
		t.Fatalf("CIDR member denied: %v", err)
	}
	if _, err := al.Permits(context.Background(), "10.0.1.7:5432"); !errors.Is(err, ErrDenied) {
		t.Fatalf("non-member permitted: %v", err)
	}
}

// TestAllowListRejectsMalformedAddress keeps the parser and the policy aligned.
func TestAllowListRejectsMalformedAddress(t *testing.T) {
	ports, _ := ParsePortSet([]string{"5432"})
	al, err := NewAllowList(context.Background(), []string{"10.0.0.5"}, ports, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"", "nohost", "host:", ":5432", "host:notaport"} {
		if _, err := al.Permits(context.Background(), addr); !errors.Is(err, ErrDenied) {
			t.Fatalf("accepted %q: %v", addr, err)
		}
	}
}

// TestParseSpec covers the table from the design, including the IPv6 shapes
// that a naive colon split breaks.
func TestParseSpec(t *testing.T) {
	cases := []struct {
		in       string
		reverse  bool
		wantBind string
		wantTgt  string
	}{
		{"8080:db:5432", false, "127.0.0.1:8080", "db:5432"},
		{"127.0.0.1:8080:db:5432", false, "127.0.0.1:8080", "db:5432"},
		{"[::1]:8080:db:5432", false, "[::1]:8080", "db:5432"},
		{"8080:[::1]:5432", false, "127.0.0.1:8080", "[::1]:5432"},
		{"[::1]:8080:[::2]:5432", false, "[::1]:8080", "[::2]:5432"},
		{"0.0.0.0:8080:db:5432", false, "0.0.0.0:8080", "db:5432"},
		{"8080:db:5432", true, "127.0.0.1:8080", "db:5432"},
	}
	for _, c := range cases {
		got, err := ParseSpec(c.in, c.reverse)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got.Bind != c.wantBind || got.Target != c.wantTgt {
			t.Fatalf("%q: bind %q target %q, want %q %q",
				c.in, got.Bind, got.Target, c.wantBind, c.wantTgt)
		}
		if got.Reverse != c.reverse {
			t.Fatalf("%q: reverse %v", c.in, got.Reverse)
		}
		if got.ID != got.Bind {
			t.Fatalf("%q: id %q != bind %q", c.in, got.ID, got.Bind)
		}
	}
}

// TestParseSpecRejects is the rejection table.
func TestParseSpecRejects(t *testing.T) {
	bad := []string{
		"8080:db",         // two fields
		"db:5432",         // two fields
		"8080:db:0",       // port 0
		"8080:db:65536",   // port too big
		"0:db:5432:extra", // too many local fields
		"::1:8080:db:5432",
		"[:8080]:db:5432", // empty bind host must be explicit
		"[::1:8080:db:5432",
		"[::1]]:8080:db:5432",
		"8080::5432", // empty target host
		"8080:db:notaport",
	}
	for _, in := range bad {
		if _, err := ParseSpec(in, false); err == nil {
			t.Fatalf("accepted %q", in)
		}
	}
}

// TestParseSpecDefaultBindIsLoopback is the safety default the review called
// out: a bare port must never bind all interfaces.
func TestParseSpecDefaultBindIsLoopback(t *testing.T) {
	got, err := ParseSpec("8080:db:5432", false)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(got.Bind)
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("default bind host is %q, want 127.0.0.1", host)
	}
}

// TestParseSpecErrorMentionsBrackets checks the hint, since an unbracketed IPv6
// address is the most likely mistake.
func TestParseSpecErrorMentionsBrackets(t *testing.T) {
	_, err := ParseSpec("::1:8080:db:5432", false)
	if err == nil {
		t.Fatal("accepted an unbracketed IPv6 bind")
	}
	if !strings.Contains(err.Error(), "bracket") {
		t.Fatalf("error %q does not tell the user to bracket the address", err)
	}
}

// TestTokenBucketLimitsRate is the anti-scan layer: MaxStreams bounds
// concurrency, this bounds rate.
func TestTokenBucketLimitsRate(t *testing.T) {
	b := newTokenBucket(10, 10)
	now := time.Now()
	b.now = func() time.Time { return now }

	allowed := 0
	for i := 0; i < 20; i++ {
		if b.Allow() {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("allowed %d in the first burst, want 10 (the burst size)", allowed)
	}

	// After 100 ms at 10/s, exactly one more token exists.
	now = now.Add(100 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("no token after 100 ms at 10/s")
	}
	if b.Allow() {
		t.Fatal("a second token appeared within the same 100 ms")
	}

	// After a long idle period the bucket refills only to its capacity.
	now = now.Add(time.Hour)
	allowed = 0
	for i := 0; i < 50; i++ {
		if b.Allow() {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("allowed %d after a long idle, want the burst cap 10", allowed)
	}
}

// TestTokenBucketNeverBlocks proves Allow is non-blocking: a forwarded
// connection that waits for a rate limit is worse than one that is refused.
func TestTokenBucketNeverBlocks(t *testing.T) {
	b := newTokenBucket(1, 1)
	for i := 0; i < 3; i++ {
		_ = b.Allow()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			b.Allow()
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Allow blocked when the bucket was empty")
	}
}

// TestPendingGateIsNonBlocking checks the accept-side bound.
func TestPendingGateIsNonBlocking(t *testing.T) {
	g := newPendingGate(2)
	if !g.Acquire() || !g.Acquire() {
		t.Fatal("could not fill the gate")
	}
	if g.Acquire() {
		t.Fatal("acquired past the limit")
	}
	if g.InUse() != 2 {
		t.Fatalf("InUse = %d, want 2", g.InUse())
	}
	g.Release()
	if !g.Acquire() {
		t.Fatal("could not acquire after release")
	}
}

// TestLimitsNormalise checks that zero fields get defaults and that a negative
// rate limit means "disabled" rather than "zero per second".
func TestLimitsNormalise(t *testing.T) {
	got := Limits{}.normalise()
	d := DefaultLimits()
	if got.MaxPending != d.MaxPending ||
		got.HandshakeTimeout != d.HandshakeTimeout ||
		got.PipeDrain != d.PipeDrain ||
		got.DialTimeout != d.DialTimeout {
		t.Fatalf("normalise did not apply defaults: %+v", got)
	}
	if neg := (Limits{MaxStreamsPerSec: -1}).normalise(); neg.MaxStreamsPerSec != 0 {
		t.Fatalf("negative rate became %d, want 0 (disabled)", neg.MaxStreamsPerSec)
	}
}
