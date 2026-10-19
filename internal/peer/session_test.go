// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/secure"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// hooks captures Handler events for tests.
type hooks struct {
	msgs  chan string
	files chan fileDone
	dir   string // destination directory for accepted offers
}

type fileDone struct {
	res  *wire.FileResult
	err  error
	name string
}

func newHooks(dir string) *hooks { return newHooksCap(dir, 64) }

func newHooksCap(dir string, msgCap int) *hooks {
	return &hooks{msgs: make(chan string, msgCap), files: make(chan fileDone, 8), dir: dir}
}

func (h *hooks) handler() Handler {
	return Handler{
		OnMessage: func(text string) { h.msgs <- text },
		OnOffer: func(tr *InboundTransfer) bool {
			go func() {
				res, err := tr.Accept(h.dir, nil)
				h.files <- fileDone{res: res, err: err, name: tr.Offer.Name}
			}()
			return true
		},
	}
}

// waitFile waits for one inbound transfer to finish.
func waitFile(t *testing.T, h *hooks) fileDone {
	t.Helper()
	select {
	case fd := <-h.files:
		return fd
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for inbound file")
		return fileDone{}
	}
}

// waitMsg waits for one inbound chat message.
func waitMsg(t *testing.T, h *hooks) string {
	t.Helper()
	select {
	case m := <-h.msgs:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for message")
		return ""
	}
}

// pair starts two sessions over an in-memory pipe and returns them after the
// hello exchange, together with their event hooks.
func pair(t *testing.T, code string) (a, b *Session, ha, hb *hooks) {
	t.Helper()
	ca, cb := newMemPipePair()
	ha, hb = newHooks(t.TempDir()), newHooks(t.TempDir())

	type res struct {
		s   *Session
		err error
	}
	chA := make(chan res, 1)
	chB := make(chan res, 1)

	go func() {
		s, err := Handshake(ca, Config{Name: "alpha", Code: code, Role: secure.RoleA})
		if err == nil {
			err = s.Start(ha.handler())
		}
		chA <- res{s, err}
	}()
	go func() {
		s, err := Handshake(cb, Config{Name: "beta", Code: code, Role: secure.RoleB})
		if err == nil {
			err = s.Start(hb.handler())
		}
		chB <- res{s, err}
	}()

	ra, rb := <-chA, <-chB
	if ra.err != nil {
		t.Fatalf("role A: %v", ra.err)
	}
	if rb.err != nil {
		t.Fatalf("role B: %v", rb.err)
	}
	t.Cleanup(func() { _ = ra.s.Close(); _ = rb.s.Close() })
	return ra.s, rb.s, ha, hb
}

// pairWithCaps is pair() with a custom OnMessage buffer size on side A (the
// concurrent-send test needs room for hundreds of queued messages).
func pairWithCaps(t *testing.T, code string, capA, capB int) (a, b *Session, ha, hb *hooks) {
	t.Helper()
	ca, cb := newMemPipePair()
	ha, hb = newHooksCap(t.TempDir(), capA), newHooksCap(t.TempDir(), capB)

	type res struct {
		s   *Session
		err error
	}
	chA := make(chan res, 1)
	chB := make(chan res, 1)

	go func() {
		s, err := Handshake(ca, Config{Name: "alpha", Code: code, Role: secure.RoleA})
		if err == nil {
			err = s.Start(ha.handler())
		}
		chA <- res{s, err}
	}()
	go func() {
		s, err := Handshake(cb, Config{Name: "beta", Code: code, Role: secure.RoleB})
		if err == nil {
			err = s.Start(hb.handler())
		}
		chB <- res{s, err}
	}()

	ra, rb := <-chA, <-chB
	if ra.err != nil {
		t.Fatalf("role A: %v", ra.err)
	}
	if rb.err != nil {
		t.Fatalf("role B: %v", rb.err)
	}
	t.Cleanup(func() { _ = ra.s.Close(); _ = rb.s.Close() })
	return ra.s, rb.s, ha, hb
}

func TestHandshakeAndMessage(t *testing.T) {
	a, b, ha, hb := pair(t, "correct horse battery")

	if a.PeerName != "beta" {
		t.Errorf("A sees peer %q, want beta", a.PeerName)
	}
	if b.PeerName != "alpha" {
		t.Errorf("B sees peer %q, want alpha", b.PeerName)
	}

	if err := a.SendMessage("hello from A"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := waitMsg(t, hb); got != "hello from A" {
		t.Errorf("payload %q, want %q", got, "hello from A")
	}

	// Bidirectional.
	if err := b.SendMessage("and back from B"); err != nil {
		t.Fatalf("send back: %v", err)
	}
	if got := waitMsg(t, ha); got != "and back from B" {
		t.Errorf("payload %q", got)
	}
}

func TestCodeNormalization(t *testing.T) {
	cases := map[string]string{
		"Abc-Def":     "abc-def",
		"  ABC DEF  ": "abc-def",
		"abc_def":     "abc-def",
		"abc--def":    "abc-def",
		"ABC":         "abc",
	}
	for in, want := range cases {
		if got := secure.NormalizeCode(in); got != want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandshakeWithDifferentCodesFails(t *testing.T) {
	ca, cb := newMemPipePair()
	errA := make(chan error, 1)
	errB := make(chan error, 1)

	go func() {
		s, err := Handshake(ca, Config{Name: "a", Code: "code-one", Role: secure.RoleA})
		if err == nil {
			// A's hello will be sealed with a key B cannot open.
			err = s.Start(Handler{})
		}
		errA <- err
	}()
	go func() {
		s, err := Handshake(cb, Config{Name: "b", Code: "code-two", Role: secure.RoleB})
		if err == nil {
			err = s.Start(Handler{})
		}
		errB <- err
	}()

	if err := <-errB; err == nil {
		t.Fatal("role B should fail when codes differ")
	}
	// A may block waiting for B's hello, which will never arrive; the
	// important property is that B refuses to accept A's frames.
	_ = ca.Close()
	_ = cb.Close()
	<-errA
}

func TestSendReceiveFileRoundTrip(t *testing.T) {
	a, _, _, hb := pair(t, "file-transfer-code")

	srcDir := t.TempDir()

	// 300 KiB of random data spans several chunks.
	data := make([]byte, 300<<10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcDir, "payload.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	fd := waitFile(t, hb)

	if fd.err != nil {
		t.Fatalf("receive: %v", fd.err)
	}
	if !res.OK {
		t.Fatalf("sender got rejection: %s", res.Message)
	}
	if !fd.res.OK {
		t.Fatalf("receiver verification failed: %s", fd.res.Message)
	}

	got, err := os.ReadFile(filepath.Join(hb.dir, "payload.bin"))
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatal("received bytes differ from source")
	}

	// No .part file should remain.
	if _, err := os.Stat(filepath.Join(hb.dir, "payload.bin.part")); !os.IsNotExist(err) {
		t.Errorf("leftover .part file: %v", err)
	}
}

// TestResumeFromPartialPartFile simulates an interrupted transfer: the
// receiver already holds a prefix of the file in <name>.part.
func TestResumeFromPartialPartFile(t *testing.T) {
	a, _, _, hb := pair(t, "resume-test-code")

	srcDir := t.TempDir()

	data := make([]byte, 256<<10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcDir, "big.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-seed a 100 KiB prefix, as if a previous run died mid-transfer.
	const prefix = 100 << 10
	if err := os.WriteFile(filepath.Join(hb.dir, "big.bin.part"), data[:prefix], 0o644); err != nil {
		t.Fatal(err)
	}

	var sentFrom int64 = -1
	res, err := a.SendFile(src, func(sent, total int64) {
		if sentFrom < 0 {
			sentFrom = sent
		}
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	fd := waitFile(t, hb)
	if fd.err != nil {
		t.Fatalf("receive: %v", fd.err)
	}
	if !res.OK {
		t.Fatalf("transfer failed: %s", res.Message)
	}
	if sentFrom != prefix {
		t.Errorf("sender resumed at %d, want %d", sentFrom, prefix)
	}

	got, err := os.ReadFile(filepath.Join(hb.dir, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatal("resumed file differs from source")
	}
}

// TestCorruptPartialIsRejectedAndRemoved checks that a .part file with wrong
// contents cannot silently produce a "successful" transfer.
func TestCorruptPartialIsRejectedAndRemoved(t *testing.T) {
	a, _, _, hb := pair(t, "corrupt-resume-code")

	srcDir := t.TempDir()

	data := make([]byte, 128<<10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcDir, "x.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Garbage prefix with the right length.
	garbage := make([]byte, 64<<10)
	if _, err := rand.Read(garbage); err != nil {
		t.Fatal(err)
	}
	partPath := filepath.Join(hb.dir, "x.bin.part")
	if err := os.WriteFile(partPath, garbage, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	fd := waitFile(t, hb)

	if res.OK {
		t.Fatal("sender should see a failed verification")
	}
	if fd.res == nil || fd.res.OK {
		t.Fatal("receiver should report failure")
	}
	if !strings.Contains(fd.res.Message, "hash mismatch") {
		t.Errorf("unexpected message: %s", fd.res.Message)
	}
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Errorf("corrupt .part should be removed, stat err = %v", err)
	}
}

// TestPingDuringFileTransfer keeps injecting FramePing from both sides while
// a transfer is in flight. The read pump must answer pings transparently; if
// transfer loops had to handle pings themselves this would desync the stream.
func TestPingDuringFileTransfer(t *testing.T) {
	a, b, ha, hb := pair(t, "ping-interleave-code")

	src := filepath.Join(t.TempDir(), "pinged.bin")
	data := make([]byte, 400<<10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var pingers sync.WaitGroup
	ping := func(s *Session) {
		defer pingers.Done()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_ = s.SendFrame(&wire.Frame{Type: wire.FramePing, Payload: []byte("hb")})
			}
		}
	}
	pingers.Add(2)
	go ping(a)
	go ping(b)

	res, err := a.SendFile(src, nil)
	close(stop)
	pingers.Wait()
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	fd := waitFile(t, hb)
	if fd.err != nil {
		t.Fatalf("receive: %v", fd.err)
	}
	if !res.OK || !fd.res.OK {
		t.Fatalf("transfer failed: %s / %s", res.Message, fd.res.Message)
	}

	// Pings answered must not have corrupted the message path either.
	if err := a.SendMessage("still alive"); err != nil {
		t.Fatalf("post-ping send: %v", err)
	}
	if got := waitMsg(t, hb); got != "still alive" {
		t.Errorf("got %q", got)
	}
	_ = ha // both directions' hooks exist; B's pings were answered by A's pump
}

// TestDuplexTransfers runs a file in each direction at the same time. The
// routing must keep the two directions independent: A's accept/result frames
// belong to A's offer and B's chunks to B's file.
func TestDuplexTransfers(t *testing.T) {
	a, b, ha, hb := pair(t, "duplex-code")

	dataA := make([]byte, 250<<10)
	dataB := make([]byte, 250<<10)
	if _, err := rand.Read(dataA); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(dataB); err != nil {
		t.Fatal(err)
	}
	srcA := filepath.Join(t.TempDir(), "from-a.bin")
	srcB := filepath.Join(t.TempDir(), "from-b.bin")
	if err := os.WriteFile(srcA, dataA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcB, dataB, 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var resA, resB *wire.FileResult
	var errA, errB error
	go func() {
		defer wg.Done()
		resA, errA = a.SendFile(srcA, nil)
	}()
	go func() {
		defer wg.Done()
		resB, errB = b.SendFile(srcB, nil)
	}()
	wg.Wait()

	fdA := waitFile(t, ha) // B's file arriving at A
	fdB := waitFile(t, hb) // A's file arriving at B

	if errA != nil || errB != nil {
		t.Fatalf("send errors: %v / %v", errA, errB)
	}
	if fdA.err != nil || fdB.err != nil {
		t.Fatalf("receive errors: %v / %v", fdA.err, fdB.err)
	}
	if !resA.OK || !resB.OK || !fdA.res.OK || !fdB.res.OK {
		t.Fatalf("results: %s / %s / %s / %s", resA.Message, resB.Message, fdA.res.Message, fdB.res.Message)
	}

	gotA, err := os.ReadFile(filepath.Join(ha.dir, "from-b.bin"))
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := os.ReadFile(filepath.Join(hb.dir, "from-a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(gotA) != sha256.Sum256(dataB) {
		t.Fatal("A received wrong bytes")
	}
	if sha256.Sum256(gotB) != sha256.Sum256(dataA) {
		t.Fatal("B received wrong bytes")
	}
}

// TestRejectOffer makes sure a handler that declines an offer informs the
// sender instead of stalling it.
func TestRejectOffer(t *testing.T) {
	a, b, _, _ := pair(t, "reject-code")

	src := filepath.Join(t.TempDir(), "refused.bin")
	if err := os.WriteFile(src, []byte("no thanks"), 0o644); err != nil {
		t.Fatal(err)
	}

	b.handler.OnOffer = nil // nil OnOffer rejects every offer

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.OK {
		t.Fatal("offer should have been rejected")
	}
	if !strings.Contains(res.Message, "not accepted") {
		t.Errorf("rejection message %q", res.Message)
	}
}

func TestValidateNameRejectsTraversal(t *testing.T) {
	bad := []string{"", ".", "..", "../etc/passwd", "a/b", `a\b`, ".hidden"}
	for _, n := range bad {
		if err := validateName(n); err == nil {
			t.Errorf("validateName(%q) should fail", n)
		}
	}
	if err := validateName("good-file.bin"); err != nil {
		t.Errorf("validateName(good) = %v", err)
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	const expected = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != expected {
		t.Errorf("hash = %s, want %s", got, expected)
	}
}
