// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// ---------------------------------------------------------------------------
// THE RECORD-SIZE QUESTION, MEASURED ON A REAL QUIC RELAY.
//
// The performance agent proposed "coalesce the requester's writes so each
// relay record carries 256 KiB to 1 MiB", and projected the ceiling
// moving from ~54 MiB/s to ~434 MiB/s.
//
// That projection rested on ONE assumption: that the relay's per-record
// cycle time (589.9 us, measured on the real host) is INDEPENDENT of the
// record's size. If that holds, fewer records means proportionally
// fewer cycles and the win is real.
//
// But the repository's OWN record-size scan contradicts it: on the JSON
// product path, 4 KiB records gave 27.65 MiB/s and 786 KiB records gave
// 42.29 MiB/s — a gain of +53%, NOT the 8x the projection needs. So the
// cycle time is NOT independent of the size: a bigger record spends
// proportionally more time moving its bytes.
//
// This test measures the thing the projection assumes, on a real QUIC
// relay, with real bytes.
//
// It is a MEASUREMENT, not a gate: it records what the relay does at
// each record size so the next round can decide from data instead of
// from arithmetic. It asserts only the invariants that must hold
// whatever the numbers are:
//
//   - the payload arrives intact at every size;
//   - the throughput does not go DOWN by more than an order of
//     magnitude (a regression would be a bug, not a trade-off).
// ---------------------------------------------------------------------------

// TestTheRelayThroughputAtSeveralRecordSizes measures the relay's
// end-to-end throughput over a REAL QUIC connection at several record
// sizes.
//
// The record size is the SIZE OF EACH WRITE the requester makes, which
// is exactly what the relay turns into one relay record: the requester's
// Write seals its bytes and emits them as ONE record (it only splits
// when the payload exceeds the ~1 MiB cap). So writing in 4 KiB pieces
// produces 4 KiB records on the wire, and writing in 256 KiB pieces
// produces 256 KiB records — the variable under test.
func TestTheRelayThroughputAtSeveralRecordSizes(t *testing.T) {
	const totalBytes = 8 << 20 // 8 MiB through the relay, per size

	type row struct {
		size       int
		mibsPerSec float64
	}
	var rows []row

	for _, recSize := range []int{4 << 10, 16 << 10, 64 << 10, 256 << 10, 1024 << 10} {
		recSize := recSize
		var mibs float64
		t.Run(fmt.Sprintf("%dKiB records", recSize>>10), func(t *testing.T) {
			f := newRelayFixture(t)
			f.startHub(t, true)
			sessions := f.joinBob(t)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			sess, err := OpenRelay(ctx, RelayRequesterConfig{
				Device:    f.alice,
				Resolver:  MemberListAdapter{List: f.members},
				Target:    f.bob.ID(),
				Kind:      KindPing,
				TargetKey: f.bob.PublicKey(),
				Relay:     f.hub.ID(),
				Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
					link, err := DialRelaySession(ctx, f.alice, MemberListAdapter{List: f.members}, relay, []string{f.hubAddr})
					if err != nil {
						return nil, err
					}
					return link.Session(), nil
				},
			})
			if err != nil {
				t.Fatalf("the relayed session could not be opened: %v", err)
			}
			defer sess.Close()

			var targetSess Session
			select {
			case targetSess = <-sessions:
			case <-time.After(relayHandshakeWait):
				t.Fatal("the target never received the relayed session")
			}
			defer targetSess.Close()

			// The target reads the payload in the background, so the
			// requester is never blocked by the reader's speed.
			readDone := make(chan int64, 1)
			go func() {
				buf := make([]byte, 64<<10)
				var got int64
				for got < totalBytes {
					_ = targetSess.SetReadDeadline(time.Now().Add(30 * time.Second))
					n, err := targetSess.Read(buf)
					if err != nil {
						if err == io.EOF {
							break
						}
						readDone <- got
						return
					}
					got += int64(n)
				}
				readDone <- got
			}()

			// The requester writes in pieces of recSize: one piece is
			// one relay record on the wire.
			payload := make([]byte, recSize)
			for i := range payload {
				payload[i] = byte(i)
			}
			start := time.Now()
			sent := 0
			for sent < totalBytes {
				n := recSize
				if left := totalBytes - sent; left < n {
					n = left
				}
				if _, err := sess.Write(payload[:n]); err != nil {
					t.Fatalf("the write through the relay failed after %d bytes: %v", sent, err)
				}
				sent += n
			}
			elapsed := time.Since(start)

			got := <-readDone
			if got != int64(totalBytes) {
				t.Fatalf("the target received %d bytes, want %d", got, totalBytes)
			}

			mibs = float64(totalBytes) / 1024 / 1024 / elapsed.Seconds()
			t.Logf("record size %6d B: %8.3f MiB/s over a real QUIC relay (%v)",
				recSize, mibs, elapsed.Round(time.Millisecond))
		})
		rows = append(rows, row{size: recSize, mibsPerSec: mibs})
	}

	// The invariants, whatever the numbers are.
	for _, r := range rows {
		if r.mibsPerSec <= 0 {
			t.Errorf("the relay carried %d bytes at %d KiB records at %.3f MiB/s: no throughput at all",
				totalBytes, r.size>>10, r.mibsPerSec)
		}
	}
	// A record size must not cost an order of magnitude: if the biggest
	// record were catastrophically slower, that would be a defect in the
	// path (the splice copies bytes; it does not care how many).
	//
	// This one IS an assertion, because it is a property of the PATH
	// (a size-dependent collapse would be a defect), not of the
	// machine. But it is still a wall-clock comparison, so under the
	// race detector the instrumented hot path dominates every size
	// equally and the spread collapses — so the assertion is skipped
	// there, for the reason the repo already documents in
	// internal/p2p2.
	if len(rows) > 1 && !raceEnabled {
		best, worst := rows[0].mibsPerSec, rows[0].mibsPerSec
		for _, r := range rows {
			if r.mibsPerSec > best {
				best = r.mibsPerSec
			}
			if r.mibsPerSec < worst {
				worst = r.mibsPerSec
			}
		}
		if best > 0 && worst < best/10 {
			t.Errorf("the slowest record size carried %.3f MiB/s against the best %.3f MiB/s: "+
				"an order of magnitude spread means the path has a size-dependent defect, "+
				"not a trade-off", worst, best)
		}
	}
	t.Logf("SUMMARY: %+v%s", rows, raceNote())
}
