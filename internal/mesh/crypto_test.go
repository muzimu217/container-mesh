// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestDialCryptoFailureIsAuth: the peer REJECTS our certificate.
//
// This is the subtle case the error classification exists for. quic.Dial
// does NOT fail here: the rejection arrives after the handshake starts,
// so the dial returns a live connection and the error only surfaces when
// the hello is written or the ack is read. A classifier that only looked
// at the dial's return value would see "success" and then a mysterious
// I/O error — with no idea that the credentials were the problem.
//
// The test drives the REAL listener with a member resolver that refuses
// the dialer's key, so the listener's certificate pin rejects the
// dialer's certificate, and asserts that the error the caller sees
// carries ErrAuth.
func TestDialCryptoFailureIsAuth(t *testing.T) {
	f := newDirectFixture(t)

	// The listener refuses every peer's key: its pin rejects the
	// dialer's certificate, which is "the remote refused our
	// certificate" seen from the dialer.
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: refusingResolver{},
		Addrs:   []string{"127.0.0.1:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	_, err = DialDirect(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        l.BoundAddrs(),
		Kind:             KindPing,
		HandshakeTimeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("a rejected certificate must fail the dial")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("a rejected certificate must classify as ErrAuth, got %v", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Fatalf("a rejected certificate must not classify as ErrUnreachable, got %v", err)
	}
}

// refusingResolver refuses every key lookup.
type refusingResolver struct{}

func (refusingResolver) KeyFor(id identity.DeviceID) (ed25519.PublicKey, error) {
	return nil, errors.New("mesh: refusing resolver")
}
