// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package credential implements the team join credential: ONE string that
// lets a device join the network without a per-device human approval.
//
// The problem it solves (M4 (a)): the current join flow needs the account
// owner to sign every device certificate and hand it over — one action per
// device, and the device cannot be admitted over the network.
//
// The shape of the answer:
//
//   - The account owner creates a credential (bounded: expiry + device
//     quota) and hands the string to the team.
//   - A member's device runs ONE command with that string, mints its own
//     certificate under the credential's delegator key, and presents it to
//     the discovery node, which admits it.
//
// Two properties are deliberate:
//
//   - The account key NEVER travels with the team. The credential carries a
//     delegator key instead, so a leaked credential is bounded by its expiry
//     and quota, not by the account's lifetime.
//   - The discovery node never holds the delegator's private key, so the
//     "a node cannot forge a member" boundary (MESH-DESIGN §5.1) still
//     holds: it can only admit what the delegator signed, and only while
//     the delegation record exists.
//
// The credential is therefore a SECRET: possession is authority. It is
// printed once, handed over like a password, and never logged.
package credential

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// Version is the credential format version. The prefix in the encoded
// string keeps a future format from being read as this one.
const Version = 1

// Prefix marks an encoded credential.
const Prefix = "cmesh1"

// DefaultTTL is the credential's default lifetime.
const DefaultTTL = 24 * time.Hour

// DefaultMaxDevices is the credential's default device quota.
const DefaultMaxDevices = 10

// DefaultCertTTL bounds the certificates a credential can mint, so a
// credential holder cannot enrol devices that outlive the credential by a
// wide margin.
const DefaultCertTTL = 24 * time.Hour

var (
	// ErrMalformed means the string is not a credential this code can read.
	ErrMalformed = errors.New("credential: malformed")
	// ErrExpired means the credential's validity window has lapsed.
	ErrExpired = errors.New("credential: expired")
	// ErrBadSignature means the credential was altered after signing.
	ErrBadSignature = errors.New("credential: signature invalid")
	// ErrAccountMismatch means the credential speaks for another account.
	ErrAccountMismatch = errors.New("credential: account mismatch")
)

// Public is the account-signed, shareable half of a credential: everything
// a verifier needs, and nothing that grants power on its own.
type Public struct {
	Version int `json:"version"`
	// ID identifies this credential for quota accounting.
	ID string `json:"id"`
	// AccountID and AccountPub say which account the credential speaks for.
	AccountID  identity.DeviceID `json:"account_id"`
	AccountPub ed25519.PublicKey `json:"account_pub"`
	// DelegatorPub is the ONLY key allowed to sign device certificates
	// under this credential.
	DelegatorPub ed25519.PublicKey `json:"delegator_pub"`
	// DiscoveryAddrs are the seed discovery endpoints a joiner should use.
	DiscoveryAddrs []string `json:"disc"`
	// IssuedAt, ExpiresAt and MaxDevices are the credential's bounds.
	IssuedAt   time.Time `json:"issued_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	MaxDevices int       `json:"max_devices"`
	// Sig is the ACCOUNT's signature over the canonical form of the fields
	// above (the struct without Sig).
	Sig []byte `json:"sig"`
}

// Secret is the holder-only half: the delegator's private key. It is
// transported inside the same string but is NOT part of the signed body —
// a verifier (the discovery node) must never see it.
type Secret struct {
	DelegatorPriv ed25519.PrivateKey `json:"delegator_priv"`
}

// Credential is a full credential: the shareable public half plus the
// secret the joiner needs.
type Credential struct {
	Public Public
	Secret Secret
}

// Create issues a credential for an account.
//
// The delegator keypair is generated here and never leaves the credential's
// secret half. The account signs the public half, so the discovery node can
// verify it against the account public key it already knows.
func Create(account *identity.Account, disc []string, ttl time.Duration, maxDevices int, now time.Time) (*Credential, error) {
	if account == nil {
		return nil, errors.New("credential: account required")
	}
	if len(disc) == 0 {
		return nil, errors.New("credential: at least one discovery address is required")
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if maxDevices <= 0 {
		return nil, fmt.Errorf("credential: device quota must be positive, got %d", maxDevices)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("credential: generate delegator key: %w", err)
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	c := &Credential{
		Public: Public{
			Version:        Version,
			ID:             id,
			AccountID:      account.ID(),
			AccountPub:     account.PublicKey(),
			DelegatorPub:   pub,
			DiscoveryAddrs: append([]string(nil), disc...),
			IssuedAt:       now,
			ExpiresAt:      now.Add(ttl),
			MaxDevices:     maxDevices,
		},
		Secret: Secret{DelegatorPriv: priv},
	}
	if err := signPublic(account, &c.Public); err != nil {
		return nil, err
	}
	return c, nil
}

// signPublic replaces the public half's signature with the account's.
func signPublic(account *identity.Account, p *Public) error {
	p.Sig = nil
	b, err := p.signable()
	if err != nil {
		return err
	}
	p.Sig = account.SignBytes(b)
	return nil
}

// signable returns the exact bytes the signature covers.
func (p *Public) signable() ([]byte, error) {
	p2 := *p
	p2.Sig = nil
	return json.Marshal(p2)
}

// Encode renders the credential as the single string that is handed over.
func Encode(c *Credential) (string, error) {
	if c == nil {
		return "", errors.New("credential: nothing to encode")
	}
	pub, err := json.Marshal(c.Public)
	if err != nil {
		return "", fmt.Errorf("credential: encode public half: %w", err)
	}
	sec, err := json.Marshal(c.Secret)
	if err != nil {
		return "", fmt.Errorf("credential: encode secret half: %w", err)
	}
	return Prefix + "." +
		base64.RawURLEncoding.EncodeToString(pub) + "." +
		base64.RawURLEncoding.EncodeToString(sec), nil
}

// Decode parses a credential string.
func Decode(s string) (*Credential, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ".")
	if len(parts) != 3 || parts[0] != Prefix {
		return nil, fmt.Errorf("%w: expected a %s credential", ErrMalformed, Prefix)
	}
	pubBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: public half is not base64url: %v", ErrMalformed, err)
	}
	secBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: secret half is not base64url: %v", ErrMalformed, err)
	}
	var c Credential
	if err := json.Unmarshal(pubBytes, &c.Public); err != nil {
		return nil, fmt.Errorf("%w: public half is not JSON: %v", ErrMalformed, err)
	}
	if err := json.Unmarshal(secBytes, &c.Secret); err != nil {
		return nil, fmt.Errorf("%w: secret half is not JSON: %v", ErrMalformed, err)
	}
	return &c, nil
}

// Verify checks the public half against the account public key: version,
// signature, account binding and expiry.
//
// A joiner can call this for a friendlier error, but it is NOT the
// authoritative check — a joiner holds the account public key from the
// credential itself, so it proves nothing to itself. The discovery node
// verifies against the account key it trusts.
func (c *Credential) Verify(accountPub ed25519.PublicKey, now time.Time) error {
	if c == nil {
		return ErrMalformed
	}
	p := c.Public
	if p.Version != Version {
		return fmt.Errorf("%w: unsupported version %d", ErrMalformed, p.Version)
	}
	if identity.AccountID(accountPub) != p.AccountID {
		return fmt.Errorf("%w: credential is for account %s, not %s",
			ErrAccountMismatch, p.AccountID, identity.AccountID(accountPub))
	}
	b, err := p.signable()
	if err != nil {
		return err
	}
	if !ed25519.Verify(accountPub, b, p.Sig) {
		return ErrBadSignature
	}
	if now.After(p.ExpiresAt) {
		return fmt.Errorf("%w: expired at %s", ErrExpired, p.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// MintCert signs a device certificate under this credential's delegator.
//
// The certificate names the delegator in SignerPub, so the admission check
// can tell that this device was admitted under a delegation rather than
// signed by the account directly.
func (c *Credential) MintCert(devPub ed25519.PublicKey, ttl time.Duration, now time.Time) (*identity.DeviceCert, error) {
	if c == nil {
		return nil, ErrMalformed
	}
	if ttl <= 0 {
		ttl = DefaultCertTTL
	}
	return identity.SignDelegatedCert(c.Public.DelegatorPub, c.Secret.DelegatorPriv,
		c.Public.AccountID, devPub, ttl, now)
}

// Delegation converts the credential into the record the discovery node
// keeps: the delegator's authority, bounded by the credential's quota and
// expiry, and signed by the account.
func (c *Credential) Delegation(account *identity.Account) (*identity.Delegation, error) {
	if c == nil {
		return nil, ErrMalformed
	}
	d := &identity.Delegation{
		ID:         c.Public.ID,
		Pub:        c.Public.DelegatorPub,
		AccountID:  c.Public.AccountID,
		MaxDevices: c.Public.MaxDevices,
		IssuedAt:   c.Public.IssuedAt,
		ExpiresAt:  c.Public.ExpiresAt,
	}
	if err := account.SignDelegation(d); err != nil {
		return nil, err
	}
	return d, nil
}

// randomID returns an identifier for quota accounting.
func randomID() (string, error) {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("credential: random id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
