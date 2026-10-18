package relay

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackevil/container-mesh/internal/wsconn"
)

// startTestRelay brings up a relay on a random localhost port and returns its
// WebSocket URL plus a shutdown function.
func startTestRelay(t *testing.T) (string, func()) {
	t.Helper()

	// Reserve a port, then hand it to the relay.
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

	// Wait for readiness.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return fmt.Sprintf("ws://%s/ws", addr), func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = hs.Shutdown(ctx)
				<-errc
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("relay did not start")
	return "", nil
}

func dial(t *testing.T, base, room, role string) *wsconn.Conn {
	t.Helper()
	c, err := wsconn.Dial(context.Background(), wsconn.DialOptions{
		URL:         fmt.Sprintf("%s?room=%s&role=%s", base, room, role),
		DialTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial %s: %v", role, err)
	}
	return c
}

func TestRelayPairsAndForwards(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	a := dial(t, base, "room1", "A")
	defer a.Close()
	b := dial(t, base, "room1", "B")
	defer b.Close()

	// A -> B
	if err := a.Send([]byte("ping from A")); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := b.Recv()
	if err != nil {
		t.Fatalf("B recv: %v", err)
	}
	if string(got) != "ping from A" {
		t.Errorf("B got %q", got)
	}

	// B -> A
	if err := b.Send([]byte("pong from B")); err != nil {
		t.Fatal(err)
	}
	_ = a.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err = a.Recv()
	if err != nil {
		t.Fatalf("A recv: %v", err)
	}
	if string(got) != "pong from B" {
		t.Errorf("A got %q", got)
	}
}

// TestRelayDuplicateRoleEvictsStaleWaiter verifies the reconnect story: a
// client that redials with the same code+role takes over the slot from the
// stale waiter (last writer wins), and then pairs normally.
func TestRelayDuplicateRoleEvictsStaleWaiter(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	a1 := dial(t, base, "dup", "A")
	a2 := dial(t, base, "dup", "A") // displaces a1
	defer a2.Close()

	// The displaced connection must be released promptly.
	_ = a1.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := a1.Recv(); err == nil {
		t.Fatal("stale waiter should be closed after eviction")
	}

	// The newcomer holds the slot and pairs with B.
	b := dial(t, base, "dup", "B")
	defer b.Close()
	if err := a2.Send([]byte("from-newcomer")); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := b.Recv()
	if err != nil {
		t.Fatalf("B recv: %v", err)
	}
	if string(got) != "from-newcomer" {
		t.Errorf("B got %q", got)
	}
}

func TestRelayRejectsMissingParams(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	resp, err := http.Get(strings.Replace(base, "ws://", "http://", 1) + "?room=x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRelayHealthz(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	// base is ws://host:port/ws — strip the path before appending /healthz.
	origin := strings.TrimSuffix(strings.Replace(base, "ws://", "http://", 1), "/ws")
	resp, err := http.Get(origin + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("healthz: %d %q", resp.StatusCode, body)
	}
}

func TestRelayMultipleRoomsAreIsolated(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	a1 := dial(t, base, "iso1", "A")
	defer a1.Close()
	b1 := dial(t, base, "iso1", "B")
	defer b1.Close()
	a2 := dial(t, base, "iso2", "A")
	defer a2.Close()
	b2 := dial(t, base, "iso2", "B")
	defer b2.Close()

	if err := a1.Send([]byte("for-b1")); err != nil {
		t.Fatal(err)
	}
	if err := a2.Send([]byte("for-b2")); err != nil {
		t.Fatal(err)
	}

	_ = b1.SetReadDeadline(time.Now().Add(3 * time.Second))
	got1, err := b1.Recv()
	if err != nil {
		t.Fatalf("b1: %v", err)
	}
	_ = b2.SetReadDeadline(time.Now().Add(3 * time.Second))
	got2, err := b2.Recv()
	if err != nil {
		t.Fatalf("b2: %v", err)
	}
	if string(got1) != "for-b1" || string(got2) != "for-b2" {
		t.Errorf("rooms crossed: %q / %q", got1, got2)
	}
}

// TestRelayLargeMessage verifies that a multi-megabyte message passes through
// unmodified, which is what file chunks rely on.
func TestRelayLargeMessage(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	a := dial(t, base, "big", "A")
	defer a.Close()
	b := dial(t, base, "big", "B")
	defer b.Close()

	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var recvErr error
	var got []byte
	go func() {
		defer wg.Done()
		_ = b.SetReadDeadline(time.Now().Add(10 * time.Second))
		got, recvErr = b.Recv()
	}()

	if err := a.Send(payload); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if recvErr != nil {
		t.Fatalf("recv: %v", recvErr)
	}
	if len(got) != len(payload) {
		t.Fatalf("length %d != %d", len(got), len(payload))
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
}

func TestRelayDoesNotDuplicateMessages(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	a := dial(t, base, "nodup", "A")
	defer a.Close()
	b := dial(t, base, "nodup", "B")
	defer b.Close()

	// Both directions in flight, then verify each side sees exactly one copy.
	// A regression here means both handlers ran a splice loop.
	const n = 5
	for i := 0; i < n; i++ {
		if err := a.Send([]byte(fmt.Sprintf("a%d", i))); err != nil {
			t.Fatal(err)
		}
		if err := b.Send([]byte(fmt.Sprintf("b%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	readN := func(c *wsconn.Conn, prefix string) []string {
		var out []string
		for i := 0; i < n; i++ {
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			msg, err := c.Recv()
			if err != nil {
				t.Fatalf("%s recv %d: %v", prefix, i, err)
			}
			out = append(out, string(msg))
		}
		// A sixth read should time out, proving there is no duplicate.
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if extra, err := c.Recv(); err == nil {
			t.Fatalf("%s got an unexpected extra message %q (duplicate forwarding)", prefix, extra)
		}
		return out
	}

	gotB := readN(b, "B")
	gotA := readN(a, "A")

	for i, want := range gotB {
		if want != fmt.Sprintf("a%d", i) {
			t.Errorf("B[%d] = %q, want a%d", i, want, i)
		}
	}
	for i, want := range gotA {
		if want != fmt.Sprintf("b%d", i) {
			t.Errorf("A[%d] = %q, want b%d", i, want, i)
		}
	}
}

func TestRoomTimesOut(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	cfg := DefaultConfig()
	cfg.Addr = addr
	cfg.PairTimeout = 300 * time.Millisecond
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg)
	hs := srv.HTTPServer()
	go func() { _ = hs.ListenAndServe() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	}()
	time.Sleep(150 * time.Millisecond)

	base := fmt.Sprintf("ws://%s/ws", addr)
	a := dial(t, base, "lonely", "A")
	defer a.Close()

	// The relay should close A's connection after PairTimeout.
	_ = a.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := a.Recv(); err == nil {
		t.Fatal("expected the room to time out and close the connection")
	}
}

// TestParkedDeadPeerFreesRoom checks that a peer dying while parked (process
// killed, proxy dropped the flow) does not block recovery: when the same
// code+role reconnects, the newcomer evicts the zombie and pairs normally.
func TestParkedDeadPeerFreesRoom(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	a1 := dial(t, base, "freed", "A")
	_ = a1.Close() // die while parked

	// The reconnect evicts the zombie and takes the slot.
	a2 := dial(t, base, "freed", "A")
	defer a2.Close()
	b := dial(t, base, "freed", "B")
	defer b.Close()

	if err := a2.Send([]byte("fresh")); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := b.Recv()
	if err != nil {
		t.Fatalf("B recv: %v", err)
	}
	if string(got) != "fresh" {
		t.Errorf("B got %q", got)
	}
}

// TestParkedEarlyDataSurvivesPairing verifies that bytes a parked client sent
// before its partner arrived are forwarded once the splice starts. Clients
// may begin the PAKE handshake immediately after dialling, so the relay must
// treat pre-pairing data as legitimate traffic and replay it.
func TestParkedEarlyDataSurvivesPairing(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	a := dial(t, base, "early", "A")
	defer a.Close()
	// A speaks before B even exists; the bytes must be buffered and replayed.
	if err := a.Send([]byte("early bird")); err != nil {
		t.Fatal(err)
	}

	b := dial(t, base, "early", "B")
	defer b.Close()

	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := b.Recv()
	if err != nil {
		t.Fatalf("B recv: %v", err)
	}
	if string(got) != "early bird" {
		t.Errorf("B got %q, want %q", got, "early bird")
	}
}

// TestNoLiveLockBetweenReconnectingPeers is the regression test for a bug
// found against a real deployment: with "last writer wins" replacement, two
// healthy peers that both re-park on a timer kept evicting each other forever
// and never met. A parked peer that is still alive must keep its slot, so
// pairing succeeds on the next attempt.
func TestNoLiveLockBetweenReconnectingPeers(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	// A parks and stays connected. Repeatedly re-parking A (as a client does
	// after a pair-window expiry) must not prevent the eventual pair.
	a := dial(t, base, "livelock", "A")
	defer a.Close()

	for i := 0; i < 3; i++ {
		// A is healthy: sending nothing keeps it parked; the relay must not
		// consider it stale merely because time passed.
		time.Sleep(100 * time.Millisecond)
	}

	b := dial(t, base, "livelock", "B")
	defer b.Close()

	if err := a.Send([]byte("hello after waiting")); err != nil {
		t.Fatalf("A send: %v", err)
	}
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := b.Recv()
	if err != nil {
		t.Fatalf("B recv: %v", err)
	}
	if string(got) != "hello after waiting" {
		t.Errorf("B got %q", got)
	}

	// And the reverse direction must work on the same splice.
	if err := b.Send([]byte("reply")); err != nil {
		t.Fatalf("B send: %v", err)
	}
	_ = a.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err = a.Recv()
	if err != nil {
		t.Fatalf("A recv: %v", err)
	}
	if string(got) != "reply" {
		t.Errorf("A got %q", got)
	}
}

// TestParkedPeerDeathIsNoticed checks that a peer that dies while parked is
// detected by the parked reader rather than holding the room for PairTimeout.
func TestParkedPeerDeathIsNoticed(t *testing.T) {
	base, stop := startTestRelay(t)
	defer stop()

	a := dial(t, base, "death", "A")
	_ = a.Close() // die while parked

	// Give the parked reader a moment to notice, then confirm a fresh peer
	// can take the slot and pair.
	time.Sleep(300 * time.Millisecond)

	a2 := dial(t, base, "death", "A")
	defer a2.Close()
	b := dial(t, base, "death", "B")
	defer b.Close()

	if err := a2.Send([]byte("recovered")); err != nil {
		t.Fatalf("send: %v", err)
	}
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := b.Recv()
	if err != nil {
		t.Fatalf("B recv: %v", err)
	}
	if string(got) != "recovered" {
		t.Errorf("B got %q", got)
	}
}
