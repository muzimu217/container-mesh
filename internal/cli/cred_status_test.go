package cli

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestARevokedCredentialSaysRevokedEvenAfterItExpires is the
// regression test for the observability gap an external tester found.
//
// A credential that was BOTH revoked AND expired was reported as
// "expired" — and the revocation, which is the far more important fact
// (it says the owner turned this key off), disappeared from the
// operator's view as the clock moved on. An operator grepping the logs
// for "revoked" would find nothing.
//
// The order is the point: a REVOKED credential must be reported as
// revoked whether or not it has also expired.
func TestARevokedCredentialSaysRevokedEvenAfterItExpires(t *testing.T) {
	acc, _, _, _ := credFixture(t)
	now := time.Now().UTC()

	// A REAL credential, issued in the past so it has EXPIRED as well —
	// the state the tester hit.
	cred, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 5, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// The credential is revoked.
	members := identity.NewMemberList(acc.ID())
	if _, err := members.RevokeCredential(cred.Public.ID); err != nil {
		t.Fatal(err)
	}

	res := credentialStatus(members, acc.PublicKey(), &cred.Public, now)
	if res.OK {
		t.Fatal("a revoked credential was reported as usable")
	}
	if !strings.Contains(res.Message, "revoked") {
		t.Fatalf("the answer is %q, want it to name the revocation: an operator "+
			"grepping for \"revoked\" must still find it after the clock moves on",
			res.Message)
	}
}

// TestAnExpiredButNotRevokedCredentialSaysExpired is the other half:
// the status query must not cry revocation when there is none.
func TestAnExpiredButNotRevokedCredentialSaysExpired(t *testing.T) {
	acc, _, _, _ := credFixture(t)
	now := time.Now().UTC()

	cred, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 5, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())

	res := credentialStatus(members, acc.PublicKey(), &cred.Public, now)
	if res.OK {
		t.Fatal("an expired credential was reported as usable")
	}
	if !strings.Contains(res.Message, "expired") {
		t.Fatalf("the answer is %q, want it to name the expiry", res.Message)
	}
	if strings.Contains(res.Message, "revoked") {
		t.Fatalf("the answer is %q, but nothing was revoked", res.Message)
	}
}

// TestAValidCredentialIsStillValid is the control.
func TestAValidCredentialIsStillValid(t *testing.T) {
	acc, _, _, _ := credFixture(t)
	now := time.Now().UTC()

	cred, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())

	res := credentialStatus(members, acc.PublicKey(), &cred.Public, now)
	if !res.OK {
		t.Fatalf("a valid credential was refused: %s", res.Message)
	}
}

// TestTheJoinerAsksTheAnchorWhyItCannotEnrol is the client half.
//
// The joiner cannot know about revocations itself, so an expired
// credential must be ASKED about rather than declared expired on the
// spot. The answer the anchor gives is the one the operator sees.
func TestTheJoinerAsksTheAnchorWhyItCannotEnrol(t *testing.T) {
	acc, _, _, _ := credFixture(t)
	now := time.Now().UTC()

	cred, err := credential.Create(acc, []string{"mesh.example:7000"}, time.Hour, 5, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	delegID := cred.Public.ID

	// The anchor's answer: this credential is revoked.
	asked := false
	got := askCredentialStatus(context.Background(), cred, enrollmentRoundTripper(func(r *http.Request) (*http.Response, error) {
		asked = true
		body := `{"ok":false,"message":"credential ` + delegID + ` has been revoked; ask the owner for a new one"}`
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	}))
	if !asked {
		t.Fatal("the joiner never asked the anchor")
	}
	if !strings.Contains(got, "revoked") {
		t.Fatalf("the joiner got %q, want the anchor's revocation verdict", got)
	}
}

// credFixture makes an account, a delegation key pair and a delegation id —
// the shape of a team credential.
func credFixture(t *testing.T) (*identity.Account, ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return acc, pub, priv, "KxQDPvIbClCE"
}
