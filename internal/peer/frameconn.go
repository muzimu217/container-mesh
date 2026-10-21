// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"bufio"
	"io"
	"sync"

	"github.com/muzimu217/container-mesh/internal/wire"
)

// FrameConn is the frame source peer.Session talks to. Two implementations
// exist and the session never learns which one it has:
//
//	*Pump           bootstrap: transport messages + AEAD
//	streamFrameConn muxed:     wire frames on a yamux control stream
//
// That is what keeps the file-transfer code (offers, resume, hash
// verification, duplex) untouched by the mux work.
type FrameConn interface {
	SendFrame(*wire.Frame) error
	RecvFrame() (*wire.Frame, error)
	Close() error
}

// streamFrameConn carries wire frames on a byte stream.
//
// There is no extra length prefix: wire.Frame already encodes its own payload
// length, so wire.DecodeFrame(io.Reader) reads exactly one frame. Adding a
// second prefix would be pure overhead.
type streamFrameConn struct {
	rw     io.ReadWriteCloser
	rd     *bufio.Reader // wire.DecodeFrame does two Reads per frame
	sendMu sync.Mutex    // one frame must not interleave with another
}

func newStreamFrameConn(rw io.ReadWriteCloser) *streamFrameConn {
	return &streamFrameConn{rw: rw, rd: bufio.NewReaderSize(rw, 64<<10)}
}

// SendFrame writes one frame. The lock covers the whole frame: two concurrent
// senders interleaving their bytes would desynchronise the receiver's frame
// boundary for the rest of the session.
func (c *streamFrameConn) SendFrame(f *wire.Frame) error {
	b, err := f.Encode(nil)
	if err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return writeFull(c.rw, b)
}

// RecvFrame decodes exactly one frame.
func (c *streamFrameConn) RecvFrame() (*wire.Frame, error) {
	return wire.DecodeFrame(c.rd)
}

// Close closes the underlying stream.
func (c *streamFrameConn) Close() error { return c.rw.Close() }

// writeFull writes all of b, retrying short writes.
//
// io.Writer is allowed to return n < len(b) with a nil error. Treating that as
// success would silently truncate a frame, and every subsequent frame on the
// stream would be misparsed.
func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if n > 0 {
			b = b[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			// A writer that neither progresses nor errors would spin forever.
			return io.ErrShortWrite
		}
	}
	return nil
}
