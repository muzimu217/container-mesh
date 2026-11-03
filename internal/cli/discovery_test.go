// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// The full mesh bootstrap story, in-process, over a real socket:
// init → account create → device-add → serve-discovery → invite →
// (second device) init → show-pub → join → account authorize → drop cert
// into the discovery host's certs dir → joiner approved and visible.
// This is the R-1.5 approval loop and the O-6 bootstrap story end to end.

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// captureStdout swaps os.Stdout while fn runs and returns what it printed.
// Invite/show-pub print the payload on stdout (copy-paste surface); the CLI
// prints progress on stderr, which stays visible in test output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fnC := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		fnC <- buf.String()
	}()
	fn()
	_ = w.Close()
	out := <-fnC
	_ = r.Close()
	return out
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return "127.0.0.1:" + port
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out := captureStdout(t, func() {
		if err := Run(args); err != nil {
			t.Fatalf("cmesh %s: %v", strings.Join(args, " "), err)
		}
	})
	return out
}

// TestMeshBootstrapApprovalFlow runs the complete two-device story.
func TestMeshBootstrapApprovalFlow(t *testing.T) {
	ownerDir := filepath.Join(t.TempDir(), "owner")
	joinerDir := filepath.Join(t.TempDir(), "joiner")
	pass := "test-pass-phrase"

	// --- owner machine: identity, account, sign itself in, host discovery
	mustRun(t, "init", "--state", ownerDir, "--passphrase", pass)
	mustRun(t, "account", "create", "--state", ownerDir, "--passphrase", pass)
	mustRun(t, "account", "device-add", "--state", ownerDir,
		"--device-passphrase", pass, "--account-passphrase", pass)

	discAddr := freePort(t)
	srvReady := make(chan struct{})
	go func() {
		// serve-discovery blocks until interrupted; the test ends the process
		// implicitly by finishing. Errors surface via t.Error in the goroutine.
		if err := Run([]string{"serve-discovery", "--state", ownerDir, "--addr", discAddr,
			"--device-passphrase", pass}); err != nil {
			t.Errorf("serve-discovery: %v", err)
		}
	}()
	close(srvReady)

	// Wait for the HTTP server to accept (the port was free a moment ago;
	// there is a tiny race window the retry absorbs).
	base := "http://" + discAddr
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("discovery node never came up: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// --- owner: print an invite for the joiner
	invite := mustRun(t, "invite", "--state", ownerDir,
		"--account-passphrase", pass, "--disc", discAddr)
	if !strings.Contains(invite, "cmesh://join?") {
		t.Fatalf("invite output missing the join string: %q", invite)
	}

	// --- joiner machine: own identity only, then join (blocks until approval)
	mustRun(t, "init", "--state", joinerDir, "--passphrase", pass)
	pubOut := mustRun(t, "show-pub", "--state", joinerDir, "--device-passphrase", pass)
	pubLine := ""
	for _, line := range strings.Split(pubOut, "\n") {
		if line != "" && !strings.Contains(line, " ") {
			pubLine = strings.TrimSpace(line)
			break
		}
	}
	if pubLine == "" {
		t.Fatalf("show-pub printed no base64 key: %q", pubOut)
	}

	joined := make(chan error, 1)
	go func() {
		// --once: exit as soon as approval lands, which is the assertion.
		// The invite goes LAST: flag parsing stops at the first positional.
		joined <- Run([]string{"join", "--state", joinerDir,
			"--device-passphrase", pass, "--endpoint", "203.0.113.50:7000",
			"--once", "--timeout", "30s", strings.TrimSpace(invite)})
	}()

	// Give the joiner a moment to save its config and start announcing, then
	// run the approval on the owner machine: authorize the remote key.
	time.Sleep(500 * time.Millisecond)
	joinerDev := loadJoinerDevice(t, joinerDir, pass)
	certOut := captureStdout(t, func() {
		if err := Run([]string{"account", "authorize", "--state", ownerDir,
			"--account-passphrase", pass, "--pub", pubLine,
			"--out", filepath.Join(ownerDir, "certs", string(joinerDev)+identity.CertSuffix)}); err != nil {
			t.Errorf("account authorize: %v", err)
		}
	})
	_ = certOut

	// The watcher must adopt the cert and push; the joiner returns approved.
	select {
	case err := <-joined:
		if err != nil {
			t.Fatalf("join failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("join never returned within 30s")
	}

	// The owner-side list must now contain both devices, persisted.
	m, err := identity.LoadMembers(filepath.Join(ownerDir, "members.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Count() != 2 {
		t.Fatalf("approved network has %d members, want 2 (adopted cert must persist)", m.Count())
	}
}

func loadJoinerDevice(t *testing.T, dir, pass string) identity.DeviceID {
	t.Helper()
	dev, err := identity.LoadDevice(identity.IdentityPath(dir, identity.DeviceFileName), pass)
	if err != nil {
		t.Fatal(err)
	}
	return dev.ID()
}

// TestServeDiscoveryTLSHalfConfigRejected pins the F11 fix: one TLS flag
// without the other is a usage error, not a silent plaintext fallback.
func TestServeDiscoveryTLSHalfConfigRejected(t *testing.T) {
	err := Run([]string{"serve-discovery", "--state", t.TempDir(), "--tls-cert", "/nonexistent.pem"})
	var exit ExitError
	if !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("half TLS config: got %v, want usage error (exit 2)", err)
	}
	if !strings.Contains(err.Error(), "together") {
		t.Fatalf("error must say the flags go together: %v", err)
	}
}
