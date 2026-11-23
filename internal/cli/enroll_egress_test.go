// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestEnrollRefusesPrivateResolvedAddress pins the outbound address policy
// on the credential enrollment path.
//
// Enrolling is the third request a client makes to the discovery origin,
// after the register POST and the members stream. It used a bare
// http.Client, so a discovery address naming a host that resolves to a
// loopback or private address reached it.
//
// The discovery origin comes from the credential, which the operator
// hands to the joiner, so it is the same operator-supplied value the
// other two paths are held to.
func TestEnrollRefusesPrivateResolvedAddress(t *testing.T) {
	// A raw listener, so the test observes the arrival itself. An
	// httptest server would refuse a request it does not recognise before
	// the test could see the connection, which passes for the wrong reason.
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
			buf := make([]byte, 1024)
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte("HTTP/1.1 501 Not Implemented\r\nContent-Length: 0\r\n\r\n"))
			_ = conn.Close()
		}
	}()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// A logical name that resolves to loopback.
	origin := "cmesh-enroll-egress-test." + host + ".nip.io:" + port

	// The request must go straight to the listener: a local proxy would
	// answer for the origin and the test would never see the connection.
	t.Setenv("NO_PROXY", origin)
	t.Setenv("no_proxy", origin)

	// The private-deployment switch must be OFF, whatever the
	// environment says.
	//
	// This test pins the EGRESS POLICY, and the switch turns that policy
	// off on purpose (it is how a LAN deployment addresses a private
	// anchor). So the switch being on in the environment means "the
	// policy is off" — which is the opposite of what this test asserts.
	//
	// It is not a hypothetical: an external tester runs his whole suite
	// with CMESH_ALLOW_PRIVATE_DISCOVERY=1 (his loopback anchor needs
	// it), and this test failed for him with "the enrollment request
	// reached a loopback address" — a failure that says nothing about
	// the product and everything about the test inheriting the
	// environment.
	//
	// A test that asserts a policy must also PIN that policy on, or it
	// measures the environment instead of the code.
	t.Setenv("CMESH_ALLOW_PRIVATE_DISCOVERY", "")

	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	// A real credential pointing at the loopback-resolving name.
	cred, err := credential.Create(acc, []string{origin}, time.Hour, 2, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_ = enrollWithCredential(ctx, cred, dev)

	select {
	case <-hit:
		t.Fatal("the enrollment request reached a loopback address: the egress " +
			"policy is not applied on the enroll path")
	case <-time.After(2 * time.Second):
		// Nothing arrived, which is the expected outcome.
	}
}
