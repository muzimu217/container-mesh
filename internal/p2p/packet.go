// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package p2p gives two peers a direct UDP path when their NATs allow it, and
// falls back to the relay when they do not.
//
// Why this exists: every byte currently travels through the relay, so the
// latency floor is two trips across the internet (peer → relay → peer) even
// when both peers sit on the same LAN. A direct path removes the relay from
// the data plane entirely; the relay is still used for rendezvous (exchanging
// candidates) and remains the fallback.
//
// Layering:
//
//	p2p.Conn  reliable, ordered, message-oriented transport over UDP
//	  ↑
//	peer.Session (SPAKE2 + AES-GCM) — unchanged, it just gets a different
//	  MessageConn implementation
//
// The reliability layer is deliberately small: it carries frames of at most
// 1 MiB, which the peer layer already chunks at 512 KiB. It is not a general
// purpose transport (no multiplexing, no connection migration).
package p2p

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// magic0/magic1 identify our datagrams. A stray packet on the port (or a
	// port scan) is dropped before any allocation happens.
	magic0 byte = 0xC5
	magic1 byte = 0x01

	version = 1

	// MaxDatagram is the largest UDP payload we emit. 1200 bytes stays under
	// the smallest MTU commonly encountered (PPPoE 1492, some VPNs 1280), so
	// we avoid IP fragmentation, which is both slow and frequently dropped.
	MaxDatagram = 1200

	// headerSize is magic(2) + version(1) + type(1) + connID(8) + seq(4) +
	// ack(4) + flags(1) + length(2).
	headerSize = 23

	// MaxPayload is the usable payload per datagram.
	MaxPayload = MaxDatagram - headerSize
)

// packetType enumerates datagram kinds.
type packetType uint8

const (
	// pktPunch opens a NAT binding. It carries no data and is answered with
	// pktPunchAck.
	pktPunch packetType = 1
	// pktPunchAck answers a pktPunch, proving the reverse direction works.
	pktPunchAck packetType = 2
	// pktData carries one fragment of a message.
	pktData packetType = 3
	// pktAck acknowledges everything below the sequence number in its seq
	// field (cumulative, like TCP).
	pktAck packetType = 4
	// pktPing/pktPong keep the path warm and detect a dead one.
	pktPing packetType = 5
	pktPong packetType = 6
	// pktFin closes the connection.
	pktFin packetType = 7
)

func (t packetType) String() string {
	switch t {
	case pktPunch:
		return "punch"
	case pktPunchAck:
		return "punch-ack"
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

// Fragment flags for pktData.
const (
	flagFirst uint8 = 1 << 0
	flagLast  uint8 = 1 << 1
)

// packet is one datagram.
type packet struct {
	typ     packetType
	connID  uint64
	seq     uint32
	ack     uint32
	flags   uint8
	payload []byte
}

// ErrMalformed is returned for datagrams that do not parse.
var ErrMalformed = errors.New("p2p: malformed packet")

// encode writes p into a fresh datagram.
func (p *packet) encode() ([]byte, error) {
	if len(p.payload) > MaxPayload {
		return nil, fmt.Errorf("p2p: payload %d exceeds %d", len(p.payload), MaxPayload)
	}
	b := make([]byte, headerSize+len(p.payload))
	b[0] = magic0
	b[1] = magic1
	b[2] = version
	b[3] = byte(p.typ)
	binary.BigEndian.PutUint64(b[4:12], p.connID)
	binary.BigEndian.PutUint32(b[12:16], p.seq)
	binary.BigEndian.PutUint32(b[16:20], p.ack)
	b[20] = p.flags
	binary.BigEndian.PutUint16(b[21:23], uint16(len(p.payload)))
	copy(b[headerSize:], p.payload)
	return b, nil
}

// decode parses a datagram. It does not copy the payload; the caller must not
// reuse the buffer.
func decode(b []byte) (*packet, error) {
	if len(b) < headerSize {
		return nil, ErrMalformed
	}
	if b[0] != magic0 || b[1] != magic1 {
		return nil, ErrMalformed
	}
	if b[2] != version {
		return nil, fmt.Errorf("p2p: unsupported version %d", b[2])
	}
	n := int(binary.BigEndian.Uint16(b[21:23]))
	if headerSize+n != len(b) {
		return nil, ErrMalformed
	}
	return &packet{
		typ:     packetType(b[3]),
		connID:  binary.BigEndian.Uint64(b[4:12]),
		seq:     binary.BigEndian.Uint32(b[12:16]),
		ack:     binary.BigEndian.Uint32(b[16:20]),
		flags:   b[20],
		payload: b[headerSize:],
	}, nil
}
