// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package forward_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/forward"
	"github.com/muzimu217/container-mesh/internal/mux"
	"github.com/muzimu217/container-mesh/internal/secure"
	"github.com/muzimu217/container-mesh/internal/wire"

	"github.com/muzimu217/container-mesh/internal/peer"
)

// echoServer starts a TCP echo server and returns its address.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// newMsgConnPair builds two connected MessageConns, the same shape the real
// transport has: messages during bootstrap, bytes after the mux barrier.
func newMsgConnPair() (a, b *msgConn) {
	ab := make(chan []byte, 128)
	ba := make(chan []byte, 128)
	shared := &closer{closed: make(chan struct{})}
	return &msgConn{out: ab, in: ba, shared: shared},
		&msgConn{out: ba, in: ab, shared: shared}
}

type closer struct {
	once   sync.Once
	closed chan struct{}
}

func (c *closer) Close() { c.once.Do(func() { close(c.closed) }) }

type msgConn struct {
	out     chan []byte
	in      chan []byte
	shared  *closer
	pending []byte
}

func (c *msgConn) Send(b []byte) error {
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case c.out <- cp:
		return nil
	case <-c.shared.closed:
		return io.ErrClosedPipe
	}
}

func (c *msgConn) Recv() ([]byte, error) {
	select {
	case msg := <-c.in:
		return msg, nil
	case <-c.shared.closed:
		return nil, io.EOF
	}
}

func (c *msgConn) Read(b []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(b, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	msg, err := c.Recv()
	if err != nil {
		return 0, err
	}
	n := copy(b, msg)
	if n < len(msg) {
		c.pending = msg[n:]
	}
	return n, nil
}

func (c *msgConn) Write(b []byte) (int, error) {
	if err := c.Send(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *msgConn) Close() error { c.shared.Close(); return nil }

// sessionProvider adapts one mux.Session to forward.Provider.
//
// The adapter exists because Go has no covariant return types: mux.Session.Open
// returns *mux.Stream, while forward.Mux promises forward.Stream. Wrapping is
// one line and keeps internal/forward free of a yamux dependency.
type sessionProvider struct {
	mux *mux.Session
}

func (p sessionProvider) Current(context.Context) (forward.Mux, error) {
	if p.mux == nil || p.mux.IsClosed() {
		return nil, fmt.Errorf("no live session")
	}
	return muxAdapter{p.mux}, nil
}

// muxAdapter presents *mux.Session through forward.Mux.
type muxAdapter struct{ s *mux.Session }

func (a muxAdapter) Open(ctx context.Context, kind byte) (forward.Stream, error) {
	return a.s.Open(ctx, kind)
}

func (a muxAdapter) NumStreams() int { return a.s.NumStreams() }

// TestLocalForwardEndToEnd is the PR #2 acceptance test: a local listener, a
// real mux, a real echo target, bytes in both directions.
func TestLocalForwardEndToEnd(t *testing.T) {
	echoAddr := echoServer(t)

	// Allow-list permits the echo server's address.
	host, portStr, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatal(err)
	}
	ports, err := forward.ParsePortSet([]string{portStr})
	if err != nil {
		t.Fatal(err)
	}
	al, err := forward.NewAllowList(context.Background(), []string{host}, ports, nil)
	if err != nil {
		t.Fatal(err)
	}

	mgr := forward.New(forward.Config{Accept: true, Allow: al})
	defer mgr.Close()

	clientMux, acceptorMgr := startForwardPair(t, mgr)
	_ = acceptorMgr

	// The initiator's -L listener.
	lf, err := forward.NewLocalForTest(forward.Spec{
		Bind:   "127.0.0.1:0",
		Target: echoAddr,
	}, mgr)
	if err != nil {
		t.Fatalf("local forward: %v", err)
	}
	defer lf.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = lf.Serve(ctx, sessionProvider{mux: clientMux}) }()

	// A client connects to the local listener and echoes a payload.
	conn, err := net.Dial("tcp", lf.Addr().String())
	if err != nil {
		t.Fatalf("dial local listener: %v", err)
	}
	defer conn.Close()

	payload := make([]byte, 32<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, len(payload))
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload corrupted through the forward")
	}
}

// startForwardPair builds two sessions with mux and returns the initiator's mux
// plus the acceptor's manager, which serves forwarded streams.
func startForwardPair(t *testing.T, acceptor *forward.Manager) (*mux.Session, *forward.Manager) {
	t.Helper()
	ca, cb := newMsgConnPair()
	const code = "forward-pair-code"

	clientCh := make(chan *mux.Session, 1)
	serverCh := make(chan error, 1)

	go func() {
		sess, err := peer.Handshake(ca, peer.Config{Name: "a", Code: code, Role: secure.RoleA})
		if err != nil {
			clientCh <- nil
			return
		}
		var cm *mux.Session
		err = sess.StartMuxed(peer.Handler{}, nil, func(s *peer.Session) (io.ReadWriteCloser, error) {
			link, err := s.Pump().StartMux(context.Background())
			if err != nil {
				return nil, err
			}
			ms, err := mux.New(link, mux.DefaultConfig(mux.RoleClient))
			if err != nil {
				return nil, err
			}
			cm = ms
			go func() {
				_ = ms.Serve(context.Background(), func(st *mux.Stream) { _ = st.Close() })
			}()
			return ms.Open(context.Background(), mux.KindControl)
		})
		if err != nil {
			clientCh <- nil
			return
		}
		clientCh <- cm
	}()

	go func() {
		sess, err := peer.Handshake(cb, peer.Config{Name: "b", Code: code, Role: secure.RoleB})
		if err != nil {
			serverCh <- err
			return
		}
		ctlCh := make(chan io.ReadWriteCloser, 1)
		err = sess.StartMuxed(peer.Handler{}, nil, func(s *peer.Session) (io.ReadWriteCloser, error) {
			link, err := s.Pump().StartMux(context.Background())
			if err != nil {
				return nil, err
			}
			ms, err := mux.New(link, mux.DefaultConfig(mux.RoleServer))
			if err != nil {
				return nil, err
			}
			go func() {
				_ = ms.Serve(context.Background(), func(st *mux.Stream) {
					if st.Kind() == mux.KindControl {
						ctlCh <- st
						return
					}
					acceptor.Serve(context.Background(), st)
				})
			}()
			select {
			case ctl := <-ctlCh:
				return ctl, nil
			case <-time.After(10 * time.Second):
				return nil, context.DeadlineExceeded
			}
		})
		serverCh <- err
	}()

	cm := <-clientCh
	if cm == nil {
		t.Fatal("initiator session failed")
	}
	if err := <-serverCh; err != nil {
		t.Fatalf("acceptor: %v", err)
	}
	return cm, acceptor
}

// TestForwardRefusedWithoutAccept is the end-to-end version of the first gate:
// with --accept off, a connection to the local listener is closed without any
// bytes reaching the target.
func TestForwardRefusedWithoutAccept(t *testing.T) {
	echoAddr := echoServer(t)
	host, portStr, _ := net.SplitHostPort(echoAddr)
	ports, _ := forward.ParsePortSet([]string{portStr})
	al, err := forward.NewAllowList(context.Background(), []string{host}, ports, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Accept is false on purpose.
	mgr := forward.New(forward.Config{Accept: false, Allow: al})
	defer mgr.Close()

	clientMux, _ := startForwardPair(t, mgr)

	lf, err := forward.NewLocalForTest(forward.Spec{Bind: "127.0.0.1:0", Target: echoAddr}, mgr)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = lf.Serve(ctx, sessionProvider{mux: clientMux}) }()

	conn, err := net.Dial("tcp", lf.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// The peer refuses, so the local connection is closed without an echo.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("got %d bytes back with --accept off: %q", n, buf[:n])
	}
}

// TestStatusCodesReachTheClient proves the operator-facing diagnosis survives
// the wire: a denied forward is distinguishable from a dead target.
func TestStatusCodesReachTheClient(t *testing.T) {
	// The allow-list permits a port the echo server is not on.
	ports, _ := forward.ParsePortSet([]string{"1"})
	al, err := forward.NewAllowList(context.Background(), []string{"127.0.0.1"}, ports, nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr := forward.New(forward.Config{Accept: true, Allow: al})
	defer mgr.Close()

	clientMux, _ := startForwardPair(t, mgr)

	// Ask for a target the policy denies.
	st, err := clientMux.Open(context.Background(), mux.KindForward)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	_ = st.SetDeadline(time.Now().Add(5 * time.Second))
	if err := wire.WritePreamble(st, wire.Preamble{
		Version: wire.PreambleVersion, Kind: wire.KindDirect, Addr: "127.0.0.1:5432",
	}); err != nil {
		t.Fatalf("write preamble: %v", err)
	}
	code, reason, err := wire.ReadStatus(st)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if code != wire.StatusDenied {
		t.Fatalf("status %v (%s), want denied", code, reason)
	}
}
