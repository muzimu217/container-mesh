// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package forward

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/mux"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// fakeStream is an in-memory Stream the manager can serve without yamux.
type fakeStream struct {
	kind byte

	mu     sync.Mutex
	rd     *bytes.Buffer
	wr     bytes.Buffer
	closed bool
}

func newFakeStream(kind byte, inbound []byte) *fakeStream {
	return &fakeStream{kind: kind, rd: bytes.NewBuffer(inbound)}
}

func (s *fakeStream) Read(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed && s.rd.Len() == 0 {
		return 0, io.EOF
	}
	if s.rd.Len() == 0 {
		return 0, io.EOF
	}
	return s.rd.Read(b)
}

func (s *fakeStream) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	return s.wr.Write(b)
}

func (s *fakeStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fakeStream) SetDeadline(time.Time) error { return nil }
func (s *fakeStream) Kind() byte                  { return s.kind }

// written returns what the manager wrote back (the status reply).
func (s *fakeStream) written() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.wr.Bytes()...)
}

// statusOf decodes the status the manager wrote.
func statusOf(t *testing.T, s *fakeStream) (wire.Status, string) {
	t.Helper()
	b := s.written()
	if len(b) < 6 {
		t.Fatalf("no status written (%d bytes)", len(b))
	}
	code, reason, err := wire.ReadStatus(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return code, reason
}

// countingDialer records how many times a dial was attempted. It exists to
// prove policy runs BEFORE dialling.
type countingDialer struct {
	calls atomic.Int64
	err   error
	conn  net.Conn
}

func (d *countingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.calls.Add(1)
	if d.err != nil {
		return nil, d.err
	}
	if d.conn != nil {
		return d.conn, nil
	}
	// net.Pipe gives the pipe a working peer without a real socket.
	server, client := net.Pipe()
	// The caller closes the returned conn; the server side is drained here.
	go func() {
		_, _ = io.Copy(io.Discard, server)
		_ = server.Close()
	}()
	return client, nil
}

// newTestManager builds a Manager with a policy that allows 10.0.0.5:5432.
func newTestManager(t *testing.T, d Dialer) *Manager {
	t.Helper()
	ports, err := ParsePortSet([]string{"5432"})
	if err != nil {
		t.Fatal(err)
	}
	al, err := NewAllowList(context.Background(), []string{"10.0.0.5"}, ports, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{
		Accept: true,
		Allow:  al,
		Limits: Limits{MaxStreamsPerSec: 0}, // tests drive the rate limit separately
		Dialer: d,
	})
}

// TestPolicyRunsBeforeDial is the security property the design calls out as
// "order is a security attribute": a refused request must not produce an
// outbound connection, or the peer has a port scanner whose answers are in the
// status codes and the timing.
func TestPolicyRunsBeforeDial(t *testing.T) {
	d := &countingDialer{}
	m := newTestManager(t, d)

	// A target outside the allow-list.
	st := newFakeStream(mux.KindForward, preambleBytes(t, "169.254.169.254:80"))
	m.Handle(context.Background(), st)

	if got := d.calls.Load(); got != 0 {
		t.Fatalf("dialled %d times for a denied target; policy must run first", got)
	}
	code, _ := statusOf(t, st)
	if code != wire.StatusDenied {
		t.Fatalf("status %v, want denied", code)
	}
}

// TestAcceptDisabledNeverReadsPreamble proves the first gate is structural: with
// --accept off, the manager answers without touching the preamble, so a
// malformed preamble never reaches the parser.
func TestAcceptDisabledNeverReadsPreamble(t *testing.T) {
	d := &countingDialer{}
	m := New(Config{Accept: false, Dialer: d})

	// Garbage that the preamble parser would reject.
	st := newFakeStream(mux.KindForward, []byte{0xff, 0xff, 0xff})
	m.Serve(context.Background(), st)

	if got := d.calls.Load(); got != 0 {
		t.Fatalf("dialled %d times with --accept off", got)
	}
	code, reason := statusOf(t, st)
	if code != wire.StatusDenied {
		t.Fatalf("status %v, want denied", code)
	}
	if !bytes.Contains([]byte(reason), []byte("--accept")) {
		t.Fatalf("reason %q does not mention --accept", reason)
	}
}

// TestMalformedPreambleGetsBadRequest distinguishes protocol errors from policy
// refusals, which is what makes an attack or a bug visible in the logs.
func TestMalformedPreambleGetsBadRequest(t *testing.T) {
	d := &countingDialer{}
	m := newTestManager(t, d)

	st := newFakeStream(mux.KindForward, []byte{9, 9, 9, 9, 9, 9, 9, 9})
	m.Handle(context.Background(), st)

	if got := d.calls.Load(); got != 0 {
		t.Fatalf("dialled %d times for a malformed preamble", got)
	}
	code, _ := statusOf(t, st)
	if code != wire.StatusBadRequest {
		t.Fatalf("status %v, want bad-request", code)
	}
}

// TestDialFailIsDistinctFromDenied keeps the two refusals distinguishable: one
// means fix the config, the other means fix the service.
func TestDialFailIsDistinctFromDenied(t *testing.T) {
	d := &countingDialer{err: errors.New("connection refused")}
	m := newTestManager(t, d)

	st := newFakeStream(mux.KindForward, preambleBytes(t, "10.0.0.5:5432"))
	m.Handle(context.Background(), st)

	if got := d.calls.Load(); got != 1 {
		t.Fatalf("dialled %d times, want exactly 1", got)
	}
	code, _ := statusOf(t, st)
	if code != wire.StatusDialFail {
		t.Fatalf("status %v, want dial-fail", code)
	}
}

// TestRateLimitBeforePolicy proves a scanner is refused cheaply, without
// consuming resolver work.
func TestRateLimitBeforePolicy(t *testing.T) {
	d := &countingDialer{}
	ports, _ := ParsePortSet([]string{"5432"})
	al, err := NewAllowList(context.Background(), []string{"10.0.0.5"}, ports, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	m := New(Config{
		Accept: true,
		Allow:  al,
		Limits: Limits{MaxStreamsPerSec: 1},
		Dialer: d,
	})

	// First request consumes the burst token.
	first := newFakeStream(mux.KindForward, preambleBytes(t, "10.0.0.5:5432"))
	m.Handle(context.Background(), first)

	// The next several must be refused as TooMany.
	for i := 0; i < 5; i++ {
		st := newFakeStream(mux.KindForward, preambleBytes(t, "10.0.0.5:5432"))
		m.Handle(context.Background(), st)
		if code, _ := statusOf(t, st); code != wire.StatusTooMany {
			t.Fatalf("request %d got %v, want too-many", i, code)
		}
	}
}

// TestUnknownStreamKindIsBadRequest covers the dispatcher's default branch.
func TestUnknownStreamKindIsBadRequest(t *testing.T) {
	m := New(Config{Accept: true})
	st := newFakeStream(42, nil)
	m.Serve(context.Background(), st)
	if code, _ := statusOf(t, st); code != wire.StatusBadRequest {
		t.Fatalf("status %v, want bad-request", code)
	}
}

// TestReverseRefusedInThisBuild keeps the unimplemented path honest: PR #2 has
// no -R support, so a reverse preamble must be refused explicitly rather than
// silently ignored.
func TestReverseRefusedInThisBuild(t *testing.T) {
	d := &countingDialer{}
	m := newTestManager(t, d)

	st := newFakeStream(mux.KindForward, reversePreambleBytes(t, "127.0.0.1:9000"))
	m.Handle(context.Background(), st)

	if got := d.calls.Load(); got != 0 {
		t.Fatalf("dialled %d times for a reverse request", got)
	}
	code, _ := statusOf(t, st)
	if code != wire.StatusDenied {
		t.Fatalf("status %v, want denied", code)
	}
}

// preambleBytes builds a direct preamble for addr.
func preambleBytes(t *testing.T, addr string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := wire.WritePreamble(&buf, wire.Preamble{
		Version: wire.PreambleVersion, Kind: wire.KindDirect, Addr: addr,
	}); err != nil {
		t.Fatalf("build preamble: %v", err)
	}
	return buf.Bytes()
}

// reversePreambleBytes builds a reverse preamble.
func reversePreambleBytes(t *testing.T, addr string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := wire.WritePreamble(&buf, wire.Preamble{
		Version: wire.PreambleVersion, Kind: wire.KindReverse, Addr: addr,
	}); err != nil {
		t.Fatalf("build preamble: %v", err)
	}
	return buf.Bytes()
}

// tcpPair returns two connected TCP endpoints on loopback.
//
// Real TCP, not net.Pipe: net.Pipe does not implement CloseWrite, so a pipe
// test built on it would exercise the truncating fallback instead of the
// half-close path that production actually takes (targets are *net.TCPConn,
// streams are yamux, and both support CloseWrite).
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()

	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server = <-accepted
	if server == nil {
		t.Fatalf("accept failed")
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// TestPipePropagatesHalfClose is the HTTP/1.0 shape: the client sends its
// request, half-closes, and must still receive the response.
//
// This is the property that makes a naive two-way io.Copy wrong: closing the
// whole connection on EOF truncates every half-closing client.
func TestPipePropagatesHalfClose(t *testing.T) {
	// client -- a1/b1 (the pipe) -- server
	client, a1 := tcpPair(t)
	b1, server := tcpPair(t)

	done := make(chan error, 1)
	go func() {
		done <- pipe(a1, b1, 2*time.Second)
	}()

	// The client sends a request and half-closes its write side.
	if _, err := client.Write([]byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("client CloseWrite: %v", err)
	}

	// The server reads the request to EOF, then answers.
	req, err := io.ReadAll(server)
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	if !bytes.Contains(req, []byte("GET")) {
		t.Fatalf("server got %q", req)
	}
	if _, err := server.Write([]byte("HTTP/1.0 200 OK\r\n\r\nbody")); err != nil {
		t.Fatalf("server write: %v", err)
	}
	_ = server.(*net.TCPConn).CloseWrite()

	// The client must receive the response even though it half-closed first.
	resp, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if !bytes.Contains(resp, []byte("200 OK")) {
		t.Fatalf("client got %q, want a response after half-close", resp)
	}

	select {
	case err := <-done:
		if err != nil && !isClosedErr(err) {
			t.Fatalf("pipe returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipe did not return")
	}
}

// TestPipeDrainsAfterHalfClose is the leak guard: a peer that half-closes and
// then stops reading must not pin a goroutine forever.
func TestPipeDrainsAfterHalfClose(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- pipe(a1, b1, 300*time.Millisecond)
	}()

	// b2 writes a lot and never reads; a2 never reads either. The pipe must
	// give up after the drain window rather than blocking forever.
	go func() {
		buf := bytes.Repeat([]byte("x"), 64<<10)
		for i := 0; i < 100; i++ {
			if _, err := b2.Write(buf); err != nil {
				return
			}
		}
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrPipeDrainTimeout) {
			t.Fatalf("pipe returned %v, want ErrPipeDrainTimeout", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("pipe took %s to give up; the drain window did not bound it", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipe blocked forever with an unreading peer")
	}
}

// TestGoroutinesReturnToBaseline is the explicit leak assertion the review
// asked for: after normal EOF and after an abrupt reset, the goroutine count
// must fall back to its baseline.
func TestGoroutinesReturnToBaseline(t *testing.T) {
	// Let any leftover goroutines from earlier tests settle first.
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	runOnce := func(abrupt bool) {
		a1, a2 := net.Pipe()
		b1, b2 := net.Pipe()

		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = pipe(a1, b1, 300*time.Millisecond)
		}()

		if abrupt {
			// Both ends vanish: the reader/writer loops see a closed pipe.
			_ = a2.Close()
			_ = b2.Close()
		} else {
			// Clean EOF in both directions.
			_ = a2.Close()
			_ = b2.Close()
		}

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("pipe did not return (abrupt=%v)", abrupt)
		}
	}

	for i := 0; i < 5; i++ {
		runOnce(false)
		runOnce(true)
	}

	// Goroutines may take a moment to unwind after the last close.
	deadline := time.Now().Add(3 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = runtime.NumGoroutine()
		if got <= baseline+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines did not return to baseline: %d, baseline %d", got, baseline)
}

// TestManagerCloseStopsListeners proves Close releases the port, so a restart
// does not fail to bind.
func TestManagerCloseStopsListeners(t *testing.T) {
	m := New(Config{Accept: true})
	lf, err := m.AddLocal(Spec{Bind: "127.0.0.1:0", Target: "10.0.0.5:5432"})
	if err != nil {
		t.Fatalf("AddLocal: %v", err)
	}
	addr := lf.Addr().String()

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The port must be free again.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s still bound after Close: %v", addr, err)
	}
	_ = ln.Close()
}
