// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"net"
	"sync"
	"testing"
	"time"
)

// pipePair returns two Conns connected to each other over real UDP loopback.
func pipePair(t *testing.T, cfg Config) (*Conn, *Conn) {
	t.Helper()

	la, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	lb, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}

	const connID = 0x1122334455667788
	a := NewConn(la, lb.LocalAddr().(*net.UDPAddr), connID, cfg)
	b := NewConn(lb, la.LocalAddr().(*net.UDPAddr), connID, cfg)

	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

func TestSendReceiveRoundTrip(t *testing.T) {
	a, b := pipePair(t, DefaultConfig())

	msg := []byte("hello over udp")
	if err := a.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case got := <-recvAsync(b):
		if !bytes.Equal(got, msg) {
			t.Errorf("got %q, want %q", got, msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

// recvAsync reads one message in a goroutine.
func recvAsync(c *Conn) <-chan []byte {
	ch := make(chan []byte, 1)
	go func() {
		msg, err := c.Recv()
		if err == nil {
			ch <- msg
		}
		close(ch)
	}()
	return ch
}

// TestFragmentationLargeMessage covers messages that span many datagrams,
// which is the normal case for file chunks (512 KiB → ~440 fragments).
func TestFragmentationLargeMessage(t *testing.T) {
	a, b := pipePair(t, Config{Window: 256})

	msg := make([]byte, 512<<10)
	if _, err := rand.Read(msg); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(msg)

	if err := a.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case got := <-recvAsync(b):
		if len(got) != len(msg) {
			t.Fatalf("length %d, want %d", len(got), len(msg))
		}
		if sha256.Sum256(got) != want {
			t.Error("payload corrupted in transit")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out")
	}
}

// TestManyMessagesInOrder checks ordering across consecutive messages.
func TestManyMessagesInOrder(t *testing.T) {
	a, b := pipePair(t, DefaultConfig())

	const n = 40
	go func() {
		for i := 0; i < n; i++ {
			msg := []byte{byte(i), byte(i >> 8)}
			if err := a.Send(msg); err != nil {
				return
			}
		}
	}()

	for i := 0; i < n; i++ {
		select {
		case got := <-recvAsync(b):
			if len(got) != 2 || got[0] != byte(i) || got[1] != byte(i>>8) {
				t.Fatalf("message %d out of order or corrupt: %v", i, got)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out at message %d", i)
		}
	}
}

// TestEmptyMessageIsNoop: Send("") must not put anything on the wire, matching
// the relay transport's behaviour.
func TestEmptyMessageIsNoop(t *testing.T) {
	a, b := pipePair(t, DefaultConfig())
	if err := a.Send(nil); err != nil {
		t.Fatalf("send nil: %v", err)
	}
	ch := recvAsync(b)
	select {
	case <-ch:
		t.Fatal("empty message should not be delivered")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestCloseUnblocksRecv mirrors the wsconn regression: closing must release a
// blocked reader rather than deadlocking.
func TestCloseUnblocksRecv(t *testing.T) {
	_, b := pipePair(t, DefaultConfig())

	errCh := make(chan error, 1)
	go func() {
		_, err := b.Recv()
		errCh <- err
	}()
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() { _ = b.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Recv should return an error after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv did not unblock")
	}
}

// TestPeerFinClosesReceiver: a FIN must terminate the other side.
func TestPeerFinClosesReceiver(t *testing.T) {
	a, b := pipePair(t, DefaultConfig())

	_ = a.Close()

	type recvRes struct{ err error }
	ch := make(chan recvRes, 1)
	go func() {
		_, err := b.Recv()
		ch <- recvRes{err}
	}()
	select {
	case r := <-ch:
		if r.err == nil {
			t.Fatal("expected error after peer closed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("peer close was not noticed")
	}
}

// TestStrayDatagramsIgnored: packets with a different connID or garbage bytes
// must not disturb the session.
func TestStrayDatagramsIgnored(t *testing.T) {
	a, b := pipePair(t, DefaultConfig())
	_ = a

	// Send noise at b's port from an unrelated socket.
	noise, err := net.DialUDP("udp4", nil, b.remoteAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer noise.Close()
	_, _ = noise.Write([]byte("not a cmesh packet at all"))
	other := &packet{typ: pktData, connID: 0xDEADBEEF, seq: 1, payload: []byte("x")}
	if data, err := other.encode(); err == nil {
		_, _ = noise.Write(data)
	}

	// The real session must still work.
	if err := a.Send([]byte("still alive")); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case got := <-recvAsync(b):
		if string(got) != "still alive" {
			t.Errorf("got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stray traffic broke the session")
	}
}

// TestStatsReportProgress sanity-checks the counters used in diagnostics.
func TestStatsReportProgress(t *testing.T) {
	a, b := pipePair(t, DefaultConfig())

	if err := a.Send([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recvAsync(b):
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}

	// Give the ACK a moment to arrive.
	time.Sleep(200 * time.Millisecond)

	if s := a.Stats(); s.Sent == 0 {
		t.Error("Sent counter not incremented")
	}
	if s := b.Stats(); s.Recv == 0 {
		t.Error("Recv counter not incremented")
	}
	if s := a.Stats(); s.SRTT <= 0 {
		t.Error("no RTT sample recorded")
	}
}

// TestFragmentSizing keeps fragments within one datagram.
func TestFragmentSizing(t *testing.T) {
	for _, size := range []int{1, 100, MaxPayload - 8, MaxPayload, 5000, 512 << 10} {
		msg := make([]byte, size)
		frags := fragment(msg)
		if len(frags) == 0 {
			t.Fatalf("size %d produced no fragments", size)
		}
		var total int
		for _, f := range frags {
			if len(f)+8 > MaxPayload {
				t.Errorf("size %d: fragment of %d bytes overflows a datagram", size, len(f))
			}
			total += len(f)
		}
		if total != size {
			t.Errorf("size %d: fragments sum to %d", size, total)
		}
	}
}

// TestPacketRoundTrip covers encoding of every field.
func TestPacketRoundTrip(t *testing.T) {
	p := &packet{
		typ: pktData, connID: 0x0102030405060708, seq: 0xDEADBEEF,
		ack: 0xCAFEBABE, flags: flagFirst | flagLast, payload: []byte("body"),
	}
	data, err := p.encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.typ != p.typ || got.connID != p.connID || got.seq != p.seq ||
		got.ack != p.ack || got.flags != p.flags || string(got.payload) != "body" {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	bad := [][]byte{
		{},
		{0xC5},
		make([]byte, headerSize), // wrong magic
		append([]byte{0xC5, 0x01, 99}, make([]byte, 30)...), // wrong version
	}
	for i, b := range bad {
		if _, err := decode(b); err == nil {
			t.Errorf("case %d should have failed", i)
		}
	}
}

// TestConcurrentSendReceive hammers both directions simultaneously; the
// send/recv paths share the socket and must not race (run with -race).
//
// Each side needs exactly ONE reader goroutine: calling Recv from several
// goroutines would compete for the same inbox and lose messages, which is a
// property of the API (one reader, like net.Conn), not a bug.
func TestConcurrentSendReceive(t *testing.T) {
	a, b := pipePair(t, Config{Window: 128, InboxSize: 256})

	const n = 20

	// One dedicated reader per side.
	gotA := make(chan []byte, n)
	gotB := make(chan []byte, n)
	go func() {
		for i := 0; i < n; i++ {
			msg, err := b.Recv()
			if err != nil {
				return
			}
			gotA <- msg
		}
	}()
	go func() {
		for i := 0; i < n; i++ {
			msg, err := a.Recv()
			if err != nil {
				return
			}
			gotB <- msg
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if err := a.Send([]byte{byte(i), 'a'}); err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if err := b.Send([]byte{byte(i), 'b'}); err != nil {
				return
			}
		}
	}()
	wg.Wait()

	deadline := time.After(20 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case msg := <-gotA:
			if len(msg) != 2 || msg[1] != 'a' {
				t.Fatalf("A received corrupt message %v", msg)
			}
		case <-deadline:
			t.Fatalf("timeout receiving A-direction message %d", i)
		}
		select {
		case msg := <-gotB:
			if len(msg) != 2 || msg[1] != 'b' {
				t.Fatalf("B received corrupt message %v", msg)
			}
		case <-deadline:
			t.Fatalf("timeout receiving B-direction message %d", i)
		}
	}
}
