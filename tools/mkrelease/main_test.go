// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

// Tests for the release builder.
//
// The property that matters most: a FAILED release must leave nothing
// behind. The earlier version copied the binaries, then failed while
// building the first archive (the installer was missing), leaving copied
// binaries and a truncated tar.gz with no SHA256SUMS. That half-product
// passes `gzip -t` and looks like a release to an automated upload step —
// so the failure was invisible downstream (doubao-cloud-vm-01, round 56).

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// writeBinary makes a fake built binary.
func writeBinary(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestAssembleRefusesToShipWithoutInstaller: the missing installer must be
// a LOUD failure that names the file.
func TestAssembleRefusesToShipWithoutInstaller(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")

	err := assemble(bin, out, "v1.0.0", filepath.Join(root, "packaging", "install.sh"), "")
	if err == nil {
		t.Fatal("a release without an installer was produced")
	}
	if !strings.Contains(err.Error(), "install.sh") {
		t.Fatalf("the failure does not name the missing file: %v", err)
	}
}

// TestAssembleProducesCompleteRelease: with the installer present, the
// release has binaries, archives, the installer and a checksum manifest.
func TestAssembleProducesCompleteRelease(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")
	writeBinary(t, bin, "cmesh-v1.0.0-darwin-arm64")
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := assemble(bin, out, "v1.0.0", installer, ""); err != nil {
		t.Fatalf("the release failed: %v", err)
	}
	for _, name := range []string{
		"cmesh-v1.0.0-linux-amd64",
		"cmesh-v1.0.0-linux-amd64.tar.gz",
		"cmesh-v1.0.0-darwin-arm64",
		"cmesh-v1.0.0-darwin-arm64.tar.gz",
		"install.sh",
		"SHA256SUMS",
	} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("the release is missing %s: %v", name, err)
		}
	}
	// The manifest must cover every downloadable artefact.
	sums, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(sums)), "\n")
	// 2 versioned binaries + 2 aliases + 2 archives + install.sh
	if len(lines) != 7 {
		t.Fatalf("the manifest lists %d files, want 7:\n%s", len(lines), sums)
	}
}

// TestAssembleEmitsLatestAliases: the documented default download URL
// (releases/latest/download/cmesh-latest-<os>-<arch>) only resolves when the
// release carries an asset with that literal name (doubao-cloud-vm-01, round
// 56: the URL was documented but nothing produced it).
func TestAssembleEmitsLatestAliases(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")
	writeBinary(t, bin, "cmesh-v1.0.0-windows-amd64.exe")
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := assemble(bin, out, "v1.0.0", installer, ""); err != nil {
		t.Fatalf("the release failed: %v", err)
	}
	for _, alias := range []string{"cmesh-latest-linux-amd64", "cmesh-latest-windows-amd64.exe"} {
		p := filepath.Join(out, alias)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the release is missing the %s alias: %v", alias, err)
			continue
		}
		// The alias must be the SAME binary, not a stub.
		versioned := strings.Replace(alias, "cmesh-latest-", "cmesh-v1.0.0-", 1)
		a, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(out, versioned))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Errorf("the %s alias does not match %s", alias, versioned)
		}
	}
}

func TestAssemblePreservesExistingOutputOnInputFailure(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := assemble(bin, out, "v1.0.0", installer, ""); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(installer); err != nil {
		t.Fatal(err)
	}
	if err := assemble(bin, out, "v1.0.1", installer, ""); err == nil {
		t.Fatal("missing installer unexpectedly produced a release")
	}
	after, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("existing release was removed: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("failed staging changed the existing release")
	}
}

// TestAssembleRejectsUnsafeVersion: a version that could escape into a
// path or a command must be refused.
func TestAssembleRejectsUnsafeVersion(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"", "v1.0.0; rm -rf /", "../escape", "v1/0"} {
		if err := assemble(bin, filepath.Join(root, "out"), v, installer, ""); err == nil {
			t.Fatalf("the unsafe version %q was accepted", v)
		}
	}
}

// --- M4 batch B: input validation and licence resolution -------------------

// readArchive unpacks a .tar.gz the way `tar tz` (plus full extraction)
// would and returns the content of every regular-file entry by entry name.
// Reading every body catches truncation that a header-only listing misses.
func readArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s is not a gzip stream: %v", path, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("%s is not a readable tar stream: %v", path, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			t.Errorf("entry %s in %s is not a regular file (typeflag %c)", hdr.Name, path, hdr.Typeflag)
		}
		if strings.HasPrefix(hdr.Name, "/") || strings.Contains(hdr.Name, "..") {
			t.Errorf("entry %s in %s escapes the archive root", hdr.Name, path)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("entry %s in %s is truncated: %v", hdr.Name, path, err)
		}
		if _, dup := files[hdr.Name]; dup {
			t.Errorf("%s lists entry %s twice", path, hdr.Name)
		}
		files[hdr.Name] = string(body)
	}
	return files
}

// sortedKeys returns the sorted entry names of a readArchive result.
func sortedKeys(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestAssembleRejectsForeignVersionInputs: every cmesh-* input must be a
// binary of exactly the version being released on a supported platform. A
// stale binary from another version, a file from outside the platform
// matrix, or any cmesh-* file that is not a binary at all must abort the
// run instead of riding along into the release assets.
func TestAssembleRejectsForeignVersionInputs(t *testing.T) {
	root := t.TempDir()
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"cmesh-v9.9.9-linux-amd64",        // another version's binary
		"cmesh-v1.0.0-linux-mips64",       // arch outside the matrix
		"cmesh-v1.0.0-solaris-amd64",      // os outside the matrix
		"cmesh-v1.0.0-linux-amd64.tar.gz", // not a binary
		"cmesh-v1.0.0",                    // no platform
	} {
		bin := filepath.Join(root, "bin-"+strings.ReplaceAll(name, "/", "_"))
		writeBinary(t, bin, name)
		err := assemble(bin, filepath.Join(root, "out"), "v1.0.0", installer, "")
		if err == nil {
			t.Errorf("input %s was accepted into the release", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the failure does not name the offending input %s: %v", name, err)
		}
	}
}

// TestAssembleRejectsLatestAliasInputs: a pre-built cmesh-latest-* file in
// the input directory must abort the run. If it rode along, mkrelease would
// "derive" a latest alias for it that IS the file itself — copying a file
// onto its own alias truncates it to zero bytes and ships an empty latest
// asset (the alias self-copy pollution this guard exists for).
func TestAssembleRejectsLatestAliasInputs(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")
	writeBinary(t, bin, "cmesh-latest-linux-amd64") // stale alias from an earlier run
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := assemble(bin, out, "v1.0.0", installer, "")
	if err == nil {
		t.Fatal("a pre-built latest alias in the input directory was accepted")
	}
	if !strings.Contains(err.Error(), "latest") {
		t.Fatalf("the failure does not mention the latest alias: %v", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Errorf("a failed run left output behind: %v", statErr)
	}
}

// TestAssembleArchivesLicenceIndependentOfCwd: the licence must be resolved
// from the release inputs (--installer and its repository), never from the
// current working directory. Running from a non-repository directory that
// happens to hold a decoy LICENSE must still ship the repository licence.
func TestAssembleArchivesLicenceIndependentOfCwd(t *testing.T) {
	repo := t.TempDir()
	want := "fake repository licence\n"
	if err := os.WriteFile(filepath.Join(repo, "LICENSE"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	installer := filepath.Join(repo, "packaging", "install.sh")
	if err := os.MkdirAll(filepath.Dir(installer), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(repo, "bin")
	out := filepath.Join(repo, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")

	// Run from a non-repository cwd holding a decoy licence.
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "LICENSE"), []byte("decoy licence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(elsewhere)

	if err := assemble(bin, out, "v1.0.0", installer, ""); err != nil {
		t.Fatalf("the release failed: %v", err)
	}
	files := readArchive(t, filepath.Join(out, "cmesh-v1.0.0-linux-amd64.tar.gz"))
	got, ok := files["cmesh-v1.0.0-linux-amd64.d/LICENSE"]
	if !ok {
		t.Fatalf("the archive does not ship a licence; entries: %v", sortedKeys(files))
	}
	if got != want {
		t.Fatalf("the archive shipped the wrong licence (the cwd decoy?): %q", got)
	}
}

// TestAssembleArchivesAreListable: every archive must be a fully readable
// tar.gz — the `tar tz` gate an upload step or a user would run — holding
// exactly the binary, the installer and the licence under <binary>.d/.
func TestAssembleArchivesAreListable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("fake licence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")
	writeBinary(t, bin, "cmesh-v1.0.0-windows-amd64.exe")

	if err := assemble(bin, out, "v1.0.0", installer, ""); err != nil {
		t.Fatalf("the release failed: %v", err)
	}

	for _, binary := range []string{"cmesh-v1.0.0-linux-amd64", "cmesh-v1.0.0-windows-amd64.exe"} {
		archive := filepath.Join(out, binary+".tar.gz")
		files := readArchive(t, archive)
		// The directory is named after the platform and architecture; the
		// executable suffix is not part of it. The binary inside keeps its
		// own name.
		dir := archiveDirName(binary)
		want := map[string]string{
			dir + "/" + binary:  "fake binary",
			dir + "/install.sh": "#!/bin/sh\n",
			dir + "/LICENSE":    "fake licence\n",
		}
		got, wantNames := sortedKeys(files), sortedKeys(want)
		if strings.Join(got, ",") != strings.Join(wantNames, ",") {
			t.Errorf("%s holds %v, want %v", archive, got, wantNames)
			continue
		}
		for name, body := range want {
			if files[name] != body {
				t.Errorf("%s: entry %s has the wrong content: %q", archive, name, files[name])
			}
		}
	}
}

// TestAssembleHonoursExplicitLicenceFlag: an explicit --license replaces the
// search near --installer and ships exactly that file.
func TestAssembleHonoursExplicitLicenceFlag(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("auto-detected licence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	licence := filepath.Join(root, "my-licence.txt")
	if err := os.WriteFile(licence, []byte("explicit licence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")

	if err := assemble(bin, out, "v1.0.0", installer, licence); err != nil {
		t.Fatalf("the release failed: %v", err)
	}
	files := readArchive(t, filepath.Join(out, "cmesh-v1.0.0-linux-amd64.tar.gz"))
	if got := files["cmesh-v1.0.0-linux-amd64.d/LICENSE"]; got != "explicit licence\n" {
		t.Fatalf("the archive did not ship the --license file: %q", got)
	}
}

// TestAssembleRefusesMissingExplicitLicence: an explicit --license that does
// not exist must be a loud failure, not a licence-less release.
func TestAssembleRefusesMissingExplicitLicence(t *testing.T) {
	root := t.TempDir()
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")

	err := assemble(bin, out, "v1.0.0", installer, filepath.Join(root, "missing", "LICENSE"))
	if err == nil {
		t.Fatal("a missing explicit licence file was accepted")
	}
	if !strings.Contains(err.Error(), "licence") {
		t.Fatalf("the failure does not mention the licence file: %v", err)
	}
}
