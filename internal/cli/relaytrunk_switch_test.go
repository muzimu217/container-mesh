// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/mesh"
)

// ---------------------------------------------------------------------------
// THE OPERATOR'S RELAY SWITCH. (Round 98)
//
// D73 promised that a node could stop relaying without being restarted.
// The capability existed — RelayTrunk.SetEnabled — and NOTHING called it:
// no flag, no signal, no admin endpoint.
//
// That is the shape this project has now hit three times: CloseAll (round
// 89), SetEnabled (round 89), and the trunk's own Shutdown wiring (round
// 96). A capability that nothing calls has exactly the symptom of a
// capability that does not exist.
//
// So the switch is wired to SIGHUP — the conventional "reload the
// configuration" signal — and this test asserts the WIRING, not the
// capability.
//
// ★ IT SENDS A REAL SIGNAL TO THIS PROCESS. That is the point: a mock
// would happily record the call, so the test would pass even if the
// signal path were not wired. The signal is the thing under test.
// ---------------------------------------------------------------------------

// TestTheRelaySwitchTogglesOnSIGHUP is the wiring test.
//
// It drives the REAL watcher and sends a REAL SIGHUP to this process,
// then reads the trunk's own state.
func TestTheRelaySwitchTogglesOnSIGHUP(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	trunk, err := mesh.NewRelayTrunk(mesh.RelayTrunkConfig{
		Self:    dev.ID(),
		Members: mesh.MemberListAdapter{List: members},
		Enabled: true,
		Joiners: mesh.NewJoinerTable(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !trunk.Enabled() {
		t.Fatal("the trunk should start enabled")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchRelayTrunkSwitch(ctx, trunk)

	// The signal must reach the watcher. Give it a moment to install
	// its handler — a signal that arrives before signal.Notify runs is
	// delivered to the DEFAULT handler, which for SIGHUP terminates the
	// process. That is a real hazard, and the test must not trip it.
	time.Sleep(100 * time.Millisecond)

	// ★ THE SWITCH.
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP: %v", err)
	}

	// The trunk must turn OFF.
	if !waitForRelaySwitch(trunk, false, 5*time.Second) {
		t.Fatalf("the relay switch did not turn the trunk off: " +
			"SetEnabled has no production caller, so the operator's switch does nothing")
	}

	// And back ON: the switch is a toggle, and a switch that can only
	// turn things off is half a switch.
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP: %v", err)
	}
	if !waitForRelaySwitch(trunk, true, 5*time.Second) {
		t.Fatal("the relay switch did not turn the trunk back on")
	}
}

// waitForRelaySwitch waits for the trunk's switch to reach the wanted
// state.
func waitForRelaySwitch(trunk *mesh.RelayTrunk, want bool, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if trunk.Enabled() == want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
