// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// Tests for credential PRE-revocation: turning a credential off BEFORE any
// device has enrolled with it.
//
// The gap this closes (D77's last one): the delegation record — the anchor's
// only knowledge that a credential exists — is written at ENROLL time
// (enrollDevice), so `cred revoke` had nothing to remove for a credential
// nobody had used yet, and a leaked-but-unused string stayed live until its
// expiry (a YEAR by default). The fix is an anchor-side revocation list in
// members.json: an id on that list is refused at enroll, whatever the
// credential's signature or expiry says.
//
// The scenarios below stage the roster file by hand (raw JSON) where the
// production flow would have written it, because the file — not a struct
// constructor — is what the anchor actually reads.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// writeRosterRaw writes members.json directly, with the given revoked-ids
// set. This is the file shape the anchor hot-reloads; writing it raw keeps
// these tests honest about the on-disk contract (field name, value shape)
// instead of testing a struct against itself.
func writeRosterRaw(t *testing.T, path string, account identity.DeviceID, revoked map[string]time.Time) {
	t.Helper()
	entries := make(map[string]string, len(revoked))
	for id, at := range revoked {
		entries[id] = at.UTC().Format(time.RFC3339Nano)
	}
	blob, err := json.MarshalIndent(map[string]any{
		"account_id":    string(account),
		"version":       3,
		"members":       map[string]any{},
		"revoked_creds": entries,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

// revokedFromDisk reads members.json's revoked_creds set back as a plain
// map, so a test can assert persistence without depending on the identity
// API that fills it.
func revokedFromDisk(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Revoked map[string]json.RawMessage `json:"revoked_creds"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: not a member list: %v", path, err)
	}
	out := make(map[string]bool, len(doc.Revoked))
	for id := range doc.Revoked {
		out[id] = true
	}
	return out
}

// captureStderrFor runs fn with os.Stderr swapped for a pipe and returns
// what the command printed, so a test can assert the operator was TOLD what
// happened, not just that the exit code was zero.
func captureStderrFor(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// enrollRequestFor mints a fresh device certificate under an existing
// credential — the request a SECOND device would send with the same string.
func enrollRequestFor(t *testing.T, cred *credential.Credential, now time.Time) discovery.EnrollRequest {
	t.Helper()
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := cred.MintCert(dev.PublicKey(), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := cred.PublicPart()
	if err != nil {
		t.Fatal(err)
	}
	return discovery.EnrollRequest{Credential: pub, Cert: *cert}
}

// TestEnrollRefusesAPreRevokedCredential is the gate for the headline gap:
// a credential that leaked before anyone used it must be dead the moment
// its id is on the anchor's revocation list — signature valid, quota free,
// expiry far away, and STILL refused, with a message that tells the joiner
// what to do.
//
// Red against the old implementation: the roster file said "revoked" and
// the anchor enrolled the device anyway, because the file's revocation set
// was dropped on load and nothing consulted it.
func TestEnrollRefusesAPreRevokedCredential(t *testing.T) {
	acc, cred, req, _ := enrollFixture(t, 5, 24*time.Hour)
	dir := t.TempDir()
	path := filepath.Join(dir, identity.MembersFileName)
	writeRosterRaw(t, path, acc.ID(), map[string]time.Time{cred.Public.ID: time.Now().UTC()})

	// What the anchor holds after a hot reload (or a restart) is whatever
	// LoadMembers kept — the revocation must survive that trip.
	m, err := identity.LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	res := enrollDevice(m, acc.PublicKey(), req, time.Now().UTC())
	if res.OK {
		t.Fatal("a credential on the anchor's revocation list was accepted: pre-revocation does not work")
	}
	if !strings.Contains(res.Message, cred.Public.ID) ||
		!strings.Contains(res.Message, "has been revoked") ||
		!strings.Contains(strings.ToLower(res.Message), "ask the owner") {
		t.Fatalf("the refusal does not tell the joiner what happened and what to do: %s", res.Message)
	}
}

// TestEnrollRefusesWhenTheDelegationAndTheRevocationCoexist: the id is BOTH
// on the roster as a delegation record and on the revocation list (a revoke
// raced an enroll, or the file was hand-edited). The revocation must win —
// a stale delegation record must not out-vote the owner's decision.
func TestEnrollRefusesWhenTheDelegationAndTheRevocationCoexist(t *testing.T) {
	acc, cred, req, _ := enrollFixture(t, 5, 24*time.Hour)
	dir := t.TempDir()
	path := filepath.Join(dir, identity.MembersFileName)
	delBlob, err := json.Marshal(cred.Public)
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte(fmt.Sprintf(`{"account_id":%q,"version":5,"members":{},"delegations":{%q:%s},"revoked_creds":{%q:%q}}`,
		string(acc.ID()), cred.Public.ID, delBlob, cred.Public.ID, time.Now().UTC().Format(time.RFC3339Nano)))
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := identity.LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Delegations[cred.Public.ID]; !ok {
		t.Fatal("setup: the delegation record did not load")
	}
	if res := enrollDevice(m, acc.PublicKey(), req, time.Now().UTC()); res.OK {
		t.Fatal("a credential whose id is on the revocation list enrolled despite the delegation record")
	}
}

// TestCredRevokeOfAnEnrolledCredentialBlocksRejoin is the rotation gate,
// end to end through the CLI: the credential's delegation is on the roster
// (a device joined with it), `cred revoke` kicks that device — and the SAME
// credential string must not bring the device (or a new one) back. The
// account signature on the old credential is still valid, so without a
// revocation-list entry the enroll path would happily re-install the
// delegation: the rotation would only pause the holder, not stop them.
//
// Red against the old implementation: the rejoin succeeded.
func TestCredRevokeOfAnEnrolledCredentialBlocksRejoin(t *testing.T) {
	acc, cred, req, m := enrollFixture(t, 5, 24*time.Hour)
	dir := t.TempDir()
	path := filepath.Join(dir, identity.MembersFileName)

	// First device joins: the delegation lands on the roster and is saved.
	if res := enrollDevice(m, acc.PublicKey(), req, time.Now().UTC()); !res.OK {
		t.Fatalf("the first device was refused: %s", res.Message)
	}
	if err := m.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	firstID := req.Cert.DeviceID

	// The owner turns the credential.
	if err := runCred([]string{"revoke", "--state", dir, "--cred-id", cred.Public.ID}); err != nil {
		t.Fatalf("revoking an enrolled credential failed: %v", err)
	}
	reloaded, err := identity.LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Contains(firstID) {
		t.Fatalf("member %s survived the revoke: the rotation did not take it out", firstID)
	}

	// The old string tries to come back.
	rejoin := enrollRequestFor(t, cred, time.Now().UTC())
	if res := enrollDevice(reloaded, acc.PublicKey(), rejoin, time.Now().UTC()); res.OK {
		t.Fatal("the revoked credential enrolled a device again: rotation without a revocation list is a revolving door")
	}
}

// TestARevokedCredentialCannotResurrect pins the guarantee explicitly: a
// revoked id stays dead across enroll attempts and across anchor restarts
// (fresh LoadMembers from the file). There is no un-revoke command by
// design — only a permanent refusal.
func TestARevokedCredentialCannotResurrect(t *testing.T) {
	acc, cred, req, m := enrollFixture(t, 5, 24*time.Hour)
	dir := t.TempDir()
	path := filepath.Join(dir, identity.MembersFileName)
	if res := enrollDevice(m, acc.PublicKey(), req, time.Now().UTC()); !res.OK {
		t.Fatalf("setup: the first device was refused: %s", res.Message)
	}
	if err := m.SaveMembers(path); err != nil {
		t.Fatal(err)
	}
	if err := runCred([]string{"revoke", "--state", dir, "--cred-id", cred.Public.ID}); err != nil {
		t.Fatalf("the revoke failed: %v", err)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		// Every attempt starts from the file, the way the anchor does after
		// a restart or a hot reload.
		reloaded, err := identity.LoadMembers(path)
		if err != nil {
			t.Fatal(err)
		}
		if res := enrollDevice(reloaded, acc.PublicKey(), req, time.Now().UTC()); res.OK {
			t.Fatalf("attempt %d: the revoked credential enrolled again — resurrection", attempt)
		}
	}
	// And nothing the attempts did put the delegation back on the roster.
	reloaded, err := identity.LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Delegations[cred.Public.ID]; ok {
		t.Fatal("the revoked credential's delegation record is back on the roster")
	}
	if _, ok := reloaded.DelegationFor(cred.Public.DelegatorPub); ok {
		t.Fatal("a delegation for the revoked credential's key is back on the roster")
	}
}

// TestCredRevokeAnUnusedCredentialRecordsTheRevocation is the CLI gate for
// the NEW state: no delegation on the roster (nobody has enrolled with the
// credential) — `cred revoke` must still SUCCEED, put the id on the
// persisted revocation list, and SAY that it did.
//
// Red against the old implementation: the command exited 1 with "no
// delegation", and a leaked-but-unused credential stayed live for its whole
// TTL.
func TestCredRevokeAnUnusedCredentialRecordsTheRevocation(t *testing.T) {
	_, cred, _, _ := enrollFixture(t, 5, 24*time.Hour)
	dir := t.TempDir()
	path := filepath.Join(dir, identity.MembersFileName)
	// A roster that has never seen this credential: no delegation for it.
	if err := identity.NewMemberList("acct").SaveMembers(path); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStderrFor(t, func() {
		err = runCred([]string{"revoke", "--state", dir, "--cred-id", cred.Public.ID})
	})
	if err != nil {
		t.Fatalf("revoking a never-enrolled credential failed: %v", err)
	}
	if !strings.Contains(out, "revocation list") {
		t.Fatalf("the output does not say the id went onto the revocation list:\n%s", out)
	}

	// The file — what the anchor hot-reloads — carries the id.
	got := revokedFromDisk(t, path)
	if !got[cred.Public.ID] {
		t.Fatalf("credential %s is not on the persisted revocation list: %v", cred.Public.ID, got)
	}
}

// TestCredRevokeOfAnUnenrolledIdSucceedsAndIsRecorded: an id with no
// delegation on the roster but ALSO no credential behind it (a typo) now
// succeeds and lands on the revocation list. That is the price of
// pre-revocation, and it is a safe one: the anchor only learns a
// credential's id when a device FIRST enrolls with it, so "leaked but
// unused" and "typo" are indistinguishable there — and listing a typo'd id
// harms nothing (no credential will ever present it), while hard-failing
// would have left the genuinely leaked case unrevokable. The output still
// says exactly what happened, so the owner is not left believing devices
// were kicked when none were.
func TestCredRevokeOfAnUnenrolledIdSucceedsAndIsRecorded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, identity.MembersFileName)
	credRevokeRoster(t, dir) // a roster with OTHER credentials on it

	if err := runCred([]string{"revoke", "--state", dir, "--cred-id", "typo-id"}); err != nil {
		t.Fatalf("revoking an id with no delegation on the roster failed: %v", err)
	}
	got := revokedFromDisk(t, path)
	if !got["typo-id"] {
		t.Fatalf("the id was not recorded on the revocation list: %v", got)
	}
	// The other credentials on the roster are untouched.
	m, err := identity.LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Delegations["cred-test"]; !ok {
		t.Fatal("the unrelated credential's delegation was removed by an unrelated revoke")
	}
}
