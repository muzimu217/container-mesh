// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/muzimu217/container-mesh/internal/secure"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// Pump owns the encrypted frame channel and is the only component that touches
// secure.Session. It has exactly two modes:
//
//	bootstrap: SendFrame/RecvFrame are the only entry points, and they are
//	  byte-for-byte what peer.Session did before the mux existed. hello and the
//	  p2p signalling run here.
//
//	muxed: StartMux has completed; one goroutine reads the transport and one
//	  writes it, and the pair presents a single ordered byte stream to yamux.
//
// The invariant that makes the whole design work: exactly one goroutine ever
// calls secure.OpenFrame, and exactly one ever calls secure.SealFrame. That is
// what the strictly increasing counter in secure requires, and it is why
// multiplexing cannot sit below secure: yamux's recvLoop dispatches by stream id
// before the application reads, so cross-stream arrival order would be lost and
// the global counter would decrypt out of order.
type Pump struct {
	conn MessageConn // the *switchable, so a p2p upgrade still works
	sec  *secure.Session
	cfg  PumpConfig
	log  *slog.Logger

	// bootMu serialises bootstrap sealing. After StartMux it is unused: the
	// writer goroutine is then the only seal path.
	bootMu sync.Mutex

	st   atomic.Int32 // pumpBootstrap -> pumpMuxed -> pumpClosed
	link *pumpLink

	readQ  *byteQueue // inbound records, bounded by cfg.BufBytes
	writeQ *byteQueue // outbound records, bounded by cfg.BufBytes

	closeOnce sync.Once
	closeErr  error
	done      chan struct{}

	errMu sync.Mutex
	err   error
}

const (
	pumpBootstrap int32 = iota
	pumpMuxed
	pumpClosed
)

// PumpConfig tunes the record pump.
type PumpConfig struct {
	// RecordSize is the largest FrameStreamData payload.
	//
	// 32 KiB, not 512 KiB. The 512 KiB figure came from loopback throughput
	// (D11: 539.3 vs 211.9 MiB/s) and is the wrong optimisation target here:
	// AEAD is not the bottleneck on any real link (measured: 1408 MB/s at
	// 32 KiB), while a large record is a latency floor for every other stream
	// sharing the connection. A 512 KiB record takes 250 ms to cross a 2 MiB/s
	// link, and nothing can be decrypted or dispatched until the whole record
	// arrives, so an interactive stream waits behind it. At 32 KiB that floor is
	// 15.6 ms.
	//
	// The write side sends one record per transport message (see writeLoop for
	// why batching was rejected). That costs more syscalls than a 512 KiB
	// record, and buys the 16x smaller floor — the trade the design wants.
	RecordSize int

	// BufBytes bounds each direction's queue. 1 MiB is 32 records at the
	// default size: enough to keep the transport busy across one AEAD plus one
	// Send, small enough that a dead peer cannot pin more than 2 MiB.
	BufBytes int

	// Logger receives debug events. Nil means discard.
	Logger *slog.Logger
}

// DefaultPumpConfig returns the production defaults described above.
func DefaultPumpConfig() PumpConfig {
	return PumpConfig{
		RecordSize: wire.MaxStreamData,
		BufBytes:   1 << 20,
	}
}

// NewPump wraps conn+sec in a record pump. The pump starts in bootstrap mode.
func NewPump(conn MessageConn, sec *secure.Session, cfg PumpConfig) *Pump {
	if cfg.RecordSize <= 0 || cfg.RecordSize > wire.MaxStreamData {
		cfg.RecordSize = wire.MaxStreamData
	}
	if cfg.BufBytes <= 0 {
		cfg.BufBytes = 1 << 20
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Pump{
		conn:   conn,
		sec:    sec,
		cfg:    cfg,
		log:    log,
		readQ:  newByteQueue(cfg.BufBytes),
		writeQ: newByteQueue(cfg.BufBytes),
		done:   make(chan struct{}),
	}
}

// ---- bootstrap mode -------------------------------------------------------

// SendFrame seals and sends one frame. Safe for concurrent use, though during
// bootstrap only one goroutine (hello, then negotiation) is ever writing.
//
// The seal and the send must happen under the same lock. The receiver rejects
// any frame whose counter is not exactly the next expected value, so if two
// goroutines sealed concurrently and then raced to the socket, the higher
// counter could hit the wire first and the session would fail with a decrypt
// error.
func (p *Pump) SendFrame(f *wire.Frame) error {
	if p.st.Load() != pumpBootstrap {
		return ErrPumpMuxed
	}
	p.bootMu.Lock()
	defer p.bootMu.Unlock()
	b, err := p.sec.SealFrame(f)
	if err != nil {
		return err
	}
	return p.conn.Send(b)
}

// RecvFrame reads and opens one frame. Only the caller that owns the read side
// may call it: exchangeHello, then p2p negotiation, then nobody.
func (p *Pump) RecvFrame() (*wire.Frame, error) {
	if p.st.Load() != pumpBootstrap {
		return nil, ErrPumpMuxed
	}
	b, err := p.conn.Recv()
	if err != nil {
		return nil, err
	}
	return p.sec.OpenFrame(b)
}

// ErrPumpMuxed is returned when a bootstrap-only method is called after the
// mux barrier. It is a programming error, not a peer fault.
var ErrPumpMuxed = errors.New("peer: pump is in mux mode")

// ErrPumpBootstrap is returned by StartMux when the pump never left bootstrap
// mode (for example because the peer does not support multiplexing).
var ErrPumpBootstrap = errors.New("peer: pump is still in bootstrap mode")

// ---- barrier --------------------------------------------------------------

// StartMux performs the FrameMuxStart barrier and hands back the byte stream
// yamux runs on.
//
// Sequence (both peers run it symmetrically, right after negotiation):
//
//  1. seal and send FrameMuxStart
//  2. read frames until the peer's FrameMuxStart arrives; anything else is
//     logged and dropped (it can only be a stray pong: the read pump has not
//     started and the application cannot be writing yet)
//  3. start the reader goroutine (transport.Recv + OpenFrame) and the writer
//     goroutine (SealFrame + transport.Send)
//  4. return the link
//
// Steps 1-2 run on the caller's goroutine, so the "one reader" invariant holds
// across the hand-off: before StartMux the caller reads, after it the pump
// goroutine reads, and there is no moment when two readers exist.
func (p *Pump) StartMux(ctx context.Context) (io.ReadWriteCloser, error) {
	if p.st.Load() != pumpBootstrap {
		return nil, ErrPumpMuxed
	}

	// Step 1: announce the barrier.
	if err := p.SendFrame(&wire.Frame{Type: wire.FrameMuxStart}); err != nil {
		return nil, err
	}

	// Step 2: wait for the peer's barrier. Drop anything else: the only frames
	// that can legitimately arrive here are stragglers from before the peer hit
	// its own barrier, and they carry no application data.
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := p.RecvFrame()
		if err != nil {
			return nil, err
		}
		if f.Type == wire.FrameMuxStart {
			break
		}
		p.log.Debug("dropping frame while waiting for mux barrier", "type", f.Type)
	}

	// Step 3: take ownership of the channel. The CAS makes the hand-off
	// explicit: SendFrame/RecvFrame refuse to run afterwards, so a caller that
	// forgot to stop reading fails loudly instead of racing the pump.
	if !p.st.CompareAndSwap(pumpBootstrap, pumpMuxed) {
		return nil, ErrPumpMuxed
	}
	link := &pumpLink{p: p}
	p.link = link
	p.startLoops()
	return link, nil
}

// startLoops starts the reader and writer goroutines. Split out from StartMux
// so tests can drive the pump without a peer to answer the barrier.
func (p *Pump) startLoops() {
	go p.readLoop()
	go p.writeLoop()
}

// Muxed reports whether the pump has passed the barrier.
func (p *Pump) Muxed() bool { return p.st.Load() == pumpMuxed }

// ---- reader / writer ------------------------------------------------------

// readLoop is the only goroutine that touches transport.Recv and OpenFrame.
func (p *Pump) readLoop() {
	for {
		msg, err := p.conn.Recv()
		if errors.Is(err, ErrTransportSwitched) {
			// A direct path replaced the relay underneath us. The session key
			// and the counters are unchanged, so keep going.
			//
			// In mux mode this can only happen if the switch raced the barrier:
			// Session.UpgradeToDirect refuses once the pump is muxed, so the
			// window is the few instructions of StartMux itself. Continuing is
			// still wrong here (a half-read record would be lost, see
			// TestPartialRecordAcrossSwitchIsLost), so the pump fails and the
			// session is rebuilt by Node.Run.
			p.fail(fmt.Errorf("peer: transport switched after the mux barrier; "+
				"the path is locked for the life of a muxed session: %w", ErrTransportSwitched))
			return
		}
		if err != nil {
			p.fail(err)
			return
		}

		f, err := p.sec.OpenFrame(msg) // single reader => counters stay in order
		if err != nil {
			p.fail(err)
			return
		}

		switch f.Type {
		case wire.FrameStreamData:
			if err := p.readQ.Put(f.Payload); err != nil {
				p.fail(err)
				return
			}
		default:
			// Nothing legitimate travels outside FrameStreamData after the
			// barrier. Dropping beats killing the session: a ping that was in
			// flight when the peer hit the barrier is harmless.
			p.log.Debug("dropping non-stream frame in mux mode", "type", f.Type)
		}
	}
}

// writeLoop is the only goroutine that touches SealFrame and transport.Send.
//
// One record, one AEAD, one transport message — no batching.
//
// Batching was designed in and then removed after measurement-free reasoning
// turned out to be wrong: concatenating N records into one transport message
// means the receiver cannot decrypt any of them until all N have arrived. At
// 16 x 32 KiB that is a 512 KiB unit on the wire, i.e. exactly the 250 ms
// head-of-line floor on a 2 MiB/s link that shrinking the record was meant to
// eliminate. It would also trigger precisely when the link is busy, which is
// when an interactive stream needs the low floor most.
//
// The syscall cost of per-record sends is the price of that floor, and it is
// affordable: a 32 KiB record amortises one write over 32 KiB, and the
// measured AEAD cost at this size (1408 MB/s) leaves the transport write as the
// only per-record cost. If syscall count ever becomes the bottleneck, the fix
// belongs in the transport (sendmmsg-style), not in a larger encryption unit.
func (p *Pump) writeLoop() {
	record := make([]byte, p.cfg.RecordSize)
	for {
		n, err := p.writeQ.Get(record)
		if err != nil {
			p.fail(err)
			return
		}
		sealed, err := p.sec.SealFrame(&wire.Frame{Type: wire.FrameStreamData, Payload: record[:n]})
		if err != nil {
			// The counter has already advanced for any earlier frame. A seal
			// failure here means the session is unusable, so fail rather than
			// retry: retrying would leave a permanent counter gap.
			p.fail(err)
			return
		}
		if err := p.conn.Send(sealed); err != nil {
			p.fail(err)
			return
		}
	}
}

// ---- lifecycle / accessors ------------------------------------------------

// fail records err (first one wins), closes both queues and the link, and
// unblocks every waiter.
//
// Closing both queues matters: closing only one leaves yamux's other loop
// parked forever, which stalls the whole session instead of failing it.
func (p *Pump) fail(err error) {
	p.errMu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.errMu.Unlock()

	p.st.Store(pumpClosed)
	p.readQ.CloseWithError(err)
	p.writeQ.CloseWithError(err)
	_ = p.Close()
}

// Close shuts the pump down. Closing the link also closes yamux (its recvLoop
// and sendLoop both error out), which fails every stream.
func (p *Pump) Close() error {
	p.closeOnce.Do(func() {
		p.st.Store(pumpClosed)
		p.readQ.Close()
		p.writeQ.Close()
		close(p.done)
		p.closeErr = p.conn.Close()
	})
	return p.closeErr
}

// Done is closed when the pump has stopped.
func (p *Pump) Done() <-chan struct{} { return p.done }

// Err returns the error that stopped the pump, or nil.
func (p *Pump) Err() error {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	return p.err
}

// ShortAuth is delegated so peer.Session and cli.Node keep their API.
func (p *Pump) ShortAuth() string { return p.sec.ShortAuth() }

// Fingerprint is delegated so peer.Session and cli.Node keep their API.
func (p *Pump) Fingerprint() string { return p.sec.Fingerprint() }

// RecvCounter exposes how many frames were opened (for tests and metrics).
func (p *Pump) RecvCounter() uint64 { return p.sec.RecvCounter() }

// ---- pumpLink -------------------------------------------------------------

// pumpLink is what yamux sees: an ordered, reliable, closeable byte stream.
type pumpLink struct {
	p      *Pump
	once   sync.Once
	closed chan struct{}
}

func (l *pumpLink) init() {
	l.once.Do(func() { l.closed = make(chan struct{}) })
}

// Read drains the inbound record queue.
func (l *pumpLink) Read(b []byte) (int, error) {
	return l.p.readQ.Read(b)
}

// Write appends to the outbound record queue, blocking when it is full. That
// is correct backpressure: a slow transport slows yamux's sendLoop, which
// slows each stream's Write, which slows the application's write().
func (l *pumpLink) Write(b []byte) (int, error) {
	return l.p.writeQ.Write(b)
}

// Close shuts the pump down, which fails every yamux stream.
func (l *pumpLink) Close() error { return l.p.Close() }
