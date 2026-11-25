// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// The credential's SOURCE rules, as the defects found by reading the code
// say they must be.
//
// The team credential is the authority to join the network: whoever holds
// it can mint a device certificate under the account. So the rules about
// where it comes from are security rules, not convenience rules:
//
//   - a file that was CONFIGURED but cannot be read (or is empty) must be
//     an ERROR, never a fall-through: a fall-through joins the network
//     with a DIFFERENT credential than the operator asked for, and the
//     operator never finds out;
//   - an ambient CMESH_CRED must not break the INVITE path: the
//     environment variable is consulted only when the operator is actually
//     on the credential path;
//   - the order is FILE > FLAG > ENVIRONMENT, and the docs must say the
//     same thing the code does;
//   - the credential itself must not be written to disk.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// identityAccountForTest creates an account for a test that needs one.
func identityAccountForTest(t *testing.T) (*identity.Account, error) {
	t.Helper()
	return identity.GenerateAccount()
}

// encodeTestInvite renders an invite for a test, naming the given
// discovery address.
func encodeTestInvite(t *testing.T, acc *identity.Account, disc string) (string, error) {
	t.Helper()
	return discovery.EncodeInvite(discovery.Invite{
		AccountID:      acc.ID(),
		AccountPub:     acc.PublicKey(),
		DiscoveryAddrs: []string{disc},
		IssuedAt:       time.Now(),
	})
}

// readRepoFile reads a file from the repository root, so a test can check
// that the documentation says what the code does.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(blob)
}

// TestAnEmptyCredentialFileIsAnError: a credential file that holds only
// whitespace is an ERROR.
//
// This is the exact failure the file source exists to prevent. The
// operator's secret was truncated (a half-written file, an editor that
// stripped it, a rotation that emptied it), and the CLI silently joined
// with a DIFFERENT credential — the ambient CMESH_CRED, or nothing at
// all. The operator would see "joined" and never know which authority
// actually admitted the device.
//
// An empty file is not "no credential given": it IS a credential given,
// and it is broken. Say so, and name the file.
func TestAnEmptyCredentialFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cred")
	if err := os.WriteFile(path, []byte("   \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An ambient credential is present: the point is that the broken FILE
	// is used, not that the fallback is missing.
	t.Setenv("CMESH_CRED", "from-the-environment")

	_, err := resolveCredential("", path)
	if err == nil {
		t.Fatal("an empty credential file was accepted: the join would have used another credential")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the error does not name the file the operator has to fix: %v", err)
	}
}

// TestTheExplicitFlagBeatsTheAmbientEnvironment: the settled priority.
//
// The order is FILE > FLAG > ENVIRONMENT. The flag beats the ambient
// variable because a flag on the command line is a DELIBERATE choice by
// the operator who typed it, while an environment variable may have been
// exported for some other purpose entirely (a wrapper script, a CI job,
// a shell profile) and says nothing about what THIS command should do.
//
// The file beats the flag because it is the only source that keeps the
// credential out of both the process list and the process environment.
func TestTheExplicitFlagBeatsTheAmbientEnvironment(t *testing.T) {
	t.Setenv("CMESH_CRED", "from-the-environment")

	got, err := resolveCredential("from-the-flag", "")
	if err != nil {
		t.Fatalf("resolveCredential: %v", err)
	}
	if got != "from-the-flag" {
		t.Fatalf("resolveCredential(flag, env) = %q, want the flag's value %q", got, "from-the-flag")
	}
}

// TestTheCredentialFileBeatsTheExplicitFlag: the file is the strongest
// source.
//
// systemd's LoadCredential hands the service a private copy that only its
// own process can read, so the credential ends up in neither the process
// list nor the environment. That is worth more than the flag's
// deliberateness — and the operator who configured a file wants THAT
// file used.
func TestTheCredentialFileBeatsTheExplicitFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cred")
	const secret = "cmesh1.from-the-file"
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CMESH_CRED", "from-the-environment")

	got, err := resolveCredential("from-the-flag", path)
	if err != nil {
		t.Fatalf("resolveCredential: %v", err)
	}
	if got != secret {
		t.Fatalf("resolveCredential(flag, file) = %q, want the file's value %q", got, secret)
	}
}

// TestAnAmbientCredentialDoesNotBreakTheInvitePath is the regression test
// for the defect an exported CMESH_CRED caused.
//
// The credential was resolved BEFORE the "either a credential or an
// invite" check, so on a host that exports CMESH_CRED globally — a
// wrapper script, a CI runner, a shell profile — `cmesh join <invite>`
// failed with "pass either --cred or an invite string, not both". The
// operator's invite was fine; the ambient variable broke it.
//
// The environment is consulted only when the operator is actually on the
// credential path: no invite positional argument was given, and neither
// --cred nor --cred-file was supplied.
func TestAnAmbientCredentialDoesNotBreakTheInvitePath(t *testing.T) {
	t.Setenv("CMESH_CRED", "from-the-environment")

	// The join is called with an INVITE and no credential source at all.
	// The failure must be about the invite (or about the device that is
	// missing), never about "both".
	err := Run([]string{"join", "--state", t.TempDir(),
		"--device-passphrase", "test-pass", "cmesh://join?x=1"})
	if err == nil {
		t.Fatal("the join succeeded with a nonsense invite: the test is not exercising the path")
	}
	msg := err.Error()
	if strings.Contains(msg, "not both") {
		t.Fatalf("an ambient CMESH_CRED broke the invite path: %q", msg)
	}
}

// TestAnAmbientCredentialDoesNotBreakTheInvitePathWithARealInvite is the
// same rule with a REAL invite: the join must get past the "either a
// credential or an invite" check and fail on the invite's own merits.
//
// The first test uses a nonsense invite, which fails EARLY — before the
// device-identity check. This one uses an invite that decodes, so the
// join gets as far as the device it needs, and the ambient variable
// still must not turn that into "not both".
func TestAnAmbientCredentialDoesNotBreakTheInvitePathWithARealInvite(t *testing.T) {
	acc, err := identityAccountForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := encodeTestInvite(t, acc, "mesh.example:18801")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CMESH_CRED", "from-the-environment")

	// No device identity in this directory: the join must complain about
	// the missing identity, which is the invite path's own complaint.
	err = Run([]string{"join", "--state", t.TempDir(),
		"--device-passphrase", "test-pass", inv})
	if err == nil {
		t.Fatal("the join succeeded with no device identity: the test is not exercising the path")
	}
	msg := err.Error()
	if strings.Contains(msg, "not both") {
		t.Fatalf("an ambient CMESH_CRED broke the invite path: %q", msg)
	}
	if !strings.Contains(msg, "identity") {
		t.Fatalf("the join did not get as far as the invite path's own check: %q", msg)
	}
}

// TestAnAmbientCredentialIsUsedWhenNoInviteIsGiven: the environment is
// still a working source — it is simply consulted LAST, and only on the
// credential path.
func TestAnAmbientCredentialIsUsedWhenNoInviteIsGiven(t *testing.T) {
	t.Setenv("CMESH_CRED", "from-the-environment")
	got, err := resolveCredential("", "")
	if err != nil {
		t.Fatalf("resolveCredential: %v", err)
	}
	if got != "from-the-environment" {
		t.Fatalf("resolveCredential(env) = %q, want %q", got, "from-the-environment")
	}
}

// TestTheCredentialIsNotWrittenToDisk: the credential must not outlive
// the join.
//
// The credential string carries the DELEGATOR PRIVATE KEY (the encoded
// credential is `pub.secret`). It was written into discovery.json's
// "invite" field on the credential path, and that file stays on disk
// indefinitely: it is not removed when the credential is revoked, and it
// lands in every backup or snapshot of the state directory.
//
// Nothing needs it there: the derived fields (account id + discovery
// addresses) are what every later command reads, and the credential path
// sets them. The raw string was dead weight — and it was the heaviest
// kind of dead weight.
func TestTheCredentialIsNotWrittenToDisk(t *testing.T) {
	dir := t.TempDir()
	const secret = "cmesh1.the-delegator-private-key"

	if _, err := saveDiscoveryConfig(dir, discoveryConfig{
		Invite:         secret,
		AccountID:      "acct-1",
		AccountPub:     []byte("key"),
		DiscoveryAddrs: []string{"mesh.example:18801"},
	}); err != nil {
		t.Fatalf("saveDiscoveryConfig: %v", err)
	}

	blob, err := os.ReadFile(filepath.Join(dir, "discovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), secret) {
		t.Fatalf("the saved configuration carries the credential secret:\n%s", blob)
	}
}

// TestTheSavedConfigurationIsPrivateEvenWhenTheFileAlreadyExists: the
// configuration is written with mode 0600 — but os.WriteFile does NOT
// change the mode of a file that ALREADY EXISTS.
//
// So a discovery.json restored from a backup with mode 0644 stayed
// world-readable, and the file holds the account key and the discovery
// addresses of the network. The write has to fix the mode, not merely
// ask for it.
func TestTheSavedConfigurationIsPrivateEvenWhenTheFileAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "discovery.json")
	// A file that came back from a backup with the wrong mode.
	if err := os.WriteFile(path, []byte(`{"invite":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := saveDiscoveryConfig(dir, discoveryConfig{
		AccountID:      "acct-1",
		DiscoveryAddrs: []string{"mesh.example:18801"},
	}); err != nil {
		t.Fatalf("saveDiscoveryConfig: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the saved configuration is mode %o, want 0600: the file was world-readable", mode)
	}
}

// TestTheCredentialSourceRulesAreTheDocumentedOnes keeps the docs and the
// code in step.
//
// The documentation claimed a priority the code did not implement (the
// docs said file > env > flag, the code said file > flag > env), and a
// reader who trusted the docs would pick the wrong source.
func TestTheCredentialSourceRulesAreTheDocumentedOnes(t *testing.T) {
	doc := readRepoFile(t, "docs/design/CREDENTIAL.md")
	// The rules live in §2.1; the rest of the document mentions the
	// sources in passing.
	start := strings.Index(doc, "### 2.1")
	if start < 0 {
		t.Fatal("the credential document has no §2.1 section")
	}
	rest := doc[start:]
	if end := strings.Index(rest[3:], "\n## "); end >= 0 {
		rest = rest[:end+3]
	}
	fileAt := strings.Index(rest, "`--cred-file")
	flagAt := strings.Index(rest, "`--cred ")
	envAt := strings.Index(rest, "`CMESH_CRED")
	if fileAt < 0 || flagAt < 0 || envAt < 0 {
		t.Fatalf("§2.1 does not carry the three sources:\n%s", rest)
	}
	if !(fileAt < flagAt && flagAt < envAt) {
		t.Fatalf("the documented order in §2.1 is not file > flag > environment "+
			"(file at %d, flag at %d, env at %d):\n%s", fileAt, flagAt, envAt, rest)
	}
}

// TestThePanelTokenFileMustBeReadable: a token file that was CONFIGURED
// but cannot be read must be an ERROR.
//
// The panel's data endpoint hands out the whole roster. If the operator
// configured --panel-token-file and the file cannot be read, the node
// used a DIFFERENT token — the environment's, the flag's, or a freshly
// generated one — and the dashboard's saved token silently stopped
// working. The operator's dashboard goes blank with no error anywhere.
//
// The same shape as the credential's file: a configured source that
// cannot be read is a MISTAKE to report, not a reason to guess.
func TestThePanelTokenFileMustBeReadable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	t.Setenv("CMESH_PANEL_TOKEN", "env-token")

	tok, generated, err := resolvePanelTokenFrom("", missing)
	if err == nil {
		t.Fatalf("a missing token file was accepted: the node would have run with the token %q", tok)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("the error does not name the file the operator has to fix: %v", err)
	}
	if generated {
		t.Fatal("the failed resolution reported a generated token")
	}
}

// TestAnEmptyPanelTokenFileIsAnError: the same for a file that holds
// nothing.
func TestAnEmptyPanelTokenFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel-token")
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CMESH_PANEL_TOKEN", "env-token")

	tok, _, err := resolvePanelTokenFrom("", path)
	if err == nil {
		t.Fatalf("an empty token file was accepted: the node would have run with the token %q", tok)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the error does not name the file the operator has to fix: %v", err)
	}
}

// TestTheGeneratedPanelTokenIsStillGenerated: the error path must not
// swallow the "generated" flag's meaning.
//
// A token the process GENERATED is printed once — the operator has no
// other way to learn it. A token the operator SUPPLIED is not printed.
// The error path must not change either.
func TestTheGeneratedPanelTokenIsStillGenerated(t *testing.T) {
	os.Unsetenv("CMESH_PANEL_TOKEN")
	tok, generated, err := resolvePanelTokenFrom("", "")
	if err != nil {
		t.Fatalf("resolvePanelTokenFrom: %v", err)
	}
	if !generated {
		t.Fatal("a generated token was reported as supplied: the operator would never learn it")
	}
	if strings.TrimSpace(tok) == "" {
		t.Fatal("the generated token is blank")
	}
}
