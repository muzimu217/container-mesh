// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package wsconn

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// startEchoServer runs an HTTP server whose /ws endpoint upgrades and echoes
// every binary message back.
func startEchoServer(t *testing.T) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			msg, err := c.Recv()
			if err != nil {
				return
			}
			if err := c.Send(msg); err != nil {
				return
			}
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()

	return fmt.Sprintf("ws://%s/ws", addr), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

func TestDialAndEcho(t *testing.T) {
	url, stop := startEchoServer(t)
	defer stop()

	c, err := Dial(context.Background(), DialOptions{URL: url, DialTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for _, msg := range []string{"one", "two", "three"} {
		if err := c.Send([]byte(msg)); err != nil {
			t.Fatalf("send %q: %v", msg, err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, err := c.Recv()
		if err != nil {
			t.Fatalf("recv %q: %v", msg, err)
		}
		if string(got) != msg {
			t.Errorf("got %q, want %q", got, msg)
		}
	}
}

func TestDialToUnreachableFails(t *testing.T) {
	// Reserve a port then close it, so nothing is listening.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	_, err = Dial(context.Background(), DialOptions{
		URL:         fmt.Sprintf("ws://%s/ws", addr),
		DialTimeout: 500 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("dial to a closed port should fail")
	}
}

func TestDialBadSchemeFails(t *testing.T) {
	_, err := Dial(context.Background(), DialOptions{
		URL:         "http://127.0.0.1:1/ws",
		DialTimeout: 300 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("http scheme should be rejected for a websocket dial")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	url, stop := startEchoServer(t)
	defer stop()

	c, err := Dial(context.Background(), DialOptions{URL: url, DialTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close should be a no-op, got %v", err)
	}
}

func TestConcurrentSendIsSafe(t *testing.T) {
	url, stop := startEchoServer(t)
	defer stop()

	c, err := Dial(context.Background(), DialOptions{URL: url, DialTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const n = 20
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			errCh <- c.Send([]byte(fmt.Sprintf("m%d", i)))
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent send: %v", err)
		}
	}
}
