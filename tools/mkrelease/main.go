// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Command mkrelease assembles the release artefacts (M4 (d)).
//
// It does NOT compile anything: the binaries are built by `go build` (see
// packaging/build-demo.sh or the release workflow) and this tool turns that
// directory into what a GitHub Release needs:
//
//	cmesh-<version>-<os>-<arch>[.exe]    the binary itself
//	cmesh-<version>-<os>-<arch>.tar.gz   binary + installer + licence
//	SHA256SUMS                            checksums of every artefact
//	install.sh                            the installer
//
// Usage:
//
//	go run ./tools/mkrelease --in dist/bin --out dist/release --version v1.0.0
//
// The tool is deliberately Go rather than shell: it creates archives and
// checksums without handing names to a shell, so a version string can never
// become a command.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	in := flag.String("in", "dist/bin", "directory holding the built binaries")
	out := flag.String("out", "dist/release", "directory to write the artefacts to")
	version := flag.String("version", "", "release version (e.g. v1.0.0); required")
	installer := flag.String("installer", filepath.Join("packaging", "install.sh"),
		"installer script to ship with the release")
	flag.Parse()

	if err := assemble(*in, *out, *version, *installer); err != nil {
		// A failed release must leave NOTHING behind. Half-written artefacts
		// are worse than none: they look valid to a checksum-less upload
		// step, and the failure is invisible downstream.
		_ = os.RemoveAll(*out)
		fmt.Fprintf(os.Stderr, "mkrelease: %v\n", err)
		fmt.Fprintf(os.Stderr, "mkrelease: failed; removed the partial output at %s\n", *out)
		os.Exit(1)
	}
}

// assemble builds the release artefacts into out. It returns an error
// instead of exiting so the caller can clean up after a failure.
func assemble(in, out, version, installerPath string) error {
	if version == "" {
		return errors.New("--version is required")
	}
	if !safeName(version) {
		return fmt.Errorf("unsafe version string: %q", version)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(in)
	if err != nil {
		return err
	}

	var artefacts []string
	// The installer ships alongside the binaries. It is REQUIRED: a release
	// without it is not a release. The earlier version skipped the copy when
	// the file was absent and then failed while building the first archive —
	// leaving copied binaries and a truncated tar.gz behind, with no
	// SHA256SUMS. That half-product looks valid to `gzip -t` and to an
	// automated upload step, so the failure was invisible downstream
	// (doubao-cloud-vm-01, round 56). Fail BEFORE touching the output
	// directory, and name the missing file.
	if _, err := os.Stat(installerPath); err != nil {
		return fmt.Errorf(
			"%s is missing: a release must ship an installer; refusing to produce a half-product", installerPath)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "cmesh-") {
			continue
		}
		src := filepath.Join(in, e.Name())
		dst := filepath.Join(out, e.Name())
		if err := copyFile(src, dst, 0o755); err != nil {
			return err
		}
		artefacts = append(artefacts, e.Name())
	}
	if len(artefacts) == 0 {
		return fmt.Errorf("no cmesh-* binaries found in %s", in)
	}
	sort.Strings(artefacts)

	if err := copyFile(installerPath, filepath.Join(out, "install.sh"), 0o755); err != nil {
		return err
	}

	// A "latest" alias for every binary.
	//
	// GitHub's `releases/latest/download/<name>` only resolves for an asset
	// whose name is literally "latest", so the versioned name alone leaves
	// the documented default URL dead (doubao-cloud-vm-01, round 56:
	// "mkrelease 只产 cmesh-<version>-…，仓库无任何 latest- 别名逻辑").
	// Emitting the alias here means the release job uploads the directory
	// as-is and the default URL works.
	for _, name := range artefacts {
		alias, ok := latestAlias(name)
		if !ok {
			continue
		}
		if err := copyFile(filepath.Join(out, name), filepath.Join(out, alias), 0o755); err != nil {
			return err
		}
	}

	// Per-platform archives: binary + installer + licence.
	for _, name := range artefacts {
		archive := filepath.Join(out, name+".tar.gz")
		if err := writeArchive(archive, out, name); err != nil {
			return err
		}
	}

	// Checksums over everything a user may download.
	sums := filepath.Join(out, "SHA256SUMS")
	if err := writeSums(sums, out); err != nil {
		return err
	}

	fmt.Printf("wrote %s\n", out)
	for _, name := range artefacts {
		fmt.Printf("  %s\n", name)
	}
	fmt.Printf("  %s\n", filepath.Base(sums))
	fmt.Println("\npublish these files as GitHub Release assets.")
	return nil
}

// safeName reports whether s can be used in a file name without ambiguity.
func safeName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == '+':
		default:
			return false
		}
	}
	return true
}

// latestAlias maps a versioned binary name to its "latest" alias, e.g.
// cmesh-v1.0.0-linux-amd64 → cmesh-latest-linux-amd64.
//
// The platform suffix is matched from a fixed set, so an unexpected name is
// skipped rather than guessed: a wrong alias would point users at a file
// that does not exist for their platform.
func latestAlias(name string) (string, bool) {
	for _, os := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			suffix := "-" + os + "-" + arch
			if strings.HasSuffix(name, suffix+".exe") {
				return "cmesh-latest-" + os + "-" + arch + ".exe", true
			}
			if strings.HasSuffix(name, suffix) {
				return "cmesh-latest-" + os + "-" + arch, true
			}
		}
	}
	return "", false
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// writeArchive packs the binary, the installer and the licence.
func writeArchive(dst, dir, binary string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	base := binary + ".d"
	add := func(name, srcPath string, mode int64) error {
		info, err := os.Stat(srcPath)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:     base + "/" + name,
			Mode:     mode,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		in, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	}
	if err := add(binary, filepath.Join(dir, binary), 0o755); err != nil {
		return err
	}
	if err := add("install.sh", filepath.Join(dir, "install.sh"), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat("LICENSE"); err == nil {
		if err := add("LICENSE", "LICENSE", 0o644); err != nil {
			return err
		}
	}
	return nil
}

// writeSums writes the SHA256SUMS file for every artefact in dir.
func writeSums(dst, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || e.Name() == "SHA256SUMS" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		sum, err := sha256File(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, name)
	}
	return os.WriteFile(dst, []byte(b.String()), 0o644)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fatal(msg string) {
	fmt.Fprintf(os.Stderr, "mkrelease: %s\n", msg)
	os.Exit(1)
}
