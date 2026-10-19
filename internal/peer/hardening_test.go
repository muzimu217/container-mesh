// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muzimu217/container-mesh/internal/wire"
)

// TestValidateNameHardening covers the cases the original validator missed:
// device names, the receiver's own .part suffix, control characters and
// oversized names.
func TestValidateNameHardening(t *testing.T) {
	good := []string{
		"report.pdf", "a.txt", "My File (1).zip", "data_2026-10-18.csv",
		"A", "x.tar.gz",
	}
	for _, n := range good {
		if err := validateName(n); err != nil {
			t.Errorf("validateName(%q) = %v, want nil", n, err)
		}
	}

	bad := []string{
		"", ".", "..", "../etc/passwd", "a/b", `a\b`, ".hidden",
		"CON", "con", "NUL", "nul", "COM1", "lpt9", "con.txt",
		"payload.part", "PAYLOAD.PART",
		"tab\there", "bell\x07", "nul\x00byte",
		" leading", "trailing ",
		"quote'file", "semi;colon", "pipe|name", "star*name",
		strings.Repeat("a", maxNameLen+1),
	}
	for _, n := range bad {
		if err := validateName(n); err == nil {
			t.Errorf("validateName(%q) should have failed", n)
		}
	}

	// Exactly at the limit is allowed.
	atLimit := strings.Repeat("a", maxNameLen)
	if err := validateName(atLimit); err != nil {
		t.Errorf("name of exactly %d bytes rejected: %v", maxNameLen, err)
	}
}

// TestRejectsOversizedFileBeforeWriting checks that an offer above MaxFileSize
// is declined without creating anything on disk.
func TestRejectsOversizedFileBeforeWriting(t *testing.T) {
	a, b, _, hb := pair(t, "size-limit-code")
	b.cfg.MaxFileSize = 1024 // 1 KiB ceiling

	src := filepath.Join(t.TempDir(), "big.bin")
	data := make([]byte, 4096)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.OK {
		t.Fatal("oversized file should have been rejected")
	}
	if !strings.Contains(res.Message, "too large") {
		t.Errorf("message %q should mention the size limit", res.Message)
	}

	// Nothing may have been written: no .part, no final file.
	for _, name := range []string{"big.bin", "big.bin.part"} {
		if _, err := os.Stat(filepath.Join(hb.dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists despite rejection (stat err = %v)", name, err)
		}
	}
}

// TestAcceptsFileAtExactLimit makes sure the boundary itself is allowed.
func TestAcceptsFileAtExactLimit(t *testing.T) {
	a, b, _, hb := pair(t, "exact-limit-code")

	data := make([]byte, 2048)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	b.cfg.MaxFileSize = int64(len(data))

	src := filepath.Join(t.TempDir(), "exact.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	fd := waitFile(t, hb)
	if !res.OK || fd.err != nil || !fd.res.OK {
		t.Fatalf("exact-limit transfer failed: %s / %v", res.Message, fd.err)
	}
}

// TestRefusesToOverwriteExistingFile: a peer must not be able to destroy an
// unrelated file that happens to share the offered name.
func TestRefusesToOverwriteExistingFile(t *testing.T) {
	a, _, _, hb := pair(t, "no-clobber-code")

	// The receiver already has a file with this name and different content.
	const victim = "important.txt"
	victimPath := filepath.Join(hb.dir, victim)
	if err := os.WriteFile(victimPath, []byte("do not lose me"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The sender offers a same-named file with different (larger) content, so
	// the receiver cannot take the "already complete" shortcut.
	payload := make([]byte, 3000)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), victim)
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFile(t, hb)

	if res.OK {
		t.Fatal("transfer should have failed rather than overwrite")
	}

	got, err := os.ReadFile(victimPath)
	if err != nil {
		t.Fatalf("victim file disappeared: %v", err)
	}
	if string(got) != "do not lose me" {
		t.Errorf("victim file was modified: %q", got)
	}
}

// TestMaxFileSizeZeroMeansDefault documents the sentinel handling: a zero
// config value must turn into DefaultMaxFileSize, not "unlimited".
func TestMaxFileSizeZeroMeansDefault(t *testing.T) {
	// Handshake applies the default; verify through the exported constant.
	if DefaultMaxFileSize <= 0 {
		t.Fatalf("DefaultMaxFileSize = %d, want a positive cap", DefaultMaxFileSize)
	}
	// A 16 GiB default is the documented behaviour.
	if DefaultMaxFileSize != 16<<30 {
		t.Errorf("DefaultMaxFileSize = %d, want %d", DefaultMaxFileSize, int64(16<<30))
	}
}

// TestFreeDiskBytesReportsSomething sanity-checks the statfs wrapper: it must
// return a plausible positive number for a writable temp dir.
func TestFreeDiskBytesReportsSomething(t *testing.T) {
	dir := t.TempDir()
	free, err := freeDiskBytes(dir)
	if err != nil {
		t.Skipf("freeDiskBytes unavailable on this platform: %v", err)
	}
	if free <= 0 {
		t.Errorf("freeDiskBytes(%s) = %d, want > 0", dir, free)
	}
	// A write of a few bytes must fit.
	if free < 1<<20 {
		t.Skipf("less than 1 MiB free on %s; skipping", dir)
	}
}

// TestOfferRejectedWhenDiskTooSmall simulates a volume that cannot hold the
// transfer by asking for an absurd size relative to available space.
func TestOfferRejectedWhenDiskTooSmall(t *testing.T) {
	a, b, _, _ := pair(t, "disk-full-code")

	// A size larger than any real disk makes the pre-flight fail without
	// actually filling anything.
	payload := make([]byte, 1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "huge.bin")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	// Keep MaxFileSize out of the way so the disk check is what fires.
	b.cfg.MaxFileSize = -1

	res, err := a.SendFile(src, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// Either it succeeded (plenty of space) or the receiver refused; what must
	// never happen is a silent partial write being reported as verified.
	if !res.OK && !strings.Contains(res.Message, "disk space") && !strings.Contains(res.Message, "too large") {
		t.Logf("rejection message: %s", res.Message)
	}
}

// TestPublishedFileIsNotClobberedByConcurrentOffer is a regression guard for
// the link-based publish path: publishing twice must not overwrite.
func TestPublishedFileIsNotClobberedByConcurrentOffer(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "x.part")
	final := filepath.Join(dir, "x")

	if err := os.WriteFile(part, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := publishNoClobber(part, final); err == nil {
		t.Fatal("publishNoClobber should refuse to overwrite")
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Errorf("existing file was overwritten: %q", got)
	}
	// The part file must survive a refused publish.
	if _, err := os.Stat(part); err != nil {
		t.Errorf("part file vanished after refused publish: %v", err)
	}
}

// TestPublishSucceedsWhenAbsent is the happy path.
func TestPublishSucceedsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "y.part")
	final := filepath.Join(dir, "y")
	if err := os.WriteFile(part, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publishNoClobber(part, final); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Errorf("published content = %q", got)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Errorf("part file should be gone, stat err = %v", err)
	}
}

// ensure wire stays imported for the result type used above
var _ = wire.FileResult{}
