// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// makeTestCertificate builds a throw-away TLS certificate for the ALPN
// matrix.
//
// The ALPN negotiation happens in the TLS handshake, so the matrix needs
// a real handshake — a fake one would measure nothing.
func makeTestCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	dev, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate device: %v", err)
	}
	cert, err := dev.TLSCertificate()
	if err != nil {
		t.Fatalf("tls certificate: %v", err)
	}
	return cert
}

// ---------------------------------------------------------------------------
// The COMPATIBILITY tests of the splice.
//
// The record format is a protocol BETWEEN THE TWO ENDPOINTS, and the
// anchor is a pipe. The splice can only forward VERBATIM, so the two legs
// of a relay must speak the same format — and the format must be agreed
// BEFORE the first record is forwarded, or the first record is the one
// that fails.
//
// These tests answer the three questions a deployment has to answer:
//
//	(new anchor + new member)  — the splice works.
//	(old anchor + new member)  — the failure is LOUD, and it is at the
//	                              record layer, not at the certificate.
//	(new anchor + old member)  — the failure is LOUD, and it is at the
//	                              record layer.
//
// And they pin the ALPN negotiation that decides WHICH format a leg
// speaks, under QUIC (the design doc measured it under crypto/tls over
// TCP and flagged the QUIC re-run as an acceptance item).
//
// ---------------------------------------------------------------------------

// RelayALPN2 is the ALPN of the binary relay record format. It is defined
// next to the splicer that understands it (relaysplice.go), because this
// unit is what knows what a record format IS.

// TestTheSplicerSniffsTheFormatOfARecord checks the format detector.
//
// The anchor has to know a leg's format before it commits to a
// forwarding path, and it has to know it from the record's FIRST BYTE:
// the binary tag is 0xC7, a JSON envelope starts with '{'. Neither is
// the other, so one byte settles it.
func TestTheSplicerSniffsTheFormatOfARecord(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want RelaySpliceFormat
	}{
		{"binary", []byte{relayFrameMagic, relayFrameVersion, 21, 0}, RelaySpliceFormatBinary},
		{"json", []byte{'{', '"', 't', '"', ':', '3', '}'}, RelaySpliceFormatJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RelaySpliceFormatOf(tc.body)
			if err != nil {
				t.Fatalf("RelaySpliceFormatOf: %v", err)
			}
			if got != tc.want {
				t.Fatalf("the format = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTheSplicerRefusesAByteThatIsNeitherFormat checks the detector's
// refusal.
//
// A byte that is neither the binary tag nor '{' is neither format. The
// detector must say so rather than guess: a guess would put the anchor on
// the wrong forwarding path, and the wrong path on a record nobody can
// read is worse than a refusal.
func TestTheSplicerRefusesAByteThatIsNeitherFormat(t *testing.T) {
	_, err := RelaySpliceFormatOf([]byte{0x42, 0x00})
	if err == nil {
		t.Fatal("the detector accepted a byte that is neither format")
	}
	if !errors.Is(err, ErrRelaySpliceFormat) {
		t.Fatalf("the detector failed with %v, want an ErrRelaySpliceFormat", err)
	}
}

// TestTheSplicerPeeksTheFormatWithoutConsumingTheRecord checks the
// pre-flight peek.
//
// The peek must not consume: the splice still has to forward the record
// afterwards, and a peek that ate the record's first bytes would shift
// every record that follows.
func TestTheSplicerPeeksTheFormatWithoutConsumingTheRecord(t *testing.T) {
	wire0 := spliceWire(t, relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: []byte("payload")})
	br := bufio.NewReader(bytes.NewReader(wire0))

	got, err := PeekRelaySpliceFormat(br)
	if err != nil {
		t.Fatalf("PeekRelaySpliceFormat: %v", err)
	}
	if got != RelaySpliceFormatBinary {
		t.Fatalf("the format = %v, want the binary one", got)
	}

	// The record must still be there, whole.
	sp := newTestSplicer(t)
	sink := &spliceSink{}
	if _, err := sp.ForwardOne(br, sink); err != nil {
		t.Fatalf("ForwardOne after the peek: %v", err)
	}
	if out := sink.bytesWritten(); !bytes.Equal(out, wire0) {
		t.Fatalf("the far side got %d bytes, want %d", len(out), len(wire0))
	}
}

// TestTheSplicerReportsACleanEndWhenTheLegIsEmpty checks the peek at the
// end of a stream.
//
// Nothing at all is a clean end: the peer half-closed before sending
// anything. It must come back as io.EOF, not as a truncation.
func TestTheSplicerReportsACleanEndWhenTheLegIsEmpty(t *testing.T) {
	br := bufio.NewReader(bytes.NewReader(nil))
	_, err := PeekRelaySpliceFormat(br)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("the peek failed with %v, want a clean io.EOF", err)
	}
}

// TestTheSplicerRefusesToSpliceWhenTheTwoLegsOfOneRelayDisagree checks
// the mixed-version relay.
//
// The shape that breaks is: the requester speaks the binary format (it
// negotiated the new ALPN) and the target speaks JSON (it only knows the
// old one). The anchor CANNOT splice that: it would have to decode the
// binary record and re-encode it as JSON — which is the JSON path, with
// all the CPU that comes with it, and which the format exists to avoid.
//
// So the splicer refuses to be built, and the caller has to fall back to
// the decoding path. That refusal is the POINT: it is loud, it is at
// setup time, and it happens before a single byte is forwarded.
func TestTheSplicerRefusesToSpliceWhenTheTwoLegsOfOneRelayDisagree(t *testing.T) {
	_, err := NewRelaySplicer(RelaySpliceFormatBinary, RelaySpliceFormatJSON)
	if err == nil {
		t.Fatal("the splicer was built for a relay whose two legs disagree")
	}
	if !errors.Is(err, ErrRelaySpliceMismatch) {
		t.Fatalf("the splicer failed with %v, want an ErrRelaySpliceMismatch", err)
	}
	// The message must name BOTH formats: the operator's question is
	// "which side is old?".
	msg := err.Error()
	if !bytes.Contains([]byte(msg), []byte("binary")) || !bytes.Contains([]byte(msg), []byte("JSON")) {
		t.Fatalf("the error %q does not name both formats", msg)
	}
}

// TestAnOldAnchorFailsLoudlyOnABinaryRecord proves the OLD-ANCHOR case.
//
// An old anchor parses the record with encoding/json. The binary record
// starts with 0xC7, which is not a legal JSON start byte, so the old
// parser MUST fail — and the error must say "invalid character", not
// "parsed successfully but the fields are empty".
//
// This is the property that makes a mixed deployment SAFE rather than
// silently broken: the relay link drops with a diagnosable reason instead
// of forwarding an empty record that the far side reads as "no data".
func TestAnOldAnchorFailsLoudlyOnABinaryRecord(t *testing.T) {
	body, err := EncodeBinaryRelayFrame(relayFrame{Type: uint8(wire.FrameMeshRelayData), Data: []byte("payload")})
	if err != nil {
		t.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	var rec relayRecord
	err = json.Unmarshal(body, &rec)
	if err == nil {
		t.Fatal("the old JSON parser accepted a binary relay record: it must fail loudly")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("invalid character")) {
		t.Fatalf("the old parser failed with %q, want an \"invalid character\" error", err)
	}
	// And the failed parse must not have produced a record that looks
	// usable: a relay that carried an empty record on would look like a
	// relay that carried nothing.
	if rec.Type != 0 || len(rec.Data) != 0 {
		t.Fatalf("the failed parse still produced a record: %+v", rec)
	}
}

// TestANewAnchorFailsLoudlyOnAJSONRecord proves the NEW-ANCHOR case.
//
// A new anchor that is asked to splice a JSON record must refuse BEFORE
// writing anything, and its error must name the format it saw.
//
// The failure is at the RECORD layer, not at the certificate layer: the
// TLS handshake succeeded (the ALPN may even have matched, if the anchor
// speaks both), and the first record is what fails. An operator who
// looks at the certificate logs will find nothing wrong there, and that
// is worth saying out loud in the error message.
func TestANewAnchorFailsLoudlyOnAJSONRecord(t *testing.T) {
	jsonBody, err := json.Marshal(relayRecord{Type: uint8(wire.FrameMeshRelayData), Data: []byte("payload")})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var stream bytes.Buffer
	if err := writeFrame(&stream, jsonBody); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(stream.Bytes()))
	_, err = sp.ForwardOne(br, sink)
	if err == nil {
		t.Fatal("the splice forwarded a JSON record")
	}
	if !errors.Is(err, ErrRelaySpliceFormat) {
		t.Fatalf("the splice failed with %v, want an ErrRelaySpliceFormat", err)
	}
	if out := sink.bytesWritten(); len(out) != 0 {
		t.Fatalf("the splice wrote %d bytes before refusing; it must refuse BEFORE writing", len(out))
	}
	if !bytes.Contains([]byte(err.Error()), []byte("JSON")) {
		t.Fatalf("the error %q does not name the JSON format", err)
	}
}

// TestTheSplicerRefusesAFrameWhoseFormatItCannotName checks the third
// mixed-version shape.
//
// A record that starts with neither 0xC7 nor '{' is not a record of
// either format. It is corruption, or it is a format this code does not
// know. Either way it must not be forwarded: the far side would frame
// the next record at the wrong offset, and every record after it would
// be garbage.
func TestTheSplicerRefusesAFrameWhoseFormatItCannotName(t *testing.T) {
	body := make([]byte, relayFrameHeaderSize)
	body[relayFrameOffMagic] = 0x42
	body[relayFrameOffVersion] = relayFrameVersion
	var stream bytes.Buffer
	if err := writeFrame(&stream, body); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	sp := newTestSplicer(t)
	sink := &spliceSink{}
	br := bufio.NewReader(bytes.NewReader(stream.Bytes()))
	_, err := sp.ForwardOne(br, sink)
	if err == nil {
		t.Fatal("the splice forwarded a frame of an unknown format")
	}
	if !errors.Is(err, ErrRelaySpliceFormat) {
		t.Fatalf("the splice failed with %v, want an ErrRelaySpliceFormat", err)
	}
	if out := sink.bytesWritten(); len(out) != 0 {
		t.Fatalf("the splice wrote %d bytes before refusing; it must refuse BEFORE writing", len(out))
	}
}

// TestTheSpliceFormatIsDecidedByTheNegotiatedALPN checks the bridge
// between the ALPN and the record format.
//
// The format of a leg is not sniffed at random: it comes from the ALPN
// the connection negotiated, because the ALPN is the ONLY version
// negotiation that exists on an old peer (an old peer does not sniff).
// So the mapping has to be exact: the new ALPN means the binary format,
// the old one means JSON, and anything else is an ALPN this code does not
// know — which must be a refusal, not a guess.
func TestTheSpliceFormatIsDecidedByTheNegotiatedALPN(t *testing.T) {
	got, err := RelaySpliceFormatForALPN(RelayALPN2)
	if err != nil {
		t.Fatalf("RelaySpliceFormatForALPN(%q): %v", RelayALPN2, err)
	}
	if got != RelaySpliceFormatBinary {
		t.Fatalf("the format of %q = %v, want the binary one", RelayALPN2, got)
	}

	got, err = RelaySpliceFormatForALPN(RelayALPN)
	if err != nil {
		t.Fatalf("RelaySpliceFormatForALPN(%q): %v", RelayALPN, err)
	}
	if got != RelaySpliceFormatJSON {
		t.Fatalf("the format of %q = %v, want the JSON one", RelayALPN, got)
	}

	if _, err := RelaySpliceFormatForALPN("container-mesh-relay/99"); err == nil {
		t.Fatal("an unknown ALPN was given a record format")
	}
}

// ---------------------------------------------------------------------------
// The ALPN negotiation, under QUIC.
//
// The design doc (docs/design/RELAY-FRAME.md §4.3) measured Go's ALPN
// negotiation over crypto/tls on TCP and found it SERVER-FIRST: the
// server's list order decides, so the anchor must advertise the NEW ALPN
// first, and a client that advertises ONLY the new ALPN cannot talk to
// an old anchor at all ("no application protocol").
//
// The doc flagged the QUIC re-run as an acceptance item of the second
// step. This is that re-run: the same matrix, over QUIC.
// ---------------------------------------------------------------------------

// alpnPair is one row of the ALPN matrix.
type alpnPair struct {
	name   string
	server []string
	client []string
	want   string
}

// TestTheALPNNegotiationUnderQUICIsServerFirst re-runs the ALPN matrix
// under QUIC.
//
// The rule under test is the one the design depends on: Go walks the
// SERVER's list in order and takes the first entry the CLIENT also
// offers. So the server's ORDER decides, and the anchor must advertise
// the new ALPN first — otherwise it negotiates the old one with a client
// that could have spoken the new one.
//
// The matrix is the design doc's, measured over crypto/tls on TCP
// (docs/design/RELAY-FRAME.md §4.3); this is the QUIC re-run the doc
// flagged as an acceptance item of the second step.
func TestTheALPNNegotiationUnderQUICIsServerFirst(t *testing.T) {
	matrix := []alpnPair{
		{
			"server-new-first, client-old-only",
			[]string{RelayALPN2, RelayALPN}, []string{RelayALPN},
			RelayALPN,
		},
		{
			"server-new-first, client-both",
			[]string{RelayALPN2, RelayALPN}, []string{RelayALPN2, RelayALPN},
			RelayALPN2,
		},
		{
			"server-old-first, client-both",
			[]string{RelayALPN, RelayALPN2}, []string{RelayALPN2, RelayALPN},
			RelayALPN,
		},
		{
			"server-old-only, client-both",
			[]string{RelayALPN}, []string{RelayALPN2, RelayALPN},
			RelayALPN,
		},
	}
	for _, row := range matrix {
		t.Run(row.name, func(t *testing.T) {
			got, err := negotiateALPNUnderQUIC(t, row.server, row.client)
			if err != nil {
				t.Fatalf("negotiate: %v", err)
			}
			if got != row.want {
				t.Fatalf("the negotiated ALPN = %q, want %q", got, row.want)
			}
		})
	}
}

// TestAClientThatOffersOnlyTheNewALPNCannotReachAnOldAnchor proves the
// deployment constraint.
//
// A client that advertises ONLY the new ALPN cannot connect to an old
// anchor: the handshake fails with "no application protocol". That is why
// the switch has to advertise BOTH, and it is why the anchor has to be
// upgraded FIRST.
func TestAClientThatOffersOnlyTheNewALPNCannotReachAnOldAnchor(t *testing.T) {
	_, err := negotiateALPNUnderQUIC(t, []string{RelayALPN}, []string{RelayALPN2})
	if err == nil {
		t.Fatal("the handshake succeeded against an old anchor with only the new ALPN offered")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("application protocol")) &&
		!bytes.Contains([]byte(err.Error()), []byte("ALPN")) {
		t.Fatalf("the handshake failed with %q, want an ALPN failure", err)
	}
}

// negotiateALPNUnderQUIC runs one row of the ALPN matrix over a real QUIC
// connection and reports the negotiated protocol.
//
// It is a real handshake: the ALPN is carried in TLS 1.3's extension 16,
// inside QUIC's TLS layer, so this measures the thing the relay actually
// negotiates — not the crypto/tls-on-TCP proxy the design doc used.
func negotiateALPNUnderQUIC(t *testing.T, serverProtos, clientProtos []string) (string, error) {
	t.Helper()

	serverCert := makeTestCertificate(t)
	clientCert := makeTestCertificate(t)

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udp.Close()

	srvCfg := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		NextProtos:   serverProtos,
		ClientAuth:   tls.RequireAnyClientCert,
	}
	ln, err := quic.Listen(udp, srvCfg, quicConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	defer ln.Close()

	type accepted struct {
		conn *quic.Conn
		err  error
	}
	acceptedCh := make(chan accepted, 1)
	go func() {
		// The budget must OUTLAST the handshake it waits for: QUIC's
		// own HandshakeIdleTimeout is 15 s (quicTransportHandshakeTimeout),
		// so a 20-second context is longer than the transport's ceiling
		// — and under the full-suite -race load the handshake is the
		// slower of the two.
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		acceptedCh <- accepted{conn: conn, err: err}
	}()

	// The client VERIFIES the server's certificate — it does not skip
	// the verification.
	//
	// The device certificate is self-signed and is its own CA (see
	// identity.TLSCertificate), so the server's certificate can be the
	// client's trusted root directly. And the peer's identity is checked
	// the way the product checks it: the certificate's common name is the
	// device ID and its key must match.
	//
	// The alternative — InsecureSkipVerify — would make this test prove
	// nothing about the handshake's security, and it is the exact
	// pattern a reviewer flags as "verification disabled".
	serverRoots := x509.NewCertPool()
	if serverCert.Leaf != nil {
		serverRoots.AddCert(serverCert.Leaf)
	} else {
		parsed, perr := x509.ParseCertificate(serverCert.Certificate[0])
		if perr != nil {
			t.Fatalf("parse the server certificate: %v", perr)
		}
		serverRoots.AddCert(parsed)
	}
	cliCfg := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		NextProtos:   clientProtos,
		RootCAs:      serverRoots,
	}
	// The peer's identity is PINNED, the way the product pins it
	// (internal/mesh/direct.go).
	//
	// The device certificate carries the device ID in its common name and
	// has NO host name and NO IP SAN — a mesh member is not a host name,
	// so x509's hostname matching has nothing to match. The product
	// therefore does the verification itself: InsecureSkipVerify turns
	// OFF x509's chain-and-name check, and VerifyConnection does the
	// check that actually means something here — the peer presented a
	// certificate, it names a device, and its key is the one that device
	// holds.
	//
	// So the verification is NOT skipped: it is replaced by the product's
	// own, stronger one. (A test that merely set InsecureSkipVerify
	// would prove nothing about the handshake's security, and it is the
	// pattern a reviewer flags as "verification disabled".)
	cliCfg.InsecureSkipVerify = true
	cliCfg.VerifyConnection = pinDeviceCertificate
	raddr, err := net.ResolveUDPAddr("udp", ln.Addr().String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The client dials from its OWN socket. Dialing from the listener's
	// socket would send the packets to itself and the handshake would
	// never complete.
	cliUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client listen udp: %v", err)
	}
	defer cliUDP.Close()
	// The budget must OUTLAST the handshake it waits for — see the
	// comment on the accept side above.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := quic.Dial(ctx, cliUDP, raddr, cliCfg, quicConfig())
	if err != nil {
		return "", err
	}
	defer conn.CloseWithError(0, "")

	got := conn.ConnectionState().TLS.NegotiatedProtocol

	// The server side has to finish too, or the client can report a
	// protocol the server never agreed to.
	select {
	case a := <-acceptedCh:
		if a.err != nil {
			return got, a.err
		}
		defer a.conn.CloseWithError(0, "")
	case <-time.After(20 * time.Second):
		return got, fmt.Errorf("the server never accepted the connection")
	}
	return got, nil
}

// pinDeviceCertificate checks that the peer presented a device
// certificate: it exists, its key is Ed25519, and the device ID it
// claims is the one its key derives.
//
// The device ID is DERIVED from the key (identity.IDFromKey), so this
// check is exact: a certificate whose name does not match its own key
// is an impersonation and is refused.
//
// It is the same shape as the product's pin (internal/mesh/direct.go), and
// it is SHARED with the test below on purpose — a test that asserted
// against its own copy of the rule would prove nothing.
func pinDeviceCertificate(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("the peer presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return errors.New("the peer certificate key is not Ed25519")
	}
	if identity.IDFromKey(pub).String() != leaf.Subject.CommonName {
		return fmt.Errorf("the certificate claims to be %s but its key derives %s",
			leaf.Subject.CommonName, identity.IDFromKey(pub))
	}
	return nil
}

// TestThePinRefusesACertificateWhoseNameIsNotItsKey proves the
// verification in the ALPN test is REAL.
//
// A test that merely turned verification off would prove nothing about
// the handshake. This one builds a certificate whose NAME does not
// match its KEY — the shape an impersonation takes — and runs it
// through the SAME pin the handshake uses. So the handshake cannot be
// said to have skipped the check.
func TestThePinRefusesACertificateWhoseNameIsNotItsKey(t *testing.T) {
	// The real device certificate: its name IS its key's ID.
	cert := makeTestCertificate(t)
	leaf, perr := x509.ParseCertificate(cert.Certificate[0])
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatal("the certificate key is not Ed25519")
	}
	if leaf.Subject.CommonName != identity.IDFromKey(pub).String() {
		t.Fatal("the device certificate's name does not derive from its key; the pin's premise is wrong")
	}

	// A certificate with the SAME KEY but a DIFFERENT name: the
	// impersonation shape.
	other, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	impostor := &x509.Certificate{
		SerialNumber:          leaf.SerialNumber,
		Subject:               pkix.Name{CommonName: other.ID().String()},
		NotBefore:             leaf.NotBefore,
		NotAfter:              leaf.NotAfter,
		KeyUsage:              leaf.KeyUsage,
		ExtKeyUsage:           leaf.ExtKeyUsage,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, impostor, impostor, pub, cert.PrivateKey)
	if err != nil {
		t.Fatalf("build the impostor certificate: %v", err)
	}
	impostorLeaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse the impostor: %v", err)
	}

	// The honest certificate passes.
	if err := pinDeviceCertificate(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err != nil {
		t.Fatalf("the pin refused an honest certificate: %v", err)
	}
	// The impostor does not.
	if err := pinDeviceCertificate(tls.ConnectionState{PeerCertificates: []*x509.Certificate{impostorLeaf}}); err == nil {
		t.Fatal("the pin accepted a certificate whose name is not its key's ID")
	}
}
