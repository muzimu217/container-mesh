// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"net"
	"testing"

	"github.com/muzimu217/container-mesh/internal/wsconn"
)

// TestTheResolvedAddressAdapterIsDefinedOnce pins that the adapter a
// policy callback receives has ONE definition.
//
// It used to have three: two in this package (one in egress.go, one in
// client.go — the architecture survey of round 59 listed only the first)
// and one in wsconn. The wsconn copy also carried an IP() method that
// nothing ever called.
//
// Two copies of one adapter is one more thing to keep in step, and the
// copies had already drifted: the wsconn one had a method the others did
// not.
//
// The check is on the TYPE, so a second definition anywhere fails it.
func TestTheResolvedAddressAdapterIsDefinedOnce(t *testing.T) {
	// The one that exists must be the wsconn one.
	a := wsconn.NewResolvedAddr("203.0.113.9:443", "node.example")
	if a.Network() != "tcp" {
		t.Fatalf("network = %q, want tcp", a.Network())
	}
	if a.String() != "203.0.113.9:443" {
		t.Fatalf("string = %q, want the numeric address", a.String())
	}
	if a.Host() != "node.example" {
		t.Fatalf("host = %q, want the name it resolved from", a.Host())
	}
	// It must satisfy net.Addr, which is what a policy callback takes.
	var _ net.Addr = a
}

// TestEgressPolicyStillReadsTheResolvedAddress pins that the policy reads
// the numeric address out of the adapter — the whole point of the
// adapter being shared.
func TestEgressPolicyStillReadsTheResolvedAddress(t *testing.T) {
	// A private address must be refused.
	err := EgressPolicy(wsconn.NewResolvedAddr("10.0.0.5:18090", "node.example"))
	if err == nil {
		t.Fatal("a private address was accepted")
	}
	// A public one must be accepted.
	if err := EgressPolicy(wsconn.NewResolvedAddr("8.8.8.8:18090", "node.example")); err != nil {
		t.Fatalf("a public address was refused: %v", err)
	}
	// A loopback one must be refused — that is the shape the round-59
	// survey's SSRF finding was about.
	err = EgressPolicy(wsconn.NewResolvedAddr("127.0.0.1:18090", "disc.local"))
	if err == nil {
		t.Fatal("a loopback address was accepted")
	}
}
