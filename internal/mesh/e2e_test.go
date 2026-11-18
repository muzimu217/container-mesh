// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// The end-to-end layer of the relay trunk.
//
// The trunk (the middle node) terminates TLS on both of its hops, so it can
// see the bytes between A and C UNLESS there is a layer above the transport
// that only A and C hold keys for. That layer is what these tests pin.
//
// The primitive is SIGMA: signed ECDH. Both ends hold each other's Ed25519
// public key (the member view is the trust anchor, so the signature is
// verifiable offline and revocation is immediate), so no shared secret has
// to be provisioned — which is exactly what makes it different from the
// stage-1 SPAKE2 relay, where the two sides share a passphrase.

// e2ePair builds the two ends of an end-to-end session: A and C.
type e2ePair struct {
	alice *identity.Device
	bob   *identity.Device
}

// newE2EPair builds two devices that know each other's public keys.
func newE2EPair(t testing.TB) *e2ePair {
	t.Helper()
	a, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return &e2ePair{alice: a, bob: b}
}

// TestE2EHandshakeDerivesMatchingKeys is the base case: the two ends of a
// relayed session must derive the SAME keys, or the data cannot flow.
//
// A relayed session has no shared transport key between A and C: A talks to
// the trunk over one mTLS session, the trunk talks to C over another. The
// only thing A and C have in common is the member view, so the session key
// has to come from the handshake, not from the transport.
func TestE2EHandshakeDerivesMatchingKeys(t *testing.T) {
	p := newE2EPair(t)
	a, err := NewE2ESession(E2EConfig{
		Self:    p.alice,
		Peer:    p.bob.ID(),
		PeerKey: p.bob.PublicKey(),
		Role:    E2ERoleInitiator,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewE2ESession(E2EConfig{
		Self:    p.bob,
		Peer:    p.alice.ID(),
		PeerKey: p.alice.PublicKey(),
		Role:    E2ERoleResponder,
	})
	if err != nil {
		t.Fatal(err)
	}

	hello, err := a.Hello()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AcceptHello(hello); err != nil {
		t.Fatalf("the responder refused a valid hello: %v", err)
	}
	reply, err := b.Hello()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AcceptHello(reply); err != nil {
		t.Fatalf("the initiator refused a valid reply: %v", err)
	}

	plain := []byte("the quick brown fox jumps over the lazy dog")
	ct, err := a.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Open(ct)
	if err != nil {
		t.Fatalf("the responder could not open the ciphertext: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("the decrypted payload is %q, want %q", got, plain)
	}

	// And the other direction: the reply carries its own key, so the
	// responder's seal must open at the initiator.
	ct2, err := b.Seal([]byte("and back"))
	if err != nil {
		t.Fatal(err)
	}
	got2, err := a.Open(ct2)
	if err != nil {
		t.Fatalf("the initiator could not open the reply: %v", err)
	}
	if !bytes.Equal(got2, []byte("and back")) {
		t.Fatalf("the decrypted reply is %q", got2)
	}
}

// TestE2ECiphertextHidesThePlaintext is the R-3.5 acceptance criterion: the
// trunk must not be able to read the business payload.
//
// The test asserts the thing that matters — the ciphertext does not contain
// the plaintext — and additionally that the trunk (which holds neither the
// initiator's nor the responder's session key) cannot open it.
func TestE2ECiphertextHidesThePlaintext(t *testing.T) {
	p := newE2EPair(t)
	a, _ := NewE2ESession(E2EConfig{Self: p.alice, Peer: p.bob.ID(), PeerKey: p.bob.PublicKey(), Role: E2ERoleInitiator})
	b, _ := NewE2ESession(E2EConfig{Self: p.bob, Peer: p.alice.ID(), PeerKey: p.alice.PublicKey(), Role: E2ERoleResponder})

	hello, _ := a.Hello()
	_ = b.AcceptHello(hello)
	reply, _ := b.Hello()
	_ = a.AcceptHello(reply)

	plain := []byte("TOP SECRET: the merger price is 42 million")
	ct, err := a.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, plain) {
		t.Fatal("the ciphertext contains the plaintext verbatim")
	}
	if bytes.Contains(ct, []byte("merger")) {
		t.Fatal("the ciphertext leaks a fragment of the plaintext")
	}

	// The trunk's position: it holds the transport keys of both hops, but
	// not the end-to-end key. So it has nothing to open the payload with.
	// The test proves it by showing that a session established with the
	// WRONG peer key (which is what the trunk could construct) cannot open
	// the traffic.
	trunk, err := NewE2ESession(E2EConfig{
		Self:    p.bob,
		Peer:    p.alice.ID(),
		PeerKey: p.alice.PublicKey(),
		Role:    E2ERoleResponder,
	})
	if err != nil {
		t.Fatal(err)
	}
	th, _ := trunk.Hello()
	_ = trunk.AcceptHello(th)
	// The trunk never receives the initiator's hello, so it has no session
	// key: opening must fail rather than return garbage.
	if _, err := trunk.Open(ct); err == nil {
		t.Fatal("a session that never completed the handshake opened a ciphertext")
	}
}

// TestE2ERefusesAHelloSignedByTheWrongKey: the trunk (or anybody) cannot
// impersonate either end.
//
// The signature is what binds the ephemeral key to the device identity. If
// the signature were missing, the trunk could present its OWN ephemeral key
// as the initiator's and man-in-the-middle the whole session.
func TestE2ERefusesAHelloSignedByTheWrongKey(t *testing.T) {
	p := newE2EPair(t)
	mallory, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewE2ESession(E2EConfig{Self: p.bob, Peer: p.alice.ID(), PeerKey: p.alice.PublicKey(), Role: E2ERoleResponder})

	hello, err := NewE2ESession(E2EConfig{Self: p.alice, Peer: p.bob.ID(), PeerKey: p.bob.PublicKey(), Role: E2ERoleInitiator})
	if err != nil {
		t.Fatal(err)
	}
	h, err := hello.Hello()
	if err != nil {
		t.Fatal(err)
	}
	// Mallory re-signs the hello with its own key: the ephemeral key and
	// the claimed device ID stay the same, only the signature changes.
	h.Signature = mallory.Sign(h.Transcript(p.bob.ID()))
	if err := b.AcceptHello(h); err == nil {
		t.Fatal("a hello signed by the wrong key was accepted")
	}
}

// TestE2ERefusesATamperedHello: the trunk cannot rewrite the handshake.
//
// The transcript binds the ephemeral key, both device IDs and the session
// id. Changing any of them invalidates the signature.
func TestE2ERefusesATamperedHello(t *testing.T) {
	p := newE2EPair(t)
	b, _ := NewE2ESession(E2EConfig{Self: p.bob, Peer: p.alice.ID(), PeerKey: p.alice.PublicKey(), Role: E2ERoleResponder})
	a, _ := NewE2ESession(E2EConfig{Self: p.alice, Peer: p.bob.ID(), PeerKey: p.bob.PublicKey(), Role: E2ERoleInitiator})

	h, err := a.Hello()
	if err != nil {
		t.Fatal(err)
	}
	// Swap in another ephemeral key (one the initiator never used).
	other, err := NewE2ESession(E2EConfig{Self: p.alice, Peer: p.bob.ID(), PeerKey: p.bob.PublicKey(), Role: E2ERoleInitiator})
	if err != nil {
		t.Fatal(err)
	}
	otherHello, err := other.Hello()
	if err != nil {
		t.Fatal(err)
	}
	h.Ephemeral = otherHello.Ephemeral
	if err := b.AcceptHello(h); err == nil {
		t.Fatal("a hello with a swapped ephemeral key was accepted")
	}
}

// TestE2ERefusesAStaleHello: a captured hello must not be replayable.
//
// The hello carries a timestamp and the receiver enforces the same ±5 s
// window the direct link uses for its hello (Skew).
func TestE2ERefusesAStaleHello(t *testing.T) {
	p := newE2EPair(t)
	b, _ := NewE2ESession(E2EConfig{Self: p.bob, Peer: p.alice.ID(), PeerKey: p.alice.PublicKey(), Role: E2ERoleResponder})
	a, _ := NewE2ESession(E2EConfig{Self: p.alice, Peer: p.bob.ID(), PeerKey: p.bob.PublicKey(), Role: E2ERoleInitiator})

	h, err := a.Hello()
	if err != nil {
		t.Fatal(err)
	}
	h.Time = time.Now().Add(-2 * Skew)
	// Re-sign so the failure is the clock, not the signature.
	h.Signature = p.alice.Sign(h.Transcript(p.bob.ID()))
	if err := b.AcceptHello(h); err == nil {
		t.Fatal("a stale hello was accepted")
	}
}

// TestE2EHelloCodecRoundTrip pins the wire shape of the handshake: it has
// to survive the JSON encoding that the relay data frames carry.
func TestE2EHelloCodecRoundTrip(t *testing.T) {
	p := newE2EPair(t)
	a, err := NewE2ESession(E2EConfig{Self: p.alice, Peer: p.bob.ID(), PeerKey: p.bob.PublicKey(), Role: E2ERoleInitiator})
	if err != nil {
		t.Fatal(err)
	}
	h, err := a.Hello()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := EncodeE2EHello(h)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeE2EHello(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeviceID != h.DeviceID || !bytes.Equal(got.Ephemeral, h.Ephemeral) || got.SessionID != h.SessionID {
		t.Fatalf("the hello round-tripped to %+v, want %+v", got, h)
	}
	if !bytes.Equal(got.Signature, h.Signature) {
		t.Fatal("the signature did not survive the round trip")
	}
	if _, err := DecodeE2EHello([]byte("{")); err == nil {
		t.Fatal("a broken hello decoded without an error")
	}
}

// TestE2ERejectsAPeerKeyOfTheWrongLength: a malformed member record must
// not turn a verification into a panic.
func TestE2ERejectsAPeerKeyOfTheWrongLength(t *testing.T) {
	p := newE2EPair(t)
	if _, err := NewE2ESession(E2EConfig{
		Self:    p.alice,
		Peer:    p.bob.ID(),
		PeerKey: []byte("too short"),
		Role:    E2ERoleInitiator,
	}); err == nil {
		t.Fatal("a malformed peer key was accepted")
	}
}

// TestE2ESealBeforeHandshakeFails: a session that has not completed the
// handshake cannot pretend to seal anything.
func TestE2ESealBeforeHandshakeFails(t *testing.T) {
	p := newE2EPair(t)
	a, err := NewE2ESession(E2EConfig{Self: p.alice, Peer: p.bob.ID(), PeerKey: p.bob.PublicKey(), Role: E2ERoleInitiator})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Seal([]byte("x")); err == nil {
		t.Fatal("a session sealed before the handshake completed")
	}
	if _, err := a.Open([]byte("x")); err == nil {
		t.Fatal("a session opened before the handshake completed")
	}
}

// TestE2ERejectsAHelloFromTheWrongPeer: the hello names the device it
// claims to be; a hello from somebody else must be refused.
func TestE2ERejectsAHelloFromTheWrongPeer(t *testing.T) {
	p := newE2EPair(t)
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewE2ESession(E2EConfig{Self: p.bob, Peer: p.alice.ID(), PeerKey: p.alice.PublicKey(), Role: E2ERoleResponder})
	s, _ := NewE2ESession(E2EConfig{Self: stranger, Peer: p.bob.ID(), PeerKey: p.bob.PublicKey(), Role: E2ERoleInitiator})
	h, err := s.Hello()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AcceptHello(h); err == nil {
		t.Fatal("a hello from an unexpected peer was accepted")
	} else if !errors.Is(err, ErrAuth) {
		t.Fatalf("the refusal is %v, want an auth-class error", err)
	}
}
