// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muzimu217/container-mesh/internal/discovery"
)

// addrOf builds the net.Addr the dialer would hand to the policy.
func addrOf(hostport string) net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18090}
}

// TestAPrivateAnchorIsRefusedByDefault pins that the default policy still
// refuses a private discovery origin.
//
// This is the SSRF protection of round 56: a discovery origin that resolves
// to a loopback or a private address must not be dialed, because a
// malicious invite could make a client reach into the host's own network.
//
// The private-deployment switch below opens this deliberately — but only
// for a deployment that says so.
func TestAPrivateAnchorIsRefusedByDefault(t *testing.T) {
	for _, addr := range []string{
		"http://10.0.0.5:18090",
		"http://192.168.1.10:18090",
		"http://127.0.0.1:18090",
	} {
		if _, err := discovery.NormalizeDiscoveryURL(addr); err == nil {
			t.Errorf("a private discovery origin was accepted by default: %s", addr)
		}
	}
}

// TestAPrivateAnchorIsAcceptedWithTheSwitch pins that a deployment which
// opted into private networking can reach its own anchor.
//
// A self-hosted mesh on a LAN has no public address at all. Without this,
// the join could not even be ENROLLED — the credential enrollment talks to
// the anchor over HTTP, and the address is refused before the request is
// made.
func TestAPrivateAnchorIsAcceptedWithTheSwitch(t *testing.T) {
	for _, addr := range []string{
		"http://10.0.0.5:18090",
		"http://192.168.1.10:18090",
		"http://127.0.0.1:18090",
	} {
		got, err := discovery.NormalizeDiscoveryURLWith(addr, true)
		if err != nil {
			t.Errorf("a private discovery origin was refused with the switch on: %s: %v", addr, err)
			continue
		}
		if !strings.Contains(got, "18090") {
			t.Errorf("the normalized address lost the port: %s", got)
		}
	}
}

// TestThePrivateSwitchStillRefusesTheReservedRanges pins that the
// relaxation is NARROW.
//
// The reserved and documentation ranges are never a deployment target —
// 0.0.0.0, 240.0.0.0/4, the TEST-NET and benchmark ranges. Allowing them
// would open an SSRF hole that no deployment needs, so the switch does
// not reach them.
func TestThePrivateSwitchStillRefusesTheReservedRanges(t *testing.T) {
	for _, addr := range []string{
		"http://0.0.0.0:18090",
		"http://240.0.0.1:18090",
		"http://192.0.2.5:18090",    // TEST-NET-1
		"http://198.51.100.5:18090", // TEST-NET-2
		"http://203.0.113.5:18090",  // TEST-NET-3
		"http://198.18.0.5:18090",   // benchmarking
		"http://100.64.0.5:18090",   // CGNAT
	} {
		if _, err := discovery.NormalizeDiscoveryURLWith(addr, true); err == nil {
			t.Errorf("a reserved address was accepted with the private switch on: %s", addr)
		}
	}
}

// TestThePrivateSwitchStillRefusesLocalhostByName pins that the NAME
// "localhost" stays refused.
//
// It is a name, not an address, and it always means the local machine —
// so no deployment needs it, private or not.
func TestThePrivateSwitchStillRefusesLocalhostByName(t *testing.T) {
	if _, err := discovery.NormalizeDiscoveryURLWith("http://localhost:18090", true); err == nil {
		t.Error("localhost was accepted with the private switch on")
	}
}

// TestAPrivateOriginIsDialedWithTheSwitch pins the RESOLVED-address half
// of the policy, which is the one that actually closes the SSRF hole.
//
// The literal-address check only judges a literal IP; a NAME passes it on
// syntax alone, and the second check happens after DNS resolution, in the
// dialer. A private deployment usually reaches its anchor by NAME, so
// this half has to be relaxed too — and it has to be relaxed by the same
// switch, or the deployment would work with an IP and fail with a name.
func TestAPrivateOriginIsDialedWithTheSwitch(t *testing.T) {
	// A listener on the loopback: with the switch on the client may
	// reach it, with the switch off it must not.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The resolved address of the loopback, as the dialer would see it.
	addr := srv.Listener.Addr().String()

	if err := discovery.EgressPolicyWith(addrOf(addr), true); err != nil {
		t.Fatalf("the loopback was refused with the private switch on: %v", err)
	}
	// Without the switch it must be refused — that is the SSRF guard.
	if err := discovery.EgressPolicyWith(addrOf(addr), false); err == nil {
		t.Fatal("the loopback was accepted by the default policy")
	}
}
