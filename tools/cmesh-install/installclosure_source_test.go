package main

import (
	"os"
	"strings"
	"testing"
)

// ★ THE INSTALLER MUST STOP THE DEPLOYED UNITS — CHECKED ON THE SOURCE.
//
// This file asserts on the installer's SOURCE TEXT rather than calling
// its functions, and it reads no symbol from the package. That is
// deliberate: it means the file compiles against a tree that predates
// the fix, so it can be used to PROVE that the fix was needed — a test
// that only compiles after the fix cannot show that the fix mattered.
//
// What it asserts, and why:
//
//  1. The installer stops and disables units at all. The Go uninstaller
//     used to remove the binaries and nothing else, so the unit files
//     stayed on disk, still enabled, with ExecStart pointing at a path
//     that no longer existed: the next boot started the unit, got
//     203/EXEC, and — with Restart=always — restarted it forever.
//
//  2. The list names the DEPLOYED units. "cmesh-discovery.service" is
//     the packaging name; a deployed machine runs the anchor as
//     "cmesh-serve-discovery.service" and has "cmesh-hub.service".
//     Stopping the wrong list leaves the real units running.
//
//  3. The staged directory is made traversable. os.MkdirTemp creates
//     0700 and the install renames that directory onto
//     versions/<version>/, so the installed directory was 0700 — and a
//     unit running as an unprivileged user could not traverse into it
//     to execute the binary.

func readInstallerSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return string(b)
}

// ★ The uninstaller must stop and disable the units.
func TestTheSourceStopsAndDisablesUnits(t *testing.T) {
	src := readInstallerSource(t)
	if !strings.Contains(src, "systemctl") {
		t.Fatal("the installer never touches systemd; an uninstall that does " +
			"not stop the units leaves them enabled and the next boot starts " +
			"them with a removed binary")
	}
	for _, want := range []string{`"stop"`, `"disable"`} {
		if !strings.Contains(src, want) {
			t.Fatalf("the installer does not ask systemd to %s", want)
		}
	}
}

// ★ And it must stop the units that are really deployed.
func TestTheSourceNamesTheDeployedUnits(t *testing.T) {
	src := readInstallerSource(t)
	for _, want := range []string{
		"cmesh-serve-discovery.service",
		"cmesh-hub.service",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("the installer's unit list does not name %q; "+
				"the anchor really runs as cmesh-serve-discovery.service", want)
		}
	}
}

// ★ And the staged version directory must end up traversable.
func TestTheSourceMakesTheStagedDirectoryTraversable(t *testing.T) {
	src := readInstallerSource(t)
	// The chmod must be applied to the STAGE, not only to the binary
	// inside it — the directory's mode is what travels with the
	// rename.
	if !strings.Contains(src, "os.Chmod(stage") {
		t.Fatal("the installer never makes the staging directory traversable; " +
			"the installed versions/<version>/ inherits MkdirTemp's 0700 and " +
			"a service user that is not its owner cannot execute the binary")
	}
}
