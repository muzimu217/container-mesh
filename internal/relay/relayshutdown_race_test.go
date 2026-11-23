// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A RELAY THAT IS STOPPING MUST NOT ADMIT CONNECTIONS.
//
// An external tester reported this one on a 2-core container under -race:
// "the pairing socket was never closed by the server after Shutdown (recv
// timed out, no close arrived): i/o timeout (20s)".
//
// I chased the ordering and the shape is this:
//
//	Shutdown:  s.shutdownReq = true          ← the relay is now stopping
//	Shutdown:  hs.Shutdown(ctx)              ← the listener closes here
//	Shutdown:  s.closeHijacked()             ← every KNOWN socket is closed
//	Shutdown:  s.waitConnsGone(ctx)          ← waits for them to be gone
//
// The window is the gap between "the relay is stopping" and "the
// listener is closed": a connection that arrives in that gap upgrades
// its socket, and net/http's Shutdown — which only waits for the
// handlers it knows about — does not close a hijacked socket.
//
// The fix is the ORDER OF ADMISSION, not the order of cleanup: a relay
// that is stopping must refuse the connection instead of adopting a
// socket it is not going to be able to close.
//
// The guard belongs at the TOP of handleWS, before the upgrade, for two
// reasons:
//
//   - a socket that was never hijacked is one net/http still knows
//     about, so Shutdown waits for it and closes it;
//   - and the client gets a truthful answer ("this relay is shutting
//     down") instead of a connection that outlives the server.
//
// The second half of the fix is in trackConn: a socket that arrives too
// late to be registered is closed on the spot. That half closes the
// window itself rather than relying on the guard winning the race.
// ---------------------------------------------------------------------------

// TestARelayThatIsStoppingRefusesNewConnections is the regression test.
//
// It arms the shutdown flag the way Shutdown does, and then checks that
// a connection that arrives afterwards is REFUSED — with a status the
// client can read, and without the relay adopting a socket it will not
// be able to close.
//
// The flag is armed directly rather than by calling Shutdown, because
// Shutdown closes the listener immediately after — and then there is no
// window to test. The flag IS the state that decides admission, so
// arming it is exactly the condition under test.
func TestARelayThatIsStoppingRefusesNewConnections(t *testing.T) {
	addr := reserveTestAddr(t)

	cfg := DefaultConfig()
	cfg.Addr = addr
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	// ★ STOP THE RELAY WHEN THE TEST ENDS. (Round 102.)
	//
	// The relay was never stopped here, so its listener stayed open for
	// the rest of the process. With -count=100 that is a hundred
	// listeners, and reserveTestAddr hands out ports from the OS's
	// ephemeral pool by binding :0 and closing it — so once the pool
	// wraps, a test gets a port that one of the previous iterations
	// still holds, the new relay cannot bind, and the dial fails with
	// "connection refused".
	//
	// That reads like a product defect and is not one: it is a leaked
	// listener in a test, and it only shows up when the test is run
	// repeatedly — which is exactly how a flake is hunted.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	waitAccept(t, addr)

	// Arm the shutdown flag exactly as Shutdown does.
	srv.srvMu.Lock()
	srv.shutdownReq = true
	srv.srvMu.Unlock()

	// The connection must be REFUSED, and the refusal must be
	// readable — the client has to learn that the relay is stopping
	// rather than seeing a socket that dies for no stated reason.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET /ws?room=late&role=A HTTP/1.1\r\n"+
		"Host: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n",
		addr); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("reading the refusal: %v", err)
	}
	resp := string(buf[:n])
	if !strings.Contains(resp, "503") {
		t.Fatalf("a relay that is stopping answered %q, want 503", strings.SplitN(resp, "\r\n", 2)[0])
	}
	if !strings.Contains(resp, "shutting down") {
		t.Fatalf("the refusal does not say WHY: %q", strings.SplitN(resp, "\r\n", 2)[0])
	}

	// And the relay must not have adopted a socket: it holds none.
	srv.mu.Lock()
	n2 := len(srv.live)
	for _, m := range srv.streams {
		n2 += len(m)
	}
	srv.mu.Unlock()
	if n2 != 0 {
		t.Fatalf("the relay adopted %d socket(s) while it was stopping: "+
			"a socket it cannot close is a socket that outlives the server", n2)
	}
}

// TestARelayThatIsRunningAcceptsConnections is the control: the guard
// must not refuse a relay that is NOT stopping.
//
// A guard that refuses everything would pass the test above.
func TestARelayThatIsRunningAcceptsConnections(t *testing.T) {
	addr := reserveTestAddr(t)

	cfg := DefaultConfig()
	cfg.Addr = addr
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	// ★ STOP THE RELAY WHEN THE TEST ENDS. (Round 102.)
	//
	// The relay was never stopped here, so its listener stayed open for
	// the rest of the process. With -count=100 that is a hundred
	// listeners, and reserveTestAddr hands out ports from the OS's
	// ephemeral pool by binding :0 and closing it — so once the pool
	// wraps, a test gets a port that one of the previous iterations
	// still holds, the new relay cannot bind, and the dial fails with
	// "connection refused".
	//
	// That reads like a product defect and is not one: it is a leaked
	// listener in a test, and it only shows up when the test is run
	// repeatedly — which is exactly how a flake is hunted.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	waitAccept(t, addr)

	// A real pairing connection, on a relay that is running.
	a := dial(t, fmt.Sprintf("ws://%s/ws", addr), "running", "A")
	t.Cleanup(func() { _ = a.Close() })

	// It must be tracked (the relay adopted it) and alive.
	deadline := time.Now().Add(3 * time.Second)
	for {
		srv.mu.Lock()
		n := len(srv.live)
		srv.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a running relay did not adopt the connection")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And a clean shutdown must still work, closing it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	<-errc

	_ = a.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := a.Recv(); err == nil {
		t.Fatal("the pairing socket is still delivering after Shutdown")
	}
}
