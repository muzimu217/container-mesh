package mesh

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// TestTheTargetSideReportsWhyASessionFailed is the regression test for
// the gap the real-machine verification found.
//
// The target's side of a relayed session used to throw the handler's
// error away: `_ = cfg.OnSession(tsess, open.From, open.Kind)`.
//
// That made a failed transfer INVISIBLE on the receiving side. Measured
// on the real machine (175, worktree build): a transfer interrupted
// mid-way deleted its partial file and delivered nothing — correctly —
// but the receiver printed NOTHING about why. The reason existed only
// in the error the handler returned, and that error was written to the
// PEER (the sender), which may be exactly the party that died.
//
// So an operator looking at the receiver saw "the file is not there"
// with no account of it.
//
// The property under test is the report's CONTENT, asserted through the
// pure function that produces the text. It is deliberately NOT asserted
// by capturing stderr: replacing a package-level sink (or os.Stderr)
// races with the background goroutines that write to it, and the race
// detector caught exactly that — the -race run of this package failed
// intermittently with the victim rotating between tests.

func TestTheTargetSideReportsWhyASessionFailed(t *testing.T) {
	handlerErr := errors.New("sha256 mismatch or short read")
	const requester = "abc123def456"

	line := relaySessionFailureLine(requester, "file", handlerErr)
	if line == "" {
		t.Fatal("a failed relayed session produced NO report: the receiver's log " +
			"must account for why the file never arrived")
	}
	if !strings.Contains(line, "failed") {
		t.Fatalf("the report %q does not say the session failed", line)
	}
	if !strings.Contains(line, requester) {
		t.Fatalf("the report %q does not name the requester %s: an operator must "+
			"be able to match it with the requester's own report", line, requester)
	}
	if !strings.Contains(line, "file") {
		t.Fatalf("the report %q does not name the session kind", line)
	}
}

// TestTheReportDoesNotCarryThePayload is the other half.
//
// The handler's error carries the data it handled — the same text
// writeMsgFailure sends to the peer. The report goes to the TRUNK'S OWN
// LOG, which is a different trust domain from the peer's stream, so
// quoting the error verbatim would carry the payload across that
// boundary: a message body ending up in the anchor's log file.
//
// So the report says WHAT KIND of failure it was, and the peer still
// receives the detailed reason (the handler's own job).
func TestTheReportDoesNotCarryThePayload(t *testing.T) {
	const secret = "the-attacker-tail"
	handlerErr := errors.New("failed: " + secret)

	line := relaySessionFailureLine("abc123def456", "file", handlerErr)
	if strings.Contains(line, secret) {
		t.Fatalf("the report carried the handler's text into the log: %q", line)
	}
	if !strings.Contains(line, "failed") {
		t.Fatalf("the report %q does not say the session failed", line)
	}
}

// TestASuccessfulSessionIsNotReportedAsAFailure is the other half of
// the first property: the report must be about FAILURES only. A member
// that relays hundreds of sessions must not produce a line per session.
func TestASuccessfulSessionIsNotReportedAsAFailure(t *testing.T) {
	line := relaySessionFailureLine("abc123def456", "ping", nil)
	if line != "" {
		t.Fatalf("a SUCCESSFUL relayed session produced a report: %q", line)
	}
}

// TestTheFailureBeforeTheHandlerIsAlsoReported pins the gap the
// adversarial review found.
//
// serveRelayedSession has five failure points, and the report used to
// cover only the fifth (the handler's error). The first four are
// BEFORE the handler: a truncated open record, a wrong record type, an
// unresolvable requester, and a failed end-to-end handshake.
//
// Those are precisely the "truncated or interrupted transfer invisible
// on the receiving side" cases the report exists for — and a TRUNCATED
// OPEN RECORD is the most likely truncation in the protocol, because
// it is the first thing on the wire.
func TestTheFailureBeforeTheHandlerIsAlsoReported(t *testing.T) {
	f := newTrunkFixture(t)

	// A truncated open record: the header promises bytes that never
	// come.
	trunc := newTruncationPipe()

	line := captureRelayLog(t, func() {
		serveRelayedSession(trunc, RelayJoinConfig{
			Device:   f.bob,
			Resolver: MemberListAdapter{List: f.members},
		})
	})
	if !strings.Contains(line, "failed") {
		t.Fatalf("a TRUNCATED open record produced NO local report: %q", line)
	}
}

// captureRelayLog collects the trunk's reports WITHOUT racing.
//
// The reports go to the package-level relayLog, which is held in an
// atomic precisely so that replacing it here is not a data race with
// the background goroutines that write to it. The race detector caught
// the earlier plain-variable version: the -race run of this package
// failed intermittently, with the victim rotating between tests.
func captureRelayLog(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := relayLogWriter()
	relayLog.Store(io.Writer(w))
	done := make(chan string, 1)
	go func() {
		var buf strings.Builder
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	out := <-done
	_ = r.Close()
	relayLog.Store(old)
	return out
}
