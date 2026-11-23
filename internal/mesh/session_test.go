// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/quic-go/quic-go"
)

// memSession is an in-memory Session used to drive the transport-agnostic
// session layer without a network.
//
// Its Close() is a HALF close — only the write side ends — and that is
// not decoration: the message protocol DEPENDS on it. The sender writes
// its frames and then half-closes to mark the end of the messages; the
// receiver sees the EOF and only then writes its "done" ack. A session
// whose Close() ended both directions would break that exchange, which
// is exactly the semantic difference between quic.Stream.Close() (FIN)
// and io.ReadWriteCloser.Close() (full close).
type memSession struct {
	mu     sync.Mutex
	inBuf  []byte
	outBuf bytes.Buffer
	peer   *memSession
	closed atomic.Bool // our write side is closed
	wake   chan struct{}
	rdead  time.Time
	wdead  time.Time
}

// newMemSessionPair returns two sessions wired to each other.
func newMemSessionPair() (*memSession, *memSession) {
	a := &memSession{wake: make(chan struct{}, 1)}
	b := &memSession{wake: make(chan struct{}, 1)}
	a.peer, b.peer = b, a
	return a, b
}

func (s *memSession) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *memSession) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if len(s.inBuf) > 0 {
			n := copy(p, s.inBuf)
			s.inBuf = s.inBuf[n:]
			s.mu.Unlock()
			return n, nil
		}
		deadline := s.rdead
		// The peer's CLOSE must be read UNDER THE SAME LOCK as the
		// buffer.
		//
		// It used to be read after the unlock, and that was a race with
		// a lost message: a reader that found the buffer empty could
		// then see "the peer has closed" and report io.EOF — even
		// though the bytes were written in between and were sitting in
		// the buffer. The reader had checked the buffer BEFORE the write
		// and the close AFTER it, so the write fell between the two
		// checks.
		//
		// The symptom was a test that failed for the wrong reason:
		// "the receiver accepted a truncated frame; it must report a
		// failure" — the truncated frame was never seen at all,
		// because the double threw it away. And it was LOAD-SENSITIVE
		// (it only appeared when the whole package ran at once),
		// which is what made it look like a product defect.
		//
		// A half close means "no MORE bytes", not "no bytes were
		// ever written". So the buffer is the authority, and the close
		// flag is only consulted once the buffer is empty — under the
		// lock that guards the buffer.
		peerClosed := s.peer.closed.Load()
		s.mu.Unlock()
		if peerClosed {
			return 0, io.EOF
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return 0, os.ErrDeadlineExceeded
		}
		select {
		case <-s.wake:
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (s *memSession) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	s.peer.mu.Lock()
	s.peer.inBuf = append(s.peer.inBuf, p...)
	s.peer.mu.Unlock()
	s.peer.signal()
	return len(p), nil
}

// Close is a HALF close: the write side ends, the read side stays open.
func (s *memSession) Close() error {
	s.closed.Store(true)
	s.peer.signal()
	return nil
}

func (s *memSession) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.rdead = t
	s.mu.Unlock()
	return nil
}

func (s *memSession) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.wdead = t
	s.mu.Unlock()
	return nil
}

// readLine reads one '\n'-terminated line from the session.
func (s *memSession) readLine(t *testing.T, capBytes int) string {
	t.Helper()
	_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	for len(buf) < capBytes {
		n, err := s.Read(one)
		if n > 0 {
			buf = append(buf, one[0])
			if one[0] == '\n' {
				return string(bytes.TrimRight(buf, "\n"))
			}
			continue
		}
		if err != nil {
			t.Fatalf("read line: %v (got %q so far)", err, buf)
		}
	}
	t.Fatalf("line too long: %q", buf)
	return ""
}

// TestQuicStreamIsASession: the QUIC stream must satisfy the Session
// interface as it stands — that is what makes the refactor a pure
// retype rather than a rewrite.
func TestQuicStreamIsASession(t *testing.T) {
	var _ Session = (*quic.Stream)(nil)
}

// TestServeSessionKindDispatch drives the transport-agnostic session
// layer with an in-memory session and asserts that each session kind
// reaches ITS OWN handler.
//
// This is the test that makes "four kinds of business share one
// transport" a checkable claim rather than a design intention: the
// dispatcher must not be able to send a message session to the ping
// handler, and each kind must produce its own characteristic answer.
func TestServeSessionKindDispatch(t *testing.T) {
	t.Run("ping", func(t *testing.T) {
		local, remote := newMemSessionPair()
		go serveSession(KindPing, identity.DeviceID("peer00000000000"), bufio.NewReader(remote), remote, ListenerConfig{})

		rec := make([]byte, pingRecordSize)
		binary.BigEndian.PutUint64(rec[0:], 7)
		if _, err := local.Write(rec); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, pingRecordSize)
		_ = local.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(local, got); err != nil {
			t.Fatalf("read ping echo: %v", err)
		}
		if !bytes.Equal(got, rec) {
			t.Fatalf("ping echo = %v, want %v", got, rec)
		}
	})

	t.Run("dial", func(t *testing.T) {
		local, remote := newMemSessionPair()
		go serveSession(KindDial, identity.DeviceID("peer00000000000"), bufio.NewReader(remote), remote, ListenerConfig{})

		if _, err := local.Write([]byte("hello\n")); err != nil {
			t.Fatal(err)
		}
		if got := local.readLine(t, 64); got != "hello" {
			t.Fatalf("dial echo = %q, want %q", got, "hello")
		}
	})

	t.Run("msg", func(t *testing.T) {
		local, remote := newMemSessionPair()
		got := make(chan string, 4)
		cfg := ListenerConfig{OnMessage: func(from identity.DeviceID, text string) {
			got <- text
		}}
		go serveSession(KindMsg, identity.DeviceID("peer00000000000"), bufio.NewReader(remote), remote, cfg)

		if err := writeFrame(local, []byte("first")); err != nil {
			t.Fatal(err)
		}
		// Half close: this is how the sender marks the end of the
		// messages, and the receiver answers only after seeing it.
		if err := local.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case text := <-got:
			if text != "first" {
				t.Fatalf("message = %q, want %q", text, "first")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the message handler never fired")
		}
		if ack := local.readLine(t, 64); ack != "done" {
			t.Fatalf("message ack = %q, want %q", ack, "done")
		}
	})

	t.Run("file", func(t *testing.T) {
		local, remote := newMemSessionPair()
		// No inbox: the file handler must answer with its own refusal,
		// which is only reachable from the file branch.
		go serveSession(KindFile, identity.DeviceID("peer00000000000"), bufio.NewReader(remote), remote, ListenerConfig{})

		hdr, err := json.Marshal(FileHeader{Name: "x.bin", Size: 4, SHA256: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := local.Write(append(hdr, '\n')); err != nil {
			t.Fatal(err)
		}
		if line := local.readLine(t, 256); line != "err: receiver has no inbox configured" {
			t.Fatalf("file answer = %q, want the inbox refusal", line)
		}
	})

	t.Run("run", func(t *testing.T) {
		local, remote := newMemSessionPair()
		go serveSession(KindRun, identity.DeviceID("peer00000000000"), bufio.NewReader(remote), remote, ListenerConfig{})

		req, err := json.Marshal(RunRequest{Verb: "@nosuchverb"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := local.Write(append(req, '\n')); err != nil {
			t.Fatal(err)
		}
		// The run handler answers with its own frame format.
		br := bufio.NewReader(local)
		tag, payload, err := readRunFrame(br)
		if err != nil {
			t.Fatalf("read run frame: %v", err)
		}
		if tag != RunTagErr {
			t.Fatalf("run frame tag = %q, want the error tag", tag)
		}
		if !bytes.Contains(payload, []byte("unknown verb")) {
			t.Fatalf("run error = %q, want the unknown-verb reason", payload)
		}
	})

	t.Run("bench", func(t *testing.T) {
		local, remote := newMemSessionPair()
		go serveSession(KindBench, identity.DeviceID("peer00000000000"), bufio.NewReader(remote), remote, ListenerConfig{})

		req, err := json.Marshal(benchRequest{Size: 0})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := local.Write(append(req, '\n')); err != nil {
			t.Fatal(err)
		}
		if line := local.readLine(t, 256); !bytes.HasPrefix([]byte(line), []byte("err:")) {
			t.Fatalf("bench answer = %q, want the bench validation error", line)
		}
	})
}

// TestServeSessionRejectsUnknownKind: an unknown kind must be refused
// rather than silently served by some handler.
func TestServeSessionRejectsUnknownKind(t *testing.T) {
	local, remote := newMemSessionPair()
	err := serveSession("nosuchkind", identity.DeviceID("peer00000000000"), bufio.NewReader(remote), remote, ListenerConfig{})
	if err == nil {
		t.Fatal("an unknown kind must be refused")
	}
	_ = local
}

// TestSessionFileRoundTripOverMemorySession: the file handler over an
// in-memory session saves a file whose bytes and hash match what the
// sender promised. It proves the file path is transport-agnostic too, not
// just the ping path.
func TestSessionFileRoundTripOverMemorySession(t *testing.T) {
	inbox := t.TempDir()
	local, remote := newMemSessionPair()
	saved := make(chan string, 1)
	cfg := ListenerConfig{
		InboxDir: inbox,
		OnFile:   func(from identity.DeviceID, path string, h FileHeader) { saved <- path },
	}
	go serveSession(KindFile, identity.DeviceID("peer00000000000"), bufio.NewReader(remote), remote, cfg)

	payload := []byte("payload over a memory session")
	sum := sha256Hex(payload)
	hdr, err := json.Marshal(FileHeader{Name: "memo.bin", Size: int64(len(payload)), SHA256: sum})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.Write(append(hdr, '\n')); err != nil {
		t.Fatal(err)
	}
	if line := local.readLine(t, 256); line != "ok" {
		t.Fatalf("file ack = %q, want %q", line, "ok")
	}
	if _, err := local.Write(payload); err != nil {
		t.Fatal(err)
	}
	if line := local.readLine(t, 256); line != "done "+sum {
		t.Fatalf("file verdict = %q, want %q", line, "done "+sum)
	}
	select {
	case p := <-saved:
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read saved file: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("saved bytes = %q, want %q", got, payload)
		}
		if filepath.Base(p) != "memo.bin" {
			t.Fatalf("saved name = %q, want %q", filepath.Base(p), "memo.bin")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnFile never fired")
	}
}

// sha256Hex is the hex sha256 of b, matching the sender's header format.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
