// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

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

// TestArchiveDirectoryNameDropsTheExeSuffix pins the archive's top-level
// directory name.
//
// The archives extract to a directory named after the binary with a ".d"
// suffix, so "cmesh-v1.0.0-linux-amd64" extracts to
// "cmesh-v1.0.0-linux-amd64.d". For a Windows binary the suffix was
// carried along, and the archive extracted to
// "cmesh-v1.0.0-windows-amd64.exe.d" — a directory whose name ends in the
// executable suffix. That reads like a file name rather than a directory,
// and unpacking it on Windows is at best confusing.
//
// The directory is named after the platform and architecture, not after the
// file, so the executable suffix does not belong in it.
func TestArchiveDirectoryNameDropsTheExeSuffix(t *testing.T) {
	root := t.TempDir()
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	writeBinary(t, bin, "cmesh-v1.0.0-windows-amd64.exe")
	writeBinary(t, bin, "cmesh-v1.0.0-linux-amd64")

	if err := assemble(bin, out, "v1.0.0", installer, ""); err != nil {
		t.Fatalf("the release failed: %v", err)
	}

	for _, tc := range []struct {
		binary  string
		archive string
		dir     string
	}{
		{
			binary:  "cmesh-v1.0.0-windows-amd64.exe",
			archive: "cmesh-v1.0.0-windows-amd64.exe.tar.gz",
			dir:     "cmesh-v1.0.0-windows-amd64.d",
		},
		{
			binary:  "cmesh-v1.0.0-linux-amd64",
			archive: "cmesh-v1.0.0-linux-amd64.tar.gz",
			dir:     "cmesh-v1.0.0-linux-amd64.d",
		},
	} {
		files := readArchive(t, filepath.Join(out, tc.archive))
		dirs := map[string]bool{}
		for name := range files {
			dirs[strings.SplitN(name, "/", 2)[0]] = true
		}
		if len(dirs) != 1 {
			t.Errorf("%s has %d top-level directories, want 1", tc.archive, len(dirs))
			continue
		}
		for dir := range dirs {
			if dir != tc.dir {
				t.Errorf("%s extracts to %q, want %q", tc.archive, dir, tc.dir)
			}
		}
		// The binary itself keeps its name inside the archive.
		if _, ok := files[tc.dir+"/"+tc.binary]; !ok {
			t.Errorf("%s does not hold %s/%s", tc.archive, tc.dir, tc.binary)
		}
	}
}

// TestArchiveDirectoryNamesAreStable locks the naming down for every
// platform, so a future change to the suffix cannot move it silently.
func TestArchiveDirectoryNamesAreStable(t *testing.T) {
	for _, tc := range []struct {
		binary string
		want   string
	}{
		{"cmesh-v1.0.0-linux-amd64", "cmesh-v1.0.0-linux-amd64.d"},
		{"cmesh-v1.0.0-linux-arm64", "cmesh-v1.0.0-linux-arm64.d"},
		{"cmesh-v1.0.0-darwin-amd64", "cmesh-v1.0.0-darwin-amd64.d"},
		{"cmesh-v1.0.0-darwin-arm64", "cmesh-v1.0.0-darwin-arm64.d"},
		{"cmesh-v1.0.0-windows-amd64.exe", "cmesh-v1.0.0-windows-amd64.d"},
		{"cmesh-v1.0.0-windows-arm64.exe", "cmesh-v1.0.0-windows-arm64.d"},
	} {
		if got := archiveDirName(tc.binary); got != tc.want {
			t.Errorf("archiveDirName(%q) = %q, want %q", tc.binary, got, tc.want)
		}
	}
}

// archiveDirNames lists the archive directory names a release produced, for
// the failure message above.
func archiveDirNames(t *testing.T, out string) []string {
	t.Helper()
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		files := readArchive(t, filepath.Join(out, e.Name()))
		for name := range files {
			names = append(names, strings.SplitN(name, "/", 2)[0])
		}
	}
	sort.Strings(names)
	return names
}

var _ = io.Discard
var _ = tar.TypeReg
var _ = gzip.BestSpeed
