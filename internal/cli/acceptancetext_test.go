package cli

import (
	"os"
	"strings"
	"testing"
)

// ★ THE ACCEPTANCE LIST'S OUTPUT CONTRACTS, ASSERTED AT THE SOURCE.
//
// The M4 acceptance list names six cells (B5..B10) whose judgement is
// the TEXT THE OPERATOR READS:
//
//   B5  msg      → "delivered N message(s) to <id>"
//   B6  send-to  → "sent <file> (N bytes) to <id>" AND "receiver verified"
//   B7  run      → the lines hostname / os / version / go
//   B9  bench    → "direction=", "sent=", "received="
//   B10 bench    → the non-aligned sizes 4095/4097/65537/1048577/33554431
//
// NONE of them had an automated assertion. So changing "delivered" to
// "delivered!" — or recording bench's sent count as received — would
// have left CI green while the acceptance item silently stopped
// holding.
//
// ★ WHY THESE ASSERT ON THE SOURCE RATHER THAN THE RUNTIME.
//
// The commands assemble their output inline, deep inside functions that
// need a live mesh session. Asserting on the source is what lets the
// contract be pinned at all — and it means this file compiles against a
// tree that predates the assertions, so it can show that the contract
// was UNGUARDED rather than merely that it holds today.
//
// The runtime behaviour of these commands IS exercised elsewhere (the
// mesh layer has tests for msg delivery, run @cat at several sizes and
// a bidirectional bench). What was missing was the thing the operator
// actually reads.
//
// These cells were never BROKEN. They were UNGUARDED — a different
// defect from a red-to-green fix, and the two must not be confused.

// commandSource reads a source file of the package under test.
func commandSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// meshSource reads a source file of the sibling mesh package.
func meshSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../mesh/" + name)
	if err != nil {
		t.Fatalf("read ../mesh/%s: %v", name, err)
	}
	return string(b)
}

// ★ B5: `msg` MUST SAY HOW MANY MESSAGES IT DELIVERED, AND TO WHOM.
func TestTheMsgCommandReportsWhatItDelivered(t *testing.T) {
	src := commandSource(t, "meshcmd.go")
	if !strings.Contains(src, "delivered %d message(s) to %s") {
		t.Fatal("the msg command no longer reports \"delivered N message(s) to <id>\"; " +
			"that line is the whole evidence for the B5 acceptance cell")
	}
}

// ★ B6: `send-to` MUST SAY THE RECEIVER VERIFIED THE BYTES.
//
// The receiver's own sha256 is the only thing that distinguishes "the
// bytes arrived" from "the bytes arrived intact" — and a transfer that
// arrives corrupted looks exactly like one that arrived.
func TestTheSendToCommandReportsTheReceiverVerifiedIt(t *testing.T) {
	src := commandSource(t, "meshcmd.go")
	if !strings.Contains(src, "receiver verified") {
		t.Fatal("the send-to command no longer says the receiver verified the bytes; " +
			"that is the whole evidence for the B6 acceptance cell")
	}
	if !strings.Contains(src, "sent %s (%d bytes) to %s") {
		t.Fatal("the send-to command no longer reports the file and its size; " +
			"the B6 acceptance cell names both")
	}
}

// ★ B7: `run @info` MUST PRINT THE LINES THE ACCEPTANCE LIST NAMES.
//
// The list says: hostname, os, version, go. The five lines are
// assembled in the mesh package (internal/mesh/exec.go) and travel
// through the CLI untouched — so the contract is asserted where it is
// built, and the CLI's job is to carry it.
func TestTheRunCommandPrintsTheAcceptanceLines(t *testing.T) {
	src := meshSource(t, "exec.go")
	for _, want := range []string{"hostname", "os ", "version", "go "} {
		if !strings.Contains(src, want) {
			t.Fatalf("the run output no longer carries %q; "+
				"the B7 acceptance cell names it", want)
		}
	}
}

// ★ B9: `bench` MUST REPORT A DIRECTION AND BOTH COUNTS.
//
// "direction=both" and a sent count equal to the received count are
// the acceptance cell. A bench that reported only one direction would
// still print numbers.
func TestTheBenchCommandReportsBothDirections(t *testing.T) {
	src := commandSource(t, "bench.go")
	for _, want := range []string{"direction=", "sent=", "received="} {
		if !strings.Contains(src, want) {
			t.Fatalf("the bench command no longer reports %q; "+
				"the B9 acceptance cell names it", want)
		}
	}
}

// ★ B10: THE NON-ALIGNED SIZES MUST BE EXERCISED.
//
// The acceptance cell lists 4095 / 4097 / 65537 / 1048577 / 33554431 as
// the sizes that must survive a transfer intact. Until they appear in
// a test, "the non-aligned sizes work" is a claim with nothing behind
// it.
//
// The audit found 4095 and 65537 in a relay-FRAME test (a different
// thing: the frame codec's limits) and NOTHING for the rest — and no
// bench test at all covered the list. So this is the cell that needs
// the work.
func TestTheNonAlignedSizesAreExercisedByABenchTest(t *testing.T) {
	want := []string{"4095", "4097", "65537", "1048577", "33554431"}

	// The mesh package's bench tests are where a size table lives.
	candidates := []string{
		"../mesh/bench_sizes_test.go",
		"../mesh/bench_test.go",
		"../mesh/relaysize_measure_test.go",
		"../mesh/relayquota_measure_test.go",
	}
	var missing []string
	for _, w := range want {
		found := false
		for _, name := range candidates {
			b, err := os.ReadFile(name)
			if err != nil {
				continue
			}
			if strings.Contains(string(b), w) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the non-aligned sizes %v are not exercised by any bench test; "+
			"the B10 acceptance cell names exactly those", missing)
	}
}
