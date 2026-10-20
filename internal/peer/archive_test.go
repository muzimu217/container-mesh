// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDirectoryRoundTrip sends a directory tree and checks the receiver ends
// up with the same files, not an opaque archive.
func TestDirectoryRoundTrip(t *testing.T) {
	a, _, _, hb := pair(t, "dir-transfer-code")

	src := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(src, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"top.txt":           "top level",
		"sub/middle.txt":    "middle level",
		"sub/deep/leaf.bin": strings.Repeat("x", 5000),
	}
	for rel, content := range files {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	fd := waitFile(t, hb)
	if fd.err != nil {
		t.Fatalf("receive: %v", fd.err)
	}
	if !res.OK || !fd.res.OK {
		t.Fatalf("directory transfer failed: %s / %s", res.Message, fd.res.Message)
	}

	// The tree must exist under the receiver's directory.
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(hb.dir, "project", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s content = %q, want %q", rel, got, want)
		}
	}

	// The archive itself must not be left behind.
	if _, err := os.Stat(filepath.Join(hb.dir, "project.cmeshtar")); !os.IsNotExist(err) {
		t.Error("archive was not removed after unpacking")
	}
}

// TestArchiveRejectsTraversal: a malicious archive must not write outside the
// destination.
func TestArchiveRejectsTraversal(t *testing.T) {
	bad := []string{
		"../escape.txt",
		"/etc/passwd",
		"a/../../escape.txt",
		`a\..\escape.txt`,
	}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			body := []byte("pwned")
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body)),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
			_ = tw.Close()

			archive := filepath.Join(t.TempDir(), "evil.cmeshtar")
			if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			if err := UnpackArchive(archive, dest, false); err == nil {
				t.Fatalf("archive entry %q was accepted", name)
			}
			// Nothing may have escaped.
			if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escape.txt")); !os.IsNotExist(err) {
				t.Error("file escaped the destination directory")
			}
		})
	}
}

// TestArchiveRejectsSymlinks: a symlink can point outside the destination, so
// archives containing one are refused wholesale.
func TestArchiveRejectsSymlinks(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc/passwd", Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()

	archive := filepath.Join(t.TempDir(), "link.cmeshtar")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UnpackArchive(archive, t.TempDir(), false); err == nil {
		t.Fatal("archive with a symlink was accepted")
	}
}

// TestArchiveNoOverwriteByDefault: extracting over an existing file is refused
// unless the operator asked for it.
func TestArchiveNoOverwriteByDefault(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("new")
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg, Name: "dir/existing.txt", Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()

	archive := filepath.Join(t.TempDir(), "a.cmeshtar")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dest, "dir", "existing.txt")
	if err := os.WriteFile(victim, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := UnpackArchive(archive, dest, false); err == nil {
		t.Fatal("existing file should not be overwritten by default")
	}
	if got, _ := os.ReadFile(victim); string(got) != "old" {
		t.Errorf("victim was modified: %q", got)
	}

	// With overwrite it replaces.
	if err := UnpackArchive(archive, dest, true); err != nil {
		t.Fatalf("forced unpack: %v", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "new" {
		t.Errorf("forced unpack content = %q", got)
	}
}

// TestPackDirSkipsSymlinks: packing must not follow or record links.
func TestPackDirSkipsSymlinks(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "real.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var buf bytes.Buffer
	if _, err := PackDir(src, &buf); err != nil {
		t.Fatalf("pack: %v", err)
	}

	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Name == filepath.Base(src)+"/link" {
			t.Error("symlink was archived")
		}
	}
}

// TestArchiveNameRoundTrip keeps the naming contract explicit.
func TestArchiveNameRoundTrip(t *testing.T) {
	dir := "/tmp/photos"
	name := ArchiveName(dir)
	if name != "photos.cmeshtar" {
		t.Errorf("ArchiveName = %q", name)
	}
	if !IsArchiveName(name) {
		t.Error("IsArchiveName should recognise its own output")
	}
	if IsArchiveName("photos.zip") {
		t.Error("ordinary files must not be treated as archives")
	}
}

// TestDirectoryHashMatchesPackedBytes: the sender hashes the archive it sends,
// so a resumed directory transfer verifies against the same value.
func TestDirectoryHashMatchesPackedBytes(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := PackDir(src, &buf); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "a.cmeshtar")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := HashFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if sum != hexOf(buf.Bytes()) {
		t.Error("file hash does not match the packed bytes")
	}
}

func hexOf(b []byte) string {
	sum := sha256.Sum256(b)
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, v := range sum {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0xf]
	}
	return string(out)
}
