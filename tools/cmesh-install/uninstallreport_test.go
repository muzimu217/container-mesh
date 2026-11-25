// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ THE UNINSTALL MUST REPORT WHAT IT STOPPED AND WHAT IT LEFT BEHIND.
//
// The uninstall is the last thing an operator does to a machine, and it
// is the thing they will be asked about later ("is it clean?"). So the
// answer has to be IN THE OUTPUT, not in the operator's memory.
//
// Two things must be reported:
//
//  1. WHICH UNITS were stopped and disabled. An uninstall that leaves a
//     unit enabled means the next boot starts a unit whose ExecStart
//     points at a binary that no longer exists: status=203/EXEC, then
//     the restart loop.
//
//  2. WHAT WAS LEFT BEHIND. A plain uninstall keeps the runtime state
//     — and that state holds the device's private key envelope and the
//     credential material. An operator who is told "uninstalled" and not
//     told that the state survived believes the machine is clean when
//     the device's key envelope is still on disk.
//
// The purge path has the same duty, and it has one more: it removed
// the unit FILES, so the operator needs to know that too.
//
// ★ WHY THE CAPTURE IS AT THE FUNCTION LEVEL.
//
// uninstall() writes to stdout with fmt.Printf, so the test captures
// os.Stdout for the duration of the call. Asserting on the RETURN
// VALUE instead would be easy — but the return value is an error, and
// "did it succeed" is not what this test is about. What it is about is
// what the operator is TOLD.

// captureStdout runs fn with os.Stdout redirected and returns what was
// written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	fn()
	w.Close()
	os.Stdout = old
	out := <-done
	r.Close()
	return out
}

// installedRoot builds a fake managed install root.
func installedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".cmesh-install-root"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := installPayload(root, "v1", []byte("binary")); err != nil {
		t.Fatalf("installPayload: %v", err)
	}
	return root
}

// ★ THE PLAIN UNINSTALL MUST NAME THE UNITS IT STOPPED.
//
// The Go uninstaller stopped and disabled the units (that part was
// fixed) but printed the list only on the NON-purge path. The purge
// path returned early, before the print — so the operator who purged
// the machine, and whose unit files were DELETED, was told nothing at
// all.
//
// The consequence is the one this project has been bitten by before: an
// operator who believes the machine is clean when it is not.
func TestTheUninstallNamesTheUnitsItStopped(t *testing.T) {
	root := installedRoot(t)
	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}

	out := captureStdout(t, func() {
		if err := uninstall(root, state, false); err != nil {
			t.Fatalf("uninstall: %v", err)
		}
	})
	if !strings.Contains(out, "stopped and disabled") {
		t.Errorf("the uninstall did not report which units it stopped; "+
			"an operator who is not told believes the machine is clean when "+
			"the units are still enabled.\n--- output ---\n%s", out)
	}
}

// ★ THE PURGE MUST REPORT TOO.
//
// The purge removed the unit FILES as well as stopping them, so the
// operator's question ("what is left?") has a longer answer here — and
// the purge path is exactly where the report went missing.
func TestThePurgeNamesTheUnitsItStopped(t *testing.T) {
	root := installedRoot(t)
	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}

	out := captureStdout(t, func() {
		if err := uninstall(root, state, true); err != nil {
			t.Fatalf("uninstall --purge: %v", err)
		}
	})
	if !strings.Contains(out, "stopped and disabled") {
		t.Errorf("the purge did not report which units it stopped; the purge "+
			"also DELETED the unit files, so the operator's \"what is left on "+
			"this machine\" question has a longer answer here.\n--- output ---\n%s", out)
	}
}

// ★ THE UNINSTALL MUST NAME THE STATE IT RETAINED.
//
// The plain uninstall keeps the runtime state on purpose — the device
// identity has to survive so a reinstall recovers it. But "kept on
// purpose" is not the same as "the operator was told".
//
// The state holds the device's private key envelope and the credential
// material. An operator who runs the uninstall and hears nothing about
// the state believes the machine was cleaned, and it was not.
func TestTheUninstallNamesTheStateItRetained(t *testing.T) {
	root := installedRoot(t)
	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	secret := filepath.Join(state, "identity.enc")
	if err := os.WriteFile(secret, []byte("secret envelope"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	out := captureStdout(t, func() {
		if err := uninstall(root, state, false); err != nil {
			t.Fatalf("uninstall: %v", err)
		}
	})
	if !strings.Contains(out, state) {
		t.Errorf("the uninstall did not mention the state directory it retained "+
			"(%s); that directory holds the device's private key envelope, so an "+
			"operator who is not told believes the machine is clean when it is "+
			"not.\n--- output ---\n%s", state, out)
	}
	// And the state must actually survive.
	if _, err := os.Stat(secret); err != nil {
		t.Errorf("the plain uninstall removed the state: %v; the device identity "+
			"has to survive so a reinstall recovers it", err)
	}
}

// ★ THE UNINSTALL MUST NAME THE STATE IT REMOVED.
//
// The purge removes the state, and the operator needs to know that the
// device's private key envelope is GONE — because the next install
// starts from a blank identity, and that is a fact the operator has to
// have before they find out.
func TestThePurgeNamesTheStateItRemoved(t *testing.T) {
	root := installedRoot(t)
	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}

	out := captureStdout(t, func() {
		if err := uninstall(root, state, true); err != nil {
			t.Fatalf("uninstall --purge: %v", err)
		}
	})
	if !strings.Contains(out, state) {
		t.Errorf("the purge did not mention the state directory it removed (%s); "+
			"the next install starts from a BLANK identity, and the operator has "+
			"to know that before they find out.\n--- output ---\n%s", state, out)
	}
}
