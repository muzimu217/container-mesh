// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Command cmesh-install fetches and installs a verified cmesh binary for this
// machine. The install layout is deliberately separate from runtime state:
//
//	<install>/versions/<version>/cmesh
//	<install>/current -> versions/<version>
//	<install>/cmesh -> current/cmesh
//
// Runtime identities, certificates, and member lists remain under ~/.cmesh (or
// the operator's --state directory); this tool never creates or reads them.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://github.com/muzimu217/container-mesh/releases"
	maxDownload    = 256 << 20
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--uninstall" {
		fs := flag.NewFlagSet("cmesh-install --uninstall", flag.ExitOnError)
		purge := fs.Bool("purge", false, "also remove the runtime state directory")
		_ = fs.Parse(os.Args[2:])
		if err := uninstall(envOr("CMESH_INSTALL", "."), envOr("CMESH_STATE", defaultStateDir()), *purge); err != nil {
			fmt.Fprintf(os.Stderr, "cmesh-install: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "cmesh-install: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	base := envOr("CMESH_BASE_URL", defaultBaseURL)
	version := envOr("CMESH_VERSION", "latest")
	dir := envOr("CMESH_INSTALL", ".")

	// ★ THE INSTALL ROOT IS REFUSED BEFORE ANYTHING IS FETCHED.
	//
	// The shell installer refuses the filesystem root on the install
	// branch too, and for the same reason: a path the tool refuses to
	// remove must also be a path it refuses to install into. Two
	// installers that ship side by side must not have opposite safety.
	//
	// The check is done BEFORE the download, so a bad target never even
	// reaches the network.
	//
	// ONLY the filesystem root itself is refused here — NOT a directory
	// that sits directly in the root. "/cmesh" is a real install root in
	// the field, and the install branch only ADDS files under it; it
	// never removes the directory itself. The stricter "direct child"
	// rule belongs to the REMOVAL path, where it is the difference
	// between a typo and a wiped machine.
	if err := refuseRootOnly(dir, "install root"); err != nil {
		return err
	}
	return installFrom(base, version, dir)
}

// installFrom downloads the named release and installs it into dir.
//
// It is separate from run() so the download-and-install path can be
// exercised against a fixture server — the fallback below is only
// meaningful if something can reach it.
//
// base may be plain http ONLY when it points at a loopback fixture:
// the download carries a binary that is verified against a checksum
// fetched over the same connection, and a plaintext connection to a
// REMOTE host could be tampered with on the way. Loopback is not on
// the way.
func installFrom(base, version, dir string) error {
	if err := checkBaseURL(base); err != nil {
		return err
	}

	if parsed, err := url.Parse(base); err != nil || parsed.Host == "" {
		return fmt.Errorf("CMESH_BASE_URL is not a URL")
	}
	if !safeVersion(version) {
		return fmt.Errorf("CMESH_VERSION is not a usable release tag: %q", version)
	}

	asset := fmt.Sprintf("cmesh-%s-%s-%s", version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	sumsURL, binaryURL := releaseURLs(base, version, asset)

	fmt.Printf("cmesh-install\n")
	fmt.Printf("  platform : %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  asset    : %s\n", asset)
	fmt.Printf("  target   : %s\n", dir)

	client := &http.Client{Timeout: 120 * time.Second}
	sums, err := fetch(client, sumsURL)
	if err != nil {
		// ★ THE "latest" PATH NEEDS A FALLBACK. (Round 103, found by the
		// install-closure audit.)
		//
		// GitHub's /releases/latest points at the newest NON-prerelease.
		// While the only release is a prerelease — which is where this
		// project is — that path is a 404, and the default install fails
		// outright: a new user following the README gets nothing at all.
		//
		// The shell installer (packaging/install.sh) already falls back
		// to the release API, which lists prereleases too. The Go
		// installer did not — so the two installers disagreed about
		// whether the documented default works.
		//
		// Measured: /releases/latest/download/SHA256SUMS → 404, while
		// the API names v1.0.0-rc2 and that release's assets are there.
		if version != "latest" {
			return fmt.Errorf("cannot fetch the release checksums: %w", err)
		}
		resolved, apiErr := resolveLatestRelease(client)
		if apiErr != nil {
			return fmt.Errorf("cannot fetch the release checksums (%v), and the release API is unreachable: %w", err, apiErr)
		}
		fmt.Printf("  no published release at the latest URL; using release %s\n", resolved)
		version = resolved
		asset = fmt.Sprintf("cmesh-%s-%s-%s", version, runtime.GOOS, runtime.GOARCH)
		if runtime.GOOS == "windows" {
			asset += ".exe"
		}
		sumsURL, binaryURL = releaseURLs(base, version, asset)
		sums, err = fetch(client, sumsURL)
		if err != nil {
			return fmt.Errorf("cannot fetch the release checksums for %s: %w", version, err)
		}
	}
	expected := checksumFor(sums, asset)
	if expected == "" {
		return fmt.Errorf("the release does not list %s", asset)
	}

	fmt.Println("  downloading...")
	payload, err := fetch(client, binaryURL)
	if err != nil {
		return fmt.Errorf("cannot fetch the release binary: %w", err)
	}
	sum := sha256.Sum256(payload)
	actual := hex.EncodeToString(sum[:])
	if actual != expected {
		return fmt.Errorf("checksum mismatch for %s (expected %s, got %s)", asset, expected, actual)
	}
	fmt.Println("  checksum ok")

	if err := installPayload(dir, version, payload); err != nil {
		return err
	}
	fmt.Printf("  installed: %s\n", filepath.Join(dir, "current", "cmesh"))
	return nil
}

// checkBaseURL refuses a base URL that is not https, except a plain
// http one that points at loopback.
//
// The download carries an executable, and the checksum that verifies it
// comes over the same connection — so a plaintext connection to a
// remote host could be tampered with on the way. Loopback is not on
// the way, and it is what a fixture server uses.
func checkBaseURL(base string) error {
	if strings.HasPrefix(base, "https://") {
		return nil
	}
	if strings.HasPrefix(base, "http://") {
		parsed, err := url.Parse(base)
		if err == nil {
			host := parsed.Hostname()
			if host == "127.0.0.1" || host == "localhost" || host == "::1" {
				return nil
			}
		}
	}
	return fmt.Errorf("CMESH_BASE_URL must be https://")
}

func releaseURLs(base, version, asset string) (sums, binary string) {
	if version == "latest" {
		return base + "/latest/download/SHA256SUMS", base + "/latest/download/" + asset
	}
	return base + "/download/" + version + "/SHA256SUMS", base + "/download/" + version + "/" + asset
}

// releaseAPIURL is where the newest releases are listed. It lists
// prereleases too, which is the point: the "latest" download URL does
// not.
const releaseAPIURL = "https://api.github.com/repos/muzimu217/container-mesh/releases"

// releaseAPIURLOverride lets a test point the fallback at a fixture
// server. It is empty in normal use.
var releaseAPIURLOverride string

// releaseAPIEndpoint returns the URL the fallback consults.
func releaseAPIEndpoint() string {
	if releaseAPIURLOverride != "" {
		return releaseAPIURLOverride
	}
	return releaseAPIURL
}

// release is one entry of the release API's list.
type release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// parseReleases reads the release API's JSON list.
func parseReleases(body []byte) ([]release, error) {
	var releases []release
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, fmt.Errorf("the release API did not return a release list: %w", err)
	}
	return releases, nil
}

// pickInstallableRelease returns the tag to install.
//
// The order is the order GitHub's own UI uses: the newest release that
// is neither a draft nor a prerelease, and if there is none, the
// newest prerelease. Both are "a release" — and while this project
// publishes only prereleases, the newest prerelease is what a user
// following the README must get.
func pickInstallableRelease(releases []release) string {
	best := ""
	for _, r := range releases {
		if r.TagName == "" || r.Draft {
			continue
		}
		if !r.Prerelease {
			return r.TagName
		}
		if best == "" {
			best = r.TagName
		}
	}
	return best
}

// resolveLatestRelease asks the release API for the newest installable
// release and returns its tag.
func resolveLatestRelease(client *http.Client) (string, error) {
	body, err := fetch(client, releaseAPIEndpoint())
	if err != nil {
		return "", err
	}
	releases, err := parseReleases(body)
	if err != nil {
		return "", err
	}
	tag := pickInstallableRelease(releases)
	if tag == "" {
		return "", fmt.Errorf("the release API named no installable release")
	}
	return tag, nil
}

func fetch(client *http.Client, rawurl string) ([]byte, error) {
	resp, err := client.Get(rawurl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", rawurl, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("empty response")
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("response exceeds %d bytes", maxDownload)
	}
	return data, nil
}

func checksumFor(sums []byte, asset string) string {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset && len(fields[0]) == sha256.Size*2 {
			if _, err := hex.DecodeString(fields[0]); err == nil {
				return strings.ToLower(fields[0])
			}
		}
	}
	return ""
}

func safeVersion(version string) bool {
	if version == "" || len(version) > 64 {
		return false
	}
	for _, r := range version {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' || r == '+' {
			continue
		}
		return false
	}
	return true
}

// installPayload is the post-verification commit step. It only changes paths
// below root and leaves runtime state untouched.
func installPayload(root, version string, payload []byte) error {
	if !safeVersion(version) {
		return fmt.Errorf("unsafe version string: %q", version)
	}
	if err := os.MkdirAll(filepath.Join(root, "versions"), 0o755); err != nil {
		return fmt.Errorf("cannot create install directory: %w", err)
	}
	stage, err := os.MkdirTemp(filepath.Join(root, "versions"), ".cmesh-install-")
	if err != nil {
		return fmt.Errorf("cannot create staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	// ★ THE STAGING DIRECTORY MUST BE TRAVERSABLE. (Round 103, found by
	// the install-closure audit.)
	//
	// os.MkdirTemp creates the directory with mode 0700, and the rename
	// below MOVES that mode onto the final versions/<version>/. So the
	// installed version directory was 0700 while the binary inside it
	// was 0755 — and a unit running as an unprivileged user could not
	// traverse INTO the directory to execute it:
	//
	//   Permission denied — ExecStart=/opt/container-mesh/current/cmesh
	//
	// The shell installer has always done this correctly (chmod 0755 on
	// the stage, and the install regression asserts it), so the two
	// installers disagreed about what they produce.
	if err := os.Chmod(stage, 0o755); err != nil {
		return fmt.Errorf("cannot make the staging directory traversable: %w", err)
	}

	binary := filepath.Join(stage, "cmesh")
	if err := os.WriteFile(binary, payload, 0o755); err != nil {
		return fmt.Errorf("cannot stage binary: %w", err)
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		return fmt.Errorf("cannot make staged binary executable: %w", err)
	}

	versions := filepath.Join(root, "versions")
	dest := filepath.Join(versions, version)
	backup := ""
	if _, err := os.Lstat(dest); err == nil {
		backup, err = os.MkdirTemp(versions, ".cmesh-old-")
		if err != nil {
			return fmt.Errorf("cannot prepare version replacement: %w", err)
		}
		os.Remove(backup)
		if err := os.Rename(dest, backup); err != nil {
			return fmt.Errorf("cannot replace existing version %s: %w", version, err)
		}
	}
	if err := os.Rename(stage, dest); err != nil {
		if backup != "" {
			_ = os.Rename(backup, dest)
		}
		return fmt.Errorf("cannot commit version %s: %w", version, err)
	}
	if err := replaceSymlink(filepath.Join(root, "cmesh"), filepath.Join("current", "cmesh")); err != nil {
		_ = os.RemoveAll(dest)
		if backup != "" {
			_ = os.Rename(backup, dest)
		}
		return fmt.Errorf("cannot switch cmesh link: %w", err)
	}
	if err := replaceSymlink(filepath.Join(root, "current"), filepath.Join("versions", version)); err != nil {
		return fmt.Errorf("cannot switch current version: %w", err)
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	marker := filepath.Join(root, ".cmesh-install-root")
	_ = os.WriteFile(marker, []byte("container-mesh managed install\n"), 0o644)
	return nil
}

func replaceSymlink(path, target string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cmesh-link-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Remove(tmpPath); err != nil {
		return err
	}
	if err := os.Symlink(target, tmpPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func uninstall(root, state string, purge bool) error {
	if root == "" || filepath.Clean(root) == string(filepath.Separator) {
		return errors.New("refusing to uninstall from an empty path or filesystem root")
	}
	// ★ THE UNITS MUST BE STOPPED AND DISABLED, OR THE UNINSTALL IS
	// INCOMPLETE — AND THE NEXT BOOT IGNITES A RESTART LOOP.
	//
	// The Go uninstaller used to remove the binaries and nothing else.
	// It never touched systemd. So the unit files stayed on disk, still
	// ENABLED, with ExecStart pointing at a path that no longer exists:
	// at the next boot systemd started the unit, got 203/EXEC, and — with
	// Restart=always — restarted it forever.
	//
	// The shell installer (packaging/install.sh) does stop and disable
	// the units, and names the ones it stopped in its output. This is
	// the same rule on the other path: any uninstall stops and disables
	// the units; only a purge also removes the unit FILES.
	//
	// The unit names are the deployed ones — cmesh-serve-discovery is
	// the unit that actually runs the anchor, and cmesh-discovery is
	// not it. A unit that is not present is simply skipped.
	stopped := stopAndDisableUnits(unitNames)

	// ★ THE STATE PATH IS GUARDED TOO, AND THAT GUARD IS THE ONE THAT
	// MATTERS MOST.
	//
	// The install root was checked above, but the PURGE branch then did
	// `os.RemoveAll(state)` with no check at all — and Go's os.RemoveAll
	// has NO root failsafe (unlike GNU rm, which refuses `rm -rf /` and
	// `rm -rf //`). So `CMESH_STATE=/ cmesh-install --uninstall --purge`
	// called os.RemoveAll("/") and DELETED THE FILESYSTEM ROOT.
	//
	// That is not theoretical: it happened, on a host that runs production
	// web services, during the audit that found this very defect. The
	// audit identified the hole and then demonstrated it on real
	// hardware. See docs/ROUND-LOG.md, Round 98.
	//
	// The lesson is the reason this guard exists in the first place: a
	// defect you have identified is a defect you do NOT demonstrate on
	// production hardware.
	if purge {
		if err := refuseDangerousPath(state, "runtime state directory"); err != nil {
			return err
		}
	}
	marker := filepath.Join(root, ".cmesh-install-root")
	if _, err := os.Stat(marker); err != nil {
		return fmt.Errorf("%s is not a cmesh install root", root)
	}
	if purge {
		if err := os.RemoveAll(root); err != nil {
			return fmt.Errorf("cannot purge install root: %w", err)
		}
		// The unit FILES go only on a purge — an uninstall keeps them.
		for _, u := range unitNames {
			_ = os.Remove(filepath.Join("/etc/systemd/system", u))
		}
		if state != "" && filepath.Clean(state) != filepath.Clean(root) {
			if err := os.RemoveAll(state); err != nil {
				return fmt.Errorf("cannot purge state directory: %w", err)
			}
		}
		return nil
	}
	for _, name := range []string{"cmesh", "current", "versions", ".cmesh-install-root"} {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			return fmt.Errorf("cannot remove %s: %w", name, err)
		}
	}
	// ★ AND THE OPERATOR IS TOLD WHAT WAS STOPPED. Otherwise they
	// believe the machine is clean when it is not.
	if stopped != "" {
		fmt.Printf("stopped and disabled: %s\n", stopped)
	}
	return nil
}

// unitNames are the systemd units this product installs.
//
// ★ THE DEPLOYED NAMES, NOT THE PACKAGING NAMES. The anchor runs as
// "cmesh-serve-discovery.service"; "cmesh-discovery.service" is the
// packaging name and is NOT what a deployed machine has. Stopping the
// wrong list leaves the real unit running and enabled — and the next
// boot starts it with a removed binary.
var unitNames = []string{
	"cmesh-serve-discovery.service",
	"cmesh-discovery.service",
	"cmesh-member.service",
	"cmesh-hub.service",
}

// stopAndDisableUnits stops and disables every unit that is present,
// and returns the names it acted on.
//
// It is best-effort: a machine without systemd has nothing to stop,
// and that is not an error. What matters is that a unit that IS there
// does not survive an uninstall.
func stopAndDisableUnits(units []string) string {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return ""
	}
	var stopped []string
	for _, u := range units {
		// Present means systemd knows it OR its file is on disk: a
		// stale enablement systemd has forgotten is exactly the case
		// this is here for.
		known := exec.Command("systemctl", "list-unit-files", u).Run() == nil
		_, statErr := os.Stat(filepath.Join("/etc/systemd/system", u))
		if !known && statErr != nil {
			continue
		}
		_ = exec.Command("systemctl", "stop", u).Run()
		_ = exec.Command("systemctl", "disable", u).Run()
		stopped = append(stopped, u)
	}
	if len(stopped) > 0 {
		_ = exec.Command("systemctl", "daemon-reload").Run()
	}
	return strings.Join(stopped, " ")
}

// refuseDangerousPath refuses the paths whose removal would take the
// machine with it.
//
// It is deliberately STRICTER than the string comparisons the shell
// installer uses, because those are bypassable: "/..", "/." and "//" all
// resolve to the filesystem root while looking like something else.
//
// The rules:
//   - the path must not be empty;
//   - it must not RESOLVE to the filesystem root (so "/", "//", "/.", "/.."
//     and "/a/.." are all refused);
//   - it must not BE the filesystem root's direct child (so "/.cmesh" and
//     "/etc" are refused — removing a directory that sits directly in the
//     root is what "rm -rf /.cmesh" was).
//
// The resolution is done with filepath.Abs + filepath.Clean, which
// collapses ".." and "//" without touching the filesystem (so it cannot
// itself be defeated by a symlink race on a missing path).
func refuseDangerousPath(path, what string) error {
	if path == "" {
		return fmt.Errorf("refusing to use an empty %s", what)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("refusing to use the %s %q: %v", what, path, err)
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return fmt.Errorf("refusing to use the filesystem root as the %s (%q resolves to /)", what, path)
	}
	// A path that sits DIRECTLY in the filesystem root is one `rm -rf`
	// away from being a root-level catastrophe, and it is exactly what
	// "${HOME:-}/.cmesh" becomes when HOME is unset.
	if filepath.Dir(abs) == string(filepath.Separator) {
		return fmt.Errorf("refusing to use %q as the %s: it would sit directly in the filesystem root", path, what)
	}
	return nil
}

// refuseRootOnly refuses ONLY the filesystem root (and the paths that
// resolve to it).
//
// It is the rule for a path the tool ADDS files under. A directory that
// sits directly in the root ("/cmesh") is a legitimate install root in
// the field — the install branch never removes the directory itself —
// so the stricter "direct child" rule of refuseDangerousPath belongs to
// the REMOVAL path only.
func refuseRootOnly(path, what string) error {
	if path == "" {
		return fmt.Errorf("refusing to use an empty %s", what)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("refusing to use the %s %q: %v", what, path, err)
	}
	if filepath.Clean(abs) == string(filepath.Separator) {
		return fmt.Errorf("refusing to use the filesystem root as the %s (%q resolves to /)", what, path)
	}
	return nil
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cmesh")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
