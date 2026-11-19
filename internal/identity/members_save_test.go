package identity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestASaveKeepsRevocationsRecordedInTheFile is the regression test for
// the defect the real machine found: a revocation that did not survive
// the next save was never a revocation.
//
// The anchor saves the roster from its IN-MEMORY list, and that list is
// only refreshed from the file every couple of seconds. So the window
// between "cmesh cred revoke wrote the file" and "the anchor reloaded
// it" is a window in which the anchor's live list does not know about
// the revocation — and any save in that window (an enrolment does one)
// writes the anchor's stale view back over the file, ERASING the
// revocation.
//
// The measured symptom on the real machine: two revoked ids were in the
// file, then one enrolment happened, and the file came back with no
// revocation list at all — both credentials could enrol again.
//
// The fix: a save must never drop a revocation that the FILE records.
// The file is the external authority (the operator edits it, and the CLI
// writes it), so the file's revocations are merged in on the way out.
//
// The plain SaveMembers keeps its old meaning — it saves exactly what the
// list holds — because some callers want that. The MERGED save is what
// the anchor uses.
func TestASaveKeepsRevocationsRecordedInTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), MembersFileName)

	// The operator revokes a credential: the CLI writes the file.
	live := NewMemberList("acct")
	if _, err := live.RevokeCredential("cred-revoked"); err != nil {
		t.Fatal(err)
	}
	if err := live.SaveMembers(path); err != nil {
		t.Fatal(err)
	}

	// The anchor has NOT reloaded yet: its live list is the old one,
	// with no revocations.
	stale := NewMemberList("acct")
	if got := len(stale.RevokedCredentials()); got != 0 {
		t.Fatalf("the anchor's stale list already knows %d revocations", got)
	}

	// An enrolment happens now, and the anchor saves its (stale) view.
	// The save MERGES the file's revocations, so the stale view cannot
	// erase them.
	if err := stale.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}

	// The revocation must still be there.
	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.CredentialRevoked("cred-revoked") {
		t.Fatal("the revocation was erased by a save: the credential could enrol again")
	}
}

// TestASaveKeepsRevocationsRecordedInTheFileWithTheMergeHelper is the
// same property, exercised through the helper the save path uses.
func TestASaveKeepsRevocationsRecordedInTheFileWithTheMergeHelper(t *testing.T) {
	path := filepath.Join(t.TempDir(), MembersFileName)

	file := NewMemberList("acct")
	if _, err := file.RevokeCredential("cred-a"); err != nil {
		t.Fatal(err)
	}
	if err := file.SaveMembers(path); err != nil {
		t.Fatal(err)
	}

	// The anchor's live list, plus a revocation of its own.
	live := NewMemberList("acct")
	if _, err := live.RevokeCredential("cred-b"); err != nil {
		t.Fatal(err)
	}

	// The save merges the FILE's revocations in, so neither is lost.
	if err := live.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}
	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.CredentialRevoked("cred-a") {
		t.Fatal("the file's revocation was erased by the save")
	}
	if !after.CredentialRevoked("cred-b") {
		t.Fatal("the live list's own revocation was erased by the save")
	}
}

// TestTheMergeNeverUnrevokes: the merge only ever ADDS revocations. A
// revocation is permanent (there is no "un-revoke" command), so a merge
// must not be able to resurrect an id by carrying an older timestamp.
func TestTheMergeNeverUnrevokes(t *testing.T) {
	path := filepath.Join(t.TempDir(), MembersFileName)

	file := NewMemberList("acct")
	if _, err := file.RevokeCredential("cred-x"); err != nil {
		t.Fatal(err)
	}
	if err := file.SaveMembers(path); err != nil {
		t.Fatal(err)
	}

	live := NewMemberList("acct")
	// The live list claims the same id was revoked EARLIER — the merge
	// must keep the file's (later) timestamp, and in any case the id
	// stays revoked.
	if live.RevokedCreds == nil {
		live.RevokedCreds = make(map[string]time.Time)
	}
	live.RevokedCreds["cred-x"] = time.Now().UTC().Add(-time.Hour)
	if err := live.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}
	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.CredentialRevoked("cred-x") {
		t.Fatal("the merge dropped a revocation")
	}
}

// TestTheRosterInheritsTheDirectorysOwner: the roster must stay
// readable by the service that serves it, whoever wrote it.
//
// The roster is written by the anchor's watcher (as the service user) AND
// by the operator's CLI (often as root). A root-written 0600 file belongs
// to root, and the anchor — running as its own unprivileged user — then
// cannot read it: its reload fails with "permission denied", it keeps its
// stale in-memory roster, and the next save ERASES the revocation the
// operator had just recorded.
//
// Measured on the real machine: `cmesh cred revoke` (root) wrote the
// roster, the anchor could not read it, and the next enrolment wiped the
// revocation — so a revoked credential could enrol again.
//
// The fix: the new file inherits the DIRECTORY's ownership. The directory
// belongs to the service user, so the roster stays readable by the
// service no matter which process wrote it.
func TestTheRosterInheritsTheDirectorysOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFileName)

	m := NewMemberList("acct")
	if err := m.SaveMembers(path); err != nil {
		t.Fatal(err)
	}

	dirStat, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	fileStat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dirUID, dirGID, ok := ownerOf(dirStat)
	if !ok {
		t.Skip("this platform does not expose file ownership")
	}
	fileUID, fileGID, ok := ownerOf(fileStat)
	if !ok {
		t.Skip("this platform does not expose file ownership")
	}
	if fileUID != dirUID || fileGID != dirGID {
		t.Fatalf("the roster is owned by %d:%d but the directory is %d:%d: "+
			"the service that serves it may not be able to read the roster",
			fileUID, fileGID, dirUID, dirGID)
	}
	// And the mode stays tight: the roster is shared with the one account
	// that has to read it, not published.
	if perm := fileStat.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the roster's mode is %o, want 0600", perm)
	}
}
