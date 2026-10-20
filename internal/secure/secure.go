// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package secure provides the authenticated, encrypted channel that sits on
// top of a raw message transport.
//
// Threat model
//
//   - The relay (and anyone who can read its traffic) is untrusted. It may
//     drop, delay, reorder or inject messages.
//   - The two peers share a short human-transcribable code out of band.
//   - An attacker who does not know the code must not be able to (a) complete
//     the handshake, or (b) mount an offline dictionary attack on recorded
//     traffic.
//
// # Design
//
// The handshake is SPAKE2 (RFC 9382 style) via github.com/schollz/pake/v3.
// That gives an explicit-authentication key exchange: only a party that knows
// the code can derive the same session key, and a passive recorder learns
// nothing usable for offline guessing.
//
// After the handshake the session key is expanded with HKDF-SHA256 into two
// independent AES-256-GCM keys, one per direction. Every frame is sealed with
// a strictly increasing 64-bit counter nonce; receivers reject out-of-order or
// replayed counters.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/schollz/pake/v3"
	"golang.org/x/crypto/hkdf"

	"github.com/muzimu217/container-mesh/internal/wire"
)

// Role identifies the two PAKE parties. RoleA opens the exchange.
type Role int

const (
	RoleA Role = 0 // initiator: sends the first PAKE message
	RoleB Role = 1 // responder: replies with the second PAKE message
)

func (r Role) String() string {
	if r == RoleA {
		return "A"
	}
	return "B"
}

// Curve is the PAKE group. siec is the default used by croc; p256 is used
// here because it is universally implemented and fast.
const Curve = "p256"

// NormalizeCode canonicalises a user supplied transfer code so that trivial
// formatting differences ("Abc Def", "abc-def", "ABC_DEF") agree.
//
// The code is the only secret shared out of band, so normalization must be
// deterministic on both sides. It is intentionally conservative: lowercase,
// and collapse any run of non-alphanumeric characters to a single '-'.
func NormalizeCode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	lastDash := true // trims leading dashes
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// Handshake drives the SPAKE2 exchange. It is not safe for concurrent use.
type Handshake struct {
	p        *pake.Pake
	role     Role
	sent     bool
	finished bool
}

// NewHandshake creates a handshake for the given code and role.
func NewHandshake(code string, role Role) (*Handshake, error) {
	norm := NormalizeCode(code)
	if len(norm) < 6 {
		return nil, fmt.Errorf("secure: transfer code too short after normalization (%d chars, need >= 6)", len(norm))
	}
	p, err := pake.InitCurve([]byte(norm), int(role), Curve)
	if err != nil {
		return nil, fmt.Errorf("secure: pake init: %w", err)
	}
	return &Handshake{p: p, role: role}, nil
}

// Step advances the handshake.
//
// Call it with in == nil to obtain the first outbound message. It returns the
// bytes to send to the peer (nil if nothing needs to be sent yet) and whether
// the handshake is complete.
// maxHandshakeMsg bounds one PAKE message. A P-256 point plus JSON envelope is
// well under 1 KiB; anything larger is either a bug or an attempt to make us
// allocate.
const maxHandshakeMsg = 8 << 10

// validateHandshakeMsg checks the shape of a PAKE message before it reaches the
// third-party library.
//
// The peer here is the relay, which is untrusted by design: it can send
// anything it likes as the first handshake message. schollz/pake unmarshals
// the bytes into a *Pake and dereferences it without a nil check, so a
// four-byte binary "null" is enough to panic the process (SIGSEGV, exit 2) —
// no code required, no pairing required. Validating first turns that into an
// ordinary handshake failure.
func validateHandshakeMsg(in []byte) error {
	if len(in) == 0 {
		return errors.New("empty handshake message")
	}
	if len(in) > maxHandshakeMsg {
		return fmt.Errorf("handshake message too large (%d bytes, max %d)", len(in), maxHandshakeMsg)
	}
	// The library's wire format is a flat JSON object with a "Role" field and
	// a set of curve-coordinate fields (Uᵤ, Uᵥ, Vᵤ, ...). Only the shape is
	// checked here: a message carrying just a role, or no coordinates at all,
	// is what makes the library dereference a nil point.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(in, &probe); err != nil {
		return fmt.Errorf("handshake message is not valid JSON: %w", err)
	}
	roleRaw, ok := probe["Role"]
	if !ok {
		return errors.New("handshake message has no role")
	}
	var role int
	if err := json.Unmarshal(roleRaw, &role); err != nil {
		return errors.New("handshake message role is not a number")
	}
	// At least one coordinate must be present and non-null; a message of
	// nothing but a role is the shape that panics.
	points := 0
	for k, v := range probe {
		if k == "Role" {
			continue
		}
		if string(v) == "null" {
			continue
		}
		points++
	}
	if points == 0 {
		return errors.New("handshake message has no curve points")
	}
	return nil
}

func (h *Handshake) Step(in []byte) (out []byte, done bool, err error) {
	if h.finished {
		return nil, true, errors.New("secure: handshake already finished")
	}

	if in != nil {
		if verr := validateHandshakeMsg(in); verr != nil {
			return nil, false, fmt.Errorf("secure: handshake message rejected: %w", verr)
		}
		// Belt and braces: pake is a third-party library and this path is
		// reachable by an untrusted peer, so a panic here must not take the
		// process down even if the validation above has a gap.
		defer func() {
			if r := recover(); r != nil {
				out, done = nil, false
				err = fmt.Errorf("secure: handshake library panicked on peer input: %v", r)
			}
		}()
	}

	if in == nil {
		if h.role == RoleA && !h.sent {
			h.sent = true
			return h.p.Bytes(), false, nil
		}
		// RoleB must wait for A's message.
		return nil, false, nil
	}

	if h.role == RoleB && !h.sent {
		if err := h.p.Update(in); err != nil {
			return nil, false, fmt.Errorf("secure: pake step 1: %w", err)
		}
		h.sent = true
		return h.p.Bytes(), false, nil
	}

	if h.role == RoleA && !h.finished {
		if err := h.p.Update(in); err != nil {
			return nil, false, fmt.Errorf("secure: pake step 2: %w", err)
		}
		h.finished = true
		return nil, true, nil
	}

	return nil, false, fmt.Errorf("secure: unexpected handshake input for role %s", h.role)
}

// SessionKey returns the derived session key once the handshake is complete.
func (h *Handshake) SessionKey() ([]byte, error) {
	k, err := h.p.SessionKey()
	if err != nil {
		return nil, fmt.Errorf("secure: session key not ready: %w", err)
	}
	return k, nil
}

// DeriveKeys expands a PAKE session key into one AES-256-GCM key per
// direction. A->B and B->A keys are distinct so the two directions can use
// independent nonce spaces.
func DeriveKeys(sessionKey []byte, role Role) (send, recv []byte, err error) {
	master, err := hkdfKey(sessionKey, "container-mesh/v1 master")
	if err != nil {
		return nil, nil, err
	}
	ab, err := hkdfKey(master, "container-mesh/v1 A->B")
	if err != nil {
		return nil, nil, err
	}
	ba, err := hkdfKey(master, "container-mesh/v1 B->A")
	if err != nil {
		return nil, nil, err
	}
	if role == RoleA {
		return ab, ba, nil
	}
	return ba, ab, nil
}

// hkdfKey derives a 32-byte key with HKDF-SHA256 (no salt, domain-separated
// by info). The PAKE output is already uniform, so an empty salt is fine.
func hkdfKey(secret []byte, info string) ([]byte, error) {
	out := make([]byte, 32)
	r := hkdf.New(sha256.New, secret, nil, []byte(info))
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("secure: hkdf %q: %w", info, err)
	}
	return out, nil
}

// roomDomain separates the room derivation from every other use of the code.
const roomDomain = "container-mesh/room/v1"

// RoomID derives the rendezvous identifier sent to the relay from the shared
// code.
//
// The relay must not learn the code: the code is the only shared secret, and a
// relay that knows it can complete the PAKE with each side separately and sit
// in the middle. RoomID is a one-way HMAC, so the relay sees only an opaque
// label it can route on.
//
// Note this is *not* by itself a defence against a malicious relay: an attacker
// holding RoomID can still guess a weak code offline (the label is a verifier).
// The defence against an active relay is the SAS check (see Session.ShortAuth);
// RoomID only removes the code from logs and URLs.
func RoomID(code string) string {
	mac := hmac.New(sha256.New, []byte(roomDomain))
	mac.Write([]byte(NormalizeCode(code)))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// ShortAuth returns a short authentication string derived from the established
// session key. Both peers compute the same value; comparing it out of band
// (reading 6 digits aloud) proves no relay sat in the middle.
//
// The derivation is symmetric — it deliberately does not mix in the role — so
// A and B produce identical digits.
func (s *Session) ShortAuth() string {
	s.sasOnce.Do(func() {
		mac := hmac.New(sha256.New, s.sasKey)
		mac.Write([]byte("container-mesh/sas/v1"))
		sum := mac.Sum(nil)
		// 6 decimal digits from the first 4 bytes: easy to read aloud, ~20 bits,
		// which is the usual size for a one-shot SAS (compare once per session).
		n := uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
		s.sas = fmt.Sprintf("%06d", n%1000000)
	})
	return s.sas
}

// Fingerprint is a longer, human-comparable digest of the session key, for
// situations where a 6-digit SAS is not enough (e.g. logging an audit trail).
func (s *Session) Fingerprint() string {
	mac := hmac.New(sha256.New, s.sasKey)
	mac.Write([]byte("container-mesh/fingerprint/v1"))
	sum := mac.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// maxCounter bounds the number of frames per direction. 2^64 is unreachable in
// practice; the check exists so a bug cannot silently reuse a nonce.
const maxCounter = ^uint64(0)

// Session is an encrypted, ordered frame channel.
type Session struct {
	sendAEAD cipher.AEAD
	recvAEAD cipher.AEAD

	sendMu  sync.Mutex
	sendCtr uint64

	recvMu  sync.Mutex
	recvCtr uint64

	// sasKey is a dedicated *symmetric* derivation of the session key. It must
	// not be the send or recv key: those differ per direction, so peers would
	// compute different SAS values.
	sasKey []byte

	sasOnce sync.Once
	sas     string
}

// NewSession builds a session from a completed handshake.
func NewSession(h *Handshake) (*Session, error) {
	key, err := h.SessionKey()
	if err != nil {
		return nil, err
	}
	sendKey, recvKey, err := DeriveKeys(key, h.role)
	if err != nil {
		return nil, err
	}
	sendAEAD, err := newAEAD(sendKey)
	if err != nil {
		return nil, err
	}
	recvAEAD, err := newAEAD(recvKey)
	if err != nil {
		return nil, err
	}
	sasKey, err := hkdfKey(key, "container-mesh/v1 SAS")
	if err != nil {
		return nil, err
	}
	return &Session{sendAEAD: sendAEAD, recvAEAD: recvAEAD, sasKey: sasKey}, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secure: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secure: gcm: %w", err)
	}
	return aead, nil
}

func nonceFor(ctr uint64) []byte {
	var n [12]byte
	for i := 0; i < 8; i++ {
		n[4+i] = byte(ctr >> (8 * (7 - i)))
	}
	return n[:]
}

// SealFrame encodes and encrypts one frame.
func (s *Session) SealFrame(f *wire.Frame) ([]byte, error) {
	plain, err := f.Encode(nil)
	if err != nil {
		return nil, err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.sendCtr == maxCounter {
		return nil, errors.New("secure: send counter exhausted, re-handshake required")
	}
	nonce := nonceFor(s.sendCtr)
	s.sendCtr++
	return s.sendAEAD.Seal(nil, nonce, plain, nil), nil
}

// OpenFrame decrypts and decodes one frame.
//
// Frames must arrive in exactly the order they were sent; anything else is
// treated as a protocol violation (this also defeats replay).
func (s *Session) OpenFrame(b []byte) (*wire.Frame, error) {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	nonce := nonceFor(s.recvCtr)
	plain, err := s.recvAEAD.Open(nil, nonce, b, nil)
	if err != nil {
		return nil, fmt.Errorf("secure: decrypt failed at counter %d (wrong code or tampered traffic): %w", s.recvCtr, err)
	}
	f, err := wire.DecodeFrameBytes(plain)
	if err != nil {
		return nil, err
	}
	s.recvCtr++
	return f, nil
}

// SendCounter exposes the number of frames sealed so far (for tests/metrics).
func (s *Session) SendCounter() uint64 {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.sendCtr
}

// RecvCounter exposes the number of frames opened so far (for tests/metrics).
func (s *Session) RecvCounter() uint64 {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	return s.recvCtr
}

// codeAlphabet omits characters that are easy to confuse when read aloud or
// copied by hand (0/O, 1/l/I). The code is meant to be transcribed once.
const codeAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// GenerateCode returns a fresh transfer code with enough entropy that the
// room label derived from it is not worth attacking offline.
//
// 26 characters from a 31-symbol alphabet is ~128 bits, which is far beyond
// what a relay could enumerate even if it records every room label it sees.
func GenerateCode() (string, error) {
	const n = 26
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	// Group for readability: abcde-fghij-...
	var sb strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 && i%5 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(out[i])
	}
	return sb.String(), nil
}
