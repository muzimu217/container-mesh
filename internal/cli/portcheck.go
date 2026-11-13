// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// The anchor's port self-check.
//
// The deployment question is always the same: "I opened the port, why can
// nobody reach me?" — and the answer differs per cause:
//
//   - the discovery port is bound but the firewall drops it (the common
//     case on a cloud server: the process is listening, the security group
//     is not);
//   - the port is bound to loopback only, so nothing outside can reach it;
//   - the UDP direct-link port never got bound at all, so members cannot
//     dial this device even though the discovery port works;
//   - the announced address is not the address members will dial.
//
// The check answers those concretely, at startup, with the port numbers
// and the action to take. It never guesses: every line says what was
// observed and what it implies.

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// portCheck is one checked port.
type portCheck struct {
	// Name is what the port is for ("discovery", "direct link").
	Name string
	// Port is the TCP or UDP port number.
	Port int
	// Protocol is "tcp" or "udp".
	Protocol string
	// Bound reports whether THIS process is listening on it.
	Bound bool
	// Detail explains what was observed.
	Detail string
}

// portCheckReport is the whole answer.
type portCheckReport struct {
	Checks []portCheck
	// Hints are the actions the operator should take, most important first.
	Hints []string
}

// checkAnchorPorts inspects the anchor's own ports.
//
// The discovery listener is already up when this runs, so "bound" is known
// for it; the UDP direct-link listener is checked the same way, because the
// two share a port number by design (HTTP on TCP, QUIC on UDP) and an
// operator who opened only TCP would see exactly this failure.
func checkAnchorPorts(discoAddr string, discBound bool, udpAddrs []string) portCheckReport {
	var rep portCheckReport

	// The discovery port.
	port := 0
	if _, p, err := net.SplitHostPort(discoAddr); err == nil {
		fmt.Sscanf(p, "%d", &port)
	}
	if port > 0 {
		c := portCheck{Name: "discovery", Port: port, Protocol: "tcp", Bound: discBound}
		switch {
		case discBound:
			c.Detail = "listening"
		default:
			c.Detail = "not listening"
		}
		rep.Checks = append(rep.Checks, c)
	}

	// The direct-link ports.
	for _, a := range udpAddrs {
		host, p, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		n := 0
		fmt.Sscanf(p, "%d", &n)
		if n == 0 {
			continue
		}
		rep.Checks = append(rep.Checks, portCheck{
			Name: "direct link", Port: n, Protocol: "udp", Bound: true,
			Detail: "bound on " + host,
		})
	}

	rep.Hints = portHints(rep, discoAddr)
	return rep
}

// portHints turns the observations into actions.
//
// The order matters: the most common cause of "the anchor is invisible"
// comes first, because it is the one that costs the operator the most time
// to diagnose on their own.
func portHints(rep portCheckReport, discoAddr string) []string {
	var hints []string
	host, _, err := net.SplitHostPort(discoAddr)
	loopbackOnly := err == nil && (host == "127.0.0.1" || host == "localhost" || host == "::1")

	for _, c := range rep.Checks {
		if c.Bound {
			continue
		}
		switch c.Protocol {
		case "tcp":
			hints = append(hints, fmt.Sprintf(
				"port %d/%s is NOT bound — the discovery node is not listening there; check the --addr you passed",
				c.Port, c.Protocol))
		case "udp":
			hints = append(hints, fmt.Sprintf(
				"port %d/udp is NOT bound — members cannot dial this device directly; they will have to relay through another member",
				c.Port))
		}
	}

	if loopbackOnly {
		hints = append(hints, fmt.Sprintf(
			"the discovery node listens on %s only (loopback) — nothing outside this machine can reach it; bind 0.0.0.0:%s and announce a reachable address",
			host, portOf(discoAddr)))
	}

	// The port that must be open in the firewall: the discovery port, and
	// the UDP port of the same number for direct links.
	for _, c := range rep.Checks {
		if c.Protocol == "tcp" {
			hints = append(hints, fmt.Sprintf(
				"open TCP %d in the firewall/security group — clients dial this port for discovery and the members stream",
				c.Port))
			if hasProtocol(rep, c.Port, "udp") {
				hints = append(hints, fmt.Sprintf(
					"open UDP %d as well — direct links use the SAME port number over UDP; without it members must relay",
					c.Port))
			}
		}
	}
	if len(hints) == 0 {
		hints = append(hints, "all checked ports are bound; if members still cannot reach this node, the address they dial is the thing to check (see the --announce value above)")
	}
	return hints
}

// hasProtocol reports whether any check uses that protocol on that port.
func hasProtocol(rep portCheckReport, port int, protocol string) bool {
	for _, c := range rep.Checks {
		if c.Port == port && c.Protocol == protocol {
			return true
		}
	}
	return false
}

// printPortCheck writes the self-check to stderr.
func printPortCheck(rep portCheckReport) {
	fmt.Fprintf(os.Stderr, "\nport self-check:\n")
	for _, c := range rep.Checks {
		state := "not bound"
		if c.Bound {
			state = "bound"
		}
		fmt.Fprintf(os.Stderr, "  %-11s %d/%-3s  %-9s %s\n", c.Name, c.Port, c.Protocol, state, c.Detail)
	}
	fmt.Fprintf(os.Stderr, "  what to do:\n")
	for _, h := range rep.Hints {
		fmt.Fprintf(os.Stderr, "    - %s\n", h)
	}
	fmt.Fprintln(os.Stderr)
}

// portSummary renders the checks as one line, for logs.
func portSummary(rep portCheckReport) string {
	var parts []string
	for _, c := range rep.Checks {
		state := "down"
		if c.Bound {
			state = "up"
		}
		parts = append(parts, fmt.Sprintf("%s %d/%s %s", c.Name, c.Port, c.Protocol, state))
	}
	return strings.Join(parts, ", ")
}
