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
	"github.com/muzimu217/container-mesh/internal/wsconn"
)

// TestClientPassesEgressPolicyToTheDialer pins the wiring, not just the
// policy function.
//
// The gap kuku found was not that the policy was wrong but that it was
// never consulted: the discovery client dialled with a bare net.Dialer, so
// a host name resolving to a loopback or private address connected
// anyway. A test on the policy function alone would have stayed green
// through that whole bug.
//
// This test captures the options the client hands to the dialer and
// requires the policy to be present and to refuse a loopback address.
func TestClientPassesEgressPolicyToTheDialer(t *testing.T) {
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

	var seen wsconn.DialOptions
	got := make(chan struct{}, 1)

	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      []string{"mesh.example:18080"},
		Device:     dev,
		AccountID:  acc.ID(),
		AccountPub: []byte(acc.PublicKey()),
		NoAnnounce: true,
		WebSocketDial: func(ctx context.Context, opts wsconn.DialOptions) (*wsconn.Conn, error) {
			seen = opts
			select {
			case got <- struct{}{}:
			default:
			}
			return nil, context.Canceled
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the client never attempted a members stream")
	}

	if seen.EgressPolicy == nil {
		t.Fatal("the client dialled without an egress policy: a host name " +
			"resolving to a loopback or private address would connect")
	}
	// The policy the client carries must actually refuse loopback.
	err = seen.EgressPolicy(&literalAddr{addr: "127.0.0.1:18080"})
	if err == nil {
		t.Fatal("the policy the client carries allowed a loopback address")
	}
	// And it must still allow a public address, or it would break every
	// real deployment.
	if err := seen.EgressPolicy(&literalAddr{addr: "93.184.216.34:18080"}); err != nil {
		t.Fatalf("the policy refused a public address: %v", err)
	}
}

type literalAddr struct{ addr string }

func (a *literalAddr) Network() string { return "tcp" }
func (a *literalAddr) String() string  { return a.addr }

var _ net.Addr = (*literalAddr)(nil)
