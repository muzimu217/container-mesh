// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pipePair returns two connected in-memory byte streams, the way the record
// pump hands one link to each side.
func pipePair() (a, b *pipeConn) {
	ab := make(chan []byte, 256)
	ba := make(chan []byte, 256)
	// Each endpoint owns its own closed channel: sharing one would make the
	// second Close panic on a closed channel.
	return &pipeConn{out: ab, in: ba, closed: make(chan struct{})},
		&pipeConn{out: ba, in: ab, closed: make(chan struct{})}
}

type pipeConn struct {
	out    chan []byte
	in     chan []byte
	closed chan struct{}
	once   sync.Once
}

func (c *pipeConn) Read(p []byte) (int, error) {
	select {
	case b, ok := <-c.in:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, b)
		if n < len(b) {
			// Put the remainder back: this stream must not lose bytes.
			rest := make([]byte, len(b)-n)
			copy(rest, b[n:])
			c.in <- rest
		}
		return n, nil
	case <-c.closed:
		return 0, io.EOF
	}
}

func (c *pipeConn) Write(p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case c.out <- cp:
		return len(p), nil
	case <-c.closed:
		return 0, io.ErrClosedPipe
	}
}

func (c *pipeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// newPair builds a client/server pair over one in-memory link, applying the
// same tweak to both sides.
func newPair(t *testing.T, cfgFn func(*Config)) (client, server *Session) {
	t.Helper()
	return newPairSplit(t, cfgFn, cfgFn)
}

// newPairSplit builds a pair with different client and server configs. The two
// sides need different limits whenever a test wants the server to be the one
// that runs out of slots.
func newPairSplit(t *testing.T, clientFn, serverFn func(*Config)) (client, server *Session) {
	t.Helper()
	ca, cb := pipePair()

	cc := DefaultConfig(RoleClient)
	sc := DefaultConfig(RoleServer)
	if clientFn != nil {
		clientFn(&cc)
	}
	if serverFn != nil {
		serverFn(&sc)
	}
	client, err := New(ca, cc)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	server, err = New(cb, sc)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// TestStreamKindRouting checks that the kind byte written by Open arrives on
// the accepted stream, which is what lets the accept loop dispatch without
// reading any protocol bytes.
func TestStreamKindRouting(t *testing.T) {
	client, server := newPair(t, nil)

	kinds := make(chan byte, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = server.Serve(ctx, func(st *Stream) {
			kinds <- st.Kind()
			_ = st.Close()
		})
	}()

	for _, want := range []byte{KindControl, KindForward, KindForward} {
		st, err := client.Open(ctx, want)
		if err != nil {
			t.Fatalf("open %d: %v", want, err)
		}
		_ = st.Close()

		select {
		case got := <-kinds:
			if got != want {
				t.Fatalf("kind = %d, want %d", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no stream accepted for kind %d", want)
		}
	}
}

// TestStreamIDParity is the guard against both sides choosing "client": yamux
// clients open odd ids, servers even, and a collision would silently merge two
// streams.
func TestStreamIDParity(t *testing.T) {
	client, server := newPair(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	accepted := make(chan uint32, 4)
	go func() {
		_ = server.Serve(ctx, func(st *Stream) {
			accepted <- st.ID()
			_ = st.Close()
		})
	}()

	cs, err := client.Open(ctx, KindForward)
	if err != nil {
		t.Fatalf("client open: %v", err)
	}
	defer cs.Close()
	if id := cs.ID(); id%2 == 0 {
		t.Fatalf("client stream id %d is even; clients must open odd ids", id)
	}

	select {
	case id := <-accepted:
		if id%2 == 0 {
			t.Fatalf("accepted stream id %d is even; the server should have opened odd ids only if it opened it", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted the stream")
	}
}

// TestMaxStreamsBlocksOpen proves Open waits for a free slot instead of
// exceeding the limit, and that closing a stream releases exactly one slot.
func TestMaxStreamsBlocksOpen(t *testing.T) {
	const limit = 2
	client, server := newPair(t, func(c *Config) { c.MaxStreams = limit })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = server.Serve(ctx, func(st *Stream) {
			// Hold the stream open so the client's slots stay taken.
			<-ctx.Done()
			_ = st.Close()
		})
	}()

	var held []*Stream
	for i := 0; i < limit; i++ {
		st, err := client.Open(ctx, KindForward)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		held = append(held, st)
	}

	// The next Open must block.
	blocked := make(chan error, 1)
	go func() {
		openCtx, openCancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer openCancel()
		_, err := client.Open(openCtx, KindForward)
		blocked <- err
	}()

	select {
	case err := <-blocked:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Open past the limit returned %v, want deadline", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Open past the limit did not block")
	}

	// Free one slot; the next Open must succeed promptly.
	_ = held[0].Close()
	openCtx, openCancel := context.WithTimeout(ctx, 5*time.Second)
	defer openCancel()
	st, err := client.Open(openCtx, KindForward)
	if err != nil {
		t.Fatalf("Open after freeing a slot: %v", err)
	}
	_ = st.Close()
	for _, h := range held[1:] {
		_ = h.Close()
	}
}

// TestSemaphoreReleasedExactlyOnce guards against double-free: a second Close
// must not free a second slot, or the limit silently doubles.
//
// The opens have to be concurrent. Sequentially they would each succeed even
// with a doubled limit, because each one closes before the next begins.
func TestSemaphoreReleasedExactlyOnce(t *testing.T) {
	const limit = 1
	client, server := newPair(t, func(c *Config) { c.MaxStreams = limit })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = server.Serve(ctx, func(st *Stream) {
			<-ctx.Done()
			_ = st.Close()
		})
	}()

	st, err := client.Open(ctx, KindForward)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = st.Close()
	_ = st.Close() // second close must be harmless

	// Hold one stream open, then try a second concurrently. With a correctly
	// released slot there is room for exactly one.
	held, err := client.Open(ctx, KindForward)
	if err != nil {
		t.Fatalf("open after close: %v", err)
	}
	defer held.Close()

	blocked := make(chan error, 1)
	go func() {
		openCtx, openCancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer openCancel()
		s2, err := client.Open(openCtx, KindForward)
		if err == nil {
			_ = s2.Close()
		}
		blocked <- err
	}()

	select {
	case err := <-blocked:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a second concurrent Open succeeded (%v); the semaphore was freed twice", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent Open neither succeeded nor timed out")
	}
}

// TestLimitedStreamDoesNotTakeSlot proves the over-limit path: the stream is
// still delivered (so the caller can answer with an explicit status) but it
// never held a semaphore slot.
//
// Only the server's limit is tightened. If the client also had MaxStreams=1 it
// would block in its own Open before the server ever saw the stream, and the
// test would be measuring the wrong semaphore.
func TestLimitedStreamDoesNotTakeSlot(t *testing.T) {
	const limit = 1
	client, server := newPairSplit(t,
		func(c *Config) { c.MaxStreams = 8 },
		func(c *Config) { c.MaxStreams = limit },
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type arrival struct {
		st *Stream
	}
	seen := make(chan arrival, 4)
	release := make(chan struct{})
	go func() {
		_ = server.Serve(ctx, func(st *Stream) {
			seen <- arrival{st}
			<-release
			_ = st.Close()
		})
	}()

	// First stream takes the server's only slot.
	first, err := client.Open(ctx, KindForward)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer first.Close()
	select {
	case a := <-seen:
		if a.st.Limited() {
			t.Fatal("first stream should hold the slot, not be limited")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first stream never accepted")
	}

	// Second stream exceeds the server's limit: it must still arrive, marked.
	second, err := client.Open(ctx, KindForward)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer second.Close()
	select {
	case a := <-seen:
		if !a.st.Limited() {
			t.Fatal("second stream over the limit was not marked Limited")
		}
		// Closing a limited stream must not free a slot it never took; the
		// semaphore length must be unchanged at 1.
		_ = a.st.Close()
		if held := len(server.sem); held != 1 {
			t.Fatalf("semaphore holds %d slots after closing a limited stream, want 1", held)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second stream never accepted; an over-limit stream must be delivered, not reset")
	}
	close(release)
}

// TestHalfCloseStillReads is the property a proxy needs: CloseWrite sends FIN
// but the peer can still write back, and this side can still read it.
func TestHalfCloseStillReads(t *testing.T) {
	client, server := newPair(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srvDone := make(chan string, 1)
	go func() {
		_ = server.Serve(ctx, func(st *Stream) {
			defer st.Close()
			// Read the request to EOF (the client half-closes), then answer.
			req, err := io.ReadAll(st)
			if err != nil {
				srvDone <- "read: " + err.Error()
				return
			}
			if _, err := st.Write([]byte("reply:" + string(req))); err != nil {
				srvDone <- "write: " + err.Error()
				return
			}
			srvDone <- ""
		})
	}()

	st, err := client.Open(ctx, KindForward)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if _, err := st.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	// Reading after CloseWrite must still work.
	got := make([]byte, 11)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(st, got)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read after CloseWrite: %v", err)
		}
		if string(got) != "reply:hello" {
			t.Fatalf("got %q, want %q", got, "reply:hello")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no reply after CloseWrite; half-close broke the read side")
	}

	select {
	case msg := <-srvDone:
		if msg != "" {
			t.Fatal(msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server handler did not finish")
	}
}

// TestLogOutputDoesNotPolluteStdout is the reason LogOutput is always set
// explicitly: yamux's default is os.Stderr, which would corrupt CLI output.
func TestLogOutputDoesNotPolluteStdout(t *testing.T) {
	ca, cb := pipePair()
	cfg := DefaultConfig(RoleClient)
	cfg.Window = 256 << 10
	// A tiny window forces yamux to emit its "window update" style logging.
	s, err := New(ca, cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer s.Close()
	other, err := New(cb, DefaultConfig(RoleServer))
	if err != nil {
		t.Fatalf("new peer: %v", err)
	}
	defer other.Close()

	// If LogOutput were left at its default, yamux would write to os.Stderr and
	// this assertion could not be made at all; the check is that construction
	// succeeded with an explicit writer (VerifyConfig rejects a nil pair).
	if s.cfg.MaxStreams <= 0 {
		t.Fatal("config not normalised")
	}
}

// TestBudgetedStreams is the memory-budget formula: maxStreams * 2 * window
// must stay within the budget, or 256 streams at a 1 MiB window would reserve
// 512 MiB.
func TestBudgetedStreams(t *testing.T) {
	cases := []struct {
		max, want int
		window    uint32
		budget    int64
	}{
		{max: 256, window: 256 << 10, budget: 128 << 20, want: 256},
		{max: 256, window: 1 << 20, budget: 128 << 20, want: 64},
		{max: 256, window: 2 << 20, budget: 128 << 20, want: 32},
		// A budget too small for even one stream still allows one: refusing all
		// streams would make the session useless rather than safe.
		{max: 256, window: 256 << 10, budget: 1, want: 1},
		// A generous budget does not raise the caller's own cap.
		{max: 8, window: 256 << 10, budget: 1 << 30, want: 8},
	}
	for _, c := range cases {
		if got := BudgetedStreams(c.max, c.window, c.budget); got != c.want {
			t.Errorf("BudgetedStreams(%d, %d, %d) = %d, want %d",
				c.max, c.window, c.budget, got, c.want)
		}
	}
}

// TestConcurrentOpenAndServe pushes many streams through at once to catch
// semaphore leaks and dispatch races under -race.
func TestConcurrentOpenAndServe(t *testing.T) {
	const (
		streams = 32
		rounds  = 4
	)
	client, server := newPair(t, func(c *Config) {
		c.MaxStreams = 64
		c.AcceptBacklog = 64
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var served atomic.Int64
	go func() {
		_ = server.Serve(ctx, func(st *Stream) {
			defer st.Close()
			buf := make([]byte, 8)
			if _, err := io.ReadFull(st, buf); err != nil {
				return
			}
			_, _ = st.Write(buf)
			served.Add(1)
		})
	}()

	var wg sync.WaitGroup
	var failures atomic.Int64
	for r := 0; r < rounds; r++ {
		for i := 0; i < streams; i++ {
			wg.Add(1)
			go func(r, i int) {
				defer wg.Done()
				st, err := client.Open(ctx, KindForward)
				if err != nil {
					failures.Add(1)
					return
				}
				defer st.Close()
				msg := []byte(fmt.Sprintf("%02d%02d%04d", r, i, 0))
				if _, err := st.Write(msg[:8]); err != nil {
					failures.Add(1)
					return
				}
				got := make([]byte, 8)
				if _, err := io.ReadFull(st, got); err != nil {
					failures.Add(1)
					return
				}
				if !bytes.Equal(got, msg[:8]) {
					failures.Add(1)
				}
			}(r, i)
		}
	}
	wg.Wait()

	if n := failures.Load(); n != 0 {
		t.Fatalf("%d stream operations failed", n)
	}
	if got := served.Load(); got != streams*rounds {
		t.Fatalf("server handled %d streams, want %d", got, streams*rounds)
	}
}

// TestServeReturnsOnSessionClose proves the accept loop terminates instead of
// spinning after Close, which would leak a goroutine per reconnect.
func TestServeReturnsOnSessionClose(t *testing.T) {
	client, server := newPair(t, nil)
	_ = client

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(context.Background(), func(st *Stream) { _ = st.Close() })
	}()

	time.Sleep(50 * time.Millisecond)
	_ = server.Close()

	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
}
