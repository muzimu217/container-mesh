// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// ★ B5 / B6 / B7 AT THE RUNTIME, NOT JUST AT THE SOURCE.
//
// The source-text assertions in acceptancetext_test.go pin the OUTPUT
// CONTRACT — the wording the acceptance list names. These tests pin
// that the contract is actually produced by running the command.
//
// Both halves are needed, and they fail differently:
//
//   * the source assertion fails when the WORDING changes;
//   * this one fails when the wording stops being printed AT ALL —
//     because the command takes an early exit, writes to the wrong
//     stream, or the delivery silently stops happening.
//
// ★ AND FOR B6 THE RECEIVER'S OWN BYTES ARE CHECKED.
//
// "Sent" and "sent intact" are different claims. The sender's report
// is the sender's claim about itself; the receiver's inbox holds the
// fact. So the test reads the RECEIVER's inbox directory and compares
// the bytes.
//
// (The receiving side of a msg is a LISTENING member, so its evidence
// is the callback that fires when the message arrives. The hook is the
// seam the package already uses for this kind of observation
// (joinResolverHook, joinLifetimeHook, discoveryClientTransportHook):
// nil in production, set by tests.)

// ★ B5: `msg` MUST REPORT THE DELIVERY, AND THE MESSAGE MUST REALLY
// HAVE ARRIVED.
//
// The sender's own line ("delivered N message(s) to X") is the
// sender's CLAIM about itself. A command that printed it while
// delivering nothing would satisfy every other check.
//
// So this test checks both sides: the sender's report, and the
// RECEIVER's arrival.
func TestMsgReportsADeliveryThatActuallyArrived(t *testing.T) {
	f := newCLIRelayFixture(t)

	// The receiver's side of the fact.
	arrived := make(chan string, 4)
	old := inboundMessageHook
	inboundMessageHook = func(from identity.DeviceID, text string) {
		select {
		case arrived <- text:
		default:
		}
	}
	defer func() { inboundMessageHook = old }()

	out := captureStderrFor(t, func() {
		if err := runMsg([]string{
			"--state", f.joinerDir,
			"--device-passphrase", f.pass,
			"--text", "acceptance-b5",
			f.ownerDev.String(),
		}); err != nil {
			t.Fatalf("msg: %v", err)
		}
	})

	// The acceptance cell names the wording.
	if !strings.Contains(out, "delivered 1 message(s) to "+f.ownerDev.String()) {
		t.Fatalf("msg did not report the delivery:\n%s", out)
	}

	// ★ AND THE MESSAGE MUST HAVE ARRIVED AT THE TARGET.
	select {
	case text := <-arrived:
		if !strings.Contains(text, "acceptance-b5") {
			t.Fatalf("the target received %q, want the message that was sent", text)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the message was reported as delivered but the target never " +
			"received it")
	}
}

// ★ B6: `send-to` MUST REPORT THE RECEIVER'S VERIFICATION, AND THE
// RECEIVER MUST HAVE THE RIGHT BYTES.
//
// The acceptance cell names two things: the file and its size, and the
// receiver's verification. The second is the one that matters —
// "delivered" and "delivered intact" are different claims, and only the
// receiver's own sha256 can tell them apart.
func TestSendToReportsTheReceiverVerifiedTheBytes(t *testing.T) {
	f := newCLIRelayFixture(t)

	// A payload whose content matters.
	payload := []byte("acceptance-b6 payload - the bytes must arrive intact\n")
	src := filepath.Join(t.TempDir(), "b6.txt")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStderrFor(t, func() {
		if err := runSendTo([]string{
			"--state", f.joinerDir,
			"--device-passphrase", f.pass,
			"--file", src,
			f.ownerDev.String(),
		}); err != nil {
			t.Fatalf("send-to: %v", err)
		}
	})

	if !strings.Contains(out, "receiver verified") {
		t.Fatalf("send-to did not report the receiver's verification:\n%s", out)
	}
	if !strings.Contains(out, "b6.txt") {
		t.Fatalf("send-to did not name the file it sent:\n%s", out)
	}
	if !strings.Contains(out, "bytes)") {
		t.Fatalf("send-to did not report the size it sent:\n%s", out)
	}

	// ★ AND THE RECEIVER MUST ACTUALLY HAVE THE BYTES.
	//
	// The receiver's inbox is a directory under the receiver's state
	// dir. The file lands there with the sender's name, and its
	// contents must be the bytes that were sent — not merely a file
	// of the right length.
	if !waitForInboxFile(t, f.ownerDir, "b6.txt", payload, 20*time.Second) {
		got := listInbox(t, f.ownerDir)
		t.Fatalf("the file was reported as sent and verified, but the receiver's "+
			"inbox does not hold those bytes (inbox: %v)", got)
	}
}

// ★ B7: `run @info` MUST PRINT THE FIVE LINES THE ACCEPTANCE LIST
// NAMES.
//
// The list says: hostname, os, version, go. A command that printed
// three of them would still "work"; the cell is about the exact set.
func TestRunInfoPrintsTheAcceptanceLines(t *testing.T) {
	f := newCLIRelayFixture(t)

	out := captureStdout(t, func() {
		// ★ `run` PROPAGATES THE REMOTE EXIT CODE, ssh-style — so a
		// successful run returns ExitError{Code: 0}. That is not a
		// failure, and a test that treats any error as a failure would
		// reject a perfectly good run.
		if err := runRun([]string{
			"--state", f.joinerDir,
			"--device-passphrase", f.pass,
			f.ownerDev.String(),
			"@info",
		}); err != nil {
			var ee ExitError
			if !errors.As(err, &ee) || ee.Code != 0 {
				t.Fatalf("run: %v", err)
			}
		}
	})

	for _, want := range []string{"hostname", "os", "version", "go"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the run output does not carry %q:\n%s", want, out)
		}
	}
}

// waitForInboxFile waits until the receiver's inbox holds a file with
// this name and exactly these bytes.
func waitForInboxFile(t *testing.T, stateDir, name string, want []byte, budget time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		p := filepath.Join(stateDir, "inbox", name)
		got, err := os.ReadFile(p)
		if err == nil && string(got) == string(want) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// listInbox names what the receiver's inbox actually holds, for the
// failure message — a test that says "it did not work" without saying
// what it did hold is a test that has to be re-run by hand.
func listInbox(t *testing.T, stateDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, "inbox"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
