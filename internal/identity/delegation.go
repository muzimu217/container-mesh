// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

// Delegation is the account's authorisation for a DELEGATOR key to sign
// device certificates on the account's behalf.
//
// Why it exists: the account owner wants ONE string that lets a team member's
// device join without a per-device human approval (M4 (a)). The account key
// itself must NOT travel with the team — that would make every holder a
// permanent certificate authority for the whole account. A delegation is the
// narrow, bounded form of that authority: one key, a device quota, an
// expiry, revocable by deleting the record.
//
// The anchor never holds the delegator's private key, so the "a discovery
// node cannot forge a member" boundary still holds (MESH-DESIGN §5.1): the
// node can only admit certificates the delegator signed, and only while the
// delegation record exists.

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrDelegationBadSignature means the delegation was altered after
	// signing, or was signed by a different account key.
	ErrDelegationBadSignature = errors.New("identity: delegation signature invalid")
	// ErrDelegationExpired means the delegation's validity window has lapsed.
	ErrDelegationExpired = errors.New("identity: delegation expired")
)

// Delegation authorises a delegator key to sign device certificates for an
// account, bounded by a device quota and an expiry.
type Delegation struct {
	// ID identifies the delegation for quota accounting. It is random, not
	// derived from the key: two delegations must not share a bucket.
	ID string `json:"id"`
	// Pub is the delegator's public key — the only key allowed to sign
	// certificates under this delegation.
	Pub ed25519.PublicKey `json:"pub"`
	// AccountID is the account this delegation speaks for.
	AccountID DeviceID `json:"account_id"`
	// MaxDevices is how many devices may be enrolled under this delegation.
	// Zero or negative means "no devices" — there is no implicit unlimited
	// mode: an unbounded delegation is a permanent credential authority.
	MaxDevices int `json:"max_devices"`
	// IssuedAt and ExpiresAt bound the delegation in time.
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Sig is the ACCOUNT's signature over the canonical form of the fields
	// above. Canonical form = the JSON of the struct with Sig omitted.
	Sig []byte `json:"sig"`
}

// delegable returns the exact bytes the signature covers.
func (d *Delegation) delegable() ([]byte, error) {
	d2 := *d
	d2.Sig = nil
	return json.Marshal(d2)
}

// SignDelegation records the account's signature over the delegation.
func (a *Account) SignDelegation(d *Delegation) error {
	if len(d.Pub) != ed25519.PublicKeySize {
		return fmt.Errorf("identity: delegation public key must be %d bytes, got %d", ed25519.PublicKeySize, len(d.Pub))
	}
	if d.MaxDevices <= 0 {
		return fmt.Errorf("identity: delegation must allow at least one device, got %d", d.MaxDevices)
	}
	if d.ID == "" {
		return errors.New("identity: delegation needs an id")
	}
	d.AccountID = a.id
	d.Sig = nil
	b, err := d.delegable()
	if err != nil {
		return err
	}
	d.Sig = ed25519.Sign(a.priv, b)
	return nil
}

// Verify checks the delegation against the account public key: the
// signature, the expiry, and that it speaks for the account whose key is
// being used.
func (d *Delegation) Verify(accountPub ed25519.PublicKey, now time.Time) error {
	if len(d.Pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: delegation public key is %d bytes", ErrDelegationBadSignature, len(d.Pub))
	}
	if AccountID(accountPub) != d.AccountID {
		return fmt.Errorf("%w: delegation is for account %s, not %s",
			ErrDelegationBadSignature, d.AccountID, AccountID(accountPub))
	}
	b, err := d.delegable()
	if err != nil {
		return err
	}
	if !ed25519.Verify(accountPub, b, d.Sig) {
		return ErrDelegationBadSignature
	}
	if now.After(d.ExpiresAt) {
		return fmt.Errorf("%w: expired at %s", ErrDelegationExpired, d.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}
