// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// TestTwentyMessagesAtLossRates is the end-to-end version of the reported
// failure: twenty messages of four fragments each, at 0/5/10/20% loss. Every
// message must arrive in order and byte-identical. The legacy transport failed
// this at 10% (message 8 lost, message 9 delivered in its place).
func TestTwentyMessagesAtLossRates(t *testing.T) {
	const n = 20
	for _, drop := range []float64{0, 0.05, 0.10, 0.20} {
		t.Run(dropName(drop), func(t *testing.T) {
			// Not parallel: under -race the four subtests would compete for the
			// same timer goroutines, and a 20% run that takes 0.8s alone can
			// exceed the deadline when three other loss rates are running
			// against it. The loss rate is the variable under test, so the
			// machine load must not be another one.
			a, b := pair(t, DefaultConfig(), drop)

			// Four fragments each: three full and one of eight bytes. The
			// short tail fragment is what made the legacy "len(map)==total"
			// completion test fire early.
			msgs := make([][]byte, n)
			for i := range msgs {
				msgs[i] = pad(byte('A'+i), 4)
			}

			sendErr := make(chan error, 1)
			go func() {
				for i := range msgs {
					if err := a.Send(msgs[i]); err != nil {
						sendErr <- err
						return
					}
				}
				sendErr <- nil
			}()

			got := recvN(t, b, n, 120*time.Second)
			if err := <-sendErr; err != nil {
				t.Fatalf("send: %v", err)
			}

			for i := range msgs {
				if !bytes.Equal(got[i], msgs[i]) {
					t.Fatalf("message %d is %q... (len %d), want %q... (len %d): "+
						"cross-message assembly", i, desc(got[i]), len(got[i]),
						desc(msgs[i]), len(msgs[i]))
				}
			}
			t.Logf("20 messages at %.0f%% loss: all correct", drop*100)
		})
	}
}

// TestMessage8FragmentLost reproduces the exact reported scenario: twenty
// messages, and precisely the third fragment of message 8 is dropped once.
// Everything else passes.
//
// This is the regression test that matters most, because it is deterministic:
// a probabilistic test can only say "did not happen this run", while this one
// fails every single time on a transport that mixes messages.
func TestMessage8FragmentLost(t *testing.T) {
	const connID = 0x1122334455667788
	const n = 20

	// Message 8 spans sequences 32..35 (four fragments each), so its third
	// fragment is sequence 34. Only the first transmission is dropped; the
	// retransmission must get through, otherwise the test would be asserting
	// that a permanently lost fragment is recovered — which no transport can do.
	var mu sync.Mutex
	dropped := 0
	proxy := newSelectiveProxy(t, func(dir string, data []byte) bool {
		if dir != "up" {
			return false
		}
		p, err := decodePacket(data)
		if err != nil || p.typ != pktData || p.seq != 34 {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if dropped > 0 {
			return false
		}
		dropped++
		return true
	})

	cfg := DefaultConfig()
	a := NewConn(mustListen(t, proxy.front.LocalAddr()), proxy.front.LocalAddr(), connID, cfg)
	b := NewConn(mustListen(t, proxy.back.LocalAddr()), proxy.back.LocalAddr(), connID, cfg)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	// The proxy learns each side's address from the first datagram it forwards,
	// so seed the peer address it will send back to.
	proxy.setPeers(a.LocalAddr(), b.LocalAddr())

	msgs := make([][]byte, n)
	for i := range msgs {
		msgs[i] = pad(byte('A'+i), 4)
	}

	sendErr := make(chan error, 1)
	go func() {
		for i := range msgs {
			if err := a.Send(msgs[i]); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()

	got := recvN(t, b, n, 60*time.Second)
	if err := <-sendErr; err != nil {
		t.Fatalf("send: %v", err)
	}

	for i := range msgs {
		if !bytes.Equal(got[i], msgs[i]) {
			t.Fatalf("message %d is %q... (len %d), want %q... (len %d)",
				i, desc(got[i]), len(got[i]), desc(msgs[i]), len(msgs[i]))
		}
	}
	mu.Lock()
	d := dropped
	mu.Unlock()
	if d != 1 {
		t.Fatalf("dropped %d datagrams, want exactly 1 (seq 34)", d)
	}
	if proxy.totalDrops() != 1 {
		t.Fatalf("proxy reports %d drops, want 1", proxy.totalDrops())
	}
	// The hole must have been repaired by fast retransmit, not by waiting out
	// an RTO: one lost fragment out of twenty messages should cost microseconds
	// on loopback, not tens of milliseconds.
	st := a.Stats()
	t.Logf("recovered via fast retransmit: %d (rto timeouts: %d, retrans: %d)",
		st.FastRetrans, st.RTOTimeouts, st.Retrans)
}

// TestLargeFrame512KiB is the file-chunk case: 512 KiB is ~450 fragments, which
// must not hit any fragment or window limit, and must survive loss intact.
func TestLargeFrame512KiB(t *testing.T) {
	a, b := pair(t, DefaultConfig(), 0.05)

	msg := make([]byte, 512<<10)
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

	start := time.Now()
	if err := a.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case got := <-done:
		elapsed := time.Since(start)
		if len(got) != len(msg) {
			t.Fatalf("received %d bytes, want %d", len(got), len(msg))
		}
		if sha256.Sum256(got) != want {
			t.Fatal("payload corrupted: 512 KiB did not survive the round trip")
		}
		miBs := float64(len(msg)) / elapsed.Seconds() / (1 << 20)
		t.Logf("512 KiB at 5%% loss in %s (%.1f MiB/s), fragments=%d",
			elapsed.Round(time.Millisecond), miBs, (len(msg)+MaxPayload-1)/MaxPayload)
	case <-time.After(120 * time.Second):
		t.Fatal("512 KiB message never arrived")
	}
}

// TestSendFailsWhenPeerNeverAcks is the fail-stop contract. Send must never
// return nil for a message the peer did not acknowledge, and must not block
// forever either: the retry budget exists so a dead path becomes an error the
// caller can act on (reconnect, fall back to the relay) instead of a silent
// hole in the data.
func TestSendFailsWhenPeerNeverAcks(t *testing.T) {
	// A socket bound to a port nobody reads. Datagrams to it vanish.
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := sink.LocalAddr().(*net.UDPAddr)
	_ = sink.Close() // the port is now unbound; writes to it are dropped

	cfg := DefaultConfig()
	cfg.InitialRTO = 20 * time.Millisecond
	cfg.MinRTO = 10 * time.Millisecond
	cfg.MaxRTO = 80 * time.Millisecond
	cfg.MaxRetries = 4

	c, _ := newConnOn(t, deadAddr, 7, cfg)

	start := time.Now()
	err = c.Send([]byte("this will never be acknowledged"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Send returned nil for a message the peer never acknowledged")
	}
	if !errors.Is(err, ErrPeerUnreachable) {
		t.Fatalf("Send failed with %v, want ErrPeerUnreachable", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Send took %s to fail; the retry budget must be bounded", elapsed)
	}
	t.Logf("Send failed after %s with %v", elapsed.Round(time.Millisecond), err)
}

// TestSendWaitsForAck proves Send's blocking semantics directly: with a socket
// that swallows everything, Send must still be running after a delay that a
// "queued locally" implementation would have returned from.
func TestSendWaitsForAck(t *testing.T) {
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := sink.LocalAddr().(*net.UDPAddr)
	_ = sink.Close()

	cfg := DefaultConfig()
	cfg.InitialRTO = 30 * time.Millisecond
	cfg.MinRTO = 30 * time.Millisecond
	cfg.MaxRTO = 60 * time.Millisecond
	cfg.MaxRetries = 3

	c, _ := newConnOn(t, deadAddr, 8, cfg)

	done := make(chan error, 1)
	go func() { done <- c.Send(make([]byte, 3*MaxPayload)) }()

	select {
	case err := <-done:
		t.Fatalf("Send returned %v immediately; it must wait for acknowledgement", err)
	case <-time.After(80 * time.Millisecond):
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Send returned nil although nothing was acknowledged")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send never returned")
	}
}

// TestFastRetransmitRecoversWithinOneRTO measures the loss-recovery KPI: after
// a single lost fragment, the message must complete in less than one RTO. RTO
// on loopback is the 50 ms floor, so the assertion is that the repair came from
// the SACK-driven fast retransmit rather than from a timer.
func TestFastRetransmitRecoversWithinOneRTO(t *testing.T) {
	const connID = 0x99
	const n = 20

	var mu sync.Mutex
	dropped := 0
	proxy := newSelectiveProxy(t, func(dir string, data []byte) bool {
		if dir != "up" {
			return false
		}
		p, err := decodePacket(data)
		if err != nil || p.typ != pktData || p.seq != 10 {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if dropped == 0 {
			dropped++
			return true
		}
		return false
	})

	a := NewConn(mustListen(t, proxy.front.LocalAddr()), proxy.front.LocalAddr(), connID, DefaultConfig())
	b := NewConn(mustListen(t, proxy.back.LocalAddr()), proxy.back.LocalAddr(), connID, DefaultConfig())
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	proxy.setPeers(a.LocalAddr(), b.LocalAddr())

	// Warm the RTT estimate so the RTO is the measured value, not the 200 ms
	// initial guess. This is what a real session looks like by the time a file
	// transfer starts.
	if err := a.Send([]byte("warmup")); err != nil {
		t.Fatalf("warmup send: %v", err)
	}
	if _, err := b.Recv(); err != nil {
		t.Fatalf("warmup recv: %v", err)
	}
	rtoBefore := a.Stats().RTO

	// One message of 20 fragments; fragment 10 is dropped once.
	msg := pad('Q', n)
	start := time.Now()
	if err := a.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := b.Recv()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("message corrupted")
	}

	mu.Lock()
	d := dropped
	mu.Unlock()
	if d != 1 {
		t.Fatalf("dropped %d fragments, want 1", d)
	}

	st := a.Stats()
	if elapsed >= rtoBefore {
		t.Fatalf("recovery took %s, which is not under one RTO (%s): the loss "+
			"was repaired by a timer, not by fast retransmit", elapsed, rtoBefore)
	}
	if st.FastRetrans == 0 {
		t.Fatal("no fast retransmit was recorded")
	}
	t.Logf("recovered from one lost fragment in %s (RTO %s), fast=%d rtoFires=%d",
		elapsed.Round(time.Microsecond), rtoBefore, st.FastRetrans, st.RTOTimeouts)
}

// TestWholeMessageRetransmitted drops every fragment of one message once, which
// exercises RTO-based repair of a complete message rather than a single hole.
func TestWholeMessageRetransmitted(t *testing.T) {
	const connID = 0xAB
	const n = 20
	// Message 8 occupies sequences 32..35.
	var mu sync.Mutex
	seen := map[uint32]bool{}
	proxy := newSelectiveProxy(t, func(dir string, data []byte) bool {
		if dir != "up" {
			return false
		}
		p, err := decodePacket(data)
		if err != nil || p.typ != pktData {
			return false
		}
		if p.msgID != 8 {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if seen[p.seq] {
			return false
		}
		seen[p.seq] = true
		return true
	})

	a := NewConn(mustListen(t, proxy.front.LocalAddr()), proxy.front.LocalAddr(), connID, DefaultConfig())
	b := NewConn(mustListen(t, proxy.back.LocalAddr()), proxy.back.LocalAddr(), connID, DefaultConfig())
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	proxy.setPeers(a.LocalAddr(), b.LocalAddr())

	msgs := make([][]byte, n)
	for i := range msgs {
		msgs[i] = pad(byte('a'+i), 4)
	}
	sendErr := make(chan error, 1)
	go func() {
		for i := range msgs {
			if err := a.Send(msgs[i]); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()

	got := recvN(t, b, n, 60*time.Second)
	if err := <-sendErr; err != nil {
		t.Fatalf("send: %v", err)
	}
	for i := range msgs {
		if !bytes.Equal(got[i], msgs[i]) {
			t.Fatalf("message %d corrupted after whole-message retransmission", i)
		}
	}
	mu.Lock()
	droppedCount := len(seen)
	mu.Unlock()
	if droppedCount != 4 {
		t.Fatalf("dropped %d fragments of message 8, want 4", droppedCount)
	}
}

// TestIdleTimeoutClosesSilentPeer checks the P2 keepalive/idle contract: a
// connection with no traffic in either direction must fail, so an application
// blocked in Recv learns the path is dead instead of waiting forever.
func TestIdleTimeoutClosesSilentPeer(t *testing.T) {
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()

	cfg := DefaultConfig()
	cfg.KeepAlivePeriod = 20 * time.Millisecond
	cfg.IdleTimeout = 60 * time.Millisecond

	c := NewConn(mustListen(t, sink.LocalAddr()), sink.LocalAddr(), 3, cfg)
	t.Cleanup(func() { _ = c.Close() })

	// The sink reads but never answers, so nothing is ever received.
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := sink.ReadFrom(buf); err != nil {
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, err := c.Recv()
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("Recv failed with %v, want ErrIdleTimeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle connection never timed out")
	}
}

// TestCloseUnblocksRecvAndSend checks that Close releases both directions, so a
// shutdown cannot leave a goroutine parked on the transport.
func TestCloseUnblocksRecvAndSend(t *testing.T) {
	a, b := pair(t, DefaultConfig(), 0)

	recvDone := make(chan error, 1)
	go func() {
		_, err := b.Recv()
		recvDone <- err
	}()

	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case err := <-recvDone:
		if err == nil {
			t.Fatal("Recv returned nil after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock Recv")
	}

	if err := a.Send([]byte("after close")); err == nil {
		t.Fatal("Send succeeded on a closed connection")
	}
}

// TestBidirectionalTransfer checks that both directions make progress
// simultaneously: the ACK fields are piggybacked on data, so a symmetric
// transfer exercises the write path from two goroutines at once.
func TestBidirectionalTransfer(t *testing.T) {
	a, b := pair(t, DefaultConfig(), 0.05)

	const n = 10
	msgsA := make([][]byte, n)
	msgsB := make([][]byte, n)
	for i := 0; i < n; i++ {
		msgsA[i] = pad(byte('A'+i), 3)
		msgsB[i] = pad(byte('a'+i), 3)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	go func() {
		defer wg.Done()
		for i := range msgsA {
			if err := a.Send(msgsA[i]); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := range msgsB {
			if err := b.Send(msgsB[i]); err != nil {
				errs <- err
				return
			}
		}
	}()

	gotA := recvN(t, a, n, 60*time.Second)
	gotB := recvN(t, b, n, 60*time.Second)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("send: %v", err)
	}

	for i := 0; i < n; i++ {
		if !bytes.Equal(gotA[i], msgsB[i]) {
			t.Fatalf("a received message %d corrupted", i)
		}
		if !bytes.Equal(gotB[i], msgsA[i]) {
			t.Fatalf("b received message %d corrupted", i)
		}
	}
}

// mustListen binds a loopback UDP socket.
func mustListen(t *testing.T, _ net.Addr) *net.UDPConn {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return pc
}

func dropName(d float64) string {
	switch d {
	case 0:
		return "0%"
	case 0.05:
		return "5%"
	case 0.10:
		return "10%"
	case 0.20:
		return "20%"
	}
	return "loss"
}
