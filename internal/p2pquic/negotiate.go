// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2pquic

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Result is the outcome of bringing up a direct QUIC path.
type Result struct {
	// Conn is the QUIC transport, nil when the attempt failed.
	Conn *Conn
	// Remote is the address that answered.
	Remote *net.UDPAddr
	// Reason explains a failure.
	Reason string
}

// Describe renders a one-line summary.
func (r *Result) Describe() string {
	if r == nil {
		return "p2p: not attempted"
	}
	if r.Conn == nil {
		return fmt.Sprintf("p2p: direct path unavailable (%s); using relay", r.Reason)
	}
	return fmt.Sprintf("p2p: direct QUIC path to %s", r.Remote)
}

// DialWithSocket opens a QUIC connection on an already-punched socket.
//
// The socket is the one hole punching just used, so its NAT mapping is open;
// opening a fresh socket would have to punch again.
func DialWithSocket(ctx context.Context, udp net.PacketConn, remote *net.UDPAddr, timeout time.Duration) (*Result, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c, err := Dial(cctx, udp, remote, DefaultConfig())
	if err != nil {
		return &Result{Reason: err.Error()}, nil
	}
	return &Result{Conn: c, Remote: remote}, nil
}

// ListenOnSocket accepts a QUIC connection on an already-punched socket.
//
// remote is the peer address learned from hole punching; it is only used for
// the diagnostic line, since QUIC has already established its own path.
func ListenOnSocket(ctx context.Context, udp net.PacketConn, remote *net.UDPAddr, timeout time.Duration) (*Result, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c, err := Listen(cctx, udp, DefaultConfig())
	if err != nil {
		return &Result{Reason: err.Error()}, nil
	}
	return &Result{Conn: c, Remote: remote}, nil
}
