// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// ---------------------------------------------------------------------------
// A REVOCATION MUST DESTROY THE ACTIVE SESSION, NOT ONLY REFUSE THE NEXT
// HANDSHAKE.
//
// The requirement (docs/REQUIREMENTS.md:239) says: "撤销一台设备的身份，
// 其活跃会话在一个推送往返内被销毁且无法重连". It does not say "relay
// sessions" — it says ACTIVE SESSIONS.
//
// The relay trunk has had a revocation watch for rounds
// (RevokeFlows / StartRevocationWatch), so a revoked device's RELAY
// flow was torn down. The DIRECT path had NO watch at all: revoking a
// device that was mid-transfer over a direct link left that transfer
// running until it finished on its own. Only NEW handshakes were
// refused.
//
// The acceptance checklist's C1 row ("撤销拆流（直连）" — the revocation
// destroys an established direct session) therefore had no evidence
// behind it: not a failing test, not a passing one — nothing.
//
// The teardown is the CONNECTION, because one connection carries every
// stream of that peer, and "its active sessions are destroyed" means
// all of them.
//
// The watch polls the member view once a second, for the same reason
// the relay's watch does: the push of a new member view can take up to
// a second (the snapshot is coalesced), so the revocation must not
// depend on the push alone.
// ---------------------------------------------------------------------------

// TestARevocationDestroysAnEstablishedDirectSession is the regression
// test for the gap.
//
// The session is established FIRST, then the member is revoked, and the
// established session must die — without the peer doing anything.
func TestARevocationDestroysAnEstablishedDirectSession(t *testing.T) {
	f := newDirectFixture(t)
	l := startListener(t, f)

	// The session is up and working.
	link := dialTo(t, f, l.BoundAddrs(), KindPing)
	rtts, err := PingExchange(context.Background(), link, 1, 0)
	if err != nil || len(rtts) != 1 {
		t.Fatalf("the pre-revocation ping failed: %v", err)
	}

	// Bob leaves the member list — that is what `cred revoke` does to
	// the roster.
	if err := f.members.Remove(f.bob.ID()); err != nil {
		t.Fatal(err)
	}

	// The ESTABLISHED session must now fail. A ping on it must not
	// succeed: the connection has to be torn down, not left running.
	//
	// The deadline is generous (the watch ticks once a second) because
	// this test asserts that the teardown HAPPENS, not how fast — and a
	// tight deadline here would make it a load-sensitive flake, which
	// is the mistake this project has already made once (see the
	// eviction/deadline split in internal/relay).
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := PingExchange(context.Background(), link, 1, 0)
		if err != nil {
			// The session died. That is the requirement.
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the revoked device's established direct session is still alive after %v: "+
				"a revocation must destroy the active sessions, not only refuse the next handshake",
				10*time.Second)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestAnExpiredCertificateIsNotARevocation pins the distinction the
// watch depends on.
//
// The watch asks "is this device still in the roster?" — NOT "does
// KeyFor still succeed?". KeyFor fails for THREE reasons, and only the
// first of them is a revocation:
//
//	"it is not a member"        ← the revocation
//	"its certificate expired"   ← a normal, recoverable event
//	"no member view is available"
//
// A peer whose certificate merely LAPSED must keep its session: the
// product renews certificates for exactly that reason, and tearing a
// legitimate session down because a timer ran out is a bug that is
// worse than the gap it fixes.
func TestAnExpiredCertificateIsNotARevocation(t *testing.T) {
	f := newDirectFixture(t)
	l := startListener(t, f)

	link := dialTo(t, f, l.BoundAddrs(), KindPing)

	// The peer's certificate lapses. It is STILL in the roster.
	//
	// The fixture issues certificates for an hour; this replaces the
	// member with one whose certificate was issued with a one-second
	// life and has already run out.
	expired, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := expired.IssueCert(f.bob, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Backdate it past its expiry.
	cert.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	// The roster rejects a re-add of the same device with a different
	// account's signature, so replace the row.
	if err := f.members.Remove(f.bob.ID()); err != nil {
		t.Fatal(err)
	}
	if err := f.members.Add(*cert); err != nil {
		t.Fatal(err)
	}

	// The session must SURVIVE: an expired certificate is not a
	// revocation.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		rtts, err := PingExchange(context.Background(), link, 1, 0)
		if err != nil {
			t.Fatalf("the session of a member whose certificate merely EXPIRED was torn down: %v "+
				"(an expiry is not a revocation; the product renews certificates for that reason)", err)
		}
		if len(rtts) != 1 {
			t.Fatalf("the ping returned %d results, want 1", len(rtts))
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func TestTheDirectRevocationWatchLeavesAValidSessionAlone(t *testing.T) {
	f := newDirectFixture(t)
	l := startListener(t, f)

	link := dialTo(t, f, l.BoundAddrs(), KindPing)

	// Several seconds — more than a few watch ticks — of the session
	// staying usable.
	for i := 0; i < 5; i++ {
		time.Sleep(600 * time.Millisecond)
		rtts, err := PingExchange(context.Background(), link, 1, 0)
		if err != nil {
			t.Fatalf("the session of a VALID member died after %v: %v",
				time.Duration(i+1)*600*time.Millisecond, err)
		}
		if len(rtts) != 1 {
			t.Fatalf("the ping returned %d results, want 1", len(rtts))
		}
	}
}
