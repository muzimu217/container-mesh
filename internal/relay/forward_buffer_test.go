// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// startTestRelayWithServer is startTestRelay plus access to the server, so a
// test can read its counters.
func startTestRelayWithServer(t *testing.T) (string, *Server, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	cfg := DefaultConfig()
	cfg.Addr = addr
	cfg.PairTimeout = 5 * time.Second
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := New(cfg)
	hs := srv.HTTPServer()
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return fmt.Sprintf("ws://%s/ws", addr), srv, func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = hs.Shutdown(ctx)
				<-errc
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("relay did not start")
	return "", nil, nil
}

// TestForwardLoopReusesTheGrownBuffer measures the relay's own buffer
// growth.
//
// The loop threads one read buffer through RecvInto. When a message does
// not fit, RecvInto grows its own copy and returns that. The loop must
// adopt the returned buffer; if it keeps its own stale slice, every
// oversized message starts the growth chain over (64, 128, 256, 512 KiB for
// a 300 KiB message).
//
// The symptom is invisible to a correctness test, because the bytes arrive
// intact either way. It shows up as allocation and CPU: makeslicecopy was
// 23.73% of the relay's CPU, and adopting the grown buffer cut it by 71% in
// an ABAB comparison.
//
// This is the stage-1 pairing relay, not the mesh relay trunk.
func TestForwardLoopReusesTheGrownBuffer(t *testing.T) {
	base, srv, stop := startTestRelayWithServer(t)
	defer stop()

	a := dial(t, base, "room-alloc", "A")
	defer a.Close()
	b := dial(t, base, "room-alloc", "B")
	defer b.Close()

	payload := bytes.Repeat([]byte("F"), 300_000)

	// One warm-up message: the first one pays the growth.
	if err := a.Send(payload); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := b.Recv(); err != nil {
		t.Fatal(err)
	}

	// Wait for the relay to have forwarded the warm-up message, then take
	// the baseline.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.messagesRelayed.Load() < 1 {
		time.Sleep(20 * time.Millisecond)
	}
	baseline := srv.readBufferGrowths.Load()

	// Now the same size again, many times. With the grown buffer adopted,
	// the relay must not grow again: the buffer it holds is already large
	// enough for this payload.
	const msgs = 20
	for i := 0; i < msgs; i++ {
		if err := a.Send(payload); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		_ = b.SetReadDeadline(time.Now().Add(10 * time.Second))
		got, err := b.Recv()
		if err != nil {
			t.Fatalf("recv %d: %v", i, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("message %d corrupted: %d bytes, want %d", i, len(got), len(payload))
		}
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.messagesRelayed.Load() < msgs+1 {
		time.Sleep(20 * time.Millisecond)
	}

	if grew := srv.readBufferGrowths.Load() - baseline; grew > 1 {
		t.Fatalf("the forward loop grew its read buffer %d times for %d messages "+
			"of the same size: it is not adopting the grown buffer and re-grows "+
			"for every message", grew, msgs)
	}
}
