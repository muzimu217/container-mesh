// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// Tests for the batched send path. They exist because batching is the one
// change in this package that touches how datagrams reach the kernel, and the
// property it could break — order and completeness — is the property the whole
// package is built around.

// TestBatchPathIsActive proves the tests below are actually exercising the
// batch path on this platform. Without it every batch test would pass on a
// machine that silently fell back to per-datagram writes.
func TestBatchPathIsActive(t *testing.T) {
	if !batchingSupported {
		t.Skip("platform has no vectored datagram send")
	}
	const size = 512 << 10
	a, b := pair(t, DefaultConfig(), 0)
	msg := make([]byte, size)
	if _, err := rand.Read(msg); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(msg)

	done := make(chan []byte, 1)
	go func() {
		m, err := b.Recv()
		if err == nil {
			done <- m
		}
		close(done)
	}()
	if err := a.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := <-done
	if sha256.Sum256(got) != want {
		t.Fatal("payload corrupted")
	}

	st := a.Stats()
	if !st.BatchEnabled {
		t.Fatal("batching is not enabled on a platform that supports it")
	}
	if st.BatchCalls == 0 {
		t.Fatal("no batch syscalls were made")
	}
	if st.BatchPackets == 0 {
		t.Fatal("the batch path carried no datagrams")
	}
	// The point of batching: fewer syscalls than datagrams. A 512 KiB message is
	// 464 fragments, so at the default size this must be a small fraction.
	if st.BatchCalls >= st.Sent {
		t.Fatalf("batch calls %d did not amortise the %d datagrams sent", st.BatchCalls, st.Sent)
	}
	avg := float64(st.BatchPackets) / float64(st.BatchCalls)
	t.Logf("512 KiB: %d datagrams in %d batch calls (%.1f per call, size=%d)",
		st.BatchPackets, st.BatchCalls, avg, st.BatchSize)
}

// TestBatchPreservesOrderAndContent is the core batch-path test: many messages
// of several sizes, sent with batching on, must arrive complete, byte-exact and
// in send order.
//
// The sizes are chosen to straddle the batch boundary: smaller than one batch,
// exactly one batch, one batch plus one fragment, and many batches. A batching
// bug that reorders or truncates at a flush point shows up as a specific size
// failing rather than as a vague corruption.
func TestBatchPreservesOrderAndContent(t *testing.T) {
	if !batchingSupported {
		t.Skip("platform has no vectored datagram send")
	}
	cfg := DefaultConfig()
	cfg.BatchSize = 8 // small on purpose: more flush points per message

	// The marker is 11 bytes, so the smallest size below still holds it.
	sizes := []int{
		16,                // one fragment, not even a full payload
		MaxPayload,        // exactly one fragment
		MaxPayload + 1,    // two fragments
		7 * MaxPayload,    // just under one batch
		8 * MaxPayload,    // exactly one batch
		9 * MaxPayload,    // one batch plus one fragment
		33*MaxPayload + 7, // several batches with a ragged tail
	}

	a, b := pair(t, cfg, 0)
	const count = 5
	for _, size := range sizes {
		msgs := make([][]byte, count)
		for i := range msgs {
			msgs[i] = make([]byte, size)
			if _, err := rand.Read(msgs[i]); err != nil {
				t.Fatal(err)
			}
			// A per-message marker makes a cross-message mix-up visible in the
			// failure output instead of only as a hash mismatch.
			copy(msgs[i], fmt.Sprintf("m%02d-s%06d", i, size))
		}

		// Send and receive concurrently: Send blocks until every fragment is
		// acknowledged, so sending on this goroutine before receiving would
		// deadlock on the window.
		sendErr := make(chan error, 1)
		go func() {
			for i := 0; i < count; i++ {
				if err := a.Send(msgs[i]); err != nil {
					sendErr <- fmt.Errorf("size %d msg %d: send: %w", size, i, err)
					return
				}
			}
			sendErr <- nil
		}()
		got := recvN(t, b, count, 60*time.Second)
		if err := <-sendErr; err != nil {
			t.Fatal(err)
		}

		for i := range got {
			if len(got[i]) != size {
				t.Fatalf("size %d: message %d arrived with %d bytes, want %d", size, i, len(got[i]), size)
			}
			if !bytes.Equal(got[i], msgs[i]) {
				t.Fatalf("size %d: message %d differs from what was sent (prefix %q, want %q)",
					size, i, desc(got[i]), desc(msgs[i]))
			}
			want := fmt.Sprintf("m%02d-s%06d", i, size)
			if len(got[i]) < len(want) {
				t.Fatalf("size %d: message %d is %d bytes, too short to hold its %d-byte marker",
					size, i, len(got[i]), len(want))
			}
			if string(got[i][:len(want)]) != want {
				t.Fatalf("size %d: message %d carries marker %q, want %q — messages were reordered",
					size, i, got[i][:len(want)], want)
			}
		}
	}

	st := a.Stats()
	if st.BatchCalls == 0 {
		t.Fatal("the batch path was never used")
	}
}

// TestBatchMatchesPerPacket checks that the two paths deliver identical bytes.
//
// This is the equivalence claim in its most direct form: the same message sent
// with batching and without it must arrive byte-identical. A batching bug that
// reorders fragments within a datagram, or that patches a stale ACK block,
// shows up here as a content mismatch rather than as a throughput oddity.
func TestBatchMatchesPerPacket(t *testing.T) {
	if !batchingSupported {
		t.Skip("platform has no vectored datagram send")
	}
	const size = 300*MaxPayload + 123

	msg := make([]byte, size)
	if _, err := rand.Read(msg); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(msg)

	transfer := func(batched bool) []byte {
		cfg := DefaultConfig()
		cfg.DisableBatching = !batched
		a, b := pair(t, cfg, 0)
		done := make(chan []byte, 1)
		go func() {
			m, err := b.Recv()
			if err == nil {
				done <- m
			}
			close(done)
		}()
		if err := a.Send(msg); err != nil {
			t.Fatalf("batched=%v: send: %v", batched, err)
		}
		select {
		case got := <-done:
			return got
		case <-time.After(120 * time.Second):
			t.Fatalf("batched=%v: message never arrived", batched)
			return nil
		}
	}

	batched := transfer(true)
	plain := transfer(false)

	if sha256.Sum256(batched) != want {
		t.Fatal("batched delivery is not byte-identical to what was sent")
	}
	if !bytes.Equal(batched, plain) {
		t.Fatal("batched and per-datagram delivery differ")
	}
}

// TestBatchSurvivesLoss checks ordering and completeness with loss, where a
// short batch, a retransmission and a partially acknowledged message all
// interact.
func TestBatchSurvivesLoss(t *testing.T) {
	if !batchingSupported {
		t.Skip("platform has no vectored datagram send")
	}
	cfg := DefaultConfig()
	cfg.BatchSize = 16
	const size = 128 << 10

	for _, drop := range []float64{0.05, 0.20} {
		a, b := pair(t, cfg, drop)
		msg := make([]byte, size)
		if _, err := rand.Read(msg); err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256(msg)

		done := make(chan []byte, 1)
		go func() {
			m, err := b.Recv()
			if err == nil {
				done <- m
			}
			close(done)
		}()
		if err := a.Send(msg); err != nil {
			t.Fatalf("drop=%.2f: send: %v", drop, err)
		}
		select {
		case got := <-done:
			if sha256.Sum256(got) != want {
				t.Fatalf("drop=%.2f: payload corrupted", drop)
			}
		case <-time.After(120 * time.Second):
			t.Fatalf("drop=%.2f: message never arrived", drop)
		}
		_ = a.Close()
		_ = b.Close()
	}
}

// TestBatchFallsBackWhenDisabled checks the fallback contract: with batching
// off, the transfer must be correct and must report that batching was unused.
func TestBatchFallsBackWhenDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DisableBatching = true
	const size = 64 << 10

	a, b := pair(t, cfg, 0)
	msg := make([]byte, size)
	if _, err := rand.Read(msg); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(msg)

	done := make(chan []byte, 1)
	go func() {
		m, err := b.Recv()
		if err == nil {
			done <- m
		}
		close(done)
	}()
	if err := a.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := <-done
	if sha256.Sum256(got) != want {
		t.Fatal("payload corrupted on the per-datagram path")
	}
	st := a.Stats()
	if st.BatchEnabled || st.BatchCalls != 0 {
		t.Fatalf("batching reported enabled=%v calls=%d with DisableBatching set", st.BatchEnabled, st.BatchCalls)
	}
}

// fakeBatchWriter is a batchWriter that records calls and can be told to
// short-return, so the re-submission logic is tested directly rather than only
// through whatever the kernel happens to do.
type fakeBatchWriter struct {
	mu sync.Mutex

	max int
	// shortAt makes the first call return this many datagrams regardless of the
	// batch size, which is the shape of a partial sendmmsg return.
	shortAt int
	calls   [][]int
	// err, when set, is returned instead of sending.
	err error
}

func (f *fakeBatchWriter) maxBatch() int { return f.max }

func (f *fakeBatchWriter) writeBatch(msgs [][]byte, addr net.Addr) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	n := len(msgs)
	if f.shortAt > 0 && len(f.calls) == 0 && f.shortAt < n {
		n = f.shortAt
	}
	f.calls = append(f.calls, make([]int, n))
	for i := 0; i < n; i++ {
		f.calls[len(f.calls)-1][i] = len(msgs[i])
	}
	return n, nil
}

// TestBatchQueueResubmitsTailAfterShortReturn pins the one behaviour that makes
// batching safe for ordering: a short return means "a prefix went out", so the
// tail is re-submitted and the concatenation of the calls is exactly the input
// sequence, in order.
func TestBatchQueueResubmitsTailAfterShortReturn(t *testing.T) {
	f := &fakeBatchWriter{max: 8, shortAt: 3}
	q := newBatchQueue(f)
	if !q.enabled() {
		t.Fatal("queue is not enabled")
	}

	msgs := make([][]byte, 8)
	for i := range msgs {
		msgs[i] = bytes.Repeat([]byte{byte(i)}, i+1)
	}
	if err := q.write(msgs, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}); err != nil {
		t.Fatalf("write: %v", err)
	}

	if len(f.calls) != 2 {
		t.Fatalf("expected 2 calls (a short one plus the tail), got %d", len(f.calls))
	}
	if len(f.calls[0]) != 3 || len(f.calls[1]) != 5 {
		t.Fatalf("calls carried %d and %d datagrams, want 3 and 5", len(f.calls[0]), len(f.calls[1]))
	}
	// The second call must be the tail, i.e. the datagram lengths continue the
	// ascending sequence instead of restarting.
	for i, n := range f.calls[1] {
		want := 3 + i + 1
		if n != want {
			t.Fatalf("tail call element %d has length %d, want %d: the tail was not the remainder", i, n, want)
		}
	}
}

// TestBatchQueueRejectsStalledWriter checks that a writer reporting no progress
// is treated as fatal rather than retried forever. A spin here would hang Send
// with no timeout, which is worse than failing the connection.
func TestBatchQueueRejectsStalledWriter(t *testing.T) {
	f := &fakeBatchWriter{max: 8}
	// A writer that always reports zero: the contract forbids it, and the queue
	// must not spin on it.
	stalled := &stalledWriter{max: 8}
	q := newBatchQueue(stalled)
	err := q.write([][]byte{[]byte("x")}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1})
	if !errors.Is(err, errBatchStalled) {
		t.Fatalf("got %v, want errBatchStalled", err)
	}
	_ = f
}

type stalledWriter struct{ max int }

func (s *stalledWriter) maxBatch() int { return s.max }
func (s *stalledWriter) writeBatch([][]byte, net.Addr) (int, error) {
	return 0, nil
}

// TestBatchQueuePropagatesError checks that a syscall error is surfaced, not
// swallowed: the caller fails the connection on it.
func TestBatchQueuePropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	f := &fakeBatchWriter{max: 8, err: sentinel}
	q := newBatchQueue(f)
	err := q.write([][]byte{[]byte("x")}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the writer's error", err)
	}
}

// TestBatchQueueHonoursPlatformLimit checks that the configured size never
// exceeds what the platform call accepts, and that a zero configured size still
// produces usable batches.
func TestBatchQueueHonoursPlatformLimit(t *testing.T) {
	f := &fakeBatchWriter{max: 4}
	q := newBatchQueue(&cappedWriter{w: f, max: 100})
	if got := q.limit(); got != 4 {
		t.Fatalf("limit is %d, want the platform's 4", got)
	}

	f2 := &fakeBatchWriter{max: 64}
	q2 := newBatchQueue(&cappedWriter{w: f2, max: 8})
	if got := q2.limit(); got != 8 {
		t.Fatalf("limit is %d, want the configured 8", got)
	}

	// A zero configured size must not disable batching: fill() replaces it, but a
	// cappedWriter built directly must still fall back to the platform limit.
	q3 := newBatchQueue(&cappedWriter{w: f2, max: 0})
	if got := q3.limit(); got != 64 {
		t.Fatalf("limit is %d with an unset configured size, want 64", got)
	}
}

// TestNilBatchQueueIsSafe checks the fallback shape: a nil queue reports
// disabled, a limit of one, and errors on write. The send path relies on the
// first two, and on the third only as a programming-error guard.
func TestNilBatchQueueIsSafe(t *testing.T) {
	var q *batchQueue
	if q.enabled() {
		t.Fatal("nil queue reports enabled")
	}
	if q.limit() != 1 {
		t.Fatalf("nil queue limit is %d, want 1", q.limit())
	}
	if err := q.write([][]byte{[]byte("x")}, nil); !errors.Is(err, errNoBatch) {
		t.Fatalf("nil queue write returned %v, want errNoBatch", err)
	}
	if s := q.snapshot(); s.enabled {
		t.Fatal("nil queue snapshot reports enabled")
	}
}

// TestBatchStatsAddUp checks the counters the KPI harness reads: every datagram
// carried by the batch path is counted once, and the totals are consistent with
// what the connection reports as sent.
func TestBatchStatsAddUp(t *testing.T) {
	if !batchingSupported {
		t.Skip("platform has no vectored datagram send")
	}
	cfg := DefaultConfig()
	cfg.BatchSize = 16
	const size = 200 << 10

	a, b := pair(t, cfg, 0)
	msg := make([]byte, size)
	if _, err := rand.Read(msg); err != nil {
		t.Fatal(err)
	}
	done := make(chan []byte, 1)
	go func() {
		m, err := b.Recv()
		if err == nil {
			done <- m
		}
		close(done)
	}()
	if err := a.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	<-done

	st := a.Stats()
	frags := (size + MaxPayload - 1) / MaxPayload
	if st.BatchPackets != uint64(frags) {
		t.Fatalf("batch path carried %d datagrams, want %d (one per fragment)", st.BatchPackets, frags)
	}
	if st.BatchCalls != uint64((frags+cfg.BatchSize-1)/cfg.BatchSize) {
		t.Fatalf("made %d batch calls for %d fragments at size %d, want %d",
			st.BatchCalls, frags, cfg.BatchSize, (frags+cfg.BatchSize-1)/cfg.BatchSize)
	}
	if st.Sent < st.BatchPackets {
		t.Fatalf("Sent %d is below the %d batched datagrams", st.Sent, st.BatchPackets)
	}
}

// TestBatchSmallMessagesNeverFillBatch checks the case a batch limit could
// break: a message smaller than one batch must still be sent promptly, because
// Send flushes the partial batch after the last fragment rather than waiting
// for it to fill.
func TestBatchSmallMessagesNeverFillBatch(t *testing.T) {
	if !batchingSupported {
		t.Skip("platform has no vectored datagram send")
	}
	cfg := DefaultConfig()
	cfg.BatchSize = 64 // far above any message here

	a, b := pair(t, cfg, 0)
	const n = 20
	const size = 3*MaxPayload + 5

	msgs := make([][]byte, n)
	for i := range msgs {
		msgs[i] = fillMessage(i, size)
	}
	sendErr := make(chan error, 1)
	go func() {
		for i := 0; i < n; i++ {
			if err := a.Send(msgs[i]); err != nil {
				sendErr <- fmt.Errorf("msg %d: send: %w", i, err)
				return
			}
		}
		sendErr <- nil
	}()
	got := recvN(t, b, n, 30*time.Second)
	if err := <-sendErr; err != nil {
		t.Fatal(err)
	}
	for i := range got {
		if !bytes.Equal(got[i], msgs[i]) {
			t.Fatalf("message %d differs (prefix %q, want %q)", i, desc(got[i]), desc(msgs[i]))
		}
	}
	// 20 messages x 4 fragments = 80 datagrams, all below the 64-datagram batch,
	// so this is one call per message.
	st := a.Stats()
	if st.BatchCalls == 0 {
		t.Fatal("the batch path was not used for small messages")
	}
	t.Logf("small messages: %d datagrams in %d calls", st.BatchPackets, st.BatchCalls)
}
