// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
)

// probeNet checks the four things that matter for a userspace transport:
// DNS, UDP egress, TCP egress, and the ability to bind a local port.
func probeNet(opts Options) NetReport {
	var n NetReport
	to := opts.NetworkTimeout
	if to <= 0 {
		to = 3 * time.Second
	}

	n.DNSResolution = checkDNS(to)
	n.TCPOutbound = checkTCP(opts.TCPAddr, to)
	n.UDPOutbound = checkUDP(opts.UDPServer, to)
	n.LocalhostListen = checkListen()

	// STUN bindings tell us whether we are behind NAT and what the public
	// address looks like. Several servers are queried in parallel — including
	// Chinese and international ones — because reachability differs per
	// region and the divergence is itself useful information. This is
	// informational only.
	n.STUN = stunProbe(stunServerList(opts), to)
	for _, r := range n.STUN {
		if r.OK {
			n.PublicIPViaSTUN = r.Result
			n.EffectiveNATNote = "STUN binding succeeded via " + r.Server + "; outbound UDP is not blocked"
			break
		}
	}

	// Outbound usability must not depend on a single hard-coded target. A
	// server inside China routinely cannot reach 1.1.1.1:53/443 while its
	// egress works fine — an earlier version called such hosts "unusable",
	// which is actively misleading. Any of these is proof of egress:
	//   - a completed TCP handshake,
	//   - a UDP datagram that got a reply,
	//   - a STUN binding (proves UDP egress to a public host),
	//   - successful DNS resolution (proves a working resolver path).
	anySTUN := false
	for _, r := range n.STUN {
		if r.OK {
			anySTUN = true
			break
		}
	}
	n.OutboundUsable = n.TCPOutbound.OK || n.UDPOutbound.OK || anySTUN || n.DNSResolution.OK

	if n.PublicIPViaSTUN == "" {
		n.STUNError = "no STUN server answered"
		if n.UDPOutbound.OK {
			n.EffectiveNATNote = "UDP works but STUN did not answer; NAT type unknown"
		} else if n.OutboundUsable {
			n.EffectiveNATNote = "outbound works (TCP/DNS) but no STUN reply; UDP may be filtered"
		} else {
			n.EffectiveNATNote = "no UDP egress"
		}
	}
	return n
}

// STUNResult is the outcome of one STUN binding attempt.
type STUNResult struct {
	Server  string `json:"server"`
	OK      bool   `json:"ok"`
	Latency string `json:"latency,omitempty"`
	Result  string `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
}

// defaultSTUNServers mixes Chinese and international public servers. Domestic
// ones come first: from inside China they answer fastest, and a successful
// binding there says more about the local NAT than a far-away server would.
var defaultSTUNServers = []string{
	"stun.miwifi.com:3478",
	"stun.qq.com:3478",
	"stun.cloudflare.com:3478",
	"stun.l.google.com:19302",
}

func stunServerList(opts Options) []string {
	if len(opts.STUNServers) > 0 {
		return opts.STUNServers
	}
	return defaultSTUNServers
}

// stunProbe queries every server in parallel and returns one result each,
// in input order.
func stunProbe(servers []string, timeout time.Duration) []STUNResult {
	results := make([]STUNResult, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func(i int, srv string) {
			defer wg.Done()
			start := time.Now()
			ip, err := stunOnce(srv, timeout)
			r := STUNResult{Server: srv, Latency: time.Since(start).Round(time.Millisecond).String()}
			if err != nil {
				r.Error = err.Error()
			} else {
				r.OK = true
				r.Result = ip
			}
			results[i] = r
		}(i, srv)
	}
	wg.Wait()
	return results
}

func checkDNS(timeout time.Duration) CheckResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, "example.com")
	lat := time.Since(start).Round(time.Millisecond).String()
	if err != nil {
		return CheckResult{OK: false, Target: "example.com", Latency: lat, Error: err.Error()}
	}
	return CheckResult{OK: true, Target: "example.com", Latency: lat,
		Detail: fmt.Sprintf("resolved %d address(es)", len(addrs))}
}

func checkTCP(addr string, timeout time.Duration) CheckResult {
	if addr == "" {
		addr = "1.1.1.1:443"
	}
	start := time.Now()
	d := net.Dialer{Timeout: timeout}
	c, err := d.Dial("tcp", addr)
	lat := time.Since(start).Round(time.Millisecond).String()
	if err != nil {
		return CheckResult{OK: false, Target: addr, Latency: lat, Error: err.Error()}
	}
	_ = c.Close()
	return CheckResult{OK: true, Target: addr, Latency: lat, Detail: "TCP handshake completed"}
}

func checkUDP(addr string, timeout time.Duration) CheckResult {
	if addr == "" {
		addr = "1.1.1.1:53"
	}
	c, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return CheckResult{OK: false, Target: addr, Error: err.Error()}
	}
	defer c.Close()

	// Build a minimal DNS query for "example.com" A record.
	q := buildDNSQuery(0x1234)
	start := time.Now()
	if _, err := c.Write(q); err != nil {
		return CheckResult{OK: false, Target: addr, Error: err.Error()}
	}
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	lat := time.Since(start).Round(time.Millisecond).String()
	if err != nil {
		// A UDP write that succeeds with no reply means egress is open but
		// the destination is silent (common with filtered 53/udp). Report it
		// as a soft failure with a clear detail so the caller can tell the
		// difference between "no route" and "no answer".
		return CheckResult{OK: false, Target: addr, Latency: lat,
			Error: err.Error(), Detail: "datagram sent but no reply (egress may still be open)"}
	}
	if n < 12 {
		return CheckResult{OK: false, Target: addr, Latency: lat, Error: "short DNS reply"}
	}
	return CheckResult{OK: true, Target: addr, Latency: lat, Detail: "DNS reply received"}
}

func checkListen() CheckResult {
	// Binding a loopback port is what the relay client needs for local
	// sidecar patterns; it also catches exotic seccomp profiles.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return CheckResult{OK: false, Target: "127.0.0.1:0", Error: err.Error()}
	}
	defer l.Close()
	return CheckResult{OK: true, Target: l.Addr().String(), Detail: "loopback bind succeeded"}
}

func buildDNSQuery(id uint16) []byte {
	b := make([]byte, 12, 12+4+14)
	binary.BigEndian.PutUint16(b[0:], id)
	binary.BigEndian.PutUint16(b[2:], 0x0100) // standard query, recursion desired
	binary.BigEndian.PutUint16(b[4:], 1)      // QDCOUNT
	for _, label := range []string{"example", "com"} {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	b = append(b, 0)    // root label
	b = append(b, 0, 1) // QTYPE A
	b = append(b, 0, 1) // QCLASS IN
	return b
}

// stunOnce tries every address a STUN hostname resolves to, IPv4 first.
//
// Dialling the hostname directly is not enough: on a dual-stack machine Go may
// pick an IPv6 address whose UDP path is blocked, and the attempt then burns
// the whole deadline even though IPv4 works.
func stunOnce(server string, timeout time.Duration) (string, error) {
	host, port, err := net.SplitHostPort(server)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}

	ordered := make([]net.IPAddr, 0, len(addrs))
	for _, a := range addrs {
		if a.IP.To4() != nil {
			ordered = append(ordered, a)
		}
	}
	for _, a := range addrs {
		if a.IP.To4() == nil {
			ordered = append(ordered, a)
		}
	}
	if len(ordered) == 0 {
		return "", fmt.Errorf("no addresses for %s", host)
	}

	var lastErr error
	for _, a := range ordered {
		ip, err := stunExchange(net.JoinHostPort(a.IP.String(), port), timeout)
		if err == nil {
			return ip, nil
		}
		lastErr = err
	}
	return "", lastErr
}

// stunExchange performs one binding request against a concrete address.
func stunExchange(server string, timeout time.Duration) (string, error) {
	c, err := net.DialTimeout("udp", server, timeout)
	if err != nil {
		return "", err
	}
	defer c.Close()

	// STUN binding request: type 0x0001, length 0, magic cookie, 12-byte txid.
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001)
	binary.BigEndian.PutUint16(req[2:], 0)
	binary.BigEndian.PutUint32(req[4:], 0x2112A442)
	// A predictable transaction id lets an off-path attacker forge the
	// binding reply and feed us a bogus public address, so it must come from
	// the cryptographic source. (RFC 5389 requires 96 bits of randomness.)
	txid := make([]byte, 12)
	if _, err := rand.Read(txid); err != nil {
		return "", err
	}
	copy(req[8:], txid)

	if _, err := c.Write(req); err != nil {
		return "", err
	}
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 1024)
	n, err := c.Read(buf)
	if err != nil {
		return "", err
	}
	if n < 20 {
		return "", fmt.Errorf("short STUN reply (%d bytes)", n)
	}
	if binary.BigEndian.Uint16(buf[0:]) != 0x0101 {
		return "", fmt.Errorf("unexpected STUN message type 0x%04x", binary.BigEndian.Uint16(buf[0:]))
	}
	// The transaction ID must echo ours, otherwise the reply belongs to some
	// other request and its address is not ours.
	if !bytes.Equal(buf[8:20], txid) {
		return "", fmt.Errorf("STUN transaction id mismatch")
	}
	// Walk attributes looking for XOR-MAPPED-ADDRESS (0x0020) or
	// MAPPED-ADDRESS (0x0001).
	pos := 20
	for pos+4 <= n {
		atype := binary.BigEndian.Uint16(buf[pos:])
		alen := int(binary.BigEndian.Uint16(buf[pos+2:]))
		val := pos + 4
		if val+alen > n {
			break
		}
		switch atype {
		case 0x0020:
			if alen >= 8 && buf[val+1] == 0x01 {
				port := binary.BigEndian.Uint16(buf[val+2:]) ^ 0x2112
				ip := make(net.IP, 4)
				for i := 0; i < 4; i++ {
					ip[i] = buf[val+4+i] ^ byte(uint32(0x2112A442)>>(8*(3-i)))
				}
				return fmt.Sprintf("%s:%d", ip, port), nil
			}
		case 0x0001:
			if alen >= 8 && buf[val+1] == 0x01 {
				port := binary.BigEndian.Uint16(buf[val+2:])
				ip := net.IP(buf[val+4 : val+8])
				return fmt.Sprintf("%s:%d", ip, port), nil
			}
		}
		pos = val + alen
		// attributes are padded to 4 bytes
		if rem := alen % 4; rem != 0 {
			pos += 4 - rem
		}
	}
	return "", fmt.Errorf("no mapped address attribute in STUN reply")
}
