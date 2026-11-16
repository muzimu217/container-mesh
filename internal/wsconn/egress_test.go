// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package wsconn

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestDialAppliesEgressPolicyAfterResolution is the end-to-end proof of the
// gap kuku reported: a host name that resolves to a loopback address must
// not connect.
//
// The dial-time check has to run after DNS resolution and before the socket
// is connected, which is what net.Dialer.Control is for. Without the policy
// the connection succeeds, because the host check upstream only ever sees
// the name.
func TestDialAppliesEgressPolicyAfterResolution(t *testing.T) {
	// A real listener on loopback: if the policy is missing, this connects.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	// "localhost" resolves to loopback. The policy must refuse it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, DialOptions{
		URL:         "ws://localhost:" + itoa(port) + "/ws",
		DialTimeout: 2 * time.Second,
		EgressPolicy: func(addr net.Addr) error {
			host, _, err := net.SplitHostPort(addr.String())
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return nil
			}
			return &egressError{addr: addr.String()}
		},
	})
	if err == nil {
		conn.Close()
		t.Fatal("dial to localhost succeeded: the egress policy was not applied")
	}
	var ee *egressError
	if !asEgressError(err, &ee) {
		t.Fatalf("dial failed with %v, want an egress-policy rejection", err)
	}
	select {
	case <-accepted:
		t.Fatal("the listener accepted a connection: the policy was checked after connecting")
	default:
	}
}

type egressError struct{ addr string }

func (e *egressError) Error() string { return "egress policy refused " + e.addr }

func asEgressError(err error, target **egressError) bool {
	for err != nil {
		if ee, ok := err.(*egressError); ok {
			*target = ee
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
