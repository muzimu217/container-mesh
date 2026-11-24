package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ A WRITE THAT DID NOT PERSIST MUST NOT BE REPORTED AS DELIVERED.
//
// This is not hypothetical: on the authorized host the state file's
// ownership was changed by an operator's cleanup script, so every write
// failed with "permission denied" — and the hub answered {"ok":true}
// anyway. For two hours the coordination channel accepted every message
// and wrote none of them. The messages existed only in the serving
// process's memory, and a restart lost all of them.
//
// The lesson is the same one the channel already knows about the
// transport: {"ok":true} is not proof. It was not proof of delivery
// (the hub creates a phantom inbox for any "to=" value), and it is not
// proof of persistence either.
//
// A hub that cannot write MUST say so — with a status code that means
// "your message was accepted and stored" versus one that means "it was
// accepted and then lost". 200 for stored, 500 for lost.

// statePathFor points the hub's state file at a path the test controls.
func statePathFor(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state.json")
}

// writeUnwritableState makes the state's DIRECTORY unwritable, which
// is the only shape in which the atomic save cannot work: the save
// writes a fresh temp file in that directory and renames it into
// place, so it needs the DIRECTORY to be writable.
//
// ★ THE SHAPE FROM THE INCIDENT IS NOT THIS ONE. There, the state
// FILE was not writable by the service user but the directory was —
// and the atomic write succeeds and REPAIRS the ownership. That is
// covered by TestTheIncidentShapeIsRepairedRatherThanReported.
//
// A directory in place of the state file is the other unusable shape:
// the save's rename would fail with "file exists".
func writeUnwritableState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Read-only directory: nothing can be created or renamed in it.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return p
}

func TestAPostThatCouldNotBePersistedIsNotReportedAsDelivered(t *testing.T) {
	old := statePath
	statePath = writeUnwritableState(t)
	defer func() { statePath = old }()
	resetStore()

	r := httptest.NewRequest("POST", "/hub/report?from=t&topic=t", strings.NewReader("a report"))
	w := httptest.NewRecorder()
	postMsg(w, r)

	if w.Code == http.StatusOK {
		body, _ := json.Marshal(w.Body.String())
		t.Fatalf("the hub answered 200 even though the state could not be saved: %s", body)
	}
	// The body must say WHY, so an agent can tell "not stored" from
	// "not delivered".
	if !strings.Contains(strings.ToLower(w.Body.String()), "save") {
		t.Fatalf("body = %q, want it to say the save failed", w.Body.String())
	}
}

func TestAnInstructThatCouldNotBePersistedIsNotReportedAsDelivered(t *testing.T) {
	old := statePath
	statePath = writeUnwritableState(t)
	defer func() { statePath = old }()
	resetStore()

	r := httptest.NewRequest("POST", "/hub/instruct?to=somebody", strings.NewReader("an instruction"))
	w := httptest.NewRecorder()
	postInstruct(w, r)

	if w.Code == http.StatusOK {
		t.Fatalf("the hub answered 200 even though the state could not be saved: %q", w.Body.String())
	}
	if !strings.Contains(strings.ToLower(w.Body.String()), "save") {
		t.Fatalf("body = %q, want it to say the save failed", w.Body.String())
	}
}

// The converse: a normal post must still be 200 and must still be
// persisted. A fix that refuses everything is not a fix.
func TestAPostThatCanBePersistedIsAccepted(t *testing.T) {
	old := statePath
	statePath = filepath.Join(t.TempDir(), "state.json")
	defer func() { statePath = old }()
	resetStore()

	r := httptest.NewRequest("POST", "/hub/report?from=t&topic=t", strings.NewReader("a report"))
	w := httptest.NewRecorder()
	postMsg(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %q)", w.Code, w.Body.String())
	}
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("the state was not written: %v", err)
	}
	if !strings.Contains(string(b), "a report") {
		t.Fatalf("the state does not contain the message: %s", b)
	}
}

// ★ THE INCIDENT SHAPE IS REPAIRED, NOT REPORTED.
//
// The incident was an ownership change: a cleanup script rewrote
// state.json as root while the service runs as a dedicated low-
// privilege user. The file was readable but not writable, and the hub
// kept answering {"ok":true} while writing nothing — for two hours.
//
// The atomic save fixes this by construction: it never writes INTO
// statePath, it writes a fresh temp file in the same directory and
// renames it into place. The rename needs the DIRECTORY, not the old
// file, and the new file belongs to the process that wrote it — the
// service user.
//
// So this shape must now SUCCEED and leave the file owned by the
// writer. That is strictly better than reporting an error: the write
// works and the ownership is repaired by the same act.
func TestTheIncidentShapeIsRepairedRatherThanReported(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	// Readable, but not writable by its owner — the incident's shape.
	if err := os.WriteFile(p, []byte("{}"), 0o400); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	old := statePath
	statePath = p
	defer func() { statePath = old }()
	resetStore()

	r := httptest.NewRequest("POST", "/hub/report?from=t&topic=t", strings.NewReader("a report"))
	w := httptest.NewRecorder()
	postMsg(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("the write failed on a state that the atomic save can repair: %q", w.Body.String())
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o200 == 0 {
		t.Fatalf("the state file is still not writable by its owner: mode %v", info.Mode().Perm())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "a report") {
		t.Fatalf("the state does not contain the message: %s", b)
	}
}

// The converse: the save must not be able to leave a file the service
// cannot write again.
func TestTheStateFileIsWrittenAtomicallyAndKeepsTheServiceOwnership(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	old := statePath
	statePath = p
	defer func() { statePath = old }()
	resetStore()

	// A pre-existing file owned by somebody else, with a mode that does
	// not allow the writer to write it. This is the state the incident
	// left behind.
	if err := os.WriteFile(p, []byte("{}"), 0o400); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	r := httptest.NewRequest("POST", "/hub/report?from=t&topic=t", strings.NewReader("a report"))
	w := httptest.NewRecorder()
	postMsg(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %q)", w.Code, w.Body.String())
	}

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o200 == 0 {
		t.Fatalf("the state file is not writable by its owner: mode %v", info.Mode().Perm())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "a report") {
		t.Fatalf("the state does not contain the message: %s", b)
	}
}

// ★ THE HUB MUST NOT START IF IT CANNOT WRITE ITS STATE.
//
// A hub that accepts messages and loses them is worse than a hub that
// is down: the down one is visible. On the authorized host the state
// file was left readable but unwritable, and the hub ran for two hours
// answering {"ok":true} while every write failed.
//
// The check has to be a WRITE, not a permission test: a file can be
// readable and still not writable (a directory in its place, a
// read-only directory, a full filesystem).
func TestTheStartupCheckRejectsAnUnwritableState(t *testing.T) {
	old := statePath
	statePath = writeUnwritableState(t)
	defer func() { statePath = old }()

	if err := checkStateWritable(); err == nil {
		t.Fatal("the startup check passed on a state that cannot be written")
	}
}

func TestTheStartupCheckAcceptsAWritableState(t *testing.T) {
	old := statePath
	statePath = filepath.Join(t.TempDir(), "state.json")
	defer func() { statePath = old }()

	if err := checkStateWritable(); err != nil {
		t.Fatalf("the startup check rejected a writable state: %v", err)
	}
}

// ★ THE STARTUP CHECK MUST NOT DESTROY THE STATE IT CHECKS.
//
// The first version of the check renamed its probe file ONTO
// statePath — so every start of the hub replaced the whole channel
// history with the two bytes "{}". It was found because the state file
// came back as 159 bytes.
//
// The check has to leave statePath exactly as it found it.
func TestTheStartupCheckLeavesTheStateFileAlone(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	original := []byte(`{"topics":{"t":[{"time":"x","body":"the original history"}]}}`)
	if err := os.WriteFile(p, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	old := statePath
	statePath = p
	defer func() { statePath = old }()

	if err := checkStateWritable(); err != nil {
		t.Fatalf("the startup check failed on a writable state: %v", err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("the state file is gone: %v", err)
	}
	if string(after) != string(original) {
		t.Fatalf("the startup check REWROTE the state file:\n got: %s\nwant: %s", after, original)
	}
}
