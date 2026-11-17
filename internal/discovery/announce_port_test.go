// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestPreJoinCadenceSurvivesAPortContaining429 is the CI failure, written
// down so it cannot come back unnoticed.
//
// The register endpoint answers 503 — a transport failure, which the
// client must retry FAST while it waits for approval. The node's ADDRESS
// carried a port whose digits contain "429", and the rate-limit test read
// "429" out of the whole error string. The client then backed off 8–12 s
// instead of 3 s, and the second attempt did not happen inside the 6 s
// bound.
//
// On a CI runner that happened to hand out port 42933, this turned a
// passing test red. It was not a test-only problem: every join against a
// node on such a port paid the same penalty, silently.
//
// The address here is built to contain "429" in the PORT, and the port is
// the one the httptest server got — so the test reproduces whatever the
// machine hands out, and the assertion holds for the real one.
func TestPreJoinCadenceSurvivesAPortContaining429(t *testing.T) {
	var attempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mesh/register" {
			atomic.AddInt64(&attempts, 1)
			http.Error(w, "transport bucket: service unavailable", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// The port the server actually got. The test asserts the cadence for
	// THIS port, so it holds whatever the machine hands out — including
	// the 42933 that broke CI.
	port := strings.TrimPrefix(srv.Listener.Addr().String(), "127.0.0.1:")
	t.Logf("register endpoint port: %s", port)

	// The verdict the client will take from a 503 at this address. The
	// CI failure was an address whose PORT contained "429"; the verdict
	// must not depend on that, so it is checked here for the port the
	// machine chose AND for the shape that broke CI.
	for _, p := range []string{port, "42933", "4290", "1429"} {
		rej := &discovery.RegisterRejectedError{
			Addr:   "http://mesh.example:" + p,
			Status: http.StatusServiceUnavailable,
			Body:   "transport bucket: service unavailable",
		}
		if discovery.IsRateLimited(rej) {
			t.Fatalf("a 503 from port %s was classified as rate-limited", p)
		}
	}

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
		Endpoints:  []string{"8.8.8.204:7000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	// The fast-retry bound is ~3 s, and the bound here is 6 s: the second
	// attempt must land inside it.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&attempts) >= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("only %d announce attempt(s) in 6 s against a 503 endpoint on port %s: "+
		"the retry cadence was gated on the address, not on the response",
		atomic.LoadInt64(&attempts), srv.URL)
}
