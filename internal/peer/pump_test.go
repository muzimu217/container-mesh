// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/secure"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// newPumpPair builds two pumps wired to each other with distinct keys, the way
// two peers would be after a handshake.
func newPumpPair(t *testing.T, cfg PumpConfig) (a, b *Pump, aConn, bConn *memPipe) {
	t.Helper()
	ac, bc := newMemPipePair()
	secA, secB := testSecurePair(t)
	a = NewPump(ac, secA, cfg)
	b = NewPump(bc, secB, cfg)
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})
	return a, b, ac, bc
}

// testSecurePair runs a real in-memory PAKE and returns the two resulting
// sessions. Using the production handshake rather than a synthetic key means
// the directional keys really are opposite, which is what the pump's ordering
// invariant depends on.
func testSecurePair(t *testing.T) (*secure.Session, *secure.Session) {
	t.Helper()
	const code = "pump-test-code"
	ha, err := secure.NewHandshake(code, secure.RoleA)
	if err != nil {
		t.Fatalf("A init: %v", err)
	}
	hb, err := secure.NewHandshake(code, secure.RoleB)
	if err != nil {
		t.Fatalf("B init: %v", err)
	}
	m1, done, err := ha.Step(nil)
	if err != nil || done {
		t.Fatalf("A step1: done=%v err=%v", done, err)
	}
	m2, done, err := hb.Step(m1)
	if err != nil || done {
		t.Fatalf("B step1: done=%v err=%v", done, err)
	}
	if _, done, err := ha.Step(m2); err != nil || !done {
		t.Fatalf("A step2: done=%v err=%v", done, err)
	}
	a, err := secure.NewSession(ha)
	if err != nil {
		t.Fatal(err)
	}
	b, err := secure.NewSession(hb)
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

// testLogger is a quiet logger for tests that need a non-nil one.
func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestPumpBootstrapMatchesLegacy is the equivalence test for the whole
// refactor: what the pump puts on the wire must be exactly one sealed frame,
// decryptable by the peer, with the counter advancing by exactly one.
//
// The pre-refactor code did `sec.SealFrame(f)` then `conn.Send(b)`; if the pump
// adds or reorders anything, the peer's OpenFrame fails here.
func TestPumpBootstrapMatchesLegacy(t *testing.T) {
	ac, _ := newMemPipePair()
	secA, secB := testSecurePair(t)
	p := NewPump(ac, secA, DefaultPumpConfig())
	defer p.Close()

	f := &wire.Frame{Type: wire.FrameMsg, Payload: []byte("hello")}
	if err := p.SendFrame(f); err != nil {
		t.Fatalf("SendFrame: %v", err)
	}
	if got := secA.SendCounter(); got != 1 {
		t.Fatalf("send counter = %d, want exactly 1", got)
	}

	// Exactly one transport message, and it opens to the original frame.
	var got []byte
	select {
	case got = <-ac.out:
	default:
		t.Fatal("SendFrame produced no transport message")
	}
	select {
	case extra := <-ac.out:
		t.Fatalf("SendFrame produced a second transport message (%d bytes)", len(extra))
	default:
	}
	opened, err := secB.OpenFrame(got)
	if err != nil {
		t.Fatalf("peer could not open the sealed frame: %v", err)
	}
	if opened.Type != f.Type || !bytes.Equal(opened.Payload, f.Payload) {
		t.Fatalf("opened %v %q, want %v %q", opened.Type, opened.Payload, f.Type, f.Payload)
	}
	if got := secB.RecvCounter(); got != 1 {
		t.Fatalf("recv counter = %d, want exactly 1", got)
	}
}

// TestPumpRoundTripBootstrap sends frames through a pump pair and reads them
// back, which is the behaviour every existing session test depends on.
func TestPumpRoundTripBootstrap(t *testing.T) {
	a, b, _, _ := newPumpPair(t, DefaultPumpConfig())

	want := &wire.Frame{Type: wire.FrameMsg, Payload: []byte("ping-pong")}
	if err := a.SendFrame(want); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := b.RecvFrame()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("got %v %q, want %v %q", got.Type, got.Payload, want.Type, want.Payload)
	}
}

// TestPumpOrderingUnderConcurrency is the core invariant of the whole design:
// with many goroutines writing, the receiver must see strictly increasing
// counters and never a decrypt failure. If two goroutines could seal out of
// order, this is what catches it.
func TestPumpOrderingUnderConcurrency(t *testing.T) {
	const (
		writers = 8
		perIter = 200
	)
	a, b, _, _ := newPumpPair(t, DefaultPumpConfig())

	var wg sync.WaitGroup
	errc := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perIter; i++ {
				payload := fmt.Appendf(nil, "w%d-%d", id, i)
				if err := a.SendFrame(&wire.Frame{Type: wire.FrameMsg, Payload: payload}); err != nil {
					errc <- err
					return
				}
			}
		}(w)
	}

	recvd := 0
	readDone := make(chan error, 1)
	go func() {
		for recvd < writers*perIter {
			if _, err := b.RecvFrame(); err != nil {
				readDone <- err
				return
			}
			recvd++
		}
		readDone <- nil
	}()

	wg.Wait()
	select {
	case err := <-errc:
		t.Fatalf("writer error: %v", err)
	case err := <-readDone:
		if err != nil {
			t.Fatalf("reader error after %d frames: %v", recvd, err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out after %d of %d frames", recvd, writers*perIter)
	}
	if recvd != writers*perIter {
		t.Fatalf("read %d frames, want %d", recvd, writers*perIter)
	}
}

// TestPumpBootstrapRefusesAfterMux makes the hand-off explicit: once the mux
// barrier has passed, the bootstrap entry points must refuse rather than race
// the pump's own reader.
func TestPumpBootstrapRefusesAfterMux(t *testing.T) {
	a, b, _, _ := newPumpPair(t, DefaultPumpConfig())
	startMuxPair(t, a, b)

	if err := a.SendFrame(&wire.Frame{Type: wire.FrameMsg}); !errors.Is(err, ErrPumpMuxed) {
		t.Fatalf("SendFrame after mux = %v, want ErrPumpMuxed", err)
	}
	if _, err := a.RecvFrame(); !errors.Is(err, ErrPumpMuxed) {
		t.Fatalf("RecvFrame after mux = %v, want ErrPumpMuxed", err)
	}
	if !a.Muxed() {
		t.Fatal("Muxed() = false after StartMux")
	}
}

// TestPumpMuxStreamRoundTrip proves the byte stream yamux will run on actually
// carries bytes in both directions, in order.
func TestPumpMuxStreamRoundTrip(t *testing.T) {
	a, b, _, _ := newPumpPair(t, DefaultPumpConfig())
	linkA, linkB := startMuxPair(t, a, b)

	// 64 KiB each way, larger than one record, so the pump has to split and
	// reassemble across record boundaries.
	payload := make([]byte, 64<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := linkA.Write(payload); err != nil {
			t.Errorf("linkA write: %v", err)
		}
	}()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(linkB, got); err != nil {
		t.Fatalf("linkB read: %v", err)
	}
	wg.Wait()
	if !bytes.Equal(got, payload) {
		t.Fatal("payload corrupted across the pump")
	}
}

// TestPumpBatchDoesNotStarveInteractive is the regression test for the Nagle
// trap the reviewer flagged: a single small write must reach the peer
// immediately, not wait for BatchedRecords neighbours that never arrive.
func TestPumpBatchDoesNotStarveInteractive(t *testing.T) {
	a, b, _, _ := newPumpPair(t, DefaultPumpConfig())
	linkA, linkB := startMuxPair(t, a, b)

	const keystroke = "x"
	start := time.Now()
	if _, err := linkA.Write([]byte(keystroke)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The peer must see the byte well before any batching window could expire.
	// There is no timer in the design, so this should be microseconds; 2 s is a
	// generous ceiling that still fails loudly if a blocking collect appears.
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, len(keystroke))
		_, err := io.ReadFull(linkB, buf)
		if err == nil && string(buf) != keystroke {
			err = fmt.Errorf("got %q, want %q", buf, keystroke)
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a single small write did not reach the peer: batching is starving interactive traffic")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("keystroke took %s", elapsed)
	}
}

// TestPumpBackpressureBlocksInsteadOfDropping verifies the bounded queue: with
// a transport that never accepts a write, the writer must block rather than
// grow without limit or drop bytes.
//
// memPipe cannot show this (its channel buffers 64 messages, so it absorbs the
// writes); a transport that parks forever is the honest setup.
func TestPumpBackpressureBlocksInsteadOfDropping(t *testing.T) {
	cfg := DefaultPumpConfig()
	cfg.BufBytes = 64 << 10
	cfg.RecordSize = 8 << 10

	secA, _ := testSecurePair(t)
	blocked := newBlockingConn()
	p := NewPump(blocked, secA, cfg)
	defer p.Close()

	// Start the loops without a barrier: this test is about the writer's
	// backpressure, not about the handshake.
	p.st.Store(pumpMuxed)
	p.startLoops()

	// 64 KiB queue + 8 KiB records = 8 records of room. Offer 256 KiB.
	payload := bytes.Repeat([]byte("A"), 32<<10)
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for i := 0; i < 8; i++ {
			if _, err := p.writeQ.Write(payload); err != nil {
				return
			}
		}
	}()

	select {
	case <-writeDone:
		t.Fatal("writer finished into a stalled transport: the queue is unbounded or dropping bytes")
	case <-time.After(300 * time.Millisecond):
		// Expected: blocked on backpressure.
	}

	if held := p.writeQ.Len(); held > cfg.BufBytes {
		t.Fatalf("queue holds %d bytes, budget is %d", held, cfg.BufBytes)
	}

	blocked.release()
	select {
	case <-writeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("writer never unblocked after the transport resumed")
	}
}

// blockingConn is a MessageConn whose Send parks until released. Recv blocks
// forever, which is fine: these tests never read.
type blockingConn struct {
	mu   sync.Mutex
	gate chan struct{}
	open bool
}

func newBlockingConn() *blockingConn { return &blockingConn{gate: make(chan struct{})} }

func (c *blockingConn) Send([]byte) error {
	c.mu.Lock()
	gate, open := c.gate, c.open
	c.mu.Unlock()
	if open {
		return nil
	}
	<-gate
	return nil
}

func (c *blockingConn) Recv() ([]byte, error) { select {} }

// release lets every parked and future Send proceed.
func (c *blockingConn) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.open {
		c.open = true
		close(c.gate)
	}
}

func (c *blockingConn) Close() error {
	c.release()
	return nil
}

// TestPumpCloseUnblocksWaiters makes sure Close releases both loops; closing
// only one would leave yamux's other loop parked forever.
func TestPumpCloseUnblocksWaiters(t *testing.T) {
	a, _, _, _ := newPumpPair(t, DefaultPumpConfig())
	// A single pump cannot cross the barrier alone (it waits for the peer), so
	// park the read on the queue directly: the property under test is that
	// Close wakes a blocked reader.
	link := &pumpLink{p: a}
	link.init()

	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := link.Read(buf)
		readErr <- err
	}()

	// Give the reader a moment to park, then close.
	time.Sleep(50 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("read returned nil error after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock a parked reader")
	}
	select {
	case <-a.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() not closed")
	}
}

// TestTruncatedRecordFailsToOpen is the evidence behind ErrPathLocked.
//
// A record is one AEAD unit: if the transport delivers only part of it, the
// remainder never arrives and OpenFrame fails. That is why a muxed session
// refuses a mid-stream transport switch instead of attempting a hot migration.
func TestTruncatedRecordFailsToOpen(t *testing.T) {
	secA, _ := testSecurePair(t)

	whole, err := secA.SealFrame(&wire.Frame{
		Type:    wire.FrameStreamData,
		Payload: bytes.Repeat([]byte("Z"), 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) < 8 {
		t.Fatalf("sealed record too short: %d", len(whole))
	}

	// Half a record is unrecoverable: the missing bytes are simply gone.
	//
	// memPipe is a crossed pair (a.out feeds b.in), so the truncated record has
	// to be injected from the peer endpoint, not from the pump's own.
	half := whole[:len(whole)/2]
	ac, peerSide := newMemPipePair()
	p := NewPump(ac, secA, DefaultPumpConfig())
	defer p.Close()

	if err := peerSide.Send(half); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RecvFrame(); err == nil {
		t.Fatal("OpenFrame accepted a truncated record; the AEAD should reject it")
	}
}

// TestUpgradeToDirectAllowedBeforeMux is the --p2p path: before the barrier a
// switch is exactly what a successful hole punch does.
func TestUpgradeToDirectAllowedBeforeMux(t *testing.T) {
	ac, peerSide := newMemPipePair()
	secA, secB := testSecurePair(t)
	_ = secB

	p := NewPump(newSwitchable(ac), secA, DefaultPumpConfig())
	defer p.Close()
	sess := &Session{
		cfg: Config{}, pump: p, frames: p, log: testLogger(),
		done: make(chan struct{}), outboundCh: make(chan *wire.Frame, 8),
	}

	// The replacement is a fresh pair; the peer side is what the pump will now
	// talk to.
	next, nextPeer := newMemPipePair()
	if err := sess.UpgradeToDirect(next); err != nil {
		t.Fatalf("switch before mux must be allowed: %v", err)
	}
	if !sess.OnDirect() {
		t.Fatal("OnDirect() = false after a successful switch")
	}

	// Prove the new path is the live one: a frame written now must arrive on
	// the replacement's peer endpoint, not the original.
	if err := sess.SendFrame(&wire.Frame{Type: wire.FrameMsg, Payload: []byte("via-direct")}); err != nil {
		t.Fatalf("SendFrame after switch: %v", err)
	}
	if len(peerSide.out) > 0 {
		t.Fatal("frame went to the old transport after the switch")
	}
	select {
	case <-nextPeer.in:
		// Arrived on the new path.
	default:
		t.Fatal("frame did not arrive on the replacement transport")
	}
}

// TestUpgradeToDirectRefusedWhenMuxed is the production guarantee: once the
// barrier has passed, the session refuses to swap the transport at all.
func TestUpgradeToDirectRefusedWhenMuxed(t *testing.T) {
	a, b, _, _ := newPumpPair(t, DefaultPumpConfig())
	sess := &Session{
		cfg: Config{}, pump: a, frames: a, log: testLogger(),
		done: make(chan struct{}), outboundCh: make(chan *wire.Frame, 8),
	}

	// Cross the barrier on both sides first: a switch is only meaningful while
	// both endpoints are still on the original transport.
	startMuxPair(t, a, b)

	third, _ := newMemPipePair()
	if err := sess.UpgradeToDirect(third); !errors.Is(err, ErrPathLocked) {
		t.Fatalf("UpgradeToDirect after mux = %v, want ErrPathLocked", err)
	}
}

// startMuxPair crosses the mux barrier on both pumps at once. Each side blocks
// until the other's FrameMuxStart arrives, so they must run concurrently.
func startMuxPair(t *testing.T, a, b *Pump) (io.ReadWriteCloser, io.ReadWriteCloser) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type res struct {
		link io.ReadWriteCloser
		err  error
	}
	cha := make(chan res, 1)
	chb := make(chan res, 1)
	go func() { l, err := a.StartMux(ctx); cha <- res{l, err} }()
	go func() { l, err := b.StartMux(ctx); chb <- res{l, err} }()

	ra, rb := <-cha, <-chb
	if ra.err != nil {
		t.Fatalf("a StartMux: %v", ra.err)
	}
	if rb.err != nil {
		t.Fatalf("b StartMux: %v", rb.err)
	}
	return ra.link, rb.link
}

// TestByteQueueWrapAround exercises the ring buffer across the wrap point,
// where an off-by-one in the two-copy logic would silently corrupt data.
func TestByteQueueWrapAround(t *testing.T) {
	q := newByteQueue(16)
	defer q.Close()

	if _, err := q.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if _, err := q.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "01234567" {
		t.Fatalf("got %q", buf)
	}
	// head is now 8; writing 10 bytes wraps the ring.
	if _, err := q.Write([]byte("abcdefghij")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 12) // "89" + "abcdefghij"
	for read := 0; read < len(got); {
		n, err := q.Read(got[read:])
		if err != nil {
			t.Fatalf("read after %d bytes: %v", read, err)
		}
		read += n
	}
	if string(got) != "89abcdefghij" {
		t.Fatalf("got %q, want %q", got, "89abcdefghij")
	}
}

// TestByteQueueTryGetNonBlocking is the primitive the batching relies on.
func TestByteQueueTryGetNonBlocking(t *testing.T) {
	q := newByteQueue(8)
	defer q.Close()

	buf := make([]byte, 4)
	if _, ok, err := q.TryGet(buf); ok || err != nil {
		t.Fatalf("empty TryGet = ok %v, err %v; want false, nil", ok, err)
	}
	if _, err := q.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	n, ok, err := q.TryGet(buf)
	if !ok || err != nil || n != 4 || string(buf) != "abcd" {
		t.Fatalf("TryGet = %d, %v, %v (%q)", n, ok, err, buf)
	}
	if _, ok, _ := q.TryGet(buf); ok {
		t.Fatal("TryGet drained twice")
	}
}

// TestByteQueueCloseWakesReader proves Close releases a parked reader, which is
// what lets Pump.Close tear down both loops.
func TestByteQueueCloseWakesReader(t *testing.T) {
	q := newByteQueue(8)
	errCh := make(chan error, 1)
	go func() {
		_, err := q.Read(make([]byte, 4))
		errCh <- err
	}()
	time.Sleep(20 * time.Millisecond)
	q.CloseWithError(errors.New("boom"))
	select {
	case err := <-errCh:
		if err == nil || err.Error() != "boom" {
			t.Fatalf("read returned %v, want boom", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseWithError did not wake the reader")
	}
}

// TestPumpMuxedDropsNonStreamFrames proves a stray bootstrap frame after the
// barrier is dropped, not fatal: a ping that was in flight when the peer hit
// its barrier must not kill the session.
func TestPumpMuxedDropsNonStreamFrames(t *testing.T) {
	a, b, _, _ := newPumpPair(t, DefaultPumpConfig())
	linkA, linkB := startMuxPair(t, a, b)

	// Reach into the peer's session to inject a ping on the wire. The pump's
	// reader must drop it and keep serving the stream.
	ping := &wire.Frame{Type: wire.FramePing}
	sealed, err := b.sec.SealFrame(ping)
	if err != nil {
		t.Fatalf("seal ping: %v", err)
	}
	// b's transport writes toward a; inject directly on b's outbound side.
	if err := b.conn.Send(sealed); err != nil {
		t.Fatalf("inject: %v", err)
	}

	// The stream must still carry data after the stray frame.
	payload := []byte("still-alive")
	if _, err := linkB.Write(payload); err != nil {
		t.Fatalf("write after stray frame: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(linkA, got); err != nil {
		t.Fatalf("read after stray frame: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

// TestPumpRecvCounterAdvancesAcrossMux keeps FramesSinceStart meaningful after
// the barrier: the CLI uses it to tell "paired with a dead peer" apart from a
// normal disconnect, and it must keep counting in mux mode.
func TestPumpRecvCounterAdvancesAcrossMux(t *testing.T) {
	a, b, _, _ := newPumpPair(t, DefaultPumpConfig())
	linkA, linkB := startMuxPair(t, a, b)

	before := b.RecvCounter()
	if _, err := linkA.Write([]byte("count-me")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 8)
	if _, err := io.ReadFull(linkB, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if after := b.RecvCounter(); after <= before {
		t.Fatalf("recv counter did not advance across mux: %d -> %d", before, after)
	}
}
