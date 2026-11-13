// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Command cmesh-install fetches a verified cmesh binary for this machine.
//
// It is published per platform as a release asset, so installing cmesh
// never means running a downloaded script:
//
//	https://github.com/muzimu217/container-mesh/releases/latest/download/cmesh-install-linux-amd64
//
// What it does:
//  1. reads the OS and CPU architecture it was built for;
//  2. downloads the matching cmesh binary from the release;
//  3. verifies its SHA-256 against the published SHA256SUMS;
//  4. writes it into the install directory.
//
// The binary is written only after its checksum matches: a corrupted or
// tampered download fails with nothing written. The default install
// directory is the current one — cmesh's home is containers and home
// directories, and writing into a system directory is the operator's
// decision, not this program's.
//
// Environment:
//
//	CMESH_INSTALL   install directory (default: current directory)
//	CMESH_VERSION   release tag to fetch (default: latest)
//	CMESH_BASE_URL  release base URL (default the project's GitHub releases)
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://github.com/muzimu217/container-mesh/releases"
	maxDownload    = 256 << 20 // a cmesh binary is ~10 MB; this is a ceiling
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "cmesh-install: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	base := envOr("CMESH_BASE_URL", defaultBaseURL)
	version := envOr("CMESH_VERSION", "latest")
	dir := envOr("CMESH_INSTALL", ".")

	if !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("CMESH_BASE_URL must be https://")
	}
	if _, err := url.Parse(base); err != nil {
		return fmt.Errorf("CMESH_BASE_URL is not a URL: %w", err)
	}
	if version == "" || strings.ContainsAny(version, "./\\ \t\n") {
		return fmt.Errorf("CMESH_VERSION is not a usable release tag: %q", version)
	}

	asset := fmt.Sprintf("cmesh-%s-%s-%s", version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	sumsURL, binaryURL := releaseURLs(base, version, asset)

	fmt.Printf("cmesh-install\n")
	fmt.Printf("  platform : %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  asset    : %s\n", asset)
	fmt.Printf("  target   : %s\n", dir)

	client := &http.Client{Timeout: 120 * time.Second}

	sums, err := fetch(client, sumsURL)
	if err != nil {
		return fmt.Errorf("cannot fetch the release checksums: %w", err)
	}
	expected := checksumFor(sums, asset)
	if expected == "" {
		return fmt.Errorf("the release does not list %s", asset)
	}

	fmt.Println("  downloading...")
	payload, err := fetch(client, binaryURL)
	if err != nil {
		return fmt.Errorf("cannot fetch the release binary: %w", err)
	}

	sum := sha256.Sum256(payload)
	actual := hex.EncodeToString(sum[:])
	if actual != expected {
		return fmt.Errorf("checksum mismatch for %s (expected %s, got %s)", asset, expected, actual)
	}
	fmt.Println("  checksum ok")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot use the install directory %s: %w", dir, err)
	}
	target := filepath.Join(dir, "cmesh")
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	if err := os.WriteFile(target, payload, 0o755); err != nil {
		return fmt.Errorf("cannot write %s: %w", target, err)
	}
	fmt.Printf("  installed: %s\n", target)

	fmt.Println()
	fmt.Println("next steps:")
	fmt.Println("  cmesh init                       # create this device's identity")
	fmt.Println("  cmesh account create             # create the team account (anchor machine)")
	fmt.Println("  cmesh account device-add         # sign this device in")
	fmt.Println("  cmesh serve-discovery --panel    # run the anchor, then open the panel")
	return nil
}

// releaseURLs builds the two URLs the installer needs.
func releaseURLs(base, version, asset string) (sums, binary string) {
	if version == "latest" {
		return base + "/latest/download/SHA256SUMS", base + "/latest/download/" + asset
	}
	return base + "/download/" + version + "/SHA256SUMS", base + "/download/" + version + "/" + asset
}

// fetch downloads a URL, refusing anything unreasonably large.
func fetch(client *http.Client, rawurl string) ([]byte, error) {
	resp, err := client.Get(rawurl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", rawurl, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("empty response")
	}
	return data, nil
}

// checksumFor picks the checksum of one asset out of a SHA256SUMS file.
func checksumFor(sums []byte, asset string) string {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			return fields[0]
		}
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
