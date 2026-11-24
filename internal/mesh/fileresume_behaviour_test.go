// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ THE RESUME MUST ACTUALLY WORK — AND THE SOURCE ASSERTIONS CANNOT
// SEE THAT.
//
// The source-level assertions in fileresume_test.go pin that the
// protocol CARRIES an offset. They cannot see whether the offset is
// used, whether the receiver keeps what it has, or whether a second
// attempt resumes rather than restarts.
//
// So this file drives the receive path the way the listener does, and
// interrupts a transfer the way a dropped link does.

// deliverPartial runs the receive path with a TRUNCATED body — the
// shape of a transfer whose link died halfway.
//
// It returns the receiver's reply and the error, so a test can see
// both what the receiver said and what it left on disk.
func deliverPartial(t *testing.T, inbox, name string, body []byte, sendOnly int) (string, error) {
	t.Helper()
	sum := sha256.Sum256(body)
	h := FileHeader{
		Name:   name,
		Size:   int64(len(body)),
		SHA256: fmt.Sprintf("%x", sum[:]),
	}
	hdr, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}

	var stream bytes.Buffer
	stream.Write(hdr)
	stream.WriteByte('\n')
	if sendOnly > len(body) {
		sendOnly = len(body)
	}
	stream.Write(body[:sendOnly])

	st := &fileTestSession{}
	br := bufio.NewReader(&stream)
	err = receiveFile("sender", br, st, ListenerConfig{InboxDir: inbox})
	return st.String(), err
}

// ★ AN INTERRUPTED TRANSFER MUST LEAVE THE PARTIAL FILE BEHIND.
//
// This is the whole point: the partial file is what the next attempt
// resumes from. Before the fix the receiver wrote to a throwaway temp
// file and deleted it on ANY failure — so a dropped link threw away
// every byte it had received.
func TestAnInterruptedTransferLeavesThePartFileBehind(t *testing.T) {
	inbox := t.TempDir()
	body := []byte("the first half of a file that will be interrupted")

	// Send only part of it: the link died.
	reply, err := deliverPartial(t, inbox, "big.bin", body, 10)
	if err == nil {
		t.Fatalf("the truncated delivery SUCCEEDED (reply %q): a short body "+
			"was accepted as a whole file", reply)
	}

	part := filepath.Join(inbox, "big.bin.part")
	info, statErr := os.Stat(part)
	if statErr != nil {
		t.Fatalf("the partial file is gone (%v); there is nothing to resume from", statErr)
	}
	if info.Size() == 0 {
		t.Fatalf("the partial file is empty")
	}
	if info.Size() > int64(len(body)) {
		t.Fatalf("the partial file is %d bytes, longer than the %d-byte file",
			info.Size(), len(body))
	}

	// ★ AND THE FINISHED FILE MUST NOT BE THERE. A partial transfer
	// that left a complete-looking file behind would be worse than
	// none.
	if _, err := os.Stat(filepath.Join(inbox, "big.bin")); err == nil {
		t.Fatalf("the interrupted transfer left a finished file behind")
	}
}

// ★ THE RECEIVER MUST SAY HOW MUCH IT ALREADY HAS.
//
// The ack is what tells the sender where to resume. It has to carry
// the count, or the sender cannot know.
func TestTheReceiverSaysHowMuchItAlreadyHas(t *testing.T) {
	inbox := t.TempDir()
	body := []byte("0123456789")

	// One interrupted attempt with 4 bytes.
	if _, err := deliverPartial(t, inbox, "r.bin", body, 4); err == nil {
		t.Fatal("the truncated delivery succeeded")
	}
	part := filepath.Join(inbox, "r.bin.part")
	info, err := os.Stat(part)
	if err != nil {
		t.Fatalf("no partial file: %v", err)
	}

	// Now a fresh attempt: the receiver must answer with the offset it
	// already holds.
	reply, err := deliverFull(t, inbox, "r.bin", body)
	if err != nil {
		t.Logf("the resumed delivery failed: %v (reply %q)", err, reply)
	}
	// The ack is the first line of the reply.
	first := strings.TrimSpace(strings.SplitN(reply, "\n", 2)[0])
	if !strings.HasPrefix(first, "ok") {
		t.Fatalf("the reply does not start with an ack: %q", first)
	}
	if !strings.Contains(first, "4") {
		t.Fatalf("the ack does not carry the offset the receiver holds: %q "+
			"(the receiver has %d bytes)", first, info.Size())
	}
}

// deliverFull runs the receive path with the WHOLE body — the shape of
// a first attempt, or of a sender that does not know about a resume.
//
// A RESUMED attempt sends only what is missing, so it has its own
// helper: deliverFrom.
func deliverFull(t *testing.T, inbox, name string, body []byte) (string, error) {
	t.Helper()
	return deliverFrom(t, inbox, name, body, body)
}

// deliverFrom runs the receive path sending only the bytes from
// `send` onward — what a resuming sender does.
func deliverFrom(t *testing.T, inbox, name string, body, send []byte) (string, error) {
	t.Helper()
	sum := sha256.Sum256(body)
	h := FileHeader{
		Name:   name,
		Size:   int64(len(body)),
		SHA256: fmt.Sprintf("%x", sum[:]),
	}
	hdr, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	stream.Write(hdr)
	stream.WriteByte('\n')
	stream.Write(send)

	st := &fileTestSession{}
	br := bufio.NewReader(&stream)
	err = receiveFile("sender", br, st, ListenerConfig{InboxDir: inbox})
	return st.String(), err
}

// ★ AND A RESUMED TRANSFER MUST DELIVER THE WHOLE FILE, VERIFIED.
//
// The bytes already on disk are hashed too, so the whole-file hash
// still holds — which is what makes a stale or corrupt prefix unable
// to survive.
func TestAResumedTransferDeliversTheWholeFileVerified(t *testing.T) {
	inbox := t.TempDir()
	body := []byte("the first half | the second half, delivered across two attempts")

	if _, err := deliverPartial(t, inbox, "whole.bin", body, 15); err == nil {
		t.Fatal("the truncated delivery succeeded")
	}
	// ★ THE RESUMING ATTEMPT SENDS ONLY WHAT IS MISSING. The receiver
	// already holds the first 15 bytes, so it hashes them and expects
	// the rest — which is what makes the whole-file hash still hold.
	reply, err := deliverFrom(t, inbox, "whole.bin", body, body[15:])
	if err != nil {
		t.Fatalf("the resumed delivery failed: %v (reply %q)", err, reply)
	}
	if !strings.Contains(reply, "done ") {
		t.Fatalf("the resumed delivery did not report success: %q", reply)
	}

	got, err := os.ReadFile(filepath.Join(inbox, "whole.bin"))
	if err != nil {
		t.Fatalf("the file was not delivered: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("the delivered file is %q, want %q", got, body)
	}
	// And the partial file must be gone: the transfer is finished.
	if _, err := os.Stat(filepath.Join(inbox, "whole.bin.part")); err == nil {
		t.Fatalf("the partial file survived a completed transfer")
	}
}

// ★ AND A RESUME WITH A WRONG PREFIX MUST BE REFUSED, NOT ACCEPTED.
//
// The receiver keeps a prefix it has verified only at the END of the
// transfer. If the file being sent is NOT the one the prefix belongs
// to, the whole-file hash will not match — and the partial file must
// be dropped rather than kept, or every later attempt would fail on
// the same bad prefix.
func TestAResumeAgainstAWrongFileIsRefused(t *testing.T) {
	inbox := t.TempDir()
	original := []byte("the ORIGINAL file, interrupted halfway")
	other := []byte("a COMPLETELY DIFFERENT file with the same name")

	if _, err := deliverPartial(t, inbox, "x.bin", original, 12); err == nil {
		t.Fatal("the truncated delivery succeeded")
	}
	if _, err := os.Stat(filepath.Join(inbox, "x.bin.part")); err != nil {
		t.Fatalf("no partial file to test against: %v", err)
	}

	// Now a DIFFERENT file with the same name arrives.
	reply, err := deliverFull(t, inbox, "x.bin", other)
	if err == nil {
		t.Fatalf("the delivery of a different file SUCCEEDED (reply %q): "+
			"the stale prefix was accepted", reply)
	}
	// And the bad prefix must be gone: it would fail every later
	// attempt.
	if _, statErr := os.Stat(filepath.Join(inbox, "x.bin.part")); statErr == nil {
		t.Fatalf("the mismatching partial file survived; every later " +
			"attempt would fail on it")
	}
}

// ★ THE .part SUFFIX MUST BE REFUSED AS A FILE NAME.
//
// The receiver's own bookkeeping uses that suffix. A sender that could
// name a file "x.part" could write into it.
func TestTheReceiverRefusesAFileNamedPart(t *testing.T) {
	inbox := t.TempDir()
	reply, err := deliverFull(t, inbox, "trap.part", []byte("x"))
	if err == nil {
		t.Fatalf("a file named \"trap.part\" was ACCEPTED (reply %q): the "+
			"sender could write into the receiver's own bookkeeping", reply)
	}
	if !strings.Contains(reply, "part") {
		t.Fatalf("the refusal does not say why: %q", reply)
	}
}
