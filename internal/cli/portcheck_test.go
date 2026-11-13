// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// Tests for the anchor's port self-check.
//
// The operator-facing contract: the check must say WHICH port to open and
// WHY something is unreachable. A check that only says "not bound" without
// naming the port and the action is the failure mode this replaces.

import (
	"strings"
	"testing"
)

func TestPortCheckNamesThePortToOpen(t *testing.T) {
	rep := checkAnchorPorts("0.0.0.0:18090", true, []string{"0.0.0.0:18090"})
	joined := strings.Join(rep.Hints, "\n")
	if !strings.Contains(joined, "18090") {
		t.Fatalf("the hints do not name the port: %s", joined)
	}
	if !strings.Contains(joined, "firewall") {
		t.Fatalf("the hints do not tell the operator what to do: %s", joined)
	}
}

func TestPortCheckFlagsLoopbackOnly(t *testing.T) {
	rep := checkAnchorPorts("127.0.0.1:18090", true, []string{"127.0.0.1:18090"})
	joined := strings.Join(rep.Hints, "\n")
	if !strings.Contains(joined, "loopback") {
		t.Fatalf("a loopback-only anchor must be called out: %s", joined)
	}
	if !strings.Contains(joined, "0.0.0.0") {
		t.Fatalf("the hint must say what to bind instead: %s", joined)
	}
}

func TestPortCheckFlagsUnboundPort(t *testing.T) {
	rep := checkAnchorPorts("0.0.0.0:18090", false, nil)
	joined := strings.Join(rep.Hints, "\n")
	if !strings.Contains(joined, "NOT bound") {
		t.Fatalf("an unbound port must be called out: %s", joined)
	}
}

func TestPortCheckReportsBothProtocols(t *testing.T) {
	rep := checkAnchorPorts("0.0.0.0:18090", true, []string{"0.0.0.0:18090"})
	if len(rep.Checks) != 2 {
		t.Fatalf("expected a TCP and a UDP check, got %d: %+v", len(rep.Checks), rep.Checks)
	}
	var sawTCP, sawUDP bool
	for _, c := range rep.Checks {
		if c.Protocol == "tcp" {
			sawTCP = true
		}
		if c.Protocol == "udp" {
			sawUDP = true
		}
		if c.Port != 18090 {
			t.Errorf("check reports port %d, want 18090", c.Port)
		}
	}
	if !sawTCP || !sawUDP {
		t.Fatalf("both protocols must be reported (tcp=%v udp=%v)", sawTCP, sawUDP)
	}
}

func TestPortCheckSaysBothPortsOpen(t *testing.T) {
	// The deployment insight that costs operators the most time: the SAME
	// port number is used for discovery (TCP) and direct links (UDP).
	rep := checkAnchorPorts("0.0.0.0:18090", true, []string{"0.0.0.0:18090"})
	joined := strings.Join(rep.Hints, "\n")
	if !strings.Contains(joined, "TCP 18090") || !strings.Contains(joined, "UDP 18090") {
		t.Fatalf("both protocols must appear in the open-port guidance: %s", joined)
	}
}
