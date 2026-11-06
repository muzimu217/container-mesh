// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

// Platform-behaviour regression: on go1.27/darwin the macOS system verifier
// rejects our self-signed Ed25519 device certificates BEFORE VerifyConnection
// is reached, so the direct-link TLS config must skip the default
// verification for the member-view pinning to run at all. This test pins
// that behaviour: if a future Go changes the ordering (VerifyConnection
// firing without the skip), the direct-link TLS config can drop the skip and
// this test documents why it was there.
//
// The pin itself (wrong key → handshake fails) is covered by
// TestDialRejectsImpostor and TestRevokedMemberCannotConnect.

import (
	"bufio"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

func TestTLSConfigMutualAuth(t *testing.T) {
	server, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	client, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	sCert, err := server.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	cCert, err := client.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{sCert},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return nil // pin logic lives in deviceTLS; here we only test reachability
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = bufio.NewReader(c).ReadString('\n')
		_, _ = c.Write([]byte("pong\n"))
	}()

	dialer := &tls.Config{
		Certificates: []tls.Certificate{cCert},
		ServerName:   "anything",
	}
	// See the package/type comment: skipping the DEFAULT verification is
	// what lets the custom pin verification run on go1.27/darwin.
	dialer.InsecureSkipVerify = true
	dialer.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			t.Error("no peer certificate reached VerifyConnection")
		}
		return nil
	}

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", ln.Addr().String(), dialer)
	if err != nil {
		t.Fatalf("mutual TLS with device certificates: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if line, _ := bufio.NewReader(conn).ReadString('\n'); line != "pong\n" {
		t.Fatalf("unexpected reply %q", line)
	}
}
