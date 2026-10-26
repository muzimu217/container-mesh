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

// benchPair builds a connected wsconn pair over real HTTP.
func benchPair(b *testing.B) (*Conn, *Conn) {
	b.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	b.Cleanup(srv.Close)
	cli, err := Dial(testCtx(b), DialOptions{URL: "ws" + srv.URL[4:]})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = cli.Close() })
	// A reader on the client side to drain echoes.
	go func() {
		for {
			if _, err := cli.Recv(); err != nil {
				return
			}
		}
	}()
	return cli, cli
}

func testCtx(b *testing.B) context.Context {
	return context.Background()
}

// BenchmarkSendRecv32K measures the client-side round trip with 32 KiB
// messages — the size the record pump puts on the wire in mux mode.
func BenchmarkSendRecv32K(b *testing.B) {
	c, _ := benchPair(b)
	payload := bytes.Repeat([]byte("x"), 32<<10)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Send(payload); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRecvIntoServerSide measures the server-side read of 32 KiB
// messages with a threaded buffer — the shape of the relay splice loop after
// the RecvInto change. Compare allocs/op against the Recv path (the server
// side of BenchmarkSendRecv32K).
func BenchmarkRecvIntoServerSide(b *testing.B) {
	srvCh := make(chan *Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			return
		}
		srvCh <- c
		buf := make([]byte, 64<<10)
		for {
			msg, err := c.RecvInto(buf)
			if err != nil {
				return
			}
			buf = msg[:cap(msg)]
		}
	}))
	b.Cleanup(srv.Close)
	cli, err := Dial(context.Background(), DialOptions{URL: "ws" + srv.URL[4:]})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = cli.Close() })
	<-srvCh

	payload := bytes.Repeat([]byte("x"), 32<<10)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cli.Send(payload); err != nil {
			b.Fatal(err)
		}
	}
}
