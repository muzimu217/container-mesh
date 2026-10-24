// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package identity implements the mesh's identity layer: per-device key
// pairs, an account that signs device certificates, and the versioned
// member list that revocation operates on.
//
// Design constraints this package is responsible for upholding:
//
//   - A private key never leaves the machine it was generated on. The only
//     exported form is an encrypted file, and the key is encrypted with a
//     key derived from a passphrase, because the target environments
//     (containers) have no keychain to defer to.
//   - A device ID is derived from the public key, so it is self-certifying:
//     given an ID and a key, anyone can check they match without a server.
//   - The account key is NOT used on the data plane. If it were, compromising
//     one device would compromise all of them, which is exactly what
//     per-device keys exist to prevent.
//
// This package imports no other project package: it is the foundation, and
// keeping it dependency-free keeps it testable and portable.
package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// idEncoding is the canonical alphabet for device and account IDs.
//
// Lowercase RFC 4648 base32 without padding. Lowercase because IDs are meant
// to be read and typed by humans, and mixed case doubles the chance of a
// transcription error.
var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// DeviceIDLen is the exact length of a DeviceID: 16 base32 characters,
// carrying the first 80 bits of the SHA-256 digest of the public key.
//
// 16 CHARACTERS, not 16 bytes and not hex. This is spelled out because
// "[:16]" means three different things depending on whether it is applied to
// a digest slice, a hex string, or a base32 string — and every consumer of
// this package must agree byte-for-byte (MESH-DESIGN §2.1).
const DeviceIDLen = 16

// DeviceID identifies one device. Self-certifying: derived from the public
// key, so binding is verifiable offline.
type DeviceID string

// Valid reports whether the ID is well-formed (length and alphabet).
func (id DeviceID) Valid() bool {
	if len(id) != DeviceIDLen {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz234567", c) {
			return false
		}
	}
	return true
}

func (id DeviceID) String() string { return string(id) }

// IDFromKey derives the canonical device ID from an Ed25519 public key.
func IDFromKey(pub ed25519.PublicKey) DeviceID {
	sum := sha256.Sum256(pub)
	return DeviceID(idEncoding.EncodeToString(sum[:])[:DeviceIDLen])
}

// Device is one machine's long-term identity.
type Device struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	id   DeviceID
	// CreatedAt records when this identity was generated. Not secret; stored
	// in the encrypted file alongside the key.
	CreatedAt time.Time
}

// ErrInvalidKey is returned when a key pair is malformed.
var ErrInvalidKey = errors.New("identity: invalid key")

// Generate creates a fresh device identity.
func Generate() (*Device, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	return &Device{priv: priv, pub: pub, id: IDFromKey(pub), CreatedAt: time.Now().UTC()}, nil
}

// ID reports the device's identifier.
func (d *Device) ID() DeviceID { return d.id }

// PublicKey reports the device's Ed25519 public key.
func (d *Device) PublicKey() ed25519.PublicKey { return d.pub }

// Sign signs msg with the device key.
func (d *Device) Sign(msg []byte) []byte { return ed25519.Sign(d.priv, msg) }

// VerifyDevice checks a signature against a device key.
func VerifyDevice(pub ed25519.PublicKey, msg, sig []byte) bool {
	return ed25519.Verify(pub, msg, sig)
}

// filePayload is the plaintext structure that gets encrypted on disk.
type filePayload struct {
	PrivateKey []byte    `json:"priv"`
	CreatedAt  time.Time `json:"created_at"`
}

// identityFile is the on-disk envelope.
type identityFile struct {
	Version int    `json:"version"`
	KDF     kdfCfg `json:"kdf"`
	Nonce   []byte `json:"nonce"`
	// Cipher is AES-256-GCM over filePayload. GCM because it is the one AEAD
	// available in the standard library and hardware-accelerated everywhere
	// this project runs.
	Cipher []byte `json:"cipher"`
}

type kdfCfg struct {
	Algo    string `json:"algo"` // "argon2id"
	Salt    []byte `json:"salt"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"`
	Threads uint8  `json:"threads"`
}

// Default KDF parameters. Memory-hard is the point of Argon2id; 64 MiB is
// noticeable but acceptable for an operation a user performs rarely (unlock
// at startup), and it puts brute-force off the table for stolen files.
const (
	kdfTime    uint32 = 1
	kdfMemory  uint32 = 64 * 1024 // KiB
	kdfThreads uint8  = 2
)

// identityPath returns the default file path for a name under dir.
func identityPath(dir, name string) string { return filepath.Join(dir, name) }

// DefaultPaths are the conventional file names.
const (
	DeviceFileName  = "device.json"
	AccountFileName = "account.json"
	MembersFileName = "members.json"
	CertSuffix      = ".cert.json"
	DefaultStateDir = ".cmesh"
)

// Save writes the encrypted identity file. The passphrase never touches
// disk; only the Argon2id-derived key encrypts the payload.
//
// The file is created with 0600: it holds a private key, and group/world
// read bits would be a standing invitation.
func (d *Device) Save(path, passphrase string) error {
	if len(passphrase) < 8 {
		return errors.New("identity: passphrase must be at least 8 characters")
	}
	payload, err := json.Marshal(filePayload{PrivateKey: d.priv, CreatedAt: d.CreatedAt})
	if err != nil {
		return err
	}
	salt, err := randomBytes(16)
	if err != nil {
		return err
	}
	cfg := kdfCfg{Algo: "argon2id", Salt: salt, Time: kdfTime, Memory: kdfMemory, Threads: kdfThreads}
	aead, err := newAEAD(deriveKey(passphrase, cfg))
	if err != nil {
		return err
	}
	nonce, err := randomBytes(nonceSize)
	if err != nil {
		return err
	}
	blob, err := json.Marshal(identityFile{
		Version: 1,
		KDF:     cfg,
		Nonce:   nonce,
		Cipher:  aead.Seal(nil, nonce, payload, nil),
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, blob, 0o600)
}

// LoadDevice reads and decrypts a device identity file.
//
// A wrong passphrase surfaces as an AEAD authentication failure; the caller
// should treat that as "try again", not as corruption.
func LoadDevice(path, passphrase string) (*Device, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f identityFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("identity: %s: not an identity file: %w", path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("identity: %s: unsupported version %d", path, f.Version)
	}
	if f.KDF.Algo != "argon2id" {
		return nil, fmt.Errorf("identity: %s: unknown KDF %q", path, f.KDF.Algo)
	}
	aead, err := newAEAD(deriveKey(passphrase, f.KDF))
	if err != nil {
		return nil, err
	}
	payload, err := aead.Open(nil, f.Nonce, f.Cipher, nil)
	if err != nil {
		return nil, fmt.Errorf("identity: %s: wrong passphrase or corrupted file", path)
	}
	var p filePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("identity: %s: payload corrupted: %w", path, err)
	}
	priv := ed25519.PrivateKey(p.PrivateKey)
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: private key has %d bytes, want %d",
			ErrInvalidKey, len(priv), ed25519.PrivateKeySize)
	}
	pub := priv.Public().(ed25519.PublicKey)
	return &Device{priv: priv, pub: pub, id: IDFromKey(pub), CreatedAt: p.CreatedAt}, nil
}

// IdentityPath joins a directory and file name; exported for the CLI and tests.
func IdentityPath(dir, name string) string { return identityPath(dir, name) }
