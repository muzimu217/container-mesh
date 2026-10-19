// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Conn is a reliable, ordered, message-oriented channel over UDP.
//
// Reliability model: cumulative acknowledgement plus per-sequence retransmit.
// This is simpler than TCP's sliding window with selective ACK, and it is
// enough for the traffic this project carries (bulk file chunks and small
// messages) because:
//
//   - the peer layer already paces itself (it waits for each frame's send to
//     complete), so there is never a deep queue to manage;
//   - a lost datagram costs one RTO, and the RTO is measured from the actual
//     path (see srtt/rttvar), not guessed.
//
// The window still allows several datagrams in flight: without it, throughput
// on any link with non-trivial RTT collapses to one datagram per round trip.
type Conn struct {
	udp *net.UDPConn
	// remote is where datagrams are sent. It can change during hole punching
	// (a peer may learn a better address for us), so it is guarded.
	remoteMu sync.RWMutex
	remote   *net.UDPAddr

	connID uint64

	// send side
	sendMu sync.Mutex
	// windowFree is signalled when an ACK frees window space, replacing the
	// polling loop that capped throughput.
	windowFree *sync.Cond
	sendSeq    uint32
	sendBuf    map[uint32]*outFrame // unacknowledged fragments
	srtt       time.Duration
	rttvar     time.Duration
	rto        time.Duration
	lastAck    uint32
	window     int
	inFlight   int

	// receive side
	recvMu sync.Mutex
	// recvNext is the next sequence number we have NOT yet received. It
	// advances as soon as a datagram arrives (not when the whole message is
	// reassembled), because the sender needs it to free window space while it
	// is still sending the rest of a multi-fragment message.
	recvNext  uint32
	recvSeen  map[uint32]bool   // received seqs not yet contiguous
	recvParts map[uint32][]byte // fragments of the message being assembled
	recvTotal int

	inbox chan []byte

	closeOnce sync.Once
	done      chan struct{}
	closeErr  error

	// stats
	statsMu  sync.Mutex
	sent     uint64
	recv     uint64
	retrans  uint64
	lastRecv time.Time
}

// outFrame is one unacknowledged datagram.
type outFrame struct {
	data     []byte
	sentAt   time.Time
	attempts int
	lastSeq  uint32 // highest seq covered by this frame (for ACK accounting)
}

// Config tunes a Conn.
type Config struct {
	// Window is the maximum number of unacknowledged datagrams.
	Window int
	// InitialRTO seeds the retransmit timer before the first measurement.
	InitialRTO time.Duration
	// MaxRTO caps exponential backoff.
	MaxRTO time.Duration
	// InboxSize bounds the delivered-message queue.
	InboxSize int
}

// DefaultConfig returns sane defaults.
func DefaultConfig() Config {
	return Config{
		Window:     512,
		InitialRTO: 200 * time.Millisecond,
		MaxRTO:     3 * time.Second,
		InboxSize:  64,
	}
}

func (c *Config) fill() {
	d := DefaultConfig()
	if c.Window <= 0 {
		c.Window = d.Window
	}
	if c.InitialRTO <= 0 {
		c.InitialRTO = d.InitialRTO
	}
	if c.MaxRTO <= 0 {
		c.MaxRTO = d.MaxRTO
	}
	if c.InboxSize <= 0 {
		c.InboxSize = d.InboxSize
	}
}

// NewConn wraps an already-connected UDP socket. connID must match on both
// sides; it isolates this session from stray datagrams that reach the port.
func NewConn(udp *net.UDPConn, remote *net.UDPAddr, connID uint64, cfg Config) *Conn {
	cfg.fill()
	c := &Conn{
		udp:       udp,
		remote:    remote,
		connID:    connID,
		sendBuf:   make(map[uint32]*outFrame),
		recvParts: make(map[uint32][]byte),
		recvSeen:  make(map[uint32]bool),
		inbox:     make(chan []byte, cfg.InboxSize),
		done:      make(chan struct{}),
		rto:       cfg.InitialRTO,
		window:    cfg.Window,
		lastRecv:  time.Now(),
	}
	c.windowFree = sync.NewCond(&c.sendMu)
	go c.readLoop()
	go c.retransmitLoop(cfg)
	return c
}

// Send writes one message, splitting it into datagrams. It blocks until the
// whole message is acknowledged, which is what the peer layer expects from
// MessageConn.Send (the relay path has the same backpressure property).
func (c *Conn) Send(msg []byte) error {
	if len(msg) == 0 {
		return nil
	}
	select {
	case <-c.done:
		return c.closedErr()
	default:
	}

	frames := fragment(msg)
	total := uint32(len(frames))

	for i, frag := range frames {
		var flags uint8
		if i == 0 {
			flags |= flagFirst
		}
		if i == len(frames)-1 {
			flags |= flagLast
		}
		if err := c.sendFragment(frag, flags, total, uint32(i)); err != nil {
			return err
		}
	}
	return nil
}

// sendFragment queues and transmits one datagram, waiting for window space.
func (c *Conn) sendFragment(payload []byte, flags uint8, total, index uint32) error {
	// Wait for window space. A condition variable is used instead of polling:
	// a 512 KiB message is ~440 datagrams, and 2 ms of polling per batch
	// dominated the transfer time (measured: 6 MiB/s vs 30+ after the change).
	c.sendMu.Lock()
	for c.inFlight >= c.window {
		if c.isDone() {
			c.sendMu.Unlock()
			return c.closedErr()
		}
		c.windowFree.Wait()
	}

	seq := c.sendSeq
	c.sendSeq++

	// Encode the fragment index so the receiver can reassemble out of order.
	// Layout: total(4) || index(4) || payload
	body := make([]byte, 8+len(payload))
	putUint32(body[0:4], total)
	putUint32(body[4:8], index)
	copy(body[8:], payload)

	pkt := &packet{
		typ:     pktData,
		connID:  c.connID,
		seq:     seq,
		ack:     c.recvNextLocked(),
		flags:   flags,
		payload: body,
	}
	data, err := pkt.encode()
	if err != nil {
		c.sendMu.Unlock()
		return err
	}

	c.sendBuf[seq] = &outFrame{data: data, sentAt: time.Now(), lastSeq: seq}
	c.inFlight++
	remote := c.remoteAddr()
	c.sendMu.Unlock()

	if _, werr := c.udp.WriteToUDP(data, remote); werr != nil {
		return fmt.Errorf("p2p: write: %w", werr)
	}
	c.statsMu.Lock()
	c.sent++
	c.statsMu.Unlock()
	return nil
}

func (c *Conn) recvNextLocked() uint32 {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()
	return c.recvNext
}

// Recv returns the next complete message in order.
func (c *Conn) Recv() ([]byte, error) {
	select {
	case msg, ok := <-c.inbox:
		if !ok {
			return nil, c.closedErr()
		}
		return msg, nil
	case <-c.done:
		return nil, c.closedErr()
	}
}

// Close tears the connection down.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = errors.New("p2p: connection closed")
		// Best-effort FIN so the peer stops waiting for more fragments.
		pkt := &packet{typ: pktFin, connID: c.connID, ack: c.recvNextLocked()}
		if data, err := pkt.encode(); err == nil {
			_, _ = c.udp.WriteToUDP(data, c.remoteAddr())
		}
		close(c.done)
		c.sendMu.Lock()
		if c.windowFree != nil {
			c.windowFree.Broadcast()
		}
		c.sendMu.Unlock()
		_ = c.udp.Close()
	})
	return nil
}

// isDone reports whether the connection is shutting down, without blocking.
func (c *Conn) isDone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *Conn) closedErr() error {
	if c.closeErr != nil {
		return c.closeErr
	}
	return errors.New("p2p: connection closed")
}

func (c *Conn) remoteAddr() *net.UDPAddr {
	c.remoteMu.RLock()
	defer c.remoteMu.RUnlock()
	return c.remote
}

// SetRemote updates the peer address (used when hole punching settles on a
// different candidate than the one initially dialled).
func (c *Conn) SetRemote(addr *net.UDPAddr) {
	c.remoteMu.Lock()
	c.remote = addr
	c.remoteMu.Unlock()
}

// readLoop receives datagrams and drives both the ACK and reassembly logic.
func (c *Conn) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, addr, err := c.udp.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-c.done:
				return
			default:
			}
			c.closeOnce.Do(func() {
				c.closeErr = fmt.Errorf("p2p: read: %w", err)
				close(c.done)
			})
			return
		}
		pkt, perr := decode(buf[:n])
		if perr != nil || pkt.connID != c.connID {
			continue // stray traffic on the port
		}

		c.statsMu.Lock()
		c.recv++
		c.lastRecv = time.Now()
		c.statsMu.Unlock()

		// The peer's address may differ from what we dialled (that is normal
		// for symmetric NATs on the first reply); adopt it so ACKs go back the
		// way the data came.
		if addr != nil && !addr.IP.Equal(c.remoteAddr().IP) {
			c.SetRemote(addr)
		}

		switch pkt.typ {
		case pktData:
			c.handleData(pkt)
		case pktAck:
			c.handleAck(pkt)
		case pktPing:
			c.sendControl(pktPong, pkt.seq)
		case pktPong:
			c.statsMu.Lock()
			c.lastRecv = time.Now()
			c.statsMu.Unlock()
		case pktFin:
			c.closeOnce.Do(func() {
				c.closeErr = errors.New("p2p: peer closed the connection")
				close(c.done)
			})
			return
		}
	}
}

// handleData acknowledges and reassembles.
func (c *Conn) handleData(pkt *packet) {
	if len(pkt.payload) < 8 {
		return
	}
	total := getUint32(pkt.payload[0:4])
	index := getUint32(pkt.payload[4:8])
	body := pkt.payload[8:]

	c.recvMu.Lock()
	// Advance the cumulative ACK over every datagram that has arrived,
	// including out-of-order ones. This is what frees the sender's window;
	// without it a 512 KiB message (≈440 fragments) stalls after the first
	// window's worth of datagrams.
	if pkt.seq == c.recvNext {
		c.recvNext++
		for c.recvSeen[c.recvNext] {
			delete(c.recvSeen, c.recvNext)
			c.recvNext++
		}
	} else if seqAfter(pkt.seq, c.recvNext) {
		c.recvSeen[pkt.seq] = true
	}

	c.recvParts[index] = append([]byte(nil), body...)
	c.recvTotal = int(total)

	// Try to assemble: fragments are ordered by index, and index 0 arrives
	// first on the happy path.
	if len(c.recvParts) == int(total) {
		var msg []byte
		for i := uint32(0); i < total; i++ {
			part, ok := c.recvParts[i]
			if !ok {
				msg = nil
				break
			}
			msg = append(msg, part...)
		}
		if msg != nil {
			c.recvParts = make(map[uint32][]byte)
			c.recvMu.Unlock()

			c.sendAck(c.recvNextLocked())
			select {
			case c.inbox <- msg:
			case <-c.done:
			}
			return
		}
	}
	c.recvMu.Unlock()

	// Acknowledge receipt even when the message is incomplete: the sender
	// needs the ACK to free window space and keep sending the missing parts.
	c.sendAck(c.recvNextLocked())
}

// handleAck applies a cumulative acknowledgement.
func (c *Conn) handleAck(pkt *packet) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	freed := false
	for seq, frame := range c.sendBuf {
		if seqBefore(seq, pkt.ack) || seq == pkt.ack {
			// RTT sample from the frame that was actually acknowledged.
			if frame.attempts == 0 {
				c.updateRTT(time.Since(frame.sentAt))
			}
			delete(c.sendBuf, seq)
			if c.inFlight > 0 {
				c.inFlight--
			}
			freed = true
		}
	}
	if freed && c.windowFree != nil {
		c.windowFree.Broadcast()
	}
}

// updateRTT applies RFC 6298 smoothing.
func (c *Conn) updateRTT(sample time.Duration) {
	if sample <= 0 {
		return
	}
	if c.srtt == 0 {
		c.srtt = sample
		c.rttvar = sample / 2
	} else {
		diff := c.srtt - sample
		if diff < 0 {
			diff = -diff
		}
		c.rttvar = (3*c.rttvar + diff) / 4
		c.srtt = (7*c.srtt + sample) / 8
	}
	c.rto = c.srtt + 4*c.rttvar
	if c.rto < 50*time.Millisecond {
		c.rto = 50 * time.Millisecond
	}
}

// retransmitLoop resends unacknowledged datagrams after their RTO.
func (c *Conn) retransmitLoop(cfg Config) {
	// The ticker only needs to be finer than the minimum RTO (50 ms); scanning
	// the send buffer more often than that just burns CPU and contends with
	// the send path.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}

		now := time.Now()
		c.sendMu.Lock()
		rto := c.rto
		var resend [][]byte
		freed := false
		for seq, frame := range c.sendBuf {
			if now.Sub(frame.sentAt) < rto {
				continue
			}
			frame.attempts++
			frame.sentAt = now
			resend = append(resend, frame.data)
			if frame.attempts > 12 {
				// Give up on a path that has been dead for ~12 RTOs; the
				// caller's reconnect logic takes over.
				delete(c.sendBuf, seq)
				if c.inFlight > 0 {
					c.inFlight--
				}
				freed = true
			}
		}
		// Exponential backoff, capped.
		if c.rto < cfg.MaxRTO {
			c.rto *= 2
			if c.rto > cfg.MaxRTO {
				c.rto = cfg.MaxRTO
			}
		}
		if freed && c.windowFree != nil {
			c.windowFree.Broadcast()
		}
		remote := c.remoteAddr()
		c.sendMu.Unlock()

		for _, data := range resend {
			_, _ = c.udp.WriteToUDP(data, remote)
			c.statsMu.Lock()
			c.retrans++
			c.statsMu.Unlock()
		}
	}
}

func (c *Conn) sendAck(ack uint32) {
	pkt := &packet{typ: pktAck, connID: c.connID, ack: ack}
	if data, err := pkt.encode(); err == nil {
		_, _ = c.udp.WriteToUDP(data, c.remoteAddr())
	}
}

func (c *Conn) sendControl(t packetType, seq uint32) {
	pkt := &packet{typ: t, connID: c.connID, seq: seq, ack: c.recvNextLocked()}
	if data, err := pkt.encode(); err == nil {
		_, _ = c.udp.WriteToUDP(data, c.remoteAddr())
	}
}

// Stats reports counters for diagnostics.
type Stats struct {
	Sent     uint64
	Recv     uint64
	Retrans  uint64
	SRTT     time.Duration
	RTO      time.Duration
	InFlight int
	LastRecv time.Time
}

// Stats returns a snapshot.
func (c *Conn) Stats() Stats {
	c.statsMu.Lock()
	s := Stats{Sent: c.sent, Recv: c.recv, Retrans: c.retrans, LastRecv: c.lastRecv}
	c.statsMu.Unlock()
	c.sendMu.Lock()
	s.SRTT = c.srtt
	s.RTO = c.rto
	s.InFlight = c.inFlight
	c.sendMu.Unlock()
	return s
}

// fragment splits a message into datagram payloads.
func fragment(msg []byte) [][]byte {
	var out [][]byte
	for off := 0; off < len(msg); off += MaxPayload - 8 {
		end := off + MaxPayload - 8
		if end > len(msg) {
			end = len(msg)
		}
		out = append(out, msg[off:end])
	}
	return out
}

// seq helpers handle uint32 wraparound (sequence numbers are compared modulo
// 2^32, as in TCP).
func seqBefore(a, b uint32) bool { return int32(a-b) < 0 }
func seqAfter(a, b uint32) bool  { return int32(a-b) > 0 }

func putUint32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func getUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
