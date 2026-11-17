// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"io"
	"time"

	"github.com/quic-go/quic-go"
)

// Session is a transport-independent byte stream: one session carries one
// business exchange, in both directions.
//
// Why an interface at all: the business layer (messages, files, run verbs,
// benchmarks) must not know whether its bytes travel over a direct QUIC
// link or through a relay tunnel. With a Session in hand, the transport is
// a detail the business code never names.
//
// Why not io.ReadWriteCloser: because Close() means DIFFERENT things on
// the two sides of that interface.
//
//   - quic.Stream.Close() closes the SEND side only — it writes a FIN.
//     The receive side stays open. The message protocol depends on this
//     exact semantic: the sender writes its frames, half-closes to mark
//     "no more messages", and the receiver only answers "done" after it
//     has consumed everything. A full close would tear the receive side
//     down too and race the receiver's reads (the tail of a message
//     would be lost).
//   - io.ReadWriteCloser's Close() closes BOTH sides.
//
// So the interface declares Close() explicitly and documents it as a HALF
// close (FIN on the send side). Every implementation must honour that.
//
// The deadline methods are here because the business layer uses rolling
// deadlines to notice a dead peer mid-transfer instead of blocking on one
// giant write.
type Session interface {
	io.Reader
	io.Writer
	// Close ends the SEND side (a FIN). The receive side stays open.
	Close() error
	// SetReadDeadline bounds the pending and future reads.
	SetReadDeadline(t time.Time) error
	// SetWriteDeadline bounds the pending and future writes.
	SetWriteDeadline(t time.Time) error
}

// quicSession adapts a *quic.Stream to Session.
//
// The adapter is thin today (quic.Stream already has the right method
// set), but it exists so the half-close contract of Session is stated in
// ONE place, and so a future transport can be adapted without touching
// the business layer.
type quicSession struct {
	st *quic.Stream
	// conn is the QUIC connection the stream belongs to. It is nil for a
	// synthetic stream (tests). It is carried so that a session handler can
	// reach the connection-level QUIC accounting — see Loss() below.
	conn *quic.Conn
}

func (s quicSession) Read(p []byte) (int, error)  { return s.st.Read(p) }
func (s quicSession) Write(p []byte) (int, error) { return s.st.Write(p) }

// Close half-closes: quic.Stream.Close() writes a FIN on the send side
// and leaves the receive side open.
func (s quicSession) Close() error { return s.st.Close() }

func (s quicSession) SetReadDeadline(t time.Time) error  { return s.st.SetReadDeadline(t) }
func (s quicSession) SetWriteDeadline(t time.Time) error { return s.st.SetWriteDeadline(t) }

// Loss reports the QUIC-level loss accounting of the connection this session
// runs on, or nil when the session has no connection behind it.
//
// This is NOT part of the Session interface on purpose: a Session is a
// byte stream, and loss accounting is a transport concern. It lives on an
// OPTIONAL interface (see lossReporter in bench.go) so a relayed session —
// which has no QUIC connection of its own — can simply not implement it.
//
// The number is per-CONNECTION, not per-STREAM: quic-go exposes no
// per-stream loss counter. A bench session carries exactly one stream on its
// own connection, so the two coincide there.
func (s quicSession) Loss() *BenchLoss {
	if s.conn == nil {
		return nil
	}
	return lossOf(s.conn)
}
