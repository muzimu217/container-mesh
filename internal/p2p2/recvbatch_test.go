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

// Tests for the batched receive path. They exist for the same reason as the
// batched-send tests: receive batching changes how datagrams come out of the
// kernel, and the property it could break — order and completeness — is the
// property the whole package is built around.

// TestRecvBatchPathIsActive proves the tests below actually exercise the batched
// receive path on this platform. Without it every receive-batch test would pass
// on a machine that silently fell back to per-datagram ReadFrom.
func TestRecvBatchPathIsActive(t *testing.T) {
	if !recvBatchingSupported {
		t.Skip("platform has no vectored datagram receive")
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

	st := b.Stats()
	if !st.RecvBatchEnabled {
		t.Fatal("receive batching is not enabled on a platform that supports it")
	}
	if st.RecvBatchCalls == 0 {
		t.Fatal("no batched receive syscalls were made")
	}
	if st.RecvBatchPackets == 0 {
		t.Fatal("the batched receive path carried no datagrams")
	}
	if st.RecvBatchPackets != st.Recv {
		t.Fatalf("the batched receive path carried %d datagrams but the connection counted %d",
			st.RecvBatchPackets, st.Recv)
	}
	if st.RecvBatchSize != DefaultConfig().RecvBatchSize {
		t.Fatalf("receive batch size is %d, want the configured %d", st.RecvBatchSize, DefaultConfig().RecvBatchSize)
	}
	avg := float64(st.RecvBatchPackets) / float64(st.RecvBatchCalls)
	t.Logf("512 KiB: %d datagrams in %d receive calls (%.1f per call, size=%d)",
		st.RecvBatchPackets, st.RecvBatchCalls, avg, st.RecvBatchSize)

	// The point of batching: fewer syscalls than datagrams. A 512 KiB message is
	// 464 fragments. Unlike the send side the ratio is not guaranteed to be near
	// the batch size — the sender paces its datagrams, so the receive queue is
	// usually shallow — but it must still be strictly better than one per
	// datagram.
	if st.RecvBatchCalls >= st.RecvBatchPackets {
		t.Fatalf("receive batching made %d calls for %d datagrams: no amortisation",
			st.RecvBatchCalls, st.RecvBatchPackets)
	}
}

// TestRecvBatchPreservesOrderAndContent is the core receive-batch test: many
// messages of several sizes, with receive batching on, must arrive complete,
// byte-exact and in send order.
//
// The sizes straddle the batch boundary on both sides — smaller than one batch,
// exactly one batch, one batch plus one fragment, many batches — so a batching
// bug that truncates or reorders at a boundary shows up as one specific size
// failing rather than as a vague corruption.
func TestRecvBatchPreservesOrderAndContent(t *testing.T) {
	if !recvBatchingSupported {
		t.Skip("platform has no vectored datagram receive")
	}
	cfg := DefaultConfig()
	cfg.RecvBatchSize = 8 // small on purpose: more receive boundaries per message
	cfg.BatchSize = 8

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
			if string(got[i][:len(want)]) != want {
				t.Fatalf("size %d: message %d carries marker %q, want %q — messages were reordered",
					size, i, got[i][:len(want)], want)
			}
		}
	}

	st := b.Stats()
	if st.RecvBatchCalls == 0 {
		t.Fatal("the batched receive path was never used")
	}
}

// TestRecvBatchMatchesPerDatagram checks that the two receive paths deliver
// identical bytes for the same message. This is the equivalence claim in its
// most direct form: if the batched path decoded a datagram differently, copied
// it into the wrong slot, or dropped the tail of a short batch, the two
// deliveries would differ here.
func TestRecvBatchMatchesPerDatagram(t *testing.T) {
	if !recvBatchingSupported {
		t.Skip("platform has no vectored datagram receive")
	}
	const size = 300*MaxPayload + 123

	msg := make([]byte, size)
	if _, err := rand.Read(msg); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(msg)

	transfer := func(batched bool) []byte {
		cfg := DefaultConfig()
		cfg.DisableRecvBatching = !batched
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
			st := b.Stats()
			if st.RecvBatchEnabled != batched {
				t.Fatalf("batched=%v: receive batching reported enabled=%v", batched, st.RecvBatchEnabled)
			}
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

// TestRecvBatchShortReturn checks the short-return contract directly, without
// the kernel: a reader that reports fewer datagrams than the batch could hold
// must be consumed exactly as reported, and the next call must continue from
// there. A short return is the normal case on a paced path, not an error.
//
// This is the receive-side analogue of TestBatchQueueResubmitsTailAfterShortReturn:
// the send side re-submits the tail, the receive side simply stops at n.
func TestRecvBatchShortReturn(t *testing.T) {
	const connID = 0x1122334455667788
	remote := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}

	// Three batches: 3 datagrams, then 1, then 2. The reader owns the buffers,
	// so the test fills them the way a kernel would.
	batches := []int{3, 1, 2}
	fr := &fakeBatchReader{
		max:   8,
		conn:  connID,
		sizes: batches,
	}
	c := &Conn{
		connID:      connID,
		remote:      remote,
		cfg:         DefaultConfig(),
		stats:       &statsCounters{},
		inbox:       make(chan []byte, 16),
		done:        make(chan struct{}),
		recvBatcher: fr,
		recvStats:   &recvBatchCounters{max: fr.maxBatch()},
	}
	c.rcv = newReceiver(connID, 512, 64<<20, c.deliver)
	c.snd = newSender(c.cfg, c.stats, c.closeWith)
	c.lastRecv.Store(time.Now().UnixNano())

	// Drive the read loop by hand rather than in a goroutine, so the assertion
	// runs after every call and no sleep is needed.
	var src srcTracker
	var seen []int
	for round := range batches {
		want := batches[round]
		got, err := c.recvBatcher.readBatch(c.recvBatcher.maxBatch())
		if err != nil {
			t.Fatalf("round %d: readBatch: %v", round, err)
		}
		if got != want {
			t.Fatalf("round %d: got %d datagrams, want the short return of %d", round, got, want)
		}
		seen = append(seen, got)
		bufs, lens := c.recvBatcher.buffers()
		cur := c.remoteAddr()
		for i := 0; i < got; i++ {
			var addr net.Addr
			if a, ok := src.changed(c.recvBatcher.rawSource(i), cur); ok {
				addr = a
				cur = a
			}
			if !c.handleDatagram(bufs[i][:lens[i]], addr) {
				t.Fatalf("round %d: connection ended", round)
			}
		}
	}

	// Every datagram in the three short batches must have been accounted for.
	st := c.Stats()
	if st.Recv != 6 {
		t.Fatalf("receive counter is %d after 3+1+2 datagrams, want 6", st.Recv)
	}
	if st.RecvBatchCalls != 0 {
		t.Fatalf("the hand-driven loop should not touch the counters, got %d calls", st.RecvBatchCalls)
	}
	t.Logf("short returns consumed as reported: %v", seen)
}

// TestRecvBatchShortReturnDeliversMessage is the same contract end to end: a
// message whose datagrams arrive in shallow groups (which is what a paced
// sender produces, and what makes the kernel return short batches) must still
// be delivered complete and in order.
func TestRecvBatchShortReturnDeliversMessage(t *testing.T) {
	if !recvBatchingSupported {
		t.Skip("platform has no vectored datagram receive")
	}
	const connID = 0x1122334455667788
	const frags = 24

	rcv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	snd, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.RecvBatchSize = 32 // far deeper than the groups below
	c := NewConn(rcv, snd.LocalAddr(), connID, cfg)
	defer c.Close()
	defer snd.Close()

	msg := make([]byte, 3*MaxPayload+11)
	if _, err := rand.Read(msg); err != nil {
		t.Fatal(err)
	}
	// frags above is larger than this message needs; rebuild the payload list
	// from the real fragment count so the headers stay self-consistent.
	n := (len(msg) + MaxPayload - 1) / MaxPayload
	pkts := make([][]byte, n)
	for i := 0; i < n; i++ {
		lo, hi := i*MaxPayload, (i+1)*MaxPayload
		if hi > len(msg) {
			hi = len(msg)
		}
		pkts[i] = makeDataPacket(connID, uint32(i), 0, uint32(i), uint32(n), uint32(len(msg)), msg[lo:hi])
	}

	done := make(chan []byte, 1)
	go func() {
		m, err := c.Recv()
		if err == nil {
			done <- m
		}
		close(done)
	}()

	// Send in groups of 1-3 with a pause between them, so every receive call
	// returns a short batch.
	for i := 0; i < n; {
		group := 1 + i%3
		for k := 0; k < group && i < n; k, i = k+1, i+1 {
			if _, err := snd.WriteToUDP(pkts[i], rcv.LocalAddr().(*net.UDPAddr)); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(200 * time.Microsecond)
	}

	select {
	case got := <-done:
		if !bytes.Equal(got, msg) {
			t.Fatalf("message differs (len %d, want %d)", len(got), len(msg))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("message assembled from short batches never arrived")
	}

	st := c.Stats()
	if !st.RecvBatchEnabled {
		t.Fatal("receive batching is not enabled")
	}
	if st.RecvBatchCalls < 2 {
		t.Fatalf("expected several short receive calls, got %d for %d datagrams", st.RecvBatchCalls, n)
	}
	t.Logf("%d datagrams in %d receive calls (mean %.1f)", st.RecvBatchPackets, st.RecvBatchCalls,
		float64(st.RecvBatchPackets)/float64(st.RecvBatchCalls))
}

// TestRecvBatchSurvivesLoss checks ordering and completeness with loss, where a
// short batch, a retransmission and a partially acknowledged message all
// interact.
func TestRecvBatchSurvivesLoss(t *testing.T) {
	if !recvBatchingSupported {
		t.Skip("platform has no vectored datagram receive")
	}
	cfg := DefaultConfig()
	cfg.RecvBatchSize = 16
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

// TestRecvBatchFallsBackWhenDisabled checks the fallback contract: with receive
// batching off, the transfer must be correct and must report that the batched
// path was unused. Send batching stays on, so this isolates the receive switch.
func TestRecvBatchFallsBackWhenDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DisableRecvBatching = true
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
		t.Fatal("payload corrupted on the per-datagram receive path")
	}
	st := b.Stats()
	if st.RecvBatchEnabled || st.RecvBatchCalls != 0 || st.RecvBatchPackets != 0 {
		t.Fatalf("receive batching reported enabled=%v calls=%d packets=%d with DisableRecvBatching set",
			st.RecvBatchEnabled, st.RecvBatchCalls, st.RecvBatchPackets)
	}
	// The send side must be unaffected by the receive switch.
	if !st.BatchEnabled {
		t.Fatal("DisableRecvBatching also disabled send batching")
	}
}

// TestRecvBatchSizeIsConfigurable checks that RecvBatchSize reaches the reader
// and is capped by the platform, and that a zero value falls back to the
// default rather than disabling the path.
func TestRecvBatchSizeIsConfigurable(t *testing.T) {
	if !recvBatchingSupported {
		t.Skip("platform has no vectored datagram receive")
	}
	for _, want := range []int{1, 4, 64} {
		cfg := DefaultConfig()
		cfg.RecvBatchSize = want
		a, b := pair(t, cfg, 0)
		msg := make([]byte, 16*MaxPayload)
		done := make(chan []byte, 1)
		go func() {
			m, err := b.Recv()
			if err == nil {
				done <- m
			}
			close(done)
		}()
		if err := a.Send(msg); err != nil {
			t.Fatalf("size=%d: send: %v", want, err)
		}
		<-done
		if got := b.Stats().RecvBatchSize; got != want {
			t.Fatalf("configured %d, reader reports %d", want, got)
		}
		_ = a.Close()
		_ = b.Close()
	}

	// Zero means the default, not "off".
	cfg := DefaultConfig()
	cfg.RecvBatchSize = 0
	if got := cfg; got.RecvBatchSize != 0 {
		t.Fatal("test setup: expected an unset RecvBatchSize")
	}
	cfg.fill()
	if cfg.RecvBatchSize != DefaultBatchSize {
		t.Fatalf("fill() left RecvBatchSize at %d, want the default %d", cfg.RecvBatchSize, DefaultBatchSize)
	}
}

// fakeBatchReader is a batchReader that hands back pre-recorded batch sizes, so
// the short-return contract can be tested directly rather than only through
// whatever the kernel happens to return.
type fakeBatchReader struct {
	max   int
	conn  uint64
	sizes []int

	mu   sync.Mutex
	call int
	bufs [][]byte
	lens []int
}

func (f *fakeBatchReader) maxBatch() int { return f.max }

func (f *fakeBatchReader) buffers() ([][]byte, []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bufs == nil {
		f.bufs = make([][]byte, f.max)
		for i := range f.bufs {
			f.bufs[i] = make([]byte, MaxDatagram)
		}
		f.lens = make([]int, f.max)
	}
	return f.bufs, f.lens
}

// rawSource reports the same source for every datagram: this fake never moves
// the peer, and the read loop only calls it when the connection has a remote.
func (f *fakeBatchReader) rawSource(int) []byte { return nil }

func (f *fakeBatchReader) readBatch(n int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.call >= len(f.sizes) {
		return 0, errors.New("fakeBatchReader: exhausted")
	}
	got := f.sizes[f.call]
	f.call++
	if got > n {
		got = n
	}
	bufs, lens := f.buffersLocked()
	// A complete, valid message per batch: the point of this fake is the batch
	// boundary, not the payload, so each datagram is a one-fragment message.
	for i := 0; i < got; i++ {
		msgID := uint32(f.call*100 + i)
		p := makeDataPacket(f.conn, uint32(f.call*100+i), msgID, 0, 1, 4, []byte("abcd"))
		copy(bufs[i], p)
		lens[i] = len(p)
	}
	return got, nil
}

func (f *fakeBatchReader) buffersLocked() ([][]byte, []int) {
	if f.bufs == nil {
		f.bufs = make([][]byte, f.max)
		for i := range f.bufs {
			f.bufs[i] = make([]byte, MaxDatagram)
		}
		f.lens = make([]int, f.max)
	}
	return f.bufs, f.lens
}
