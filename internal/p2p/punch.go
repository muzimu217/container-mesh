// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// Candidate is one address at which a peer might be reachable.
type Candidate struct {
	// Addr is "ip:port".
	Addr string
	// Kind is "local", "stun" (server-reflexive) or "host" (directly given).
	Kind string
	// Priority orders attempts: higher is tried first.
	Priority int
}

// GatherResult is what a peer learns about itself before signalling.
type GatherResult struct {
	Candidates []Candidate
	// STUNErrors records why a particular server did not answer; useful in
	// diagnostics because the reason differs by region and carrier.
	STUNErrors map[string]string
}

// Gather collects local and server-reflexive candidates.
//
// The local addresses come from the socket's own port (the NAT mapping is
// per-socket, so a candidate is only useful if it belongs to the socket that
// will carry the data). Server-reflexive candidates come from STUN servers
// queried over that same socket — that is what makes the mapping reusable.
func Gather(ctx context.Context, udp *net.UDPConn, stunServers []string, timeout time.Duration) (*GatherResult, error) {
	res := &GatherResult{STUNErrors: map[string]string{}}

	// Local addresses: the socket's bound port on every local interface.
	port := udp.LocalAddr().(*net.UDPAddr).Port
	for _, ip := range localIPs() {
		res.Candidates = append(res.Candidates, Candidate{
			Addr:     net.JoinHostPort(ip, fmt.Sprint(port)),
			Kind:     "local",
			Priority: 10, // LAN beats everything when it works
		})
	}

	if len(stunServers) == 0 {
		return res, nil
	}

	// Query STUN over the *same* socket so the reflexive address matches the
	// mapping the peer will actually send to.
	for _, srv := range stunServers {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
		mapped, err := stunQueryOnSocket(udp, srv, timeout)
		if err != nil {
			res.STUNErrors[srv] = err.Error()
			continue
		}
		res.Candidates = append(res.Candidates, Candidate{
			Addr:     mapped,
			Kind:     "stun",
			Priority: 100,
		})
	}
	return res, nil
}

// localIPs returns non-loopback unicast addresses of the host.
func localIPs() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				out = append(out, v4.String())
			}
		}
	}
	return out
}

// stunQueryOnSocket sends a binding request from udp and returns the mapped
// address the server observed.
func stunQueryOnSocket(udp *net.UDPConn, server string, timeout time.Duration) (string, error) {
	addr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return "", err
	}

	// STUN binding request: type 0x0001, length 0, magic cookie, 12-byte txid.
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001)
	binary.BigEndian.PutUint16(req[2:], 0)
	binary.BigEndian.PutUint32(req[4:], 0x2112A442)
	txid := req[8:20]
	if _, err := rand.Read(txid); err != nil {
		return "", err
	}

	deadline := time.Now().Add(timeout)
	if err := udp.SetReadDeadline(deadline); err != nil {
		return "", err
	}
	defer udp.SetReadDeadline(time.Time{})

	if _, err := udp.WriteToUDP(req, addr); err != nil {
		return "", err
	}

	buf := make([]byte, 1500)
	for {
		n, from, err := udp.ReadFromUDP(buf)
		if err != nil {
			return "", err
		}
		// Only accept a reply from the server we asked.
		if !from.IP.Equal(addr.IP) {
			continue
		}
		// Replies that are not STUN (e.g. a punch from the peer arriving at
		// the same moment) are skipped rather than misparsed.
		if n < 20 || binary.BigEndian.Uint16(buf[0:]) != 0x0101 {
			continue
		}
		if string(buf[8:20]) != string(txid) {
			continue
		}
		return parseXorMapped(buf[:n])
	}
}

// parseXorMapped extracts XOR-MAPPED-ADDRESS (0x0020) or MAPPED-ADDRESS.
func parseXorMapped(b []byte) (string, error) {
	n := len(b)
	pos := 20
	for pos+4 <= n {
		atype := binary.BigEndian.Uint16(b[pos:])
		alen := int(binary.BigEndian.Uint16(b[pos+2:]))
		val := pos + 4
		if val+alen > n {
			break
		}
		switch atype {
		case 0x0020: // XOR-MAPPED-ADDRESS
			if alen >= 8 && b[val+1] == 0x01 {
				port := binary.BigEndian.Uint16(b[val+2:]) ^ 0x2112
				ip := make(net.IP, 4)
				for i := 0; i < 4; i++ {
					ip[i] = b[val+4+i] ^ byte(uint32(0x2112A442)>>(8*(3-i)))
				}
				return net.JoinHostPort(ip.String(), fmt.Sprint(port)), nil
			}
		case 0x0001: // MAPPED-ADDRESS
			if alen >= 8 && b[val+1] == 0x01 {
				port := binary.BigEndian.Uint16(b[val+2:])
				ip := net.IP(b[val+4 : val+8])
				return net.JoinHostPort(ip.String(), fmt.Sprint(port)), nil
			}
		}
		pos = val + alen
		if rem := alen % 4; rem != 0 {
			pos += 4 - rem
		}
	}
	return "", errors.New("p2p: no mapped address in STUN reply")
}

// PunchOptions controls hole punching.
type PunchOptions struct {
	// LocalCandidates are our own candidates (from Gather).
	LocalCandidates []Candidate
	// RemoteCandidates are the peer's, received via the relay signalling.
	RemoteCandidates []Candidate
	// Timeout bounds the whole attempt.
	Timeout time.Duration
	// ProbeInterval is how often to repeat punches at one address.
	ProbeInterval time.Duration
	// ConnID identifies the session; punches for other sessions are ignored.
	ConnID uint64
}

// PunchResult reports how punching went.
type PunchResult struct {
	// Remote is the address that answered, nil if nothing did.
	Remote *net.UDPAddr
	// RTT is the measured round trip to that address.
	RTT time.Duration
	// Attempts is how many probe rounds were sent.
	Attempts int
	// Tried lists every candidate and its outcome, for diagnostics.
	Tried []PunchAttempt
}

// PunchAttempt is one candidate's result.
type PunchAttempt struct {
	Addr   string
	Kind   string
	OK     bool
	RTT    time.Duration
	Reason string
}

// Punch tries every remote candidate until one answers.
//
// The algorithm is the simplest thing that works for the common NAT types:
// send a punch to every remote candidate repeatedly, and accept a punch or
// punch-ack from any of them. Because both sides do this at the same time,
// each outbound punch opens its own NAT mapping, which is what allows the
// peer's inbound punch to get through (classic simultaneous-open).
//
// It cannot succeed against a symmetric NAT on both sides, or when UDP is
// filtered; the caller then falls back to the relay.
func Punch(ctx context.Context, udp *net.UDPConn, opts PunchOptions) (*PunchResult, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.ProbeInterval <= 0 {
		opts.ProbeInterval = 250 * time.Millisecond
	}

	deadline := time.Now().Add(opts.Timeout)
	res := &PunchResult{}

	// Resolve candidates up front; unresolvable ones are reported, not fatal.
	type target struct {
		cand Candidate
		addr *net.UDPAddr
	}
	var targets []target
	seen := map[string]bool{}
	for _, c := range opts.RemoteCandidates {
		if seen[c.Addr] {
			continue
		}
		seen[c.Addr] = true
		a, err := net.ResolveUDPAddr("udp", c.Addr)
		if err != nil {
			res.Tried = append(res.Tried, PunchAttempt{Addr: c.Addr, Kind: c.Kind, Reason: err.Error()})
			continue
		}
		targets = append(targets, target{cand: c, addr: a})
	}
	if len(targets) == 0 {
		return res, errors.New("p2p: no usable remote candidates")
	}

	// A dedicated goroutine reads replies while the main loop sends probes.
	// Reading and writing share the socket, so the read deadline is set only
	// in the reader.
	replies := make(chan struct {
		addr *net.UDPAddr
		pkt  *packet
		at   time.Time
	}, 64)

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 2048)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			_ = udp.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, addr, err := udp.ReadFromUDP(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					if time.Now().After(deadline) {
						return
					}
					continue
				}
				return
			}
			pkt, perr := decode(buf[:n])
			if perr != nil {
				continue
			}
			select {
			case replies <- struct {
				addr *net.UDPAddr
				pkt  *packet
				at   time.Time
			}{addr, pkt, time.Now()}:
			default:
			}
		}
	}()
	defer func() {
		_ = udp.SetReadDeadline(time.Now()) // unblock the reader
		<-readDone
	}()

	probe := &packet{typ: pktPunch, connID: opts.ConnID}
	probeData, _ := probe.encode()

	// Track when each target was first probed, to compute RTT on reply.
	firstProbe := map[string]time.Time{}
	attempts := 0

	for {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case r := <-replies:
			// Only punches for our session matter.
			if r.pkt.connID != opts.ConnID {
				continue
			}
			switch r.pkt.typ {
			case pktPunch:
				// Answer so the peer's own check succeeds, then use it.
				ack := &packet{typ: pktPunchAck, connID: opts.ConnID, seq: r.pkt.seq}
				if data, err := ack.encode(); err == nil {
					_, _ = udp.WriteToUDP(data, r.addr)
				}
				res.Remote = r.addr
				res.RTT = measuredRTT(r.addr.String(), firstProbe, r.at)
				res.Tried = append(res.Tried, PunchAttempt{
					Addr: r.addr.String(), Kind: "stun", OK: true, RTT: res.RTT,
				})
				return res, nil
			case pktPunchAck:
				res.Remote = r.addr
				res.RTT = measuredRTT(r.addr.String(), firstProbe, r.at)
				res.Tried = append(res.Tried, PunchAttempt{
					Addr: r.addr.String(), Kind: "stun", OK: true, RTT: res.RTT,
				})
				return res, nil
			}
		case <-time.After(opts.ProbeInterval):
			if time.Now().After(deadline) {
				for _, t := range targets {
					res.Tried = append(res.Tried, PunchAttempt{
						Addr: t.cand.Addr, Kind: t.cand.Kind,
						Reason: "no reply within timeout",
					})
				}
				return res, errors.New("p2p: hole punching timed out")
			}
			attempts++
			for _, t := range targets {
				if _, ok := firstProbe[t.addr.String()]; !ok {
					firstProbe[t.addr.String()] = time.Now()
				}
				_, _ = udp.WriteToUDP(probeData, t.addr)
			}
			res.Attempts = attempts
		}
	}
}

// measuredRTT returns the round trip to addr, or 0 when we never probed it.
//
// The peer sends its own punch at the same time as ours, so the first thing
// that arrives may be its probe rather than a reply to one of ours. Timing
// that from the arrival of an unrelated packet yields a meaningless
// sub-millisecond figure; reporting 0 ("not measured") is honest.
func measuredRTT(key string, probed map[string]time.Time, arrivedAt time.Time) time.Duration {
	t, ok := probed[key]
	if !ok {
		return 0
	}
	return arrivedAt.Sub(t)
}

// randomConnID returns a session identifier.
func randomConnID() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b[:]), nil
}

// NewConnID exposes randomConnID to callers that need to agree on an ID
// before punching (it is exchanged through the relay).
func NewConnID() (uint64, error) { return randomConnID() }

// Listen opens a UDP socket for punching. It binds to an ephemeral port on all
// interfaces, which is required for the STUN mapping to be reachable.
func Listen() (*net.UDPConn, error) {
	return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
}

// DialUDP opens a connected UDP socket to addr.
func DialUDP(addr *net.UDPAddr) (*net.UDPConn, error) {
	return net.DialUDP("udp4", nil, addr)
}
