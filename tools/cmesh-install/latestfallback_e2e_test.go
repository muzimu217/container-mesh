package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// checksumFile builds the SHA256SUMS line for a payload.
func checksumFile(t *testing.T, payload []byte, asset string) string {
	t.Helper()
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]) + "  " + asset + "\n"
}

// ★ THE FALLBACK MUST ACTUALLY FETCH THE INSTALLABLE RELEASE.
//
// The unit tests above pin the resolution ORDER. This one proves the
// whole path: the "latest" URL 404s, the release API names a
// prerelease, and the installer then downloads THAT release's
// checksums and binary and installs it.
//
// It is the shape the real repository is in right now:
//
//	/releases/latest/download/SHA256SUMS        → 404
//	/releases/download/v1.0.0-rc2/SHA256SUMS   → 200
//
// A "latest" that cannot find a release is a "latest" that hands a new
// user nothing at all.
func TestTheLatestFallbackInstallsTheReleaseTheAPINames(t *testing.T) {
	payload := []byte("#!/bin/sh\necho cmesh\n")
	// The asset name is built from the RUNNING platform, so the fixture
	// must serve the asset this machine would ask for.
	asset := fmt.Sprintf("cmesh-v1.0.0-rc2-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	sums := checksumFile(t, payload, asset)

	var apiHits, latestHits, taggedHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases":
			apiHits++
			w.Write([]byte(`[{"tag_name":"v1.0.0-rc2","draft":false,"prerelease":true}]`))
		case r.URL.Path == "/latest/download/SHA256SUMS":
			latestHits++
			http.NotFound(w, r)
		case r.URL.Path == "/download/v1.0.0-rc2/SHA256SUMS":
			taggedHits++
			w.Write([]byte(sums))
		case r.URL.Path == "/download/v1.0.0-rc2/"+asset:
			taggedHits++
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// The installer resolves the release through the API; point it at
	// the fixture's API path by overriding the package's API URL for
	// the duration of the test.
	oldAPI := releaseAPIURLOverride
	releaseAPIURLOverride = srv.URL + "/releases"
	defer func() { releaseAPIURLOverride = oldAPI }()

	dir := t.TempDir()
	if err := installFrom(srv.URL, "latest", dir); err != nil {
		t.Fatalf("install: %v", err)
	}
	if apiHits == 0 {
		t.Fatal("the release API was never consulted")
	}
	if latestHits == 0 {
		t.Fatal("the latest URL was never tried")
	}
	if taggedHits == 0 {
		t.Fatal("the release the API named was never fetched")
	}

	// And the binary must be installed, in the version directory the
	// API named.
	bin := filepath.Join(dir, "versions", "v1.0.0-rc2", "cmesh")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("the binary was not installed at %s: %v", bin, err)
	}
}

// ★ AND A PLAINTEXT BASE URL MUST STILL BE REFUSED FOR A REMOTE HOST.
//
// The fallback path downloads an executable and verifies it against a
// checksum that arrives over the SAME connection — so a plaintext
// connection to a remote host could be tampered with on the way.
// Loopback is exempt because it is not on the way.
func TestAPlaintextRemoteBaseURLIsRefused(t *testing.T) {
	if err := installFrom("http://example.invalid/releases", "latest", t.TempDir()); err == nil {
		t.Fatal("the installer accepted a plaintext base URL for a remote host")
	}
}
