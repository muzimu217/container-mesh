// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/crypto/hkdf"

	"github.com/muzimu217/container-mesh/internal/identity"
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

// ---- The end-to-end layer of the relay trunk --------------------------------
//
// WHY THIS EXISTS. The trunk (the middle node) terminates TLS on BOTH of its
// hops: it holds the A↔B session key and the B↔C session key. TLS is an
// endpoint-authenticated transport, so if the business payload travels inside
// the per-hop TLS, the trunk sees it in the clear — the moment A writes a
// message to B, B holds the plaintext.
//
// That is not an implementation shortcut, it is a property of the layering:
// re-encrypting per hop REQUIRES the intermediate to hold the plaintext at
// the moment it re-seals it. So the relay needs a layer ABOVE the transport
// whose key only the two ENDPOINTS hold.
//
// WHAT THIS IS. SIGMA: signed ECDH over X25519.
//
//   - The two ends have no shared secret to provision. What they share is
//     the member view: each one's Ed25519 public key is in it. That is why
//     the stage-1 SPAKE2 mechanism (internal/secure) CANNOT be used here —
//     it needs a passphrase, and the two ends of a relayed session have
//     never met.
//   - Each end generates an ephemeral X25519 key pair, sends the public
//     half, and SIGNS a transcript binding: the ephemeral key, both device
//     IDs, the session id and a timestamp.
//   - The signature is what makes the handshake non-forgeable by the
//     trunk: the trunk cannot present its own ephemeral key as the
//     initiator's, because it cannot sign as the initiator.
//   - The session key is HKDF-SHA256 over the ECDH shared secret, bound to
//     the session id and the direction.
//
// The layer is transport-independent: it seals BYTES, so it works over the
// relayed data frames exactly as it would over a direct link.

// E2ERole says which side of the handshake a session is.
type E2ERole uint8

const (
	// E2ERoleInitiator is the side that opens the relayed session (A).
	E2ERoleInitiator E2ERole = iota
	// E2ERoleResponder is the side that answers it (C).
	E2ERoleResponder
)

// e2eDomain separates the end-to-end derivation from every other use of
// the key material. A key derived for this domain can never be mistaken
// for a key derived for something else.
const e2eDomain = "cmesh-e2e-v1"

// E2EHello is the on-the-wire handshake message.
//
// It is NOT secret: the ephemeral public key and the device ID are meant to
// be visible (the trunk sees them — that is the routing metadata it is
// allowed to see). What it carries is a SIGNATURE, which is what makes it
// unforgeable.
type E2EHello struct {
	// DeviceID is the sender's device identity.
	DeviceID identity.DeviceID `json:"device_id"`
	// Ephemeral is the sender's X25519 public key (32 bytes).
	Ephemeral []byte `json:"ephemeral"`
	// SessionID ties the handshake to one relayed session.
	SessionID uint64 `json:"session_id"`
	// Time is when the hello was made. The receiver enforces the ±Skew
	// window, so a captured hello cannot be replayed later.
	Time time.Time `json:"time"`
	// Signature is the sender's Ed25519 signature over Transcript().
	Signature []byte `json:"signature"`
}

// Transcript returns the bytes the signature covers.
//
// It binds EVERYTHING that must not be malleable: the ephemeral key, both
// device IDs, the session id and the timestamp. Change any of them and the
// signature no longer verifies — which is exactly what stops the trunk
// from swapping an ephemeral key or redirecting the session.
//
// other is the device at the far end — the sender's own id is h.DeviceID.
// The receiver passes ITS OWN id, so the two ends compute the SAME
// transcript from mirrored arguments.
func (h E2EHello) Transcript(other identity.DeviceID) []byte {
	return e2eTranscript(h.Ephemeral, h.DeviceID, other, h.SessionID, h.Time)
}

// e2eTranscript builds the signed transcript bytes.
//
// The two ends sign DIFFERENT transcripts — each signs the one where it is
// the owner of the ephemeral key — but they derive the SAME session key
// from the ECDH secret. The session id is part of the transcript, so a
// hello cannot be moved into another session.
func e2eTranscript(ephemeral []byte, owner, other identity.DeviceID, sessionID uint64, ts time.Time) []byte {
	h := sha256.New()
	h.Write([]byte(e2eDomain))
	h.Write([]byte{0x01}) // transcript version
	writeLenPrefixed(h, ephemeral)
	writeLenPrefixed(h, []byte(owner))
	writeLenPrefixed(h, []byte(other))
	var sid [8]byte
	binary.BigEndian.PutUint64(sid[:], sessionID)
	h.Write(sid[:])
	var tsb [8]byte
	binary.BigEndian.PutUint64(tsb[:], uint64(ts.UnixNano()))
	h.Write(tsb[:])
	return h.Sum(nil)
}

// writeLenPrefixed writes a length-prefixed field, so two fields can never
// be confused with one another ("ab"+"c" vs "a"+"bc").
func writeLenPrefixed(h io.Writer, b []byte) {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(b)))
	_, _ = h.Write(l[:])
	_, _ = h.Write(b)
}

// EncodeE2EHello encodes a handshake message.
func EncodeE2EHello(h E2EHello) ([]byte, error) {
	return json.Marshal(h)
}

// DecodeE2EHello decodes a handshake message.
func DecodeE2EHello(p []byte) (E2EHello, error) {
	var h E2EHello
	if err := json.Unmarshal(p, &h); err != nil {
		return E2EHello{}, fmt.Errorf("mesh: e2e: bad hello payload: %w", err)
	}
	return h, nil
}

// E2EConfig configures one end of an end-to-end session.
type E2EConfig struct {
	// Self is this end's identity. It signs the hello.
	Self *identity.Device
	// Peer is the device at the other end. The hello must name it.
	Peer identity.DeviceID
	// PeerKey is the peer's Ed25519 public key, from the member view.
	// It verifies the peer's hello signature.
	PeerKey ed25519.PublicKey
	// Role says which side this is.
	Role E2ERole
	// SessionID ties the session to one relayed session. Zero means
	// "generate one".
	SessionID uint64
}

// E2ESession is one end of an end-to-end session.
//
// It is safe for concurrent use: the seal and open counters are the only
// mutable state, and each direction has its own.
type E2ESession struct {
	self    *identity.Device
	peer    identity.DeviceID
	peerKey ed25519.PublicKey
	role    E2ERole
	sessID  uint64

	priv *ecdh.PrivateKey
	pub  []byte

	// send and recv are the two directions' AEADs. send seals, recv opens.
	send cipher.AEAD
	recv cipher.AEAD

	// mu guards the counters and the established flag. The send and recv
	// counters belong to different directions, so they never contend for
	// the same counter — but a session that is read and written from two
	// goroutines at once still needs the flag to be read safely.
	mu          sync.Mutex
	sendCounter uint64
	recvCounter uint64
	// established flips when the handshake completes. A session that has
	// not completed it must not pretend to seal or open anything.
	established bool
}

// NewE2ESession builds one end of an end-to-end session.
//
// The peer key is REQUIRED: without it the hello signature cannot be
// verified, and a handshake whose signature is not verified is worth
// nothing — the trunk could then present its own key.
func NewE2ESession(cfg E2EConfig) (*E2ESession, error) {
	if cfg.Self == nil {
		return nil, errors.New("mesh: e2e: needs a device identity")
	}
	if cfg.Peer == "" {
		return nil, errors.New("mesh: e2e: needs a peer device id")
	}
	if len(cfg.PeerKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("mesh: e2e: the peer key is malformed (%d bytes)", len(cfg.PeerKey))
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mesh: e2e: ephemeral key: %w", err)
	}
	sessID := cfg.SessionID
	if sessID == 0 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("mesh: e2e: session id: %w", err)
		}
		sessID = binary.BigEndian.Uint64(b[:])
	}
	return &E2ESession{
		self:    cfg.Self,
		peer:    cfg.Peer,
		peerKey: cfg.PeerKey,
		role:    cfg.Role,
		sessID:  sessID,
		priv:    priv,
		pub:     priv.PublicKey().Bytes(),
	}, nil
}

// SessionID reports the session's identifier.
func (s *E2ESession) SessionID() uint64 { return s.sessID }

// Established reports whether the handshake has completed.
func (s *E2ESession) Established() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.established
}

// Hello builds this end's handshake message and signs it.
//
// The signature covers the transcript, which binds the ephemeral key, both
// device IDs, the session id and the timestamp.
func (s *E2ESession) Hello() (E2EHello, error) {
	if s == nil || s.self == nil {
		return E2EHello{}, errors.New("mesh: e2e: no session")
	}
	now := time.Now().UTC()
	h := E2EHello{
		DeviceID:  s.self.ID(),
		Ephemeral: s.pub,
		SessionID: s.sessID,
		Time:      now,
	}
	// The transcript is built with THIS end as the owner of the ephemeral
	// key and the peer as the other end. The peer verifies it with the
	// roles mirrored, so the two sides sign DIFFERENT transcripts — each
	// signing its own — and derive the same session key from the ECDH.
	h.Signature = s.self.Sign(h.Transcript(s.peer))
	return h, nil
}

// AcceptHello verifies a peer's hello and completes the handshake.
//
// The order of the checks is the security property:
//
//  1. The hello must name the peer we expect. A hello from somebody else
//     is refused before anything is derived.
//  2. The timestamp must be inside the ±Skew window, so a captured hello
//     cannot be replayed.
//  3. The SIGNATURE must verify against the member view's key for the
//     claimed device. This is what makes the handshake non-forgeable: the
//     trunk holds neither end's private key.
//  4. The session id must be the one this side already committed to (the
//     initiator's) — otherwise the two sides would derive different keys.
//
// Only then is the session key derived and the AEADs installed.
//
// The RESPONDER adopts the initiator's session id: the initiator is the
// side that opened the session, so its id is the session's id. Adopting it
// is what makes the two sides derive the SAME key — each side generating
// its own random id would give two different sessions.
func (s *E2ESession) AcceptHello(h E2EHello) error {
	if s == nil {
		return errors.New("mesh: e2e: no session")
	}
	if h.DeviceID != s.peer {
		return fmt.Errorf("%w: mesh: e2e: the hello is from %s, not %s", ErrAuth, h.DeviceID, s.peer)
	}
	now := time.Now()
	if now.Sub(h.Time) > Skew || h.Time.Sub(now) > Skew {
		return fmt.Errorf("%w: mesh: e2e: the hello timestamp is outside the window", ErrAuth)
	}
	if len(h.Ephemeral) != 32 {
		return fmt.Errorf("%w: mesh: e2e: the ephemeral key is malformed (%d bytes)", ErrAuth, len(h.Ephemeral))
	}
	// The transcript here is built with the PEER as the owner of the
	// ephemeral key and THIS end as the other end — the mirror of what
	// the peer signed.
	transcript := e2eTranscript(h.Ephemeral, h.DeviceID, s.self.ID(), h.SessionID, h.Time)
	if !identity.VerifyDevice(s.peerKey, transcript, h.Signature) {
		return fmt.Errorf("%w: mesh: e2e: the hello signature does not verify against the member view", ErrAuth)
	}
	s.mu.Lock()
	if s.role == E2ERoleInitiator && h.SessionID != s.sessID {
		// The initiator committed to a session id when it opened the
		// session; a reply naming another one belongs to a different
		// session and must not be mixed in.
		s.mu.Unlock()
		return fmt.Errorf("%w: mesh: e2e: the reply names session %d, not %d", ErrAuth, h.SessionID, s.sessID)
	}
	if s.role == E2ERoleResponder {
		// The responder adopts the initiator's session id.
		s.sessID = h.SessionID
	}
	s.mu.Unlock()
	peerPub, err := ecdh.X25519().NewPublicKey(h.Ephemeral)
	if err != nil {
		return fmt.Errorf("%w: mesh: e2e: the peer ephemeral key is invalid: %v", ErrAuth, err)
	}
	shared, err := s.priv.ECDH(peerPub)
	if err != nil {
		return fmt.Errorf("%w: mesh: e2e: key agreement failed: %v", ErrAuth, err)
	}
	send, recv, err := e2eKeys(shared, s.sessID, s.role)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.send, s.recv = send, recv
	s.established = true
	s.mu.Unlock()
	return nil
}

// e2eKeys derives the two directions' AEAD keys.
//
// The DIRECTION is bound into the derivation, so the initiator's send key
// is NOT the responder's send key: a reflection attack (replaying A's
// ciphertext back at A) opens nothing.
func e2eKeys(shared []byte, sessionID uint64, role E2ERole) (cipher.AEAD, cipher.AEAD, error) {
	initiatorSend := "cmesh-e2e-v1/send/initiator"
	responderSend := "cmesh-e2e-v1/send/responder"
	info := initiatorSend
	if role == E2ERoleResponder {
		info = responderSend
	}
	sendKey, err := e2eDerive(shared, sessionID, info)
	if err != nil {
		return nil, nil, err
	}
	recvInfo := responderSend
	if role == E2ERoleResponder {
		recvInfo = initiatorSend
	}
	recvKey, err := e2eDerive(shared, sessionID, recvInfo)
	if err != nil {
		return nil, nil, err
	}
	send, err := e2eAEAD(sendKey)
	if err != nil {
		return nil, nil, err
	}
	recv, err := e2eAEAD(recvKey)
	if err != nil {
		return nil, nil, err
	}
	return send, recv, nil
}

// e2eDerive derives one 32-byte key with HKDF-SHA256.
//
// The session id is in the info string, so two sessions that somehow shared
// a shared secret still get different keys.
func e2eDerive(shared []byte, sessionID uint64, info string) ([]byte, error) {
	var sid [8]byte
	binary.BigEndian.PutUint64(sid[:], sessionID)
	full := info + "|" + string(sid[:])
	out := make([]byte, 32)
	r := hkdf.New(sha256.New, shared, nil, []byte(full))
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("mesh: e2e: key derivation: %w", err)
	}
	return out, nil
}

// e2eAEAD builds the AEAD from a 32-byte key (AES-256-GCM).
func e2eAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("mesh: e2e: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("mesh: e2e: gcm: %w", err)
	}
	return gcm, nil
}

// Seal encrypts a payload for the peer.
//
// The output is the nonce followed by the ciphertext, so the receiver needs
// nothing but the bytes to open it.
//
// Go's GCM Seal does NOT prepend the nonce (it only appends the ciphertext
// and the tag), so the nonce is prepended HERE. That is why the receiver
// splits it back off in Open.
func (s *E2ESession) Seal(plain []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.established || s.send == nil {
		return nil, errors.New("mesh: e2e: the session has not completed its handshake")
	}
	nonce := e2eNonce(s.sendCounter)
	s.sendCounter++
	sealed := s.send.Seal(nil, nonce, plain, nil)
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// Open decrypts a payload from the peer.
//
// The nonce is the first 12 bytes of the input; the rest is the ciphertext.
func (s *E2ESession) Open(ct []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.established || s.recv == nil {
		return nil, errors.New("mesh: e2e: the session has not completed its handshake")
	}
	ns := s.recv.NonceSize()
	if len(ct) < ns {
		return nil, fmt.Errorf("mesh: e2e: ciphertext is too short (%d bytes)", len(ct))
	}
	nonce := ct[:ns]
	body := ct[ns:]
	plain, err := s.recv.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: mesh: e2e: the ciphertext does not authenticate: %v", ErrAuth, err)
	}
	s.recvCounter++
	return plain, nil
}

// e2eNonce builds the AEAD nonce for a counter.
//
// The nonce is a 4-byte big-endian counter followed by 8 zero bytes. The
// session id is NOT mixed in: each direction has its own KEY, so the two
// directions' nonces never collide even at the same counter value.
func e2eNonce(counter uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint32(n[0:4], uint32(counter))
	return n
}
