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
	"context"
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
	"github.com/muzimu217/container-mesh/internal/relay"
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

	// --- Leg 3: direct links over the mesh. The OWNER device joins its own
	// network with a listener (it is already a member, so approval lands on
	// the first heartbeat); the joiner then pings it across a direct QUIC
	// link, endpoints resolved from the member view.
	ownerDev := loadJoinerDevice(t, ownerDir, pass)
	ownerJoined := make(chan struct{})
	go func() {
		defer close(ownerJoined)
		if err := Run([]string{"join", "--state", ownerDir, "--device-passphrase", pass,
			"--listen", "127.0.0.1:0", "--timeout", "15s", strings.TrimSpace(invite)}); err != nil {
			t.Errorf("owner join --listen: %v", err)
		}
	}()
	// Reap the join goroutine before the test ends: it prints view blocks
	// via fmt.Fprintf(os.Stderr, …), which races with the NEXT test's
	// stderr capture if still running (netns-probe round 21, O-B1 — the
	// failure surfaced under TestCLIAccountWorkflow's name). 15 s bounds
	// the goroutine while leaving 3-4× margin over legs 3-4.
	t.Cleanup(func() { <-ownerJoined })
	time.Sleep(1500 * time.Millisecond) // owner announces; endpoints propagate

	pingOut := captureStdout(t, func() {
		if err := Run([]string{"mesh", "ping", "--state", joinerDir, "--device-passphrase", pass,
			"--count", "5", "--timeout", "25s", ownerDev.String()}); err != nil {
			t.Errorf("mesh ping: %v", err)
		}
	})
	if !strings.Contains(pingOut, "5/5 pings") || !strings.Contains(pingOut, "p95") {
		t.Fatalf("mesh ping output incomplete: %q", pingOut)
	}

	// --- Leg 4 (D-7 regression): the OWNER now runs a one-shot ping at the
	// joiner (which has no listener → an immediate hint, exit 1). In the
	// buggy version the owner's one-shot client announced empty endpoints on
	// its way through, blanking the owner's own advertised endpoints for up
	// to a heartbeat — so the JOINER's NEXT ping at the owner failed with
	// "has announced no endpoints". With the fix (view-only one-shot clients
	// + server-side empty-announce guard) the repeat ping must succeed.
	pingErr := Run([]string{"mesh", "ping", "--state", ownerDir, "--device-passphrase", pass,
		"--count", "1", "--timeout", "25s", loadJoinerDevice(t, joinerDir, pass).String()})
	var hintExit ExitError
	if !errors.As(pingErr, &hintExit) || hintExit.Code != 1 {
		t.Fatalf("ping at listener-less joiner: got %v, want exit-1 hint", pingErr)
	}
	ping2 := captureStdout(t, func() {
		if err := Run([]string{"mesh", "ping", "--state", joinerDir, "--device-passphrase", pass,
			"--count", "3", "--timeout", "25s", ownerDev.String()}); err != nil {
			t.Errorf("repeat mesh ping after owner's one-shot: %v", err)
		}
	})
	if !strings.Contains(ping2, "3/3 pings") {
		t.Fatalf("owner's one-shot ping wiped its own advertised endpoints (D-7): %q", ping2)
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

// TestWatchApprovalsNoSelfTriggerLoop pins the D-3 fix (issue #8): with no
// external edits and no certificates dropped, the watcher must leave the
// member list alone — before the fix it reload+saved every 2 s, inflating
// the version (v3→v208 in 6 minutes of pure idle) and re-pushing identical
// snapshots to every member.
func TestWatchApprovalsNoSelfTriggerLoop(t *testing.T) {
	dir := t.TempDir()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	membersPath := filepath.Join(dir, "members.json")
	if err := members.SaveMembers(membersPath); err != nil {
		t.Fatal(err)
	}

	relaySrv := relay.New(relay.DefaultConfig())
	relaySrv.EnableDiscovery(members)
	t.Cleanup(relaySrv.StopDiscovery)

	certsDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go watchApprovals(ctx, membersPath, certsDir, members, relaySrv, []byte(acc.PublicKey()))

	// Three watcher ticks with zero input: the file must be untouched.
	time.Sleep(5500 * time.Millisecond)
	cancel()

	m, err := identity.LoadMembers(membersPath)
	if err != nil {
		t.Fatal(err)
	}
	if m.CurrentVersion() != 1 {
		t.Fatalf("idle watcher inflated the member-list version to %d (self-trigger loop, D-3)", m.CurrentVersion())
	}
	if m.Count() != 1 {
		t.Fatalf("idle watcher changed the member count to %d (self-trigger loop, D-3)", m.Count())
	}
}

// TestLocalAddrTreatsLoopbackAsLocal pins the kuku round-51 finding: an
// operator who runs `serve-discovery --addr 127.0.0.1:port` said "loopback
// only", but localAddr classified the loopback IP as non-local and the
// caller widened the UDP direct-link listener to 0.0.0.0 — a binding-scope
// defect visible in /proc/net/udp. Loopback is local by definition; only
// the NAT-mapped public address takes the wildcard branch.
func TestLocalAddrTreatsLoopbackAsLocal(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if !localAddr(host) {
			t.Fatalf("localAddr(%q) = false; loopback is a local address — the NAT wildcard branch must not capture it", host)
		}
	}
}

// TestEndpointPortZeroResolvesToTheBoundPort pins oh-no-agent's defect B-2:
// `--listen 0` expands and prints the port it bound, so writing
// `--endpoint 127.0.0.1:0` plainly means "announce me on that same port".
// The literal ":0" used to be announced verbatim and the anchor rejected it
// as malformed — the join failed over a flag that reads as valid.
func TestEndpointPortZeroResolvesToTheBoundPort(t *testing.T) {
	var notes bytes.Buffer
	got := resolvePortZeroEndpoints(
		[]string{"127.0.0.1:0", "203.0.113.7:18090", "example.com:0"},
		[]string{"127.0.0.1:54321"},
		&notes,
	)
	want := []string{"127.0.0.1:54321", "203.0.113.7:18090", "example.com:54321"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("endpoint %d = %q, want %q", i, got[i], want[i])
		}
	}
	if !strings.Contains(notes.String(), "54321") {
		t.Errorf("the resolution was not reported to the operator: %q", notes.String())
	}

	// Nothing to resolve against (wildcard-only bind): leave the input as-is
	// rather than inventing a port.
	wild := resolvePortZeroEndpoints([]string{"127.0.0.1:0"}, nil, io.Discard)
	if wild[0] != "127.0.0.1:0" {
		t.Errorf("with no bound address the endpoint must be left alone; got %q", wild[0])
	}

	// Malformed input must not panic or be rewritten.
	odd := resolvePortZeroEndpoints([]string{"not-an-address"}, []string{"127.0.0.1:7"}, io.Discard)
	if odd[0] != "not-an-address" {
		t.Errorf("a malformed endpoint was rewritten to %q", odd[0])
	}
}
