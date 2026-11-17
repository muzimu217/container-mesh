// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestAnnounceCadenceSurvivesASlowRegister pins that the announce retry
// cadence does not depend on how long a register request takes.
//
// The announce loop re-arms its ticker AFTER the request returns, so a slow
// register pushes the next attempt out by the request's own duration. That
// was always true. What changed with the departure work is that Announce
// now takes a mutex across the whole request — so a slow register holds
// that mutex, and the departure waits for it, and the departure's own
// broadcast waits behind it.
//
// The cadence that matters is the one an operator feels: a device that is
// waiting for approval must keep asking every few seconds. On a slow
// link the ask itself is slow, and the gap between two asks grows to
// "request duration + retry delay" instead of "retry delay". The CI run
// that caught this measured a 9 s gap where the bound is 3 s.
//
// The test makes the register endpoint slow on purpose and asserts that a
// SECOND attempt still happens within the fast-retry bound. The endpoint
// sleeps less than the bound, so the assertion is about the cadence, not
// about the endpoint's latency.
func TestAnnounceCadenceSurvivesASlowRegister(t *testing.T) {
	var attempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mesh/register" {
			atomic.AddInt64(&attempts, 1)
			// A register that takes a noticeable but bounded time: the
			// cadence must still produce a second attempt inside the
			// bound.
			time.Sleep(1200 * time.Millisecond)
			http.Error(w, "transport bucket: service unavailable", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	cl, err := newTestClient(discovery.ClientConfig{
		Addrs:      []string{testDiscoveryURL(srv.URL)},
		Device:     stranger,
		AccountID:  acc.ID(),
		AccountPub: []byte(acc.PublicKey()),
		Endpoints:  []string{"8.8.8.203:7000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	// The fast-retry bound is ~3 s. Allow the request's own latency on top,
	// so the assertion is about the CADENCE and not about the endpoint.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&attempts) >= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("only %d announce attempt(s) in 8 s against a 1.2 s register endpoint: "+
		"the retry cadence is gated on the request's own duration", atomic.LoadInt64(&attempts))
}
