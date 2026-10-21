// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/wsconn"
)

// writeTestCert creates a self-signed certificate for 127.0.0.1 and returns
// its file paths.
func writeTestCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	cf, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	_ = cf.Close()

	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	kf, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}); err != nil {
		t.Fatal(err)
	}
	_ = kf.Close()
	return certPath, keyPath
}

// TestRelayServesTLSDirectly: with a certificate configured the relay speaks
// wss:// itself, so a deployment can own port 443 without a reverse proxy.
func TestRelayServesTLSDirectly(t *testing.T) {
	certPath, keyPath := writeTestCert(t)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	cfg := DefaultConfig()
	cfg.Addr = addr
	cfg.TLSCert = certPath
	cfg.TLSKey = keyPath
	srv := New(cfg)
	hs := srv.HTTPServer()
	go func() { _ = hs.ListenAndServeTLS(certPath, keyPath) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	}()
	time.Sleep(200 * time.Millisecond)

	// The health endpoint must answer over TLS.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get("https://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("https healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz status = %d", resp.StatusCode)
	}

	// The WebSocket endpoint must be reachable over the same TLS listener.
	// wsconn no longer has an insecure-skip-verify switch (removed during
	// hardening), so this checks the HTTP upgrade handshake rather than a full
	// session: a plain GET to /ws without upgrade headers must not 404.
	resp2, err := client.Get("https://" + addr + "/ws")
	if err != nil {
		t.Fatalf("https /ws: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode == http.StatusNotFound {
		t.Error("/ws is not routed on the TLS listener")
	}

	_ = wsconn.ErrClosed
}
