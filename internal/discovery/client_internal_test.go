// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

// Internal test for the reconnect penalty: the ladder must grow on genuine
// failures and RESET after a healthy stream — the latter is what stops a
// server-side cleanup kick (the server dropping a slow consumer) from being
// charged to the client as a failure (issue #8 #7, netns-probe round 7:
// repeated kicks walked the delay 1s→2s→…→30s, and a state change landing
// in that gap arrived 30s late).

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wsconn"
)

func testDiscoveryURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	u.Host = net.JoinHostPort("mesh.example", u.Port())
	return u.String()
}

type testDiscoveryTransport struct {
	base http.RoundTripper
}

func (t testDiscoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	if u.Hostname() == "mesh.example" {
		u.Host = net.JoinHostPort("127.0.0.1", u.Port())
	}
	clone := r.Clone(r.Context())
	clone.URL = &u
	clone.Host = u.Host
	return t.base.RoundTrip(clone)
}

func testDiscoveryHTTPClient() *http.Client {
	return &http.Client{Transport: testDiscoveryTransport{base: http.DefaultTransport}}
}

func testDiscoveryWebSocketDial(ctx context.Context, opts wsconn.DialOptions) (*wsconn.Conn, error) {
	u, err := url.Parse(opts.URL)
	if err != nil {
		return nil, err
	}
	if u.Hostname() == "mesh.example" {
		u.Host = net.JoinHostPort("127.0.0.1", u.Port())
	}
	opts.URL = u.String()
	// This fixture deliberately redirects a logical host name to loopback,
	// which is what the egress policy refuses. The production path keeps the
	// policy; a test that opts into a loopback destination drops it here.
	opts.EgressPolicy = nil
	return wsconn.Dial(ctx, opts)
}

// TestReconnectLadderThenReset drives the client's reconnect behaviour
// against a server that fails a few times, then succeeds, then drops the
// stream. Expected: gaps 1s, 2s, 4s while failing (the ladder), then a
// ~1s gap after the healthy stream (the reset).
func TestReconnectLadderThenReset(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	node := NewNode(members)

	var mu sync.Mutex
	var attempts []time.Time
	live := make(chan *wsconn.Conn, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts = append(attempts, time.Now())
		n := len(attempts)
		mu.Unlock()
		if n <= 3 {
			// Genuine failure: the client must back off progressively.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := wsconn.Upgrade(w, r)
		if err != nil {
			return
		}
		snap := BuildSnapshot(members, node, time.Now())
		b, err := json.Marshal(snap)
		if err != nil {
			return
		}
		_ = conn.Send(b)
		select {
		case live <- conn:
		default:
		}
		// Hold the stream until the client goes away.
		for {
			if _, err := conn.Recv(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	cl, err := NewClient(ClientConfig{
		Addrs:         []string{testDiscoveryURL(srv.URL)},
		Device:        dev,
		AccountID:     acc.ID(),
		AccountPub:    []byte(acc.PublicKey()),
		NoAnnounce:    true,
		HTTPClient:    testDiscoveryHTTPClient(),
		WebSocketDial: testDiscoveryWebSocketDial,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	// Wait for the healthy connection (attempt 4) and its snapshot.
	deadline := time.Now().Add(20 * time.Second)
	for cl.Snapshot() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if cl.Snapshot() == nil {
		t.Fatal("client never received a snapshot")
	}

	// Drop the healthy stream: the reconnect must be prompt (~1s), because a
	// healthy stream resets the penalty.
	var last *wsconn.Conn
	select {
	case last = <-live:
	case <-time.After(5 * time.Second):
		t.Fatal("no live connection to drop")
	}
	mu.Lock()
	before := len(attempts)
	mu.Unlock()
	_ = last.Close()

	// The client reconnects; the new attempt must land quickly.
	deadline = time.Now().Add(8 * time.Second)
	for {
		mu.Lock()
		n := len(attempts)
		mu.Unlock()
		if n > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client did not reconnect after the stream was dropped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	gap := attempts[len(attempts)-1].Sub(attempts[before-1])
	mu.Unlock()
	t.Logf("reconnect gap after a healthy stream: %s", gap.Round(time.Millisecond))
	if gap > 4*time.Second {
		t.Fatalf("reconnect took %s — the penalty was not reset after a healthy stream", gap.Round(time.Millisecond))
	}
}
