package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestCredListOnAMemberDeviceSaysWhereTheRosterLives is the regression
// test for the defect the external tester found.
//
// The ENOENT fix (a missing roster means an empty roster) is right for a
// FRESH ANCHOR — that is where a credential leaked before anyone used it
// gets turned off. But the roster is the DISCOVERY NODE's file, and a
// member device's state directory holds only its own identity
// (device.json + discovery.json).
//
// So on a member device the command used to print "no credentials
// recorded in <dir>/members.json" — pointing at a file that does not
// exist, about a network that has forty members. An operator reading
// that would look for a missing file instead of being told where the
// roster actually lives.
func TestCredListOnAMemberDeviceSaysWhereTheRosterLives(t *testing.T) {
	dir := t.TempDir()
	// A member device's state directory: its own identity and the
	// discovery config it saved when it joined.
	if err := os.WriteFile(filepath.Join(dir, identity.DeviceFileName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "discovery.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runCred([]string{"list", "--state", dir})
	if err == nil {
		t.Fatal("cred list on a member device must fail: there is no roster to list")
	}
	out := err.Error()
	if strings.Contains(out, "no credentials recorded") {
		t.Fatalf("the command claims there are no credentials: %s", out)
	}
	if !strings.Contains(out, "discovery node") && !strings.Contains(out, "anchor") {
		t.Fatalf("the command does not say where the roster lives: %s", out)
	}
}

// TestCredRevokeOnAMemberDeviceSaysWhereTheRosterLives: the same
// distinction on the write side. A member cannot revoke — the roster is
// the anchor's — and saying "no such file" sends the operator hunting
// for a file that was never supposed to be there.
func TestCredRevokeOnAMemberDeviceSaysWhereTheRosterLives(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, identity.DeviceFileName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "discovery.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runCred([]string{"revoke", "--state", dir, "--cred-id", "cred-x"})
	if err == nil {
		t.Fatal("cred revoke on a member device must fail: the roster is the anchor's")
	}
	out := err.Error()
	if strings.Contains(out, "no such file") {
		t.Fatalf("the command reports a missing file instead of where the roster lives: %s", out)
	}
	if !strings.Contains(out, "discovery node") && !strings.Contains(out, "anchor") {
		t.Fatalf("the command does not say where the roster lives: %s", out)
	}
}

// TestCredListOnAFreshAnchorSaysThereIsNothingYet: the OTHER case. A
// fresh anchor has no roster because no member has ever joined, and
// saying so is useful — it is where a leaked credential gets revoked.
func TestCredListOnAFreshAnchorSaysThereIsNothingYet(t *testing.T) {
	dir := t.TempDir()
	// A fresh ANCHOR has the account file — that is what makes it an
	// anchor. (A member device never has one.)
	if err := os.WriteFile(filepath.Join(dir, identity.AccountFileName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStderrFor(t, func() {
		if err := runCred([]string{"list", "--state", dir}); err != nil {
			t.Fatalf("cred list on a fresh anchor failed: %v", err)
		}
	})
	if strings.Contains(out, "no roster yet") == false && strings.Contains(out, "no credentials recorded") == false {
		t.Fatalf("the command does not say that there is nothing yet: %s", out)
	}
	// And it must NOT claim the roster belongs to a discovery node —
	// on a fresh anchor the command is the right one to run.
	if strings.Contains(out, "belongs to the discovery node") {
		t.Fatalf("a fresh anchor was told the roster is not its own: %s", out)
	}
}

// TestAnAnchorThatAlsoRanAJoinIsStillAnAnchor is the regression test
// for the narrow gap the external tester found by reading the code.
//
// The criterion for "is this the anchor's directory" was a
// discovery.json in it — and ANYBODY can run `join` against a state
// directory. On the anchor that is a natural debugging move, and the
// anchor's own directory would then carry a discovery.json and be
// mistaken for a MEMBER's — so the revoke would refuse to run on the
// very machine that owns the roster.
//
// The criterion is the ACCOUNT file: `cmesh account create` writes it,
// only the account owner does that, and a member device never has one.
func TestAnAnchorThatAlsoRanAJoinIsStillAnAnchor(t *testing.T) {
	dir := t.TempDir()
	// The anchor's own directory: it has the account file AND somebody
	// ran a join against it (a debugging move).
	if err := os.WriteFile(filepath.Join(dir, identity.AccountFileName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "discovery.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The revoke must WORK here — it is the anchor's own directory.
	err := runCred([]string{"revoke", "--state", dir, "--cred-id", "cred-x"})
	if err != nil {
		t.Fatalf("the revoke refused on the anchor's own directory: %v", err)
	}
}
