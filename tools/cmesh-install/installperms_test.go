// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// ★ THE INSTALL TREE MUST BE TRAVERSABLE BY THE SERVICE USER.
//
// The systemd units run the node as an unprivileged "cmesh" account and
// execute
//
//	ExecStart=/opt/container-mesh/current/cmesh ...
//
// so the service account has to be able to REACH the binary. On Linux
// that means the execute (traverse) bit on every directory on the path:
// the install root, versions/, and versions/<version>/.
//
// ★ WHY THE TEST SETS THE UMASK ITSELF.
//
// os.MkdirAll applies the PROCESS'S umask, and the umask applies to the
// INTERMEDIATE directories too — so os.MkdirAll(root+"/versions", 0755)
// creates root itself at 0700 under a umask of 077.
//
// A test that inherits the developer's umask cannot see this: under the
// usual 022 the tree comes out 0755 and everything passes. The
// regression only shows under 077, so the test has to SAY so.
//
// The umask is a process-wide setting, so it is set, the install runs,
// and the previous value is restored — with a defer, so it is restored
// even when the test fails.
//
// ★ THE SYMPTOM IS THE WORST KIND, BECAUSE IT LOOKS LIKE A DIFFERENT
// PROBLEM.
//
// The unit starts, is scheduled, and then fails with
//
//	Failed to execute ...: Permission denied
//
// which systemd reports as status=203/EXEC — the SAME status a MISSING
// binary produces. The operator's first conclusion is "the install did
// not happen", and the real cause (a directory the service user cannot
// walk into) is invisible.
func TestTheInstalledTreeIsTraversableUnderARestrictiveUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	root := filepath.Join(t.TempDir(), "install")
	if err := installPayload(root, "v1", []byte("binary")); err != nil {
		t.Fatalf("installPayload: %v", err)
	}

	for _, dir := range []string{
		root,
		filepath.Join(root, "versions"),
		filepath.Join(root, "versions", "v1"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		perm := info.Mode().Perm()
		if perm&0o001 == 0 {
			t.Errorf("the directory %s is mode %v: the service user (cmesh) "+
				"cannot traverse into it, so ExecStart=/opt/container-mesh/"+
				"current/cmesh fails with 203/EXEC — the same status a MISSING "+
				"binary gives, so the operator looks in the wrong place",
				dir, perm)
		}
	}
}
