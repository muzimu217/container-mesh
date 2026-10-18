// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// TestConcurrentSendsStayOrdered hammers SendFrame from several goroutines.
// Because the receiver enforces a strictly increasing counter, any window
// where sealing and sending are not atomic shows up as a decrypt failure.
func TestConcurrentSendsStayOrdered(t *testing.T) {
	const (
		workers = 8
		each    = 50
		total   = workers * each
	)

	a, _, ha, hb := pairWithCaps(t, "concurrent-send-code", 64, total)

	seen := make(map[string]bool, total)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := a.SendMessage(fmt.Sprintf("w%d-%d", id, i)); err != nil {
					t.Errorf("worker %d send %d: %v", id, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	for i := 0; i < total; i++ {
		select {
		case m := <-hb.msgs:
			seen[m] = true
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out at message %d/%d", i+1, total)
		}
	}
	if len(seen) != total {
		t.Fatalf("got %d distinct messages, want %d", len(seen), total)
	}
	_ = ha
}

// TestMessageAfterFileTransfer checks that the stream is still usable after a
// file transfer completes, i.e. the file-done frame was consumed exactly once.
func TestMessageAfterFileTransfer(t *testing.T) {
	a, _, _, hb := pair(t, "post-transfer-code")

	dir := t.TempDir()
	src := dir + "/small.txt"
	if err := writeFile(src, []byte("payload")); err != nil {
		t.Fatal(err)
	}

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send file: %v", err)
	}
	fd := waitFile(t, hb)
	if fd.err != nil {
		t.Fatalf("receive file: %v", fd.err)
	}
	if !res.OK {
		t.Fatalf("transfer failed: %s", res.Message)
	}

	// Now a plain message must still work.
	if err := a.SendMessage("after file"); err != nil {
		t.Fatalf("post-transfer send: %v", err)
	}
	if got := waitMsg(t, hb); got != "after file" {
		t.Errorf("got %q", got)
	}
}

// TestRolesAreSymmetric makes sure role B can also initiate a file send.
// Nothing in the protocol ties the sender to role A.
func TestRolesAreSymmetric(t *testing.T) {
	_, b, ha, _ := pair(t, "symmetric-roles-code")

	dir := t.TempDir()
	src := dir + "/from-b.bin"
	if err := writeFile(src, []byte("b sends this")); err != nil {
		t.Fatal(err)
	}

	res, err := b.SendFile(src, nil)
	if err != nil {
		t.Fatalf("B send: %v", err)
	}
	fd := waitFile(t, ha)
	if fd.err != nil {
		t.Fatalf("A receive: %v", fd.err)
	}
	if !res.OK || fd.res == nil || !fd.res.OK {
		t.Fatalf("transfer failed: %v / %v", res, fd.res)
	}
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
