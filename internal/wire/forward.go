// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"unicode/utf8"
)

// PreambleVersion is the forward preamble version. It is independent of
// Version: the forward sub-protocol can evolve without touching the frame
// format that send/recv depend on.
const PreambleVersion byte = 1

// Preamble kinds. The kind decides what Addr means, which is why it cannot be
// inferred from context.
const (
	// KindDirect means the opener asks the acceptor to dial Addr (-L).
	KindDirect byte = 1
	// KindReverse means the opener reports which of the acceptor's own -R
	// listeners fired. Addr is a listener ID, never a dial target.
	KindReverse byte = 2
)

// MaxPreambleAddr bounds the address field. 255 keeps the length in one byte
// and is far above any real host:port.
const MaxPreambleAddr = 255

// Preamble is the small header that starts every forwarded stream.
//
// It is read from an authenticated peer, but "authenticated" only means the
// peer knows the code; it does not mean the peer is trusted. Every field is
// validated as if it came from an attacker, because a leaked code makes it
// exactly that.
type Preamble struct {
	Version byte
	Kind    byte
	Addr    string
}

// ErrBadPreamble is the sentinel for every malformed preamble. Callers turn it
// into StatusBadRequest; the wrapped text says what was wrong.
var ErrBadPreamble = errors.New("wire: bad forward preamble")

// WritePreamble writes the header in one Write, so a stream never carries a
// half-written preamble if the write is interrupted.
func WritePreamble(w io.Writer, p Preamble) error {
	if p.Version == 0 {
		p.Version = PreambleVersion
	}
	if p.Version != PreambleVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadPreamble, p.Version)
	}
	if p.Kind != KindDirect && p.Kind != KindReverse {
		return fmt.Errorf("%w: unknown kind %d", ErrBadPreamble, p.Kind)
	}
	if len(p.Addr) == 0 || len(p.Addr) > MaxPreambleAddr {
		return fmt.Errorf("%w: address length %d out of range", ErrBadPreamble, len(p.Addr))
	}
	if err := validatePreambleAddr(p.Addr); err != nil {
		return err
	}
	buf := make([]byte, 3+len(p.Addr))
	buf[0] = p.Version
	buf[1] = p.Kind
	buf[2] = byte(len(p.Addr))
	copy(buf[3:], p.Addr)
	_, err := w.Write(buf)
	return err
}

// ReadPreamble parses and validates a preamble.
//
// The parse is strict on purpose: a lenient parser here would accept inputs the
// writer can never produce, which turns every future ambiguity into a
// compatibility trap. Rejecting is cheap because the caller answers
// StatusBadRequest and closes the stream.
func ReadPreamble(r io.Reader) (Preamble, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Preamble{}, fmt.Errorf("%w: reading header: %v", ErrBadPreamble, err)
	}
	p := Preamble{Version: hdr[0], Kind: hdr[1]}
	n := int(hdr[2])
	if p.Version != PreambleVersion {
		return Preamble{}, fmt.Errorf("%w: unsupported version %d", ErrBadPreamble, p.Version)
	}
	if p.Kind != KindDirect && p.Kind != KindReverse {
		return Preamble{}, fmt.Errorf("%w: unknown kind %d", ErrBadPreamble, p.Kind)
	}
	if n == 0 {
		return Preamble{}, fmt.Errorf("%w: empty address", ErrBadPreamble)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Preamble{}, fmt.Errorf("%w: reading address: %v", ErrBadPreamble, err)
	}
	p.Addr = string(buf)
	if err := validatePreambleAddr(p.Addr); err != nil {
		return Preamble{}, err
	}
	return p, nil
}

// validatePreambleAddr enforces the table in the design: non-empty, valid
// UTF-8, no control characters, and a real host:port with a usable port.
//
// The port checks matter beyond tidiness: port 0 makes dial mean "pick a
// random port", and an empty host makes ":22" resolve to any local address,
// which would let a peer reach a service the allow-list never named.
func validatePreambleAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("%w: empty address", ErrBadPreamble)
	}
	if len(addr) > MaxPreambleAddr {
		return fmt.Errorf("%w: address longer than %d bytes", ErrBadPreamble, MaxPreambleAddr)
	}
	if !utf8.ValidString(addr) {
		return fmt.Errorf("%w: address is not valid UTF-8", ErrBadPreamble)
	}
	for _, r := range addr {
		// Reject anything that could break a log line or a terminal. The
		// address is echoed in audit output, so control characters are an
		// injection vector into whatever reads those logs.
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: address contains a control character", ErrBadPreamble)
		}
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%w: address %q is not host:port", ErrBadPreamble, addr)
	}
	if host == "" {
		return fmt.Errorf("%w: empty host in %q", ErrBadPreamble, addr)
	}
	pn, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("%w: port %q is not a number", ErrBadPreamble, port)
	}
	if pn < 1 || pn > 65535 {
		return fmt.Errorf("%w: port %d out of range", ErrBadPreamble, pn)
	}
	return nil
}

// Status is the acceptor's answer to a preamble.
//
// Without it a refused connection looks identical to a dead target: the stream
// just closes. With it the operator sees "policy denied" versus "target did not
// answer", which is the difference between fixing a config and fixing a service.
type Status uint32

const (
	// StatusOK means the target is dialled and the stream is now a byte pipe.
	StatusOK Status = 0
	// StatusDenied means policy refused: no --accept, not in the allow-list, or
	// an unknown -R listener ID.
	StatusDenied Status = 1
	// StatusDialFail means policy allowed it but the target did not answer.
	StatusDialFail Status = 2
	// StatusTooMany means a stream or rate limit was hit.
	StatusTooMany Status = 3
	// StatusBadRequest means the preamble was malformed or carried an unknown
	// kind or version.
	StatusBadRequest Status = 4
	// StatusBindFail means a requested -R listener could not bind (port in use,
	// permission denied). It appears only in ForwardListenResult.
	StatusBindFail Status = 5
)

// String makes logs readable.
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusDenied:
		return "denied"
	case StatusDialFail:
		return "dial-failed"
	case StatusTooMany:
		return "too-many"
	case StatusBadRequest:
		return "bad-request"
	case StatusBindFail:
		return "bind-failed"
	default:
		return fmt.Sprintf("status(%d)", uint32(s))
	}
}

// MaxStatusReason bounds the operator-facing reason. Long reasons are truncated
// rather than rejected: a reason is diagnostic, and losing its tail is better
// than failing the handshake over it.
const MaxStatusReason = 200

// WriteStatus writes status(4 BE) followed by reasonLen(2 BE) and the reason.
func WriteStatus(w io.Writer, s Status, reason string) error {
	if len(reason) > MaxStatusReason {
		// Truncate on a rune boundary so the receiver never sees invalid UTF-8.
		cut := MaxStatusReason
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut]
	}
	buf := make([]byte, 6+len(reason))
	binary.BigEndian.PutUint32(buf[0:4], uint32(s))
	binary.BigEndian.PutUint16(buf[4:6], uint16(len(reason)))
	copy(buf[6:], reason)
	_, err := w.Write(buf)
	return err
}

// ReadStatus parses a status reply.
func ReadStatus(r io.Reader) (Status, string, error) {
	var hdr [6]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, "", fmt.Errorf("wire: reading status: %w", err)
	}
	code := Status(binary.BigEndian.Uint32(hdr[0:4]))
	n := int(binary.BigEndian.Uint16(hdr[4:6]))
	if n > MaxStatusReason {
		return 0, "", fmt.Errorf("wire: status reason too long: %d", n)
	}
	if code > StatusBindFail {
		return 0, "", fmt.Errorf("wire: unknown status %d", code)
	}
	if n == 0 {
		return code, "", nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, "", fmt.Errorf("wire: reading status reason: %w", err)
	}
	return code, string(buf), nil
}

// ForwardListenSpec is one -R listener the opener asks the acceptor to host.
type ForwardListenSpec struct {
	// ID is the opener's normalized bind address. The acceptor matches it
	// against its own -R configuration; it is never dialled.
	ID string `json:"id"`
	// Host and Port are the requested bind. The acceptor re-validates both.
	Host string `json:"host"`
	Port int    `json:"port"`
}

// ForwardListenStatus is the acceptor's answer for one spec.
type ForwardListenStatus struct {
	ID      string `json:"id"`
	Status  Status `json:"status"`
	Message string `json:"message,omitempty"`
}

// ValidateForwardListen checks one inbound spec. The acceptor applies this
// before binding anything: a spec arrives from a peer, so its host and port are
// attacker-controlled even though the peer passed the PAKE.
func ValidateForwardListen(s ForwardListenSpec) error {
	if s.ID == "" || len(s.ID) > MaxPreambleAddr {
		return fmt.Errorf("wire: listen spec id length %d out of range", len(s.ID))
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("wire: listen spec port %d out of range", s.Port)
	}
	if s.Host == "" {
		return errors.New("wire: listen spec has an empty host")
	}
	if _, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port))); err != nil {
		return fmt.Errorf("wire: listen spec %q:%d is not usable: %w", s.Host, s.Port, err)
	}
	return nil
}
