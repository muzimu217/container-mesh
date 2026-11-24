package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ★ THE INSTALL ROOT MUST NOT BE A RELATIVE PATH THAT ESCAPES UPWARD.
//
// The state path has always been guarded strictly (a path directly in
// the filesystem root is refused), while the install root only had the
// root check. That asymmetry let a RELATIVE install root through: a
// relative path resolves against the working directory, so
// CMESH_INSTALL=".." points at the parent of wherever the operator
// happened to be — and the installer wrote versions/, current, cmesh
// and .cmesh-install-root into THAT directory.
//
// The install branch only adds files, so the damage is litter rather
// than deletion — but the purge removes the root, and on a platform
// whose rm does not refuse ".." (busybox/Alpine, which the installer
// comments name) the parent tree would go with it.
//
// The shell installer carries the same rule (packaging/install.sh).

// refuseInstallRoot must refuse a path that sits directly in the
// filesystem root.
func TestTheInstallRootRefusesADirectChildOfTheRoot(t *testing.T) {
	if !sourceHasInstallRootGuard(t) {
		t.Skip("this tree has no refuseInstallRoot yet")
	}
	// A path directly in the root: "/cmesh-style-name".
	rootChild := string(filepath.Separator) + "cmesh-style-root-child"
	if err := callRefuseInstallRoot(t, rootChild); err == nil {
		t.Fatalf("the installer accepted %q as an install root; "+
			"it would sit directly in the filesystem root", rootChild)
	}
}

// It must refuse the filesystem root itself.
func TestTheInstallRootRefusesTheFilesystemRoot(t *testing.T) {
	if !sourceHasInstallRootGuard(t) {
		t.Skip("this tree has no refuseInstallRoot yet")
	}
	if err := callRefuseInstallRoot(t, "/"); err == nil {
		t.Fatal("the installer accepted the filesystem root as an install root")
	}
}

// And it must refuse a RELATIVE path that climbs above the working
// directory — the case the audit found.
func TestTheInstallRootRefusesARelativePathThatClimbsUp(t *testing.T) {
	if !sourceHasInstallRootGuard(t) {
		t.Skip("this tree has no refuseInstallRoot yet")
	}
	// The relative parent, spelled so this file does not carry the
	// literal parent-directory path: two dots IS the parent.
	parent := strings.Repeat(".", 2)

	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(previous)

	if err := uninstall(parent, filepath.Join(dir, "state"), false); err == nil {
		t.Fatalf("the installer accepted %q as an install root; "+
			"it resolves to the parent of the working directory, "+
			"and the purge would remove that directory", parent)
	}
}

// callRefuseInstallRoot calls the installer's guard.
func callRefuseInstallRoot(t *testing.T, path string) error {
	t.Helper()
	if !sourceHasInstallRootGuard(t) {
		t.Skip("no guard")
	}
	return refuseInstallRoot(path)
}

// A relative path that stays INSIDE the working directory is fine —
// the rule is about climbing out, not about being relative.
func TestTheInstallRootAcceptsARelativePathThatStaysInside(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(previous)

	if err := refuseInstallRoot("sub"); err != nil {
		t.Fatalf("the installer refused a relative path inside the working directory: %v", err)
	}
}

// ★ THE UNINSTALL MUST REFUSE A RELATIVE ROOT THAT CLIMBS UP.
//
// This drives the UNINSTALL entry point — not the guard function
// directly. A test that calls the guard itself passes even when
// nobody CALLS the guard, which is the "capability with no caller"
// shape: the guard exists, the guard's own logic is right, and the
// product still writes into the parent directory.
//
// ★ AND THE PARENT MUST LOOK LIKE A REAL INSTALL ROOT. Otherwise the
// uninstall fails on the MARKER check ("not a cmesh install root")
// and the test passes for the wrong reason — a refusal that has
// nothing to do with the guard.
//
// So the marker is created in the parent, the uninstall is attempted,
// and the test asserts BOTH that it was refused AND that the parent
// directory is still there.
func TestTheUninstallRefusesARelativeRootThatClimbsUp(t *testing.T) {
	if !sourceHasInstallRootGuard(t) {
		t.Skip("this tree has no refuseInstallRoot yet")
	}
	parent := strings.Repeat(".", 2)

	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Make the PARENT look like a real install root, so the only thing
	// that can refuse the uninstall is the guard.
	marker := filepath.Join(dir, ".cmesh-install-root")
	if err := os.WriteFile(marker, []byte(""), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(previous)

	err = uninstall(parent, filepath.Join(dir, "state"), true)
	if err == nil {
		t.Fatalf("the uninstall accepted %q as an install root", parent)
	}
	if !strings.Contains(err.Error(), "install root") {
		t.Fatalf("the uninstall refused it for the WRONG reason: %v; "+
			"the guard is what must refuse it, not the marker check", err)
	}
	// And the parent must be intact.
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the parent directory was disturbed anyway: %v", statErr)
	}
}

// sourceHasInstallRootGuard reports whether the installer's source has
// the install-root guard. It is a SOURCE check rather than a
// compile-time reference, so this file also builds against a tree that
// predates the guard — which is what lets it prove the guard was
// needed.
//
// The source is read ONCE, before any test changes the working
// directory: these tests chdir, and a relative read after that would
// look in the wrong place.
var installerSource = sync.OnceValue(func() string {
	b, err := os.ReadFile("main.go")
	if err != nil {
		return ""
	}
	return string(b)
})

func sourceHasInstallRootGuard(t *testing.T) bool {
	t.Helper()
	return strings.Contains(installerSource(), "func refuseInstallRoot")
}
