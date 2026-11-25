// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ★ THE ONE-SHOT COMMAND MUST SAY "YOU WERE REVOKED", NOT "UNREACHABLE".
// (Round 108, found by the M4 acceptance line.)
//
// The one-shot commands (`cmesh mesh ping`, `msg`, `send-to`, `run`,
// `bench`) open a session, and when no member view arrives they have to
// tell the user WHICH of the two failures it was:
//
//   - the discovery node is unreachable, or
//   - THIS DEVICE IS NOT A MEMBER (typically: revoked).
//
// The distinction matters for the operator: "unreachable" sends you off
// to debug routing, while "revoked" says the account owner removed you.
//
// ★ THE MESSAGE WAS DEAD CODE.
//
//	internal/cli/meshcmd.go
//	  waitCtx, cancel := context.WithTimeout(ctx, snapshotTimeout)
//	  ...
//	  case <-waitCtx.Done():                 // ← waitCtx is ALREADY EXPIRED here
//	      if cl.Snapshot() == nil {
//	          if perr := cl.Announce(waitCtx); perr != nil && discovery.IsNotAMember(perr) {
//	              return nil, fmt.Errorf("this device has been removed from this network ...")
//	          }
//	          return nil, fmt.Errorf("no member view within %s (discovery node unreachable, ...)")
//
// The `Announce` call is given `waitCtx` — the context whose deadline
// JUST fired. So the request never leaves the process: the HTTP client
// fails with "context deadline exceeded", never with the 401 that
// `IsNotAMember` looks for. The condition is therefore always false and
// the revocation message is NEVER printed.
//
// The user sees "no member view within 15s (discovery node unreachable,
// or not approved yet?)" for a device that was revoked hours ago.
//
// ★ The resident path (a long-running `join`) DOES report it — the
// watch loop there is not on an expired context. That is why the
// acceptance cell C3 looked covered: the coverage was of the OTHER
// path. (See `TestARevokedDeviceIsToldItWasRemoved` in
// m4acceptance_runtime_test.go, which covers the resident path.)
//
// This test drives the ONE-SHOT path, because that is the one that was
// dead.

// revocationTestPass is the passphrase for the revocation fixtures.
const revocationTestPass = "{{SECRET_7vbj2mn4}}"

// startDiscoveryNode starts a discovery node and waits until it answers.
// It returns the loopback address it listens on and its base URL.
func startDiscoveryNode(t *testing.T, ownerDir string) (addr string, base string) {
	t.Helper()
	discAddr := freePort(t)
	go func() {
		// The node runs until the test ends. Its error is not the
		// subject here — the SUBJECT is what the one-shot command SAYS
		// when it cannot get a member view.
		_ = Run([]string{"serve-discovery", "--state", ownerDir, "--addr", discAddr,
			"--device-passphrase", revocationTestPass})
	}()

	base = "http://" + discAddr
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			return discAddr, base
		}
		if time.Now().After(deadline) {
			t.Fatalf("the discovery node never came up: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestTheOneShotCommandSaysTheDeviceWasRevoked is the behavioural
// half: the one-shot path must distinguish "revoked" from
// "unreachable".
func TestTheOneShotCommandSaysTheDeviceWasRevoked(t *testing.T) {
	ownerDir := t.TempDir()
	joinerDir := t.TempDir()
	pass := revocationTestPass

	mustRun(t, "init", "--state", ownerDir, "--passphrase", pass)
	mustRun(t, "account", "create", "--state", ownerDir, "--passphrase", pass)
	mustRun(t, "account", "device-add", "--state", ownerDir,
		"--device-passphrase", pass, "--account-passphrase", pass)

	discAddr, base := startDiscoveryNode(t, ownerDir)

	// The joiner is NEVER authorized. So the node's answer to its
	// register is a rejection — the same shape a revoked device gets.
	oldTransport := discoveryClientTransportHook
	discoveryClientTransportHook = meshDiscoveryTestTransport(t, base)
	t.Cleanup(func() { discoveryClientTransportHook = oldTransport })
	logicalDiscAddr := "mesh.example:" + strings.TrimPrefix(discAddr, "127.0.0.1:")

	mustRun(t, "init", "--state", joinerDir, "--passphrase", pass)
	invite := mustRun(t, "invite", "--state", ownerDir,
		"--account-passphrase", pass, "--disc", logicalDiscAddr)
	invite = strings.TrimSpace(invite)
	if !strings.Contains(invite, "cmesh://join?") {
		t.Fatalf("the invite has no join string: %q", invite)
	}

	// ★ THE JOINER MUST ACTUALLY JOIN. The one-shot path reads the
	// SAVED discovery configuration, so without a join there is no
	// configuration to read and the failure is a different one
	// ("has not joined a network yet") — which would make this test
	// prove nothing.
	//
	// The join is EXPECTED TO TIME OUT, because the owner never
	// authorizes this device. That is the point: the device is a
	// stranger, and the node rejects it.
	joinDone := make(chan error, 1)
	go func() {
		joinDone <- Run([]string{"join", "--state", joinerDir,
			"--device-passphrase", pass, "--endpoint", "203.0.113.50:7000",
			"--once", "--timeout", "8s", invite})
	}()
	select {
	case <-joinDone:
		// Either way is fine: the device is not a member.
	case <-time.After(20 * time.Second):
		t.Log("the join did not return within 20s; continuing")
	}
	if _, err := os.Stat(filepath.Join(joinerDir, "discovery.json")); err != nil {
		t.Fatalf("the joiner never saved its discovery configuration, so the "+
			"one-shot path cannot be exercised: %v", err)
	}

	// ★ THE ASSERTION. The one-shot command must not tell the user
	// that the node is unreachable when the truth is "you are not a
	// member".
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	_, err := openMeshSessionWithSnapshotTimeout(ctx, joinerDir, pass, nil, 2*time.Second)
	if err == nil {
		t.Fatal("the session opened, so there is nothing to check")
	}

	msg := err.Error()
	if strings.Contains(msg, "unreachable") || strings.Contains(msg, "not approved yet") {
		t.Fatalf("★ THE DEFECT: the one-shot command reported %q — but the device is "+
			"NOT a member, so the honest message is \"this device has been removed from "+
			"this network\". The revocation branch is DEAD CODE: it calls Announce with "+
			"waitCtx, whose deadline has just fired, so the request never leaves the "+
			"process and IsNotAMember can never be true", msg)
	}
	if !strings.Contains(msg, "removed from this network") {
		t.Fatalf("the one-shot command must say the device was removed from the "+
			"network, got: %q", msg)
	}
}
