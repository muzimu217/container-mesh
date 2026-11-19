package cli

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/relay"
)

// TestTheMemberRenewsItsCertificateBeforeItLapses is the regression test
// for the second half of D77: the owner ruled membership PERSISTENT
// ("既然已经入网，那就是持续性入网"), and the anchor already accepted a
// renewal (MemberList.Add refreshes a certificate for the same device key
// and account). The missing half was the client: nothing ever asked for a
// fresh certificate, so a member's certificate expired and the device fell
// out of the network SILENTLY — the real-machine "device mysteriously
// disappeared".
//
// The test runs the REAL enrollment endpoint in process, joins a device
// with a short credential, and lets the renewal loop run with a
// compressed interval. The roster's certificate for the device must move
// forward — the same device, refreshed, not a second member — and the
// loop must STOP once the credential itself has expired (the credential
// is the authority: past it, nothing may extend the membership).
func TestTheMemberRenewsItsCertificateBeforeItLapses(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	srv := relay.New(relay.DefaultConfig())
	members := identity.NewMemberList(acc.ID())
	srv.EnableDiscovery(members)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.StopDiscovery()

	// A SHORT credential: the whole renewal cycle fits in the test's
	// running time.
	const credTTL = 8 * time.Second
	cred, err := credential.Create(acc, []string{"mesh.example:1"}, credTTL, 5, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	credStr, err := credential.Encode(cred)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := credential.Decode(credStr)
	if err != nil {
		t.Fatal(err)
	}

	membersPath := filepath.Join(t.TempDir(), "members.json")
	if err := members.SaveMembers(membersPath); err != nil {
		t.Fatal(err)
	}
	installEnrollHandler(srv, membersPath, members, acc.PublicKey())

	dir := t.TempDir()
	dev, err := ensureDeviceIdentity(dir, "test-pass")
	if err != nil {
		t.Fatal(err)
	}

	// The transport is injected: the enrollment request goes through the
	// same policy-carrying transport the real client uses.
	transport := testEnrollmentTransport(t, ts.URL)

	// First enrollment: the join itself.
	if err := enrollWithCredentialUsing(context.Background(), parsed, dev, transport); err != nil {
		t.Fatalf("the first enrollment failed: %v", err)
	}
	first, ok := members.Get(dev.ID())
	if !ok {
		t.Fatal("the device is not in the roster after the first enrollment")
	}

	// The renewal loop, with the interval compressed to a second.
	oldHook := certRenewIntervalHook
	certRenewIntervalHook = func(time.Duration) time.Duration { return 1200 * time.Millisecond }
	defer func() { certRenewIntervalHook = oldHook }()

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		certRenewLoop(ctx, parsed, dev, transport)
		close(done)
	}()

	// Wait for at least one renewal: the roster certificate's expiry must
	// move forward (each renewal mints a certificate valid until the
	// credential expires, which is later than the first one only if time
	// has passed — so the meaningful assertion is that the certificate was
	// RE-SET, i.e. its IssuedAt moved forward and the member stayed the
	// same single member).
	deadline := time.Now().Add(8 * time.Second)
	renewed := false
	for time.Now().Before(deadline) {
		mm, ok := members.Get(dev.ID())
		if !ok {
			t.Fatal("the device vanished from the roster during renewal")
		}
		if mm.Cert.IssuedAt.After(first.Cert.IssuedAt) {
			renewed = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !renewed {
		t.Fatal("the certificate was never renewed: the member would have silently fallen out when it lapsed")
	}
	// Still exactly ONE member: a renewal must not consume a quota slot.
	if got := members.Count(); got != 1 {
		t.Fatalf("the roster holds %d members after a renewal, want 1 (a renewal is a refresh, not a second member)", got)
	}

	// The loop must STOP once the credential has expired: the credential
	// is the authority, and past it nothing may extend the membership.
	// Wait out the credential (8s from creation) plus margin, then check
	// the loop has exited and no enrollment is still being attempted.
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the renewal loop did not end after the credential expired")
	}
	if got := members.Count(); got != 1 {
		t.Fatalf("the roster holds %d members after the credential expired, want 1", got)
	}
}
