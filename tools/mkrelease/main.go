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
// Every cmesh-* input must be named cmesh-<version>-<os>-<arch>[.exe] with
// the version given to --version and a platform from the release matrix
// (linux/darwin/windows × amd64/arm64); anything else in the input directory
// is refused, so a stale binary or a pre-built cmesh-latest-* alias can
// never ride along into the release.
//
// The archives ship a licence. It is resolved from the release inputs, not
// from the working directory: an explicit --license path wins, otherwise
// LICENSE is searched upwards from the --installer directory, stopping at
// the go.mod repository root.
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
	license := flag.String("license", "",
		"licence file to ship inside the archives (default: LICENSE found upwards from --installer)")
	flag.Parse()

	if err := assemble(*in, *out, *version, *installer, *license); err != nil {
		fmt.Fprintf(os.Stderr, "mkrelease: %v\n", err)
		fmt.Fprintf(os.Stderr, "mkrelease: no release was committed\n")
		os.Exit(1)
	}
}

// assemble builds the release artefacts into out. It returns an error
// instead of exiting so the caller can clean up after a failure.
func assemble(in, out, version, installerPath, licenseFlag string) error {
	if version == "" {
		return errors.New("--version is required")
	}
	if !safeName(version) {
		return fmt.Errorf("unsafe version string: %q", version)
	}
	if version == "latest" {
		return errors.New(`--version must not be "latest": the versioned binaries would collide with the cmesh-latest-* aliases mkrelease derives`)
	}
	entries, err := os.ReadDir(in)
	if err != nil {
		return err
	}

	var artefacts []string
	// Validate every required input before creating any output. The caller may
	// safely remove a stale output after an error, but this check also prevents
	// a failed invocation from destroying a previously published directory.
	if _, err := os.Stat(installerPath); err != nil {
		return fmt.Errorf(
			"%s is missing: a release must ship an installer; refusing to produce a half-product", installerPath)
	}
	licensePath, err := resolveLicense(licenseFlag, installerPath)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "cmesh-") {
			continue
		}
		if info, err := e.Info(); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("release input %s is not a regular file", e.Name())
		}
		inputVersion, ok := parseBinaryName(e.Name())
		if !ok {
			return fmt.Errorf(
				"release input %s does not match cmesh-<version>-<os>-<arch>[.exe] (os: linux/darwin/windows, arch: amd64/arm64)", e.Name())
		}
		if inputVersion == "latest" {
			return fmt.Errorf(
				"release input %s is a pre-built latest alias: remove it from %s, mkrelease derives the latest aliases itself", e.Name(), in)
		}
		if inputVersion != version {
			return fmt.Errorf(
				"release input %s is for version %s, but this release is %s", e.Name(), inputVersion, version)
		}
		artefacts = append(artefacts, e.Name())
	}
	if len(artefacts) == 0 {
		return fmt.Errorf("no cmesh-* binaries found in %s", in)
	}
	sort.Strings(artefacts)

	parent := filepath.Dir(filepath.Clean(out))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".mkrelease-")
	if err != nil {
		return fmt.Errorf("create release staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	for _, name := range artefacts {
		if err := copyFile(filepath.Join(in, name), filepath.Join(stage, name), 0o755); err != nil {
			return err
		}
	}
	if err := copyFile(installerPath, filepath.Join(stage, "install.sh"), 0o755); err != nil {
		return err
	}

	for _, name := range artefacts {
		alias, ok := latestAlias(name)
		if !ok {
			continue
		}
		if err := copyFile(filepath.Join(stage, name), filepath.Join(stage, alias), 0o755); err != nil {
			return err
		}
	}
	for _, name := range artefacts {
		archive := filepath.Join(stage, name+".tar.gz")
		if err := writeArchive(archive, stage, name, licensePath); err != nil {
			return err
		}
	}
	if err := writeSums(filepath.Join(stage, "SHA256SUMS"), stage); err != nil {
		return err
	}

	// Commit the complete tree with one same-filesystem rename. Never delete an
	// existing output until the new release has been fully staged and checksummed.
	old := ""
	if _, err := os.Lstat(out); err == nil {
		old, err = os.MkdirTemp(parent, ".mkrelease-old-")
		if err != nil {
			return err
		}
		os.Remove(old)
		if err := os.Rename(out, old); err != nil {
			return fmt.Errorf("move existing release aside: %w", err)
		}
	}
	if err := os.Rename(stage, out); err != nil {
		if old != "" {
			_ = os.Rename(old, out)
		}
		return fmt.Errorf("commit release: %w", err)
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}

	fmt.Printf("wrote %s\n", out)
	for _, name := range artefacts {
		fmt.Printf("  %s\n", name)
	}
	fmt.Printf("  SHA256SUMS\n")
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

// parseBinaryName parses a release input name of the form
// cmesh-<version>-<os>-<arch>[.exe] and returns its version. The platform
// suffix is matched against the fixed release matrix only, so a name that
// merely looks like a binary (cmesh-v1.0.0-linux-amd64.tar.gz, a build
// script, ...) is rejected rather than guessed into the release.
func parseBinaryName(name string) (version string, ok bool) {
	base := strings.TrimSuffix(name, ".exe")
	if !strings.HasPrefix(base, "cmesh-") {
		return "", false
	}
	for _, o := range []string{"linux", "darwin", "windows"} {
		for _, a := range []string{"amd64", "arm64"} {
			suffix := "-" + o + "-" + a
			if strings.HasSuffix(base, suffix) {
				version = strings.TrimSuffix(strings.TrimPrefix(base, "cmesh-"), suffix)
				if version == "" {
					return "", false
				}
				return version, true
			}
		}
	}
	return "", false
}

// resolveLicense returns the path of the licence file to ship inside the
// archives ("" means ship none). An explicit --license path wins and must
// exist; otherwise LICENSE is searched upwards from the installer's own
// directory — the release inputs decide, never the current working
// directory — stopping at the go.mod repository root so a licence from an
// unrelated parent directory is not shipped by accident.
func resolveLicense(licenseFlag, installerPath string) (string, error) {
	if licenseFlag != "" {
		info, err := os.Stat(licenseFlag)
		if err != nil {
			return "", fmt.Errorf("licence file %s: %w", licenseFlag, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("licence file %s is not a regular file", licenseFlag)
		}
		return licenseFlag, nil
	}
	abs, err := filepath.Abs(installerPath)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(abs)
	for level := 0; level < 32; level++ {
		if info, err := os.Stat(filepath.Join(dir, "LICENSE")); err == nil && info.Mode().IsRegular() {
			return filepath.Join(dir, "LICENSE"), nil
		}
		// Repository boundary: do not reach past this source tree.
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return "", nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
	return "", nil
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

// writeArchive packs the binary, the installer and the licence. licensePath
// may be empty, in which case the archive ships no licence.
func writeArchive(dst, dir, binary, licensePath string) error {
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
	if licensePath != "" {
		if err := add("LICENSE", licensePath, 0o644); err != nil {
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
