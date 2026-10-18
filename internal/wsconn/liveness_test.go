// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package wsconn

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startLivenessServer upgrades every connection with the given liveness
// settings and hands the Conn to the channel.
func startLivenessServer(t *testing.T, l Liveness) (string, chan *Conn) {
	t.Helper()
	conns := make(chan *Conn, 4)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := newConn(ws)
		c.EnableLiveness(l)
		conns <- c
	}))
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http"), conns
}

// TestCloseUnblocksRecv is a regression test. Close() used to acquire the
// read mutex that a blocked Recv holds forever, deadlocking any caller that
// tried to tear down a live connection.
func TestCloseUnblocksRecv(t *testing.T) {
	lv := DefaultLiveness()
	url, srvConns := startLivenessServer(t, lv)

	client, err := Dial(t.Context(), DialOptions{URL: url, DialTimeout: 3 * time.Second, Liveness: &lv})
	if err != nil {
		t.Fatal(err)
	}
	<-srvConns // server side; left connected and idle

	blocked := make(chan error, 1)
	go func() {
		_, err := client.Recv()
		blocked <- err
	}()
	time.Sleep(200 * time.Millisecond) // let Recv block on the socket

	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked behind a blocked Recv")
	}

	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("expected Recv to fail after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv did not unblock after Close")
	}
}

// TestLivenessDetectsSilentPeer verifies the core promise of keepalive: a
// peer that stops reading (and therefore never answers pings) is noticed
// within the idle timeout instead of hanging forever.
func TestLivenessDetectsSilentPeer(t *testing.T) {
	fast := Liveness{PingEvery: 50 * time.Millisecond, IdleTimeout: 200 * time.Millisecond}
	url, srvConns := startLivenessServer(t, fast)

	client, err := Dial(t.Context(), DialOptions{URL: url, DialTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// The client never reads: pings go unanswered.

	dead := make(chan error, 1)
	go func() {
		c := <-srvConns
		for {
			if _, err := c.Recv(); err != nil {
				dead <- err
				return
			}
		}
	}()

	select {
	case err := <-dead:
		if err == nil {
			t.Fatal("expected a non-nil error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("silent peer was not detected within 3s")
	}
}

// TestLivenessSurvivesTraffic makes sure keepalive does not kill healthy,
// busy connections: both sides answer pings (gorilla auto-pongs during
// reads) and data keeps flowing past several ping periods.
func TestLivenessSurvivesTraffic(t *testing.T) {
	fast := Liveness{PingEvery: 50 * time.Millisecond, IdleTimeout: 200 * time.Millisecond}
	url, srvConns := startLivenessServer(t, fast)

	client, err := Dial(t.Context(), DialOptions{URL: url, DialTimeout: 3 * time.Second, Liveness: &fast})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	srv := <-srvConns

	// Client echoes whatever it receives.
	go func() {
		for {
			msg, err := client.Recv()
			if err != nil {
				return
			}
			if err := client.Send(msg); err != nil {
				return
			}
		}
	}()

	deadline := time.Now().Add(700 * time.Millisecond)
	sent := 0
	for time.Now().Before(deadline) {
		if err := srv.Send([]byte("probe")); err != nil {
			t.Fatalf("send: %v", err)
		}
		sent++
		_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := srv.Recv(); err != nil {
			t.Fatalf("healthy connection closed early after %d messages: %v", sent, err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if sent < 5 {
		t.Fatalf("test window too small: only %d exchanges", sent)
	}
}
