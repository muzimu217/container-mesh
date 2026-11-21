package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestThePanelTokenIsNotPrintedWhenTheOperatorSuppliedIt is the
// regression test for the security defect the installation review found.
//
// The panel's data endpoint hands out the WHOLE roster, and the token is
// the only thing standing in front of it. The token used to be printed
// on startup:
//
//   - into the journal (Storage=persistent), which the
//     systemd-journal GROUP can read;
//   - and, when it was passed on the command line, it was visible to
//     EVERY local user through /proc/<pid>/cmdline.
//
// Measured on the real machine: an unprivileged local user read the
// token out of /proc and pulled the entire roster with it.
//
// So a token the operator supplied (flag or environment) is NOT
// printed. One the process GENERATED is printed — the operator has no
// other way to learn it — and that is the only case that writes it.
func TestThePanelTokenIsNotPrintedWhenTheOperatorSuppliedIt(t *testing.T) {
	const tok = "operator-supplied-token-abc123"

	// From the flag.
	got, generated := resolvePanelToken(tok)
	if got != tok {
		t.Fatalf("the token is %q, want the one the operator supplied", got)
	}
	if generated {
		t.Fatal("a token the operator supplied was reported as generated: it would be printed into the log")
	}

	// From the environment.
	t.Setenv("CMESH_PANEL_TOKEN", tok)
	got, generated = resolvePanelToken("")
	if got != tok {
		t.Fatalf("the token is %q, want the one from the environment", got)
	}
	if generated {
		t.Fatal("a token from the environment was reported as generated: it would be printed into the log")
	}
}

// TestThePanelTokenFallsBackToAGeneratedOne: with nothing supplied, the
// process generates a token — and THAT one is printed, because the
// operator has no other way to learn it.
func TestThePanelTokenFallsBackToAGeneratedOne(t *testing.T) {
	os.Unsetenv("CMESH_PANEL_TOKEN")

	got, generated := resolvePanelToken("")
	if !generated {
		t.Fatal("a token was reported as supplied when none was: the operator would never learn it")
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("the generated token is blank")
	}
}

// TestTwoGeneratedTokensDiffer: a fixed or guessable token would make
// the roster readable by anyone who finds the port.
func TestTwoGeneratedTokensDiffer(t *testing.T) {
	os.Unsetenv("CMESH_PANEL_TOKEN")
	a, _ := resolvePanelToken("")
	b, _ := resolvePanelToken("")
	if a == b {
		t.Fatal("two generated tokens are the same")
	}
}

// TestThePanelTokenCanComeFromAFile is the regression test for the
// external tester's second point: the environment fallback only REDUCES
// the exposure, it does not remove it.
//
// A token in the environment is still in the process's environ, which
// every process running as the same user can read — the service's own
// children included. A token in a FILE that only the service can read
// is in neither.
//
// systemd's LoadCredential hands the service exactly that: a private
// copy of a root-owned file, readable by the service's process and by
// nobody else.
func TestThePanelTokenCanComeFromAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel-token")
	const tok = "file-token-abc123"
	// systemd's credential files carry a trailing newline.
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, generated, err := resolvePanelTokenFrom("", path)
	if err != nil {
		t.Fatalf("resolvePanelTokenFrom: %v", err)
	}
	if got != tok {
		t.Fatalf("the token is %q, want the one from the file", got)
	}
	if generated {
		t.Fatal("a token from the file was reported as generated: it would be printed into the log")
	}

	// The file wins over the flag and the environment: it is the
	// strongest source, and a file that cannot be read must not silently
	// fall back to something weaker.
	t.Setenv("CMESH_PANEL_TOKEN", "env-token")
	got, _, err = resolvePanelTokenFrom("flag-token", path)
	if err != nil {
		t.Fatalf("resolvePanelTokenFrom: %v", err)
	}
	if got != tok {
		t.Fatalf("the token is %q, want the one from the file", got)
	}
}

// TestAMissingTokenFileIsAnError: a file that cannot be read must not
// silently fall through to a weaker source — the operator would think
// the file was in use.
//
// The test's name and its assertion have to agree, and the BEHAVIOUR has
// to be the loud one: the operator configured --panel-token-file, the
// file was not there, and the node started with the ENVIRONMENT's token
// instead. The dashboard's saved token then silently stopped working —
// the panel went blank with no error anywhere.
//
// The environment is a DOWNGRADE, not a clean zero: the token is there,
// but every process running as the same user can read it out of
// /proc/<pid>/environ. Falling back to it is not "no token", it is "a
// token with a worse exposure", and nobody asked for that.
func TestAMissingTokenFileIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	t.Setenv("CMESH_PANEL_TOKEN", "env-token")

	tok, generated, err := resolvePanelTokenFrom("", missing)
	if err == nil {
		t.Fatalf("a missing token file was accepted and the token %q was used instead", tok)
	}
	if generated {
		t.Fatal("the failed resolution reported a generated token")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("the error does not name the file the operator has to fix: %v", err)
	}
}

// TestAnEmptyTokenFileIsAnError: the same for a file that holds
// nothing. An empty file is a CONFIGURED source that is broken — not an
// absent one.
func TestAnEmptyTokenFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel-token")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CMESH_PANEL_TOKEN", "env-token")

	if _, _, err := resolvePanelTokenFrom("", path); err == nil {
		t.Fatal("an empty token file was accepted")
	}
}
