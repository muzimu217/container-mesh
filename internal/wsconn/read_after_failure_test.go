package wsconn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ★ A SECOND READ AFTER A FAILED READ MUST RETURN AN ERROR, NOT PANIC.
//
// gorilla/websocket remembers that a read failed and PANICS on the next
// one: "repeated read on failed websocket connection". That panic
// surfaced on CI in TestParkedPeerDeathIsNoticed (internal/relay), on a
// commit that changed only a README — so it is a latent defect in the
// read path, not something the commit introduced.
//
// It matters because the read pattern in the relay is exactly the one
// that trips it: read → the peer died → the error is handled → the
// caller reads again to confirm → PANIC. A panic in the relay's
// message path is a crash of the whole node, and it looks like a
// random flake rather than a bug.
//
// The connection must be failed ONCE and then keep returning that same
// error. A caller that reads again must get the error, not a panic.

func TestAReadAfterAFailedReadReturnsTheError(t *testing.T) {
	// A server that accepts the websocket and then closes the TCP
	// connection, so the client's first read fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Close the underlying connection: the next read on the client
		// fails with a network error, and gorilla records the failure.
		c.Close()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, err := Dial(context.Background(), DialOptions{URL: wsURL, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// The first read fails. That is expected and is what sets the
	// connection's "failed" state.
	_, firstErr := c.Recv()
	if firstErr == nil {
		t.Fatal("the first read should have failed (the server closed the connection)")
	}

	// The second read must NOT panic. Before the fix it panicked with
	// "repeated read on failed websocket connection".
	secondErr := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the second read PANICKED instead of returning an error: %v", r)
			}
		}()
		_, err = c.Recv()
		return err
	}()
	if secondErr == nil {
		t.Fatal("the second read returned no error; a failed connection must keep failing")
	}
	// It should be the SAME error, not a fresh one: the connection was
	// already failed, so nothing changed.
	if !errors.Is(secondErr, firstErr) {
		t.Fatalf("the second read returned %v, want the original error %v", secondErr, firstErr)
	}
}

// ★ THE SPIN THAT CRASHES THE NODE.
//
// gorilla/websocket panics after 1000 consecutive failed reads on the
// same connection. Its own comment says why: "Applications that do
// handle the error returned from this method spin in tight loop on
// connection failure. To help application developers detect this error,
// panic on repeated reads to the failed connection."
//
// So the panic is gorilla telling us that a caller kept reading a dead
// connection. The defect is not gorilla's — it is that our Conn lets a
// dead connection keep being read.
//
// After the first failure gorilla returns the cached error INSTANTLY,
// so a loop that continues on error spins at full speed: 1000
// iterations in a few milliseconds, then the process panics.
//
// This is what CI caught: TestParkedPeerDeathIsNoticed panicked with
// "repeated read on failed websocket connection" on a commit that
// changed only a README — a latent defect in the read path, not
// something that commit introduced.
//
// A Conn that has failed must keep returning its error, and must never
// hand another read to gorilla.
func TestAFailedConnectionDoesNotPanicAfterAThousandReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c.Close()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, err := Dial(context.Background(), DialOptions{URL: wsURL, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if _, err := c.Recv(); err == nil {
		t.Fatal("the first read should have failed")
	}

	// 1500 reads — more than gorilla's 1000-read threshold.
	err = func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("reading a dead connection panicked after repeated failures: %v", r)
			}
		}()
		for i := 0; i < 1500; i++ {
			if _, e := c.Recv(); e != nil {
				err = e
			}
		}
		return err
	}()
	if err == nil {
		t.Fatal("reads on a dead connection must keep failing")
	}
}
