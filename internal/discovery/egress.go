// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// resolvedAddr is the address a host name resolved to, as the dialer sees
// it: the numeric address plus the original host name.
type resolvedAddr struct {
	addr string
	host string
}

func (a *resolvedAddr) Network() string { return "tcp" }
func (a *resolvedAddr) String() string  { return a.addr }

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
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("discovery: egress policy: cannot parse %q: %w", address, err)
		}
		return EgressPolicy(&resolvedAddr{addr: address, host: host})
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
}
