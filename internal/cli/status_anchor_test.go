package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ `cmesh status` MUST NOT TELL THE ANCHOR TO JOIN.
//
// The anchor IS the network: it holds account.json and members.json
// and serves the roster. But `status` requires discovery.json — a file
// only a MEMBER has — and its failure message says:
//
//   "this device has not joined a network yet (run cmesh join)"
//
// On the anchor that is simply false, and it tells the operator to
// run the one command that would make things worse. Measured on the
// authorized host: `cmesh status` exits 1 with exactly that message,
// while the anchor holds a roster with a member in it.
//
// The anchor-vs-member distinction is already written down in the
// credential code: the ANCHOR is the one that has account.json (see
// internal/cli/cred.go). This test pins that rule where it is used.

func TestTheStatusMessageDistinguishesAnAnchorFromAMember(t *testing.T) {
	dir := t.TempDir()

	// A node with a device identity and an ACCOUNT is an anchor: it is
	// the network. It has no discovery.json, and it must not be told
	// to join.
	writeFile(t, filepath.Join(dir, "device.json"), `{"device_id":"d1"}`)
	writeFile(t, filepath.Join(dir, "account.json"), `{"account_id":"a1"}`)

	msg := statusDiscoveryConfigError(dir)

	if strings.Contains(msg, "run cmesh join") {
		t.Fatalf("the message tells an ANCHOR to join: %q", msg)
	}
	if msg == "" {
		t.Fatal("the anchor case produced no message at all")
	}
	if !strings.Contains(msg, "anchor") && !strings.Contains(msg, "network") {
		t.Fatalf("the anchor message does not say what the node IS: %q", msg)
	}
}

// A node with a device identity and NO account is a member that has
// not joined yet — the original message was right for it.
func TestTheStatusMessageStillTellsAMemberToJoin(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "device.json"), `{"device_id":"d1"}`)

	msg := statusDiscoveryConfigError(dir)
	if !strings.Contains(msg, "run cmesh join") {
		t.Fatalf("the message does not tell an un-joined MEMBER to join: %q", msg)
	}
}

// The rule itself: the presence of account.json is what makes a node
// an anchor.
func TestTheAnchorIsTheNodeWithAnAccount(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "device.json"), `{"device_id":"d1"}`)
	if isAnchorState(dir) {
		t.Fatal("a node with no account.json was treated as an anchor")
	}
	writeFile(t, filepath.Join(dir, "account.json"), `{"account_id":"a1"}`)
	if !isAnchorState(dir) {
		t.Fatal("a node WITH account.json was not treated as an anchor")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
