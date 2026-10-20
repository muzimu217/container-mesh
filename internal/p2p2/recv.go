// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"fmt"
	"sync"
)

// reassembly is one partially received message.
//
// It is stored under its msgID in receiver.reasm and never under a fragment
// index. That is the whole point of this package: the transport it replaces
// kept fragments in a single map keyed by fragment index, so fragments of
// message N+1 could occupy the holes of message N, and the "is it complete"
// test (len(map) == total) then passed for a mixture of two messages.
//
// buf is allocated once, at the exact size the sender announced, so a partial
// message holds msgLen bytes — not "however much the peer has sent so far".
// That makes the memory bound a property of the announced message, not of the
// peer's willingness to keep sending.
type reassembly struct {
	msgLen    uint32
	fragCount uint32
	buf       []byte
	have      []bool
	remain    int
}

// sackState tracks which datagram sequence numbers have arrived, in the form
// the peer consumes in one ACK: a cumulative "next missing" number plus a
// 64-bit bitmap.
//
// Internally the tracking is wider than the wire format on purpose. A 64-bit
// bitmap describes only 64 outstanding datagrams, while the send window allows
// 1024 in flight. If arrivals beyond next+63 were forgotten, the sender would
// never learn they arrived, keep them queued, and retransmit them on every RTO
// until the cumulative number crawled past. Measured with the 64-bit-only
// version: 788 retransmissions for 454 fragments at 1% loss, 0.35 MiB/s.
//
// So the receiver remembers every arrival inside the window in a ring bitmap
// and exports the 64 bits the protocol carries. Nothing is lost: when next
// advances, the newly covered range is already known.
type sackState struct {
	next uint32
	// bits is a ring indexed by seq % sackRingBits. A bit is set exactly while
	// its sequence is inside [next, next+sackRingBits) and has arrived.
	bits []uint64
}

// sackRingBits must be a power of two and comfortably larger than the send
// window, otherwise a datagram at the far edge of the window would fall outside
// the ring and its arrival could not be recorded — the sender would then never
// learn it arrived. It must also be larger than the range an ACK can describe,
// or an arrival could be forgotten before it ever had a chance to be reported.
//
// 4096 = 4x the default 1024-packet window. The extra room means an arrival is
// still remembered while the cumulative point catches up across a full window of
// reordering, which is what a 20%-loss path produces.
const sackRingBits = 4096

func newSackState() sackState {
	return sackState{bits: make([]uint64, sackRingBits/64)}
}

func (s *sackState) get(seq uint32) bool {
	i := seq & (sackRingBits - 1)
	return s.bits[i/64]&(1<<(i%64)) != 0
}

func (s *sackState) set(seq uint32) {
	i := seq & (sackRingBits - 1)
	s.bits[i/64] |= 1 << (i % 64)
}

func (s *sackState) clear(seq uint32) {
	i := seq & (sackRingBits - 1)
	s.bits[i/64] &^= 1 << (i % 64)
}

// mark records one arrival and advances the cumulative part over every
// contiguous sequence that is now present.
//
// Clearing each bit as it is passed keeps the ring correct across wrap-around:
// a stale bit at slot seq%N would otherwise claim that a sequence one full ring
// later had arrived.
func (s *sackState) mark(seq uint32) {
	// Accept the next expected sequence itself and anything ahead of it.
	// Anything behind next is already covered by the cumulative number, and
	// recording it would leave a stale bit that, one full ring later, would
	// claim a different sequence had arrived.
	if seq != s.next && !seqAfter(seq, s.next) {
		return
	}
	if seq-s.next >= sackRingBits {
		return // beyond the window the sender can have in flight
	}
	s.set(seq)
	for s.get(s.next) {
		s.clear(s.next)
		s.next++
	}
}

// snapshot returns the wire form: the cumulative point plus the highest runs of
// received sequence numbers above it.
//
// Runs are built from the top of the ring downward and stop at maxAckRanges.
// Keeping the highest runs is deliberate: loss detection is driven by what the
// peer received *above* a hole, so the top of the range is the part that must
// survive truncation. The lowest run that fits becomes the block's floor, and
// the sender is forbidden from declaring anything below it lost — truncation can
// cost a slower repair, never a spurious retransmission.
//
// The scan is bounded by the ring (sackRingBits) rather than by the send window,
// so its cost is constant. It allocates nothing.
func (s *sackState) snapshot() ackBlock {
	a := ackBlock{una: s.next, floor: s.next}

	// firstAbove is the low end of the previous run, used as the reference for
	// the next gap. For the first run the reference is una itself.
	var firstAbove uint32

	// Walk down from the highest sequence the ring can hold.
	seq := s.next + sackRingBits - 1
	for a.n < maxAckRanges && seqAfter(seq, s.next) {
		if !s.get(seq) {
			seq--
			continue
		}

		last := seq
		for seqAfter(seq, s.next) && s.get(seq-1) {
			seq--
		}
		first := seq

		var gap uint32
		if a.n == 0 {
			gap = first - s.next
		} else {
			gap = firstAbove - 1 - first
		}
		length := last - first
		if gap > 0xFFFF || length > 0xFFFF {
			// Cannot be encoded. Ending the report here is the safe choice: the
			// block claims less than it knows, and floor says where it stops.
			break
		}
		a.ranges[a.n] = ackRange{gap: uint16(gap), length: uint16(length)}
		a.n++
		firstAbove = first

		if !seqAfter(first, s.next) {
			break
		}
		seq = first - 1
	}
	// floor is una, always: the block states outright that una has not arrived,
	// and everything above una is either described by a run or known to be
	// missing. Truncation only stops the *runs* from describing the far end; it
	// does not make the near end uncertain, because a run is a set of arrivals
	// and the absence of a run is not evidence either way — but una itself is.
	//
	// Getting this wrong is a silent throughput hole: treating the lowest
	// reported run as the floor makes the head hole (the one una names) look
	// "unreported", so it is never declared lost and the transfer stalls until an
	// RTO. Measured on 512 KiB at 5% loss: fast retransmits dropped to zero and
	// the connection exhausted its retry budget.
	a.floor = a.una
	return a
}

// recvOutcome is what one data datagram produced. The ACK policy depends on it,
// so it is returned rather than inferred by the caller.
type recvOutcome struct {
	// duplicate means the datagram (or its sequence number) had already been
	// accounted for. It still requires an immediate ACK: the only reason a
	// duplicate arrives is that our previous ACK was lost, and staying silent
	// would make the sender retransmit until its retry budget ran out. RFC 5681
	// §4.2 requires an immediate duplicate ACK for the same reason.
	duplicate bool
	// delivered is how many messages became complete and were queued.
	delivered int
}

// receiver owns everything that turns datagrams into ordered messages.
type receiver struct {
	connID uint64

	mu         sync.Mutex
	sack       sackState
	nextMsgID  uint32
	reasm      map[uint32]*reassembly
	reasmBytes int

	recvWindow    int
	maxReasmBytes int

	deliver func([]byte)

	delivered uint64
	dropped   uint64
}

func newReceiver(connID uint64, recvWindow, maxReasmBytes int, deliver func([]byte)) *receiver {
	if recvWindow <= 0 {
		recvWindow = DefaultConfig().RecvWindow
	}
	if maxReasmBytes <= 0 {
		maxReasmBytes = DefaultConfig().MaxReasmBytes
	}
	return &receiver{
		connID:        connID,
		sack:          newSackState(),
		reasm:         make(map[uint32]*reassembly),
		recvWindow:    recvWindow,
		maxReasmBytes: maxReasmBytes,
		deliver:       deliver,
	}
}

// ackSnapshot returns the acknowledgement state for an outgoing datagram.
//
// The result is a value, not a slice into receiver state: the caller patches it
// into a datagram under its own lock, and a shared backing array would be
// rewritten while another goroutine was encoding it.
func (r *receiver) ackSnapshot() ackBlock {
	r.mu.Lock()
	a := r.sack.snapshot()
	r.mu.Unlock()
	return a
}

// handleData consumes one data datagram and delivers any messages it completes.
//
// The error return is reserved for a peer that contradicts its own headers: a
// message whose fragments cannot add up to the announced length is not a
// recoverable condition, because continuing would mean handing the layer above
// a message whose bytes do not correspond to what was sent.
func (r *receiver) handleData(p packet) (recvOutcome, error) {
	r.mu.Lock()
	before := r.sack.next
	// A sequence number at or behind the cumulative point has already been
	// accounted for. This is the receiver's view of "duplicate", and it is
	// checked here because sackState deliberately forgets everything behind
	// next.
	dupSeq := p.seq != before && !seqAfter(p.seq, before)
	msgs, err := r.handleDataLocked(p)
	out := recvOutcome{
		duplicate: dupSeq,
		delivered: len(msgs),
	}
	r.mu.Unlock()

	for _, m := range msgs {
		r.deliver(m)
	}
	return out, err
}

func (r *receiver) handleDataLocked(p packet) ([][]byte, error) {
	// Record the arrival before validating the payload. A datagram whose body
	// we cannot use still proves that the datagram was not lost, and the sender
	// must be told so, otherwise it retransmits data the peer already holds.
	r.sack.mark(p.seq)

	if !r.validFragment(p) {
		r.dropped++
		return nil, nil
	}

	// Message identity decides everything from here on. A message below the
	// delivery point is a duplicate (or a very late fragment) and is discarded
	// without touching the reassembly table.
	if seqBefore(p.msgID, r.nextMsgID) {
		r.dropped++
		return nil, nil
	}
	// A message too far ahead is outside the receive window. Bounding this is
	// what keeps the reassembly table finite: it can hold at most recvWindow
	// entries, whatever the peer sends.
	if uint32(p.msgID-r.nextMsgID) >= uint32(r.recvWindow) {
		r.dropped++
		return nil, nil
	}

	ra := r.reasm[p.msgID]
	if ra == nil {
		if r.reasmBytes+int(p.msgLen) > r.maxReasmBytes {
			// The byte cap is a second, independent bound: a window's worth of
			// maximum-size messages would otherwise be a multi-gigabyte budget.
			r.dropped++
			return nil, nil
		}
		ra = &reassembly{
			msgLen:    p.msgLen,
			fragCount: p.fragCount,
			buf:       make([]byte, p.msgLen),
			have:      make([]bool, p.fragCount),
			remain:    int(p.fragCount),
		}
		r.reasm[p.msgID] = ra
		r.reasmBytes += int(p.msgLen)
	} else if ra.fragCount != p.fragCount || ra.msgLen != p.msgLen {
		// The same message ID must describe the same message on every fragment.
		// Accepting a different shape here would let a second sender (or a
		// confused one) redefine an existing slot, which is the class of bug
		// this package exists to make impossible.
		r.dropped++
		return nil, nil
	}

	if ra.have[p.fragIndex] {
		r.dropped++
		return nil, nil
	}
	ra.have[p.fragIndex] = true
	copy(ra.buf[int(p.fragIndex)*MaxPayload:], p.payload)
	ra.remain--

	// Deliver strictly in message order. A complete message above a hole waits:
	// the layer above decrypts with a monotonically increasing counter, so
	// delivering out of order is a hard protocol error there, not a latency
	// trade-off.
	var msgs [][]byte
	for {
		next := r.reasm[r.nextMsgID]
		if next == nil || next.remain != 0 {
			break
		}
		delete(r.reasm, r.nextMsgID)
		r.reasmBytes -= int(next.msgLen)
		r.nextMsgID++
		r.delivered++
		msgs = append(msgs, next.buf)
	}
	return msgs, nil
}

// validFragment checks every header field before anything is allocated.
//
// fragCount is not trusted: it must equal the count implied by msgLen, and each
// fragment's payload must be exactly the slice of the message its index names.
// Enforcing the shape here means the copy into buf can never leave a hole, so a
// completed message is byte-identical to what the sender fragmented — no
// separate "did every byte arrive" check is needed at assembly time.
func (r *receiver) validFragment(p packet) bool {
	if p.fragCount == 0 || p.fragCount > maxFragments {
		return false
	}
	if p.fragIndex >= p.fragCount {
		return false
	}
	if p.msgLen == 0 || p.msgLen > MaxMessageSize {
		return false
	}
	want := (int(p.msgLen) + MaxPayload - 1) / MaxPayload
	if int(p.fragCount) != want {
		return false
	}
	wantLen := MaxPayload
	if p.fragIndex == p.fragCount-1 {
		wantLen = int(p.msgLen) - int(p.fragCount-1)*MaxPayload
	}
	return len(p.payload) == wantLen
}

// stats reports receive-side counters.
func (r *receiver) stats() (delivered, dropped uint64, entries, bytes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.delivered, r.dropped, len(r.reasm), r.reasmBytes
}

// maxMessageError explains an oversized Send.
func maxMessageError(n int) error {
	return fmt.Errorf("p2p2: message of %d bytes exceeds the %d byte limit", n, MaxMessageSize)
}
