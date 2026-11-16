// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"net"
	"testing"
)

// TestEgressPolicyRefusesNonPublicAddresses pins the second half of the
// outbound-address policy.
//
// allowedDiscoveryHost refuses a literal loopback, private or reserved
// address, and its comment says the remaining job belongs to the dialer:
// "callers that dial them must use an egress policy that revalidates
// resolved addresses before connecting". That revalidation was never
// implemented: the WebSocket dialer used a bare net.Dialer, so a hostname
// that resolves to a loopback or private address walked straight through
// the policy. A discovery address of "disc.local" pointing at 127.0.0.1
// was reachable, and the same trick reaches any private address on the
// operator's network.
func TestEgressPolicyRefusesNonPublicAddresses(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		want bool // true = must be refused
	}{
		{"loopback v4", "127.0.0.1:8080", true},
		{"loopback v6", "[::1]:8080", true},
		{"private 10/8", "10.1.2.3:8080", true},
		{"private 172.16/12", "172.16.0.1:8080", true},
		{"private 192.168/16", "192.168.1.1:8080", true},
		{"link local", "169.254.1.1:8080", true},
		{"unspecified", "0.0.0.0:8080", true},
		{"cgnat", "100.64.0.1:8080", true},
		{"documentation", "203.0.113.7:8080", true},
		{"public v4", "93.184.216.34:8080", false},
		{"public v6", "[2606:2800:220:1:248:1893:25c8:1946]:8080", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := EgressPolicy(&testAddr{addr: tc.addr})
			if tc.want && err == nil {
				t.Fatalf("EgressPolicy(%s) allowed it; the policy must refuse a non-public address", tc.addr)
			}
			if !tc.want && err != nil {
				t.Fatalf("EgressPolicy(%s) refused a public address: %v", tc.addr, err)
			}
		})
	}
}

// testAddr is a net.Addr over a literal address.
type testAddr struct{ addr string }

func (a *testAddr) Network() string { return "tcp" }
func (a *testAddr) String() string  { return a.addr }

var _ = net.ParseIP
