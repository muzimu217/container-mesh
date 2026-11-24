// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ★ B9 AT THE RUNTIME: THE BENCH MUST REPORT A DIRECTION AND BOTH
// COUNTS, AND THE COUNTS MUST MATCH.
//
// The acceptance cell says: "bench 双向：输出含 direction=both，且
// sent == received".
//
// The source-level assertion (in acceptancetext_test.go) pins the
// WORDING. This one runs the command and checks the NUMBERS — because
// a bench that prints "sent=100 received=0" satisfies the wording and
// fails the cell.
//
// ★ AND THE COUNTS MUST EQUAL THE SIZE THAT WAS ASKED FOR.
//
// "sent == received" alone would be satisfied by a bench that moved
// nothing at all (0 == 0). The size was requested, so the size is
// what has to be there.

// benchLine is the line the bench prints with its counts.
var benchCountsLine = regexp.MustCompile(`direction=(\S+) size_each=(\d+) sent=(\d+) received=(\d+)`)

// TestTheBenchReportsBothDirectionsAndMatchingCounts runs the bench
// and checks the line an operator reads.
func TestTheBenchReportsBothDirectionsAndMatchingCounts(t *testing.T) {
	f := newCLIRelayFixture(t)

	out := captureStdout(t, func() {
		if err := runBench([]string{
			"--state", f.joinerDir,
			"--device-passphrase", f.pass,
			"--size", "64KB",
			f.ownerDev.String(),
		}); err != nil {
			t.Fatalf("bench: %v", err)
		}
	})

	m := benchCountsLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the bench did not print a counts line:\n%s", out)
	}
	if m[1] != "both" {
		t.Fatalf("the bench reports direction=%q, want \"both\"", m[1])
	}
	sizeEach, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		t.Fatalf("size_each %q is not a number: %v", m[2], err)
	}
	sent, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		t.Fatalf("sent %q is not a number: %v", m[3], err)
	}
	received, err := strconv.ParseInt(m[4], 10, 64)
	if err != nil {
		t.Fatalf("received %q is not a number: %v", m[4], err)
	}
	if sent != received {
		t.Fatalf("sent %d, received %d — the cell requires them to be equal", sent, received)
	}
	if sent != sizeEach {
		t.Fatalf("sent %d, want the %d bytes that were asked for", sent, sizeEach)
	}
	if sizeEach == 0 {
		t.Fatalf("the bench moved nothing")
	}
}

// ★ AND THE BENCH MUST NOT STALL.
//
// The bench is the throughput tool: a run that hangs is a failed run,
// whatever the numbers say. This is the same magnitude-of-degradation
// shape the mesh package's guards use (T-2.5): a healthy loopback
// bench of a few hundred kilobytes is milliseconds, and the budget is
// seconds.
func TestTheBenchDoesNotStall(t *testing.T) {
	f := newCLIRelayFixture(t)

	started := time.Now()
	out := captureStdout(t, func() {
		if err := runBench([]string{
			"--state", f.joinerDir,
			"--device-passphrase", f.pass,
			"--size", "64KB",
			f.ownerDev.String(),
		}); err != nil {
			t.Fatalf("bench: %v", err)
		}
	})
	elapsed := time.Since(started)

	if elapsed >= 10*time.Second {
		t.Fatalf("the bench STALLED: 64 KB took %s", elapsed)
	}
	// And it must have printed the counts, not just finished.
	if !strings.Contains(out, "direction=") {
		t.Fatalf("the bench printed no counts line:\n%s", out)
	}
}
