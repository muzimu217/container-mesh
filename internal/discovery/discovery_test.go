// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// fixture builds an account with N devices, all signed in, plus the member
// list the discovery node runs on.
type fixture struct {
	acc     *identity.Account
	devices []*identity.Device
	members *identity.MemberList
}

func newFixture(t *testing.T, n int) *fixture {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{acc: acc, members: identity.NewMemberList(acc.ID())}
	for i := 0; i < n; i++ {
		dev, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		cert, err := acc.IssueCert(dev, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.members.Add(*cert); err != nil {
			t.Fatal(err)
		}
		f.devices = append(f.devices, dev)
	}
	return f
}

var testNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func announceFor(dev *identity.Device, eps []string) MemberAnnounce {
	return MemberAnnounce{DeviceID: dev.ID(), Endpoints: eps, Timestamp: testNow}
}

func TestAnnounceValidateAcceptsSignedMember(t *testing.T) {
	f := newFixture(t, 1)
	a := announceFor(f.devices[0], []string{"203.0.113.5:7000"})
	sig := a.Sign(f.devices[0])

	key := deviceKey(t, f, f.devices[0].ID())
	if err := a.Validate(key, sig, testNow); err != nil {
		t.Fatalf("valid announce rejected: %v", err)
	}
}

// TestAnnounceRejectsNonMember is R-2.4: a stranger who learns the discovery
// node's address still cannot inject itself.
func TestAnnounceRejectsNonMember(t *testing.T) {
	// A real device with a real key — but never signed into the account.
	outsider, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	a := announceFor(outsider, []string{"203.0.113.9:7000"})
	sig := a.Sign(outsider)

	if err := a.Validate(nil, sig, testNow); err != ErrAnnounceNotMember {
		t.Fatalf("outsider announce = %v, want ErrAnnounceNotMember", err)
	}
}

// TestAnnounceRejectsForgedSignature: an outsider signing as a member's ID.
func TestAnnounceRejectsForgedSignature(t *testing.T) {
	f := newFixture(t, 1)
	outsider, _ := identity.Generate()

	// Claim to be the member, sign with the outsider's key.
	a := announceFor(f.devices[0], []string{"203.0.113.5:7000"})
	sig := a.Sign(outsider)

	key := deviceKey(t, f, f.devices[0].ID())
	if err := a.Validate(key, sig, testNow); err != ErrAnnounceBadSignature {
		t.Fatalf("forged announce = %v, want ErrAnnounceBadSignature", err)
	}
}

func TestAnnounceRejectsStaleTimestamp(t *testing.T) {
	f := newFixture(t, 1)
	a := announceFor(f.devices[0], []string{"203.0.113.5:7000"})
	sig := a.Sign(f.devices[0])
	key := deviceKey(t, f, f.devices[0].ID())

	old := a
	old.Timestamp = testNow.Add(-2 * AnnounceClockSkew)
	oldSig := old.Sign(f.devices[0])
	if err := old.Validate(key, oldSig, testNow); err != ErrAnnounceStale {
		t.Fatalf("stale announce = %v, want ErrAnnounceStale", err)
	}

	// Fresh one still passes at the same instant.
	if err := a.Validate(key, sig, testNow); err != nil {
		t.Fatalf("fresh announce rejected: %v", err)
	}
}

func TestAnnounceRejectsMalformedEndpoints(t *testing.T) {
	f := newFixture(t, 1)
	key := deviceKey(t, f, f.devices[0].ID())

	bad := [][]string{
		{""},                 // empty
		{"noport"},           // no port
		{"host:0"},           // port 0
		{"host:65536"},       // port too big
		{":7000"},            // empty host
		{"ws://h:7000/x"},    // a URL is not an endpoint
		{"h:7000", "h:7000"}, // duplicate
		{"a:1", "b:2", "c:3", "d:4", "e:5", "f:6", "g:7", "h:8",
			"i:9", "j:10", "k:11", "l:12", "m:13", "n:14", "o:15",
			"p:16", "q:17"}, // over MaxEndpoints
	}
	for _, eps := range bad {
		a := announceFor(f.devices[0], eps)
		sig := a.Sign(f.devices[0])
		if err := a.Validate(key, sig, testNow); err == nil {
			t.Fatalf("endpoints %v accepted", eps)
		}
	}

	// Good ones still pass, including IPv6 and private ranges.
	a := announceFor(f.devices[0], []string{"[2001:db8::1]:7000", "10.0.0.5:7000"})
	sig := a.Sign(f.devices[0])
	if err := a.Validate(key, sig, testNow); err != nil {
		t.Fatalf("valid endpoints rejected: %v", err)
	}
}

func deviceKey(t *testing.T, f *fixture, id identity.DeviceID) ed25519.PublicKey {
	t.Helper()
	m, ok := f.members.Get(id)
	if !ok {
		t.Fatalf("member %s not in list", id)
	}
	return m.Cert.DevicePub
}

// --- node state -------------------------------------------------------------

func TestNodeRegisterAndSweep(t *testing.T) {
	f := newFixture(t, 2)
	n := NewNode(f.members)
	now := testNow
	n.SetClock(func() time.Time { return now })

	n.Register(announceFor(f.devices[0], []string{"1.2.3.4:1"}), now)
	if got := n.Endpoints(f.devices[0].ID()); len(got) != 1 || got[0] != "1.2.3.4:1" {
		t.Fatalf("endpoints = %v", got)
	}

	// Advance past the timeout without a second heartbeat from device 0.
	now = now.Add(MemberTimeout + time.Second)
	n.Register(announceFor(f.devices[1], []string{"1.2.3.5:2"}), now)

	dropped := n.Sweep()
	if len(dropped) != 1 || dropped[0] != f.devices[0].ID() {
		t.Fatalf("dropped = %v, want [%s]", dropped, f.devices[0].ID())
	}
	if _, ok := n.LastSeen(f.devices[0].ID()); ok {
		t.Fatal("timed-out member still has liveness")
	}
	// Membership survives: liveness expired, belonging did not.
	if !f.members.Contains(f.devices[0].ID()) {
		t.Fatal("sweep removed the device from the MEMBER list; liveness and membership are different things")
	}
}

func TestNodeRegisterNotifiesOnEndpointChangeOnly(t *testing.T) {
	f := newFixture(t, 1)
	n := NewNode(f.members)

	ch, unsub := n.Subscribe(f.devices[0].ID())
	defer unsub()
	lastSubChan = ch

	n.Register(announceFor(f.devices[0], []string{"1.1.1.1:1"}), testNow)
	expectSignal(t, "first register")

	// Same endpoints again: no push expected.
	n.Register(announceFor(f.devices[0], []string{"1.1.1.1:1"}), testNow)
	expectNoSignal(t, "unchanged endpoints")

	n.Register(announceFor(f.devices[0], []string{"2.2.2.2:2"}), testNow)
	expectSignal(t, "changed endpoints")
}

// These helpers reach into the node's subscription through the channel the
// Subscribe call returned; they live at package scope so both tests share them.
var lastSubChan <-chan struct{}

func expectSignal(t *testing.T, what string) {
	t.Helper()
	select {
	case <-lastSubChan:
	default:
		t.Fatalf("expected a notification after %s", what)
	}
}

func expectNoSignal(t *testing.T, what string) {
	t.Helper()
	select {
	case <-lastSubChan:
		t.Fatalf("unexpected notification after %s", what)
	default:
	}
}

func TestNodeSnapshotSorted(t *testing.T) {
	f := newFixture(t, 3)
	n := NewNode(f.members)
	n.Register(announceFor(f.devices[2], []string{"a:1"}), testNow)
	n.Register(announceFor(f.devices[0], []string{"b:2"}), testNow)

	snap := n.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot size = %d", len(snap))
	}
	if snap[0].DeviceID >= snap[1].DeviceID {
		t.Fatalf("snapshot not sorted: %s then %s", snap[0].DeviceID, snap[1].DeviceID)
	}
}

// --- invite -----------------------------------------------------------------

func TestInviteRoundTrip(t *testing.T) {
	f := newFixture(t, 1)
	inv := Invite{
		AccountID:      f.acc.ID(),
		AccountPub:     f.acc.PublicKey(),
		DiscoveryAddrs: []string{"203.0.113.10:8080"},
		IssuedAt:       testNow,
	}
	s, err := EncodeInvite(inv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeInvite(s)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AccountID != inv.AccountID {
		t.Fatal("account id changed")
	}
	if !got.AccountPub.Equal(inv.AccountPub) {
		t.Fatal("account key changed")
	}
	if len(got.DiscoveryAddrs) != 1 || got.DiscoveryAddrs[0] != "203.0.113.10:8080" {
		t.Fatalf("discovery addrs = %v", got.DiscoveryAddrs)
	}
}

func TestInviteRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"http://example.com",
		"cmesh://join?",                      // no fields
		"cmesh://join?acct=short",            // invalid account id
		"cmesh://join?acct=abcdefghijklmnop", // valid id but no key
		"cmesh://join?acct=abcdefghijklmnop&k=xx&disc=1.2.3.4:1&at=2026-01-01T00:00:00Z",                                   // key wrong length
		"cmesh://join?acct=abcdefghijklmnop&k=" + string(make([]byte, 32)) + "&at=2026-01-01T00:00:00Z",                    // no disc
		"cmesh://join?acct=abcdefghijklmnop&k=" + string(make([]byte, 32)) + "&disc=notanendpoint&at=2026-01-01T00:00:00Z", // bad disc
		"cmesh://join?acct=abcdefghijklmnop&k=" + string(make([]byte, 32)) + "&disc=1.2.3.4:1&at=nonsense",                 // bad time
	}
	for _, s := range bad {
		if _, err := DecodeInvite(s); err == nil {
			t.Fatalf("invite accepted: %q", truncate(s, 50))
		}
	}
}

// TestInviteAcceptsSchemeDiscoveryAddrs pins the F2 fix (netns-probe round
// 31): invite emits scheme-ed discovery addrs for TLS deployments (e.g.
// --disc https://10.0.0.1:18443), and join used to reject exactly what
// invite produced — validEndpoint saw "https://host:port" and failed
// SplitHostPort, leaving 443 as the only usable TLS port with a misleading
// "malformed endpoint" error. The scheme prefix must survive the round
// trip (the client's normalizeBaseURL keeps it), while a scheme with a
// bogus host:port is still rejected.
func TestInviteAcceptsSchemeDiscoveryAddrs(t *testing.T) {
	f := newFixture(t, 1)
	inv := Invite{
		AccountID:      f.acc.ID(),
		AccountPub:     f.acc.PublicKey(),
		DiscoveryAddrs: []string{"https://10.55.0.1:18443", "203.0.113.10:8080"},
		IssuedAt:       testNow,
	}
	s, err := EncodeInvite(inv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeInvite(s)
	if err != nil {
		t.Fatalf("scheme-ed discovery addr rejected: %v", err)
	}
	if len(got.DiscoveryAddrs) != 2 || got.DiscoveryAddrs[0] != "https://10.55.0.1:18443" {
		t.Fatalf("discovery addrs = %v; scheme must be preserved verbatim", got.DiscoveryAddrs)
	}

	bad, err := EncodeInvite(Invite{
		AccountID:      f.acc.ID(),
		AccountPub:     f.acc.PublicKey(),
		DiscoveryAddrs: []string{"https://no-port-here"},
		IssuedAt:       testNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeInvite(bad); err == nil {
		t.Fatal("scheme-ed addr with malformed host:port must still be rejected")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- security fixes from the Round 4 external review ---

// TestEndpointRejectsControlCharacters is the log-injection guard: the review
// found "evil\nFAKE:80" passed validEndpoint, which would have put a newline
// into every downstream log and terminal that prints an endpoint.
func TestEndpointRejectsControlCharacters(t *testing.T) {
	f := newFixture(t, 1)
	key := deviceKey(t, f, f.devices[0].ID())
	bad := [][]string{
		{"a\nb:80"}, {"a\rb:80"}, {"a\tb:80"}, {"a\x1bb:80"},
		{"evil\nFAKE:80"}, {"\x7f:80"},
	}
	for _, eps := range bad {
		a := announceFor(f.devices[0], eps)
		sig := a.Sign(f.devices[0])
		if err := a.Validate(key, sig, testNow); err == nil {
			t.Fatalf("endpoints %q accepted", eps)
		}
	}
}

// TestEndpointRejectsMalformedHosts closes the remaining shapes the review
// enumerated: "@:80" and "a b:80" both pass SplitHostPort but are not
// diallable addresses in any useful sense.
func TestEndpointRejectsMalformedHosts(t *testing.T) {
	f := newFixture(t, 1)
	key := deviceKey(t, f, f.devices[0].ID())
	for _, e := range []string{"@:80", "a b:80"} {
		a := announceFor(f.devices[0], []string{e})
		sig := a.Sign(f.devices[0])
		if err := a.Validate(key, sig, testNow); err == nil {
			t.Fatalf("endpoint %q accepted", e)
		}
	}
}

// TestValidateRejectsWrongLengthKey guards the panic: ed25519.Verify panics
// on a non-32-byte public key instead of returning an error (verified
// empirically), so a malformed member record reaching the list would turn a
// validation call into a crash.
func TestValidateRejectsWrongLengthKey(t *testing.T) {
	f := newFixture(t, 1)
	a := announceFor(f.devices[0], []string{"1.2.3.4:80"})
	sig := a.Sign(f.devices[0])
	// A truncated key: not nil (so the membership check passes shape-wise)
	// but the wrong length.
	bad := make([]byte, 5)
	if err := a.Validate(bad, sig, testNow); err != ErrAnnounceBadSignature {
		t.Fatalf("wrong-length key = %v, want ErrAnnounceBadSignature (not a panic)", err)
	}
}

// TestRegisterRefusesNonMember is the structural backstop: Register must
// refuse to record state for a device that is not in the member list, even
// if the caller skipped Validate entirely.
func TestRegisterRefusesNonMember(t *testing.T) {
	f := newFixture(t, 1)
	n := NewNode(f.members)

	outsider, _ := identity.Generate()
	changed := n.Register(announceFor(outsider, []string{"9.9.9.9:9"}), testNow)
	if changed {
		t.Fatal("Register recorded state for a non-member")
	}
	if _, ok := n.LastSeen(outsider.ID()); ok {
		t.Fatal("non-member has liveness after Register")
	}
}

// TestRegisterCopiesEndpoints guards against aliasing: the announce's
// Endpoints slice belongs to the caller (a transport buffer), so storing the
// reference would let a later message silently rewrite recorded state.
func TestRegisterCopiesEndpoints(t *testing.T) {
	f := newFixture(t, 1)
	n := NewNode(f.members)

	eps := []string{"1.2.3.4:80"}
	a := announceFor(f.devices[0], eps)
	n.Register(a, testNow)

	// Mutate the caller's slice; the recorded endpoints must not change.
	eps[0] = "evil:9999"
	if got := n.Endpoints(f.devices[0].ID()); len(got) != 1 || got[0] != "1.2.3.4:80" {
		t.Fatalf("recorded endpoints aliased the caller's slice: %v", got)
	}
}

// TestSubscribeSecondClosesFirst ensures a re-Subscribe does not orphan the
// first subscriber on a channel nobody will ever signal.
func TestSubscribeSecondClosesFirst(t *testing.T) {
	f := newFixture(t, 1)
	n := NewNode(f.members)

	ch1, unsub1 := n.Subscribe(f.devices[0].ID())
	defer unsub1()
	ch2, _ := n.Subscribe(f.devices[0].ID())

	select {
	case _, ok := <-ch1:
		if ok {
			t.Fatal("first channel received a value; want close")
		}
	default:
		t.Fatal("first channel not closed after second Subscribe")
	}
	// The second channel must still be live.
	select {
	case <-ch2:
		t.Fatal("second channel signalled before any change")
	default:
	}
}

// TestInviteRejectsTooManyDiscoveryAddrs mirrors MaxEndpoints for invites:
// an unbounded disc append is the same memory-amplification face.
func TestInviteRejectsTooManyDiscoveryAddrs(t *testing.T) {
	f := newFixture(t, 1)
	inv := Invite{
		AccountID:  f.acc.ID(),
		AccountPub: f.acc.PublicKey(),
		IssuedAt:   testNow,
	}
	for i := 0; i < MaxEndpoints+1; i++ {
		inv.DiscoveryAddrs = append(inv.DiscoveryAddrs, "10.0.0.1:80")
	}
	if _, err := EncodeInvite(inv); err != nil {
		t.Fatal(err)
	}
	// Decode it back and expect rejection on the count.
	s, err := EncodeInvite(inv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeInvite(s); err == nil {
		t.Fatal("invite with more discovery addrs than MaxEndpoints accepted")
	}
}
