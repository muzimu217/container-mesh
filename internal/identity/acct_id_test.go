package identity

import (
	"path/filepath"
	"testing"
)

// TestAnEmptyRosterKeepsTheAccountIDOnDisk: the account id is what the
// anchor checks its roster against, so a roster written without it is
// refused — "members file reload refused: file is for account X, network
// is Y".
//
// A fresh anchor has no roster, so the list an operator builds with
// `cred revoke` has no account id of its own. It must take the one the
// file already records, or the very first revoke would make the anchor
// refuse its own roster.
func TestAnEmptyRosterKeepsTheAccountIDOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), MembersFileName)

	// The anchor's roster, with its account.
	live := NewMemberList("acct-1")
	if err := live.SaveMembers(path); err != nil {
		t.Fatal(err)
	}

	// A list that has no account of its own: this is what an operator
	// gets on a brand-new anchor, where no roster exists yet.
	fresh := NewMemberList("")
	if fresh.AccountID != "" {
		t.Fatalf("the fresh list already has the account id %q", fresh.AccountID)
	}
	if _, err := fresh.RevokeCredential("cred-x"); err != nil {
		t.Fatal(err)
	}
	if err := fresh.SaveMembersMerged(path); err != nil {
		t.Fatal(err)
	}

	after, err := LoadMembers(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.AccountID != "acct-1" {
		t.Fatalf("the roster's account id is %q, want acct-1: the anchor would refuse it", after.AccountID)
	}
	if !after.CredentialRevoked("cred-x") {
		t.Fatal("the revocation was lost")
	}
}
