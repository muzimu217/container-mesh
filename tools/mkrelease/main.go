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
	flag.Parse()

	if *version == "" {
		fatal("--version is required")
	}
	if !safeName(*version) {
		fatal(fmt.Sprintf("unsafe version string: %q", *version))
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err.Error())
	}
	entries, err := os.ReadDir(*in)
	if err != nil {
		fatal(err.Error())
	}

	var artefacts []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "cmesh-") {
			continue
		}
		src := filepath.Join(*in, e.Name())
		dst := filepath.Join(*out, e.Name())
		if err := copyFile(src, dst, 0o755); err != nil {
			fatal(err.Error())
		}
		artefacts = append(artefacts, e.Name())
	}
	if len(artefacts) == 0 {
		fatal(fmt.Sprintf("no cmesh-* binaries found in %s", *in))
	}
	sort.Strings(artefacts)

	// The installer ships alongside the binaries.
	installer := filepath.Join("packaging", "install.sh")
	if _, err := os.Stat(installer); err == nil {
		if err := copyFile(installer, filepath.Join(*out, "install.sh"), 0o755); err != nil {
			fatal(err.Error())
		}
	}

	// Per-platform archives: binary + installer + licence.
	for _, name := range artefacts {
		archive := filepath.Join(*out, name+".tar.gz")
		if err := writeArchive(archive, *out, name); err != nil {
			fatal(err.Error())
		}
	}

	// Checksums over everything a user may download.
	sums := filepath.Join(*out, "SHA256SUMS")
	if err := writeSums(sums, *out); err != nil {
		fatal(err.Error())
	}

	fmt.Printf("wrote %s\n", *out)
	for _, name := range artefacts {
		fmt.Printf("  %s\n", name)
	}
	fmt.Printf("  %s\n", filepath.Base(sums))
	fmt.Println("\npublish these files as GitHub Release assets.")
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
