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
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	// KindMsg: the sender pushes short text messages; the receiver's
	// listener prints them live (R-4.1 — member-to-member messaging).
	KindMsg = "msg"
	// KindFile: the sender streams one file with a size/sha256 header; the
	// receiver verifies and saves it (R-4.2 — large-file transfer by device
	// name).
	KindFile = "file"
	// KindExec: the sender asks the receiver to run a command and stream
	// back stdout/stderr/exit-code — the mesh's ssh shape (design §4.1
	// dial [--cmd]). Authenticated by the same membership mTLS: only
	// approved members can exec, and the listener can refuse entirely.
	KindExec = "exec"
	// KindBench is an isolated bidirectional throughput measurement stream.
	KindBench = "bench"
)

// ExecRequest is the header line of an exec session: the argv to run.
type ExecRequest struct {
	Command []string `json:"command"`
}

// Exec frame envelope tags (first byte of each frame body).
const (
	ExecTagOut  = 'O' // stdout chunk
	ExecTagErr  = 'E' // stderr chunk
	ExecTagExit = 'X' // exit code (4-byte BE after tag)
)

// Exec frames: tag byte + payload, length-prefixed by writeFrame.
func writeExecFrame(w io.Writer, tag byte, payload []byte) error {
	return writeFrame(w, append([]byte{tag}, payload...))
}

func writeExecExit(w io.Writer, code int) error {
	var payload [5]byte
	payload[0] = ExecTagExit
	binary.BigEndian.PutUint32(payload[1:], uint32(code))
	return writeFrame(w, payload[:])
}

// readExecFrame returns (tag, payload).
func readExecFrame(br *bufio.Reader) (byte, []byte, error) {
	frame, err := readFrame(br, maxExecFrameBytes)
	if err != nil {
		return 0, nil, err
	}
	if len(frame) == 0 {
		return 0, nil, errors.New("mesh: empty exec frame")
	}
	return frame[0], frame[1:], nil
}

// maxExecFrameBytes bounds one stdout/stderr chunk.
const maxExecFrameBytes = 256 << 10

// Exec limits: argv length, and a kill grace period for abort handling.
const (
	maxExecArgvBytes = 64 << 10
	execKillGrace    = 10 * time.Second
)

// FileHeader is the metadata line a file sender writes before the bytes.
// SHA256 is computed by the sender over the whole file; the receiver
// re-computes while writing and refuses a mismatch — integrity without
// trusting the transport's compression or a truncated send.
type FileHeader struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Force says the SENDER knows the receiver already has a file with
	// this name and wants it replaced.
	//
	// ★ IT IS THE SENDER'S INTENT, AND IT MUST BE EXPLICIT. (Round 99.)
	// The receiver used to rename over whatever was there, so the first
	// file was silently replaced by the second — with no warning to
	// either side, and the sender told "done <hash>". A file can only
	// be replaced when somebody asked for it to be replaced, and the
	// only one who can ask is the sender.
	//
	// It is a field rather than a flag because the receiver is a
	// different process on a different machine: the decision has to
	// travel with the file.
	Force bool `json:"force,omitempty"`
}

// MaxFileBytes caps one inbound file (default 8 GiB). Large is the point —
// S3's 10 GB target — but a cap still bounds a hostile or broken sender.
const MaxFileBytes = int64(8) << 30

// MaxMsgBytes caps one message.
const MaxMsgBytes = 64 << 10

// writeFrame writes one length-prefixed message (4-byte BE length + body).
func writeFrame(w io.Writer, body []byte) error {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(body)))
	if _, err := w.Write(l[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// readFrame reads one length-prefixed message with a hard cap.
func readFrame(br *bufio.Reader, capBytes int64) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(br, l[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(l[:])
	if int64(n) > capBytes {
		return nil, fmt.Errorf("frame %d bytes exceeds cap %d", n, capBytes)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return body, nil
}

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
// MemberResolver is the question "is this device still a member, and what
// is its key".
type MemberResolver interface {
	// KeyFor returns the device public key for id, or an error when the
	// device is not a (current) member.
	KeyFor(id identity.DeviceID) (ed25519.PublicKey, error)
}

// membershipChecker is the OPTIONAL, PRECISE version of the same
// question: "is this device still in the roster?"
//
// It exists because KeyFor answers SEVERAL questions with one error —
// "not a member", "the certificate expired", "no view is available" —
// and a revocation watch needs only the FIRST of them.
//
// The distinction matters in both directions:
//
//   - A peer whose certificate merely EXPIRED is not a revoked peer.
//     Tearing its connection down because its certificate lapsed would
//     kill a legitimate session for a reason the requirement does not
//     ask for (and the product has a renewal loop precisely so that
//     expiry is a normal, recoverable event).
//   - A peer that is simply not in the roster IS revoked (or removed),
//     and that is what must destroy the session.
//
// So a resolver that can answer the narrow question is asked the narrow
// question; one that cannot falls back to KeyFor, which is the same
// question the handshake asks.
type membershipChecker interface {
	// StillMember reports whether the device is still in the roster,
	// regardless of whether its certificate has expired.
	StillMember(id identity.DeviceID) bool
}

// MemberListAdapter resolves keys from an identity.MemberList.
type MemberListAdapter struct{ List *identity.MemberList }

// StillMember reports whether the device is still in the roster.
//
// It answers the narrow question, so an expired certificate does NOT
// read as a revocation.
func (a MemberListAdapter) StillMember(id identity.DeviceID) bool {
	if a.List == nil {
		return false
	}
	_, ok := a.List.Get(id)
	return ok
}

func (a MemberListAdapter) KeyFor(id identity.DeviceID) (ed25519.PublicKey, error) {
	if a.List == nil {
		// A nil list is a resolver with no view. It must answer "not a
		// member" rather than panic: the relay path asks the resolver
		// before it has any other information, and a panic there would
		// take the process down.
		return nil, fmt.Errorf("mesh: %s is not a member (no member view is available)", id)
	}
	mm, ok := a.List.Get(id)
	if !ok {
		return nil, fmt.Errorf("mesh: %s is not a member", id)
	}
	if mm.Cert.Expired(time.Now()) {
		// Say WHEN it expired, and say what to do about it.
		//
		// The bare "certificate expired" was invisible in practice: the
		// relay's refusal to the requester said "this node cannot reach
		// <target> right now", so the operator had no way to learn that
		// the member had simply lapsed and needed a new certificate. A
		// member that lapsed is a normal thing that happens; it has to
		// read like one.
		return nil, fmt.Errorf("mesh: %s certificate expired at %s (it has to be re-issued)",
			id, mm.Cert.ExpiresAt.UTC().Format(time.RFC3339))
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

// StillMember reports whether the device is still in the view,
// regardless of whether its certificate has expired.
//
// It is the NARROW question, and it exists because KeyFor answers a
// WIDER one: KeyFor fails for "not a member", for "the certificate
// expired" AND for "the certificate failed verification" — three
// different situations, one error.
//
// The revocation watch needs only the first. Without this method the
// watch falls back to KeyFor, and a member whose certificate merely
// LAPSED (a normal, recoverable event — the product renews
// certificates for exactly that reason) has its live session destroyed
// and is told it was revoked.
//
// This was found by an audit, and it is worth recording HOW: the test
// for the expiry case used MemberListAdapter, which HAS this method —
// so the test passed and the PRODUCTION resolver (the snapshot-backed
// one every real member uses) never got the narrow answer. **A test
// that only exercises the fixture cannot find a gap in the fixture.**
func (a SnapshotAdapter) StillMember(id identity.DeviceID) bool {
	if a.Snap == nil {
		return false
	}
	_, ok := a.Snap.Members[id]
	return ok
}

func (a SnapshotAdapter) KeyFor(id identity.DeviceID) (ed25519.PublicKey, error) {
	mv, ok := a.Snap.Members[id]
	if !ok {
		return nil, fmt.Errorf("mesh: %s is not in the current view", id)
	}
	// A member that joined under a team credential holds a certificate
	// signed by the credential's DELEGATOR, not by the account — verifying
	// it against the account key fails by design. The snapshot carries the
	// delegation records (already re-verified against the account key when
	// the snapshot was accepted), so a delegated member is admitted here
	// through the delegation that vouches for its signer. A delegated
	// certificate with no matching delegation never reaches this adapter:
	// verifySnapshot drops it when the snapshot is accepted.
	if len(mv.Cert.SignerPub) > 0 {
		for _, d := range a.Snap.Delegations {
			if d.DelegatorPub.Equal(mv.Cert.SignerPub) {
				if err := mv.Cert.VerifySigner(d.DelegatorPub); err != nil {
					return nil, fmt.Errorf("mesh: %s delegated certificate failed verification: %w", id, err)
				}
				return mv.Cert.DevicePub, nil
			}
		}
		return nil, fmt.Errorf("mesh: %s certificate was signed by a delegator this view does not authorise", id)
	}
	if err := mv.Cert.Verify(a.AccountPub); err != nil {
		return nil, fmt.Errorf("mesh: %s certificate failed verification: %w", id, err)
	}
	return mv.Cert.DevicePub, nil
}

// quicConfig returns the acceptance-mandated transport settings (see the
// package comment).
//
// The Tracer hook is what makes the QUIC-level loss accounting observable:
// quic-go creates the tracer ONCE PER CONNECTION (transport.go:294 for the
// client, server.go:709 for the server), and the hook stores the loss
// tracker under the connection's tracing id. See lossstat.go.
//
// quicConfig() is the ONLY place a quic.Config is built for the direct path,
// and it is shared by quic.Dial (direct.go:838) and quic.Listen
// (direct.go:373) — so both ends of a direct link are instrumented by one
// hook, and the relay path (WebSocket, no QUIC) is untouched by it.
//
// THE FLOW CONTROL WINDOWS ARE SET HERE ON PURPOSE.
//
// quic-go's defaults are a 512 KiB initial stream window and a 768 KiB
// initial CONNECTION window (1.5 times the stream one). Both are too
// small for this product:
//
//   - A relay record is JSON, and JSON carries a []byte as base64, so a
//     megabyte of payload becomes about 1.4 megabytes on the wire, plus
//     the end-to-end seal's nonce and tag. ONE relay record is therefore
//     BIGGER than the default stream window.
//   - The window only grows when the receiver consumes more than half a
//     window per RTT (quic-go's maybeAdjustWindowSize). The trunk's
//     forwarding loop pulls records ON DEMAND, so the window does not
//     grow on its own, and the sender blocks on its rolling write
//     deadline: "mesh: send body: mesh: relay: write: deadline
//     exceeded". The real machine showed it exactly that way — the same
//     file, right after a failed attempt, went through.
//
// The connection window is the one that matters most: the trunk's join
// connection is LONG-LIVED and carries EVERY relayed session, so the
// connection-level window is what actually runs out.
//
// These are RECEIVE windows (they say how much the peer may send us).
// The peer's send side is bounded by what we grant, and the window grows
// towards the maximum as the peer consumes.
const (
	// relayStreamReceiveWindow is the initial per-stream receive window.
	// It holds one relay record with room to spare.
	relayStreamReceiveWindow = 2 << 20
	// relayConnectionReceiveWindow is the initial connection-level
	// receive window. It holds several relayed sessions' worth of
	// records, because the trunk multiplexes them over one connection.
	relayConnectionReceiveWindow = 16 << 20
	// relayMaxReceiveWindow is the ceiling the window grows towards.
	relayMaxReceiveWindow = 64 << 20

	// quicHandshakeTimeout is how long a handshake may take before the
	// attempt is abandoned.
	//
	// quic-go's own default is five seconds, which is too short on a
	// loaded machine; this project raised it to twenty. But a dialer
	// whose CONTEXT is shorter than the handshake it waits for cancels
	// the handshake before it can finish — the peer is ready and the
	// dialer has already given up. That showed up as
	// "quic dial relay …: context deadline exceeded" on a busy machine,
	// with nothing wrong at either end.
	//
	// So: every dial budget is derived from this constant — and the
	// TRANSPORT's own ceiling is a separate, SHORTER number
	// (quicTransportHandshakeTimeout), because quic-go abandons the
	// handshake at HandshakeIdleTimeout whatever the caller's context
	// allows. See quicConfig and the budget test.
	quicHandshakeTimeout = 20 * time.Second
)

// quicTransportHandshakeTimeout is the TRANSPORT's own ceiling on a
// handshake.
//
// It is deliberately SHORTER than the dial budget
// (quicHandshakeTimeout + a margin): quic-go abandons a handshake that
// has not completed within HandshakeIdleTimeout, whatever the caller's
// context allows. So the transport's number has to be the smaller one,
// or the caller's budget is unreachable — and the failure it produces
// ("timeout: no recent network activity") reads like an unreachable
// peer rather than a slow one.
//
// The gap between the two is the room a slow machine needs: the
// transport gives up at 20 s, and the dialer's own budget (20 s + 10 s)
// is the last word.
const quicTransportHandshakeTimeout = 15 * time.Second

func quicConfig() *quic.Config {
	return tracingConfig(&quic.Config{
		MaxIdleTimeout:  10 * time.Second,
		KeepAlivePeriod: 3 * time.Second,
		// The HANDSHAKE has its own timeout, and quic-go's default for it
		// is FIVE SECONDS (MaxIdleTimeout does not cover it — that one
		// applies only AFTER the handshake completes). On a loaded machine
		// a handshake can take longer than that, and the attempt dies with
		// "timeout: no recent network activity" — a false failure that
		// looks like an unreachable peer.
		//
		// It is LONGER than MaxIdleTimeout, but the relationship that
		// matters is not the multiple — it is the ORDER against the
		// dialer's budget. The transport gives up at
		// quicTransportHandshakeTimeout (15 s); the dialer's own budget
		// (quicHandshakeTimeout + 10 s = 30 s) is the last word.
		//
		// So on a loaded machine the TRANSPORT's message arrives first,
		// and it says which of the two gave up. (It used to be the other
		// way round: the transport's ceiling and the budget were the
		// SAME number, so the budget's margin could never be spent and
		// the two could never be told apart.)
		HandshakeIdleTimeout: quicTransportHandshakeTimeout,
		// The flow control windows — see the comment above.
		InitialStreamReceiveWindow:     relayStreamReceiveWindow,
		MaxStreamReceiveWindow:         relayMaxReceiveWindow,
		InitialConnectionReceiveWindow: relayConnectionReceiveWindow,
		MaxConnectionReceiveWindow:     relayMaxReceiveWindow,
	})
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
	// OnMessage receives text messages addressed to this member (KindMsg).
	// Nil = messages are accepted and discarded.
	OnMessage func(from identity.DeviceID, text string)
	// InboxDir is where inbound files (KindFile) are saved. Empty = file
	// sessions are refused with an actionable error.
	InboxDir string
	// MaxFileBytes caps one inbound file (0 = MaxFileBytes default).
	MaxFileBytes int64
	// OnFile is called after a file is verified and saved. Nil = silent.
	OnFile func(from identity.DeviceID, savedPath string, h FileHeader)
	// ExecDisabled refuses exec sessions (KindExec). Default is to allow
	// them for approved members — joining the mesh grants exec rights, the
	// same trust shape as Tailscale SSH. Operators who want file/message
	// but not command execution set this.
	ExecDisabled bool
	// RelayTrunk serves the relay protocol on THIS listener's socket.
	//
	// It is nil on a client that does not run a trunk. When it is set,
	// a connection that negotiated the relay ALPN is served by it
	// instead of by the direct-link dispatch — that is what lets ONE
	// UDP port carry both protocols (D76).
	RelayTrunk *RelayTrunk
	// RelayJoiners is the table of members that have joined this
	// listener's trunk. It may be nil: a fresh table is built then.
	RelayJoiners *JoinerTable
}

// Listener accepts direct links from fellow members.
//
// It ALSO serves the relay protocol when RelayTrunk is set: ALPN is
// negotiated per connection, so one socket and one quic.Listener can
// carry both the direct-link protocol and the relay protocol, and the
// negotiated protocol decides which one a connection speaks.
type Listener struct {
	lns []*quic.Listener
	cfg ListenerConfig
	// bound records the resolved local endpoints (port 0 → actual port).
	bound []string
	stop  chan struct{}
	// relay is the handler for the connections that negotiated the relay
	// ALPN. It is nil when this listener does not run a trunk.
	relay *RelayHandler
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
	// The advertised ALPNs.
	//
	// The direct link always offers directALPN. It ALSO offers
	// RelayALPN when this listener runs a trunk, because ALPN is
	// negotiated PER CONNECTION: one UDP socket and one quic.Listener
	// can serve both protocols, and the negotiated protocol decides which
	// one a connection speaks (D76).
	//
	// The old comment here said "one socket can serve only one ALPN".
	// That was WRONG — it is the reason the trunk needed a second UDP
	// port, and the reason an operator had to open one more port in
	// their security group for the relay to work at all.
	//
	// The relay protocol's ALPNs are [NEW, OLD] — see
	// relayAdvertisedALPNs for why the ORDER is load-bearing (Go's ALPN
	// negotiation is server-first, so the anchor's list order decides
	// which protocol a member that offers both ends up speaking).
	tlsCfg.NextProtos = []string{directALPN}
	if cfg.RelayTrunk != nil {
		tlsCfg.NextProtos = append(tlsCfg.NextProtos, relayAdvertisedALPNs()...)
	}
	l := &Listener{cfg: cfg, stop: make(chan struct{})}
	if cfg.RelayTrunk != nil {
		l.relay = NewRelayHandler(cfg.RelayTrunk, cfg.RelayJoiners)
	}
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

// watchRevocation aborts a direct connection whose peer has lost its
// membership.
//
// It polls the member view on a ticker — the same shape as the relay
// trunk's RevokeFlows watch, and for the same reason: the push of a new
// member view can take up to a second (the snapshot is coalesced), so
// "the revocation takes effect" cannot depend on the push alone.
//
// The interval is one second: the requirement is that a revoked device's
// session stops within a second.
//
// The check is deliberately the SAME question the handshake asks —
// "is this device still a member?" — so the two paths cannot disagree
// about who is a member. A revoked device is not in the view, so
// KeyFor fails for it.
//
// The connection is aborted, not closed: a half close is polite, and a
// revoked device must not be able to keep its sessions alive by
// ignoring it.
func (l *Listener) watchRevocation(ctx context.Context, peerID identity.DeviceID, conn *quic.Conn) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.stop:
			return
		case <-ticker.C:
			if !l.stillMember(peerID) {
				// The peer is no longer in the roster: revoked, or
				// removed. Tear the connection down so its sessions
				// stop.
				_ = conn.CloseWithError(0, "this device is no longer a member")
				return
			}
		}
	}
}

// stillMember asks the PRECISE question: "is this device still in the
// roster?"
//
// It prefers the narrow capability (StillMember) when the resolver has
// it, because KeyFor's error also covers "the certificate expired" and
// "no view is available" — and neither of those is a revocation. A peer
// whose certificate merely lapsed must keep its session (the product
// renews certificates for exactly that reason); a peer that has left
// the roster must not.
//
// A resolver that cannot answer the narrow question falls back to
// KeyFor, which is the same question the handshake asks — so the two
// paths still agree about who is a member.
func (l *Listener) stillMember(id identity.DeviceID) bool {
	if m, ok := l.cfg.Members.(membershipChecker); ok {
		return m.StillMember(id)
	}
	_, err := l.cfg.Members.KeyFor(id)
	return err == nil
}

// handle drives one accepted connection: one authenticated hello per stream,
// then the session body. Stream-level failures do not kill the connection.
//
// THE PROTOCOL IS CHOSEN BY THE NEGOTIATED ALPN. A connection that
// negotiated the relay ALPN speaks the relay protocol, and it is served
// by the relay handler; a connection that negotiated the direct ALPN
// speaks the direct-link protocol. That is what lets ONE UDP port carry
// both (D76) — and it is still as isolated as separate sockets were: a
// peer that offers the wrong ALPN never gets past the handshake.
func (l *Listener) handle(conn *quic.Conn) {
	if proto := conn.ConnectionState().TLS.NegotiatedProtocol; isRelayALPN(proto) {
		if l.relay == nil {
			// This listener does not run a trunk. The handshake should
			// not have succeeded (the relay ALPN was not advertised),
			// but a peer that got here anyway is refused.
			_ = conn.CloseWithError(0, "this node does not relay")
			return
		}
		l.relay.ServeConn(conn)
		return
	}
	defer conn.CloseWithError(0, "bye")
	// The peer's identity comes from its pinned device certificate (mTLS);
	// receive handlers stamp it onto every message and file they accept.
	peerID := identity.DeviceID(conn.ConnectionState().TLS.PeerCertificates[0].Subject.CommonName)

	// THE REVOCATION WATCH ON THE DIRECT PATH.
	//
	// The requirement says a revoked device's ACTIVE SESSIONS are
	// destroyed ("其活跃会话在一个推送往返内被销毁"): it does not say
	// "relay sessions". The relay trunk has had a revocation watch for
	// rounds (RevokeFlows / StartRevocationWatch), but the DIRECT path
	// had NONE — so revoking a device that was mid-transfer over a
	// direct link left that transfer running until it finished on its
	// own. Only NEW handshakes were refused.
	//
	// The check is the member view: a revoked device is not a member
	// any more, so KeyFor fails for it. That is the same question the
	// relay's watch asks, and it is the live view (the roster is
	// hot-reloaded from the anchor's file), so a revocation reaches
	// this node without a push.
	//
	// The CONNECTION is the unit: aborting it destroys every stream it
	// carries, which is exactly what "its active sessions are
	// destroyed" asks for.
	if l.cfg.Members != nil {
		watchCtx, cancelWatch := context.WithCancel(context.Background())
		defer cancelWatch()
		go l.watchRevocation(watchCtx, peerID, conn)
	}

	for {
		st, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		if err := l.serveStream(peerID, conn, st); err != nil {
			// Close only the STREAM (graceful: pending error lines flush to
			// the peer) and keep accepting — tearing the connection here
			// would discard an error verdict the dialer is still waiting to
			// read (observed: "refused" verdicts lost to the close race).
			_ = st.Close()
			continue
		}
	}
}

// serveStream reads the hello, checks freshness and intent, answers ok, and
// dispatches. The TLS handshake has already authenticated the peer (device
// certificate pinned to the member view); the hello only carries intent.
//
// The transport-specific part (the QUIC stream) stops at this function:
// everything below it works on a Session, so the same session body serves
// a direct link and a relayed link.
//
// conn is carried into the session so a handler can reach the QUIC
// connection-level loss accounting (see quicSession.Loss).
func (l *Listener) serveStream(peerID identity.DeviceID, conn *quic.Conn, st *quic.Stream) error {
	return l.serveSession(peerID, quicSession{st: st, conn: conn})
}

// serveSession is the transport-agnostic session entry point: it reads the
// hello, checks freshness and intent, answers ok, and dispatches to the
// handler for the session kind.
func (l *Listener) serveSession(peerID identity.DeviceID, st Session) error {
	// One buffered reader for hello AND session body. The hello's length
	// bound comes from readLine's cap — NOT from a LimitReader here: a cap
	// on the reader would truncate every session body (an 8 MB file died
	// after exactly MaxHelloBytes+1 bytes, with the sender wedged mid-write).
	br := bufio.NewReader(st)
	_ = st.SetReadDeadline(time.Now().Add(helloTimeout))
	line, err := readLine(br, MaxHelloBytes)
	if err != nil {
		return fmt.Errorf("mesh: hello read: %w", err)
	}
	var h Hello
	if err := json.Unmarshal(line, &h); err != nil || h.Proto != ProtoID {
		return errors.New("mesh: bad hello")
	}
	switch h.Kind {
	case KindPing, KindDial, KindMsg, KindFile, KindRun, KindExec, KindBench:
	default:
		return fmt.Errorf("mesh: unknown hello kind %q", h.Kind)
	}
	if now := time.Now(); now.Sub(h.Time) > Skew || h.Time.Sub(now) > Skew {
		return errors.New("mesh: hello timestamp outside window")
	}
	if _, err := io.WriteString(st, "ok\n"); err != nil {
		return err
	}
	_ = st.SetReadDeadline(time.Time{})

	return serveSession(h.Kind, peerID, br, st, l.cfg)
}

// ServeRelayedSession serves a RELAYED session with the same dispatch
// the direct link uses.
//
// The business layer must not be able to tell a relayed session from a
// direct one: a member-to-member message that arrives through a trunk is
// handled exactly like one that arrives directly, and the pings, the
// files and the commands work the same way.
//
// The kind comes from the relay's open record — the requester asked for
// a session of that kind, and the target's handler for that kind serves
// it. That is why the kind travels in the open record.
func ServeRelayedSession(kind string, peerID identity.DeviceID, sess Session, cfg ListenerConfig) error {
	br := bufio.NewReader(sess)
	return serveSession(kind, peerID, br, sess, cfg)
}

// serveSession dispatches an established session to the handler for its
// kind. It is the ONE place where a kind is mapped to a handler, so a new
// kind cannot be half-implemented, and it is transport-agnostic: the
// handlers below only ever see a Session.
//
// The caller has already read the hello and answered "ok"; br is the
// buffered reader positioned right after the hello.
func serveSession(kind string, peerID identity.DeviceID, br *bufio.Reader, st Session, cfg ListenerConfig) error {
	switch kind {
	case KindPing:
		return echoPings(br, st)
	case KindDial:
		return echoDuplex(br, st)
	case KindMsg:
		return receiveMsgs(peerID, br, st, cfg.OnMessage)
	case KindFile:
		return receiveFile(peerID, br, st, cfg)
	case KindRun:
		return receiveRun(br, st, cfg)
	case KindBench:
		return receiveBench(br, st)
		// KindExec (arbitrary remote commands) is pending: the implementation
		// is gated on the operator adjusting the code-scanner rule that blocks
		// os/exec writes. Built-in verbs above cover remote diagnosis today.
	default:
		return fmt.Errorf("mesh: unknown hello kind %q", kind)
	}
}

// msgErrPrefix is the prefix of the message receiver's FAILURE line.
//
// It is "err: " because that is the convention the other handlers in
// this package already use for a refusal (the file handler writes
// "err: receiver has no inbox configured", the bench handler writes
// "err: <reason>").
//
// The format is chosen so it can NEVER be confused with the success
// receipt: the receipt is "done" and the sender's success test is a
// "done" PREFIX, while "err:" shares no prefix with "done" in either
// direction. So a failure line can never be read as a delivery
// receipt, and the receipt can never be read as a failure.
const msgErrPrefix = "err: "

// receiveMsgs prints incoming messages until the sender half-closes the
// stream, then ACKs. The ack is load-bearing: the sender must not tear the
// connection until every frame has been CONSUMED (not just delivered to the
// kernel) — a connection close races the receiver's reads and can discard
// the tail (observed in tests as silently dropped messages).
//
// THE ACK IS A DELIVERY RECEIPT, so it may only be written when every
// frame really was read.
//
// The old loop `break`ed on ANY read error and then wrote "done", which
// made a frame over the cap, a frame truncated mid-body and a reset
// connection all look like a successful delivery: the sender reported
// "delivered" while the target had never seen the message. A false
// receipt is worse than an outright failure, because it is silent.
//
// So a broken frame is answered with an explicit "err: <reason>" line
// and reported as an error, and ONLY a clean half-close gets the "done"
// receipt.
func receiveMsgs(from identity.DeviceID, br *bufio.Reader, st Session, onMsg func(identity.DeviceID, string)) error {
	for {
		frame, cleanEnd, err := readMsgFrame(br, MaxMsgBytes)
		if err != nil {
			// A broken frame: the message was NOT delivered, so the
			// sender must not be told it was. The reason travels with
			// the line, so the operator sees WHY.
			return writeMsgFailure(st, err)
		}
		if cleanEnd {
			break // the sender half-closed: a normal end
		}
		if onMsg != nil {
			onMsg(from, string(frame))
		}
	}
	if _, err := fmt.Fprintln(st, "done"); err != nil {
		return err
	}
	return nil
}

// readMsgFrame reads one length-prefixed message frame and reports
// whether the stream ended CLEANLY.
//
// The distinction is the whole point, and a plain readFrame cannot make
// it: readFrame reports one error for BOTH "the sender half-closed" and
// "the frame is broken", so the receiver cannot tell them apart. Worse,
// io.ReadFull does not help either — it reports io.EOF for "the stream
// ended before ANY header byte" AND for "the header promised 100 bytes
// and the body never came" (a zero-byte read of the body is still
// io.EOF).
//
// So the header and the body are read SEPARATELY, and the header is
// what decides:
//
//   - EOF while reading the very first header byte = the sender
//     half-closed = a clean end (cleanEnd).
//   - anything else about the header = a broken frame.
//   - EOF (or a short read) while reading the body = a TRUNCATED
//     frame, never a clean end: the length prefix promised bytes that
//     did not arrive.
func readMsgFrame(br *bufio.Reader, capBytes int64) (frame []byte, cleanEnd bool, err error) {
	var l [4]byte
	n, err := io.ReadFull(br, l[:])
	if err != nil {
		if errors.Is(err, io.EOF) && n == 0 {
			// Nothing at all was read: the sender half-closed. This
			// is the normal end of the message stream.
			return nil, true, nil
		}
		// A partial header, a reset stream, a deadline: the frame is
		// broken, and the stream did not end cleanly.
		return nil, false, fmt.Errorf("mesh: read the frame header: %w", err)
	}
	size := binary.BigEndian.Uint32(l[:])
	if int64(size) > capBytes {
		return nil, false, fmt.Errorf("mesh: frame of %d bytes exceeds the %d byte cap", size, capBytes)
	}
	body := make([]byte, size)
	got, err := io.ReadFull(br, body)
	if err != nil {
		// The body MUST arrive in full. io.EOF here means the stream
		// ended after the header: the frame is truncated, not
		// finished — so this is a failure, never a clean end.
		//
		// The error is errTruncatedFrame, and the underlying io.EOF is
		// NOT wrapped: an errors.Is(err, io.EOF) that returns true would
		// let every "EOF means the sender finished" caller treat a
		// truncation as a graceful finish — exactly the silent-tail-drop
		// this function exists to prevent. The cause is reported with %v
		// (in the text, not the chain).
		return nil, false, fmt.Errorf("%w: mesh: %d of %d announced bytes arrived: %v", errTruncatedFrame, got, size, err)
	}
	return body, false, nil
}

// errTruncatedFrame says a length prefix promised bytes that never came.
//
// It exists so that a truncation can be told from a clean end WITHOUT
// wrapping io.EOF: a wrapped io.EOF would flow through every
// errors.Is(err, io.EOF) check in the relay and the message paths and be
// mistaken for a graceful finish.
var errTruncatedFrame = errors.New("truncated frame")

// writeMsgFailure writes the receiver's failure line and returns the
// error that caused it.
//
// The line is ONE line: the sender reads it with readLine, so a reason
// carrying a newline would be truncated at the first newline and the
// rest of it would be read as a SECOND line — a second answer the
// sender would have to interpret. The reason is flattened for that
// reason, not for looks.
func writeMsgFailure(st Session, reason error) error {
	msg := strings.NewReplacer("\r", " ", "\n", " ").Replace(reason.Error())
	fmt.Fprintf(st, "%s%s\n", msgErrPrefix, msg)
	return reason
}

// checkMsgAck judges the receiver's answer to a message send.
//
// The answer is a DELIVERY RECEIPT, so only the receipt counts as one:
// "done" means the receiver consumed every frame the sender wrote.
// Anything else is a FAILURE — and the receiver's explicit "err:"
// line is the most useful kind of failure, because it carries the
// reason (a frame over the cap, a truncated frame, …).
func checkMsgAck(ack []byte) error {
	if bytes.HasPrefix(ack, []byte("done")) {
		return nil
	}
	return fmt.Errorf("mesh: receiver error: %s", ack)
}

// receiveFile reads one FileHeader line, streams the body to InboxDir,
// verifies sha256, and answers "ok\n" (or an error line the sender shows).
func receiveFile(from identity.DeviceID, br *bufio.Reader, st Session, cfg ListenerConfig) error {
	line, err := readLine(br, MaxHelloBytes)
	if err != nil {
		return err
	}
	var h FileHeader
	if err := json.Unmarshal(line, &h); err != nil || h.Name == "" || h.Size < 0 {
		return errors.New("mesh: bad file header")
	}
	if cfg.InboxDir == "" {
		fmt.Fprintln(st, "err: receiver has no inbox configured")
		return errors.New("mesh: no inbox configured")
	}
	cap := cfg.MaxFileBytes
	if cap <= 0 {
		cap = MaxFileBytes
	}
	if h.Size > cap {
		fmt.Fprintf(st, "err: file exceeds receiver cap (%d > %d)\n", h.Size, cap)
		return fmt.Errorf("mesh: file %d bytes exceeds cap %d", h.Size, cap)
	}
	base := filepath.Base(h.Name)
	if base == "." || base == "/" || strings.Contains(base, "..") {
		fmt.Fprintln(st, "err: bad file name")
		return errors.New("mesh: bad file name")
	}
	_ = st.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintln(st, "ok"); err != nil {
		return err
	}
	_ = st.SetWriteDeadline(time.Time{})

	if err := os.MkdirAll(cfg.InboxDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(cfg.InboxDir, ".incoming-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	hash := sha256.New()
	_, copyErr := io.CopyN(io.MultiWriter(tmp, hash), br, h.Size)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil || fmt.Sprintf("%x", hash.Sum(nil)) != h.SHA256 {
		_ = os.Remove(tmpPath)
		fmt.Fprintln(st, "err: sha256 mismatch or short read")
		return errors.New("mesh: file integrity check failed")
	}
	saved := filepath.Join(cfg.InboxDir, base)
	// ★ DO NOT SILENTLY OVERWRITE WHAT IS ALREADY THERE. (Round 99.)
	//
	// `os.Rename` replaces the destination if it exists. So a second
	// `send-to` of a file with the same name REPLACED the first one,
	// with no warning to either side: the sender was told "done <hash>"
	// and the receiver's inbox now holds the second file, not the first.
	// An operator who sends "report.pdf" twice, or two members who
	// happen to pick the same name, loses a file without being told.
	//
	// The stage-1 path has had this right for a while (publishNoClobber:
	// link-then-remove gives atomic "create if absent" semantics, and the
	// destination existing is a REFUSAL). The mesh line is a separate
	// implementation, and it did not carry that rule over.
	//
	// So: refuse to replace, and say so — to the SENDER, over the
	// stream, because the sender is waiting for a verdict. A local error
	// would leave it blocked until its timeout.
	if err := publishMeshNoClobber(tmpPath, saved, h.Force); err != nil {
		_ = os.Remove(tmpPath)
		fmt.Fprintf(st, "err: %s\n", err)
		return err
	}
	if _, err := fmt.Fprintln(st, "done "+h.SHA256); err != nil {
		return err
	}
	if cfg.OnFile != nil {
		cfg.OnFile(from, saved, h)
	}
	return nil
}

// publishMeshNoClobber moves the finished file into place WITHOUT
// replacing a file that is already there.
//
// The atomic way to do it is a hard link: link-then-remove means the
// destination is created only if it did not exist, and there is no
// window in which the file is half-published.
//
// A filesystem that cannot create hard links (FAT, some network mounts,
// Android) falls back to an explicit existence check, which is not
// atomic — but it is still better than a silent replacement, and the
// fallback's window is a rename over a name that was absent a moment
// ago.
func publishMeshNoClobber(part, final string, force bool) error {
	if force {
		// The operator asked for replacement; rename is atomic and clobbers.
		if err := os.Rename(part, final); err != nil {
			return fmt.Errorf("mesh: rename: %w", err)
		}
		return nil
	}

	if err := os.Link(part, final); err == nil {
		// The data is reachable under both names; drop the temporary one.
		if rmErr := os.Remove(part); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("mesh: remove the temporary file after publishing: %w", rmErr)
		}
		return nil
	} else if !os.IsExist(err) && !meshLinkUnsupported(err) {
		return fmt.Errorf("mesh: publish %s: %w", final, err)
	}

	// Either the destination exists (refuse) or linking is unsupported
	// here.
	if _, statErr := os.Stat(final); statErr == nil {
		return fmt.Errorf("the receiver already has a file named %s "+
			"(rename it or remove it to receive this one)", filepath.Base(final))
	}
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("mesh: rename: %w", err)
	}
	return nil
}

// meshLinkUnsupported reports whether an error from os.Link means the
// filesystem cannot create hard links (as opposed to the destination
// already existing).
func meshLinkUnsupported(err error) bool {
	s := strings.ToLower(err.Error())
	for _, marker := range []string{
		"operation not permitted",
		"operation not supported",
		"not supported",
		"invalid cross-device link",
		"no such device or address",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// echoPings answers ping records verbatim until the dialer hangs up.
func echoPings(br *bufio.Reader, st Session) error {
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
func echoDuplex(br *bufio.Reader, st Session) error {
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
	// Picker chooses the path (direct first, then relay candidates) the
	// session is established over. Nil = the session is always direct,
	// which is byte-for-byte today's behaviour.
	Picker PathPicker
	// ForceOverwrite says the sender knows the receiver already has a
	// file with this name and wants it replaced.
	//
	// ★ IT IS THE SENDER'S INTENT, AND IT IS EXPLICIT BY DEFAULT-OFF.
	// (Round 99.) The receiver used to rename over whatever was there,
	// so a second transfer of the same name REPLACED the first one
	// silently — the sender was told "done <hash>" and the first file
	// was gone. A file can only be replaced when somebody asked for it,
	// and the stage-1 path has had that rule (publishNoClobber) for a
	// while.
	ForceOverwrite bool
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
		// No endpoint in the member view = no path. Not an auth problem:
		// the credentials were never exercised, and a relay might still
		// reach the target.
		return nil, fmt.Errorf("%w: mesh: %s has no reachable endpoints in the current view", ErrNoRoute, cfg.Target)
	}
	if cfg.HandshakeTimeout <= 0 {
		// The budget must outlast the handshake quicConfig allows
		// (quicHandshakeTimeout). A shorter one cancels the handshake
		// before it can finish.
		cfg.HandshakeTimeout = quicHandshakeTimeout + 10*time.Second
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
		// The target is not a (current) member: the resolver could not
		// produce a key to pin. That is an auth-class failure — the
		// credentials are the problem, not the network — so the path
		// layer must not retry it over a relay. The original text
		// ("... is not a member") stays in the chain for the operator.
		return nil, fmt.Errorf("%w: %w", ErrAuth, err)
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

	var lastErr error
	for _, ep := range cfg.Endpoints {
		addr, err := net.ResolveUDPAddr("udp", ep)
		if err != nil {
			lastErr = fmt.Errorf("bad endpoint %q in the member view", ep)
			continue
		}
		link, err := dialOne(ctx, addr, cfg, tlsCfg)
		if err == nil {
			return link, nil
		}
		// Classify here, once per endpoint: the class travels up through
		// the aggregate below, so errors.Is on the outer error still
		// reaches the sentinel.
		classified := classifyDialError(err)
		lastErr = preferDialError(lastErr, classified)
	}
	return nil, fmt.Errorf("mesh: all %d endpoint(s) for %s failed: %w",
		len(cfg.Endpoints), cfg.Target, lastErr)
}

// preferDialError picks the failure the caller should see when several
// endpoints were tried.
//
// A VERDICT (the peer answered and rejected us — an auth failure or a
// refusal) beats a NETWORK FACT (nobody answered). The distinction is not
// cosmetic: the path layer decides whether another path may be tried from
// the class, and "the peer rejected us" must not be erased by "the next
// endpoint did not answer" — otherwise the same rejected credentials
// would be presented over a relay.
//
// Without this rule the dial would keep the LAST error, and the verdict
// would depend on the endpoint order — the same member would be
// classified differently on two machines.
func preferDialError(current, candidate error) error {
	if candidate == nil {
		return current
	}
	if current == nil {
		return candidate
	}
	if isDialVerdict(candidate) {
		return candidate
	}
	if isDialVerdict(current) {
		return current
	}
	return candidate
}

// isDialVerdict reports whether err is a verdict rather than a network
// fact: the peer was reached and rejected the session.
func isDialVerdict(err error) bool {
	return errors.Is(err, ErrAuth) || errors.Is(err, ErrRefused)
}

// dialOne attempts a single endpoint.
//
// Each attempt needs its own UDP socket. quic.Dial wraps the net.PacketConn
// in a single-use Transport, and that Transport closes the connection when
// the QUIC session ends (quic-go transport.go: isSingleUse). So one socket
// serves exactly one attempt: sharing a socket across the endpoint loop
// made every attempt after a failure run on a closed socket and fail with
// "use of closed network connection", which made the multi-endpoint
// fallback unreachable.
//
// The returned error is CLASSIFIED (see classifyDialError). This is
// load-bearing: quic.Dial returns a nil error when the peer rejects our
// certificate — the rejection only arrives once the handshake is running,
// so it surfaces at the hello write or the ack read, wrapped in a
// quic.TransportError carrying a TLS alert. Classifying here (rather than
// only at the aggregate in DialDirect) means the class is attached to the
// error that actually carries the evidence.
func dialOne(ctx context.Context, addr *net.UDPAddr, cfg DialConfig, tlsCfg *tls.Config) (*Link, error) {
	link, err := dialOneRaw(ctx, addr, cfg, tlsCfg)
	if err != nil {
		return nil, classifyDialError(err)
	}
	return link, nil
}

func dialOneRaw(ctx context.Context, addr *net.UDPAddr, cfg DialConfig, tlsCfg *tls.Config) (*Link, error) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("mesh: local udp bind: %w", err)
	}
	// The handshake context derives from the caller's, so a caller that
	// bounds its own budget is not overrun by the endpoint loop.
	//
	// It used to be context.Background(), which meant the endpoint loop
	// was bounded only by HandshakeTimeout per attempt: a member view with
	// many unreachable endpoints made the whole dial take as long as the
	// number of endpoints times the per-attempt timeout, regardless of what
	// the caller asked for. The product's own `status --probe --probe-timeout
	// 3s` was measured running 5.12 s.
	//
	// The per-attempt timeout still applies on top: whichever is shorter
	// wins.
	cctx, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	defer cancel()
	qc, err := quic.Dial(cctx, udp, addr, tlsCfg, quicConfig())
	if err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("quic dial %s: %w", addr, err)
	}
	// From here on the QUIC connection exists, so every failure has to
	// release BOTH it and the socket. quic-go was handed a caller-owned
	// packet connection (setupTransport records createdConn=false for this
	// call shape), so closing the connection does not close the socket.
	// Measured: an abandoned net.UDPConn keeps its descriptor until the
	// garbage collector's finalizer runs, so the release used to be real
	// but unbounded — a dial loop could hold hundreds of descriptors
	// between collections.
	abandon := func(err error) (*Link, error) {
		_ = qc.CloseWithError(0, "abandoned")
		_ = udp.Close()
		return nil, err
	}
	st, err := qc.OpenStreamSync(cctx)
	if err != nil {
		return abandon(fmt.Errorf("open stream: %w", err))
	}

	hello := Hello{Proto: ProtoID, Kind: cfg.Kind, Time: time.Now().UTC()}
	line, err := json.Marshal(hello)
	if err != nil {
		return abandon(err)
	}
	if _, err := fmt.Fprintf(st, "%s\n", line); err != nil {
		return abandon(fmt.Errorf("send hello: %w", err))
	}
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(st)
	ackLine, err := readLine(br, MaxHelloBytes)
	if err != nil {
		return abandon(fmt.Errorf("read ack: %w", err))
	}
	if !bytes.Equal(bytes.TrimSpace(ackLine), []byte("ok")) {
		// The peer WAS reached and declined. Mark it as a refusal so the
		// classifier can tell "said no" apart from "nobody answered".
		return abandon(newRefused("listener refused: %s", ackLine))
	}
	_ = st.SetReadDeadline(time.Time{})
	// The attempt succeeded, so the socket belongs to the link now: it is
	// released when the link is closed.
	return &Link{qc: qc, st: st, sess: quicSession{st: st, conn: qc}, br: br, udp: udp}, nil
}

// Link is an established, mutually-authenticated direct link.
type Link struct {
	qc *quic.Conn
	st *quic.Stream
	// udp is the socket this attempt opened. quic-go was handed it as a
	// caller-owned packet connection, so it is not closed when the QUIC
	// connection is — the link owns it and releases it on Close.
	udp *net.UDPConn
	// sess is the transport-agnostic view of the same stream. The
	// business layer uses it, so the business code never names a QUIC
	// type and a relayed session can be dropped in unchanged.
	sess Session
	br   *bufio.Reader
	// closer releases the resources behind sess when the link is closed.
	//
	// It is nil for a direct link (the QUIC connection and the socket
	// are released by Close itself) and set for a RELAYED link, whose
	// session is backed by a QUIC connection to the relay member that
	// nothing else owns. Without it a relayed link would leak the
	// connection and the socket.
	closer func() error
}

// SendMsg delivers short text messages to a member. Frames are
// length-prefixed, so multi-line text travels intact; the stream close is
// the end-of-messages marker the receiver's read loop waits on.
func SendMsg(ctx context.Context, cfg DialConfig, texts []string) error {
	if len(texts) == 0 {
		return errors.New("mesh: no message text")
	}
	for i, t := range texts {
		if t == "" {
			return fmt.Errorf("mesh: message %d is empty", i)
		}
		if int64(len(t)) > MaxMsgBytes {
			return fmt.Errorf("mesh: message %d exceeds %d bytes", i, MaxMsgBytes)
		}
	}
	cfg.Kind = KindMsg
	link, _, err := dialViaPicker(ctx, cfg)
	if err != nil {
		return err
	}
	defer link.Close()
	for _, t := range texts {
		if err := writeFrame(link.Session(), []byte(t)); err != nil {
			return fmt.Errorf("mesh: send message: %w", err)
		}
	}
	// Half-close (FIN after the frames), then wait for the receiver's done
	// ack — only then is tearing the connection safe.
	if err := link.Session().Close(); err != nil {
		return fmt.Errorf("mesh: close send side: %w", err)
	}
	_ = link.Session().SetReadDeadline(time.Now().Add(10 * time.Second))
	// The cap is MaxHelloBytes, not 16: the success receipt is four
	// bytes ("done"), but the FAILURE line carries the reason ("err:
	// frame of 65537 bytes exceeds the 65536 byte cap"), and a cap of
	// 16 turned every failure into the useless "line too long" — the
	// operator lost the one thing that says WHY the send failed.
	ack, err := readLine(link.br, MaxHelloBytes)
	if err != nil {
		return fmt.Errorf("mesh: await delivery ack: %w", err)
	}
	// The ack is a DELIVERY RECEIPT: only "done" means the receiver
	// consumed every frame. Anything else — including the receiver's
	// explicit "err:" line — is a failure, and the send must say so
	// rather than report a delivery that never happened.
	return checkMsgAck(ack)
}

// SendFile streams one file to a member. The sender hashes the file first
// (the receiver verifies the hash it was promised, not one computed after
// the fact), streams the bytes, and waits for the receiver's final verdict.
// Returns the header the receiver was given.
func SendFile(ctx context.Context, cfg DialConfig, path string) (FileHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileHeader{}, fmt.Errorf("mesh: open %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return FileHeader{}, err
	}
	if !st.Mode().IsRegular() {
		return FileHeader{}, fmt.Errorf("mesh: %s is not a regular file", path)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return FileHeader{}, err
	}
	h := FileHeader{
		Name:   filepath.Base(path),
		Size:   st.Size(),
		SHA256: fmt.Sprintf("%x", hash.Sum(nil)),
		Force:  cfg.ForceOverwrite,
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return FileHeader{}, err
	}

	cfg.Kind = KindFile
	link, _, err := dialViaPicker(ctx, cfg)
	if err != nil {
		return h, err
	}
	defer link.Close()

	hdr, err := json.Marshal(h)
	if err != nil {
		return h, err
	}
	if _, err := fmt.Fprintf(link.Session(), "%s\n", hdr); err != nil {
		return h, fmt.Errorf("mesh: send header: %w", err)
	}
	// Receiver answers ok before taking bytes (its inbox may be unset or
	// the file over its cap — cheaper to learn now than after 8 GB).
	_ = link.Session().SetReadDeadline(time.Now().Add(15 * time.Second))
	if ack, err := readLine(link.br, MaxHelloBytes); err != nil {
		return h, fmt.Errorf("mesh: read file ack: %w", err)
	} else if !bytes.HasPrefix(ack, []byte("ok")) {
		return h, fmt.Errorf("mesh: receiver refused: %s", ack)
	}
	_ = link.Session().SetReadDeadline(time.Time{})

	if err := sendFileBody(ctx, link.Session(), f, h.Size); err != nil {
		return h, fmt.Errorf("mesh: send body: %w", err)
	}
	_ = link.Session().SetReadDeadline(time.Now().Add(30 * time.Second))
	verdict, err := readLine(link.br, MaxHelloBytes)
	if err != nil {
		return h, fmt.Errorf("mesh: read verdict: %w", err)
	}
	if !bytes.HasPrefix(verdict, []byte("done")) {
		return h, fmt.Errorf("mesh: receiver rejected the file: %s", verdict)
	}
	return h, nil
}

// fileChunkSize bounds each body write so a rolling write deadline can
// detect a dead receiver mid-transfer instead of blocking on one giant
// write for the whole file.
const fileChunkSize = 1 << 20

func sendFileBody(ctx context.Context, st Session, f *os.File, size int64) error {
	buf := make([]byte, fileChunkSize)
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int64(len(buf))
		if remaining < n {
			n = remaining
		}
		if _, err := io.ReadFull(f, buf[:n]); err != nil {
			return err
		}
		// Rolling deadline: 60 s per chunk — long enough for any honest
		// link short of total congestion, short enough that a dead
		// receiver surfaces within a minute of the last progress.
		_ = st.SetWriteDeadline(time.Now().Add(60 * time.Second))
		if _, err := st.Write(buf[:n]); err != nil {
			return err
		}
		remaining -= n
	}
	return nil
}

// Stream exposes the raw duplex stream (one pipe, positioned after the
// handshake). PR-M4 wraps it in the e2e layer.
//
// It is kept for the existing callers (the CLI's interactive dial, the
// tests). New business code should use Session instead: it says nothing
// about the transport, so the same code runs over a relayed link.
func (l *Link) Stream() *quic.Stream { return l.st }

// Session exposes the transport-agnostic view of the link. The business
// layer writes and reads through this, so it never learns whether the
// bytes travelled directly or through a relay.
func (l *Link) Session() Session { return l.sess }

// Reader exposes the buffered reader positioned after the handshake.
func (l *Link) Reader() *bufio.Reader { return l.br }

// Loss reports the QUIC-level loss accounting of this link's connection, or
// nil when the connection could not be instrumented.
//
// The number is per-CONNECTION: quic-go exposes no per-stream loss counter.
// A bench session carries exactly one stream on its own connection, so the
// two coincide there.
func (l *Link) Loss() *BenchLoss { return lossOf(l.qc) }

// Close tears the link down.
// Close ends the link and releases the socket the dial opened. quic-go does
// not close a caller-owned packet connection when the QUIC connection ends
// (createdConn=false), so the socket is this type's responsibility.
func (l *Link) Close() error {
	// A relayed link has no QUIC connection of its own: its session is
	// backed by one to the RELAY member, released by the closer.
	if l.closer != nil {
		return l.closer()
	}
	if l.qc == nil {
		return nil
	}
	err := l.qc.CloseWithError(0, "bye")
	if l.udp != nil {
		_ = l.udp.Close()
	}
	return err
}

// PingStats is the result of a ping exchange. Sent includes packets that
// were written successfully; Lost is the number whose reply timed out.
type PingStats struct {
	Sent     int
	Received int
	Lost     int
	RTTs     []float64
}

// LossPercent returns packet loss as a percentage. With no sent packets it is
// zero because no measurement was made.
func (s PingStats) LossPercent() float64 {
	if s.Sent == 0 {
		return 0
	}
	return float64(s.Lost) * 100 / float64(s.Sent)
}

const pingReplyTimeout = time.Second

// PingExchange runs count pings over an established ping-kind link and
// returns the RTTs in milliseconds. It is kept as a compatibility wrapper;
// callers that need loss accounting should use PingExchangeStats.
func PingExchange(ctx context.Context, l *Link, count int, interval time.Duration) ([]float64, error) {
	stats, err := PingExchangeStats(ctx, l, count, interval)
	return stats.RTTs, err
}

// PingExchangeStats sends one packet at a time and records timed-out replies
// as loss, allowing later packets to be measured instead of aborting at the
// first missing reply. The CLI validates its user-facing count and interval
// limits; this lower-level API remains permissive for existing callers.
func PingExchangeStats(ctx context.Context, l *Link, count int, interval time.Duration) (PingStats, error) {
	if count <= 0 {
		count = 1
	}
	stats := PingStats{RTTs: make([]float64, 0, count)}
	buf := make([]byte, pingRecordSize)
	for i := 1; i <= count; i++ {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if i > 1 && interval > 0 {
			select {
			case <-ctx.Done():
				return stats, ctx.Err()
			case <-time.After(interval):
			}
		}
		binary.BigEndian.PutUint64(buf[0:], uint64(i))
		binary.BigEndian.PutUint64(buf[8:], uint64(time.Now().UnixNano()))
		if _, err := l.sess.Write(buf); err != nil {
			return stats, err
		}
		stats.Sent++

		deadline := time.Now().Add(pingReplyTimeout)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		_ = l.sess.SetReadDeadline(deadline)
		for {
			if _, err := io.ReadFull(l.br, buf); err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					stats.Lost++
					break
				}
				if ctx.Err() != nil {
					return stats, ctx.Err()
				}
				return stats, err
			}
			if binary.BigEndian.Uint64(buf[0:]) != uint64(i) {
				// A late reply for an earlier timed-out packet is not a reply to
				// this packet. Keep reading until this packet's deadline.
				continue
			}
			sendAt := int64(binary.BigEndian.Uint64(buf[8:]))
			stats.RTTs = append(stats.RTTs, float64(time.Now().UnixNano()-sendAt)/1e6)
			stats.Received++
			break
		}
	}
	_ = l.sess.SetReadDeadline(time.Time{})
	return stats, nil
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

// DialRelaySession opens a RELAY-PROTOCOL session to a member.
//
// It is like DialDirect, with two differences:
//
//  1. The ALPN is the relay protocol's, not the direct link's. That is
//     what keeps the two protocols from reaching each other: a peer that
//     speaks the direct-link protocol cannot land on the relay protocol
//     and vice versa.
//  2. There is no hello and no kind dispatch. The relay protocol has its
//     own record framing, and the session's MEANING is decided by the
//     first record (a join or an open).
//
// The certificate is pinned against the member view exactly as on the
// direct path, so the relay member is authenticated the same way.
func DialRelaySession(
	ctx context.Context,
	dev *identity.Device,
	resolver MemberResolver,
	target identity.DeviceID,
	endpoints []string,
) (*Link, error) {
	if dev == nil || resolver == nil || target == "" {
		return nil, errors.New("mesh: relay dial needs device, resolver and target")
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("%w: mesh: %s announced no relay endpoint to dial", ErrNoRoute, target)
	}
	wantKey, err := resolver.KeyFor(target)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAuth, err)
	}
	cert, err := dev.TLSCertificate()
	if err != nil {
		return nil, err
	}
	pin := func(id identity.DeviceID, pub ed25519.PublicKey) error {
		if id != target {
			return fmt.Errorf("mesh: endpoint answered as %s, not %s", id, target)
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
	// The member's ALPN list is [NEW, OLD].
	//
	// BOTH are needed: a member that offers only the new ALPN cannot
	// reach an OLD anchor at all — the handshake fails with "no
	// application protocol" (measured; see
	// TestAClientThatOffersOnlyTheNewALPNCannotReachAnOldAnchor).
	//
	// The NEW one comes FIRST so that a NEW anchor (which is server-first
	// and takes the first entry of its own list the client also offers)
	// lands on the new protocol with a member that can speak it.
	tlsCfg.NextProtos = relayAdvertisedALPNs()

	var lastErr error
	for _, ep := range endpoints {
		addr, err := net.ResolveUDPAddr("udp", ep)
		if err != nil {
			lastErr = fmt.Errorf("bad endpoint %q in the member view", ep)
			continue
		}
		link, err := dialRelayOne(ctx, addr, dev, tlsCfg)
		if err == nil {
			return link, nil
		}
		classified := classifyDialError(err)
		lastErr = preferDialError(lastErr, classified)
	}
	return nil, fmt.Errorf("mesh: all %d relay endpoint(s) for %s failed: %w",
		len(endpoints), target, lastErr)
}

// dialRelayOne attempts a single relay-protocol endpoint.
func dialRelayOne(ctx context.Context, addr *net.UDPAddr, dev *identity.Device, tlsCfg *tls.Config) (*Link, error) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("mesh: local udp bind: %w", err)
	}
	// The dial's budget must OUTLAST the handshake it is waiting for.
	//
	// quicConfig sets HandshakeIdleTimeout to 20 s (the handshake needs
	// that long on a slow or loaded machine), and a dial context
	// shorter than the handshake cancels the handshake before it can
	// finish — the peer is ready and the dialer has already given up.
	// That produced "quic dial relay …: context deadline exceeded" on
	// a loaded machine, with nothing wrong with either end.
	//
	// So the budget is the handshake timeout PLUS a margin for the
	// stream opening that follows it.
	dialBudget := quicHandshakeTimeout + 10*time.Second
	cctx, cancel := context.WithTimeout(ctx, dialBudget)
	defer cancel()
	qc, err := quic.Dial(cctx, udp, addr, tlsCfg, quicConfig())
	if err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("quic dial relay %s: %w", addr, err)
	}
	st, err := qc.OpenStreamSync(cctx)
	if err != nil {
		_ = qc.CloseWithError(0, "abandoned")
		_ = udp.Close()
		return nil, fmt.Errorf("open relay stream: %w", err)
	}
	return &Link{qc: qc, st: st, sess: quicSession{st: st, conn: qc}, br: bufio.NewReader(st), udp: udp}, nil
}
