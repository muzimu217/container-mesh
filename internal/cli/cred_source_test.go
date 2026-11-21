package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTheCredentialNeedNotBeOnTheCommandLine is the regression test for
// the defect the real-machine review found.
//
// The team credential IS the authority to join the network: whoever
// holds it can mint a device certificate under the account. It was
// passed with `--cred <string>`, and a command line is world-readable
// on Linux — every local user reads it through /proc/<pid>/cmdline.
// Measured on the real machine: `ps` printed the whole credential.
//
// So the credential had exactly one home, and it was the worst one.
// There was no environment variable, no file, no stdin.
//
// The file is the strongest source: systemd's LoadCredential hands the
// service a private copy that only its own process can read, so the
// credential ends up in neither the process list nor the environment.
func TestTheCredentialNeedNotBeOnTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cred")
	const secret = "cmesh1.credential.secret"
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The file wins over the environment, and the trailing newline
	// (systemd credential files carry one) is trimmed.
	t.Setenv("CMESH_CRED", "from-the-environment")
	got, err := resolveCredential("", path)
	if err != nil {
		t.Fatalf("resolveCredential: %v", err)
	}
	if got != secret {
		t.Fatalf("resolveCredential(file) = %q, want %q", got, secret)
	}

	// The environment is the LAST source: a deployment that has no file
	// and no flag but sets the variable must still work.
	got, err = resolveCredential("", "")
	if err != nil {
		t.Fatalf("resolveCredential: %v", err)
	}
	if got != "from-the-environment" {
		t.Fatalf("resolveCredential(env) = %q, want %q", got, "from-the-environment")
	}

	// The flag still works, and it still wins over the environment.
	got, err = resolveCredential("from-the-flag", "")
	if err != nil {
		t.Fatalf("resolveCredential: %v", err)
	}
	if got != "from-the-flag" {
		t.Fatalf("resolveCredential(flag) = %q, want %q", got, "from-the-flag")
	}

	// Nothing anywhere → empty, and the caller turns that into the
	// "pass an invite" path.
	t.Setenv("CMESH_CRED", "")
	got, err = resolveCredential("", "")
	if err != nil {
		t.Fatalf("resolveCredential: %v", err)
	}
	if got != "" {
		t.Fatalf("resolveCredential(nothing) = %q, want empty", got)
	}
}

// TestAMissingCredentialFileIsAnErrorNotASilentFallback: a file that
// cannot be read must say so. Falling back to the environment would
// hide the mistake and, worse, could join with the WRONG credential.
func TestAMissingCredentialFileIsAnErrorNotASilentFallback(t *testing.T) {
	t.Setenv("CMESH_CRED", "from-the-environment")
	_, err := resolveCredential("", filepath.Join(t.TempDir(), "not-there"))
	if err == nil {
		t.Fatal("resolveCredential with a missing file returned no error")
	}
}
