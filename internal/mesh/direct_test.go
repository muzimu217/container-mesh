// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// fixture: an account, two signed-in devices, the member list both sides
// resolve against.
type directFixture struct {
	acc     *identity.Account
	aline   *identity.Device // listener side
	bob     *identity.Device // dialer side
	members *identity.MemberList
}

func newDirectFixture(t *testing.T) *directFixture {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	f := &directFixture{acc: acc, members: identity.NewMemberList(acc.ID())}
	for _, d := range []**identity.Device{&f.aline, &f.bob} {
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
		*d = dev
	}
	return f
}

func startListener(t *testing.T, f *directFixture) *Listener {
	t.Helper()
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l
}

func dialTo(t *testing.T, f *directFixture, eps []string, kind string) *Link {
	t.Helper()
	link, err := DialDirect(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        eps,
		Kind:             kind,
		HandshakeTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = link.Close() })
	return link
}

// TestPingRoundTrip is the PR-M3 acceptance path: direct dial + RTT. The
// test asserts a generous smoke bound (T-2.4: no absolute thresholds in
// tests); the P-1 line (<5 ms same-machine) is measured and printed for the
// round log, judged by humans not asserted in CI.
func TestPingRoundTrip(t *testing.T) {
	f := newDirectFixture(t)
	l := startListener(t, f)
	link := dialTo(t, f, l.BoundAddrs(), KindPing)

	rtts, err := PingExchange(context.Background(), link, 20, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("ping exchange: %v", err)
	}
	if len(rtts) != 20 {
		t.Fatalf("got %d RTTs, want 20", len(rtts))
	}
	p95 := Percentile(rtts, 95)
	t.Logf("measured: p50=%.3fms p95=%.3fms max=%.3fms (P-1 line: <5ms)",
		Percentile(rtts, 50), p95, Percentile(rtts, 100))
	if p95 >= 50 {
		t.Fatalf("p95 %.2fms — loopback should be far below the 50ms smoke bound", p95)
	}
}

// TestDialDuplexEcho exercises the dial kind: lines sent come back.
func TestDialDuplexEcho(t *testing.T) {
	f := newDirectFixture(t)
	l := startListener(t, f)
	link := dialTo(t, f, l.BoundAddrs(), KindDial)

	msg := "hello over the direct link\n"
	if _, err := link.Stream().Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	_ = link.Stream().SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := bufio.NewReader(link.Reader()).ReadString('\n')
	if err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if got != msg {
		t.Fatalf("echo mismatch: %q", got)
	}
}

// TestDialRejectsImpostor pins the core PR-M3 security property: an endpoint
// answered by a device whose key is NOT the target's member-view key fails
// the handshake. The pin in deviceTLS is the gate; removing it turns this
// test red by construction.
func TestDialRejectsImpostor(t *testing.T) {
	f := newDirectFixture(t)

	// The impostor runs a listener with a device that is NOT the target.
	impostor, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := f.acc.IssueCert(impostor, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  impostor,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	_, err = DialDirect(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(), // dial the REAL member, reach the impostor
		Endpoints:        l.BoundAddrs(),
		Kind:             KindPing,
		HandshakeTimeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("impostor endpoint accepted — certificate pinning is not working")
	}
	if !strings.Contains(err.Error(), "failed") && !strings.Contains(err.Error(), "mismatch") &&
		!strings.Contains(err.Error(), "not") {
		t.Fatalf("unexpected failure mode: %v", err)
	}
}

// TestDialFailsForNonMember: dialing a device that is not in the view fails
// before any packet is sent.
func TestDialFailsForNonMember(t *testing.T) {
	f := newDirectFixture(t)
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	_, err = DialDirect(context.Background(), DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    stranger.ID(),
		Endpoints: []string{"127.0.0.1:1"},
		Kind:      KindPing,
	})
	if err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("non-member target must fail with membership reason, got: %v", err)
	}
}

// TestMsgRoundTrip: member-to-member message (R-4.1) — the listener's
// OnMessage fires with the sender's verified identity and the exact text.
func TestMsgRoundTrip(t *testing.T) {
	f := newDirectFixture(t)
	got := make(chan struct {
		from identity.DeviceID
		text string
	}, 4)
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
		OnMessage: func(from identity.DeviceID, text string) {
			got <- struct {
				from identity.DeviceID
				text string
			}{from, text}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	texts := []string{"first line", "第二行中文", ""}
	_ = texts[2]
	if err := SendMsg(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        l.BoundAddrs(),
		HandshakeTimeout: 5 * time.Second,
	}, texts[:2]); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	for _, want := range texts[:2] {
		select {
		case m := <-got:
			if m.from != f.bob.ID() {
				t.Fatalf("message attributed to %s, want %s", m.from, f.bob.ID())
			}
			if m.text != want {
				t.Fatalf("text mismatch: %q ≠ %q", m.text, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("message not delivered within 5s")
		}
	}
}

// TestFileRoundTrip: member-to-member file transfer (R-4.2) — 8 MB of
// random bytes, sha256 verified on both ends, saved into the receiver's
// inbox under its own name.
func TestFileRoundTrip(t *testing.T) {
	f := newDirectFixture(t)
	inbox := t.TempDir()
	savedCh := make(chan string, 1)
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:   f.aline,
		Members:  MemberListAdapter{List: f.members},
		Addrs:    []string{"127.0.0.1:0"},
		InboxDir: inbox,
		OnFile: func(from identity.DeviceID, savedPath string, h FileHeader) {
			savedCh <- savedPath
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	src := make([]byte, 8<<20)
	if _, err := crand.Read(src); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(t.TempDir(), "payload-大文件.bin")
	if err := os.WriteFile(srcPath, src, 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := SendFile(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        l.BoundAddrs(),
		HandshakeTimeout: 5 * time.Second,
	}, srcPath)
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	if h.SHA256 != fmt.Sprintf("%x", sha256.Sum256(src)) {
		t.Fatal("sender-side sha mismatch")
	}
	select {
	case saved := <-savedCh:
		got, err := os.ReadFile(saved)
		if err != nil {
			t.Fatalf("saved file unreadable: %v", err)
		}
		if len(got) != len(src) || !bytes.Equal(got, src) {
			t.Fatalf("saved file differs: %d bytes vs %d", len(got), len(src))
		}
		if filepath.Base(saved) != filepath.Base(srcPath) {
			t.Fatalf("saved name changed: %s", saved)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnFile never fired")
	}
}

// TestFileRefusedWithoutInbox: a listener with no inbox configured refuses
// the transfer with an actionable error BEFORE the bytes move.
func TestFileRefusedWithoutInbox(t *testing.T) {
	f := newDirectFixture(t)
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
		// no InboxDir
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	src := filepath.Join(t.TempDir(), "x.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = SendFile(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        l.BoundAddrs(),
		HandshakeTimeout: 5 * time.Second,
	}, src)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("no-inbox receiver must refuse before transfer, got: %v", err)
	}
}

// TestDialRejectsWildcardEndpoint pins the D-1 defense (issue #8): a peer
// that announced 0.0.0.0 must produce the actionable error, not a 10s dial
// into our own loopback.
func TestDialRejectsWildcardEndpoint(t *testing.T) {
	f := newDirectFixture(t)
	_, err := DialDirect(context.Background(), DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.aline.ID(),
		Endpoints: []string{"0.0.0.0:7000"},
		Kind:      KindPing,
	})
	if err == nil || !strings.Contains(err.Error(), "--endpoint") {
		t.Fatalf("wildcard endpoint must fail with the actionable reason, got: %v", err)
	}
}

// TestBoundAddrsFiltersWildcard pins the listener half of D-1: wildcard
// binds serve but are never announced.
func TestBoundAddrsFiltersWildcard(t *testing.T) {
	f := newDirectFixture(t)
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"0.0.0.0:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	if got := l.BoundAddrs(); len(got) != 0 {
		t.Fatalf("wildcard bind must not be announced, got %v", got)
	}
	if got := l.WildcardAddrs(); len(got) != 1 {
		t.Fatalf("wildcard bind should be reported for the warning, got %v", got)
	}
}

// TestRevokedMemberCannotConnect pins revocation on the listener side: once
// the device is out of the member list, its certificate no longer pins and
// the TLS handshake fails.
func TestRevokedMemberCannotConnect(t *testing.T) {
	f := newDirectFixture(t)
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	// Sanity: before revocation the dial works.
	link := dialTo(t, f, l.BoundAddrs(), KindPing)
	rtts, err := PingExchange(context.Background(), link, 1, 0)
	if err != nil || len(rtts) != 1 {
		t.Fatalf("pre-revocation ping failed: %v", err)
	}
	_ = link.Close()

	// Revocation: bob leaves the list.
	if err := f.members.Remove(f.bob.ID()); err != nil {
		t.Fatal(err)
	}
	_, err = DialDirect(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        l.BoundAddrs(),
		Kind:             KindPing,
		HandshakeTimeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("revoked member must fail to connect")
	}
}
