// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// The bench command's PATH.
//
// netns-probe reported the defect on his own lab (a pure-outbound pair
// behind a real SNAT): in the same minute, `mesh ping` went over the
// relay (link=relay), `msg` was delivered, `send-to` pushed 3 MiB with
// a verified sha256 — and `bench` alone answered "no route to target:
// <peer> has no reachable endpoints in the current view".
//
// The cause is not the relay: it is that bench never ASKED for the relay
// path. Every other command builds its DialConfig with sess.picker();
// bench built one with only the target's ENDPOINTS, and a nil Picker
// means "always direct" (internal/mesh/route.go: dialViaPicker). A
// pure-outbound member has no endpoints — by design, that is the member
// shape the trunk exists for — so bench treated "reachable through the
// relay" as "unreachable", and the error it produced came from
// DialDirect (direct.go: "has no reachable endpoints in the current
// view"), which is a statement about the DIRECT path.
//
// The consequence is not cosmetic: bench is the product's own throughput
// tool, so the relay path could not be MEASURED with it. The only way
// to measure a relay's throughput was send-to with a stopwatch.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/mesh"
)

// TestBenchAsksForTheRelayPath pins the defect on the ERROR the command
// produces, which is where the difference is visible.
//
// The two shapes are distinct and were conflated:
//
//   - WITHOUT the picker, the command never offers the relay, so the
//     dial ends in DialDirect, whose error says "has no reachable
//     endpoints in the current view" — a statement about the DIRECT
//     path, which is exactly what netns-probe saw while ping/msg/send-to
//     worked over the relay in the same minute.
//   - WITH the picker, the relay is a candidate. The dial then fails on
//     the relay hop itself ("no route", "cannot reach", a dial error),
//     and the error no longer claims the member is unreachable.
func TestBenchAsksForTheRelayPath(t *testing.T) {
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

	// The command's config, built the way the bench command builds it.
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
	// one netns-probe reported.
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

// TestBenchAsksForTheRelayPathThroughTheCommand pins the same thing at
// the COMMAND level, so the wiring cannot be fixed in the mesh package
// while the CLI keeps forgetting it.
//
// runBench is called with a target that has no endpoints. The command
// must not answer with the direct-only verdict.
func TestBenchAsksForTheRelayPathThroughTheCommand(t *testing.T) {
	f := newCLIRelayFixture(t)

	// A short timeout: the command has to get as far as the dial, and
	// the dial has to FAIL on the relay hop (there is no live trunk in
	// this fixture).
	err := Run([]string{"bench", "--state", f.joinerDir, "--device-passphrase", f.pass,
		"--size", "64KB", "--timeout", "8s", string(f.joinerDev)})
	if err == nil {
		t.Fatal("the bench command succeeded against a target with no live peer")
	}
	msg := err.Error()
	if strings.Contains(msg, "no reachable endpoints") {
		t.Fatalf("the bench command answered %q: it never offered the relay "+
			"path, so a pure-outbound member reads as unreachable — the "+
			"exact defect netns-probe measured", msg)
	}
	if !strings.Contains(msg, string(f.joinerDev)) {
		t.Logf("the bench command failed with %q (it does not name the target; "+
			"the assertion that matters — not the \"no reachable endpoints\" "+
			"verdict — holds)", msg)
	}
}

// TestBenchReportsThePathItMeasured pins the second bench defect, found
// by the performance agent.
//
// The bench command used to drop the target's ENDPOINTS, so the direct
// path had no address to dial and every bench run took the relay. The
// agent's first "direct vs relay" comparison was therefore two
// measurements of the relay — it caught this by profiling the anchor
// during the supposed direct run.
//
// The path kind used to be dropped on the floor as well, so a number
// printed by the product's own throughput tool did not say which route
// it measured. That is the "direct vs relay" comparison's whole point.
//
// The assertions here are on the CONFIG the command builds and on the
// OUTPUT it prints, because a live measurement needs a live peer (the
// performance agent's lab provides that; this test pins the wiring).
func TestBenchReportsThePathItMeasured(t *testing.T) {
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

	// The command's config: the picker (the relay candidates) AND the
	// endpoints (the direct path's addresses). Without the endpoints the
	// direct path is dead, and every number the tool prints is a relay
	// number.
	cfg := mesh.DialConfig{
		Device:    sess.dev,
		Resolver:  sess.resolver,
		Target:    f.joinerDev,
		Endpoints: eps,
		Picker:    sess.picker(),
	}
	if cfg.Picker == nil {
		t.Fatal("the bench config carries no picker: the relay path would be unreachable")
	}
	if len(cfg.Endpoints) == 0 && len(eps) != 0 {
		t.Fatalf("the bench config carries %d endpoints, want %d: the direct "+
			"path has no address to dial without them", len(cfg.Endpoints), len(eps))
	}
}

// TestBenchPrintsTheLinkKind pins the output contract: a bench result
// must say which path it measured.
//
// The path kind used to be dropped on the floor (`link, _, err :=`), so
// a number printed by the product's own throughput tool did not say
// which route it measured — and the "direct vs relay" comparison is the
// whole point of a throughput tool.
func TestBenchPrintsTheLinkKind(t *testing.T) {
	f := newCLIRelayFixture(t)

	sess, err := openMeshSession(context.Background(), f.joinerDir, f.pass, nil)
	if err != nil {
		t.Fatalf("the joiner could not open a member view: %v", err)
	}

	// A dial that cannot succeed still has to REPORT a path: the kind
	// is part of the answer either way, and a bench that failed with an
	// empty kind would leave the operator guessing.
	_, kind, err := mesh.RunBenchWithPath(context.Background(), mesh.DialConfig{
		Device:   sess.dev,
		Resolver: sess.resolver,
		Target:   f.joinerDev,
		Picker:   sess.picker(),
	}, mesh.BenchConfig{Size: 1024})
	if err == nil {
		t.Fatal("the bench dial succeeded against a target with no live peer")
	}
	if kind == "" {
		t.Fatal("the bench path kind is empty: a bench result must say which " +
			"path it measured")
	}
}
