// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package wsconn

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// pairOverHTTP builds a client and a server conn over a real httptest server.
func pairOverHTTP(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	srvCh := make(chan *Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			return
		}
		srvCh <- c
		select {}
	}))
	t.Cleanup(srv.Close)
	cli, err := Dial(context.Background(), DialOptions{URL: "ws" + srv.URL[4:]})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	srvConn := <-srvCh
	t.Cleanup(func() { _ = srvConn.Close() })
	return cli, srvConn
}

func TestRecvIntoRoundTrip(t *testing.T) {
	cli, srv := pairOverHTTP(t)

	payload := bytes.Repeat([]byte("Z"), 100_000)
	if err := cli.Send(payload); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024) // deliberately small: forces growth
	got, err := srv.RecvInto(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload corrupted: %d bytes, want %d", len(got), len(payload))
	}
	if cap(got) < len(payload) {
		t.Fatalf("buffer did not grow: cap %d < %d", cap(got), len(payload))
	}
}

func TestRecvIntoReusesBufferAcrossCalls(t *testing.T) {
	cli, srv := pairOverHTTP(t)

	buf := make([]byte, 4096)
	for i := 0; i < 3; i++ {
		payload := bytes.Repeat([]byte{byte('a' + i)}, 5000)
		if err := cli.Send(payload); err != nil {
			t.Fatal(err)
		}
		got, err := srv.RecvInto(buf)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("message %d corrupted", i)
		}
		buf = got // thread it back, as the relay pump does
	}
}

// TestRecvIntoIgnoresTextFrames keeps RecvInto's parity with Recv: non-binary
// frames are drained and skipped, not returned and not fatal.
func TestRecvIntoIgnoresTextFrames(t *testing.T) {
	cli, srv := pairOverHTTP(t)

	// A raw text frame, sent by reaching around wsconn (which only sends
	// binary). Use the underlying gorilla conn through the test hook.
	if err := cli.sendRawText("skip me"); err != nil {
		t.Fatal(err)
	}
	if err := cli.Send([]byte("real")); err != nil {
		t.Fatal(err)
	}
	got, err := srv.RecvInto(make([]byte, 128))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "real" {
		t.Fatalf("got %q, want the message after the text frame", got)
	}
}

func TestRecvIntoPropagatesError(t *testing.T) {
	cli, srv := pairOverHTTP(t)
	_ = cli
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RecvInto(make([]byte, 128)); err == nil {
		t.Fatal("RecvInto on a closed conn succeeded")
	}
}
