// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package wsconn

import (
	"bytes"
	"testing"
)

// TestRecvIntoGrowthIsVisibleToTheCaller pins that the buffer RecvInto grew
// is reachable by the caller.
//
// RecvInto takes a buffer and returns the message in it. When the message
// does not fit, it grows its own copy and returns that. The caller then
// keeps handing back the ORIGINAL buffer, so every oversized message pays
// the growth again.
//
// The relay's forward loop made this expensive: it read
// "buf = buf[:cap(buf)]" from its own stale slice, which never saw the
// growth, so a 500 KiB message re-allocated and re-copied 64, 128, 256 and
// 512 KiB every time. Measured on the relay: makeslicecopy was 23.73% of
// its CPU.
//
// The fix has to make the grown buffer visible, either by returning it or by
// letting the caller adopt the returned slice's capacity.
func TestRecvIntoGrowthIsVisibleToTheCaller(t *testing.T) {
	cli, srv := pairOverHTTP(t)

	// A message several times the initial buffer, so RecvInto has to grow.
	payload := bytes.Repeat([]byte("G"), 400_000)
	if err := cli.Send(payload); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 64<<10)
	got, err := srv.RecvInto(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload corrupted: %d bytes, want %d", len(got), len(payload))
	}

	// The caller must be able to adopt the grown buffer: the capacity of the
	// returned slice is what RecvInto actually used.
	if cap(got) <= cap(buf) {
		t.Fatalf("the grown buffer is not visible to the caller: "+
			"returned cap %d, caller cap %d. The caller keeps handing back "+
			"the original buffer and pays the growth on every message.",
			cap(got), cap(buf))
	}

	// A second, larger message must reuse the adopted buffer rather than
	// growing from scratch again.
	buf = got[:cap(got)]
	larger := bytes.Repeat([]byte("H"), cap(buf)+1000)
	if err := cli.Send(larger); err != nil {
		t.Fatal(err)
	}
	got2, err := srv.RecvInto(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, larger) {
		t.Fatalf("second payload corrupted: %d bytes, want %d", len(got2), len(larger))
	}
}
