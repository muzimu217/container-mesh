// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package p2p2 is a message-oriented reliable transport over UDP.
//
// It replaces package p2p, which failed under packet loss: that layer keyed its
// reassembly map by fragment index and declared a message complete when
// len(map) == total, so fragments of message N+1 could fill the holes of
// message N and the mixture was delivered as message N. This package makes that
// structurally impossible:
//
//   - every datagram carries msgID + fragIndex + fragCount, and msgID is
//     assigned once per message and copied verbatim on retransmission;
//   - reassembly is a map keyed by msgID, never by fragment index;
//   - delivery is strictly in msgID order; a complete message above a hole
//     waits, it is never delivered early;
//   - a msgID below the delivery point is discarded as a duplicate;
//   - Send blocks until every fragment is acknowledged, and a fragment that
//     exhausts its retry budget fails the connection instead of being dropped.
//
// The reliability core follows KCP (cumulative una + selective ACK + fast
// retransmit + RFC 6298 RTO), the message header follows ENet's fragment
// header, loss detection follows RFC 9002's packet threshold, and the send
// window is driven by a simplified Reno controller (see cc.go). Multiplexing,
// connection migration and FEC are out of scope: the peer layer paces itself
// and the direct path carries one byte pipe.
//
// The layer above (peer.Session) decrypts with a monotonically increasing
// counter, so out-of-order delivery there is a hard protocol error, not a
// latency trade-off. That is why ordering is strict rather than best effort.
package p2p2

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Config tunes a Conn. The zero value is not usable; start from
// DefaultConfig.
type Config struct {
	// SendWindow is the maximum number of unacknowledged datagrams. It bounds
	// both memory and the burst size; without a bound, a 512 KiB message would
	// push 450 datagrams into the socket at once and overflow the receive
	// buffer, which is indistinguishable from loss.
	//
	// Since congestion control exists this is the *ceiling*, not the operating
	// window: the congestion window starts at 10 and grows towards this value as
	// the path acknowledges data. Set DisableCongestionControl to get the old
	// fixed-window behaviour back.
	SendWindow int
	// DisableCongestionControl turns off the Reno controller, restoring the fixed
	// SendWindow as the operating window. It exists for A/B measurement and as an
	// escape hatch: on a path with no congestion to speak of (a private link, a
	// loopback benchmark) the controller can only cost throughput, because there
	// is nothing for it to adapt to. Default is enabled.
	DisableCongestionControl bool
	// RecvWindow bounds how many incomplete messages may be tracked. A msgID
	// this far ahead of the delivery point is dropped, which keeps the
	// reassembly table bounded no matter what the peer sends.
	RecvWindow int
	// MaxReasmBytes is a second, independent bound on reassembly memory: a
	// window's worth of maximum-size messages would otherwise be gigabytes.
	MaxReasmBytes int
	// InboxSize bounds the queue of delivered messages.
	InboxSize int

	// InitialRTO seeds the retransmit timer before the first RTT sample.
	InitialRTO time.Duration
	// MinRTO is the RTO floor. It is small on purpose: srtt is measured per
	// packet here, so a large floor makes every loss cost that floor rather than
	// the path delay. The effective floor is never below four RTTick intervals,
	// which is what keeps a sub-tick value from expiring before the scan that is
	// meant to notice it.
	MinRTO time.Duration
	// MaxRTO caps exponential backoff.
	MaxRTO time.Duration
	// MaxRetries is how many times one datagram may be retransmitted before the
	// connection fails. Failure is the point: a transport that forgets a datagram
	// silently is what made the legacy layer corrupt messages.
	//
	// 20 is sized for the worst case this package is tested against. At 20% loss
	// the chance that a datagram and its next 19 retransmissions all vanish is
	// 0.2^20 ≈ 1e-14, so a healthy path never reaches the limit; a path that does
	// is not carrying traffic in any useful sense.
	MaxRetries int
	// RTTick is how often the retransmit timer scans the send queue. It also
	// bounds the delayed-ACK latency.
	RTTick time.Duration

	// AckEvery flushes an ACK after this many in-order data datagrams. ACKing
	// every datagram roughly doubles the packet rate of a bulk transfer for no
	// information gain, since one ACK reports the whole SACK bitmap.
	AckEvery int
	// AckInterval is the maximum delay for a partial ACK batch.
	AckInterval time.Duration

	// KeepAlivePeriod sends a ping when nothing has been sent for this long.
	KeepAlivePeriod time.Duration
	// IdleTimeout fails the connection when nothing has been received for this
	// long. It must exceed KeepAlivePeriod, otherwise a healthy idle connection
	// times itself out.
	IdleTimeout time.Duration

	// BatchSize is how many datagrams one send syscall may carry on platforms
	// that support a vectored send (Linux sendmmsg, Darwin sendmsg_x). Zero
	// means DefaultBatchSize.
	//
	// The value is a syscall-amortisation knob, not a protocol parameter: the
	// datagrams are identical, they leave in the same order, and a short send is
	// retried for the tail, so nothing about delivery changes. It is capped by
	// the platform limit (1024 on both) and by the number of fragments a Send has
	// ready.
	BatchSize int
	// DisableBatching forces one WriteTo per datagram. It exists for A/B
	// measurement and as an escape hatch for a platform whose vectored send
	// misbehaves; with it set the send path is exactly the pre-batching one.
	//
	// It is the send-side switch only. Receive batching has its own pair
	// (RecvBatchSize, DisableRecvBatching) so the two directions can be measured
	// independently.
	DisableBatching bool

	// RecvBatchSize is how many datagrams one receive syscall may carry on
	// platforms that support a vectored receive (Linux recvmmsg, Darwin
	// recvmsg_x). Zero means DefaultBatchSize, the same default as the send
	// side.
	//
	// It is a syscall-amortisation knob like BatchSize, with one extra effect: it
	// is also the number of datagram buffers the connection preallocates, so a
	// connection configured for 32 holds 32 × MaxDatagram of receive space rather
	// than growing it per call. It is capped by the platform limit.
	RecvBatchSize int
	// DisableRecvBatching forces one ReadFrom per datagram. It exists for A/B
	// measurement of the receive path on its own, and as an escape hatch for a
	// platform whose vectored receive misbehaves; with it set the read loop is
	// exactly the pre-receive-batching one.
	DisableRecvBatching bool
}

// DefaultBatchSize is how many datagrams one sendmmsg/sendmsg_x call carries by
// default.
//
// Chosen by measurement, not by feel: 8/16/32/64 were swept on 512 KiB at 0%
// loss (KPI.md round 11). Throughput is flat from 16 upward because the win is
// per-syscall and 16 datagrams already amortise it to within noise. 32 is taken
// over 16 because it halves the syscall count again at no measured cost, and
// over 64 because a batch holds its datagrams until it fills or Send ends, so a
// larger batch adds latency to the front of a message for no gain.
const DefaultBatchSize = 32

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		SendWindow:      1024,
		RecvWindow:      512,
		MaxReasmBytes:   64 << 20,
		InboxSize:       64,
		InitialRTO:      200 * time.Millisecond,
		MinRTO:          5 * time.Millisecond,
		MaxRTO:          3 * time.Second,
		MaxRetries:      20,
		RTTick:          2 * time.Millisecond,
		AckEvery:        8,
		AckInterval:     2 * time.Millisecond,
		KeepAlivePeriod: 20 * time.Second,
		IdleTimeout:     60 * time.Second,
		BatchSize:       DefaultBatchSize,
		RecvBatchSize:   DefaultBatchSize,
	}
}

func (c *Config) fill() {
	d := DefaultConfig()
	if c.SendWindow <= 0 {
		c.SendWindow = d.SendWindow
	}
	if c.RecvWindow <= 0 {
		c.RecvWindow = d.RecvWindow
	}
	if c.MaxReasmBytes <= 0 {
		c.MaxReasmBytes = d.MaxReasmBytes
	}
	if c.InboxSize <= 0 {
		c.InboxSize = d.InboxSize
	}
	if c.InitialRTO <= 0 {
		c.InitialRTO = d.InitialRTO
	}
	if c.MinRTO <= 0 {
		c.MinRTO = d.MinRTO
	}
	if c.MaxRTO <= 0 {
		c.MaxRTO = d.MaxRTO
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = d.MaxRetries
	}
	if c.RTTick <= 0 {
		c.RTTick = d.RTTick
	}
	if c.AckEvery <= 0 {
		c.AckEvery = d.AckEvery
	}
	if c.AckInterval <= 0 {
		c.AckInterval = d.AckInterval
	}
	if c.KeepAlivePeriod <= 0 {
		c.KeepAlivePeriod = d.KeepAlivePeriod
	}
	if c.BatchSize <= 0 {
		c.BatchSize = d.BatchSize
	}
	if c.RecvBatchSize <= 0 {
		c.RecvBatchSize = d.RecvBatchSize
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = d.IdleTimeout
	}
	if c.IdleTimeout <= c.KeepAlivePeriod {
		c.IdleTimeout = 2 * c.KeepAlivePeriod
	}
}

// statsCounters is the shared counter block. Atomics rather than a mutex
// because every counter is incremented on the packet path, and a mutex there
// would serialise the read and write loops against each other.
type statsCounters struct {
	sent      atomic.Uint64
	recv      atomic.Uint64
	retrans   atomic.Uint64
	rtoFires  atomic.Uint64
	fastFires atomic.Uint64
	failures  atomic.Uint64
}

func (s *statsCounters) incFailures() { s.failures.Add(1) }

// recvBatchCounters counts the batched receive path. They are separate from
// statsCounters because they are written by readLoop only, but read by Stats
// from another goroutine, so they are still atomic.
type recvBatchCounters struct {
	calls   atomic.Uint64
	packets atomic.Uint64
	short   atomic.Uint64
	max     int
}

// Stats is a snapshot of connection counters, for diagnostics and KPI runs.
type Stats struct {
	// Sent counts datagrams written, including retransmissions.
	Sent uint64
	// Recv counts datagrams read.
	Recv uint64
	// Retrans counts retransmitted datagrams (fast + RTO).
	Retrans uint64
	// RTOTimeouts counts scan passes that retransmitted at least one packet.
	RTOTimeouts uint64
	// FastRetrans counts packets declared lost by the packet threshold.
	FastRetrans uint64
	// Failures counts connections failed by the retry budget.
	Failures uint64

	// InFlight is the number of unacknowledged datagrams.
	InFlight int
	// Cwnd is the congestion window in datagrams, Ssthresh its slow-start
	// threshold, and SendWindow the configured ceiling. All three are reported so
	// a KPI run can see whether the sender was limited by the path (cwnd below
	// the ceiling) or by its own configuration.
	Cwnd       int
	Ssthresh   int
	SendWindow int
	// CCEnabled reports whether the congestion controller is active, and
	// CCReductions how many times it has reduced the window. Reductions counts
	// congestion *events*, not lost datagrams.
	CCEnabled    bool
	CCReductions uint64
	// ReasmEntries and ReasmBytes describe the reassembly table.
	ReasmEntries int
	ReasmBytes   int
	// Delivered and Dropped are receive-side message counters. Dropped counts
	// fragments discarded as duplicates or out of window.
	Delivered uint64
	Dropped   uint64

	// BatchCalls and BatchPackets describe the batched send path: how many
	// sendmmsg/sendmsg_x calls were made and how many datagrams they carried.
	// Batching is unavailable (BatchEnabled false) when the platform has no
	// vectored send, the socket is not a *net.UDPConn, or the connection was
	// built with DisableBatching.
	BatchCalls   uint64
	BatchPackets uint64
	BatchSize    int
	BatchEnabled bool

	// RecvBatchCalls and RecvBatchPackets describe the batched receive path: how
	// many recvmmsg/recvmsg_x calls the read loop made and how many datagrams
	// they dequeued. RecvBatchEnabled is false on a platform with no vectored
	// receive, on a socket that cannot expose a raw handle, and when the
	// connection was built with DisableRecvBatching.
	//
	// A healthy batched transfer has RecvBatchPackets ≈ Recv and
	// RecvBatchCalls ≪ RecvBatchPackets: the ratio is the syscall amortisation
	// the receive path gained.
	RecvBatchCalls   uint64
	RecvBatchPackets uint64
	RecvBatchSize    int
	RecvBatchEnabled bool

	SRTT time.Duration
	RTO  time.Duration
}

// Conn is one reliable, ordered, message-oriented channel.
type Conn struct {
	pc     net.PacketConn
	connID uint64

	remoteMu sync.RWMutex
	remote   net.Addr

	snd   *sender
	rcv   *receiver
	stats *statsCounters
	cfg   Config
	inbox chan []byte
	done  chan struct{}
	// closed is set exactly once, by closeWith.
	closed atomic.Bool

	// batcher carries the first transmission of a message in syscall-sized
	// batches (see batch.go). It holds nil when the socket cannot batch or
	// batching is disabled, in which case the send path writes one datagram per
	// call.
	//
	// It is an atomic pointer, not a plain field, because it can be cleared at
	// runtime (a destination the vectored call cannot express switches batching
	// off) while Stats reads it from another goroutine. Only Send writes it, and
	// the retransmission paths deliberately never touch it, so loss recovery
	// cannot be affected by batching at all.
	batcher atomic.Pointer[batchQueue]

	// recvBatcher carries up to RecvBatchSize datagrams per receive syscall (see
	// recvbatch.go). It is nil when the socket cannot batch or receive batching
	// is disabled, in which case readLoop reads one datagram per call.
	//
	// Unlike batcher it is set once at construction and never cleared: the
	// receive direction has no "destination the call cannot express", because a
	// reader does not need a destination. It is a plain field for the same
	// reason — only readLoop touches it.
	recvBatcher batchReader
	recvStats   *recvBatchCounters

	// ackPending counts in-order data datagrams since the last ACK, and ackDue
	// is when a partial batch must be flushed. Both are guarded by ackMu, which
	// is never held across a write.
	ackMu      sync.Mutex
	ackPending int
	ackDue     time.Time

	// ackBuf is the encode buffer for a pure ACK. Guarded by writeMu, which
	// sendAckNow holds across both the encode and the write.
	ackBuf [commonHeaderSize + maxAckBlockSize]byte

	// sendMu serialises Send: msgID order must match call order, and the peer
	// layer sends one frame at a time. It also keeps the ACK-field patching of
	// concurrent Sends from interleaving.
	sendMu sync.Mutex

	// writeMu serialises the patch-then-write sequence. A retransmission reuses
	// the buffer stored in the send queue, and three goroutines can retransmit
	// the same packet (Send for the first copy, the read loop for a fast
	// retransmit, the timer loop for an RTO). Patching the piggybacked ACK
	// fields in place without this mutex would be a data race on the buffer and
	// could ship a datagram whose header was half-updated.
	writeMu sync.Mutex

	closeOnce sync.Once
	closeErr  atomic.Pointer[error]

	lastRecv atomic.Int64 // unix nanos
	lastSend atomic.Int64
}

// NewConn wraps a packet socket. connID must match on both sides; it rejects
// datagrams that belong to a different session but landed on the same port (a
// normal occurrence with NAT rebinding and on a shared relay port).
//
// The socket is owned by the Conn: Close closes it.
func NewConn(pc net.PacketConn, remote net.Addr, connID uint64, cfg Config) *Conn {
	cfg.fill()
	c := &Conn{
		pc:     pc,
		connID: connID,
		remote: remote,
		cfg:    cfg,
		stats:  &statsCounters{},
		inbox:  make(chan []byte, cfg.InboxSize),
		done:   make(chan struct{}),
	}
	c.rcv = newReceiver(connID, cfg.RecvWindow, cfg.MaxReasmBytes, c.deliver)
	c.snd = newSender(cfg, c.stats, c.closeWith)
	c.lastRecv.Store(time.Now().UnixNano())

	// Decide batching once. A socket that cannot batch, or a connection with
	// batching disabled, leaves c.batcher nil and the send path writes one
	// datagram per call — the pre-batching behaviour, through the same code.
	if !cfg.DisableBatching {
		if w := senderFor(pc); w != nil {
			if q := newBatchQueue(&cappedWriter{w: w, max: cfg.BatchSize}); q != nil {
				c.batcher.Store(q)
			}
		}
	}

	// Same decision, other direction. The reader owns its datagram buffers, so it
	// is sized by the configured depth; a nil reader means the read loop uses
	// ReadFrom, one datagram per call.
	if !cfg.DisableRecvBatching {
		if r := receiverFor(pc, cfg.RecvBatchSize); r != nil {
			c.recvBatcher = r
			c.recvStats = &recvBatchCounters{max: r.maxBatch()}
		}
	}

	// The kernel's default receive buffer holds only a few hundred of our
	// 1200-byte datagrams while the send window allows 1024 in flight. A burst
	// that overflows the buffer is dropped by the kernel and looks exactly like
	// loss, so the effective window would collapse to whatever the buffer
	// holds. The assertion is on an interface rather than *net.UDPConn so that
	// a wrapper (a test proxy, a future instrumentation layer) can forward the
	// setting instead of silently losing it. Best effort: the platform may cap
	// the value.
	type bufferSetter interface{ SetReadBuffer(int) error }
	type writeBufferSetter interface{ SetWriteBuffer(int) error }
	if bs, ok := pc.(bufferSetter); ok {
		_ = bs.SetReadBuffer(8 << 20)
	}
	if bs, ok := pc.(writeBufferSetter); ok {
		_ = bs.SetWriteBuffer(8 << 20)
	}

	go c.readLoop()
	go c.timerLoop()
	go c.keepAliveLoop()
	return c
}

// Send writes one message and blocks until the peer acknowledges every
// fragment.
//
// Returning nil means "the peer's transport has all of it", which is the
// contract the peer layer needs before it can consider a frame transferred. It
// is deliberately not "queued locally": the legacy layer returned as soon as
// window space was available, so a message could be reported as sent and then
// never arrive.
//
// Send is not safe for concurrent use; the peer layer serialises sends.
func (c *Conn) Send(msg []byte) error {
	if len(msg) == 0 {
		return nil
	}
	if len(msg) > MaxMessageSize {
		return maxMessageError(len(msg))
	}
	if c.closed.Load() {
		return c.err()
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	frags := (len(msg) + MaxPayload - 1) / MaxPayload
	msgID := c.allocMsgID()
	out := &outMessage{remaining: int32(frags), done: make(chan struct{})}

	// The batch is a run of consecutive fragments of this one message. It is
	// flushed whenever it fills, whenever the window or the pacer says stop, and
	// once more after the last fragment. Every datagram in it is encoded before
	// the syscall, so a full batch costs one sendmmsg instead of one sendto per
	// datagram — the change that takes the transfer off the syscall floor.
	//
	// Ordering is preserved by construction: fragments are encoded and appended
	// in index order, and sendmmsg/sendmsg_x transmit the array in order. A short
	// return re-submits the tail (see batchQueue.write), so a partially sent batch
	// never reorders and never drops.
	q := c.batcher.Load()
	limit := q.limit()
	var batch [][]byte
	var batchPkts []*outPacket
	if q != nil {
		n := limit
		if n > frags {
			n = frags
		}
		batch = make([][]byte, 0, n)
		batchPkts = make([]*outPacket, 0, n)
	}

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := c.writeBatchData(q, batch, batchPkts)
		batch = batch[:0]
		batchPkts = batchPkts[:0]
		return err
	}

	for i := 0; i < frags; i++ {
		lo := i * MaxPayload
		hi := lo + MaxPayload
		if hi > len(msg) {
			hi = len(msg)
		}

		// Flush a pending batch before blocking on the window, never after.
		//
		// A datagram in the batch already holds a sequence number and is counted
		// in inFlight, but the peer has not seen it and nothing writes it until the
		// batch fills. If the window is smaller than the batch limit the batch can
		// never fill, so blocking here with a non-empty batch deadlocks the send
		// path: the window cannot free because the datagrams that would free it
		// are sitting in the buffer, and only the RTO timer eventually writes
		// them.
		//
		// That is not hypothetical. The congestion window starts at 10 and the
		// default batch limit is 32, so a congestion-controlled transfer that
		// batched before waiting sent its first ten fragments and then waited out
		// an RTO for every following group of ten: measured at 0.02 MiB/s on a
		// simulated 4 MB/s link, against 0.6 MiB/s with the fixed window.
		//
		// The check is "would block", not "flush every time". Flushing
		// unconditionally is correct but throws away batching: with a window of 10
		// and a limit of 32 the batch would be written every ten datagrams, which
		// is still an improvement over one syscall each but far below what the
		// batching exists for. So the batch is flushed only when the window is
		// full, which is exactly when waiting would deadlock.
		if !c.snd.hasWindowRoom() {
			if err := flush(); err != nil {
				return c.err()
			}
		}

		if !c.snd.waitForWindow(c.done) {
			return c.err()
		}
		// Pace before taking a sequence number: a datagram that has a sequence
		// number but has not been written is invisible to the peer, and the
		// timer would treat it as lost while it is still waiting here.
		if !c.snd.waitPacing(c.done) {
			return c.err()
		}

		seq := c.snd.allocSeq()
		data := makeDataPacket(c.connID, seq, msgID, uint32(i), uint32(frags), uint32(len(msg)), msg[lo:hi])
		p := &outPacket{data: data, seq: seq, msg: out}

		c.snd.mu.Lock()
		c.snd.enqueueLocked(p)
		c.snd.mu.Unlock()

		if q != nil {
			batch = append(batch, data)
			batchPkts = append(batchPkts, p)
			if len(batch) >= limit {
				if err := flush(); err != nil {
					return c.err()
				}
			}
			continue
		}

		// writeData refines the timestamp taken at enqueue. The enqueue stamp
		// exists so the timer tick can never observe a packet with a zero
		// send time; this one accounts for the time spent waiting here, which
		// matters because WriteTo can block for milliseconds on a full socket
		// buffer and a stale timestamp makes the packet look overdue the instant
		// it leaves.
		c.writeData(data, p)
	}
	if err := flush(); err != nil {
		return c.err()
	}

	select {
	case <-out.done:
		return nil
	case <-c.done:
		return c.err()
	}
}

// allocMsgID hands out message identities in call order. Wrap-around is
// harmless: every comparison is modular and the receive window is far below
// 2^31.
func (c *Conn) allocMsgID() uint32 {
	c.snd.mu.Lock()
	id := c.snd.nextMsgID
	c.snd.nextMsgID++
	c.snd.mu.Unlock()
	return id
}

// Recv returns the next complete message in message order.
func (c *Conn) Recv() ([]byte, error) {
	select {
	case m := <-c.inbox:
		return m, nil
	case <-c.done:
		// Messages already delivered must not be lost when the connection
		// ends; drain the inbox before reporting the error.
		select {
		case m := <-c.inbox:
			return m, nil
		default:
			return nil, c.err()
		}
	}
}

// deliver queues a completed message. It blocks when the inbox is full, which
// applies backpressure all the way to the sender's window.
func (c *Conn) deliver(m []byte) {
	select {
	case c.inbox <- m:
	case <-c.done:
	}
}

// Close tears the connection down. It is idempotent.
func (c *Conn) Close() error {
	c.closeWith(ErrClosed)
	return nil
}

func (c *Conn) closeWith(err error) {
	c.closeOnce.Do(func() {
		c.setErr(err)
		c.closed.Store(true)
		// Best-effort FIN so the peer stops waiting immediately instead of
		// after its idle timeout.
		buf := make([]byte, commonHeaderSize)
		encodeControl(buf, c.connID, pktFin)
		_, _ = c.pc.WriteTo(buf, c.remoteAddr())
		close(c.done)
		_ = c.pc.Close()
		// Release Send calls parked on the window: they select on done, but a
		// waiter that already re-checked the counter would otherwise sleep
		// until the next ACK that will never arrive.
		c.snd.signalSpace()
	})
}

func (c *Conn) setErr(err error) {
	if err == nil {
		err = ErrClosed
	}
	c.closeErr.CompareAndSwap(nil, &err)
}

func (c *Conn) err() error {
	if p := c.closeErr.Load(); p != nil {
		return *p
	}
	return ErrClosed
}

func (c *Conn) remoteAddr() net.Addr {
	c.remoteMu.RLock()
	defer c.remoteMu.RUnlock()
	return c.remote
}

// SetRemote changes the peer address. Hole punching may settle on a different
// candidate than the one initially dialled, and the transport should follow it
// rather than requiring a new connection.
func (c *Conn) SetRemote(addr net.Addr) {
	c.remoteMu.Lock()
	c.remote = addr
	c.remoteMu.Unlock()
}

// writeData transmits an encoded data datagram with the current ACK state
// patched in, and records the send time on the packet.
//
// Every transmission path (first send, fast retransmit, RTO) goes through here,
// so the peer always sees up-to-date acknowledgement information piggybacked on
// data. The buffer belongs to the send queue and may be written by more than one
// goroutine, so patch and write happen under writeMu.
//
// The retransmission timeout is stamped only on the *first* transmission. A
// retransmission that reset it to the current estimate would silently undo the
// exponential backoff the RTO path just applied, and the packet would be
// retransmitted again on every following tick. Measured before the fix: up to
// six copies of one datagram per 512 KiB transfer at 20% loss, roughly three
// times the retransmission count that the loss rate alone implies.
func (c *Conn) writeData(b []byte, p *outPacket) {
	c.writeMu.Lock()
	ack := c.rcv.ackSnapshot()
	patchAck(b, &ack)
	if p != nil {
		c.snd.mu.Lock()
		p.sentAt = time.Now()
		if p.rto == 0 {
			p.rto = c.snd.rto()
		}
		c.snd.mu.Unlock()
	}
	c.writeLocked(b)
	c.writeMu.Unlock()
}

// writeBatchData transmits a run of encoded data datagrams in one syscall,
// patching the current ACK state into each and stamping every packet's send
// time.
//
// The whole run is patched under one hold of writeMu, which is what keeps the
// ACK state consistent across the batch: a retransmission on another goroutine
// cannot slip between two datagrams of the same batch and give the peer a
// batch whose members report different receive windows. The datagrams carry
// the same ACK block, so patching once and copying would be equivalent, but
// patching each buffer in place avoids a copy per datagram.
//
// Timestamps are taken after the patch and before the syscall. They are all
// within microseconds of each other, and every one of them precedes the moment
// the kernel actually transmits, which is the safe direction for an RTO: a
// stamp taken after the write would make a datagram look younger than it is.
func (c *Conn) writeBatchData(q *batchQueue, bs [][]byte, ps []*outPacket) error {
	if q == nil || c.batcher.Load() == nil {
		// Batching was switched off at runtime (the destination is not an address
		// family the vectored call can express). Fall back to one datagram per
		// write rather than failing: the caller's fragments are already in the send
		// queue and must reach the peer.
		for i, b := range bs {
			var p *outPacket
			if i < len(ps) {
				p = ps[i]
			}
			c.writeData(b, p)
			if c.closed.Load() {
				return c.err()
			}
		}
		return nil
	}

	c.writeMu.Lock()
	ack := c.rcv.ackSnapshot()
	now := time.Now()
	for i, b := range bs {
		patchAck(b, &ack)
		if i < len(ps) && ps[i] != nil {
			p := ps[i]
			c.snd.mu.Lock()
			p.sentAt = now
			if p.rto == 0 {
				p.rto = c.snd.rto()
			}
			c.snd.mu.Unlock()
		}
	}
	err := c.writeBatchLocked(q, bs)
	c.writeMu.Unlock()
	return err
}

// writeBatchLocked performs the batched syscall. The caller holds writeMu.
//
// A batch is atomic from the caller's point of view: either it was submitted
// and every datagram in it left, or the connection is failed. There is no
// partial-success return, because a caller that had to reason about "some of
// these went out" would have to re-derive which ones, and the send queue
// already knows — those datagrams are in flight and the loss detector owns
// them. That is also why a write error fails the connection here exactly as it
// does on the single-datagram path: a UDP write error means the socket is gone.
func (c *Conn) writeBatchLocked(q *batchQueue, bs [][]byte) error {
	if len(bs) == 0 {
		return nil
	}
	c.lastSend.Store(time.Now().UnixNano())
	if err := q.write(bs, c.remoteAddr()); err != nil {
		// A destination the batched call cannot express is not a socket failure:
		// disable batching and let the next flush take the per-datagram path.
		// Any other error is fatal, as on the single-datagram path.
		if errors.Is(err, errBatchUnsupported) {
			c.batcher.Store(nil)
			return nil
		}
		c.closeWith(fmt.Errorf("p2p2: write: %w", err))
		return err
	}
	c.stats.sent.Add(uint64(len(bs)))
	return nil
}

// write transmits a datagram whose encoding is complete.
func (c *Conn) write(b []byte) {
	c.writeMu.Lock()
	c.writeLocked(b)
	c.writeMu.Unlock()
}

// writeLocked performs the syscall. The caller holds writeMu.
func (c *Conn) writeLocked(b []byte) {
	c.lastSend.Store(time.Now().UnixNano())
	if _, err := c.pc.WriteTo(b, c.remoteAddr()); err != nil {
		// A write error on a UDP socket means the socket is gone (closed
		// locally, or a hard ICMP error was delivered). Failing the connection
		// is the honest response; retrying would loop forever.
		c.closeWith(fmt.Errorf("p2p2: write: %w", err))
		return
	}
	c.stats.sent.Add(1)
}

// readLoop turns datagrams into state changes.
//
// The batched path and the per-datagram path share everything after the read:
// handleDatagram is the same function for both, so batching cannot change what
// a datagram means. The only difference is how many arrive per syscall, and
// that they arrive in the order the kernel dequeued them, which is the order
// they were sent on a FIFO path and the order they arrived on any path.
func (c *Conn) readLoop() {
	if c.recvBatcher != nil {
		c.readLoopBatched()
		return
	}
	c.readLoopSingle()
}

// readLoopSingle is the pre-receive-batching path: one ReadFrom per datagram.
// It is also the fallback on platforms with no vectored receive, and the
// reference the batched path is compared against.
func (c *Conn) readLoopSingle() {
	buf := make([]byte, MaxDatagram)
	for {
		n, addr, err := c.pc.ReadFrom(buf)
		if err != nil {
			if !c.closed.Load() {
				c.closeWith(fmt.Errorf("p2p2: read: %w", err))
			}
			return
		}
		// Follow a source that differs from the current remote, and skip the lock
		// when it does not. sameAddr is the allocation-free form of the
		// addr.String() comparison this path used before; the semantics are
		// identical.
		if addr != nil && sameAddr(addr, c.remoteAddr()) {
			addr = nil
		}
		if !c.handleDatagram(buf[:n], addr) {
			return
		}
	}
}

// readLoopBatched dequeues up to RecvBatchSize datagrams per syscall.
//
// The buffers belong to the reader and are reused for the life of the
// connection. That is safe because handleDatagram consumes each datagram
// completely before the next is looked at: it decodes the header, copies the
// payload into reassembly state, and never retains the slice. The packet
// struct's payload aliases the buffer, and nothing outside handleData/handleAck
// keeps a packet.
//
// A short return (fewer datagrams than the batch could hold) is not an error
// and needs no retry: it just means the socket queue was shallower than the
// batch, and the next call picks up whatever arrives next. Processing stops at
// the returned count, so a stale length in the tail of the array can never be
// read as a datagram.
func (c *Conn) readLoopBatched() {
	depth := c.recvBatcher.maxBatch()
	if depth < 1 {
		depth = 1
	}
	var src srcTracker

	for {
		got, err := c.recvBatcher.readBatch(depth)
		if err != nil {
			if !c.closed.Load() {
				c.closeWith(fmt.Errorf("p2p2: read: %w", err))
			}
			return
		}
		if got <= 0 {
			// The reader contract forbids this; treat it as fatal rather than
			// spinning on a call that reports nothing.
			c.closeWith(fmt.Errorf("p2p2: read: %w", errBatchRecvStalled))
			return
		}
		// Taken after the read, not before: a wrapping reader may build its
		// output buffers lazily, and a reader is allowed to hand back a
		// different buffer set for a different depth.
		bufs, lens := c.recvBatcher.buffers()
		if got > len(bufs) || got > len(lens) {
			c.closeWith(fmt.Errorf("p2p2: read: reader reported %d datagrams but holds %d buffers", got, len(bufs)))
			return
		}
		if c.recvStats != nil {
			c.recvStats.calls.Add(1)
			c.recvStats.packets.Add(uint64(got))
			if got < depth {
				c.recvStats.short.Add(1)
			}
		}

		// Read the current remote once per batch, not once per datagram: the
		// common case is that no datagram in the batch changes it.
		cur := c.remoteAddr()
		for i := 0; i < got; i++ {
			var addr net.Addr
			if a, ok := src.changed(c.recvBatcher.rawSource(i), cur); ok {
				addr = a
				cur = a
			}
			if !c.handleDatagram(bufs[i][:lens[i]], addr) {
				return
			}
		}
	}
}

// handleDatagram applies one datagram to connection state. It returns false when
// the connection ended and the read loop must stop.
//
// addr is non-nil only when the source of this datagram differs from the
// connection's current remote; that is the read path's existing "follow the
// peer" behaviour, decided by the caller so both read paths share it.
func (c *Conn) handleDatagram(b []byte, addr net.Addr) bool {
	p, perr := decodePacket(b)
	if perr != nil || p.connID != c.connID {
		return true // stray traffic on the port
	}
	c.stats.recv.Add(1)
	c.lastRecv.Store(time.Now().UnixNano())

	// The peer's address can differ from the one dialled (normal for symmetric
	// NATs on the first reply). Follow it so ACKs go back the way the data came.
	if addr != nil {
		c.SetRemote(addr)
	}

	switch p.typ {
	case pktData:
		c.handleData(p)
	case pktAck:
		c.handleAck(p)
	case pktPing:
		c.writeControl(pktPong)
	case pktPong:
		// liveness already recorded above
	case pktFin:
		c.closeWith(ErrClosed)
		return false
	}
	return true
}

func (c *Conn) handleData(p packet) {
	// The ACK policy follows from what the datagram meant:
	//
	//   - it completed a message: ACK now, because the peer's Send is blocked
	//     on exactly that message and any delay is added to its latency;
	//   - it was a duplicate: ACK now. A duplicate exists only because our
	//     previous ACK was lost or late, so silence would keep the sender
	//     retransmitting the same datagram until its retry budget ran out. RFC
	//     5681 §4.2 mandates the immediate duplicate ACK for the same reason;
	//   - anything else, in order or out of order: batch it.
	//
	// Batching out-of-order arrivals is safe because one ACK block reports every
	// run the receiver knows about, so a single ACK carries the same information
	// as N immediate ones. It is also necessary: a loss makes every subsequent
	// datagram "out of order", so ACKing each of them sends one datagram back per
	// datagram received. Measured on 512 KiB at 5% loss: one ACK per data
	// datagram (ratio 0.98) and a third of the CPU in the ACK path, versus 0.13
	// when batched.
	//
	// The cost of batching is that the loss signal is delayed by at most
	// AckInterval, which is 2ms — far below the RTO it replaces, and below the
	// reordering window that loss detection already waits out.
	out, err := c.rcv.handleData(p)
	if err != nil {
		c.closeWith(err)
		return
	}
	if out.duplicate || out.delivered > 0 {
		c.ackMu.Lock()
		c.ackPending = 0
		c.ackDue = time.Time{}
		c.ackMu.Unlock()
		c.sendAckNow()
		return
	}
	c.noteAck()
}

// noteAck counts an in-order datagram and flushes when the batch is full.
//
// The flush must not go through flushAck: that function's job is "send only if
// something is pending", and by the time the batch is full the counter has been
// reset to zero, so it would find nothing to do and stay silent. The effect is
// subtle and severe — the last fragments of every message would never be
// acknowledged, so Send would retransmit them until the retry budget ran out.
func (c *Conn) noteAck() {
	c.ackMu.Lock()
	c.ackPending++
	due := c.ackPending >= c.cfg.AckEvery
	if due {
		c.ackPending = 0
		c.ackDue = time.Time{}
	} else if c.ackDue.IsZero() {
		c.ackDue = time.Now().Add(c.cfg.AckInterval)
	}
	c.ackMu.Unlock()
	if due {
		c.sendAckNow()
	}
}

// flushAck sends a pure ACK if a batch is outstanding.
func (c *Conn) flushAck() {
	c.ackMu.Lock()
	if c.ackPending == 0 {
		c.ackMu.Unlock()
		return
	}
	c.ackPending = 0
	c.ackDue = time.Time{}
	c.ackMu.Unlock()
	c.sendAckNow()
}

// sendAckNow emits an ACK unconditionally. It is the only place that builds one,
// so the ACK format and the state it reports cannot drift apart.
//
// The buffer is a field rather than a local: it is passed to net.PacketConn's
// WriteTo through an interface, which makes a local array escape and cost one
// allocation per ACK (measured: 40k allocations per 800 transfers, 4% of the
// transfer's total). The field is safe because it is only touched under writeMu,
// which is held across the encode and the write.
func (c *Conn) sendAckNow() {
	c.writeMu.Lock()
	ack := c.rcv.ackSnapshot()
	n := encodeAck(c.ackBuf[:], c.connID, &ack)
	c.writeLocked(c.ackBuf[:n])
	c.writeMu.Unlock()
}

func (c *Conn) handleAck(p packet) {
	res := c.snd.handleAck(p.ack, time.Now())
	if n := len(res.retransmit); n > 0 {
		c.stats.fastFires.Add(uint64(n))
		c.stats.retrans.Add(uint64(n))
		for _, rp := range res.retransmit {
			c.writeData(rp.data, rp)
		}
	}
}

func (c *Conn) writeControl(t packetType) {
	buf := make([]byte, commonHeaderSize)
	encodeControl(buf, c.connID, t)
	c.write(buf)
}

// timerLoop drives RTO retransmission, delayed ACK flushing and idle timeout.
func (c *Conn) timerLoop() {
	tick := time.NewTicker(c.cfg.RTTick)
	defer tick.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-tick.C:
		}

		now := time.Now()

		// Delayed ACK: a batch that did not reach AckEvery still has to be
		// acknowledged, or the last fragments of a message would wait for an
		// ACK that is never sent.
		c.ackMu.Lock()
		due := c.ackPending > 0 && !c.ackDue.IsZero() && !now.Before(c.ackDue)
		c.ackMu.Unlock()
		if due {
			c.flushAck()
		}

		// Loss detection on the tick, not only on ACKs. A Send bursts a whole
		// message, so the last ACK often arrives before the reordering window has
		// elapsed; after that no further ACK comes (nothing is left to
		// acknowledge) and every remaining loss would wait out a full RTO. The
		// predicate is the same one the ACK path uses, so this cannot declare
		// anything lost that a late ACK would not have.
		if resend := c.snd.lossScan(now); len(resend) > 0 {
			c.stats.fastFires.Add(uint64(len(resend)))
			c.stats.retrans.Add(uint64(len(resend)))
			for _, rp := range resend {
				c.writeData(rp.data, rp)
			}
		}

		if resend := c.snd.timeoutScan(now); len(resend) > 0 {
			c.stats.rtoFires.Add(1)
			c.stats.retrans.Add(uint64(len(resend)))
			for _, rp := range resend {
				c.writeData(rp.data, rp)
			}
		}

		last := time.Unix(0, c.lastRecv.Load())
		if now.Sub(last) > c.cfg.IdleTimeout {
			c.closeWith(ErrIdleTimeout)
			return
		}
	}
}

// keepAliveLoop keeps the NAT mapping open and gives the peer liveness signal.
func (c *Conn) keepAliveLoop() {
	t := time.NewTicker(c.cfg.KeepAlivePeriod)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, c.lastSend.Load())) < c.cfg.KeepAlivePeriod {
				continue
			}
			c.writeControl(pktPing)
		}
	}
}

// Stats returns a snapshot of the connection counters.
func (c *Conn) Stats() Stats {
	c.snd.mu.Lock()
	s := Stats{
		InFlight:     c.snd.inFlight,
		Cwnd:         c.snd.cc.window(),
		Ssthresh:     c.snd.cc.threshold(),
		SendWindow:   c.snd.cc.max,
		CCEnabled:    c.snd.cc.enabled,
		CCReductions: c.snd.cc.reductions,
		SRTT:         c.snd.srtt,
		RTO:          c.snd.rto(),
	}
	c.snd.mu.Unlock()

	delivered, dropped, entries, bytes := c.rcv.stats()
	s.Delivered = delivered
	s.Dropped = dropped
	s.ReasmEntries = entries
	s.ReasmBytes = bytes

	s.Sent = c.stats.sent.Load()
	s.Recv = c.stats.recv.Load()
	s.Retrans = c.stats.retrans.Load()
	s.RTOTimeouts = c.stats.rtoFires.Load()
	s.FastRetrans = c.stats.fastFires.Load()
	s.Failures = c.stats.failures.Load()

	bs := c.batcher.Load().snapshot()
	s.BatchCalls = bs.calls
	s.BatchPackets = bs.packets
	s.BatchSize = bs.max
	s.BatchEnabled = bs.enabled

	if rs := c.recvStats; rs != nil {
		s.RecvBatchCalls = rs.calls.Load()
		s.RecvBatchPackets = rs.packets.Load()
		s.RecvBatchSize = rs.max
		s.RecvBatchEnabled = c.recvBatcher != nil
	}
	return s
}

// LocalAddr reports the local address of the underlying socket.
func (c *Conn) LocalAddr() net.Addr { return c.pc.LocalAddr() }

// RemoteAddr reports the current peer address.
func (c *Conn) RemoteAddr() net.Addr { return c.remoteAddr() }

// Err returns the reason the connection ended, or nil while it is alive.
func (c *Conn) Err() error {
	if !c.closed.Load() {
		return nil
	}
	return c.err()
}

// errors.Is needs the sentinel values to be comparable; keeping them here
// documents that every error returned by Recv/Send wraps one of them.
var _ = errors.Is
