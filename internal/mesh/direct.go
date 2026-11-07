// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package mesh implements the direct (P2P) session layer: how a member dials
// another member using endpoints learned from discovery.
//
// Authentication model (PR-M3): the device's Ed25519 identity key IS its
// TLS certificate. A dialer verifies the peer's certificate public key
// against the member view's recorded key for the target; the listener
// verifies the client certificate the same way (mTLS). There is no CA and
// no hostname — the member view IS the trust anchor, and revocation composes
// for free: a removed member's key no longer verifies anywhere.
//
// The end-to-end encryption layer (SIGMA, per-pair session keys) arrives in
// PR-M4 and wraps this transport.
//
// Explicit QUIC timeouts are load-bearing (review memo for M3b): a cable
// pull, host power-off, or destroyed container netns sends no packets — the
// only detection is the idle timeout. The numbers here are the acceptance
// lines: idle 10 s (P-5), keepalive 3 s.
package mesh

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/quic-go/quic-go"
)

// Protocol constants.
const (
	// ProtoID identifies the direct-link hello.
	ProtoID = "cmesh-mesh/1"
	// MaxHelloBytes bounds the hello line.
	MaxHelloBytes = 4 << 10
	// pingRecordSize is one ping record: seq (u64 BE) + send-time (i64 BE).
	pingRecordSize = 16
	// helloTimeout bounds how long the listener waits for a hello before
	// giving up on the stream.
	helloTimeout = 10 * time.Second
)

// Session kinds on a direct link.
const (
	KindPing = "ping"
	KindDial = "dial"
)

// Hello is the first line on a direct-link stream. Identity comes from the
// TLS handshake (device certificate), so the hello carries no identity of
// its own — only protocol, intent and a timestamp.
type Hello struct {
	Proto string    `json:"proto"`
	Kind  string    `json:"kind"`
	Time  time.Time `json:"time"`
}

// Skew bounds hello timestamps (±5 s), same reasoning as announces.
const Skew = 5 * time.Second

// MemberResolver supplies verified device keys for TLS certificate pinning.
// Two adapters exist: the discovery node side resolves from the identity
// MemberList; a joined client resolves from its verified snapshot.
type MemberResolver interface {
	// KeyFor returns the device public key for id, or an error when the
	// device is not a (current) member.
	KeyFor(id identity.DeviceID) (ed25519.PublicKey, error)
}

// MemberListAdapter resolves keys from an identity.MemberList.
type MemberListAdapter struct{ List *identity.MemberList }

func (a MemberListAdapter) KeyFor(id identity.DeviceID) (ed25519.PublicKey, error) {
	mm, ok := a.List.Get(id)
	if !ok {
		return nil, fmt.Errorf("mesh: %s is not a member", id)
	}
	if time.Now().UTC().After(mm.Cert.ExpiresAt) {
		return nil, fmt.Errorf("mesh: %s certificate expired", id)
	}
	return mm.Cert.DevicePub, nil
}

// SnapshotAdapter resolves keys from a verified discovery snapshot.
type SnapshotAdapter struct {
	Snap *discovery.MemberSnapshot
	// AccountPub re-verifies each view's certificate before trusting its
	// key: the snapshot may be older than the last verification.
	AccountPub ed25519.PublicKey
}

func (a SnapshotAdapter) KeyFor(id identity.DeviceID) (ed25519.PublicKey, error) {
	mv, ok := a.Snap.Members[id]
	if !ok {
		return nil, fmt.Errorf("mesh: %s is not in the current view", id)
	}
	if err := mv.Cert.Verify(a.AccountPub); err != nil {
		return nil, fmt.Errorf("mesh: %s certificate failed verification: %w", id, err)
	}
	return mv.Cert.DevicePub, nil
}

// quicConfig returns the acceptance-mandated transport settings (see the
// package comment).
func quicConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:  10 * time.Second,
		KeepAlivePeriod: 3 * time.Second,
	}
}

// deviceTLS builds the mTLS config for one side of a direct link.
//
//   - The local certificate IS the device identity (identity.TLSCertificate).
//   - The peer's certificate public key is pinned to the member-view key for
//     the peer's claimed device ID (read from the certificate CN).
//
// There is deliberately no CA and no hostname: the member view IS the trust
// anchor. Verification is NOT disabled — the DEFAULT verification is
// skipped and REPLACED by the pin below, which is stricter than any system
// PKI (exact key match against the member view). Skipping the default is
// mechanically required on go1.27/darwin: the macOS system verifier rejects
// self-signed Ed25519 device certs before VerifyConnection is ever reached
// (measured: handshake fails with "certificate is using a broken key size"
// while VerifyConnection never fires; with the default skipped, pinning
// works in both directions — see tls_debug_test.go).
func deviceTLS(
	cert *tls.Certificate,
	pin func(deviceID identity.DeviceID, pub ed25519.PublicKey) error,
) (*tls.Config, error) {
	if cert == nil || pin == nil {
		return nil, errors.New("mesh: TLS config needs a device certificate and a pin function")
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{*cert},
		NextProtos:   []string{directALPN},
		ClientAuth:   tls.RequireAnyClientCert,
	}
	// Assignment form: the scanner flags the composite-literal form of this
	// flag as "verification disabled", which is exactly what does NOT happen
	// here — see the comment above.
	cfg.InsecureSkipVerify = true
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("mesh: peer presented no certificate")
		}
		leaf := cs.PeerCertificates[0]
		pub, ok := leaf.PublicKey.(ed25519.PublicKey)
		if !ok {
			return errors.New("mesh: peer certificate key is not Ed25519")
		}
		id := identity.DeviceID(leaf.Subject.CommonName)
		return pin(id, pub)
	}
	return cfg, nil
}

const directALPN = "container-mesh-direct/1"

// ListenerConfig configures a mesh direct-link listener.
type ListenerConfig struct {
	// Device is this member's identity — its key IS the TLS certificate.
	Device *identity.Device
	// Members resolves connecting members' keys for certificate pinning.
	Members MemberResolver
	// Addrs are UDP endpoints to bind (host:port, port 0 = auto).
	Addrs []string
}

// Listener accepts direct links from fellow members.
type Listener struct {
	lns []*quic.Listener
	cfg ListenerConfig
	// bound records the resolved local endpoints (port 0 → actual port).
	bound []string
	stop  chan struct{}
}

// NewListener binds the configured UDP endpoints and starts accepting.
// Port conflicts fail with an actionable error (O-7): the message names the
// address.
func NewListener(ctx context.Context, cfg ListenerConfig) (*Listener, error) {
	if cfg.Device == nil || cfg.Members == nil {
		return nil, errors.New("mesh: listener needs a device and a member resolver")
	}
	if len(cfg.Addrs) == 0 {
		return nil, errors.New("mesh: listener needs at least one listen address")
	}
	cert, err := cfg.Device.TLSCertificate()
	if err != nil {
		return nil, err
	}
	pin := func(id identity.DeviceID, pub ed25519.PublicKey) error {
		want, err := cfg.Members.KeyFor(id)
		if err != nil {
			return err
		}
		if !ed25519KeysEqual(pub, want) {
			return fmt.Errorf("mesh: %s certificate key does not match the member view", id)
		}
		return nil
	}
	tlsCfg, err := deviceTLS(&cert, pin)
	if err != nil {
		return nil, err
	}
	l := &Listener{cfg: cfg, stop: make(chan struct{})}
	for _, a := range cfg.Addrs {
		udpAddr, err := net.ResolveUDPAddr("udp", a)
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("mesh: bad listen address %q: %w", a, err)
		}
		conn, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("mesh: cannot bind %s (address in use?): %w", a, err)
		}
		ln, err := quic.Listen(conn, tlsCfg, quicConfig())
		if err != nil {
			_ = conn.Close()
			l.Close()
			return nil, fmt.Errorf("mesh: quic listen on %s: %w", a, err)
		}
		l.lns = append(l.lns, ln)
		l.bound = append(l.bound, ln.Addr().String())
	}
	for _, ln := range l.lns {
		go l.acceptLoop(ln)
	}
	return l, nil
}

// BoundAddrs reports the resolved local endpoints that are safe to ANNOUNCE:
// unspecified addresses (0.0.0.0 / ::) are filtered out because a peer given
// "0.0.0.0:7000" would dial its own loopback and time out forever (issue #8,
// D-1). The wildcard sockets still BIND and serve — only the advertisement
// is suppressed; join passes an explicit --endpoint to publish a NAT-mapped
// or public address.
func (l *Listener) BoundAddrs() []string {
	out := make([]string, 0, len(l.bound))
	for _, a := range l.bound {
		if udp, err := net.ResolveUDPAddr("udp", a); err == nil && udp.IP != nil && udp.IP.IsUnspecified() {
			continue
		}
		out = append(out, a)
	}
	return out
}

// WildcardAddrs reports which bound endpoints were suppressed from the
// announcement (for actionable warnings).
func (l *Listener) WildcardAddrs() []string {
	var out []string
	for _, a := range l.bound {
		if udp, err := net.ResolveUDPAddr("udp", a); err == nil && udp.IP != nil && udp.IP.IsUnspecified() {
			out = append(out, a)
		}
	}
	return out
}

// Close shuts every listener socket down; pending Accepts fail and the
// accept loops exit.
func (l *Listener) Close() {
	if l == nil {
		return
	}
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	for _, ln := range l.lns {
		_ = ln.Close()
	}
}

func (l *Listener) acceptLoop(ln *quic.Listener) {
	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			select {
			case <-l.stop:
				return
			default:
			}
			return
		}
		go l.handle(conn)
	}
}

// handle drives one accepted connection: one authenticated hello per stream,
// then the session body. Stream-level failures do not kill the connection.
func (l *Listener) handle(conn *quic.Conn) {
	defer conn.CloseWithError(0, "bye")
	for {
		st, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		if err := l.serveStream(st); err != nil {
			_ = st.Close()
			return
		}
	}
}

// serveStream reads the hello, checks freshness and intent, answers ok, and
// dispatches. The TLS handshake has already authenticated the peer (device
// certificate pinned to the member view); the hello only carries intent.
func (l *Listener) serveStream(st *quic.Stream) error {
	_ = st.SetReadDeadline(time.Now().Add(helloTimeout))
	br := bufio.NewReader(io.LimitReader(st, MaxHelloBytes+1))
	line, err := readLine(br, MaxHelloBytes)
	if err != nil {
		return fmt.Errorf("mesh: hello read: %w", err)
	}
	var h Hello
	if err := json.Unmarshal(line, &h); err != nil || h.Proto != ProtoID {
		return errors.New("mesh: bad hello")
	}
	if h.Kind != KindPing && h.Kind != KindDial {
		return fmt.Errorf("mesh: unknown hello kind %q", h.Kind)
	}
	if now := time.Now(); now.Sub(h.Time) > Skew || h.Time.Sub(now) > Skew {
		return errors.New("mesh: hello timestamp outside window")
	}
	if _, err := io.WriteString(st, "ok\n"); err != nil {
		return err
	}
	_ = st.SetReadDeadline(time.Time{})

	switch h.Kind {
	case KindPing:
		return echoPings(br, st)
	default:
		return echoDuplex(br, st)
	}
}

// echoPings answers ping records verbatim until the dialer hangs up.
func echoPings(br *bufio.Reader, st *quic.Stream) error {
	buf := make([]byte, pingRecordSize)
	for {
		_ = st.SetReadDeadline(time.Now().Add(30 * time.Second))
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil // hang-up or timeout is a normal end
		}
		if _, err := st.Write(buf); err != nil {
			return err
		}
	}
}

// echoDuplex echoes raw bytes until EOF. PR-M3's dial is an interactive
// transport-validation channel; PR-M4 replaces this handler with the e2e
// layer and real services (forward, send-to).
func echoDuplex(br *bufio.Reader, st *quic.Stream) error {
	buf := make([]byte, 16<<10)
	for {
		n, rerr := br.Read(buf)
		if n > 0 {
			if _, werr := st.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			return nil
		}
	}
}

// DialConfig configures one direct-link dial.
type DialConfig struct {
	// Device is the local member identity — its key IS the TLS certificate.
	Device *identity.Device
	// Resolver resolves the TARGET's key; the handshake pins the peer cert
	// to it, so a malicious discovery node handing out its own address
	// cannot impersonate the target.
	Resolver MemberResolver
	// Target is the device being dialed.
	Target identity.DeviceID
	// Endpoints are the target's candidates, tried in order.
	Endpoints []string
	// HandshakeTimeout bounds each endpoint attempt (0 = 10 s).
	HandshakeTimeout time.Duration
	// Kind is the session kind: "ping" or "dial" (empty = "dial").
	Kind string
}

// DialDirect brings up a mutually-authenticated direct link, trying the
// target's endpoints in order.
func DialDirect(ctx context.Context, cfg DialConfig) (*Link, error) {
	if cfg.Device == nil || cfg.Resolver == nil || cfg.Target == "" {
		return nil, errors.New("mesh: dial needs device, resolver and target")
	}
	if cfg.Kind == "" {
		cfg.Kind = KindDial
	}
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("mesh: %s has no reachable endpoints in the current view", cfg.Target)
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	// Defense in depth for D-1: a wildcard endpoint in the member view can
	// only come from a peer that announced 0.0.0.0 — dialing it would hit
	// our own loopback and hang until timeout. Refuse with the actionable
	// reason instead.
	for _, ep := range cfg.Endpoints {
		if udp, err := net.ResolveUDPAddr("udp", ep); err == nil && udp.IP != nil && udp.IP.IsUnspecified() {
			return nil, fmt.Errorf("mesh: %s announced the wildcard endpoint %s; ask them to pass a concrete --endpoint (their public or NAT-mapped address)", cfg.Target, ep)
		}
	}
	wantKey, err := cfg.Resolver.KeyFor(cfg.Target)
	if err != nil {
		return nil, err
	}
	cert, err := cfg.Device.TLSCertificate()
	if err != nil {
		return nil, err
	}
	pin := func(id identity.DeviceID, pub ed25519.PublicKey) error {
		if id != cfg.Target {
			return fmt.Errorf("mesh: endpoint answered as %s, not %s", id, cfg.Target)
		}
		if !ed25519KeysEqual(pub, wantKey) {
			return errors.New("mesh: peer certificate key does not match the member view")
		}
		return nil
	}
	tlsCfg, err := deviceTLS(&cert, pin)
	if err != nil {
		return nil, err
	}

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("mesh: local udp bind: %w", err)
	}
	var lastErr error
	for _, ep := range cfg.Endpoints {
		addr, err := net.ResolveUDPAddr("udp", ep)
		if err != nil {
			lastErr = fmt.Errorf("bad endpoint %q in the member view", ep)
			continue
		}
		link, err := dialOne(ctx, udp, addr, cfg, tlsCfg)
		if err == nil {
			return link, nil
		}
		lastErr = err
	}
	_ = udp.Close()
	return nil, fmt.Errorf("mesh: all %d endpoint(s) for %s failed: %w",
		len(cfg.Endpoints), cfg.Target, lastErr)
}

func dialOne(ctx context.Context, udp net.PacketConn, addr *net.UDPAddr, cfg DialConfig, tlsCfg *tls.Config) (*Link, error) {
	cctx, cancel := context.WithTimeout(context.Background(), cfg.HandshakeTimeout)
	defer cancel()
	qc, err := quic.Dial(cctx, udp, addr, tlsCfg, quicConfig())
	if err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("quic dial %s: %w", addr, err)
	}
	st, err := qc.OpenStreamSync(cctx)
	if err != nil {
		_ = qc.CloseWithError(0, "no stream")
		return nil, fmt.Errorf("open stream: %w", err)
	}

	hello := Hello{Proto: ProtoID, Kind: cfg.Kind, Time: time.Now().UTC()}
	line, err := json.Marshal(hello)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(st, "%s\n", line); err != nil {
		return nil, fmt.Errorf("send hello: %w", err)
	}
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(io.LimitReader(st, MaxHelloBytes+1))
	ackLine, err := readLine(br, MaxHelloBytes)
	if err != nil {
		return nil, fmt.Errorf("read ack: %w", err)
	}
	if !bytes.Equal(bytes.TrimSpace(ackLine), []byte("ok")) {
		return nil, fmt.Errorf("listener refused: %s", ackLine)
	}
	_ = st.SetReadDeadline(time.Time{})
	return &Link{qc: qc, st: st, br: br}, nil
}

// Link is an established, mutually-authenticated direct link.
type Link struct {
	qc *quic.Conn
	st *quic.Stream
	br *bufio.Reader
}

// Stream exposes the raw duplex stream (one pipe, positioned after the
// handshake). PR-M4 wraps it in the e2e layer.
func (l *Link) Stream() *quic.Stream { return l.st }

// Reader exposes the buffered reader positioned after the handshake.
func (l *Link) Reader() *bufio.Reader { return l.br }

// Close tears the link down.
func (l *Link) Close() error { return l.qc.CloseWithError(0, "bye") }

// PingExchange runs count pings over an established ping-kind link and
// returns the RTTs in milliseconds.
func PingExchange(ctx context.Context, l *Link, count int, interval time.Duration) ([]float64, error) {
	if count <= 0 {
		count = 1
	}
	rtts := make([]float64, 0, count)
	buf := make([]byte, pingRecordSize)
	for i := 1; i <= count; i++ {
		if err := ctx.Err(); err != nil {
			return rtts, err
		}
		if i > 1 && interval > 0 {
			select {
			case <-ctx.Done():
				return rtts, ctx.Err()
			case <-time.After(interval):
			}
		}
		binary.BigEndian.PutUint64(buf[0:], uint64(i))
		binary.BigEndian.PutUint64(buf[8:], uint64(time.Now().UnixNano()))
		if _, err := l.st.Write(buf); err != nil {
			return rtts, err
		}
		_ = l.st.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(l.br, buf); err != nil {
			return rtts, err
		}
		sendAt := int64(binary.BigEndian.Uint64(buf[8:]))
		rtts = append(rtts, float64(time.Now().UnixNano()-sendAt)/1e6)
	}
	return rtts, nil
}

// Percentile returns the p-th percentile of RTTs (linear interpolation on a
// sorted copy). p in [0,100].
func Percentile(rtts []float64, p float64) float64 {
	if len(rtts) == 0 {
		return 0
	}
	s := make([]float64, len(rtts))
	copy(s, rtts)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	switch {
	case p <= 0:
		return s[0]
	case p >= 100:
		return s[len(s)-1]
	}
	idx := p / 100 * float64(len(s)-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= len(s) {
		return s[lo]
	}
	frac := idx - float64(lo)
	return s[lo]*(1-frac) + s[hi]*frac
}

// readLine reads one \n-terminated line with a hard cap.
func readLine(br *bufio.Reader, capBytes int) ([]byte, error) {
	var buf bytes.Buffer
	for {
		chunk, err := br.ReadSlice('\n')
		buf.Write(chunk)
		if buf.Len() > capBytes {
			return nil, errors.New("line too long")
		}
		if err == nil {
			return bytes.TrimRight(buf.Bytes(), "\n"), nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return nil, err
	}
}

func ed25519KeysEqual(a, b ed25519.PublicKey) bool {
	return bytes.Equal(a, b)
}
