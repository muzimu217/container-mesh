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
	// ★ THE ROLLBACK PATH IS A COMMAND, NOT A MANUAL SYMLINK EDIT.
	//
	// The installer keeps every version under versions/<version>/ and
	// points `current` at one of them, so rolling back IS a symlink
	// rotation. But rotating it by hand has no floor check, and a
	// version the units cannot drive turns into a restart loop that
	// the operator finds out about from the journal minutes later.
	//
	// This command performs the rotation AND refuses a version below
	// the floor — before anything is touched.
	if len(os.Args) > 1 && os.Args[1] == "--rollback" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: cmesh-install --rollback <version>")
			os.Exit(2)
		}
		if err := rollback(envOr("CMESH_INSTALL", "."), os.Args[2]); err != nil {
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
	if err := refuseInstallRoot(dir); err != nil {
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

	if err := installPayloadWithDigest(dir, version, payload, expected); err != nil {
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

// safeVersion reports whether a version string may be used as a
// directory name under `versions/`.
//
// ★ IT MUST NOT BE ABLE TO WALK OUT OF `versions/`. (Round 107.)
// The allowed charset includes ".", so ".." and "." passed the check —
// and the version is then JOINED into a path:
//
//	bin  = root/versions/<version>/cmesh
//	link = root/versions/<version>
//
// With version=".." both COLLAPSE out of the versions directory:
//
//	bin  = root/cmesh      ← the "current" symlink, not a version
//	link = root            ← the install root itself
//
// So `cmesh-install --rollback ..` would rotate `current` to point at
// the install ROOT, and the documented layout
// (<install>/current -> versions/<version>) would be destroyed.
//
// So a version made only of dots is refused. A real version always
// carries at least one non-dot character ("v1.0.0-rc1", "1.0.0+build").
//
// ★ AND: this is NOT a shell-injection guard. Nothing in this tool
// reaches a shell — every process spawn is `exec.Command(prog, args...)`,
// which calls execve directly with an argv list, so a value containing
// ";" or "$(...)" is a LITERAL argument. The scanner's finding on line
// 676 was a false positive; this is a PATH defect, and it is fixed on
// its own terms.
func safeVersion(version string) bool {
	if version == "" || len(version) > 64 {
		return false
	}
	hasNonDot := false
	for _, r := range version {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '+' {
			hasNonDot = true
			continue
		}
		if r == '.' {
			continue
		}
		return false
	}
	// A version of only dots ("." / ".." / "...") resolves to the
	// parent (or the versions directory itself), so it is refused.
	return hasNonDot
}

// digestFileName is the file that records a version's expected digest
// next to its binary.
//
// ★ WHY IT EXISTS. (Round 107.) The install path verifies the payload
// against the release's SHA256SUMS and then discards the digest, so
// nothing on disk attests to what the installed binary is. The
// rollback path therefore has nothing to compare against — and it
// EXECUTES the binary (to ask it for --help) before rotating
// `current` onto it.
const digestFileName = "cmesh.sha256"

// installPayload is the post-verification commit step. It only changes paths
// below root and leaves runtime state untouched.
//
// It does NOT record the digest, so a version installed this way cannot
// be attested later — and the rollback path REFUSES a version it cannot
// verify. Use installPayloadWithDigest for the real install.
func installPayload(root, version string, payload []byte) error {
	return installPayloadWithDigest(root, version, payload, "")
}

// installPayloadWithDigest is installPayload plus the recording of the
// payload's digest beside the binary.
//
// ★ WHY THE DIGEST IS RECORDED. (Round 107.) The install path verifies
// the payload against the release's SHA256SUMS and then throws the
// digest away — nothing on disk records it. So a later `--rollback` had
// nothing to compare against, and it EXECUTED whatever sat in
// versions/<version>/cmesh (the compatibility probe runs the binary
// with "--help").
//
// That is the same trust hole the install path closes, left open on the
// rollback path: the binary was verified when it was installed (maybe
// long ago, maybe by the SHELL installer, which does not write this
// file), and nothing re-verifies it.
//
// So the digest is stored beside the binary, and the rollback reads it
// back BEFORE executing anything.
//
// An empty expected digest means "the caller could not verify it", and
// then nothing is recorded — and the rollback refuses that version
// rather than executing it on trust.
func installPayloadWithDigest(root, version string, payload []byte, expected string) error {
	if !safeVersion(version) {
		return fmt.Errorf("unsafe version string: %q", version)
	}
	versions := filepath.Join(root, "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		return fmt.Errorf("cannot create install directory: %w", err)
	}

	// ★ THE INSTALL ROOT AND THE versions/ DIRECTORY MUST BE
	// TRAVERSABLE BY THE SERVICE USER. (Round 104.)
	//
	// The systemd units run the node as an unprivileged "cmesh" user and
	// execute
	//
	//	ExecStart=/opt/container-mesh/current/cmesh ...
	//
	// so the service account has to be able to REACH the binary. On
	// Linux that means the execute (traverse) bit on every directory on
	// the path: the install root, versions/, and versions/<version>/.
	//
	// os.MkdirAll creates the tree with the PROCESS'S umask, and the
	// umask applies to the INTERMEDIATE directories too — so
	// os.MkdirAll(root+"/versions", 0755) creates root itself at 0700
	// under a umask of 077.
	//
	// ★ THE HISTORICAL BUG, AND WHY IT WAS STILL ALIVE.
	//
	// This exact class of defect has already been fixed twice in this
	// installer: once for the staging directory (MkdirTemp creates
	// 0700) and once for the version directory. Both fixes were applied
	// to the LEAF, and the tree AROUND them was left alone — so the
	// install root, the directory that holds versions/, was still 0700,
	// and the service user still could not get in.
	//
	// The symptom is the worst kind, because it looks like a different
	// problem: the unit starts, is scheduled, and then fails with
	//
	//	Failed to execute ...: Permission denied
	//
	// which systemd reports as status=203/EXEC — the SAME status a
	// MISSING binary produces. The operator's first conclusion is "the
	// install did not happen", and the real cause is a directory the
	// service user cannot walk into.
	//
	// The explicit Chmod is what makes the mode independent of the
	// umask: MkdirAll's 0755 is a REQUEST, and the kernel's umask gets
	// the last word.
	for _, dir := range []string{root, versions} {
		if err := os.Chmod(dir, 0o755); err != nil {
			return fmt.Errorf("cannot make %s traversable: %w", dir, err)
		}
	}

	stage, err := os.MkdirTemp(versions, ".cmesh-install-")
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

	// ★ THE DIGEST IS WRITTEN NEXT TO THE BINARY. (Round 107.)
	//
	// The install path verifies the payload against the release's
	// SHA256SUMS — and then THROWS THE DIGEST AWAY. Nothing on disk
	// records it, so a later `--rollback` has nothing to compare
	// against and ends up EXECUTING whatever sits in
	// versions/<version>/cmesh.
	//
	// That is the same trust hole the install path closes, left open
	// on the rollback path: the binary was verified when it was
	// installed (possibly by the SHELL installer, possibly long ago),
	// and nothing re-verifies it. A file placed there by anything else
	// is executed with the installer's privileges.
	//
	// So the digest is stored beside the binary, and the rollback
	// reads it back BEFORE executing anything.
	if expected != "" {
		sumFile := filepath.Join(stage, digestFileName)
		if err := os.WriteFile(sumFile, []byte(expected+"  cmesh\n"), 0o644); err != nil {
			return fmt.Errorf("cannot record the binary digest: %w", err)
		}
	}

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
	// ★ THE INSTALL ROOT GETS THE SAME "DIRECT CHILD OF THE ROOT" RULE
	// THE STATE PATH HAS.
	//
	// The state path was guarded strictly while the install root only
	// had the root check, and that asymmetry let a RELATIVE install root
	// through: a relative path resolves against the working directory,
	// so CMESH_INSTALL=".." points at the parent of wherever the
	// operator happened to be — and the purge then removed THAT
	// directory.
	if err := refuseInstallRoot(root); err != nil {
		return err
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
		// ★ AND THE OPERATOR IS TOLD WHAT WAS STOPPED AND WHAT WAS
		// REMOVED. (Round 104.)
		//
		// The purge removed the unit FILES as well as stopping them, so
		// the operator's question — "what is left on this machine?" —
		// has a LONGER answer here than it does for a plain uninstall.
		//
		// The report used to be printed only on the plain-uninstall path:
		// the purge branch returned before reaching it. So the operator
		// who purged the machine, and whose unit files were DELETED, was
		// told nothing at all.
		reportUninstall(stopped, state, true)
		return nil
	}
	for _, name := range []string{"cmesh", "current", "versions", ".cmesh-install-root"} {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			return fmt.Errorf("cannot remove %s: %w", name, err)
		}
	}
	// ★ AND THE OPERATOR IS TOLD WHAT WAS STOPPED. Otherwise they
	// believe the machine is clean when it is not.
	reportUninstall(stopped, state, false)
	return nil
}

// reportUninstall tells the operator what the uninstall did.
//
// ★ EVERY UNINSTALL REPORTS. Even one that found nothing to stop.
//
// A silent uninstall is indistinguishable from one that did nothing,
// and the operator's next question is always "is it clean?". The
// report answers it in full:
//
//   - which units were stopped and disabled (and that they are no
//     longer enabled, so the next boot does not start a unit whose
//     ExecStart points at a binary that no longer exists);
//   - what happened to the runtime state, WHICH IS NOT THE SAME THING
//     on the two paths.
//
// The state difference is the part that matters most. The plain
// uninstall KEEPS the state — and that state holds the device's private
// key envelope and the credential material — so an operator who is
// told "uninstalled" and not told about the state believes the machine
// was cleaned when the device's key envelope is still on disk.
//
// The purge REMOVES the state, so the next install starts from a blank
// identity. That is a fact the operator has to have BEFORE they find
// out.
func reportUninstall(stopped, state string, purge bool) {
	if stopped == "" {
		fmt.Println("stopped and disabled: no cmesh systemd units were running")
	} else {
		fmt.Printf("stopped and disabled: %s\n", stopped)
	}
	if state == "" {
		return
	}
	if purge {
		fmt.Printf("purged install root and runtime state: %s\n", state)
		return
	}
	fmt.Printf("uninstalled binaries; runtime state was retained at %s\n", state)
}

// unitDriverFlags are the CLI flags the shipped systemd units pass.
//
// The units drive the binary like this:
//
//	ExecStart=/opt/container-mesh/current/cmesh serve-discovery --state … --account-pub … --panel-token-file …
//	ExecStart=/opt/container-mesh/current/cmesh join --state … --cred-file … --relay-trunk-id …
//
// A version that does not accept those flags cannot be driven by the
// units, and the failure is not a clean one: the binary exits with
// "flag provided but not defined" (status=2), and Restart=on-failure
// restarts it until the start limit stops it. The operator sees a
// restart loop, not "this version is too old".
//
// The list is EVERY long flag a shipped unit passes, because every one
// of them is a flag whose absence breaks that unit. It is checked
// against the unit files by the packaging regression, so it cannot
// drift away from what the units actually pass.
var unitDriverFlags = map[string][]string{
	"serve-discovery": {
		"account-pub", "addr", "announce", "panel",
		"panel-token-file", "relay-trunk", "state",
	},
	"join": {
		"cred-file", "listen", "relay-trunk", "relay-trunk-id", "state",
	},
}

// checkUnitCompatibility reports whether the binary at path can be
// driven by the shipped systemd units.
//
// ★ WHY IT EXECUTES THE BINARY.
//
// The question is "does this CLI accept this flag", and the only
// honest way to answer it is to ask the CLI: the flag name can appear
// in unrelated bytes, and a version can carry a flag in its help
// without accepting it.
//
// Running "--help" is also exactly what the unit does, and on the
// INSTALL path the binary is checksum-verified against the release's
// SHA256SUMS BEFORE this runs — so the thing being executed is the
// published artifact.
//
// ★ BUT THE ROLLBACK PATH IS DIFFERENT, AND THIS COMMENT USED TO
// SAY OTHERWISE. (Round 107.) The rollback path verifies the digest
// recorded BESIDE the installed binary (see verifyInstalledDigest)
// before calling this — and REFUSES a version that has no recorded
// digest, rather than executing it on trust. So the claim "the thing
// being executed is the published artifact" holds on both paths, but
// only because the rollback path now checks it.
//
// ★ AND: this is NOT a shell. `exec.Command(prog, args...)` calls
// execve directly with an argv list — there is no `sh -c` anywhere in
// this tool — so an argument containing ";" or "$(...)" is a LITERAL
// argument, never interpreted. The scanner's finding on this line
// ("command injection") was a false positive; the real defects nearby
// were a path traversal and a missing digest attestation, and both are
// fixed.
//
// The probe asks for help and never starts the service: "--help" is
// handled by the flag parser, which prints the usage and exits.
func checkUnitCompatibility(binPath string) error {
	for sub, flags := range unitDriverFlags {
		out, err := exec.Command(binPath, sub, "--help").CombinedOutput()
		if err != nil {
			// ★ A NON-ZERO EXIT IS NOT A FAILURE TO RUN.
			//
			// "--help" is handled by the flag package, which prints the
			// usage and EXITS 2. So the normal, successful case of this
			// probe is a non-zero exit — and treating that as "the
			// binary is broken" would refuse every version, including
			// the good ones.
			//
			// What matters is whether the binary RAN. An ExitError
			// means it ran and chose that exit code; any other error
			// means it could not be started at all — a broken install,
			// and "cannot tell" is not "compatible".
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return fmt.Errorf("cannot check %s: %w", binPath, err)
			}
		}
		usage := string(out)
		if !strings.Contains(usage, "Usage of "+sub+":") {
			return fmt.Errorf("%s has no %q subcommand, so the systemd unit "+
				"cannot drive it at all (the unit's ExecStart names it)",
				binPath, sub)
		}
		accepted := flagsInUsage(usage)
		for _, flag := range flags {
			if !accepted[flag] {
				return fmt.Errorf("%s does not accept --%s, so the systemd unit "+
					"would fail with \"flag provided but not defined\" and restart "+
					"until the start limit stops it; this version is below the "+
					"rollback floor", binPath, flag)
			}
		}
	}
	return nil
}

// flagsInUsage returns the set of flags a `--help` output declares.
//
// ★ THE MATCHING IS EXACT, AND THAT MATTERS.
//
// The usage lines look like this:
//
//	-panel
//	-panel-token-file string
//	-relay-trunk
//
// A substring search for "-panel" would match "-panel-token-file" too —
// so a version that LOST the boolean -panel would be accepted, and the
// unit would then fail with "flag provided but not defined".
//
// So each line is parsed: the flag name is the text between the dash
// and the first whitespace, and the comparison is on that name alone.
func flagsInUsage(usage string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(usage, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, "-") {
			continue
		}
		name := strings.TrimPrefix(trimmed, "-")
		if idx := strings.IndexAny(name, " \t"); idx >= 0 {
			name = name[:idx]
		}
		if name == "" {
			continue
		}
		out[name] = true
	}
	return out
}

// rollback re-points `current` at an already-installed version.
//
// ★ THE ROLLBACK PATH IS: the installer keeps every version under
// versions/<version>/ and points `current` at one of them. Rolling
// back is therefore a symlink rotation — and this is the command that
// performs it, WITH the floor check.
//
// The floor check is what makes the rotation safe. Rotating `current`
// to a version the units cannot drive does not fail loudly at the
// moment of rotation: the symlink rotates happily and the unit dies
// on the next start with "flag provided but not defined" (status=2),
// then restarts until the start limit stops it. The operator finds
// out from a journal, minutes later, with no indication that the
// rollback was the cause.
//
// ★ THE REFUSAL HAPPENS BEFORE ANYTHING IS TOUCHED.
//
// A rollback that half-succeeds leaves the service pointing at
// nothing, which is worse than not rolling back at all. So the
// version is checked FIRST, and `current` is only rotated once the
// check has passed.
// verifyInstalledDigest checks that the binary in a version directory
// is the one that was verified when it was installed.
//
// ★ WHY THIS EXISTS. (Round 107.) The install path verifies the
// downloaded payload against the release's SHA256SUMS and then throws
// the digest away — nothing on disk recorded it. So the rollback path
// had nothing to compare against, and it EXECUTED whatever sat in
// versions/<version>/cmesh (the compatibility probe runs the binary
// with "--help").
//
// The binary was verified WHEN IT WAS INSTALLED — but that may have
// been long ago, and it may have been installed by the SHELL
// installer, which does not write this file. So the honest rule is:
// **if there is no recorded digest, the version cannot be attested,
// and a rollback to it is refused** rather than executed on trust.
//
// ★ The refusal is deliberately LOUD. A rollback that is refused with a
// clear reason is recoverable; a rollback that silently executes a
// swapped binary is not.
func verifyInstalledDigest(versionDir, binPath string) error {
	sumFile := filepath.Join(versionDir, digestFileName)
	raw, err := os.ReadFile(sumFile)
	if err != nil {
		return fmt.Errorf("this version has no recorded digest (%s), so it cannot be "+
			"verified before it is executed — refusing to run it on trust", sumFile)
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return fmt.Errorf("%s is empty, so the version cannot be verified", sumFile)
	}
	expected := strings.ToLower(fields[0])
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("%s does not hold a sha256 digest", sumFile)
	}

	payload, err := os.ReadFile(binPath)
	if err != nil {
		return fmt.Errorf("cannot read the installed binary: %w", err)
	}
	sum := sha256.Sum256(payload)
	actual := hex.EncodeToString(sum[:])
	if actual != expected {
		return fmt.Errorf("the installed binary does not match the digest recorded "+
			"when it was installed (expected %s, got %s) — refusing to execute it", expected, actual)
	}
	return nil
}

func rollback(root, version string) error {
	if !safeVersion(version) {
		return fmt.Errorf("unsafe version string: %q", version)
	}
	marker := filepath.Join(root, ".cmesh-install-root")
	if _, err := os.Stat(marker); err != nil {
		return fmt.Errorf("%s is not a cmesh install root", root)
	}
	bin := filepath.Join(root, "versions", version, "cmesh")
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("version %s is not installed under %s", version, root)
	}
	// ★ THE DIGEST IS CHECKED BEFORE THE BINARY IS EXECUTED.
	// (Round 107.) checkUnitCompatibility RUNS the binary (to ask it for
	// --help), so the digest has to be verified BEFORE that call — an
	// unverified binary must never be executed, not even to ask it a
	// question.
	if err := verifyInstalledDigest(filepath.Join(root, "versions", version), bin); err != nil {
		return fmt.Errorf("refusing to roll back to %s: %w", version, err)
	}
	if err := checkUnitCompatibility(bin); err != nil {
		return fmt.Errorf("refusing to roll back to %s: %w", version, err)
	}
	if err := replaceSymlink(filepath.Join(root, "cmesh"), filepath.Join("current", "cmesh")); err != nil {
		return fmt.Errorf("cannot switch cmesh link: %w", err)
	}
	if err := replaceSymlink(filepath.Join(root, "current"), filepath.Join("versions", version)); err != nil {
		return fmt.Errorf("cannot switch current version: %w", err)
	}
	fmt.Printf("rolled back to %s\n", version)
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

// refuseInstallRoot refuses an install root that the tool would write
// into or remove.
//
// ★ THE "DIRECT CHILD OF THE ROOT" RULE APPLIES HERE TOO.
//
// The state path has always been guarded strictly (a path directly in
// the root is refused), while the install root only had the root
// check. That asymmetry let a RELATIVE install root through: a
// relative path resolves against the working directory, so
// CMESH_INSTALL=".." points at the parent of wherever the operator
// happened to be — and the installer wrote versions/, current, cmesh
// and .cmesh-install-root into that directory.
//
// The install branch only ADDS files, so the damage is litter rather
// than deletion — but a tool that will happily write into a directory
// it was not pointed at will do it again on a typo. The shell
// installer (packaging/install.sh) carries the same rule.
func refuseInstallRoot(path string) error {
	if path == "" {
		return fmt.Errorf("refusing to use an empty install root")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("refusing to use the install root %q: %v", path, err)
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return fmt.Errorf("refusing to use the filesystem root as the install root (%q resolves to /)", path)
	}
	// A path that sits DIRECTLY in the filesystem root.
	if filepath.Dir(abs) == string(filepath.Separator) {
		return fmt.Errorf("refusing to use %q as the install root: it would sit directly in the filesystem root", path)
	}
	// ★ A RELATIVE PATH THAT CLIMBS ABOVE THE WORKING DIRECTORY.
	//
	// filepath.Abs resolves the path against the working directory, so
	// ".." becomes the parent of it — a perfectly ordinary directory
	// that the operator never meant to point at. The install branch
	// writes into it; the purge removes it.
	//
	// The rule is about climbing OUT, not about being relative: a
	// relative path that stays inside the working directory is fine.
	if !filepath.IsAbs(path) && climbsAbove(path) {
		return fmt.Errorf("refusing to use %q as the install root: "+
			"it climbs above the working directory (%s)", path, abs)
	}
	return nil
}

// climbsAbove reports whether a relative path contains a ".." that
// takes it above the directory it starts in.
//
// It is pure string arithmetic over the path's components: the first
// ".." that is not cancelled by a preceding ordinary component is the
// one that escapes. A path that never goes above its starting point
// returns false.
func climbsAbove(rel string) bool {
	depth := 0
	for _, comp := range strings.Split(rel, string(filepath.Separator)) {
		switch comp {
		case "", ".":
			// no effect
		case "..":
			if depth == 0 {
				return true
			}
			depth--
		default:
			depth++
		}
	}
	return false
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
