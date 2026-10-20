// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"errors"
	"sync"
	"time"
)

// Errors a Conn can fail with. Send never returns nil unless the peer
// acknowledged the whole message, so these are the only ways a Send can end
// without delivery.
var (
	// ErrPeerUnreachable means a fragment was retransmitted until the retry
	// budget ran out. The connection is failed rather than dropping the
	// fragment: a transport that silently forgets data turns a recoverable
	// network problem into application-visible corruption.
	ErrPeerUnreachable = errors.New("p2p2: peer stopped acknowledging")
	// ErrIdleTimeout means nothing arrived for IdleTimeout.
	ErrIdleTimeout = errors.New("p2p2: idle timeout")
	// ErrClosed means Close was called (locally or by the peer).
	ErrClosed = errors.New("p2p2: connection closed")
)

// fastRetransmitThreshold is how many packets sent after a hole must be
// acknowledged before the hole is presumed lost. Three is the value RFC 9002
// §6.1.1 recommends; it tolerates the reordering that ordinary networks
// produce without waiting for an RTO.
const fastRetransmitThreshold = 3

// timerGranularity is the floor on the reordering window, from RFC 9002 §6.1.2
// (kGranularity = 1ms).
//
// Without a floor the window is 9/8 of the smoothed RTT, which on a loopback
// path with a 50µs RTT is 56µs — shorter than the time a datagram spends in the
// kernel's socket queue. Every packet that waited in the queue then looks lost,
// and the sender retransmits data the peer is about to receive: measured as 112
// retransmissions for the ~45 datagrams actually lost at 10%, and a 3x
// throughput penalty.
const timerGranularity = time.Millisecond

// outMessage tracks one message until every fragment is acknowledged.
type outMessage struct {
	remaining int32 // fragments not yet acknowledged
	done      chan struct{}
}

// outPacket is one unacknowledged datagram.
//
// data is the encoded datagram and is written more than once (initial send,
// fast retransmit, RTO). The message identity fields inside it are never
// rewritten: only the ACK fields at the tail are patched at transmission time.
type outPacket struct {
	data []byte
	seq  uint32
	msg  *outMessage

	sentAt        time.Time
	rto           time.Duration
	attempts      int
	retransmitted bool // Karn's rule: never take an RTT sample from these
	lost          bool // already declared lost; the RTO timer owns it now
	// lostAtAcked is largestAcked at the moment this packet was last declared
	// lost. A packet may only be re-declared once the peer has acknowledged
	// something *newer* than that, because otherwise nothing has changed since
	// the declaration and retransmitting again would just duplicate the copy
	// already in flight.
	lostAtAcked uint32
	haveLostAt  bool

	prev, next *outPacket
}

// sender owns the send window, the retransmission queue and RTT estimation.
type sender struct {
	mu sync.Mutex

	// head/tail keep unacknowledged packets in sequence order. Order matters
	// twice: loss detection walks forward from the oldest packet, and a
	// cumulative ACK removes a prefix.
	head, tail *outPacket
	bySeq      map[uint32]*outPacket

	nextSeq   uint32
	nextMsgID uint32
	inFlight  int

	// largestAcked is the highest sequence number the peer has reported
	// receiving, and haveAck says whether that is meaningful yet.
	largestAcked uint32
	haveAck      bool
	// largestAckedAt is when largestAcked last advanced. It is what makes the
	// reordering window meaningful: a packet is only "old" relative to the ACK
	// that passed it, not relative to the wall clock. Without it, every ACK after
	// the first re-declares the same holes, because largestAcked never moves
	// backwards.
	largestAckedAt time.Time

	// ackUna is the highest cumulative point any ACK has reported, and haveUna
	// says whether one has arrived. It is a diagnostic and a sanity gate for loss
	// detection: until an ACK has named a cumulative point there is nothing to
	// measure a reordering window against.
	ackUna  uint32
	haveUna bool

	// RFC 6298 state. srtt/rttvar are zero until the first sample.
	srtt   time.Duration
	rttvar time.Duration

	// lastRTT is the most recent sample, kept because RFC 9002 §5.3 uses the
	// latest RTT (not the smoothed one) to decide when a packet is too old.
	lastRTT time.Duration

	// minRTT is the smallest RTT ever sampled. It estimates the path's
	// propagation delay without queueing, which is what pacing needs; the
	// smoothed RTT is deliberately not used there because it grows under
	// load and would slow the sender exactly when it is busiest.
	minRTT time.Duration

	cfg   Config
	stats *statsCounters

	// Pacing. A window-sized burst is not free: the last datagrams of the burst
	// queue behind the first in the kernel socket buffer and, on the receiving
	// side, in the socket's receive queue. When that queue overflows the kernel
	// drops datagrams and the loss is indistinguishable from a congested link —
	// measured as 148 RTO fires and 316 retransmissions for 454 fragments at 20%
	// loss with an unpaced 1024-packet window, and outright connection failure
	// at 256.
	//
	// Spacing the window over one RTT removes the queue without reducing
	// throughput: the packets still leave within a round trip, but no single
	// instant has more than a few of them in flight.
	//
	// The rate is derived from minRTT (queue-free propagation delay), not srtt:
	// srtt includes the very queueing that pacing exists to avoid, so using it
	// would slow the sender down whenever the queue grew.
	nextSend time.Time

	// windowFree wakes Send calls waiting for space. A channel rather than a
	// sync.Cond so that a connection failure (which may be signalled from
	// inside the sender lock) can release waiters without lock-ordering
	// tricks: waiters also select on the connection's done channel.
	windowFree chan struct{}

	// failed records that the retry budget ran out. It stops loss detection
	// from retransmitting into a connection that is already dead: onFail
	// closes the connection, but a scan that is mid-iteration still holds the
	// queue and would otherwise keep scheduling writes for a socket that no
	// longer exists.
	failed bool

	onFail func(error)
}

func newSender(cfg Config, stats *statsCounters, onFail func(error)) *sender {
	return &sender{
		bySeq:      make(map[uint32]*outPacket),
		cfg:        cfg,
		stats:      stats,
		onFail:     onFail,
		windowFree: make(chan struct{}, 1),
	}
}

// rto returns the current retransmission timeout. RFC 6298 §2.3: the
// estimator is srtt + 4*rttvar, clamped so a tiny RTT does not produce
// spurious retransmissions and a pathological one does not stall forever.
//
// The floor is deliberately small (MinRTO, default 5ms) rather than the 1s of
// RFC 6298 §2.4. That rule was written for a 1980s path where the only timing
// information was a coarse clock; here srtt and rttvar are measured per packet,
// so a 1s floor on a 50µs loopback path means every lost retransmission costs
// 20,000x the path delay. Measured at 5% loss on 512 KiB: 4 MiB/s with a 50ms
// floor versus 35 MiB/s with 5ms, same loss pattern, same retransmit logic.
// A spurious retransmission costs one duplicate datagram that the receiver
// suppresses; waiting out a needless RTO costs throughput for the whole
// transfer. Karn's rule keeps the estimate honest either way.
func (s *sender) rto() time.Duration {
	floor := s.cfg.MinRTO
	// Never trust a floor below a few timer ticks: the scan runs every RTTick,
	// so a shorter RTO would expire before the tick that is meant to notice it.
	if min := 4 * s.cfg.RTTick; floor < min {
		floor = min
	}
	if s.srtt == 0 {
		return s.cfg.InitialRTO
	}
	rto := s.srtt + 4*s.rttvar
	if rto < floor {
		rto = floor
	}
	if rto > s.cfg.MaxRTO {
		rto = s.cfg.MaxRTO
	}
	return rto
}

// observeRTT applies RFC 6298 §2 smoothing to one unambiguous sample.
//
// Karn's rule (RFC 6298 §3): samples from retransmitted datagrams are
// ambiguous, because the ACK may belong to either copy. Feeding them back
// makes the RTO grow exactly when the path is bad, which is the opposite of
// what the estimate is for — measured as a 30x throughput loss in the legacy
// implementation.
//
// Sampling only the most recently sent acknowledged packet (see handleAck) is
// the other half of keeping the estimate honest. An ACK covers a range of
// packets, and the older ones in that range may have waited in a queue; taking
// their age as the RTT reports queueing delay as propagation delay. Measured on
// loopback: minRTT 174µs, but a sample taken from the oldest packet in a
// 450-packet burst read 200ms, which then inflated the RTO to 222ms and made
// every subsequent loss cost a fifth of a second.
func (s *sender) observeRTT(sample time.Duration) {
	if sample <= 0 {
		return
	}
	s.lastRTT = sample
	if s.minRTT == 0 || sample < s.minRTT {
		s.minRTT = sample
	}
	if s.srtt == 0 {
		s.srtt = sample
		s.rttvar = sample / 2
		return
	}
	diff := s.srtt - sample
	if diff < 0 {
		diff = -diff
	}
	s.rttvar = (3*s.rttvar + diff) / 4
	s.srtt = (7*s.srtt + sample) / 8
}

// paceFor returns how long the caller should wait before sending one more
// datagram, so that a full window is spread over one round trip.
//
// The value is the per-datagram slot: minRTT divided by the window. With the
// default 1024-packet window and a 50µs loopback RTT that is 49ns — below the
// cost of the write itself, so on loopback pacing is a no-op and full speed is
// preserved. On a 20ms path it becomes 20µs, which still allows 50k datagrams
// per second, far above any real link's capacity. Pacing therefore only bites
// where it must: when the sender would otherwise outrun the path.
func (s *sender) paceFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paceLocked()
}

func (s *sender) paceLocked() time.Duration {
	rtt := s.minRTT
	if rtt == 0 {
		return 0
	}
	d := rtt / time.Duration(s.cfg.SendWindow)
	// Below a microsecond the spacing is indistinguishable from noise and the
	// timer would cost more than it saves.
	if d < time.Microsecond {
		return 0
	}
	return d
}

// minPaceSleep is the smallest pacing delay worth a timer wakeup.
//
// A per-datagram sleep is only cheap if the sleep is long. On loopback the
// computed slot is tens of microseconds, while a timer wakeup costs ~200µs, so
// sleeping per datagram makes the sender 5-10x slower than not pacing at all:
// measured 512 KiB at 5% loss as 112ms (4 MiB/s) with a per-packet timer versus
// 12ms (41 MiB/s) with the debt carried across packets. The pacing rate stays
// identical — the debt is still owed and is paid in one sleep once it is large
// enough — only the wakeup count drops.
const minPaceSleep = 250 * time.Microsecond

// waitPacing blocks until this sender may transmit again. It returns false if
// the connection ended while waiting.
//
// The schedule is absolute (nextSend) rather than a sleep per packet, so a slow
// write does not compound: if a write took longer than the slot, the next one
// goes immediately and the schedule catches up instead of drifting. A debt
// smaller than minPaceSleep is carried rather than slept off, which turns a
// per-packet timer into one wakeup per burst without changing the average rate.
func (s *sender) waitPacing(done <-chan struct{}) bool {
	for {
		s.mu.Lock()
		slot := s.paceLocked()
		if slot == 0 {
			s.mu.Unlock()
			return true
		}
		now := time.Now()
		if !now.Before(s.nextSend) {
			s.nextSend = now.Add(slot)
			s.mu.Unlock()
			return true
		}
		deficit := s.nextSend.Sub(now)
		s.nextSend = s.nextSend.Add(slot)
		s.mu.Unlock()

		if deficit < minPaceSleep {
			// The debt is below the cost of a wakeup. Carry it: the following
			// packets push nextSend further ahead until sleeping is worth it.
			return true
		}

		t := time.NewTimer(deficit)
		select {
		case <-done:
			t.Stop()
			return false
		case <-t.C:
		}
	}
}

// enqueueLocked appends a freshly sent packet. The caller holds s.mu.
//
// The timer is stamped here, not at the write. A packet that is in the queue but
// not yet stamped is indistinguishable from one whose timer expired 292 years
// ago (the zero time), and the timer tick can run between the enqueue and the
// write. That produced one or two spurious RTO retransmissions per transfer at
// zero loss, which is both wasted bandwidth and a false loss signal.
func (s *sender) enqueueLocked(p *outPacket) {
	if p.rto == 0 {
		p.sentAt = time.Now()
		p.rto = s.rto()
	}
	if s.tail == nil {
		s.head, s.tail = p, p
	} else {
		p.prev = s.tail
		s.tail.next = p
		s.tail = p
	}
	s.bySeq[p.seq] = p
	s.inFlight++
}

// removeLocked unlinks p and completes its message when it was the last
// outstanding fragment. The caller holds s.mu.
func (s *sender) removeLocked(p *outPacket) {
	if p.prev != nil {
		p.prev.next = p.next
	} else {
		s.head = p.next
	}
	if p.next != nil {
		p.next.prev = p.prev
	} else {
		s.tail = p.prev
	}
	delete(s.bySeq, p.seq)
	s.inFlight--
	p.prev, p.next = nil, nil

	if p.msg != nil {
		p.msg.remaining--
		if p.msg.remaining == 0 {
			close(p.msg.done)
		}
	}
}

// allocSeq reserves a datagram sequence number.
func (s *sender) allocSeq() uint32 {
	s.mu.Lock()
	seq := s.nextSeq
	s.nextSeq++
	s.mu.Unlock()
	return seq
}

// waitForWindow blocks until fewer than SendWindow packets are in flight. It
// returns false when the connection ends while waiting.
func (s *sender) waitForWindow(done <-chan struct{}) bool {
	for {
		s.mu.Lock()
		ok := s.inFlight < s.cfg.SendWindow
		s.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-done:
			return false
		case <-s.windowFree:
		}
	}
}

// signalSpace releases one waiter. Non-blocking: a single token is enough,
// because every waiter re-checks the counter after waking.
func (s *sender) signalSpace() {
	select {
	case s.windowFree <- struct{}{}:
	default:
	}
}

// ackResult is what handleAck asks the caller to put on the wire. The packets
// are returned rather than their buffers because the caller stamps sentAt and
// rto at transmission time, and it must update the same object the timer scan
// will later inspect.
type ackResult struct {
	retransmit []*outPacket
}

// handleAck applies one acknowledgement.
//
// una is exclusive: everything below it has arrived. The runs above una report
// exactly which higher datagrams arrived, so the sender can drop those and
// retransmit only the holes instead of waiting for a cumulative ACK to crawl
// past a loss.
func (s *sender) handleAck(ack ackBlock, now time.Time) ackResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	var res ackResult
	freed := false
	cumAcked := false

	// Cumulative part: a prefix of the queue, in order. Only the newest packet
	// in the prefix yields an RTT sample: the older ones may have sat in a
	// queue, and their age would report that queueing delay as path delay.
	var newestCum *outPacket
	for {
		p := s.head
		if p == nil || !seqBefore(p.seq, ack.una) {
			break
		}
		newestCum = p
		s.removeLocked(p)
		freed = true
		cumAcked = true
	}
	if newestCum != nil && !newestCum.retransmitted {
		s.observeRTT(now.Sub(newestCum.sentAt))
	}

	// Selective part: walk the reported runs and drop every packet they name.
	// The highest reported sequence is the most recently sent packet the ACK
	// covers, so it is the only one worth sampling for RTT — the others may have
	// queued behind it.
	var newestSack *outPacket
	highest, any := ack.highest()
	for i := 0; i < ack.n; i++ {
		first := ack.firstOf(i)
		last := ack.lastOf(i)
		for seq := first; ; seq++ {
			if p := s.bySeq[seq]; p != nil {
				newestSack = p
				s.removeLocked(p)
				freed = true
			}
			if seq == last {
				break
			}
		}
	}
	if newestSack != nil && !newestSack.retransmitted {
		s.observeRTT(now.Sub(newestSack.sentAt))
	}

	// A cumulative ACK also proves the datagram just below una arrived, and it
	// may be the highest sequence the peer has reported so far.
	if cumAcked && (!any || seqAfter(ack.una-1, highest)) {
		highest, any = ack.una-1, true
	}
	if any && (!s.haveAck || seqAfter(highest, s.largestAcked)) {
		s.largestAcked, s.haveAck = highest, true
		s.largestAckedAt = now
	}
	// Remember the cumulative point. A stale or reordered ACK must not move it
	// backwards.
	if s.haveAck && (!s.haveUna || seqAfter(ack.una, s.ackUna)) {
		s.ackUna, s.haveUna = ack.una, true
	}

	if lost := s.detectLossLocked(now); len(lost) > 0 {
		res.retransmit = append(res.retransmit, lost...)
		if s.failed {
			return res
		}
	}

	if freed {
		s.signalSpace()
	}
	return res
}

// detectLossLocked declares packets lost and returns the ones to retransmit.
//
// RFC 9002 §6.1 declares a packet lost when *either* holds:
//
//   - packet threshold: a packet at least fastRetransmitThreshold (3) below the
//     highest acknowledged sequence number is lost;
//   - time threshold: a packet outstanding longer than the reordering window
//     (9/8 of the smoothed RTT, floored at timerGranularity) is lost.
//
// The two are alternatives, not a conjunction. Requiring both is a trap for
// small messages: a four-fragment message that loses one fragment has only
// three later packets, so the packet threshold is met exactly at the boundary,
// and if the ACK for the third arrives before the reordering window elapses the
// fragment is never declared lost — it waits out a full RTO. Measured on 20
// four-fragment messages at 5% loss: every repair came from the RTO timer
// (rtoFires=4, fast=0); with the time threshold acting alone the same transfer
// recovers within one reordering window.
//
// The scan walks the unacknowledged queue from the head, which is short
// (bounded by SendWindow) and already in sequence order. There is no separate
// scan pointer: a pointer that only moves forward has to be rewound whenever a
// retransmission is itself lost, and getting that rewind wrong strands packets
// until the RTO timer.
//
// Truncation at maxAckRanges is safe because the block always states una
// outright: the head hole is reportable even when the runs are cut short, and a
// packet that a truncated block fails to mention is retransmitted only if the
// packet threshold or the reordering timer also fires — which is what the ACK
// was meant to establish anyway.
//
// This runs from two places: the ACK path, and the connection's timer tick.
// Running it only on ACKs is a throughput trap, not a saving. A Send bursts a
// whole message into the socket, so the last ACK for a window arrives within
// one RTT — often before the reordering window has elapsed. After that no ACK
// arrives at all (nothing is left to acknowledge), so every lost datagram in
// that burst waits out a full RTO.
func (s *sender) detectLossLocked(now time.Time) []*outPacket {
	if !s.haveAck || s.srtt == 0 || !s.haveUna {
		return nil
	}
	reorder := s.srtt * 9 / 8
	if reorder < timerGranularity {
		reorder = timerGranularity
	}

	var lost []*outPacket
	for p := s.head; p != nil; p = p.next {
		// Packet threshold: at least three later sequence numbers were
		// acknowledged. This is the fast path — it fires as soon as the peer
		// reports three arrivals above the hole, without waiting for a timer.
		byCount := !seqAfter(p.seq+fastRetransmitThreshold, s.largestAcked)
		// Time threshold: outstanding longer than the reordering window and known
		// to sit behind something the peer acknowledged. The second half is not
		// optional: a burst puts a whole window in flight in far less than a
		// reordering window, so "older than 9/8 RTT" alone describes most of a
		// healthy burst. Measured at zero loss: 25-38 spurious fast retransmits
		// per 512 KiB transfer without it, zero with it.
		byTime := now.Sub(p.sentAt) >= reorder && !seqAfter(p.seq, s.largestAcked)
		if !byCount && !byTime {
			continue
		}
		// A packet may be retransmitted by fast retransmit more than once, but not
		// more often than the retransmit throttle allows. The packet threshold is
		// monotone — largestAcked never moves backwards — so without a throttle
		// every ACK re-declares the same hole: measured as 404 retransmissions for
		// 478 fragments at 20% loss, against a geometric floor of ~120. A one-shot
		// limit is too strict in the other direction: it hands every second loss of
		// the same datagram to the RTO timer, and at 20% loss that is most of them.
		//
		// The throttle is derived from minRTT rather than srtt; see
		// retransmitThrottle for why that distinction matters.
		if p.retransmitted && now.Sub(p.sentAt) < s.retransmitThrottle() {
			continue
		}
		p.lost = true
		p.attempts++
		p.retransmitted = true
		p.sentAt = now
		if p.attempts > s.cfg.MaxRetries {
			s.failed = true
			s.failLocked()
			return lost
		}
		lost = append(lost, p)
	}
	return lost
}

// lossScan is the timer-side entry point for loss detection. It exists as a
// separate method so the tick loop does not need to know about the lock
// discipline inside the sender.
func (s *sender) lossScan(now time.Time) []*outPacket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.detectLossLocked(now)
}

// timeoutScan retransmits one expired packet and restarts the timers of the rest.
//
// Retransmitting every expired packet at once is the intuitive choice and it is
// wrong: the sender bursts a whole window, so packets share a send time and
// expire together. One stuck head then produces a mass retransmission of data
// that is merely queued behind it — measured as ~44 retransmissions per RTO fire
// and 823 for 454 fragments at 10% loss, which was most of the transfer.
//
// Only the head can unblock the cumulative ACK: una cannot advance past it, and
// while una is stuck no ACK can describe anything above it. So the head is
// retransmitted, its timer backs off, and every other expired timer is pushed
// forward without sending. The packets behind it are recovered by fast
// retransmit as soon as una advances, an RTT away instead of an RTO.
func (s *sender) timeoutScan(now time.Time) []*outPacket {
	s.mu.Lock()
	defer s.mu.Unlock()

	var head *outPacket
	for p := s.head; p != nil; p = p.next {
		if now.Sub(p.sentAt) >= p.rto {
			head = p
			break
		}
	}
	if head == nil {
		return nil
	}

	head.attempts++
	head.retransmitted = true
	head.lost = true
	head.sentAt = now
	// Back off from the current estimate, scaled by this packet's attempt count,
	// rather than doubling the stored value in place. The in-place form never
	// recovers: a packet that needed three RTOs carries a timer eight times the
	// path estimate for the rest of its life, and every later loss behind it
	// inherits that wait. Measured at 20% loss with in-place doubling: 3.0 seconds
	// for 512 KiB.
	head.rto = s.backoffN(s.rto(), head.attempts)
	if head.attempts > s.cfg.MaxRetries {
		s.failed = true
		s.failLocked()
		return nil
	}

	var resend []*outPacket
	resend = append(resend, head)
	for p := head.next; p != nil; p = p.next {
		if now.Sub(p.sentAt) < p.rto {
			continue
		}
		if !p.lost {
			// Not yet known to be lost: push the timer forward instead of
			// sending. It is most likely queued behind the head, and sending it
			// now would duplicate a datagram the peer is about to receive.
			p.sentAt = now
			continue
		}
		// Already declared lost at least once, so its first retransmission did
		// not arrive either. This one has to go out: fast retransmit will not
		// re-declare it while the cumulative ACK is stuck behind the head.
		p.attempts++
		p.retransmitted = true
		p.sentAt = now
		p.rto = s.backoffN(s.rto(), p.attempts)
		if p.attempts > s.cfg.MaxRetries {
			s.failed = true
			s.failLocked()
			return nil
		}
		resend = append(resend, p)
	}
	return resend
}

// backoffN returns base doubled n times, capped at MaxRTO. Deriving the timeout
// from the current estimate each time keeps it proportional to the path: a
// packet that has failed twice waits four times the estimate, and once the
// estimate improves (because other packets are being acknowledged) so does the
// wait.
// retransmitThrottle is how long a retransmitted packet must wait before fast
// retransmit may declare it lost again.
//
// It is derived from minRTT, not srtt. srtt grows under load, and load is exactly
// when retransmissions happen, so an srtt-derived throttle weakens as the path
// degrades — a positive feedback loop that showed up as 20%-loss transfers
// varying between 20ms and 92ms across runs. minRTT is the queue-free propagation
// delay, which does not move when the sender is busy, so the throttle stays a
// property of the path.
//
// A quarter of the RTO is the scale: long enough that the previous copy has had a
// fair chance to be acknowledged, short enough that recovery stays in
// fast-retransmit territory.
func (s *sender) retransmitThrottle() time.Duration {
	// Four propagation delays is the natural scale: the previous copy needs at
	// least a round trip to be acknowledged, and minRTT is the queue-free part of
	// that round trip.
	d := 4 * s.minRTT
	// Never shorter than a quarter of the current RTO. On a path whose minRTT is
	// tens of microseconds, 4*minRTT is far below the delay a retransmission
	// actually experiences (the receiver ACKs in batches), so the throttle would
	// re-declare the same packet before its copy could be reported — at 20% loss
	// that exhausted the retry budget outright.
	if quarter := s.rto() / 4; quarter > d {
		d = quarter
	}
	if d < timerGranularity {
		d = timerGranularity
	}
	return d
}

func (s *sender) backoffN(base time.Duration, n int) time.Duration {
	for i := 0; i < n; i++ {
		base *= 2
		if base >= s.cfg.MaxRTO {
			return s.cfg.MaxRTO
		}
	}
	return base
}

// failLocked reports an unrecoverable send failure. The connection must die:
// once a fragment has exhausted its retry budget, the message it belongs to
// can never be completed, and pretending otherwise is how the legacy layer
// delivered the wrong bytes.
func (s *sender) failLocked() {
	s.stats.incFailures()
	s.onFail(ErrPeerUnreachable)
}

// pending returns how many packets are in flight.
func (s *sender) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}
