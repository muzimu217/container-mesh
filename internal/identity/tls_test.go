// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

import (
	"crypto/ed25519"
	"crypto/x509"
	"testing"
	"time"
)

// TestTLSCertificate pins the device-certificate contract: self-signed,
// Ed25519 key = identity key, CN = device ID, and it verifies as its own
// root when presented in an explicit pool (the form Go TLS reaches after
// skipping the default system-PKI verification — see internal/mesh).
func TestTLSCertificate(t *testing.T) {
	dev, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	c, err := dev.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Certificate) != 1 {
		t.Fatalf("cert chain length %d, want 1", len(c.Certificate))
	}
	parsed, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := parsed.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("cert public key is %T, want ed25519.PublicKey", parsed.PublicKey)
	}
	if !pub.Equal(dev.PublicKey()) {
		t.Fatal("certificate key must equal the device key")
	}
	if parsed.Subject.CommonName != dev.ID().String() {
		t.Fatalf("CN %q, want device ID %q", parsed.Subject.CommonName, dev.ID())
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	if _, err := parsed.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: time.Now()}); err != nil {
		t.Fatalf("device cert fails verification against itself as root: %v", err)
	}
	if !VerifyDevice(dev.PublicKey(), []byte("msg"), dev.Sign([]byte("msg"))) {
		t.Fatal("device key must still sign/verify normally")
	}
}
