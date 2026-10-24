// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/argon2"
)

// nonceSize is the GCM standard nonce length.
const nonceSize = 12

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// deriveKey runs Argon2id over the passphrase with the stored parameters.
func deriveKey(pass string, cfg kdfCfg) []byte {
	return argon2.IDKey([]byte(pass), cfg.Salt, cfg.Time, cfg.Memory, cfg.Threads, 32)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Account is the "same identity, many devices" anchor.
//
// It is just another Ed25519 key pair, kept offline or on the most trusted
// device. It signs device certificates; it never touches the data plane.
type Account struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	id   DeviceID
}

// AccountID derives exactly like a DeviceID. Same length, same alphabet, so
// tooling can render either uniformly; the type distinction prevents
// accidentally treating one as the other.
func AccountID(pub ed25519.PublicKey) DeviceID { return IDFromKey(pub) }

// GenerateAccount creates a fresh account key pair.
func GenerateAccount() (*Account, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	return &Account{priv: priv, pub: pub, id: IDFromKey(pub)}, nil
}

// LoadAccount reads an encrypted account file (same envelope as a device).
func LoadAccount(path, passphrase string) (*Account, error) {
	// The envelope is shared, so reuse the loader and convert.
	d, err := LoadDevice(path, passphrase)
	if err != nil {
		return nil, err
	}
	return &Account{priv: d.priv, pub: d.pub, id: d.id}, nil
}

// Save writes the encrypted account file.
func (a *Account) Save(path, passphrase string) error {
	d := &Device{priv: a.priv, pub: a.pub, id: a.id, CreatedAt: time.Now().UTC()}
	return d.Save(path, passphrase)
}

// ID reports the account's identifier.
func (a *Account) ID() DeviceID { return a.id }

// PublicKey reports the account's public key — the one value every member
// needs in order to verify device certificates.
func (a *Account) PublicKey() ed25519.PublicKey { return a.pub }

// DeviceCert binds a device to an account. Any member can verify it with the
// account public key alone; no server is involved.
type DeviceCert struct {
	DeviceID  DeviceID          `json:"device_id"`
	DevicePub ed25519.PublicKey `json:"device_pub"`
	AccountID DeviceID          `json:"account_id"`
	IssuedAt  time.Time         `json:"issued_at"`
	ExpiresAt time.Time         `json:"expires_at"`
	// Sig is the account's signature over the canonical form of the fields
	// above. Canonical form = the JSON of the struct with Sig omitted, which
	// is deterministic because every field has a fixed JSON shape.
	Sig []byte `json:"sig"`
}

// DefaultCertTTL is the validity window of a device certificate. Renewal is
// re-signing; there is no revocation list for certs themselves, because
// membership in the MemberList is what grants access.
const DefaultCertTTL = 365 * 24 * time.Hour

// certSignable returns the exact bytes the signature covers.
func (c *DeviceCert) certSignable() ([]byte, error) {
	c2 := *c
	c2.Sig = nil
	return json.Marshal(c2)
}

// IssueCert signs a device certificate binding dev to this account.
//
// A non-positive ttl is an error, not a default: silently replacing a
// caller's negative TTL with a year-long certificate would rewrite intent
// (and made legitimately-signed expired certs impossible to construct, which
// is how the test caught it). Pass DefaultCertTTL explicitly.
func (a *Account) IssueCert(dev *Device, ttl time.Duration) (*DeviceCert, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("identity: cert ttl must be positive, got %v", ttl)
	}
	now := time.Now().UTC()
	c := &DeviceCert{
		DeviceID:  dev.ID(),
		DevicePub: dev.PublicKey(),
		AccountID: a.id,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
	}
	b, err := c.certSignable()
	if err != nil {
		return nil, err
	}
	c.Sig = ed25519.Sign(a.priv, b)
	return c, nil
}

var (
	// ErrCertBadSignature means the cert was altered after signing, or was
	// signed by a different key.
	ErrCertBadSignature = errors.New("identity: certificate signature invalid")
	// ErrCertExpired means the TTL has lapsed.
	ErrCertExpired = errors.New("identity: certificate expired")
	// ErrCertMismatch means the device ID does not match the device public
	// key — the ID is not derived from the key in the cert.
	ErrCertMismatch = errors.New("identity: device ID does not match device key")
)

// Verify checks a certificate against an account public key. It enforces the
// ID/key binding as well as the signature: a cert whose DeviceID is not
// derived from its DevicePub is broken no matter who signed it.
func (c *DeviceCert) Verify(accountPub ed25519.PublicKey) error {
	if IDFromKey(c.DevicePub) != c.DeviceID {
		return ErrCertMismatch
	}
	b, err := c.certSignable()
	if err != nil {
		return err
	}
	if !ed25519.Verify(accountPub, b, c.Sig) {
		return ErrCertBadSignature
	}
	if time.Now().UTC().After(c.ExpiresAt) {
		return ErrCertExpired
	}
	return nil
}

// SaveCert writes a certificate (public data, no encryption needed).
func SaveCert(c *DeviceCert, path string) error {
	blob, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, blob, 0o644)
}

// LoadCert reads a certificate.
func LoadCert(path string) (*DeviceCert, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c DeviceCert
	if err := json.Unmarshal(blob, &c); err != nil {
		return nil, fmt.Errorf("identity: %s: not a certificate: %w", path, err)
	}
	return &c, nil
}

func dirOf(p string) string {
	d := p
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == os.PathSeparator {
			d = p[:i]
			break
		}
	}
	if d == p {
		d = "."
	}
	return d
}
