// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestDialAuthFailureIsNotMaskedByLaterEndpoint pins the classification
// precedence inside the multi-endpoint dial.
//
// A member announces more than one endpoint. Suppose the FIRST endpoint
// answers with a certificate the member view does not accept — an
// authentication failure — and the SECOND endpoint simply does not
// answer. If the dial keeps the LAST error, the auth failure is erased
// and the caller sees "unreachable", which would let the path layer fall
// back to a relay with the same (rejected) credentials.
//
// The auth failure is the more informative verdict, so it must win.
func TestDialAuthFailureIsNotMaskedByLaterEndpoint(t *testing.T) {
	f := newDirectFixture(t)

	// The impostor answers on the first endpoint with a key that is NOT
	// the target's — the pin rejects it (a crypto error, i.e. ErrAuth).
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

	// The second endpoint answers nobody.
	dead := deadEndpoint(t)

	_, err = DialDirect(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(), // dial the REAL member, reach the impostor
		Endpoints:        []string{l.BoundAddrs()[0], dead},
		Kind:             KindPing,
		HandshakeTimeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("an impostor endpoint must be rejected")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("the auth failure must survive the later dead endpoint, got %v", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Fatalf("the auth failure must not be reclassified as unreachable, got %v", err)
	}
}

// TestDialRefusalIsNotMaskedByLaterEndpoint pins the same precedence for
// a refusal, through the rule that decides which endpoint failure the
// caller sees.
//
// The refusal verdict itself ("listener refused: ...") is raised by the
// listener's hello answer, and the listener answers "ok" for every kind
// it accepts — so the refusal path is exercised at the unit level here
// rather than end to end.
func TestDialRefusalIsNotMaskedByLaterEndpoint(t *testing.T) {
	refusal := classifyDialError(newRefused("listener refused: %s", "no inbox configured"))
	if !errors.Is(refusal, ErrRefused) {
		t.Fatalf("a refusal must classify as ErrRefused, got %v", refusal)
	}
	unreachable := classifyDialError(fmt.Errorf("quic dial: %w", context.DeadlineExceeded))

	// The refusal comes first, the unreachable second.
	got := preferDialError(refusal, unreachable)
	if !errors.Is(got, ErrRefused) {
		t.Fatalf("the refusal verdict must win over a later unreachable, got %v", got)
	}
	// And the other order.
	got = preferDialError(unreachable, refusal)
	if !errors.Is(got, ErrRefused) {
		t.Fatalf("the refusal verdict must win over an earlier unreachable, got %v", got)
	}
}

// TestPreferDialErrorKeepsVerdict: an auth verdict also survives a later
// network failure, in either order.
func TestPreferDialErrorKeepsVerdict(t *testing.T) {
	auth := classifyDialError(fmt.Errorf("read ack: %w", ErrAuth))
	unreachable := classifyDialError(fmt.Errorf("quic dial: %w", context.DeadlineExceeded))

	for _, tc := range []struct {
		name string
		a, b error
	}{
		{"verdict first", auth, unreachable},
		{"verdict second", unreachable, auth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := preferDialError(tc.a, tc.b)
			if !errors.Is(got, ErrAuth) {
				t.Fatalf("the auth verdict must win, got %v", got)
			}
		})
	}
}
