// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// The --timeout semantics of `cmesh join`, as the real machine proved them
// wrong.
//
// The flag says "how long to wait for approval before giving up (0 =
// forever)". The implementation did not implement that: it wrapped the
// deadline onto the SIGNAL context, and the signal context is what the whole
// member process runs on. So the timeout was the PROCESS deadline, not the
// approval deadline.
//
// The consequence on a real machine: two clients joined with
// `join --timeout 5m`; about five minutes in, the processes exited THEMSELVES
// and broadcast "left the network (LEAVE broadcast sent)". The members went
// offline at the anchor, every later relay request answered "this node cannot
// reach <target> right now", and the anchor could not reach the members any
// more. A member that was approved and working was torn down by a flag that
// was documented as bounding only the WAIT.
//
// The rule these tests pin:
//
//   - --timeout bounds the APPROVAL WAIT and nothing else.
//   - Once the approval lands, the join keeps running until it is
//     interrupted (SIGINT/SIGTERM) — it is a resident member process.
//   - If the approval never lands, the join gives up at the deadline with an
//     error the operator can act on, and leaves no half-member state behind.
//
// The joins run IN PROCESS, against a real discovery node over a real socket.
// The member's lifetime comes from joinLifetimeHook (a test-only seam), so a
// test can end a resident join without a signal that would take every other
// join in the test binary down with it.

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/relay"
)

// joinApprovalFixture is a running discovery node plus the accounts needed
// to approve a joining device.
//
// It is built from the REAL pieces — a real relay with the discovery
// endpoints enabled, a real account, a real invite, the real approval
// watcher — so the test exercises the same approval path the real machine
// did.
type joinApprovalFixture struct {
	t *testing.T

	joinerDir string
	pass      string

	// account is the account the network belongs to.
	account *identity.Account
	// invite is the join string the joiner consumes.
	invite string
	// joinerPub is the joiner's public key, base64 — what the account owner
	// authorizes.
	joinerPub string
	// joinerID is the joiner's device ID.
	joinerID identity.DeviceID

	// srv is the discovery node.
	srv *relay.Server
	// members is the live member list the node enforces.
	members *identity.MemberList
	// membersPath is where the roster is persisted.
	membersPath string
	// certsDir is where the approved certificate is dropped.
	certsDir string
}

// newJoinApprovalFixture starts a discovery node and prepares a joiner. The
// joiner is NOT approved yet: approval is a separate step the test performs
// when it wants it.
func newJoinApprovalFixture(t *testing.T) *joinApprovalFixture {
	t.Helper()

	f := &joinApprovalFixture{
		t:           t,
		joinerDir:   filepath.Join(t.TempDir(), "joiner"),
		pass:        "test-pass-phrase",
		membersPath: filepath.Join(t.TempDir(), "members.json"),
		certsDir:    filepath.Join(t.TempDir(), "certs"),
	}

	// The account: the network's identity.
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	f.account = acc

	// The discovery node, listening on a real port.
	f.srv = relay.New(relay.DefaultConfig())
	f.members = identity.NewMemberList(acc.ID())
	f.srv.EnableDiscovery(f.members)
	discAddr := freePort(t)
	serveDiscoveryInProcess(t, f.srv, discAddr)
	t.Cleanup(f.srv.StopDiscovery)

	// The roster file and the certs directory the owner drops the approved
	// certificate into.
	if err := os.MkdirAll(f.certsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.members.SaveMembers(f.membersPath); err != nil {
		t.Fatal(err)
	}
	// The node must learn about later changes the same way a live
	// serve-discovery does: the watcher reloads the file and swaps the list.
	f.srv.SetMeshMembers(f.members)
	ctx, cancel := context.WithCancel(context.Background())
	go watchApprovals(ctx, f.membersPath, f.certsDir, f.members, f.srv, acc.PublicKey())
	t.Cleanup(cancel)

	// The joiner device: its own identity, no certificate.
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	f.joinerID = dev.ID()
	f.joinerPub = base64.StdEncoding.EncodeToString(dev.PublicKey())
	if err := dev.Save(identity.IdentityPath(f.joinerDir, identity.DeviceFileName), f.pass); err != nil {
		t.Fatal(err)
	}

	// The invite: the account, the account key and the node's address. The
	// address is a LOGICAL name that the test transport routes to the
	// in-process listener — the production host policy must not be
	// weakened to make a test pass.
	oldTransport := discoveryClientTransportHook
	discoveryClientTransportHook = meshDiscoveryTestTransport(t, "http://"+discAddr)
	t.Cleanup(func() { discoveryClientTransportHook = oldTransport })
	logicalAddr := "mesh.example:" + strings.TrimPrefix(discAddr, "127.0.0.1:")

	inv, err := discovery.EncodeInvite(discovery.Invite{
		AccountID:      acc.ID(),
		AccountPub:     acc.PublicKey(),
		DiscoveryAddrs: []string{logicalAddr},
		IssuedAt:       time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.invite = inv

	return f
}

// serveDiscoveryInProcess starts the relay's HTTP handler on a real port.
func serveDiscoveryInProcess(t *testing.T, srv *relay.Server, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	hs := srv.HTTPServer()
	go func() { _ = hs.Serve(ln) }()
	// Wait for the listener to accept.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the discovery node never accepted a connection")
}

// approveCert signs the joiner's key into the account and returns the
// certificate the account owner hands to the discovery host.
func (f *joinApprovalFixture) approveCert() identity.DeviceCert {
	f.t.Helper()
	pub, err := base64.StdEncoding.DecodeString(f.joinerPub)
	if err != nil {
		f.t.Fatal(err)
	}
	cert, err := f.account.IssueCertForPub(pub, time.Hour)
	if err != nil {
		f.t.Fatal(err)
	}
	return *cert
}

// approveFile performs the REAL owner-side approval: the signed certificate
// is dropped into the discovery host's certs directory, and the watcher
// adopts it within a couple of seconds and pushes the new view.
func (f *joinApprovalFixture) approveFile() {
	f.t.Helper()
	cert := f.approveCert()
	path := filepath.Join(f.certsDir, string(cert.DeviceID)+identity.CertSuffix)
	if err := identity.SaveCert(&cert, path); err != nil {
		f.t.Fatal(err)
	}
}

// addToRoster puts a certificate into the live roster, persists the roster
// and pushes the new view to the members.
func (f *joinApprovalFixture) addToRoster(cert identity.DeviceCert) {
	f.t.Helper()
	if err := f.members.Add(cert); err != nil {
		f.t.Fatal(err)
	}
	// The roster file must carry the member too, or the watcher would
	// reload the file and drop it from the live list again.
	if err := f.members.SaveMembers(f.membersPath); err != nil {
		f.t.Fatal(err)
	}
	f.srv.SetMeshMembers(f.members)
}

// approveRosterOnly approves the device at the node WITHOUT leaving a
// certificate in the certs directory: the watcher must not be able to
// re-adopt the device after a test revokes it.
func (f *joinApprovalFixture) approveRosterOnly() {
	f.t.Helper()
	f.addToRoster(f.approveCert())
}

// approveNow performs the approval AND makes it effective at the node right
// away: the roster carries the member and the new view is pushed.
//
// It is the same two steps the watcher performs, only without the file
// watcher's two-second tick — a test that must see the approval inside a
// ONE-SECOND window cannot wait for a poll.
func (f *joinApprovalFixture) approveNow() {
	f.t.Helper()
	cert := f.approveCert()
	f.addToRoster(cert)
	// The certificate also goes into the certs directory: that is where the
	// account owner puts it, and the watcher adopts the same (identical)
	// certificate without changing anything.
	path := filepath.Join(f.certsDir, string(cert.DeviceID)+identity.CertSuffix)
	if err := identity.SaveCert(&cert, path); err != nil {
		f.t.Fatal(err)
	}
}

// revoke removes the joiner from the roster — the owner's "revoke this
// device" action — and pushes the new view.
func (f *joinApprovalFixture) revoke() {
	f.t.Helper()
	if err := f.members.Remove(f.joinerID); err != nil {
		f.t.Fatal(err)
	}
	if err := f.members.SaveMembers(f.membersPath); err != nil {
		f.t.Fatal(err)
	}
	f.srv.SetMeshMembers(f.members)
}

// waitMember blocks until the node's roster carries the joiner.
func (f *joinApprovalFixture) waitMember(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, ok := f.members.Get(f.joinerID); ok {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// streaming reports whether the node still holds a members-stream connection
// for the device.
//
// It is the precise liveness signal: the member's roster entry survives a
// departure for a long time (the certificate stays valid), but its STREAM
// dies with the process. So "the stream is gone" means "the member left the
// network" — exactly what a premature timeout did.
func (f *joinApprovalFixture) streaming(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f.srv.DiscoNode().StreamCount(f.joinerID) > 0 {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// runJoin starts `cmesh join` in the background and returns a handle.
//
// The join's lifetime is the returned cancel function: an APPROVED join is
// resident and runs until it is interrupted, so a test has to be able to end
// it. The timeout flag is passed through as given.
func runJoinInProcess(t *testing.T, f *joinApprovalFixture, extra ...string) (cancel func(), exited <-chan error) {
	t.Helper()

	oldHook := joinLifetimeHook
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	joinLifetimeHook = func() (context.Context, context.CancelFunc) { return lifeCtx, lifeCancel }
	t.Cleanup(func() {
		joinLifetimeHook = oldHook
		lifeCancel()
	})

	args := []string{"join", "--state", f.joinerDir, "--device-passphrase", f.pass}
	args = append(args, extra...)
	args = append(args, f.invite)

	done := make(chan error, 1)
	go func() { done <- Run(args) }()
	return lifeCancel, done
}

// TestJoinKeepsRunningAfterApproval pins the defect.
//
// The device is ALREADY a member when the join starts (the operator approved
// it beforehand — the ordinary "re-join" case), and the timeout is set to
// ONE SECOND, far shorter than any sane approval wait.
//
// The member must NOT be taken down by that timeout. Before the fix the
// timeout was the process deadline: the join exited by itself about a second
// after it started, broadcast a LEAVE, and the member went offline at the
// anchor.
func TestJoinKeepsRunningAfterApproval(t *testing.T) {
	f := newJoinApprovalFixture(t)

	// Approved BEFORE the join: the roster carries the device, so the
	// client's first announce already succeeds and the membership lands
	// within milliseconds — inside the one-second window.
	f.approveNow()

	cancel, exited := runJoinInProcess(t, f, "--endpoint", "203.0.113.50:7000", "--timeout", "1s")
	defer cancel()

	// The join must become a member: the approval was already in place.
	if !f.streaming(30 * time.Second) {
		t.Fatal("the approved device never attached to the network")
	}

	// THE ASSERTION: the timeout was ONE SECOND and the approval is in.
	// The member must still be running well past that deadline. Before the
	// fix the join exited by itself right after it — and broadcast a LEAVE,
	// so the member went offline at the anchor.
	select {
	case err := <-exited:
		t.Fatalf("the join exited on its own (%v) after the approval — the timeout "+
			"must bound the approval wait, not the member's lifetime", err)
	case <-time.After(15 * time.Second):
	}

	// And the member must still be ONLINE at the anchor: the join is a
	// resident member process, and its presence is what other members see.
	if !f.streaming(20 * time.Second) {
		t.Fatal("the approved member is not online at the anchor any more")
	}
}

// TestJoinApprovalLandsWhileWaiting pins the same rule for the other order:
// the join is already waiting when the approval lands.
//
// The timeout is short compared with the wait for a human to approve, and
// the approval lands well before the deadline. From then on the deadline is
// DISARMED: the member stays.
func TestJoinApprovalLandsWhileWaiting(t *testing.T) {
	f := newJoinApprovalFixture(t)

	cancel, exited := runJoinInProcess(t, f, "--endpoint", "203.0.113.50:7000", "--timeout", "15s")
	defer cancel()

	// The owner approves the device while the join is waiting.
	time.Sleep(500 * time.Millisecond)
	f.approveFile()
	if !f.waitMember(30 * time.Second) {
		t.Fatal("the approved device never reached the roster")
	}

	// The join must pick the approval up (the client's own polling decides
	// when it SEES it) and then keep running past the deadline.
	select {
	case err := <-exited:
		t.Fatalf("the join exited on its own (%v) although it was approved — "+
			"the timeout must bound the approval wait, not the member's lifetime", err)
	case <-time.After(25 * time.Second):
	}

	if !f.streaming(20 * time.Second) {
		t.Fatal("the approved member is not online at the anchor any more")
	}
}

// TestJoinGivesUpWhenApprovalNeverLands pins the other half of the
// contract: with no approval, the join MUST give up at the deadline — and
// say something the operator can act on.
//
// The timeout bounds the wait, so a join that waits forever is a join that
// hangs a shell prompt.
func TestJoinGivesUpWhenApprovalNeverLands(t *testing.T) {
	f := newJoinApprovalFixture(t)

	cancel, exited := runJoinInProcess(t, f, "--timeout", "3s")
	defer cancel()

	select {
	case err := <-exited:
		if err == nil {
			t.Fatal("the join exited 0 although it was never approved")
		}
		var exit ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("the join gave up with %v, want an exit error", err)
		}
		if exit.Code != 1 {
			t.Fatalf("the join gave up with exit code %d, want 1", exit.Code)
		}
		msg := err.Error()
		if !strings.Contains(msg, "not approved within") {
			t.Fatalf("the join gave up without saying it was never approved: %q", msg)
		}
		// The message must tell the operator what to do about it.
		if !strings.Contains(msg, "certificate") {
			t.Fatalf("the give-up message does not say what the operator has to do: %q", msg)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the join never gave up although the approval never came")
	}
}

// TestJoinTimeoutZeroWaitsForever pins that 0 still means "wait forever":
// the timeout is a BOUND on the approval wait, and 0 removes it.
func TestJoinTimeoutZeroWaitsForever(t *testing.T) {
	f := newJoinApprovalFixture(t)

	cancel, exited := runJoinInProcess(t, f, "--timeout", "0")
	defer cancel()

	// Give it well past the 3 s the other test used: with 0 there is no
	// deadline at all, so the join must still be waiting.
	select {
	case err := <-exited:
		t.Fatalf("a join with --timeout 0 exited by itself (%v) — 0 means wait forever", err)
	case <-time.After(15 * time.Second):
	}
}

// TestJoinTimeoutLeavesNoHalfMemberState pins the "no half-member state"
// half of the contract: a join that gave up must not leave the device in the
// roster.
func TestJoinTimeoutLeavesNoHalfMemberState(t *testing.T) {
	f := newJoinApprovalFixture(t)

	cancel, exited := runJoinInProcess(t, f, "--timeout", "2s")
	defer cancel()

	select {
	case <-exited:
	case <-time.After(60 * time.Second):
		t.Fatal("the join never gave up")
	}

	// The device was never approved, so it must not be in the roster.
	if _, ok := f.members.Get(f.joinerID); ok {
		t.Fatal("a join that timed out left the device in the member roster")
	}
}

// TestJoinReboundsAfterMembershipLost pins the SECOND wait.
//
// A device that is revoked while it is a member is a PENDING member again:
// the join goes back to waiting for the approval, and that wait is bounded
// by the same flag. Without the re-arm the join would wait forever for a
// re-approval — the same hang the first wait is bounded against.
func TestJoinReboundsAfterMembershipLost(t *testing.T) {
	f := newJoinApprovalFixture(t)
	// No certificate in the certs directory: the watcher must not be able
	// to re-adopt the device after the revocation.
	f.approveRosterOnly()

	cancel, exited := runJoinInProcess(t, f, "--timeout", "4s")
	defer cancel()

	if !f.streaming(30 * time.Second) {
		t.Fatal("the approved device never attached to the network")
	}

	// The owner revokes the device.
	f.revoke()

	select {
	case err := <-exited:
		var exit ExitError
		if !errors.As(err, &exit) || exit.Code != 1 {
			t.Fatalf("the revoked join ended with %v, want exit 1", err)
		}
		if !strings.Contains(err.Error(), "not approved within") {
			t.Fatalf("the revoked join ended with %v, want the not-approved error", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the join waited forever for a re-approval")
	}
}

// TestJoinRejectsNegativeTimeout keeps the flag honest: a negative timeout
// is a usage error, not "wait forever" and not "expired already".
func TestJoinRejectsNegativeTimeout(t *testing.T) {
	dir := t.TempDir()
	err := Run([]string{"join", "--state", dir, "--timeout", "-1s", "cmesh://join?x=1"})
	var exit ExitError
	if !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("a negative timeout was accepted: %v", err)
	}
}
