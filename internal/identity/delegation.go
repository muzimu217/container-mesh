// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

// Delegation is the account's authorisation for a DELEGATOR key to sign
// device certificates.
//
// Why it exists: the account owner wants ONE string a member's device can
// join with, instead of signing a certificate per device and handing it
// over. The account key must not travel with the team — a credential
// carrying it would make every holder a permanent certificate authority for
// the whole account. So the credential carries a delegator key instead.
//
// It is deliberately the SAME object as the credential's public half: the
// account signs it once, the discovery node stores it in the roster, and
// every member verifies the delegated certificate against it with the same
// account signature. A separate record with its own signature could not be
// produced by the node (it holds no account key), and a delegated device
// whose authorisation cannot be verified would be dropped from every view —
// in the network and invisible to it.
//
// The bounds are structural, not advisory: a delegation without a device
// quota or without an expiry is refused, so a leaked credential is a
// bounded leak rather than a permanent key.

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
//
// The JSON shape is the credential's public half, and the signature covers
// exactly these fields — one canonical form, one signature, one rule for
// everyone who has to check it.
type Delegation struct {
	// Version is the format version.
	Version int `json:"version"`
	// ID identifies this delegation; it is also the quota bucket.
	ID string `json:"id"`
	// AccountID and AccountPub say which account the delegation speaks for.
	AccountID  DeviceID          `json:"account_id"`
	AccountPub ed25519.PublicKey `json:"account_pub"`
	// DelegatorPub is the ONLY key allowed to sign certificates under this
	// delegation.
	DelegatorPub ed25519.PublicKey `json:"delegator_pub"`
	// DiscoveryAddrs are the seed discovery endpoints a joiner should use.
	DiscoveryAddrs []string `json:"disc"`
	// IssuedAt, ExpiresAt and MaxDevices are the bounds.
	IssuedAt   time.Time `json:"issued_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	MaxDevices int       `json:"max_devices"`
	// Sig is the ACCOUNT's signature over the canonical form of the fields
	// above (the struct without Sig).
	Sig []byte `json:"sig"`
}

// delegable returns the exact bytes the signature covers.
func (d *Delegation) delegable() ([]byte, error) {
	d2 := *d
	d2.Sig = nil
	return json.Marshal(d2)
}

// Signable returns the exact bytes the account signature covers.
//
// It is exported because the credential package signs the SAME struct: the
// credential's public half and the roster's delegation record are one
// object, and a second implementation of the canonical form would be a
// second way to get the bytes wrong.
func (d *Delegation) Signable() ([]byte, error) { return d.delegable() }

// SignDelegation records the account's signature over the delegation.
func (a *Account) SignDelegation(d *Delegation) error {
	if len(d.DelegatorPub) != ed25519.PublicKeySize {
		return fmt.Errorf("identity: delegation public key must be %d bytes, got %d", ed25519.PublicKeySize, len(d.DelegatorPub))
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
	if len(d.DelegatorPub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: delegation public key is %d bytes", ErrDelegationBadSignature, len(d.DelegatorPub))
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
