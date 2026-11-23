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
	"time"
)

// ---------------------------------------------------------------------------
// THE RECEIVER MUST NOT SILENTLY REPLACE A FILE. (Round 99.)
//
// `send-to` used to rename the finished file over whatever was in the
// inbox with the same name. So:
//
//   1. a member sent "report.pdf" → it arrived;
//   2. somebody sent "report.pdf" again → THE FIRST ONE WAS GONE,
//      and the sender was told "done <hash>" both times.
//
// Nothing said a word. The sender's success message was true — the
// second file did arrive, verified — and the first file had silently
// disappeared.
//
// The stage-1 path has had the right rule for a while
// (publishNoClobber: link-then-remove gives atomic "create if absent"
// semantics, and the destination existing is a REFUSAL). The mesh line
// is a separate implementation and did not carry that rule over.
//
// So the receiver refuses to replace, and says so — to the SENDER, over
// the stream, because the sender is waiting for a verdict and a local
// error would leave it blocked until its timeout.
// ---------------------------------------------------------------------------

// fileTestSession is a Session that records what the receiver wrote back.
//
// The receiver answers over the stream: "ok", then either "done <hash>"
// or "err: <reason>". This captures that.
type fileTestSession struct {
	bytes.Buffer
}

func (s *fileTestSession) Read(p []byte) (int, error) { return 0, os.ErrClosed }
func (s *fileTestSession) Close() error               { return nil }
func (s *fileTestSession) SetReadDeadline(t time.Time) error {
	return nil
}
func (s *fileTestSession) SetWriteDeadline(t time.Time) error { return nil }

// deliverFile runs the receive path for one file, the way the listener
// does: the header line, then the body.
func deliverFile(t *testing.T, inbox, name string, body []byte, force bool) (string, error) {
	t.Helper()
	sum := sha256.Sum256(body)
	h := FileHeader{
		Name:   name,
		Size:   int64(len(body)),
		SHA256: fmt.Sprintf("%x", sum[:]),
		Force:  force,
	}
	hdr, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}

	// The receiver reads the header LINE, then the body.
	var stream bytes.Buffer
	stream.Write(hdr)
	stream.WriteByte('\n')
	stream.Write(body)

	st := &fileTestSession{}
	br := bufio.NewReader(&stream)
	err = receiveFile("sender", br, st, ListenerConfig{InboxDir: inbox})
	return st.String(), err
}

// TestTheReceiverRefusesToReplaceAFile is the regression test.
//
// The first delivery succeeds. The second one — the same name, without
// the sender asking for a replacement — must be REFUSED, and the file
// that is there must be the FIRST one.
func TestTheReceiverRefusesToReplaceAFile(t *testing.T) {
	inbox := t.TempDir()

	reply, err := deliverFile(t, inbox, "report.pdf", []byte("the FIRST file"), false)
	if err != nil {
		t.Fatalf("the first delivery failed: %v (%s)", err, reply)
	}
	if !strings.Contains(reply, "done ") {
		t.Fatalf("the first delivery did not report success: %q", reply)
	}

	saved := filepath.Join(inbox, "report.pdf")
	first, err := os.ReadFile(saved)
	if err != nil {
		t.Fatalf("the first file is not in the inbox: %v", err)
	}
	if string(first) != "the FIRST file" {
		t.Fatalf("the inbox holds %q, want the first file", first)
	}

	// The SECOND delivery, same name, no force.
	reply, err = deliverFile(t, inbox, "report.pdf", []byte("the SECOND file"), false)
	if err == nil {
		t.Fatalf("the second delivery SUCCEEDED (reply %q): the receiver "+
			"silently replaced a file that was already there", reply)
	}
	if !strings.Contains(reply, "err:") {
		t.Fatalf("the refusal was not reported to the sender: %q", reply)
	}

	// AND THE FIRST FILE MUST STILL BE THERE, with its original content.
	after, err := os.ReadFile(saved)
	if err != nil {
		t.Fatalf("the first file is gone after the refused delivery: %v", err)
	}
	if string(after) != "the FIRST file" {
		t.Fatalf("the inbox now holds %q: the refused delivery still replaced the file", after)
	}
}

// TestTheReceiverReplacesWhenTheSenderAsks is the other half.
//
// A refusal with no way to proceed is not a fix, it is an obstruction.
// The sender can ask for the replacement explicitly, and then it happens.
func TestTheReceiverReplacesWhenTheSenderAsks(t *testing.T) {
	inbox := t.TempDir()

	if _, err := deliverFile(t, inbox, "report.pdf", []byte("the FIRST file"), false); err != nil {
		t.Fatalf("the first delivery failed: %v", err)
	}

	reply, err := deliverFile(t, inbox, "report.pdf", []byte("the SECOND file"), true)
	if err != nil {
		t.Fatalf("the forced delivery failed: %v (%s)", err, reply)
	}
	if !strings.Contains(reply, "done ") {
		t.Fatalf("the forced delivery did not report success: %q", reply)
	}

	after, err := os.ReadFile(filepath.Join(inbox, "report.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "the SECOND file" {
		t.Fatalf("the inbox holds %q after a forced delivery, want the second file", after)
	}
}
