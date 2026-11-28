package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ★ THE CHECKSUM IS THE POINT OF THE INSTALLER, SO THE CHECKSUM MUST
// BE PINNED.
//
// The acceptance cell F1 says the installer "downloads + verifies the
// sha256 + writes it to disk, exit code 0". Every guard that existed
// covered the POSITIVE half: the download lands, the version directory
// appears, the `current` symlink points at it.
//
// A positive half cannot tell a verifying installer from one that does
// not verify at all. If the sha256 comparison were removed — or weakened
// into a comparison of the wrong bytes — every existing guard would stay
// green, and the machine would be running whatever the network served.
//
// So the guard has to be the NEGATIVE one: a payload whose digest does
// not match the release's SHA256SUMS must be REFUSED, and the refusal
// must leave NOTHING behind. A half install is worse than no install —
// a version directory without a `current` symlink, or a `current` that
// points at bytes nobody verified, is what an operator finds when the
// service starts.
//
// ★ NO REAL NETWORK. The fixture is a local HTTP server (httptest), so
// the test never talks to github.com and never depends on a release
// that may or may not exist today.

// fixtureRelease is one release the fixture server serves.
type fixtureRelease struct {
	version string
	// payload is the bytes the server hands out as the asset.
	payload []byte
	// sumsOver is the payload the SHA256SUMS line is computed over.
	//
	// When it differs from payload, the release is TAMPERED: the
	// checksum file names a digest the served bytes do not have. That
	// is exactly the shape of a compromised download, and it is the
	// only input the negative guard needs.
	sumsOver []byte
}

// assetNameFor returns the asset name the installer asks for on this
// machine.
//
// The asset name is built from the RUNNING platform, so the fixture
// must serve the asset this machine would ask for — a hardcoded name
// fails on every platform but one.
func assetNameFor(version string) string {
	asset := fmt.Sprintf("cmesh-%s-%s-%s", version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	return asset
}

// newReleaseFixture serves the given releases over a local HTTP server.
//
// The server speaks the same URL shape as the release host the installer
// talks to:
//
//	/download/<version>/SHA256SUMS
//	/download/<version>/<asset>
//
// A path it does not know answers 404, which is what the installer
// distinguishes from a transport failure.
func newReleaseFixture(t *testing.T, releases ...fixtureRelease) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, rel := range releases {
			asset := assetNameFor(rel.version)
			switch r.URL.Path {
			case "/download/" + rel.version + "/SHA256SUMS":
				sumsOver := rel.sumsOver
				if sumsOver == nil {
					sumsOver = rel.payload
				}
				w.Write([]byte(checksumFile(t, sumsOver, asset)))
				return
			case "/download/" + rel.version + "/" + asset:
				w.Write(rel.payload)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertNothingInstalled fails the test when the install root holds any
// trace of an install.
//
// The named paths are the ones the criterion names — the version
// directory and the `current` symlink — and the emptiness check catches
// everything else (a staging directory left behind, the managed-install
// marker, the `cmesh` symlink).
func assertNothingInstalled(t *testing.T, root, version string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(root, "versions", version),
		filepath.Join(root, "current"),
		filepath.Join(root, "cmesh"),
		filepath.Join(root, ".cmesh-install-root"),
	} {
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("the refused install left %s behind; a payload whose sha256 does "+
				"not match the release checksum must not be installed at all", path)
		} else if !os.IsNotExist(err) {
			t.Errorf("cannot stat %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read the install root %s: %v", root, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the refused install left %d entries in the install root %s (%s); "+
			"a rejected download must leave no half install behind",
			len(entries), root, strings.Join(names, ", "))
	}
}

// ★ A TAMPERED PAYLOAD MUST BE REFUSED, AND MUST LEAVE NOTHING BEHIND.
//
// The control comes first: the SAME fixture server hands out an
// untampered release, and that one installs. Without it, a fixture that
// is broken in some other way (a wrong URL, a wrong asset name) would
// make this guard green for the wrong reason.
func TestATamperedPayloadIsRefusedAndNothingIsInstalled(t *testing.T) {
	good := []byte("#!/bin/sh\necho good\n")
	tampered := []byte("#!/bin/sh\necho tampered\n")
	srv := newReleaseFixture(t,
		fixtureRelease{version: "v1.0.0", payload: good},
		fixtureRelease{version: "v2.0.0", payload: tampered, sumsOver: good},
	)

	// The control: the untampered release installs.
	controlRoot := t.TempDir()
	if err := installFrom(srv.URL, "v1.0.0", controlRoot); err != nil {
		t.Fatalf("the untampered release did not install (error: %v); the fixture "+
			"is broken, so the refusal below would prove nothing", err)
	}
	if _, err := os.Stat(filepath.Join(controlRoot, "current", "cmesh")); err != nil {
		t.Fatalf("the untampered release was not installed: %v", err)
	}

	// The subject: the tampered payload is served under a release whose
	// SHA256SUMS names a DIFFERENT digest.
	root := t.TempDir()
	err := installFrom(srv.URL, "v2.0.0", root)
	if err == nil {
		t.Errorf("the installer ACCEPTED a payload whose sha256 does not match the "+
			"release checksum; the bytes at %s were never verified",
			filepath.Join(root, "current", "cmesh"))
	} else if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("the installer refused the tampered payload but did not say why "+
			"(error: %v); the operator must be told the checksum did not match", err)
	}
	// And whatever else happened, nothing may have been installed.
	assertNothingInstalled(t, root, "v2.0.0")
}

// ★ THE OPERATOR SEES AN EXIT CODE, SO THE EXIT CODE IS THE OBSERVABLE.
//
// installFrom returning an error proves the function refuses; it does
// not prove the PROCESS refuses. main() turns an error into exit code
// 1, and that is the number an operator's script sees.
//
// So the real installer binary is run, against the same local fixture:
// the untampered release must exit 0 and land on disk, and the tampered
// one must exit non-zero and land nowhere.
func TestTheInstallerExitsNonZeroOnAChecksumMismatch(t *testing.T) {
	good := []byte("#!/bin/sh\necho good\n")
	tampered := []byte("#!/bin/sh\necho tampered\n")
	srv := newReleaseFixture(t,
		fixtureRelease{version: "v1.0.0", payload: good},
		fixtureRelease{version: "v2.0.0", payload: tampered, sumsOver: good},
	)
	bin := buildInstallerBinary(t)

	// The control: the untampered release exits 0 and is installed.
	goodRoot := t.TempDir()
	out, code := runInstaller(t, bin, srv.URL, "v1.0.0", goodRoot)
	if code != 0 {
		t.Fatalf("the installer exited %d on an untampered release (want 0):\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(goodRoot, "current", "cmesh")); err != nil {
		t.Fatalf("the untampered release was not installed: %v", err)
	}

	// The subject: the tampered payload must fail the process.
	root := t.TempDir()
	out, code = runInstaller(t, bin, srv.URL, "v2.0.0", root)
	if code == 0 {
		t.Errorf("the installer exited 0 on a payload whose sha256 does not match the "+
			"release checksum; the binary at %s was installed from unverified bytes",
			filepath.Join(root, "current", "cmesh"))
	} else if !strings.Contains(out, "checksum mismatch") {
		t.Errorf("the installer exited %d but did not report a checksum mismatch:\n%s",
			code, out)
	}
	// And nothing may have been installed.
	assertNothingInstalled(t, root, "v2.0.0")
}

// buildInstallerBinary compiles the real installer into a temporary path.
//
// The exit code lives in main(), so it cannot be observed in process —
// the binary has to run as a process.
func buildInstallerBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "cmesh-install-under-test")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the installer: %v\n%s", err, out)
	}
	return bin
}

// runInstaller runs the installer binary against the fixture and returns
// its combined output and its exit code.
//
// The environment is built from scratch for the three variables the
// installer reads, so an inherited CMESH_* variable in the developer's
// shell cannot change what is being tested.
func runInstaller(t *testing.T, bin, base, version, root string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(installerEnv(), "CMESH_BASE_URL="+base, "CMESH_VERSION="+version,
		"CMESH_INSTALL="+root)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run the installer: %v\n%s", err, out)
	}
	return string(out), exitErr.ExitCode()
}

// installerEnv returns the environment for an installer run, without any
// CMESH_* variable that could steer it.
func installerEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CMESH_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}
