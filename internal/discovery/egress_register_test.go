// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestRegisterRefusesPrivateResolvedAddress pins the egress policy on the
// register path, not just the members stream.
//
// The first half of the fix covered the WebSocket members stream. The
// register request is the other half: it is an HTTP POST to the discovery
// origin, and it went out through a plain http.Client with no policy at
// all. A host name resolving to a loopback or private address therefore
// still reached the register endpoint, which is the same SSRF shape the
// members stream was closed against.
//
// The external tester confirmed it: with a listener on 127.0.0.1 and a
// discovery origin of 127.0.0.1.nip.io, the register request arrived
// (the listener answered 501 Unsupported method) even though the
// members stream was correctly refused.
func TestRegisterRefusesPrivateResolvedAddress(t *testing.T) {
	// A raw listener that records whether anything reached it, and answers
	// with a plain HTTP response. An httptest.Server would refuse a request
	// it does not recognise before the test could see the connection, which
	// would make the test pass for the wrong reason.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	hit := make(chan struct{}, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case hit <- struct{}{}:
			default:
			}
			// Read enough to answer, then close.
			buf := make([]byte, 1024)
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte("HTTP/1.1 501 Not Implemented\r\nContent-Length: 0\r\n\r\n"))
			_ = conn.Close()
		}
	}()

	// The origin the client will use: a logical name that resolves to
	// loopback.
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	origin := "cmesh-register-egress-test." + host + ".nip.io:" + port

	// The register request must go straight to the listener, not through a
	// local egress proxy: a proxy would answer for the origin, and the test
	// would never see the connection reach loopback.
	t.Setenv("NO_PROXY", origin)
	t.Setenv("no_proxy", origin)

	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}

	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{origin},
		Device:     dev,
		AccountID:  acc.ID(),
		AccountPub: []byte(acc.PublicKey()),
		NoAnnounce: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	select {
	case <-hit:
		t.Fatal("the register request reached a loopback address: the egress " +
			"policy is not applied on the HTTP path")
	case <-time.After(4 * time.Second):
		// Nothing arrived, which is the expected outcome.
	}
}
