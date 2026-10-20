package p2p2

import (
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"
)

// Regression: a window smaller than the batch limit must not deadlock.
//
// A batched datagram holds a sequence number and counts toward inFlight but is
// not written until the batch fills. If Send blocks on the window with a
// non-empty batch, the window can never free: the datagrams that would free it
// are the ones sitting in the buffer.
func TestBatchWindowSmallerThanBatchDoesNotDeadlock(t *testing.T) {
	if !batchingSupported {
		t.Skip("platform has no vectored datagram send")
	}
	for _, window := range []int{2, 4, 10} {
		cfg := DefaultConfig()
		cfg.SendWindow = window
		cfg.BatchSize = 32 // far above the window
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
		st := time.Now()
		if err := a.Send(msg); err != nil {
			t.Fatalf("window=%d: send: %v", window, err)
		}
		select {
		case got := <-done:
			if sha256.Sum256(got) != want {
				t.Fatalf("window=%d: corrupt", window)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("window=%d: deadlocked with a batch larger than the window", window)
		}
		t.Logf("window=%d batch=%d: %s", window, cfg.BatchSize, time.Since(st).Round(time.Microsecond))
		_ = a.Close()
		_ = b.Close()
	}
}
