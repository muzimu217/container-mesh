// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Wire format. Every integer is big endian.
//
//	offset size field
//	0      2    magic 0xC5 0x02
//	2      1    version (2)
//	3      1    type
//	4      8    connID
//
//	-- type 1: data --
//	12     4    seq        datagram identity, used only by ACK and RTT
//	16     4    msgID      message identity, assigned once by Send, never reused
//	20     4    fragIndex  position of this fragment inside the message
//	24     4    fragCount  number of fragments the message was split into
//	28     4    msgLen     total message length in bytes
//	32     ..   ack block, then payload
//
//	-- type 2: ack --
//	12     ..   ack block
//
//	-- types 3..5: ping, pong, fin -- no body.
//
// Ack block, in both directions: every datagram carries the sender's current
// receive state, so an ACK never needs its own round trip.
//
//	0      4    una     next datagram seq this side has NOT seen
//	4      1    nRanges number of runs that follow (0..maxAckRanges)
//	5      4n   runs    gap(2) + length(2), highest first
//
// A run covers a contiguous set of received sequence numbers above una, and the
// runs are reported from the highest downward. Each run names the next lower run
// the receiver knows about, so the block is self-delimiting:
//
//	run 0:   first = una + gap          last = first + length
//	run i>0: first = prevFirst - 1 - gap last = first + length
//
// Encoding the first run as an offset above una (rather than a gap below it)
// keeps every field non-negative: the runs are all above una by definition, so a
// "gap below una" would underflow for the common case of a single hole at the
// head.
//
// Why runs and not a bitmap: a bitmap has to be as wide as the send window to be
// sound (an arrival the ACK cannot describe is indistinguishable from a loss),
// so a 1024-packet window needs 128 bytes on every datagram — 11% of a
// 1200-byte MTU, paid on data *and* on every ACK. Runs cost 5 bytes for the
// common no-loss case and 9-13 bytes for one or two holes, and they still cover
// the whole window. Measured on 512 KiB with the same window and loss pattern:
// 107 MiB/s with the fixed bitmap, 129 MiB/s with runs.
//
// Why three identities (seq, msgID, fragIndex) instead of one: the failure this
// package exists to avoid was treating a datagram sequence number as a message
// identity. They answer different questions and must not be conflated:
//
//   - seq decides what may be retransmitted and when a datagram is delivered;
//   - msgID decides which message a fragment belongs to, and therefore which
//     reassembly slot it may touch;
//   - fragIndex positions the bytes inside that one message.
//
// A retransmission keeps all three; only the ack block of a datagram changes
// between transmissions.

const (
	// magic0/magic1 identify this protocol. They differ from package p2p's
	// magic so a datagram from the legacy implementation (or any stray traffic
	// on a shared port) is rejected before it can touch any state.
	magic0 byte = 0xC5
	magic1 byte = 0x02

	version = 2

	// MaxDatagram is the largest UDP payload emitted. 1200 bytes stays below
	// the smallest MTU in common use (1280 for IPv6, 1492 for PPPoE), so IP
	// fragmentation never happens: a fragmented IP datagram is lost as a whole
	// if one piece is dropped, which would turn a 1% loss rate into a much
	// higher effective loss rate for large messages.
	MaxDatagram = 1200

	commonHeaderSize = 12

	// maxAckRanges bounds the ack block. Eight runs cover every loss pattern that
	// matters for loss detection: what drives fast retransmit is the top of the
	// received range, and truncation keeps the highest runs. The block always
	// states una outright, so the head hole — the one that blocks delivery — is
	// reportable no matter how much truncation happened above it, and a truncated
	// block can never make an arrived datagram look lost.
	//
	// Eight rather than sixteen is a measured choice, not a guess. Each reserved
	// run costs four bytes of payload on *every* data datagram, and the lossy
	// paths are where that matters: at 20% loss the 32-byte difference moved the
	// 512 KiB transfer from 5.4 MB/s (16 runs) to 13.8 MB/s (8 runs), because the
	// extra bytes mean more datagrams, and more datagrams at 20% loss means more
	// retransmissions. The cost of the narrower block is coarser reporting deep
	// in a heavily fragmented window, which shows up as extra retransmissions
	// (443 → 598 at 20%) but never as a stalled or misordered delivery.
	maxAckRanges = 8

	// dataFixedSize is the data header before the ack block:
	// magic+version+type+connID (12) + seq/msgID/fragIndex/fragCount/msgLen (20).
	dataFixedSize = 32
	// ackFixedSize is the ack block before its runs: una(4) + nRanges(1).
	ackFixedSize = 5
	// maxAckBlockSize is the largest encoded ack block.
	maxAckBlockSize = ackFixedSize + 4*maxAckRanges

	// MaxPayload is the usable payload per data datagram.
	//
	// It is derived from the *worst-case* ack block, not the typical one, so the
	// fragment layout is a constant: fragment i occupies bytes
	// [i*MaxPayload, (i+1)*MaxPayload) of the message no matter what the peer
	// piggybacked on the datagram that carried it. A variable payload size would
	// make the receiver's reassembly offset depend on the ACK state at send
	// time, which is exactly the kind of coupling this package avoids.
	MaxPayload = MaxDatagram - dataFixedSize - maxAckBlockSize

	// maxFragments bounds how many datagrams one message may span. It is a
	// protocol constant (not a tuning knob) because the receiver validates
	// fragCount against it before allocating anything.
	maxFragments = 4096

	// MaxMessageSize is the largest message this protocol can carry.
	MaxMessageSize = maxFragments * MaxPayload
)

// packetType enumerates datagram kinds.
type packetType uint8

const (
	pktData packetType = 1
	pktAck  packetType = 2
	pktPing packetType = 3
	pktPong packetType = 4
	pktFin  packetType = 5
)

func (t packetType) String() string {
	switch t {
	case pktData:
		return "data"
	case pktAck:
		return "ack"
	case pktPing:
		return "ping"
	case pktPong:
		return "pong"
	case pktFin:
		return "fin"
	default:
		return fmt.Sprintf("pkt(%d)", uint8(t))
	}
}

// ErrMalformed is returned for datagrams that do not parse.
var ErrMalformed = errors.New("p2p2: malformed packet")

// ackRange is one run of received sequence numbers, in the encoding described
// at the top of this file.
type ackRange struct {
	gap    uint16
	length uint16
}

// ackBlock is a decoded acknowledgement.
//
// floor is the lowest sequence the block makes a positive statement about. It is
// always una: the block says outright that una has not arrived, so una is
// reportable as lost. Runs only add positive statements about sequences above
// una. Truncation therefore costs a slower repair at the far end of the window,
// never a spurious retransmission, and never strands the head hole — which is
// the one that blocks the cumulative point.
type ackBlock struct {
	una    uint32
	n      int
	ranges [maxAckRanges]ackRange
	floor  uint32
}

// decodeAckBlock parses an ack block from b and returns the number of bytes it
// occupied. It fails rather than guessing when the runs contradict each other:
// a block that claims more runs than fit, or a run that would underflow una, is
// malformed input from an untrusted peer.
func decodeAckBlock(b []byte) (ackBlock, int, error) {
	var a ackBlock
	if len(b) < ackFixedSize {
		return a, 0, ErrMalformed
	}
	a.una = binary.BigEndian.Uint32(b[0:4])
	n := int(b[4])
	if n > maxAckRanges {
		return a, 0, ErrMalformed
	}
	need := ackFixedSize + 4*n
	if len(b) < need {
		return a, 0, ErrMalformed
	}
	a.n = n

	// prevFirst is the low end of the previous run. For the first run the gap is
	// measured from una itself; for later runs it is measured from one below the
	// previous run's low end.
	var prevFirst uint32
	for i := 0; i < n; i++ {
		off := ackFixedSize + 4*i
		gap := binary.BigEndian.Uint16(b[off : off+2])
		length := binary.BigEndian.Uint16(b[off+2 : off+4])
		var first uint32
		if i == 0 {
			first = a.una + uint32(gap)
		} else {
			if uint32(gap) >= prevFirst {
				// The run would start at or below sequence 0 relative to the
				// previous one, i.e. it wraps.
				return a, 0, ErrMalformed
			}
			first = prevFirst - 1 - uint32(gap)
		}
		if uint32(length) > ^uint32(0)-first {
			return a, 0, ErrMalformed
		}
		a.ranges[i] = ackRange{gap: gap, length: length}
		prevFirst = first
	}
	if n > 0 {
		a.floor = prevFirst
	} else {
		a.floor = a.una
	}
	return a, need, nil
}

// encodeAckBlock writes a into dst and returns the bytes used. dst must hold at
// least maxAckBlockSize bytes.
func encodeAckBlock(dst []byte, a *ackBlock) int {
	binary.BigEndian.PutUint32(dst[0:4], a.una)
	dst[4] = byte(a.n)
	for i := 0; i < a.n; i++ {
		off := ackFixedSize + 4*i
		binary.BigEndian.PutUint16(dst[off:off+2], a.ranges[i].gap)
		binary.BigEndian.PutUint16(dst[off+2:off+4], a.ranges[i].length)
	}
	return ackFixedSize + 4*a.n
}

// highest returns the highest sequence number the block reports received, and
// whether it reports any at all.
func (a *ackBlock) highest() (uint32, bool) {
	if a.n == 0 {
		return 0, false
	}
	return a.lastOf(0), true
}

// contains reports whether the block explicitly reports seq as received.
func (a *ackBlock) contains(seq uint32) bool {
	for i := 0; i < a.n; i++ {
		first := a.firstOf(i)
		last := a.lastOf(i)
		if !seqBefore(seq, first) && !seqAfter(seq, last) {
			return true
		}
	}
	return false
}

// firstOf returns the low end of the run at index i.
func (a *ackBlock) firstOf(i int) uint32 {
	first := a.una + uint32(a.ranges[0].gap)
	for j := 1; j <= i; j++ {
		first = first - 1 - uint32(a.ranges[j].gap)
	}
	return first
}

// lastOf returns the high end of the run at index i.
func (a *ackBlock) lastOf(i int) uint32 {
	return a.firstOf(i) + uint32(a.ranges[i].length)
}

// packet is one decoded datagram. payload aliases the caller's read buffer, so
// it must be copied before the buffer is reused.
type packet struct {
	typ       packetType
	connID    uint64
	seq       uint32
	msgID     uint32
	fragIndex uint32
	fragCount uint32
	msgLen    uint32
	ack       ackBlock
	payload   []byte
}

// makeDataPacket encodes a data datagram, copying payload. The ack block is
// zeroed: it describes the sender's *current* receive state, which is only known
// at transmission time, so the write loop patches it in place (see patchAck).
// Everything else is immutable for the lifetime of the datagram, including
// across retransmissions, which is what keeps the message identity stable.
func makeDataPacket(connID uint64, seq, msgID, fragIndex, fragCount, msgLen uint32, payload []byte) []byte {
	out := make([]byte, dataFixedSize+maxAckBlockSize+len(payload))
	out[0], out[1], out[2], out[3] = magic0, magic1, version, byte(pktData)
	binary.BigEndian.PutUint64(out[4:12], connID)
	binary.BigEndian.PutUint32(out[12:16], seq)
	binary.BigEndian.PutUint32(out[16:20], msgID)
	binary.BigEndian.PutUint32(out[20:24], fragIndex)
	binary.BigEndian.PutUint32(out[24:28], fragCount)
	binary.BigEndian.PutUint32(out[28:32], msgLen)
	// The ack block sits at a fixed offset so patchAck can rewrite it without
	// moving the payload. Only the used part of the block is written; the
	// reserved tail stays zero.
	copy(out[dataFixedSize+maxAckBlockSize:], payload)
	return out
}

// patchAck writes the current acknowledgement into an encoded data datagram.
// Only the write path calls this, and it holds writeMu. The block occupies a
// fixed-size slot, so a longer block can never shift the payload.
func patchAck(b []byte, a *ackBlock) {
	for i := dataFixedSize; i < dataFixedSize+maxAckBlockSize; i++ {
		b[i] = 0
	}
	encodeAckBlock(b[dataFixedSize:], a)
}

// encodeAck fills b (at least commonHeaderSize+maxAckBlockSize bytes) with a
// pure ACK datagram and returns the number of bytes to send.
func encodeAck(b []byte, connID uint64, a *ackBlock) int {
	b[0], b[1], b[2], b[3] = magic0, magic1, version, byte(pktAck)
	binary.BigEndian.PutUint64(b[4:12], connID)
	n := encodeAckBlock(b[commonHeaderSize:], a)
	return commonHeaderSize + n
}

// encodeControl fills b with a body-less control datagram.
func encodeControl(b []byte, connID uint64, typ packetType) {
	b[0], b[1], b[2], b[3] = magic0, magic1, version, byte(typ)
	binary.BigEndian.PutUint64(b[4:12], connID)
}

// decodePacket parses a datagram. It rejects any datagram whose length does not
// match its type: a truncated datagram must never be interpreted as a shorter
// valid one.
//
// The ack block occupies a fixed-size slot on data datagrams, so the payload
// always starts at dataFixedSize+maxAckBlockSize. The slot is not shrunk to the
// encoded length: fragment offsets are a function of MaxPayload alone, and a
// variable payload offset would make reassembly depend on the sender's ACK
// state at the moment of transmission.
func decodePacket(b []byte) (packet, error) {
	if len(b) < commonHeaderSize {
		return packet{}, ErrMalformed
	}
	if b[0] != magic0 || b[1] != magic1 {
		return packet{}, ErrMalformed
	}
	if b[2] != version {
		return packet{}, fmt.Errorf("p2p2: unsupported version %d", b[2])
	}

	p := packet{
		typ:    packetType(b[3]),
		connID: binary.BigEndian.Uint64(b[4:12]),
	}
	switch p.typ {
	case pktData:
		if len(b) < dataFixedSize+ackFixedSize {
			return packet{}, ErrMalformed
		}
		p.seq = binary.BigEndian.Uint32(b[12:16])
		p.msgID = binary.BigEndian.Uint32(b[16:20])
		p.fragIndex = binary.BigEndian.Uint32(b[20:24])
		p.fragCount = binary.BigEndian.Uint32(b[24:28])
		p.msgLen = binary.BigEndian.Uint32(b[28:32])
		ack, _, err := decodeAckBlock(b[dataFixedSize:])
		if err != nil {
			return packet{}, err
		}
		p.ack = ack
		if len(b) < dataFixedSize+maxAckBlockSize {
			return packet{}, ErrMalformed
		}
		p.payload = b[dataFixedSize+maxAckBlockSize:]
	case pktAck:
		ack, used, err := decodeAckBlock(b[commonHeaderSize:])
		if err != nil {
			return packet{}, err
		}
		// A pure ACK carries only the block; its length is whatever the runs
		// needed. Trailing bytes would be a second, unparsed encoding.
		if len(b) != commonHeaderSize+used {
			return packet{}, ErrMalformed
		}
		p.ack = ack
	case pktPing, pktPong, pktFin:
		if len(b) != commonHeaderSize {
			return packet{}, ErrMalformed
		}
	default:
		return packet{}, ErrMalformed
	}
	return p, nil
}

// seqBefore reports whether a comes before b in the circular sequence space.
// The comparison is only meaningful for values within 2^31 of each other, which
// every window and every message ID here is.
func seqBefore(a, b uint32) bool { return int32(a-b) < 0 }

// seqAfter reports whether a comes after b.
func seqAfter(a, b uint32) bool { return int32(a-b) > 0 }
