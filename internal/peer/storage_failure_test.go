// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

// F42: a receiver that cannot store the file must TELL the sender, with the
// reason, instead of letting the transfer die in silence.
//
// Before the fix the receiver returned a local error to its own caller and
// sent nothing: the sender kept waiting — 30 s for an accept/reject, and up
// to TEN MINUTES for a result — and all it learned was "no response within
// …". The root cause (read-only volume, out of space, a path that is not a
// directory) never crossed the wire, so the operator's only clue was a
// timeout on the other machine.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/secure"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// pairReceivingInto is pair() with the receiver's output directory chosen
// by the test. The directory is fixed BEFORE the session starts: changing
// the hooks' directory afterwards is a data race with the session's own
// goroutine.
func pairReceivingInto(t *testing.T, code, recvDir string) (a, b *Session, ha, hb *hooks) {
	t.Helper()
	ca, cb := newMemPipePair()
	ha, hb = newHooks(t.TempDir()), newHooksCap(recvDir, 64)

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

// TestReceiverReportsStorageFailureToSender: the sender must get a verdict
// quickly, and the verdict must name the local cause.
func TestReceiverReportsStorageFailureToSender(t *testing.T) {
	// The receiver's directory cannot be written to: creating the .part
	// file fails with EACCES.
	roDir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })
	if probeErr := os.WriteFile(filepath.Join(roDir, ".probe"), []byte("x"), 0o600); probeErr == nil {
		_ = os.Remove(filepath.Join(roDir, ".probe"))
		t.Skip("this process can write to a 0500 directory (running as root?); the failure cannot be simulated")
	}

	a, b, _, _ := pairReceivingInto(t, "storage-failure-code", roDir)

	src := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		res *wire.FileResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := a.SendFile(src, nil)
		done <- outcome{res, err}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(12 * time.Second):
		// The unfixed receiver says nothing at all, so the sender is still
		// inside its 30 s accept wait. Failing here rather than waiting it
		// out keeps the regression cheap to notice.
		t.Fatal("the sender is still waiting after 12s: the receiver must report its local storage failure")
	}

	if got.res == nil {
		t.Fatalf("the sender got no verdict at all (err=%v)", got.err)
	}
	if got.res.OK {
		t.Fatalf("the transfer reported success despite a read-only directory: %+v", got.res)
	}
	// The verdict must carry the CAUSE, not just "failed".
	msg := strings.ToLower(got.res.Message)
	if !strings.Contains(msg, "read-only") && !strings.Contains(msg, "permission denied") &&
		!strings.Contains(msg, "cannot store") && !strings.Contains(msg, "denied") {
		t.Fatalf("the verdict does not name the local cause: %q", got.res.Message)
	}
	_ = b
}

// TestReceiverReportsStorageFailureMidTransfer: the failure can also happen
// AFTER the accept — the volume fills up while the bytes stream. There the
// sender is waiting for a RESULT (not a reject), and it waits ten minutes.
func TestReceiverReportsStorageFailureMidTransfer(t *testing.T) {
	roDir := filepath.Join(t.TempDir(), "ro2")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })
	if probeErr := os.WriteFile(filepath.Join(roDir, ".probe"), []byte("x"), 0o600); probeErr == nil {
		_ = os.Remove(filepath.Join(roDir, ".probe"))
		t.Skip("this process can write to a 0500 directory (running as root?); the failure cannot be simulated")
	}

	a, b, _, _ := pairReceivingInto(t, "storage-failure-mid-code", roDir)

	src := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(src, []byte(strings.Repeat("y", 8192)), 0o644); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		res *wire.FileResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := a.SendFile(src, nil)
		done <- outcome{res, err}
	}()

	select {
	case got := <-done:
		if got.res == nil {
			t.Fatalf("the sender got no verdict at all (err=%v)", got.err)
		}
		if got.res.OK {
			t.Fatalf("the transfer reported success despite a read-only directory: %+v", got.res)
		}
		msg := strings.ToLower(got.res.Message)
		if !strings.Contains(msg, "read-only") && !strings.Contains(msg, "permission denied") &&
			!strings.Contains(msg, "cannot store") && !strings.Contains(msg, "denied") {
			t.Fatalf("the verdict does not name the local cause: %q", got.res.Message)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("the sender is still waiting after 12s: the receiver must report its local storage failure")
	}
	_ = b
}
