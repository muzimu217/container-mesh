// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// The dial command's PATH.
//
// `cmesh dial` opens an INTERACTIVE, mutually-authenticated channel to a
// member: the operator types lines and they echo back. It was the ONE
// command in the CLI that never asked for the relay path — it called
// mesh.DialDirect directly, so a nil Picker meant "always direct"
// (internal/mesh/route.go).
//
// The consequence: `dial` could never reach a PURE-OUTBOUND member — a
// member with no endpoints of its own, which is exactly the member shape
// the relay trunk exists for. Every other dial in the CLI goes through
// the picker, and `bench` had this fixed already for exactly this reason
// (netns-probe reported it on a real SNAT lab).
//
// So the command that opens a channel to a person was the one that could
// not reach half the network.
//
// And the line it printed said "direct" UNCONDITIONALLY — so even if it
// had fallen back, the operator would have been told the wrong thing.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/mesh"
)

// TestDialAsksForTheRelayPath pins the defect on the ERROR the command
// produces, which is where the difference is visible.
//
// The two shapes are distinct and were conflated:
//
//   - WITHOUT the picker, the dial never offers the relay, so it ends in
//     DialDirect, whose error says "has no reachable endpoints in the
//     current view" — a statement about the DIRECT path.
//   - WITH the picker, the relay is a candidate. The dial then fails on
//     the relay hop itself, and the error no longer claims the member is
//     unreachable.
func TestDialAsksForTheRelayPath(t *testing.T) {
	f := newCLIRelayFixture(t)

	sess, err := openMeshSession(context.Background(), f.joinerDir, f.pass, nil)
	if err != nil {
		t.Fatalf("the joiner could not open a member view: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eps, err := sess.resolveTarget(ctx, f.joinerDev)
	if err != nil {
		t.Fatalf("the target could not be resolved: %v", err)
	}
	if len(eps) != 0 {
		t.Fatalf("the fixture's target has endpoints %v; the defect needs a "+
			"pure-outbound target with none", eps)
	}

	// The command's config, built the way the dial command builds it.
	withPicker := mesh.DialConfig{
		Device:    sess.dev,
		Resolver:  sess.resolver,
		Target:    f.joinerDev,
		Endpoints: eps,
		Picker:    sess.picker(),
	}
	withoutPicker := withPicker
	withoutPicker.Picker = nil

	// The direct-only dial is the old behaviour, and its error is the
	// one that says the member is unreachable.
	_, errDirect := mesh.DialDirect(ctx, withoutPicker)
	if errDirect == nil {
		t.Skip("the fixture's target happens to be directly reachable; the " +
			"defect needs a pure-outbound target")
	}
	if !strings.Contains(errDirect.Error(), "no reachable endpoints") {
		t.Fatalf("the direct-only dial failed with %v; the defect needs the "+
			"\"no reachable endpoints\" shape to be distinguishable", errDirect)
	}

	// The fixed command offers the relay, so the failure is about the
	// RELAY hop — never the "the member has no endpoints" verdict.
	_, _, errPicker := mesh.DialViaPicker(ctx, withPicker)
	if errPicker == nil {
		return // the relay worked: even better
	}
	if strings.Contains(errPicker.Error(), "no reachable endpoints") {
		t.Fatalf("the command with a picker still answered %v: the relay "+
			"path was not offered, so the command treats a pure-outbound "+
			"member as unreachable", errPicker)
	}
	if !errors.Is(errPicker, mesh.ErrNoRoute) && !errors.Is(errPicker, mesh.ErrRelayUnavailable) &&
		!errors.Is(errPicker, mesh.ErrUnreachable) {
		t.Logf("the picker dial failed with %v (not a route error); the "+
			"assertion that matters — that it is NOT the \"no reachable "+
			"endpoints\" verdict — holds", errPicker)
	}
}

// TestDialAsksForTheRelayPathThroughTheCommand pins the same thing at
// the COMMAND level, so the wiring cannot be fixed in the mesh package
// while the CLI keeps forgetting it.
//
// runDial is called with a target that has no endpoints. The command must
// not answer with the direct-only verdict.
func TestDialAsksForTheRelayPathThroughTheCommand(t *testing.T) {
	f := newCLIRelayFixture(t)

	err := runDial([]string{
		"--state", f.joinerDir,
		"--device-passphrase", f.pass,
		"--timeout", "10s",
		f.joinerDev.String(),
	})
	if err == nil {
		t.Skip("the fixture's target happens to be directly reachable")
	}
	// The command's own error, not the mesh package's: this is the
	// command's answer to the operator.
	text := err.Error()
	if strings.Contains(text, "no reachable endpoints") {
		t.Fatalf("the dial command answered %v: it never offered the relay "+
			"path, so it treats a pure-outbound member as unreachable", text)
	}
}
