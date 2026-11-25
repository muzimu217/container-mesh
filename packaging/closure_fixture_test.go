// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package packaging holds the offline regression for the Linux headless
// install closure.
//
// The closure is the whole path an operator walks on a headless Linux box:
//
//	install.sh  ->  the service user can execute the binary
//	           ->  the unit files are semantically correct
//	           ->  the uninstall stops the units and reports what is left
//	           ->  a rollback refuses a version the units cannot run
//
// The tests drive packaging/install.sh as a PROCESS, in a temporary
// directory, against a local fixture. Nothing here touches the network and
// nothing here touches a real systemd: the unit files are checked by
// PARSING them, and the "would systemd accept this" question is answered
// by the parser, not by a live manager.
//
// ★ WHY A GO TEST AND NOT MORE SHELL. (Round 104.)
//
// The shell installer had a green offline regression (install_test.sh) and
// it did not catch the install root being 0700, because the test ran under
// the developer's umask (022) and the bug only appears under a restrictive
// one (077). A test that only passes under a permissive umask is a test
// that cannot see the bug.
//
// The Go test makes the umask EXPLICIT (it is set, the installer is run,
// the umask is restored), so the case is exercised on every run and on
// every machine, whatever the developer's shell happens to use.
package packaging

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ★ THE ASSET NAME THE INSTALLER ASKS FOR.
//
// The installer resolves the OS and the architecture from uname, so a
// hardcoded fixture name fails on every platform but one (F-1: the
// offline regression went red on linux/amd64 because the fixture was
// hardcoded to darwin/arm64). The fixture therefore computes the name the
// SAME WAY the installer does.
func fixtureAsset() string {
	os_ := runtime.GOOS
	switch os_ {
	case "linux", "darwin":
	case "freebsd", "openbsd", "netbsd":
		os_ = "freebsd"
	case "windows":
		os_ = "windows"
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	}
	return fmt.Sprintf("cmesh-latest-%s-%s", os_, arch)
}

// fixture is a local release tree plus a stub curl that serves it.
//
// The stub is what keeps the test offline: the installer insists on an
// https:// base URL, and a stub curl that maps one fixed https host onto
// the temporary tree satisfies that without a network.
type fixture struct {
	root     string // the temporary root
	releases string // the release tree the stub serves
	bin      string // the directory holding the stub curl
}

// newFixture builds a release tree containing one asset.
//
// The asset's content is the given string; callers that want a REAL
// cmesh-shaped binary pass one (see modernBinary and ancientBinary).
func newFixture(t *testing.T, assetContent string) *fixture {
	t.Helper()
	f := &fixture{
		root:     t.TempDir(),
		releases: filepath.Join(t.TempDir(), "releases"),
		bin:      filepath.Join(t.TempDir(), "bin"),
	}
	if err := os.MkdirAll(filepath.Join(f.releases, "latest", "download"), 0o755); err != nil {
		t.Fatalf("mkdir releases: %v", err)
	}
	if err := os.MkdirAll(f.bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	f.writeAsset(t, "latest", fixtureAsset(), assetContent)
	f.writeStubCurl(t)
	// The release API answer, for the installer's prerelease fallback.
	// It names the same version the fixture serves.
	if err := os.WriteFile(filepath.Join(f.releases, "api-releases.json"),
		[]byte(`[{"tag_name":"latest","prerelease":false,"draft":false}]`), 0o644); err != nil {
		t.Fatalf("write api-releases.json: %v", err)
	}
	return f
}

// writeAsset puts one asset into the release tree and updates the
// SHA256SUMS that sits beside it.
//
// The checksum is REAL: the installer verifies it, so a fixture whose
// sums are wrong would make every test fail for the wrong reason.
func (f *fixture) writeAsset(t *testing.T, version, asset, content string) {
	t.Helper()
	dir := filepath.Join(f.releases, "download", version)
	if version == "latest" {
		dir = filepath.Join(f.releases, "latest", "download")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, asset), []byte(content), 0o755); err != nil {
		t.Fatalf("write asset: %v", err)
	}
	sum := sha256.Sum256([]byte(content))
	line := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset)
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	existing, _ := os.ReadFile(sumsPath)
	if err := os.WriteFile(sumsPath, append(existing, []byte(line)...), 0o644); err != nil {
		t.Fatalf("write sums: %v", err)
	}
}

// writeStubCurl installs a curl that serves the fixture tree.
//
// It answers 404 (not a transport failure) for a path that does not
// exist, because that is what GitHub answers for a release path that is
// not there, and the installer distinguishes the two.
func (f *fixture) writeStubCurl(t *testing.T) {
	t.Helper()
	script := `#!/bin/sh
set -eu
url=""
out=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o) out=$2; shift 2 ;;
        -*) shift ;;
        *) url=$1; shift ;;
    esac
done
case "$url" in
    https://fixture.invalid/releases/latest/download/*)
        src="$FIXTURE_ROOT/latest/download/${url##*/}"
        ;;
    https://fixture.invalid/releases/download/*)
        src="$FIXTURE_ROOT/download/${url#https://fixture.invalid/releases/download/}"
        ;;
    https://api.fixture.invalid/repos/*/releases*)
        # The release API: the installer falls back to it when the
        # latest URL is a 404 (which is the case while every release is
        # a prerelease).
        src="$FIXTURE_ROOT/api-releases.json"
        ;;
    *)
        printf 'curl: (22) The requested URL returned error: 404\n' >&2
        exit 22
        ;;
esac
[ -f "$src" ] || { printf 'curl: (22) The requested URL returned error: 404\n' >&2; exit 22; }
cp "$src" "$out"
`
	if err := os.WriteFile(filepath.Join(f.bin, "curl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub curl: %v", err)
	}
}

// env returns the environment for an installer run: the fixture's stub
// curl first on the PATH, and the fixture's release tree as FIXTURE_ROOT.
//
// CMESH_INSTALL and CMESH_BASE_URL are set here so that a test can
// never reach the real network: without them the installer would use
// its production defaults (the current directory and github.com), and
// the test would be testing the internet instead of the installer.
//
// The PATH is set too, and it MATTERS: the installer resolves curl
// through the PATH, so without the stub there the real curl would run
// and the test would talk to github.com. CMESH_RELEASE_API points at
// the stub as well, so the installer's "the latest URL is a 404, ask
// the release API" fallback stays offline too.
func (f *fixture) env() []string {
	env := os.Environ()
	env = append(env, "FIXTURE_ROOT="+f.releases)
	env = append(env, "FIXTURE_CALLS="+filepath.Join(f.root, "systemctl.calls"))
	env = append(env, "CMESH_INSTALL="+f.installRoot())
	env = append(env, "CMESH_BASE_URL=https://fixture.invalid/releases")
	env = append(env, "CMESH_RELEASE_API=https://api.fixture.invalid/repos/muzimu217/container-mesh/releases")
	env = append(env, "PATH="+f.path())
	return env
}

// path returns the PATH the installer must use: the stub curl first, so
// the real curl is never reached.
func (f *fixture) path() string {
	return f.bin + string(os.PathListSeparator) + os.Getenv("PATH")
}

// run runs the installer under the given umask and returns its combined
// output and the exit code.
//
// The umask is SET, the installer is run, and the umask is RESTORED —
// in that order, with the restore deferred. A test that leaves the
// process umask changed would silently change the permissions of every
// file the rest of the test binary creates, which is exactly the kind of
// cross-test contamination that makes a suite untrustworthy.
func (f *fixture) run(t *testing.T, umask int, args ...string) (string, int) {
	return f.runWithEnv(t, umask, nil, args...)
}

// runWithEnv runs the installer with extra environment variables.
//
// The variables are APPENDED to the environment, so they override an
// earlier value of the same name — which is what an operator's
// `CMESH_STATE=... install.sh --uninstall` does.
func (f *fixture) runWithEnv(t *testing.T, umask int, extra map[string]string, args ...string) (string, int) {
	t.Helper()
	old := setUmask(umask)
	defer setUmask(old)

	cmd := exec.Command("sh", append([]string{filepath.Join("packaging", "install.sh")}, args...)...)
	env := f.env()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run installer: %v", err)
		}
	}
	return string(out), code
}

// asExitError is errors.As specialised for *exec.ExitError, kept here so
// the test file does not need the errors import in every helper.
func asExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// repoRoot returns the repository root, which is the directory that holds
// packaging/install.sh.
//
// The test runs with its working directory set to the package directory
// (packaging/), so the repository root is one level up.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(wd)
}

// installRoot is where the fixture's installs go.
//
// It is a DIRECTORY THAT DOES NOT EXIST YET on purpose: the installer
// creates it, and the mode it creates it with is exactly what the
// service user has to be able to traverse.
func (f *fixture) installRoot() string {
	return filepath.Join(f.root, "install")
}

// mode returns the permission bits of a path.
func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// readLink returns the target of a symlink.
func readLink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	return target
}

// mustContain fails the test when the output does not carry the phrase.
//
// The phrase is quoted into the failure message, because "the installer's
// output did not mention the thing" is only actionable if the reader can
// see the output.
func mustContain(t *testing.T, what, output, phrase string) {
	t.Helper()
	if !strings.Contains(output, phrase) {
		t.Fatalf("%s: the output does not mention %q\n--- output ---\n%s", what, phrase, output)
	}
}
