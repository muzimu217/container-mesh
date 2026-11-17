// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTheOutboundTransportIsBuiltInOnePlace pins that the outbound HTTP
// transport has ONE builder.
//
// The client's register path used to carry its own copy of the transport,
// with the policy wired in the same way as the exported one but with one
// difference: the policy there could be switched off (AllowLocalDiscovery,
// for the local test harness). Two copies of a dialer setup are two places
// to keep the policy in step — and the SSRF finding of round 56 was
// exactly the kind of drift that a second copy invites.
//
// The behaviour that matters is pinned here: a transport built with a
// policy REFUSES a private address, and one built without a policy does
// not.
func TestTheOutboundTransportIsBuiltInOnePlace(t *testing.T) {
	// A listener on the loopback: the transport must not reach it when a
	// policy is in force.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	// With the default policy the loopback must be refused.
	tr := EgressTransport(2 * time.Second)
	client := &http.Client{Transport: tr}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("the transport with a policy reached a loopback address")
	}

	// Without a policy it must get through — that is the escape hatch the
	// local test harness needs, and it has to keep working.
	tr2 := PolicyTransport(2*time.Second, nil)
	client2 := &http.Client{Transport: tr2}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	req2, err := http.NewRequestWithContext(ctx2, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := client2.Do(req2)
	if err != nil {
		t.Fatalf("the transport without a policy did not reach the listener: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp2.StatusCode)
	}
}
