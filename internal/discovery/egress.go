// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/muzimu217/container-mesh/internal/wsconn"
)

// EgressPolicy decides whether a resolved address may be connected to.
//
// The host check in allowedDiscoveryHost can only judge a literal address.
// A hostname passes that check on syntax alone, so the dialer has to make
// the second call: resolve, then look at what the name actually points at.
// This function is that second call.
func EgressPolicy(addr net.Addr) error {
	return EgressPolicyWith(addr, false)
}

// EgressPolicyWith is the switchable form.
//
// privateAllowed relaxes the policy for a deployment that opted into
// private networking: a self-hosted mesh on a LAN reaches its anchor by a
// private address or by a name that resolves to one, and the default
// policy refuses both — so the client could not even enroll.
//
// The relaxation is NARROW: the private, loopback and link-local ranges
// are allowed, because a real private deployment uses them. The reserved
// and documentation ranges are still refused: they are never a deployment
// target.
func EgressPolicyWith(addr net.Addr, privateAllowed bool) error {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("discovery: egress policy: %q did not resolve to an IP address", addr)
	}
	if privateAllowed {
		if !allowedPrivateIP(ip) {
			return fmt.Errorf(
				"discovery: egress policy: %s resolved to %s, which is not an address a private "+
					"deployment may use (the reserved ranges are refused even then)", addr, ip)
		}
		return nil
	}
	if !isPublicRoutableIP(ip) {
		return fmt.Errorf(
			"discovery: egress policy: %s resolved to %s, which is not a public address "+
				"(loopback, private, link-local and reserved ranges are refused)", addr, ip)
	}
	return nil
}

// EgressTransport returns an HTTP transport that applies the outbound
// address policy to every connection it makes.
//
// A caller that reaches a discovery node over HTTP needs the same protection
// the members stream has. The policy is consulted with the address the host
// name resolved to, after DNS and before the socket is connected, so a
// refused address never reaches the network.
//
// timeout bounds the dial and the response headers.
func EgressTransport(timeout time.Duration) *http.Transport {
	return PolicyTransport(timeout, EgressPolicy)
}

// EgressTransportWith is EgressTransport with the private-deployment
// switch. A caller that opted into private networking gets the relaxed
// policy on every path it dials.
func EgressTransportWith(timeout time.Duration, privateAllowed bool) *http.Transport {
	return PolicyTransport(timeout, func(addr net.Addr) error {
		return EgressPolicyWith(addr, privateAllowed)
	})
}

// PolicyTransport returns an HTTP transport that applies the given policy to
// every connection it makes.
//
// This is the one place the outbound HTTP transport is built. The client's
// own register path used to carry a second copy of it, with one difference:
// the policy there could be switched off (AllowLocalDiscovery, for the
// local test harness). That difference is now a parameter, so the two
// paths cannot drift apart again.
//
// A nil policy means no check — the caller has said so explicitly.
func PolicyTransport(timeout time.Duration, policy func(net.Addr) error) *http.Transport {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	if policy != nil {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("discovery: egress policy: cannot parse %q: %w", address, err)
			}
			return policy(wsconn.NewResolvedAddr(address, host))
		}
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
}
