package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestCredRevokeWorksOnAnAnchorWithNoMembersYet is the regression test
// for the defect the external tester found.
//
// "A leaked credential that nobody has used yet" is the case that
// matters MOST — a credential that was just handed out and pasted into
// the wrong group. And on a FRESH anchor (no member has ever joined, so
// there is no members.json at all) the revoke could not be done:
//
//	cmesh: …/members.json: open …/members.json: no such file or directory
//
// Because the delegation record is only written at ENROLL time, there is
// no record to remove — and the command refused to write the revocation
// list either, because it read the roster before it could write one.
//
// So the oldest failure mode of a credential — "it leaked before anyone
// used it" — was the one case the rotation could not handle.
func TestCredRevokeWorksOnAnAnchorWithNoMembersYet(t *testing.T) {
	dir := t.TempDir()
	// An anchor's directory has the ACCOUNT file — that is what makes it
	// an anchor's, and it is the criterion the commands use.
	if err := os.WriteFile(filepath.Join(dir, identity.AccountFileName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The roster does not exist: this is a fresh anchor.
	if _, err := os.Stat(filepath.Join(dir, identity.MembersFileName)); !os.IsNotExist(err) {
		t.Fatal("the roster already exists: the test is not testing a fresh anchor")
	}

	err := runCred([]string{"revoke", "--state", dir, "--cred-id", "cred-fresh"})
	if err != nil {
		t.Fatalf("the revoke failed on a fresh anchor: %v", err)
	}

	// The revocation must be ON DISK — that is the whole point.
	m, err := identity.LoadMembers(filepath.Join(dir, identity.MembersFileName))
	if err != nil {
		t.Fatalf("the roster was not written: %v", err)
	}
	if !m.CredentialRevoked("cred-fresh") {
		t.Fatal("the revocation was not recorded: a credential that leaked before anyone used it could still enrol")
	}
}

// TestCredListWorksOnAnAnchorWithNoMembersYet: the same fresh anchor,
// the read side.
func TestCredListWorksOnAnAnchorWithNoMembersYet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, identity.AccountFileName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCred([]string{"list", "--state", dir}); err != nil {
		t.Fatalf("cred list failed on a fresh anchor: %v", err)
	}
}

// TestCredListShowsTheRevokedCredentials: the operator must be able to
// SEE what has been revoked.
//
// The revocation list was written to the roster, but `cred list` only
// walked the delegation records — so after a rotation the operator had
// no CLI way to confirm what had been turned off, and had to read the
// JSON. A rotation you cannot inspect is a rotation you cannot trust.
func TestCredListShowsTheRevokedCredentials(t *testing.T) {
	dir := t.TempDir()
	m := identity.NewMemberList("acct")
	if _, err := m.RevokeCredential("cred-gone"); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveMembers(filepath.Join(dir, identity.MembersFileName)); err != nil {
		t.Fatal(err)
	}

	// The list command prints to stderr; the test captures it.
	out := captureStderrFor(t, func() {
		if err := runCred([]string{"list", "--state", dir}); err != nil {
			t.Fatalf("cred list failed: %v", err)
		}
	})
	if !strings.Contains(out, "cred-gone") {
		t.Fatalf("cred list does not mention the revoked credential:\n%s", out)
	}
}

// TestARosterWithAnEmptyRevocationListStillLoads: a hand-written roster
// is a normal thing for an operator to do (and the external tester did
// it, and broke it).
//
// "revoked_creds": [] is the JSON a person writes when they mean "no
// revocations" — but the field is a map, so the roster failed to load
// and EVERY cred command became unusable. The roster must survive it.
func TestARosterWithAnEmptyRevocationListStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, identity.MembersFileName)
	// A roster written by hand, with the revocation list as an empty ARRAY.
	blob := `{
  "account_id": "acct",
  "version": 1,
  "members": {},
  "delegations": {},
  "revoked_creds": []
}`
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := identity.LoadMembers(path)
	if err != nil {
		t.Fatalf("a hand-written roster with an empty revocation array did not load: %v", err)
	}
	if got := len(m.RevokedCredentials()); got != 0 {
		t.Fatalf("the roster reports %d revocations, want 0", got)
	}
}
