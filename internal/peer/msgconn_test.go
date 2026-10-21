// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer_test

import (
	"io"
	"sync"
)

// newMsgConnPair returns two connected in-memory MessageConns that also expose
// their byte streams.
//
// The session needs both faces of the same wire: Handshake and the bootstrap
// frames speak messages ("one Send is one message"), while the record pump
// after the barrier speaks an ordered byte stream. A real transport is exactly
// this — a WebSocket message connection whose messages are byte sequences — so
// the test harness models it the same way.
func newMsgConnPair() (a, b *msgConn) {
	ab := make(chan []byte, 128)
	ba := make(chan []byte, 128)
	shared := &pipeCloser{closed: make(chan struct{})}
	return &msgConn{out: ab, in: ba, shared: shared},
		&msgConn{out: ba, in: ab, shared: shared}
}

// msgConn is one endpoint of a crossed pair of message channels.
type msgConn struct {
	out     chan []byte
	in      chan []byte
	shared  *pipeCloser
	pending []byte // remainder of a message partially consumed by Read
}

// Send writes one complete message.
func (c *msgConn) Send(b []byte) error {
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case c.out <- cp:
		return nil
	case <-c.shared.closed:
		return io.ErrClosedPipe
	}
}

// Recv reads one complete message.
func (c *msgConn) Recv() ([]byte, error) {
	select {
	case msg, ok := <-c.in:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	case <-c.shared.closed:
		return nil, io.EOF
	}
}

// Read reads bytes, spilling across message boundaries. The record pump's link
// is a byte stream even though the transport underneath is message-oriented,
// so this is what the pump's own MessageConn wrapper effectively provides.
func (c *msgConn) Read(b []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(b, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	msg, err := c.Recv()
	if err != nil {
		return 0, err
	}
	n := copy(b, msg)
	if n < len(msg) {
		c.pending = msg[n:]
	}
	return n, nil
}

// Write writes bytes as one message. Each Write becomes one transport message,
// which is how the pump's sealed records travel.
func (c *msgConn) Write(b []byte) (int, error) {
	if err := c.Send(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *msgConn) Close() error {
	c.shared.Close()
	return nil
}

// pipeCloser lets both endpoints share one close signal.
type pipeCloser struct {
	once   sync.Once
	closed chan struct{}
}

func (c *pipeCloser) Close() {
	c.once.Do(func() { close(c.closed) })
}
