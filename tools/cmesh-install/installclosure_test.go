package main

import (
	"os"
	"path/filepath"
	"testing"
)

// ★ THE INSTALLED VERSION DIRECTORY MUST BE TRAVERSABLE.
//
// os.MkdirTemp creates a directory with mode 0700, and the install
// renames the staging directory onto versions/<version>/ — so the
// installed directory inherited 0700. The binary inside was 0755, but a
// unit running as an unprivileged user could not traverse INTO the
// directory to reach it:
//
//	Permission denied — ExecStart=/opt/container-mesh/current/cmesh
//
// The shell installer has always done this correctly, and the install
// regression asserts it there — so the two installers disagreed about
// what they produce.
//
// This test calls installPayload, so it needs the package to compile —
// it is the behavioural half of the pair. The source-level half lives
// in installclosure_source_test.go, which compiles against any tree.
func TestTheInstalledVersionDirectoryIsTraversable(t *testing.T) {
	root := t.TempDir()
	if err := installPayload(root, "v1.2.3", []byte("#!/bin/sh\necho hi\n")); err != nil {
		t.Fatalf("install: %v", err)
	}

	dir := filepath.Join(root, "versions", "v1.2.3")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	perm := info.Mode().Perm()
	if perm&0o755 != 0o755 {
		t.Fatalf("the installed version directory is mode %v; a service user "+
			"that is not its owner cannot traverse it", perm)
	}

	// And the binary inside must be executable and reachable.
	bin := filepath.Join(dir, "cmesh")
	bi, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat binary: %v", err)
	}
	if bi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("the installed binary is not executable: mode %v", bi.Mode().Perm())
	}
}
