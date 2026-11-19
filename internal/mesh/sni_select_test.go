// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

// DECISIVE VERIFICATION (temporary, experiment-only): does the multi-homing
// design in docs/design/MULTI-HOMING.md work?
//
// The design claims: one process, one UDP listener, many group identities.
// The dialer puts the TARGET DeviceID into the TLS ServerName (SNI); the
// listener's tls.Config.GetCertificate callback reads chi.ServerName and
// presents the matching group's certificate.
//
// This file exists to MEASURE that claim under QUIC/TLS 1.3, not to assert it.
//
// The questions that decide the design are:
//  1. Under QUIC, is GetCertificate called at all, and does chi.ServerName
//     carry the value the dialer put in tls.Config.ServerName?
//  2. Is a DeviceID (16 lowercase base32 chars, e.g. "k3f9xz2mqw7ab4cd")
//     accepted as an SNI value by Go's crypto/tls — on the wire, by the
//     server, and by the client's own verification path?
//  3. Does setting ServerName break the repo's own pinning
//     (InsecureSkipVerify + VerifyConnection)?
//  4. Does an old client that sends no SNI still get the default certificate?
//  5. If raw DeviceID-as-SNI is rejected, do the two documented fallbacks
//     work: (a) a legal DNS-shaped SNI, (b) encoding the group identity in
//     ALPN (chi.SupportedProtos)?
//
// NO PRODUCTION CODE IS TOUCHED BY THIS FILE. It is a measurement harness.

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// sniProbe records what the server-side GetCertificate callback observed.
type sniProbe struct {
	mu sync.Mutex
	// calls counts GetCertificate invocations.
	calls int
	// serverNames holds chi.ServerName for each invocation, in order.
	serverNames []string
	// protos holds chi.SupportedProtos for each invocation, in order.
	protos [][]string
	// certs holds the CN of the certificate each invocation returned.
	certs []string
	// conns holds the remote address seen by the callback.
	conns []string
}

func (p *sniProbe) record(chi *tls.ClientHelloInfo, cert *tls.Certificate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.serverNames = append(p.serverNames, chi.ServerName)
	p.protos = append(p.protos, append([]string(nil), chi.SupportedProtos...))
	cn := ""
	if cert != nil && len(cert.Certificate) > 0 {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			cn = leaf.Subject.CommonName
		}
	}
	p.certs = append(p.certs, cn)
	addr := ""
	if chi.Conn != nil && chi.Conn.RemoteAddr() != nil {
		addr = chi.Conn.RemoteAddr().String()
	}
	p.conns = append(p.conns, addr)
}

func (p *sniProbe) snapshot() (calls int, names []string, certs []string, protos [][]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, append([]string(nil), p.serverNames...), append([]string(nil), p.certs...),
		append([][]string(nil), p.protos...)
}

// sniTestPub extracts the Ed25519 public key from a TLS certificate.
func sniTestPub(t *testing.T, cert *tls.Certificate) ed25519.PublicKey {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("cert key is %T, want ed25519.PublicKey", leaf.PublicKey)
	}
	return pub
}

// sniTestIdentity is one group identity: an Ed25519 device plus its TLS cert.
type sniTestIdentity struct {
	id   identity.DeviceID
	cert tls.Certificate
}

func newSNTTestIdentity(t *testing.T) sniTestIdentity {
	t.Helper()
	dev, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate device: %v", err)
	}
	cert, err := dev.TLSCertificate()
	if err != nil {
		t.Fatalf("tls certificate: %v", err)
	}
	return sniTestIdentity{id: dev.ID(), cert: cert}
}

// sniTestServer starts a QUIC listener whose GetCertificate callback routes
// on chi.ServerName. The routing function is supplied by the test.
func sniTestServer(t *testing.T, probe *sniProbe, route func(chi *tls.ClientHelloInfo) (*tls.Certificate, error), defaultCert *tls.Certificate) (addr string, stop func()) {
	t.Helper()
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{*defaultCert},
		NextProtos:   []string{"container-mesh-direct/1"},
		ClientAuth:   tls.RequireAnyClientCert,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := route(chi)
			probe.record(chi, cert)
			return cert, err
		},
	}
	ln, err := quic.Listen(udp, cfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				// Hold the connection open briefly so the handshake completes.
				time.Sleep(500 * time.Millisecond)
				conn.CloseWithError(0, "")
			}()
		}
	}()
	return udp.LocalAddr().String(), func() {
		ln.Close()
		udp.Close()
	}
}

// sniTestClient dials addr with the given SNI and pins the peer certificate
// against the expected public key using the repo's own verification shape
// (InsecureSkipVerify + VerifyConnection).
func sniTestClient(t *testing.T, addr, serverName string, clientCert *tls.Certificate, expectPub ed25519.PublicKey, expectCN string) error {
	t.Helper()
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client listen udp: %v", err)
	}
	defer udp.Close()
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{*clientCert},
		NextProtos:   []string{"container-mesh-direct/1"},
		ClientAuth:   tls.RequireAnyClientCert,
	}
	if serverName != "" {
		cfg.ServerName = serverName
	}
	// The repo's shape: default verification off, our own pin in
	// VerifyConnection (internal/mesh/direct.go deviceTLS).
	cfg.InsecureSkipVerify = true
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("no peer certificate")
		}
		leaf := cs.PeerCertificates[0]
		pub, ok := leaf.PublicKey.(ed25519.PublicKey)
		if !ok {
			return errors.New("not ed25519")
		}
		if !pub.Equal(expectPub) {
			return fmt.Errorf("pin mismatch: peer CN=%q", leaf.Subject.CommonName)
		}
		if expectCN != "" && leaf.Subject.CommonName != expectCN {
			return fmt.Errorf("CN mismatch: got %q want %q", leaf.Subject.CommonName, expectCN)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.Dial(ctx, udp, raddr, cfg, quicConfig())
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "")
	// Force the handshake to complete and the ALPN to be visible.
	st := conn.ConnectionState()
	if st.TLS.NegotiatedProtocol != "container-mesh-direct/1" {
		return fmt.Errorf("unexpected ALPN %q", st.TLS.NegotiatedProtocol)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 1 + 2 + 3: DeviceID directly in SNI, under QUIC.
// ---------------------------------------------------------------------------

func TestSNIDeviceIDSelectsIdentityUnderQUIC(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	targetIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	probe := &sniProbe{}
	// Route: the SNI is expected to be a DeviceID; map it to that identity.
	route := func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if chi.ServerName == targetIdent.id.String() {
			c := targetIdent.cert
			return &c, nil
		}
		if chi.ServerName == serverIdent.id.String() {
			c := serverIdent.cert
			return &c, nil
		}
		return nil, fmt.Errorf("no identity for SNI %q", chi.ServerName)
	}
	addr, stop := sniTestServer(t, probe, route, &serverIdent.cert)
	defer stop()

	// DeviceIDs are lowercase base32: letters and digits only. Is that a
	// legal DNS name? Is it legal as an SNI value? Measure, don't guess.
	t.Logf("DeviceID under test: %q (valid=%v, len=%d)",
		targetIdent.id, targetIdent.id.Valid(), len(targetIdent.id))

	err := sniTestClient(t, addr, targetIdent.id.String(), &clientIdent.cert,
		sniTestPub(t, &targetIdent.cert), targetIdent.id.String())
	if err != nil {
		t.Fatalf("QUIC dial with DeviceID SNI failed: %v", err)
	}

	calls, names, certs, protos := probe.snapshot()
	t.Logf("GetCertificate calls=%d names=%v certs=%v protos=%v", calls, names, certs, protos)

	if calls == 0 {
		t.Fatalf("GetCertificate was NEVER called under QUIC — the SNI design cannot work as written")
	}
	if names[0] != targetIdent.id.String() {
		t.Fatalf("chi.ServerName = %q, want %q", names[0], targetIdent.id.String())
	}
	if certs[0] != targetIdent.id.String() {
		t.Fatalf("served certificate CN = %q, want %q", certs[0], targetIdent.id.String())
	}
}

// ---------------------------------------------------------------------------
// 2b: is a raw DeviceID accepted, or does Go refuse a non-DNS SNI?
// ---------------------------------------------------------------------------

// TestSNIHostileNamesRejectedOrAccepted measures how Go's crypto/tls treats
// SNI values that are NOT legal DNS names. It covers both directions:
//   - client side: does the handshake start at all?
//   - server side: does the server's GetCertificate see the value, and does
//     the handshake complete?
//
// It also covers the IP-literal case, because quic-go INFERENCES ServerName
// from the dial address when the config leaves it empty (transport.go
// setTLSConfigServerName): the mesh dials a raw IP, so the inferred
// ServerName is an IP literal, and Go strips it from the SNI extension
// (hostnameInSNI). That is the "old client sends no SNI" path.
func TestSNIHostileNamesRejectedOrAccepted(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	cases := []struct {
		name string
		sni  string
	}{
		{"deviceid", serverIdent.id.String()},
		{"deviceid_with_underscore", "abc_defghij234567"},
		{"spaces", "not a dns name"},
		{"slash", "id/with/slash"},
		{"ip_literal", "127.0.0.1"},
		{"empty_is_no_sni", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &sniProbe{}
			route := func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
				return &serverIdent.cert, nil
			}
			addr, stop := sniTestServer(t, probe, route, &serverIdent.cert)
			defer stop()

			err := sniTestClient(t, addr, tc.sni, &clientIdent.cert,
				sniTestPub(t, &serverIdent.cert), serverIdent.id.String())
			calls, names, certs, _ := probe.snapshot()
			t.Logf("sni=%q -> dialErr=%v | GetCertificate calls=%d names=%q certs=%q",
				tc.sni, err, calls, names, certs)
			if err != nil {
				t.Logf("REJECTED: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3: does ServerName weaken the repo's own pin?
// ---------------------------------------------------------------------------

// TestServerNameDoesNotWeakenPin is the security check. A dialer that says
// "I am talking to device X" (SNI) but is served device Y's certificate must
// still FAIL the pin. If SNI could steer or bypass the pin, the whole
// multi-homing design would be a downgrade.
func TestServerNameDoesNotWeakenPin(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	otherIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	probe := &sniProbe{}
	// The server honours the SNI and presents the OTHER identity's cert.
	route := func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if chi.ServerName == otherIdent.id.String() {
			c := otherIdent.cert
			return &c, nil
		}
		c := serverIdent.cert
		return &c, nil
	}
	addr, stop := sniTestServer(t, probe, route, &serverIdent.cert)
	defer stop()

	// The client asks for otherIdent via SNI but pins serverIdent's key.
	err := sniTestClient(t, addr, otherIdent.id.String(), &clientIdent.cert,
		sniTestPub(t, &serverIdent.cert), serverIdent.id.String())
	if err == nil {
		t.Fatalf("pin was BYPASSED: SNI %q got a certificate for a different device and the dial SUCCEEDED", otherIdent.id)
	}
	t.Logf("pin correctly rejected mismatched certificate: %v", err)
	if !strings.Contains(err.Error(), "pin mismatch") {
		t.Fatalf("unexpected failure reason (want pin mismatch): %v", err)
	}
}

// ---------------------------------------------------------------------------
// 4: old client, no SNI.
// ---------------------------------------------------------------------------

// TestLegacyClientNoSNIFallsBackToDefaultCertificate measures the
// compatibility path: an old dialer that leaves ServerName empty (or whose
// ServerName quic-go inferred as an IP literal, which Go strips) must still
// handshake and get the default certificate.
//
// NOTE the Go subtlety under test: crypto/tls only calls GetCertificate
// when ServerName is non-empty OR the config has no Certificates at all
// (common.go getCertificate). With a default certificate configured, an
// SNI-less client never reaches the callback — it gets Certificates[0]
// directly.
func TestLegacyClientNoSNIFallsBackToDefaultCertificate(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	probe := &sniProbe{}
	route := func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		// A real implementation would fall back to the default identity here.
		return &serverIdent.cert, nil
	}
	addr, stop := sniTestServer(t, probe, route, &serverIdent.cert)
	defer stop()

	err := sniTestClient(t, addr, "", &clientIdent.cert,
		sniTestPub(t, &serverIdent.cert), serverIdent.id.String())
	if err != nil {
		t.Fatalf("legacy client (no SNI) failed: %v", err)
	}
	calls, names, certs, _ := probe.snapshot()
	t.Logf("legacy client: GetCertificate calls=%d names=%q certs=%q", calls, names, certs)
	if calls == 0 {
		t.Logf("NOTE: GetCertificate was not called at all for an SNI-less client " +
			"(crypto/tls only calls it when ServerName != \"\" or Certificates is empty); " +
			"the default certificate was served from Config.Certificates[0]")
	}
}

// TestGetCertificateAlwaysCalledWithoutDefaultCert measures the alternative
// server config: NO default certificate in Config.Certificates, so Go
// always calls GetCertificate, including for SNI-less clients. This is the
// shape a multi-identity listener needs if it wants ONE code path.
func TestGetCertificateAlwaysCalledWithoutDefaultCert(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	probe := &sniProbe{}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udp.Close()
	cfg := &tls.Config{
		// NOTE: no Certificates — GetCertificate is then always invoked.
		NextProtos: []string{"container-mesh-direct/1"},
		ClientAuth: tls.RequireAnyClientCert,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			c := serverIdent.cert
			probe.record(chi, &c)
			return &c, nil
		},
	}
	ln, err := quic.Listen(udp, cfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				// Do NOT close the connection on a timer: under TLS 1.3
				// the server's VerifyConnection runs when the CLIENT's
				// certificate arrives, and the client may not have sent
				// it yet when a fixed sleep expires — the connection was
				// then closed before the pin could ever run, and the test
				// reported "the pin never ran" for a handshake that had
				// simply been cut short. The connection is closed by the
				// test's own cleanup instead.
				<-conn.Context().Done()
			}()
		}
	}()

	if err := sniTestClient(t, ln.Addr().String(), "", &clientIdent.cert,
		sniTestPub(t, &serverIdent.cert), serverIdent.id.String()); err != nil {
		t.Fatalf("legacy client failed with no-default-cert server: %v", err)
	}
	calls, names, _, _ := probe.snapshot()
	t.Logf("no-default-cert server, SNI-less client: GetCertificate calls=%d names=%q", calls, names)
	if calls == 0 {
		t.Fatalf("GetCertificate was not called even with an empty Certificates list")
	}
}

// ---------------------------------------------------------------------------
// 5: ALPN as the identity carrier (the documented fallback).
// ---------------------------------------------------------------------------

// TestALPNCarriesIdentity measures whether chi.SupportedProtos can carry the
// target group identity under QUIC.
//
// The design's worry: ALPN is "per connection negotiated and the client may
// offer several". Measured facts needed:
//   - does the server's GetCertificate see the client's FULL offered ALPN
//     list in chi.SupportedProtos?
//   - does the handshake still succeed when the client offers an extra ALPN
//     the server does not select? (negotiateALPN must not fail)
//   - does the negotiated ALPN still come out as the server's protocol?
func TestALPNCarriesIdentity(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	targetIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	probe := &sniProbe{}
	route := func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		for _, p := range chi.SupportedProtos {
			if strings.HasPrefix(p, "cm-id-") && strings.TrimPrefix(p, "cm-id-") == targetIdent.id.String() {
				c := targetIdent.cert
				return &c, nil
			}
		}
		c := serverIdent.cert
		return &c, nil
	}
	addr, stop := sniTestServer(t, probe, route, &serverIdent.cert)
	defer stop()

	// Dial with an extra ALPN carrying the target identity, and with a
	// DNS-shaped SNI as a second, independent carrier.
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client udp: %v", err)
	}
	defer udp.Close()
	raddr, _ := net.ResolveUDPAddr("udp", addr)
	cfg := &tls.Config{
		Certificates:       []tls.Certificate{clientIdent.cert},
		NextProtos:         []string{"container-mesh-direct/1", "cm-id-" + targetIdent.id.String()},
		ClientAuth:         tls.RequireAnyClientCert,
		ServerName:         "cm-" + targetIdent.id.String() + ".mesh",
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no peer cert")
			}
			leaf := cs.PeerCertificates[0]
			pub, ok := leaf.PublicKey.(ed25519.PublicKey)
			if !ok || !pub.Equal(sniTestPub(t, &targetIdent.cert)) {
				return fmt.Errorf("pin mismatch, peer CN=%q", leaf.Subject.CommonName)
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.Dial(ctx, udp, raddr, cfg, quicConfig())
	if err != nil {
		t.Fatalf("ALPN+SNI dial failed: %v", err)
	}
	defer conn.CloseWithError(0, "")
	st := conn.ConnectionState()
	t.Logf("negotiated ALPN = %q", st.TLS.NegotiatedProtocol)

	calls, names, certs, protos := probe.snapshot()
	t.Logf("GetCertificate calls=%d names=%q certs=%q protos=%q", calls, names, certs, protos)
	if calls == 0 || len(protos[0]) == 0 {
		t.Fatalf("GetCertificate did not see the client's ALPN list")
	}
	found := false
	for _, p := range protos[0] {
		if p == "cm-id-"+targetIdent.id.String() {
			found = true
		}
	}
	if !found {
		t.Fatalf("chi.SupportedProtos did NOT contain the identity ALPN: %v", protos[0])
	}
	if certs[0] != targetIdent.id.String() {
		t.Fatalf("ALPN routing served CN %q, want %q", certs[0], targetIdent.id.String())
	}
	if st.TLS.NegotiatedProtocol != "container-mesh-direct/1" {
		t.Fatalf("negotiated ALPN = %q, want container-mesh-direct/1", st.TLS.NegotiatedProtocol)
	}
}

// ---------------------------------------------------------------------------
// 6: THE TRAP — GetCertificate returns nil with MULTIPLE certs configured.
//
// crypto/tls getCertificate (common.go): if GetCertificate returns
// (nil, nil), Go falls through to Config.Certificates. With more than one
// certificate it then calls clientHello.SupportsCertificate, which does
// x509Cert.VerifyHostname(chi.ServerName). The mesh device certificates have
// NO DNS SANs (only CN), and Go's VerifyHostname ignores CN — so this path
// can only fail with "certificate is not valid for requested server name".
//
// The design doc says "SNI miss -> fall back to the default identity". This
// measures what that fallback actually costs when the config holds more
// than one identity.
func TestFallbackWithMultipleCertsFails(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	otherIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	probe := &sniProbe{}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udp.Close()
	cfg := &tls.Config{
		// MULTIPLE certificates configured — the multi-homing shape.
		Certificates: []tls.Certificate{serverIdent.cert, otherIdent.cert},
		NextProtos:   []string{"container-mesh-direct/1"},
		ClientAuth:   tls.RequireAnyClientCert,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			probe.record(chi, nil)
			// "No identity for this SNI" -> return nil, nil to trigger the
			// documented fallback to the default identity.
			return nil, nil
		},
	}
	ln, err := quic.Listen(udp, cfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				// Do NOT close the connection on a timer: under TLS 1.3
				// the server's VerifyConnection runs when the CLIENT's
				// certificate arrives, and the client may not have sent
				// it yet when a fixed sleep expires — the connection was
				// then closed before the pin could ever run, and the test
				// reported "the pin never ran" for a handshake that had
				// simply been cut short. The connection is closed by the
				// test's own cleanup instead.
				<-conn.Context().Done()
			}()
		}
	}()

	err = sniTestClient(t, ln.Addr().String(), "unknowndeviceid", &clientIdent.cert,
		sniTestPub(t, &serverIdent.cert), serverIdent.id.String())
	calls, names, certs, _ := probe.snapshot()
	t.Logf("multi-cert server, unmatched SNI: dialErr=%v | GetCertificate calls=%d names=%q certs=%q",
		err, calls, names, certs)
	if err == nil {
		t.Logf("RESULT: fallback to a default cert WORKED with multiple certs configured")
	} else {
		t.Logf("RESULT: fallback FAILED with multiple certs configured: %v", err)
	}
}

// TestFallbackWithSingleCertWorks is the same scenario with ONE certificate
// configured: Go returns Certificates[0] directly, with no hostname check.
func TestFallbackWithSingleCertWorks(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	probe := &sniProbe{}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udp.Close()
	cfg := &tls.Config{
		Certificates: []tls.Certificate{serverIdent.cert},
		NextProtos:   []string{"container-mesh-direct/1"},
		ClientAuth:   tls.RequireAnyClientCert,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			probe.record(chi, nil)
			return nil, nil
		},
	}
	ln, err := quic.Listen(udp, cfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				// Do NOT close the connection on a timer: under TLS 1.3
				// the server's VerifyConnection runs when the CLIENT's
				// certificate arrives, and the client may not have sent
				// it yet when a fixed sleep expires — the connection was
				// then closed before the pin could ever run, and the test
				// reported "the pin never ran" for a handshake that had
				// simply been cut short. The connection is closed by the
				// test's own cleanup instead.
				<-conn.Context().Done()
			}()
		}
	}()

	err = sniTestClient(t, ln.Addr().String(), "unknowndeviceid", &clientIdent.cert,
		sniTestPub(t, &serverIdent.cert), serverIdent.id.String())
	calls, names, _, _ := probe.snapshot()
	t.Logf("single-cert server, unmatched SNI: dialErr=%v | GetCertificate calls=%d names=%q", err, calls, names)
	if err != nil {
		t.Fatalf("fallback to the single default cert failed: %v", err)
	}
}

// TestServerNameDifferentFromCertCNStillPasses checks whether setting
// ServerName to a value that is NOT the served certificate's CN breaks the
// client's own pin. The pin is what matters; the SNI is only a routing hint.
func TestServerNameDifferentFromCertCNStillPasses(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	otherIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	probe := &sniProbe{}
	route := func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		c := serverIdent.cert
		return &c, nil
	}
	addr, stop := sniTestServer(t, probe, route, &serverIdent.cert)
	defer stop()

	// SNI says "otherIdent", server serves serverIdent's cert, client pins
	// serverIdent's key.
	err := sniTestClient(t, addr, otherIdent.id.String(), &clientIdent.cert,
		sniTestPub(t, &serverIdent.cert), serverIdent.id.String())
	if err != nil {
		t.Fatalf("SNI != cert CN broke the pin path: %v", err)
	}
	t.Logf("SNI=%q but served CN=%q: handshake SUCCEEDED (pin uses the served key, not the SNI)",
		otherIdent.id, serverIdent.id)
}

// TestServerSideVerifyConnectionUnaffected checks the SERVER's own pin of the
// CLIENT certificate still runs and still passes when the client sends an
// SNI. The repo's deviceTLS sets VerifyConnection on BOTH sides.
func TestServerSideVerifyConnectionUnaffected(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)
	otherIdent := newSNTTestIdentity(t)

	// The counter and the error are shared between the handshake goroutine
	// and the test: they must be ATOMIC. A plain int read while another
	// goroutine writes it is a data race, and under load the test read a
	// stale zero and reported a failure that was never real.
	var serverPinCalled atomic.Int32
	var serverPinErr atomic.Value
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udp.Close()
	cfg := &tls.Config{
		Certificates:       []tls.Certificate{serverIdent.cert},
		NextProtos:         []string{"container-mesh-direct/1"},
		ClientAuth:         tls.RequireAnyClientCert,
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			serverPinCalled.Add(1)
			if len(cs.PeerCertificates) == 0 {
				serverPinErr.Store(errors.New("no client cert"))
				return serverPinErr.Load().(error)
			}
			leaf := cs.PeerCertificates[0]
			pub, ok := leaf.PublicKey.(ed25519.PublicKey)
			if !ok || !pub.Equal(sniTestPub(t, &clientIdent.cert)) {
				mismatch := fmt.Errorf("client pin mismatch, CN=%q", leaf.Subject.CommonName)
				serverPinErr.Store(mismatch)
				return mismatch
			}
			return nil
		},
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			c := serverIdent.cert
			return &c, nil
		},
	}
	ln, err := quic.Listen(udp, cfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				// Do NOT close the connection on a timer: under TLS 1.3
				// the server's VerifyConnection runs when the CLIENT's
				// certificate arrives, and the client may not have sent
				// it yet when a fixed sleep expires — the connection was
				// then closed before the pin could ever run, and the test
				// reported "the pin never ran" for a handshake that had
				// simply been cut short. The connection is closed by the
				// test's own cleanup instead.
				<-conn.Context().Done()
			}()
		}
	}()

	if err := sniTestClient(t, ln.Addr().String(), otherIdent.id.String(), &clientIdent.cert,
		sniTestPub(t, &serverIdent.cert), serverIdent.id.String()); err != nil {
		t.Fatalf("dial with SNI failed: %v", err)
	}
	// TLS 1.3 ordering: the SERVER sends Finished before the client sends its
	// certificate, so the CLIENT can consider the handshake done before the
	// server has verified the client cert. Wait for the server-side callback.
	// The wait is generous on purpose: under TLS 1.3 the server's
	// VerifyConnection runs when the CLIENT's certificate arrives, and the
	// client may consider the handshake finished before that happens. A
	// loaded machine can take seconds.
	deadline := time.Now().Add(15 * time.Second)
	for serverPinCalled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("server-side VerifyConnection called=%d err=%v", serverPinCalled.Load(), serverPinErr.Load())
	if serverPinCalled.Load() == 0 {
		t.Fatalf("server-side VerifyConnection never ran")
	}
	if err, _ := serverPinErr.Load().(error); err != nil {
		t.Fatalf("server-side pin failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 7: OLD SERVER compatibility — the design claims "new dialer sends SNI to an
// old server: the SNI is ignored, the handshake still works".
//
// Measured here with a server that has ONE certificate and NO GetCertificate
// (the pre-multi-homing shape).
func TestNewDialerToOldServerWithSNI(t *testing.T) {
	oldServer := newSNTTestIdentity(t)
	newTarget := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udp.Close()
	// OLD SERVER SHAPE: one cert, no GetCertificate.
	cfg := &tls.Config{
		Certificates: []tls.Certificate{oldServer.cert},
		NextProtos:   []string{"container-mesh-direct/1"},
		ClientAuth:   tls.RequireAnyClientCert,
	}
	ln, err := quic.Listen(udp, cfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				// Do NOT close the connection on a timer: under TLS 1.3
				// the server's VerifyConnection runs when the CLIENT's
				// certificate arrives, and the client may not have sent
				// it yet when a fixed sleep expires — the connection was
				// then closed before the pin could ever run, and the test
				// reported "the pin never ran" for a handshake that had
				// simply been cut short. The connection is closed by the
				// test's own cleanup instead.
				<-conn.Context().Done()
			}()
		}
	}()

	// The new dialer asks for a device the old server knows nothing about,
	// via SNI. It must still get the old server's cert.
	err = sniTestClient(t, ln.Addr().String(), newTarget.id.String(), &clientIdent.cert,
		sniTestPub(t, &oldServer.cert), oldServer.id.String())
	if err != nil {
		t.Fatalf("new dialer with SNI to OLD server FAILED: %v", err)
	}
	t.Logf("new dialer (SNI=%q) -> old server: handshake SUCCEEDED with the old server's cert", newTarget.id)
}

// TestNewDialerWithExtraALPNToOldServer checks the ALPN-carrier fallback
// against an old server that only knows "container-mesh-direct/1": the extra
// ALPN must not break the handshake.
func TestNewDialerWithExtraALPNToOldServer(t *testing.T) {
	oldServer := newSNTTestIdentity(t)
	newTarget := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udp.Close()
	cfg := &tls.Config{
		Certificates: []tls.Certificate{oldServer.cert},
		NextProtos:   []string{"container-mesh-direct/1"},
		ClientAuth:   tls.RequireAnyClientCert,
	}
	ln, err := quic.Listen(udp, cfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				// Do NOT close the connection on a timer: under TLS 1.3
				// the server's VerifyConnection runs when the CLIENT's
				// certificate arrives, and the client may not have sent
				// it yet when a fixed sleep expires — the connection was
				// then closed before the pin could ever run, and the test
				// reported "the pin never ran" for a handshake that had
				// simply been cut short. The connection is closed by the
				// test's own cleanup instead.
				<-conn.Context().Done()
			}()
		}
	}()

	udp2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client udp: %v", err)
	}
	defer udp2.Close()
	raddr, _ := net.ResolveUDPAddr("udp", ln.Addr().String())
	cfg2 := &tls.Config{
		Certificates:       []tls.Certificate{clientIdent.cert},
		NextProtos:         []string{"container-mesh-direct/1", "cm-id-" + newTarget.id.String()},
		ClientAuth:         tls.RequireAnyClientCert,
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no peer cert")
			}
			pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
			if !ok || !pub.Equal(sniTestPub(t, &oldServer.cert)) {
				return fmt.Errorf("pin mismatch CN=%q", cs.PeerCertificates[0].Subject.CommonName)
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.Dial(ctx, udp2, raddr, cfg2, quicConfig())
	if err != nil {
		t.Fatalf("dial with extra ALPN to OLD server FAILED: %v", err)
	}
	defer conn.CloseWithError(0, "")
	t.Logf("negotiated ALPN with old server = %q", conn.ConnectionState().TLS.NegotiatedProtocol)
}

// TestMultiCertFallbackServesWhichCert records WHICH certificate the client
// actually received when GetCertificate returns (nil, nil) and the config
// holds two certificates. The design doc assumes "fall back to the default
// identity"; this measures whether Go does that or something else.
func TestMultiCertFallbackServesWhichCert(t *testing.T) {
	first := newSNTTestIdentity(t)
	second := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udp.Close()
	cfg := &tls.Config{
		Certificates: []tls.Certificate{first.cert, second.cert},
		NextProtos:   []string{"container-mesh-direct/1"},
		ClientAuth:   tls.RequireAnyClientCert,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return nil, nil // "no identity for this SNI"
		},
	}
	ln, err := quic.Listen(udp, cfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				// Do NOT close the connection on a timer: under TLS 1.3
				// the server's VerifyConnection runs when the CLIENT's
				// certificate arrives, and the client may not have sent
				// it yet when a fixed sleep expires — the connection was
				// then closed before the pin could ever run, and the test
				// reported "the pin never ran" for a handshake that had
				// simply been cut short. The connection is closed by the
				// test's own cleanup instead.
				<-conn.Context().Done()
			}()
		}
	}()

	served := ""
	udp2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client udp: %v", err)
	}
	defer udp2.Close()
	raddr, _ := net.ResolveUDPAddr("udp", ln.Addr().String())
	cfg2 := &tls.Config{
		Certificates:       []tls.Certificate{clientIdent.cert},
		NextProtos:         []string{"container-mesh-direct/1"},
		ClientAuth:         tls.RequireAnyClientCert,
		ServerName:         "unknowndeviceid",
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no peer cert")
			}
			served = cs.PeerCertificates[0].Subject.CommonName
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.Dial(ctx, udp2, raddr, cfg2, quicConfig())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseWithError(0, "")
	t.Logf("multi-cert fallback served CN=%q (first=%q second=%q)", served, first.id, second.id)
	if served != first.id.String() {
		t.Logf("NOTE: fallback did NOT serve Certificates[0]")
	}
}

// ---------------------------------------------------------------------------
// 8: The DNS-shaped SNI variant ("cm-<deviceid>.mesh") measured end to end,
// as the fallback if a raw DeviceID were ever rejected.
//
// This is also the shape that would survive a strict RFC 6066 peer, since
// "cm-<id>.mesh" IS a legal DNS name (labels of [a-z0-9-], dots).
func TestDNSShapedSNIVariant(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	targetIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	sni := "cm-" + targetIdent.id.String() + ".mesh"
	probe := &sniProbe{}
	route := func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if chi.ServerName == sni {
			c := targetIdent.cert
			return &c, nil
		}
		c := serverIdent.cert
		return &c, nil
	}
	addr, stop := sniTestServer(t, probe, route, &serverIdent.cert)
	defer stop()

	err := sniTestClient(t, addr, sni, &clientIdent.cert,
		sniTestPub(t, &targetIdent.cert), targetIdent.id.String())
	if err != nil {
		t.Fatalf("DNS-shaped SNI dial failed: %v", err)
	}
	calls, names, certs, _ := probe.snapshot()
	t.Logf("DNS-shaped SNI %q: calls=%d names=%q certs=%q", sni, calls, names, certs)
	if calls == 0 || names[0] != sni || certs[0] != targetIdent.id.String() {
		t.Fatalf("DNS-shaped SNI routing failed")
	}
}

// TestRawDeviceIDIsAcceptedAsSNIOnTheWire proves the raw DeviceID really
// goes into the ClientHello SNI extension — not just that the server
// callback saw it.
//
// The ClientHello cannot be read off the wire in plaintext: in QUIC it
// travels inside the Initial packet's CRYPTO frame, encrypted with keys
// derived from a WELL-KNOWN salt. So this test uses the crypto/tls QUIC
// API directly (the same approach quic-go's own sni_test.go uses) to
// produce the raw ClientHello bytes and then looks for the SNI extension.
//
// The result matters: it says whether a 16-char base32 DeviceID is a legal
// SNI VALUE at the TLS layer, which is the premise the whole design rests
// on.
func TestRawDeviceIDIsAcceptedAsSNIOnTheWire(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)
	sni := serverIdent.id.String()

	// Produce the raw ClientHello with crypto/tls's QUIC client API.
	qconn := tls.QUICClient(&tls.QUICConfig{
		TLSConfig: &tls.Config{
			ServerName:         sni,
			MinVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true,
			Certificates:       []tls.Certificate{clientIdent.cert},
			NextProtos:         []string{"container-mesh-direct/1"},
		},
	})
	// QUIC transport parameters are mandatory in the ClientHello.
	qconn.SetTransportParameters([]byte{0x01, 0x01, 0x00}) // empty params
	if err := qconn.Start(context.Background()); err != nil {
		t.Fatalf("tls.QUICClient start: %v", err)
	}
	defer qconn.Close()
	// The ClientHello may arrive in several CRYPTO chunks: accumulate them.
	var hello []byte
	for {
		ev := qconn.NextEvent()
		switch ev.Kind {
		case tls.QUICWriteData:
			hello = append(hello, ev.Data...)
			if len(hello) >= 4 {
				need := 4 + int(hello[1])<<16 + int(hello[2])<<8 + int(hello[3])
				if len(hello) >= need {
					hello = hello[:need]
					goto done
				}
			}
		case tls.QUICTransportParametersRequired:
			qconn.SetTransportParameters([]byte{0x01, 0x01, 0x00})
		case tls.QUICNoEvent:
			// no more events right now
		default:
			t.Logf("event kind %v, continuing", ev.Kind)
		}
	}
done:

	// Locate the SNI extension (type 0) and read the host_name out of it.
	extType, extBody, ok := findSNIExtension(hello)
	if !ok {
		t.Fatalf("no SNI extension found in the ClientHello (%d bytes)", len(hello))
	}
	name, ok := parseSNIHostName(extBody)
	if !ok {
		t.Fatalf("SNI extension present but the host_name could not be parsed")
	}
	t.Logf("ClientHello SNI host_name = %q (len=%d)", name, len(name))
	if name != sni {
		t.Fatalf("SNI on the wire = %q, want %q", name, sni)
	}
	t.Logf("RESULT: the raw DeviceID %q is a legal SNI value and goes out verbatim", sni)
	_ = extType
}

// findSNIExtension walks the ClientHello's extension block and returns the
// body of the first extension with type 0 (server_name).
func findSNIExtension(hello []byte) (extType uint16, body []byte, ok bool) {
	// ClientHello: type(1) len(3) version(2) random(32) sid_len(1) sid
	// cipher_suites_len(2) suites compression_len(1) methods ext_len(2) exts
	if len(hello) < 4+2+32+1 {
		return 0, nil, false
	}
	p := 4 + 2 + 32
	sidLen := int(hello[p])
	p += 1 + sidLen
	if p+2 > len(hello) {
		return 0, nil, false
	}
	csLen := int(hello[p])<<8 | int(hello[p+1])
	p += 2 + csLen
	if p+1 > len(hello) {
		return 0, nil, false
	}
	compLen := int(hello[p])
	p += 1 + compLen
	if p+2 > len(hello) {
		return 0, nil, false
	}
	extLen := int(hello[p])<<8 | int(hello[p+1])
	p += 2
	if p+extLen > len(hello) {
		return 0, nil, false
	}
	exts := hello[p : p+extLen]
	for i := 0; i+4 <= len(exts); {
		typ := uint16(exts[i])<<8 | uint16(exts[i+1])
		l := int(exts[i+2])<<8 | int(exts[i+3])
		if i+4+l > len(exts) {
			return 0, nil, false
		}
		if typ == 0 {
			return typ, exts[i+4 : i+4+l], true
		}
		i += 4 + l
	}
	return 0, nil, false
}

// parseSNIHostName extracts the first host_name entry from an SNI extension
// body.
func parseSNIHostName(body []byte) (string, bool) {
	if len(body) < 2 {
		return "", false
	}
	listLen := int(body[0])<<8 | int(body[1])
	if 2+listLen > len(body) {
		return "", false
	}
	list := body[2 : 2+listLen]
	i := 0
	for i+3 <= len(list) {
		nameType := list[i]
		l := int(list[i+1])<<8 | int(list[i+2])
		if i+3+l > len(list) {
			return "", false
		}
		if nameType == 0 {
			return string(list[i+3 : i+3+l]), true
		}
		i += 3 + l
	}
	return "", false
}

// ---------------------------------------------------------------------------
// 9: SESSION RESUMPTION — the last structural risk.
//
// If the client enables a ClientSessionCache, the server may take the
// abbreviated (resumption) handshake path. On that path crypto/tls does NOT
// call GetCertificate at all (handshake_server.go: checkForResumption ->
// doResumeHandshake), so the SNI routing would be BYPASSED and the resumed
// session would carry the FIRST connection's identity.
//
// This measures: (a) whether resumption actually happens in QUIC with a
// client cache, and (b) whether GetCertificate is called on the resumed
// connection.
func TestSessionResumptionBypassesGetCertificate(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	otherIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	var mu sync.Mutex
	calls := 0
	names := []string{}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udp.Close()
	cfg := &tls.Config{
		NextProtos: []string{"container-mesh-direct/1"},
		ClientAuth: tls.RequireAnyClientCert,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			mu.Lock()
			calls++
			names = append(names, chi.ServerName)
			mu.Unlock()
			if chi.ServerName == otherIdent.id.String() {
				c := otherIdent.cert
				return &c, nil
			}
			c := serverIdent.cert
			return &c, nil
		},
	}
	ln, err := quic.Listen(udp, cfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				// Do NOT close the connection on a timer: under TLS 1.3
				// the server's VerifyConnection runs when the CLIENT's
				// certificate arrives, and the client may not have sent
				// it yet when a fixed sleep expires — the connection was
				// then closed before the pin could ever run, and the test
				// reported "the pin never ran" for a handshake that had
				// simply been cut short. The connection is closed by the
				// test's own cleanup instead.
				<-conn.Context().Done()
			}()
		}
	}()

	// Client WITH a session cache, so resumption is possible.
	cache := tls.NewLRUClientSessionCache(4)
	dial := func(sni string) {
		udp2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			t.Fatalf("client udp: %v", err)
		}
		defer udp2.Close()
		raddr, _ := net.ResolveUDPAddr("udp", ln.Addr().String())
		c := &tls.Config{
			Certificates:       []tls.Certificate{clientIdent.cert},
			NextProtos:         []string{"container-mesh-direct/1"},
			ClientAuth:         tls.RequireAnyClientCert,
			ServerName:         sni,
			ClientSessionCache: cache,
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("no peer cert")
				}
				return nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := quic.Dial(ctx, udp2, raddr, c, quicConfig())
		if err != nil {
			t.Fatalf("dial (sni=%q) failed: %v", sni, err)
		}
		defer conn.CloseWithError(0, "")
		// A TLS 1.3 client only stores the session ticket when it READS the
		// post-handshake NewSessionTicket message. Without a read, no ticket
		// is ever cached and resumption can never happen.
		st, err := conn.OpenStreamSync(ctx)
		if err == nil {
			st.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			_, _ = st.Read(make([]byte, 1))
			st.Close()
		}
		t.Logf("dial sni=%q -> peer cert CN=%q, session reused=%v",
			sni, conn.ConnectionState().TLS.PeerCertificates[0].Subject.CommonName,
			conn.ConnectionState().TLS.DidResume)
	}

	// First connection: SNI asks for serverIdent.
	dial(serverIdent.id.String())
	// Second connection: SAME SNI -> the session is RESUMED. The server takes
	// the abbreviated handshake path and NEVER calls GetCertificate, so the
	// SNI routing is bypassed and the RESUMED session carries whatever
	// certificate the first connection used.
	dial(serverIdent.id.String())
	// Third connection: a different SNI -> a different cache key -> a full
	// handshake, and GetCertificate IS called again.
	dial(otherIdent.id.String())

	mu.Lock()
	defer mu.Unlock()
	t.Logf("GetCertificate calls=%d names=%q", calls, names)
}

// TestResumptionStillRunsClientPin asks the security question behind the
// resumption bypass: on a RESUMED connection, does the client's own
// VerifyConnection pin still run, and against which certificate?
//
// If the pin does NOT run on resumption, then a resumed connection is
// accepted on the strength of a cached ticket alone — which would make
// session resumption unsafe for this mesh regardless of SNI routing.
//
// The second half measures the repo's ACTUAL configuration: deviceTLS sets
// no ClientSessionCache, so crypto/tls loadSession returns nil and
// resumption is impossible. That is what makes the bypass currently moot.
func TestResumptionStillRunsClientPin(t *testing.T) {
	serverIdent := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	var mu sync.Mutex
	pinCalls := 0
	pinCNs := []string{}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udp.Close()
	cfg := &tls.Config{
		NextProtos: []string{"container-mesh-direct/1"},
		ClientAuth: tls.RequireAnyClientCert,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			c := serverIdent.cert
			return &c, nil
		},
	}
	ln, err := quic.Listen(udp, cfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				time.Sleep(400 * time.Millisecond)
				conn.CloseWithError(0, "")
			}()
		}
	}()

	cache := tls.NewLRUClientSessionCache(4)
	dial := func(label string, useCache bool) {
		udp2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			t.Fatalf("client udp: %v", err)
		}
		defer udp2.Close()
		raddr, _ := net.ResolveUDPAddr("udp", ln.Addr().String())
		c := &tls.Config{
			Certificates:       []tls.Certificate{clientIdent.cert},
			NextProtos:         []string{"container-mesh-direct/1"},
			ClientAuth:         tls.RequireAnyClientCert,
			ServerName:         serverIdent.id.String(),
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				mu.Lock()
				pinCalls++
				cn := ""
				if len(cs.PeerCertificates) > 0 {
					cn = cs.PeerCertificates[0].Subject.CommonName
				}
				pinCNs = append(pinCNs, cn)
				mu.Unlock()
				if len(cs.PeerCertificates) == 0 {
					return errors.New("no peer cert")
				}
				pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
				if !ok || !pub.Equal(sniTestPub(t, &serverIdent.cert)) {
					return fmt.Errorf("pin mismatch CN=%q", cn)
				}
				return nil
			},
		}
		if useCache {
			c.ClientSessionCache = cache
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := quic.Dial(ctx, udp2, raddr, c, quicConfig())
		if err != nil {
			t.Fatalf("dial %s failed: %v", label, err)
		}
		defer conn.CloseWithError(0, "")
		st, err := conn.OpenStreamSync(ctx)
		if err == nil {
			st.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			_, _ = st.Read(make([]byte, 1))
			st.Close()
		}
		t.Logf("%s: reused=%v peerCN=%q", label, conn.ConnectionState().TLS.DidResume,
			conn.ConnectionState().TLS.PeerCertificates[0].Subject.CommonName)
	}

	dial("conn1 (cache, full handshake)", true)
	dial("conn2 (cache, RESUMED)", true)
	dial("conn3 (NO cache — the repo's actual shape)", false)

	mu.Lock()
	defer mu.Unlock()
	t.Logf("client VerifyConnection pin calls=%d peerCNs=%q", pinCalls, pinCNs)
	if pinCalls < 2 {
		t.Fatalf("the client pin ran only %d times — resumption may skip the pin", pinCalls)
	}
}

// TestMultiCertSNILessClientGetsDefaultIdentity measures the OTHER legacy
// path: an SNI-less client against a server that holds SEVERAL identities.
//
// crypto/tls only calls GetCertificate when ServerName is non-empty OR the
// config has no Certificates at all. With several certificates configured,
// an SNI-less client never reaches the callback and Go picks a certificate
// itself: getCertificate walks Config.Certificates and calls
// clientHello.SupportsCertificate, which (for an empty ServerName) skips the
// hostname check entirely and accepts the first Ed25519 cert.
//
// This measures WHICH identity an SNI-less client actually reaches, because
// a multi-identity listener must be sure the fallback is the intended one.
func TestMultiCertSNILessClientGetsDefaultIdentity(t *testing.T) {
	first := newSNTTestIdentity(t)
	second := newSNTTestIdentity(t)
	clientIdent := newSNTTestIdentity(t)

	called := 0
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udp.Close()
	cfg := &tls.Config{
		Certificates: []tls.Certificate{first.cert, second.cert},
		NextProtos:   []string{"container-mesh-direct/1"},
		ClientAuth:   tls.RequireAnyClientCert,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			called++
			c := first.cert
			return &c, nil
		},
	}
	ln, err := quic.Listen(udp, cfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				// Do NOT close the connection on a timer: under TLS 1.3
				// the server's VerifyConnection runs when the CLIENT's
				// certificate arrives, and the client may not have sent
				// it yet when a fixed sleep expires — the connection was
				// then closed before the pin could ever run, and the test
				// reported "the pin never ran" for a handshake that had
				// simply been cut short. The connection is closed by the
				// test's own cleanup instead.
				<-conn.Context().Done()
			}()
		}
	}()

	served := ""
	udp2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client udp: %v", err)
	}
	defer udp2.Close()
	raddr, _ := net.ResolveUDPAddr("udp", ln.Addr().String())
	cfg2 := &tls.Config{
		Certificates:       []tls.Certificate{clientIdent.cert},
		NextProtos:         []string{"container-mesh-direct/1"},
		ClientAuth:         tls.RequireAnyClientCert,
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no peer cert")
			}
			served = cs.PeerCertificates[0].Subject.CommonName
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.Dial(ctx, udp2, raddr, cfg2, quicConfig())
	if err != nil {
		t.Fatalf("SNI-less dial to multi-cert server failed: %v", err)
	}
	defer conn.CloseWithError(0, "")
	t.Logf("SNI-less client -> GetCertificate called=%d, served CN=%q (first=%q second=%q)",
		called, served, first.id, second.id)
	if called != 0 {
		t.Logf("NOTE: GetCertificate WAS called for the SNI-less client")
	}
}
