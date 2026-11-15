// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Command cmesh-install fetches and installs a verified cmesh binary for this
// machine. The install layout is deliberately separate from runtime state:
//
//	<install>/versions/<version>/cmesh
//	<install>/current -> versions/<version>
//	<install>/cmesh -> current/cmesh
//
// Runtime identities, certificates, and member lists remain under ~/.cmesh (or
// the operator's --state directory); this tool never creates or reads them.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
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
	maxDownload    = 256 << 20
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--uninstall" {
		fs := flag.NewFlagSet("cmesh-install --uninstall", flag.ExitOnError)
		purge := fs.Bool("purge", false, "also remove the runtime state directory")
		_ = fs.Parse(os.Args[2:])
		if err := uninstall(envOr("CMESH_INSTALL", "."), envOr("CMESH_STATE", defaultStateDir()), *purge); err != nil {
			fmt.Fprintf(os.Stderr, "cmesh-install: %v\n", err)
			os.Exit(1)
		}
		return
	}
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
	if parsed, err := url.Parse(base); err != nil || parsed.Host == "" {
		return fmt.Errorf("CMESH_BASE_URL is not a URL")
	}
	if !safeVersion(version) {
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

	if err := installPayload(dir, version, payload); err != nil {
		return err
	}
	fmt.Printf("  installed: %s\n", filepath.Join(dir, "current", "cmesh"))
	return nil
}

func releaseURLs(base, version, asset string) (sums, binary string) {
	if version == "latest" {
		return base + "/latest/download/SHA256SUMS", base + "/latest/download/" + asset
	}
	return base + "/download/" + version + "/SHA256SUMS", base + "/download/" + version + "/" + asset
}

func fetch(client *http.Client, rawurl string) ([]byte, error) {
	resp, err := client.Get(rawurl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", rawurl, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("empty response")
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("response exceeds %d bytes", maxDownload)
	}
	return data, nil
}

func checksumFor(sums []byte, asset string) string {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset && len(fields[0]) == sha256.Size*2 {
			if _, err := hex.DecodeString(fields[0]); err == nil {
				return strings.ToLower(fields[0])
			}
		}
	}
	return ""
}

func safeVersion(version string) bool {
	if version == "" || len(version) > 64 {
		return false
	}
	for _, r := range version {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' || r == '+' {
			continue
		}
		return false
	}
	return true
}

// installPayload is the post-verification commit step. It only changes paths
// below root and leaves runtime state untouched.
func installPayload(root, version string, payload []byte) error {
	if !safeVersion(version) {
		return fmt.Errorf("unsafe version string: %q", version)
	}
	if err := os.MkdirAll(filepath.Join(root, "versions"), 0o755); err != nil {
		return fmt.Errorf("cannot create install directory: %w", err)
	}
	stage, err := os.MkdirTemp(filepath.Join(root, "versions"), ".cmesh-install-")
	if err != nil {
		return fmt.Errorf("cannot create staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	binary := filepath.Join(stage, "cmesh")
	if err := os.WriteFile(binary, payload, 0o755); err != nil {
		return fmt.Errorf("cannot stage binary: %w", err)
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		return fmt.Errorf("cannot make staged binary executable: %w", err)
	}

	versions := filepath.Join(root, "versions")
	dest := filepath.Join(versions, version)
	backup := ""
	if _, err := os.Lstat(dest); err == nil {
		backup, err = os.MkdirTemp(versions, ".cmesh-old-")
		if err != nil {
			return fmt.Errorf("cannot prepare version replacement: %w", err)
		}
		os.Remove(backup)
		if err := os.Rename(dest, backup); err != nil {
			return fmt.Errorf("cannot replace existing version %s: %w", version, err)
		}
	}
	if err := os.Rename(stage, dest); err != nil {
		if backup != "" {
			_ = os.Rename(backup, dest)
		}
		return fmt.Errorf("cannot commit version %s: %w", version, err)
	}
	if err := replaceSymlink(filepath.Join(root, "cmesh"), filepath.Join("current", "cmesh")); err != nil {
		_ = os.RemoveAll(dest)
		if backup != "" {
			_ = os.Rename(backup, dest)
		}
		return fmt.Errorf("cannot switch cmesh link: %w", err)
	}
	if err := replaceSymlink(filepath.Join(root, "current"), filepath.Join("versions", version)); err != nil {
		return fmt.Errorf("cannot switch current version: %w", err)
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	marker := filepath.Join(root, ".cmesh-install-root")
	_ = os.WriteFile(marker, []byte("container-mesh managed install\n"), 0o644)
	return nil
}

func replaceSymlink(path, target string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cmesh-link-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Remove(tmpPath); err != nil {
		return err
	}
	if err := os.Symlink(target, tmpPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func uninstall(root, state string, purge bool) error {
	if root == "" || filepath.Clean(root) == string(filepath.Separator) {
		return errors.New("refusing to uninstall from an empty path or filesystem root")
	}
	marker := filepath.Join(root, ".cmesh-install-root")
	if _, err := os.Stat(marker); err != nil {
		return fmt.Errorf("%s is not a cmesh install root", root)
	}
	if purge {
		if err := os.RemoveAll(root); err != nil {
			return fmt.Errorf("cannot purge install root: %w", err)
		}
		if state != "" && filepath.Clean(state) != filepath.Clean(root) {
			if err := os.RemoveAll(state); err != nil {
				return fmt.Errorf("cannot purge state directory: %w", err)
			}
		}
		return nil
	}
	for _, name := range []string{"cmesh", "current", "versions", ".cmesh-install-root"} {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			return fmt.Errorf("cannot remove %s: %w", name, err)
		}
	}
	return nil
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cmesh")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
