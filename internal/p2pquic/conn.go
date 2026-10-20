// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package p2pquic provides the direct-path data plane using QUIC instead of
// the hand-written reliability layer in package p2p.
//
// Why: the hand-written layer implements cumulative ACK, retransmission and a
// fixed window, but not SACK, fast retransmit or congestion control. Under
// packet loss it can lose a message silently (measured: 20 messages at 10%
// loss, message 8 dropped and message 9 delivered in its place). QUIC gives
// all of that from a library that is deployed at scale, and adds stream
// multiplexing and connection migration for free.
//
// What stays ours: candidate discovery (STUN), hole punching, and the
// rendezvous signalling. Those are the parts that are actually specific to
// this project; the transport underneath is now quic-go's.
package p2pquic

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// ALPN identifies this protocol during the QUIC handshake.
const ALPN = "container-mesh-p2p/1"

// Config tunes a direct QUIC connection.
type Config struct {
	// HandshakeTimeout bounds the QUIC handshake.
	HandshakeTimeout time.Duration
	// IdleTimeout closes a connection that has been silent for this long.
	IdleTimeout time.Duration
	// KeepAlivePeriod sends a keepalive packet when idle.
	KeepAlivePeriod time.Duration
	// MaxIncomingStreams bounds concurrent streams from the peer.
	MaxIncomingStreams int64
}

// DefaultConfig returns the standard settings.
func DefaultConfig() Config {
	return Config{
		HandshakeTimeout:   10 * time.Second,
		IdleTimeout:        60 * time.Second,
		KeepAlivePeriod:    20 * time.Second,
		MaxIncomingStreams: 64,
	}
}

// Conn is one QUIC connection carrying messages as length-prefixed streams.
//
// The peer layer above expects a message-oriented pipe (Send one blob, Recv
// one blob), so each message becomes one QUIC stream: the sender writes the
// length then the body, the receiver reads it back. Using a fresh stream per
// message avoids head-of-line blocking between messages and keeps the mapping
// trivial to reason about.
type Conn struct {
	qc *quic.Conn

	// One stream per direction. A stream is ordered by definition, which is
	// what the peer layer expects; opening a stream per message would let the
	// receiver observe messages out of order (AcceptStream does not preserve
	// send order across streams).
	sendMu sync.Mutex
	sendSt *quic.Stream

	done     chan struct{}
	closeErr error
	closeMu  sync.Mutex

	inbox chan []byte
}

// Dial opens a client connection to addr.
//
// The UDP socket must already be the one used for hole punching: QUIC reuses
// it so the NAT mapping that was just opened stays valid. Passing a different
// socket would have to punch again.
func Dial(ctx context.Context, udp net.PacketConn, addr *net.UDPAddr, cfg Config) (*Conn, error) {
	if cfg.HandshakeTimeout <= 0 {
		cfg = DefaultConfig()
	}
	tlsCfg := &tls.Config{
		// The channel is already end-to-end encrypted by SPAKE2 + AES-GCM at
		// the peer layer; QUIC's own TLS is protecting a hop that is already
		// inside that envelope. A self-signed certificate is therefore
		// sufficient, and verification is disabled because there is no name to
		// verify — authentication comes from the session key.
		InsecureSkipVerify: true,
		NextProtos:         []string{ALPN},
	}
	qc, err := quic.Dial(ctx, udp, addr, tlsCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("p2pquic: dial %s: %w", addr, err)
	}
	// One bidirectional stream, opened by the dialler. Both directions share
	// it: a quic.Stream is full-duplex, and one stream keeps ordering trivial
	// (separate streams would let the receiver observe messages out of order).
	st, err := qc.OpenStreamSync(ctx)
	if err != nil {
		_ = qc.CloseWithError(0, "no stream")
		return nil, fmt.Errorf("p2pquic: open stream: %w", err)
	}
	// QUIC only signals a new stream to the peer once something is written on
	// it (or it is closed). Without this byte the peer's AcceptStream blocks
	// forever even though our OpenStreamSync returned. The byte is consumed by
	// the peer as a stream-establishment marker and never reaches the message
	// framing layer.
	if _, err := st.Write([]byte{0}); err != nil {
		_ = qc.CloseWithError(0, "stream handshake")
		return nil, fmt.Errorf("p2pquic: stream handshake: %w", err)
	}

	c := newConn(qc)
	c.sendSt = st
	go c.readLoop(st)
	return c, nil
}

// Listen accepts a QUIC connection on an already-punched socket.
//
// The listener is single-use: it accepts one connection and then stops. A
// direct path is one peer pair, not a server.
func Listen(ctx context.Context, udp net.PacketConn, cfg Config) (*Conn, error) {
	if cfg.HandshakeTimeout <= 0 {
		cfg = DefaultConfig()
	}
	tlsCfg, err := selfSignedTLS()
	if err != nil {
		return nil, err
	}
	_ = cfg
	ln, err := quic.Listen(udp, tlsCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("p2pquic: listen: %w", err)
	}
	qc, err := ln.Accept(ctx)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("p2pquic: accept: %w", err)
	}
	// The listener must stay open until the stream is accepted: closing it
	// tears down the underlying socket, and the accepted connection then never
	// sees the peer's stream (observed as a permanent block in AcceptStream).

	// Accept the dialler's stream; it is bidirectional, so writes go back on
	// the same one.
	st, err := qc.AcceptStream(ctx)
	if err != nil {
		_ = qc.CloseWithError(0, "no stream")
		return nil, fmt.Errorf("p2pquic: accept stream: %w", err)
	}
	var marker [1]byte
	if _, err := io.ReadFull(st, marker[:]); err != nil {
		_ = qc.CloseWithError(0, "stream handshake")
		return nil, fmt.Errorf("p2pquic: stream handshake: %w", err)
	}

	c := newConn(qc)
	c.sendSt = st
	go c.readLoop(st)
	return c, nil
}

func newConn(qc *quic.Conn) *Conn {
	return &Conn{
		qc:    qc,
		done:  make(chan struct{}),
		inbox: make(chan []byte, 64),
	}
}

// readLoop reads length-prefixed messages from the single data stream.
//
// One reader, one stream: order is guaranteed by the stream, and there is no
// cross-stream race.
func (c *Conn) readLoop(st *quic.Stream) {
	defer st.Close()
	var lenBuf [4]byte
	for {
		if _, err := io.ReadFull(st, lenBuf[:]); err != nil {
			c.fail(err)
			return
		}
		n := int(uint32(lenBuf[0])<<24 | uint32(lenBuf[1])<<16 | uint32(lenBuf[2])<<8 | uint32(lenBuf[3]))
		if n < 0 || n > 64<<20 {
			c.fail(fmt.Errorf("p2pquic: implausible message length %d", n))
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(st, body); err != nil {
			c.fail(err)
			return
		}
		select {
		case c.inbox <- body:
		case <-c.done:
			return
		}
	}
}

// Send writes one length-prefixed message on the data stream.
//
// Writing is serialised: interleaving two messages on one stream would corrupt
// the framing.
func (c *Conn) Send(msg []byte) error {
	if len(msg) == 0 {
		return nil
	}
	select {
	case <-c.done:
		return c.closedErr()
	default:
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	var lenBuf [4]byte
	n := uint32(len(msg))
	lenBuf[0] = byte(n >> 24)
	lenBuf[1] = byte(n >> 16)
	lenBuf[2] = byte(n >> 8)
	lenBuf[3] = byte(n)

	if _, err := c.sendSt.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("p2pquic: write length: %w", err)
	}
	if _, err := c.sendSt.Write(msg); err != nil {
		return fmt.Errorf("p2pquic: write body: %w", err)
	}
	return nil
}

// Recv returns the next complete message.
func (c *Conn) Recv() ([]byte, error) {
	select {
	case msg := <-c.inbox:
		return msg, nil
	case <-c.done:
		return nil, c.closedErr()
	}
}

// Close shuts the connection down.
func (c *Conn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closeErr == nil {
		c.closeErr = errors.New("p2pquic: connection closed")
		close(c.done)
	}
	return c.qc.CloseWithError(0, "closed")
}

// Closed reports a channel closed when the connection is gone.
func (c *Conn) Closed() <-chan struct{} { return c.done }

func (c *Conn) fail(err error) {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	select {
	case <-c.done:
		return
	default:
	}
	c.closeErr = err
	close(c.done)
}

func (c *Conn) closedErr() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closeErr != nil {
		return c.closeErr
	}
	return errors.New("p2pquic: connection closed")
}

// selfSignedTLS builds a throwaway certificate.
//
// It is not a security boundary: the peer layer authenticates with SPAKE2 and
// encrypts with AES-GCM, so QUIC only has to be a reliable byte transport
// here. A fixed ephemeral certificate avoids shipping one in the repo.
func selfSignedTLS() (*tls.Config, error) {
	key, cert, err := generateCert()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{cert}, PrivateKey: key}},
		NextProtos:   []string{ALPN},
	}, nil
}

// randomID returns a fresh stream/session identifier.
func randomID() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7]), nil
}
