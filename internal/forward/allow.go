// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package forward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ErrDenied is the sentinel for every policy refusal. Callers map it to
// wire.StatusDenied; the wrapped text goes to the operator.
var ErrDenied = errors.New("forward: denied by policy")

// Resolver turns a hostname into addresses. It is an interface so tests can
// simulate DNS changing between the check and the dial.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// netResolver is the production Resolver.
type netResolver struct{}

func (netResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// hostRule is one --allow-host entry, pinned at startup.
//
// Pinning is the whole point: if the allow-list held hostnames and the dialler
// resolved them again, a DNS answer could change between the check and the
// connect (rebinding), and the peer would reach an address the operator never
// approved.
type hostRule struct {
	name string   // as written on the command line, for messages
	ips  []net.IP // resolved once at construction
}

// PortSet is a set of allowed target ports, built from "5432" and "6000-7000"
// style entries.
type PortSet struct {
	ports  map[int]bool
	ranges []portRange
	empty  bool // true when nothing was configured, which denies everything
}

type portRange struct{ lo, hi int }

// ParsePortSet builds a PortSet from entries like "5432", "6000-7000".
//
// An empty list yields a set that denies everything. That is deliberate: a
// forgotten configuration must not mean "any port".
func ParsePortSet(entries []string) (PortSet, error) {
	ps := PortSet{ports: make(map[int]bool)}
	if len(entries) == 0 {
		ps.empty = true
		return ps, nil
	}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(e, "-"); ok {
			l, err := parsePort(lo)
			if err != nil {
				return PortSet{}, fmt.Errorf("forward: port range %q: %w", e, err)
			}
			h, err := parsePort(hi)
			if err != nil {
				return PortSet{}, fmt.Errorf("forward: port range %q: %w", e, err)
			}
			if l > h {
				return PortSet{}, fmt.Errorf("forward: port range %q is reversed", e)
			}
			ps.ranges = append(ps.ranges, portRange{l, h})
			continue
		}
		p, err := parsePort(e)
		if err != nil {
			return PortSet{}, fmt.Errorf("forward: port %q: %w", e, err)
		}
		ps.ports[p] = true
	}
	if len(ps.ports) == 0 && len(ps.ranges) == 0 {
		ps.empty = true
	}
	return ps, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, errors.New("not a number")
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("%d out of range 1-65535", n)
	}
	return n, nil
}

// Contains reports whether port is allowed.
func (ps PortSet) Contains(port int) bool {
	if ps.empty {
		return false
	}
	if ps.ports[port] {
		return true
	}
	for _, r := range ps.ranges {
		if port >= r.lo && port <= r.hi {
			return true
		}
	}
	return false
}

// Empty reports whether the set denies everything.
func (ps PortSet) Empty() bool { return ps.empty }

// AllowList decides whether a target may be dialled.
//
// The zero value permits NOTHING. That is the load-bearing default: a forgotten
// configuration must not turn the container into an open proxy, and an empty
// host list is "allow none", never "allow all".
type AllowList struct {
	// hosts, ports and resolver are all set at construction and never mutated
	// afterwards, which is what makes AllowList safe to copy and to read from
	// many goroutines at once. There is deliberately no mutex here: one would
	// make the type uncopyable (go vet rejects it) for no benefit.
	hosts    []hostRule
	ports    PortSet
	resolver Resolver
}

// NewAllowList resolves and pins every host entry.
//
// Resolution happens once, here, and never again. A failure to resolve is a
// startup error rather than a silent skip: an operator who wrote a hostname
// expects it to work, and silently dropping it would turn a typo into "denied"
// at 3 a.m.
func NewAllowList(ctx context.Context, hostEntries []string, ports PortSet, r Resolver) (AllowList, error) {
	if r == nil {
		r = netResolver{}
	}
	al := AllowList{ports: ports, resolver: r}
	for _, raw := range hostEntries {
		host := strings.TrimSpace(raw)
		if host == "" {
			continue
		}
		// An all-addresses CIDR is refused outright. ARCHITECTURE-SDWAN §8 is
		// explicit that --allow-any is never a feature; if an operator really
		// wants a wide range they can write the specific blocks.
		if host == "0.0.0.0/0" || host == "::/0" {
			return AllowList{}, fmt.Errorf("forward: %q is refused: list concrete hosts or CIDRs", host)
		}
		if ip := net.ParseIP(host); ip != nil {
			al.hosts = append(al.hosts, hostRule{name: host, ips: []net.IP{ip}})
			continue
		}
		// A literal CIDR is accepted without resolution.
		if _, _, err := net.ParseCIDR(host); err == nil {
			al.hosts = append(al.hosts, hostRule{name: host})
			continue
		}
		ips, err := r.LookupIPAddr(ctx, host)
		if err != nil {
			return AllowList{}, fmt.Errorf("forward: resolving %q: %w", host, err)
		}
		if len(ips) == 0 {
			return AllowList{}, fmt.Errorf("forward: %q resolved to no addresses", host)
		}
		pinned := make([]net.IP, 0, len(ips))
		for _, a := range ips {
			pinned = append(pinned, a.IP)
		}
		al.hosts = append(al.hosts, hostRule{name: host, ips: pinned})
	}
	if len(al.hosts) == 0 {
		// Zero value behaviour, stated explicitly so a caller cannot think an
		// empty list means "anything".
		al.ports = PortSet{empty: true}
	}
	return al, nil
}

// Empty reports whether this list denies every target.
func (a AllowList) Empty() bool { return len(a.hosts) == 0 }

// Permits returns the concrete IP to dial, or ErrDenied explaining the refusal.
//
// Returning the IP rather than a bool is what closes the check-then-dial
// window: the caller dials exactly the address that was authorised. A bool
// would force the caller to resolve the name again, which is a TOCTOU bug.
func (a AllowList) Permits(ctx context.Context, addr string) (net.IP, error) {
	if len(a.hosts) == 0 {
		return nil, fmt.Errorf("%w: no --allow-host configured", ErrDenied)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not host:port", ErrDenied, addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("%w: port %q is not a number", ErrDenied, portStr)
	}
	if !a.ports.Contains(port) {
		return nil, fmt.Errorf("%w: port %d is not in --allow-port", ErrDenied, port)
	}

	// A name that was configured at startup is matched by NAME, and the pinned
	// address is returned. Re-resolving here would be the bug this type exists
	// to prevent: the answer could have changed since startup, so the request
	// would either be denied (a rebinding outage) or, worse, approved for an
	// address the operator never listed.
	for _, rule := range a.hosts {
		if len(rule.ips) > 0 && strings.EqualFold(rule.name, host) {
			return rule.ips[0], nil
		}
	}

	// Otherwise the request names something concrete (an IP or an unlisted
	// hostname). Resolve it and require the answer to match the pinned set.
	var candidates []net.IP
	if ip := net.ParseIP(host); ip != nil {
		candidates = []net.IP{ip}
	} else {
		res := a.resolver
		if res == nil {
			res = netResolver{}
		}
		ips, err := res.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot resolve %q: %v", ErrDenied, host, err)
		}
		for _, ipa := range ips {
			candidates = append(candidates, ipa.IP)
		}
	}

	for _, rule := range a.hosts {
		// Pinned literal / hostname addresses.
		for _, want := range rule.ips {
			for _, got := range candidates {
				if want.Equal(got) {
					// Return the pinned address, not the freshly resolved one.
					return want, nil
				}
			}
		}
		// CIDR rules.
		if len(rule.ips) == 0 {
			if _, ipnet, err := net.ParseCIDR(rule.name); err == nil {
				for _, got := range candidates {
					if ipnet.Contains(got) {
						return got, nil
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("%w: %q is not in --allow-host", ErrDenied, addr)
}
