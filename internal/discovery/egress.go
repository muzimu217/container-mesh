// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"fmt"
	"net"
)

// EgressPolicy decides whether a resolved address may be connected to.
//
// The host check in allowedDiscoveryHost can only judge a literal address.
// A hostname passes that check on syntax alone, so the dialer has to make
// the second call: resolve, then look at what the name actually points at.
// This function is that second call.
func EgressPolicy(addr net.Addr) error {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("discovery: egress policy: %q did not resolve to an IP address", addr)
	}
	if !isPublicRoutableIP(ip) {
		return fmt.Errorf(
			"discovery: egress policy: %s resolved to %s, which is not a public address "+
				"(loopback, private, link-local and reserved ranges are refused)", addr, ip)
	}
	return nil
}
