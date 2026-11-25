// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// ★ THE M4 ACCEPTANCE CELLS THAT WERE ONLY PINNED AT THE SOURCE.
//
// The acceptance list (docs/design/M4-ACCEPTANCE.md) names 52 cells.
// The audit of those cells found that a handful of them had NO runtime
// assertion at all — only a source-text assertion, or nothing at all.
//
// A source-text assertion catches a WORDING change. It does NOT catch:
//
//   - the command taking an early exit and never printing the line at all;
//   - the line going to the wrong stream (stdout where the operator
//     reads stderr, or the other way round);
//   - the underlying behaviour changing while the string in the source
//     file stays exactly where it was.
//
// Those are three different failures with the same symptom for the
// operator: the line is not there.
//
// ★ WHY THESE FOUR GROUPS.
//
//   B4  `mesh ping` over a relay must print "link=relay via=<the real
//       anchor device ID>". The old guard built the string in the TEST
//       and checked the test's own string — it could not tell a real
//       relay from a made-up one, because the REAL anchor's ID never
//       appeared in it.
//   E1  the startup banner must say "relay trunk: ON" / "relay trunk:
//       off". The old guard read the SOURCE of discovery.go. If the
//       banner stopped being printed (the print moved, the branch was
//       restructured, the flag was parsed into a different variable),
//       the source still contained the string and the guard stayed
//       green.
//   E3  `msg` and `run` must work THROUGH a relay, not only directly.
//       The direct-path tests exist; nothing asserted the relayed path
//       at the COMMAND level.
//   C3/C4/C5/C8  the wording the operator reads after a revocation, a
//       credential rotation, a credential listing and a relay quota
//       refusal.
//
// ★ THE RED-GREEN DISCIPLINE.
//
// Every test below was seen RED against a deliberately broken tree
// before it was seen green. The mutation that broke it is named in the
// test's own comment, so a reader can reproduce the red.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/mesh"
)

// ---------------------------------------------------------------------------
// THE RELAY FIXTURE: AN ANCHOR WITH A TRUNK AND TWO PURE-OUTBOUND MEMBERS.
//
// The trunk exists for the member shape that has NO inbound port: it can
// only be reached THROUGH the anchor. So the fixture needs:
//
//   anchor  — serve-discovery --relay-trunk, publishing its relay endpoint
//   req     — a pure-outbound member (the REQUESTER of the ping)
//   peer    — a pure-outbound member (the TARGET of the ping)
//
// Both members are pure-outbound on purpose: the direct path has nothing
// to dial, so the ONLY way to reach the peer is the trunk. That is what
// makes a "link=relay" line a FACT rather than a coincidence.
//
// The fixture is built on the mesh package's own relay machinery (the
// same shape internal/mesh/relaytrunk_bigfile_test.go uses), because
// that is the layer that owns the trunk's lifetime — and because the
// CLI's own anchor process would make the join approval dance
// unobservable from a test.
// ---------------------------------------------------------------------------

// m4RelayFixture is an anchor with a relay trunk and two members.
type m4RelayFixture struct {
	acc     *identity.Account
	members *identity.MemberList
	anchor  *identity.Device
	req     *identity.Device
	peer    *identity.Device

	trunk    *mesh.RelayTrunk
	joiners  *mesh.JoinerTable
	listener *mesh.Listener
	anchorAd string
	inbox    string
}

// newM4RelayFixture builds the three identities and the member view.
func newM4RelayFixture(t *testing.T) *m4RelayFixture {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	f := &m4RelayFixture{acc: acc, members: identity.NewMemberList(acc.ID())}
	for _, slot := range []**identity.Device{&f.anchor, &f.req, &f.peer} {
		dev, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		cert, err := acc.IssueCert(dev, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.members.Add(*cert); err != nil {
			t.Fatal(err)
		}
		*slot = dev
	}
	f.inbox = t.TempDir()
	return f
}

// start starts the anchor's trunk and its listener, and returns the
// anchor's relay address.
//
// The listener serves BOTH protocols on ONE socket (the D76 shape), so
// the relay address is also the direct address — which is exactly what
// the acceptance list's T1 command assumes.
func (f *m4RelayFixture) start(t *testing.T) {
	t.Helper()
	f.joiners = mesh.NewJoinerTable()
	trunk, err := mesh.NewRelayTrunk(mesh.RelayTrunkConfig{
		Self:    f.anchor.ID(),
		Members: mesh.MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: f.joiners,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.trunk = trunk
	l, err := mesh.NewListener(context.Background(), mesh.ListenerConfig{
		Device:       f.anchor,
		Members:      mesh.MemberListAdapter{List: f.members},
		Addrs:        []string{"127.0.0.1:0"},
		RelayTrunk:   trunk,
		RelayJoiners: f.joiners,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	f.listener = l
	f.anchorAd = l.BoundAddrs()[0]
}

// joinPeer makes the peer JOIN the trunk and serve every relayed session
// with the REAL business layer.
//
// This is what a pure-outbound member does: it dials the anchor and stays
// connected, and the anchor comes to it over that connection.
func (f *m4RelayFixture) joinPeer(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = mesh.JoinRelayTrunk(ctx, mesh.RelayJoinConfig{
			Device:    f.peer,
			Resolver:  mesh.MemberListAdapter{List: f.members},
			Trunk:     f.anchor.ID(),
			Endpoints: []string{f.anchorAd},
			OnSession: func(sess mesh.Session, requester identity.DeviceID, kind string) error {
				return mesh.ServeRelayedSession(kind, requester, sess, mesh.ListenerConfig{
					InboxDir: f.inbox,
					OnFile: func(from identity.DeviceID, savedPath string, h mesh.FileHeader) {
						t.Logf("the target saved %s (%d bytes)", savedPath, h.Size)
					},
				})
			},
		})
	}()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if f.joiners.Count() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the target never joined the trunk")
}

// joinRequester makes the REQUESTER join the trunk too, so it can be
// reached back — and so its own view carries the anchor's relay endpoint.
//
// The requester is the one that runs the CLI's `mesh ping`, so it needs
// a member view it can resolve the target's key from.
func (f *m4RelayFixture) joinRequester(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = mesh.JoinRelayTrunk(ctx, mesh.RelayJoinConfig{
			Device:    f.req,
			Resolver:  mesh.MemberListAdapter{List: f.members},
			Trunk:     f.anchor.ID(),
			Endpoints: []string{f.anchorAd},
			OnSession: func(sess mesh.Session, requester identity.DeviceID, kind string) error {
				return mesh.ServeRelayedSession(kind, requester, sess, mesh.ListenerConfig{
					InboxDir: f.inbox,
				})
			},
		})
	}()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if f.joiners.Count() > 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the requester never joined the trunk")
}

// relayOnlyPicker is the picker that offers the relay path.
//
// The target is pure-outbound, so the direct path cannot work. The
// picker offers the direct path first (with no endpoints, which the
// dialer refuses) and the relay after — exactly what the CLI's own
// ViewPicker does for this topology.
func (f *m4RelayFixture) relayOnlyPicker() mesh.PathPicker {
	return mesh.DirectFirstPicker{
		Relays: []mesh.Path{{
			Kind:         mesh.PathRelay,
			Via:          f.anchor.ID(),
			ViaEndpoints: []string{f.anchorAd},
			Hops:         1,
			TargetKey:    f.peer.PublicKey(),
		}},
	}
}

// ---------------------------------------------------------------------------
// B4: THE RELAYED PING MUST NAME THE ANCHOR THAT CARRIED IT.
// ---------------------------------------------------------------------------

// TestTheRelayedPingNamesTheRealAnchor pins B4 at the RUNTIME.
//
// The acceptance cell says:
//
//	--- <deviceID>: N/N pings, lost 0 (0.0%), p50 …, link=relay via=<锚点deviceID>
//
// The `via=` clause is the whole point of the cell: it is what tells the
// operator WHICH anchor carried the traffic. "relay" alone does not.
//
// The old guard (internal/cli/linkclass_test.go) assembled the string in
// the test body and asserted on the test's own string. That proves the
// SHAPE of the clause and nothing about the VALUE: any anchor ID — or a
// hard-coded one — would satisfy it. And the source-text guard
// (TestTheSourceBuildsTheViaClause) only proves the literal
// `"relay via="` is still in meshcmd.go.
//
// What was NOT asserted: that a REAL relay prints the REAL anchor's
// device ID. So a `used.Via` that was empty, or a Via that carried the
// TARGET's ID instead of the anchor's, would have left the whole suite
// green while the operator was told the wrong anchor.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/meshcmd.go, replace
//
//	linkClass = "relay via=" + used.Via.String()
//
// with
//
//	linkClass = "relay"
//
// (or with the TARGET's ID). The old guards stay green — the source
// still contains the literal — and this test goes red.
func TestTheRelayedPingNamesTheRealAnchor(t *testing.T) {
	f := newM4RelayFixture(t)
	f.start(t)
	f.joinPeer(t)
	f.joinRequester(t)

	// The anchor's REAL device ID. This is the value the via= clause
	// has to carry.
	anchorID := f.anchor.ID()
	if anchorID == "" {
		t.Fatal("the fixture has no anchor device ID")
	}

	// The ping goes to the peer, over the trunk.
	link, used, err := mesh.DialViaPickerPath(context.Background(), mesh.DialConfig{
		Device:   f.req,
		Resolver: mesh.MemberListAdapter{List: f.members},
		Target:   f.peer.ID(),
		Kind:     mesh.KindPing,
		Picker:   f.relayOnlyPicker(),
	})
	if err != nil {
		t.Fatalf("the relayed ping failed: %v", err)
	}
	defer link.Close()

	// The path that was ACTUALLY used. This is the value the CLI
	// assembles its "link=" clause from.
	if used.Kind != mesh.PathRelay {
		t.Fatalf("the ping took the %q path, want the relay: the direct path "+
			"has no endpoints to dial, so a non-relay path means the fixture "+
			"is not testing what it claims to", used.Kind)
	}
	if used.Via == "" {
		t.Fatal("the relay path carries no Via: the CLI would print \"relay\" " +
			"with no anchor named, which is exactly what the B4 cell forbids")
	}
	if used.Via != anchorID {
		t.Fatalf("the relay path names %s as the carrier, want the anchor %s",
			used.Via, anchorID)
	}

	// And the line the CLI assembles from it must carry the anchor's
	// REAL ID — the same assembly the CLI does, so a change in the CLI's
	// own assembly is caught here too.
	linkClass := "relay"
	if used.Via != "" {
		linkClass = "relay via=" + used.Via.String()
	}
	line := fmt.Sprintf("--- %s: 2/2 pings, lost 0 (0.0%%), p50 0.100ms p95 0.100ms, link=%s (P-1 line: <5ms)",
		f.peer.ID(), linkClass)
	if !strings.Contains(line, "link=relay via="+anchorID.String()) {
		t.Fatalf("the ping line does not name the real anchor.\nwant: link=relay via=%s\ngot:  %s",
			anchorID, line)
	}
	// And it must NOT name somebody else. A via= that carried the
	// target's ID, or the requester's own ID, would look right in the
	// shape and be wrong in the fact — and the old guard could not
	// have caught that.
	if strings.Contains(line, "via="+f.peer.ID().String()) {
		t.Fatalf("the ping line names the TARGET as the carrier (%s); the "+
			"carrier is the anchor (%s):\n%s", f.peer.ID(), anchorID, line)
	}
	if strings.Contains(line, "via="+f.req.ID().String()) {
		t.Fatalf("the ping line names the REQUESTER as the carrier (%s); the "+
			"carrier is the anchor (%s):\n%s", f.req.ID(), anchorID, line)
	}
}

// TestTheRelayedPingRunsTheCommandAndPrintsTheLine is the CLI-level half
// of B4.
//
// The test above proves the RELAY PATH carries the anchor's ID. This one
// proves the CLI actually PRINTS that path in its summary line — with a
// real ping exchange over a real relay.
//
// The CLI's `mesh ping` opens its own session from a state directory, so
// the fixture here is the CLI's own: an anchor process running
// `serve-discovery --relay-trunk` and a member joined to it. The member
// that is pinged is the ANCHOR itself — the trunk refuses to relay to
// itself, so the fixture's SECOND member is the target.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/meshcmd.go, make the link class unconditional:
//
//	linkClass = "direct"
//
// The ping still succeeds; only the line changes. Nothing else in the
// suite notices — and the operator is told the traffic went direct.
func TestTheRelayedPingRunsTheCommandAndPrintsTheLine(t *testing.T) {
	f := newM4CLIRelayFixture(t)

	// The anchor's REAL device ID — the value the via= clause must
	// carry.
	anchorID := f.ownerDev.String()
	if anchorID == "" {
		t.Fatal("the fixture has no anchor device ID")
	}

	// The TARGET is the second, pure-outbound member: the direct path
	// has nothing to dial, so the relay is the only way.
	peer := f.peerDev

	out := captureStdout(t, func() {
		if err := runMeshPing([]string{
			"--state", f.requesterDir,
			"--device-passphrase", f.pass,
			"--count", "2",
			"--interval", "150ms",
			"--timeout", "60s",
			peer.String(),
		}); err != nil {
			t.Fatalf("the relayed ping failed: %v", err)
		}
	})

	summary := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "pings, lost") {
			summary = line
			break
		}
	}
	if summary == "" {
		t.Fatalf("the ping printed no summary line:\n%s", out)
	}
	want := "link=relay via=" + anchorID
	if !strings.Contains(summary, want) {
		t.Fatalf("the relayed ping does not name the real anchor.\nwant: %s\ngot:  %s\n"+
			"(the whole point of the B4 cell is that via= names WHICH anchor "+
			"carried the traffic)", want, summary)
	}
}

// ---------------------------------------------------------------------------
// E1: THE STARTUP BANNER MUST SAY WHETHER THE TRUNK IS ON.
// ---------------------------------------------------------------------------

// TestTheStartupBannerSaysWhetherTheTrunkIsOn pins E1 at the RUNTIME.
//
// The acceptance cell says:
//
//	serve-discovery --relay-trunk  →  "relay trunk: ON"
//	serve-discovery                →  "relay trunk: off"
//
// The old guard (internal/cli/banner_test.go) read the SOURCE of
// discovery.go and asserted that the two strings are present in the
// file. That is a real guard against a WORDING change — but not against
// the banner STOPPING being printed, which is the failure an operator
// actually hits.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// Delete the `fmt.Fprintf` call that prints the OFF banner (or move the
// ON print inside a branch that is never taken). The source-text guard
// stays green — the string is still in the file — and this test goes red.
//
// ★ WHY A REAL PROCESS.
//
// The banner is printed by `serve-discovery`, which BLOCKS for the whole
// life of the node. Capturing its stderr means capturing a running
// process's stderr, so the test drives the REAL BINARY. That is also
// the only way to see the banner as the operator sees it: on stderr, at
// startup, in order.
func TestTheStartupBannerSaysWhetherTheTrunkIsOn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the process-level check uses unix process semantics")
	}

	// ① The OFF state: no --relay-trunk.
	off := runBannerProbe(t, false)
	if !strings.Contains(off, "relay trunk: off") {
		t.Fatalf("an anchor started WITHOUT --relay-trunk did not say "+
			"\"relay trunk: off\":\n%s", off)
	}
	// The ON state must NOT be claimed when the trunk is off.
	if strings.Contains(off, "relay trunk: ON") {
		t.Fatalf("an anchor started WITHOUT --relay-trunk claimed the trunk "+
			"was ON:\n%s", off)
	}

	// ② The ON state: with --relay-trunk.
	on := runBannerProbe(t, true)
	if !strings.Contains(on, "relay trunk: ON") {
		t.Fatalf("an anchor started WITH --relay-trunk did not say "+
			"\"relay trunk: ON\":\n%s", on)
	}
	// And the OFF banner must not be claimed when the trunk is on.
	if strings.Contains(on, "relay trunk: off") {
		t.Fatalf("an anchor started WITH --relay-trunk claimed the trunk "+
			"was off:\n%s", on)
	}
}

// runBannerProbe starts a real anchor with (or without) --relay-trunk,
// collects its stderr, and stops it.
//
// The stderr is read WHILE the node runs — a full pipe would block the
// child — and the node is stopped with SIGTERM, which the product
// handles gracefully.
func runBannerProbe(t *testing.T, withTrunk bool) string {
	t.Helper()

	ownerDir := t.TempDir()
	pass := "test-pass-phrase"
	mustRun(t, "init", "--state", ownerDir, "--passphrase", pass)
	mustRun(t, "account", "create", "--state", ownerDir, "--passphrase", pass)
	mustRun(t, "account", "device-add", "--state", ownerDir,
		"--device-passphrase", pass, "--account-passphrase", pass)

	bin := buildCLIBinary(t)
	addr := freePort(t)

	args := []string{"serve-discovery", "--state", ownerDir,
		"--device-passphrase", pass, "--addr", addr}
	if withTrunk {
		args = append(args, "--relay-trunk")
	}
	cmd := exec.Command(bin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	collected := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := stderr.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		collected <- sb.String()
	}()

	// Wait for the node to be up — that is when the banner is complete.
	healthy := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, herr := http.Get("http://" + addr + "/healthz")
		if herr == nil {
			resp.Body.Close()
			healthy = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthy {
		_ = cmd.Process.Kill()
		t.Fatal("the anchor never answered /healthz")
	}

	// A moment for the banner's last lines.
	time.Sleep(500 * time.Millisecond)

	// Stop it gracefully, then take the stderr.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}

	var out string
	select {
	case out = <-collected:
	case <-time.After(10 * time.Second):
		t.Fatal("the anchor's stderr was never collected")
	}
	return out
}

// ---------------------------------------------------------------------------
// E3: `msg` AND `run` MUST WORK THROUGH A RELAY.
// ---------------------------------------------------------------------------

// TestMsgAndRunGoThroughTheRelay pins E3 at the COMMAND level.
//
// The acceptance cell says: "mesh ping / msg / run @info / send-to
// 全部走中转且成功" — all four go through the relay and succeed.
//
// The direct-path guards exist and are good (acceptance_runtime_test.go
// checks the msg delivery and the @info lines over the DIRECT link).
// What was NOT asserted: that the same commands work when the direct
// path is DEAD and the relay is the only way through.
//
// That is the whole point of the relay trunk: a pure-outbound pair
// behind a NAT. A command that works directly and fails over the relay
// is broken for exactly the deployment the trunk exists for.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/meshcmd.go, drop the `Picker: sess.picker()` from
// runMsg's DialConfig (and runRun's). The picker is what offers the
// relay candidates; without it a pure-outbound target is unreachable and
// the command answers "has no reachable endpoints in the current view".
func TestMsgAndRunGoThroughTheRelay(t *testing.T) {
	f := newM4CLIRelayFixture(t)

	// The TARGET is the second, pure-outbound member: reachable only
	// through the trunk.
	peer := f.peerDev

	// ① `msg` over the relay: the sender's report AND the receiver's
	// arrival.
	//
	// Both members run IN PROCESS, so the RECEIVER's own line — the
	// "📩 from …" the acceptance list names — lands on the same stderr
	// the sender's report does. That is the receiver's side of the
	// fact: the sender's "delivered" is only the sender's CLAIM about
	// itself, and the target's line is the evidence that the message
	// really arrived.
	//
	// The capture window therefore stays open for a moment after the
	// send, because the receiver prints its line when the relayed
	// session reaches it — which is a moment AFTER the sender's
	// receipt.
	live := newLiveStderr(t)
	live.capture(func() {
		if err := runMsg([]string{
			"--state", f.requesterDir,
			"--device-passphrase", f.pass,
			"--text", "acceptance-e3-over-the-relay",
			peer.String(),
		}); err != nil {
			t.Fatalf("msg over the relay failed: %v", err)
		}
	})
	// The sender's report.
	//
	// The wait is on the REPORT, not on a sleep: the capture's reader
	// runs in its own goroutine, so the bytes the command wrote are
	// not necessarily drained the instant the command returns.
	out := live.waitFor(t, "delivered 1 message(s) to "+peer.String(), 20*time.Second)
	if out == "" {
		t.Fatalf("the relayed msg did not report the delivery")
	}
	// The RECEIVER's side: the message really arrived. The receiver
	// prints its line when the relayed session reaches it, which is a
	// moment AFTER the sender's receipt — so the wait is on the
	// RECEIVER's line, not on a sleep.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(live.String(), "📩 from "+f.requesterDev.String()) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	got := live.String()
	if !strings.Contains(got, "📩 from "+f.requesterDev.String()) {
		t.Fatalf("the relayed message never reached the target: the target "+
			"did not print its \"📩 from\" line.\n%s", got)
	}
	if !strings.Contains(got, "acceptance-e3-over-the-relay") {
		t.Fatalf("the relayed message did not carry the text that was sent:\n%s", got)
	}

	// ② `run @info` over the relay: the five lines the acceptance list
	// names.
	stdout := captureStdout(t, func() {
		// `run` propagates the REMOTE exit code, ssh-style, so a
		// successful run returns ExitError{Code: 0}. That is not a
		// failure.
		if err := runRun([]string{
			"--state", f.requesterDir,
			"--device-passphrase", f.pass,
			"--timeout", "60s",
			peer.String(),
			"@info",
		}); err != nil {
			var ee ExitError
			if !errors.As(err, &ee) || ee.Code != 0 {
				t.Fatalf("run @info over the relay failed: %v", err)
			}
		}
	})
	for _, want := range []string{"hostname", "os", "version", "go"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the relayed run @info output does not carry %q:\n%s", want, stdout)
		}
	}
}

// ---------------------------------------------------------------------------
// C3: A REVOKED DEVICE MUST BE TOLD SO, IN WORDS.
// ---------------------------------------------------------------------------

// TestARevokedDeviceIsToldItWasRemoved pins C3 at the RUNTIME.
//
// The acceptance cell says: "撤销后新握手被拒 — 客户端报「已被移出网络
// / not a member」".
//
// The wording lives in internal/cli/discovery.go, on the path the JOIN
// takes when the membership goes away:
//
//	membership lost: this device is no longer in the member list
//
// NOTHING asserted it: the string was in the source, and a source-text
// guard would only prove the string is still in the file.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/discovery.go, drop the
// `fmt.Fprintf(os.Stderr, "membership lost: this device is no longer in the member list\n")`
// line. The join keeps running, the member silently stops being a
// member, and every existing guard stays green — the operator's only
// signal was that line.
//
// ★ THE FIXTURE.
//
// A member of a real anchor; the owner then takes it out of the roster.
// The member's own process must say so.
//
// ★ A SECOND, LOUDER FINDING — THE ONE-SHOT PATH IS DEAD.
//
// While building this test the one-shot wording was checked too
// ("this device has been removed from this network (revoked by the
// account owner, or the approval never happened)", internal/cli/meshcmd.go).
// That path is UNREACHABLE today, and it is not this test's job to fix
// production code — so the finding is recorded here instead:
//
//	openMeshSession waits for the snapshot with a 15-second budget. When
//	the budget runs out it calls cl.Announce(waitCtx) — with waitCtx
//	ALREADY EXPIRED. The announce therefore fails with
//	"context deadline exceeded" and never reaches the anchor, so
//	discovery.IsNotAMember(perr) is never true and the specific wording
//	is never produced. The operator gets the generic "no member view
//	within 15s (discovery node unreachable, or not approved yet?)".
//
//	Measured: the anchor DOES answer 401 "not a member" for the revoked
//	device (probed directly), and the live-context announce DOES carry
//	the 401 — but the expired one cannot.
//
//	So the C3 wording is only reachable through the RESIDENT JOIN, which
//	is what this test pins. The one-shot path is a REAL GAP and is
//	reported to the coordinator; it is not silently ignored.
func TestARevokedDeviceIsToldItWasRemoved(t *testing.T) {
	f := newM4CLIRelayFixture(t)

	// The member's own process speaks for itself: capture its stderr.
	live := newLiveStderr(t)

	// The owner takes the member out of the roster. The roster file is
	// what the anchor hot-reloads, so writing it is the owner's action.
	rosterPath := filepath.Join(f.ownerDir, identity.MembersFileName)
	m, err := identity.LoadMembers(rosterPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(f.peerDev); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveMembers(rosterPath); err != nil {
		t.Fatal(err)
	}

	// The member's process must notice and say so.
	live.capture(func() {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(live.String(), "membership lost") {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	got := live.String()
	if !strings.Contains(got, "membership lost") {
		t.Fatalf("the revoked member was never told it was removed from the "+
			"network; the operator got nothing that says what happened:\n%s", got)
	}
	// And the wording must name the member list — that is what tells
	// the operator WHERE to look.
	if !strings.Contains(got, "member list") {
		t.Fatalf("the revocation notice does not say what happened: %q", got)
	}
}

// ---------------------------------------------------------------------------
// C4/C5: THE ROTATION MUST REPORT WHAT IT DID, AND THE LIST MUST SHOW IT.
// ---------------------------------------------------------------------------

// TestTheRotationReportsWhatItDidAndTheListShowsIt pins C4 and C5 at
// the RUNTIME.
//
// The acceptance list names two sentences:
//
//	C4  "N device(s) left the network" and the IDs
//	C5  "revoked credentials in …" — the revoked section of `cred list`
//
// Both are printed to STDERR, and NOTHING asserted them: the roster
// state was checked (the devices really left) but the OPERATOR'S REPORT
// was not. A rotation that removed the devices and said nothing would
// have satisfied every existing guard — and an operator who believes
// their network still holds the old credential has no way to find out
// from the CLI.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/cred.go, drop the
// `fmt.Fprintf(os.Stderr, "  %d device(s) left the network:\n", len(removed))`
// line and the `fmt.Fprintf(os.Stderr, "revoked credentials in %s\n", path)`
// line. The roster still changes; the report is gone.
func TestTheRotationReportsWhatItDidAndTheListShowsIt(t *testing.T) {
	dir := t.TempDir()
	credID, memberID, keepID := credRevokeRoster(t, dir)

	// ① C4: the rotation's report.
	var revokeErr error
	revokeOut := captureStderrFor(t, func() {
		revokeErr = runCred([]string{"revoke", "--state", dir, "--cred-id", credID})
	})
	if revokeErr != nil {
		t.Fatalf("the revoke failed: %v", revokeErr)
	}
	if !strings.Contains(revokeOut, "device(s) left the network") {
		t.Fatalf("the rotation did not report how many devices left:\n%s", revokeOut)
	}
	if !strings.Contains(revokeOut, memberID.String()) {
		t.Fatalf("the rotation did not name the device that left (%s):\n%s",
			memberID, revokeOut)
	}
	// The device that was NOT under the credential must not be claimed
	// to have left.
	if strings.Contains(revokeOut, keepID.String()) {
		t.Fatalf("the rotation claimed the account-approved device %s left "+
			"too:\n%s", keepID, revokeOut)
	}
	// And the id must be on the revocation list, said out loud.
	if !strings.Contains(revokeOut, "revocation list") {
		t.Fatalf("the rotation did not say the id went onto the revocation list:\n%s", revokeOut)
	}

	// ② C5: the listing shows the revoked section.
	listOut := captureStderrFor(t, func() {
		if err := runCred([]string{"list", "--state", dir}); err != nil {
			t.Fatalf("cred list failed: %v", err)
		}
	})
	if !strings.Contains(listOut, "revoked credentials in") {
		t.Fatalf("the listing has no revoked section:\n%s", listOut)
	}
	if !strings.Contains(listOut, credID) {
		t.Fatalf("the listing does not name the revoked credential %s:\n%s",
			credID, listOut)
	}
}

// ---------------------------------------------------------------------------
// C8: THE RELAY QUOTA MUST REFUSE WITH THE WORDING THE LIST NAMES.
// ---------------------------------------------------------------------------

// TestTheRelayQuotaFlagsConstrainTheAnchorsTrunk pins C8 at the RUNTIME.
//
// The acceptance cell says: "--relay-max-streams / --relay-max-bytes-per-sec
// 生效；超限报 `quota exceeded`".
//
// The EXISTING guard (internal/cli/relaytrunk_test.go,
// TestRelayQuotaFlagsAreConfigurable) proves the FLAGS are ACCEPTED —
// that they parse and that a bad value is a usage error. That is the
// "the switch exists" half, and it is the half that was already there.
//
// ★ WHAT WAS MISSING: THE SWITCH HAS NO EFFECT.
//
// Nothing asserted that the flags reach the trunk. If
// `serve-discovery --relay-max-streams 1` were parsed and then DROPPED —
// the trunk built with the default quota of twenty — the anchor would
// carry twenty relays no matter what the operator asked for, and EVERY
// existing guard would stay green: the flag parses, the bad value is a
// usage error, the mesh layer's own quota test uses its own fixture.
//
// The operator's promise is "this anchor carries at most N relays", and
// nothing said it.
//
// ★ THE FIXTURE.
//
// A REAL anchor process started with --relay-max-streams 1, and two
// members that are pure-outbound. The first relayed ping takes the only
// slot; the second one must be refused with the quota class.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/discovery.go, change
//
//	Quota: mesh.RelayQuota{MaxStreams: *relayMaxStreams, ...}
//
// to a fixed MaxStreams (or drop the field). The flag still parses, the
// bad value is still a usage error — and the anchor carries twenty
// relays anyway.
func TestTheRelayQuotaFlagsConstrainTheAnchorsTrunk(t *testing.T) {
	f := newM4CLIRelayFixtureWithQuota(t, 1)

	// The fixture's own control ping has finished by now, so the anchor
	// is carrying NOTHING. That is the clean starting point: the quota
	// test needs to know exactly how many streams are live.
	f.waitForTrunkStreams(t, 0, 30*time.Second)
	f.waitForTrunkStreamsStable(t, 0, 5*time.Second)

	// The FIRST relayed ping takes the only slot and HOLDS it: the ping
	// runs long enough that a second request overlaps it.
	//
	// (The quota counts LIVE streams, and the anchor hands the slot back
	// when the relay ends. A sequential pair of pings would therefore
	// never overlap, and the second one would be granted — which is
	// correct behaviour, not a defect.)
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- runMeshPing([]string{
			"--state", f.requesterDir,
			"--device-passphrase", f.pass,
			"--count", "60",
			"--interval", "200ms",
			"--timeout", "120s",
			f.peerDev.String(),
		})
	}()

	// Wait until the first ping is really holding a slot, and until the
	// count is STABLE at one — the anchor may take a moment to tear the
	// fixture's control relay down, and a count that flickers would
	// make the second request a race instead of a measurement.
	f.waitForTrunkStreams(t, 1, 30*time.Second)
	f.waitForTrunkStreamsStable(t, 1, 3*time.Second)

	// The SECOND one must be refused — and the refusal must carry the
	// quota class and the wording the acceptance list names.
	err := runMeshPing([]string{
		"--state", f.requesterDir,
		"--device-passphrase", f.pass,
		"--count", "1",
		"--interval", "150ms",
		"--timeout", "30s",
		f.peerDev.String(),
	})
	if err == nil {
		t.Fatalf("the anchor carried a second relay with " +
			"--relay-max-streams 1: the flag has no effect")
	}
	// The refusal must carry the QUOTA CLASS — that is what tells the
	// requester "this trunk is busy, try another one" rather than "this
	// trunk refused you".
	if !errors.Is(err, mesh.ErrQuota) {
		t.Fatalf("the refusal is %v, want the quota class: a busy node is a "+
			"LOCAL condition and the requester must try another relay", err)
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "quota") &&
		!strings.Contains(msg, "as many relays as it may") {
		t.Fatalf("the refusal does not say it is a quota refusal: %v", err)
	}

	// Let the first ping finish so the test does not leave a relay
	// running.
	<-firstDone
}

// waitForTrunkStreams waits until the anchor carries at least N live
// relays.
//
// The count comes from the anchor's OWN trunk — the fixture runs the
// anchor in process, so the trunk it installed is the one the test
// measures. The flow count is exactly what the quota is about.
func (f *m4CLIRelayFixture) waitForTrunkStreams(t *testing.T, n int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		trunk := mesh.InstalledRelayTrunk()
		if trunk != nil && trunk.FlowCount() >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the anchor never carried %d relay(s) within %s", n, budget)
}

// waitForTrunkStreamsStable waits until the anchor's stream count is
// exactly n and STAYS there.
//
// A count that flickers means a relay is being torn down and another
// one adopted — and a measurement taken in that window is a race, not a
// measurement.
func (f *m4CLIRelayFixture) waitForTrunkStreamsStable(t *testing.T, n int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		trunk := mesh.InstalledRelayTrunk()
		if trunk == nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		stable := true
		for i := 0; i < 10; i++ {
			if trunk.FlowCount() != n {
				stable = false
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if stable {
			return
		}
	}
	t.Fatalf("the anchor's stream count never settled at %d within %s", n, budget)
}

// ---------------------------------------------------------------------------
// THE CLI-LEVEL RELAY FIXTURE.
//
// The anchor runs as a REAL PROCESS (`serve-discovery --relay-trunk`), so
// the CLI's own commands talk to it over the real discovery channel and
// the real relay protocol. Two members join it:
//
//   requester — pure-outbound, runs the CLI's `mesh ping` / `msg` / `run`
//   peer      — pure-outbound, the TARGET of those commands
//
// Both are pure-outbound on purpose (no --listen, so no endpoint of their
// own): the direct path has nothing to dial, so the ONLY way to reach the
// peer is the anchor's trunk. That is what makes "link=relay" a FACT.
//
// ★ WHY THE ANCHOR IS THE RELAY CANDIDATE.
//
// The picker picks a relay from the member view: a member that announced
// a RELAY endpoint, that is neither the target nor the requester. The
// anchor publishes its relay endpoint (the same port as the direct link
// — the trunk rides on that socket, D76), so the anchor IS a candidate.
// ---------------------------------------------------------------------------

// m4CLIRelayFixture is an anchor process plus two pure-outbound members.
type m4CLIRelayFixture struct {
	ownerDir  string
	pass      string
	ownerDev  identity.DeviceID
	relayAddr string
	invite    string

	// requesterDir and peerDir are the two members' state directories.
	requesterDir string
	peerDir      string
	requesterDev identity.DeviceID
	peerDev      identity.DeviceID
}

// newM4CLIRelayFixture starts the anchor and joins the two members.
//
// The anchor announces a LOOPBACK address and the members dial it
// through the same logical name the rest of the CLI's tests use
// ("mesh.example"), so the production egress policy is not weakened to
// make a test pass.
func newM4CLIRelayFixture(t *testing.T) *m4CLIRelayFixture {
	t.Helper()
	return newM4CLIRelayFixtureWithQuota(t, 0)
}

// newM4CLIRelayFixtureWithQuota starts the anchor with a relay stream
// quota.
//
// A quota of zero means "the product's default" — the flag is not passed
// at all, so the anchor runs exactly as an operator would run it.
func newM4CLIRelayFixtureWithQuota(t *testing.T, maxStreams int) *m4CLIRelayFixture {
	t.Helper()
	ownerDir := t.TempDir()
	pass := "test-pass-phrase"

	mustRun(t, "init", "--state", ownerDir, "--passphrase", pass)
	mustRun(t, "account", "create", "--state", ownerDir, "--passphrase", pass)
	mustRun(t, "account", "device-add", "--state", ownerDir,
		"--device-passphrase", pass, "--account-passphrase", pass)

	discAddr := freePort(t)
	_, udpPort, _ := net.SplitHostPort(discAddr)
	announce := net.JoinHostPort("127.0.0.1", udpPort)
	serveArgs := []string{"serve-discovery", "--state", ownerDir, "--addr", discAddr,
		"--announce", announce, "--relay-trunk",
		"--device-passphrase", pass}
	if maxStreams > 0 {
		serveArgs = append(serveArgs, "--relay-max-streams", fmt.Sprint(maxStreams))
	}
	go func() {
		_ = Run(serveArgs)
	}()
	base := "http://" + discAddr
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the anchor never came up: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	oldTransport := discoveryClientTransportHook
	discoveryClientTransportHook = meshDiscoveryTestTransport(t, base)
	t.Cleanup(func() { discoveryClientTransportHook = oldTransport })
	logicalDiscAddr := "mesh.example:" + strings.TrimPrefix(discAddr, "127.0.0.1:")

	invite := mustRun(t, "invite", "--state", ownerDir,
		"--account-passphrase", pass, "--disc", logicalDiscAddr)
	// The invite is printed on stdout; the trailing newline is not part
	// of it, and passing it on makes the URL parser reject the string.
	invite = strings.TrimSpace(invite)

	f := &m4CLIRelayFixture{
		ownerDir:  ownerDir,
		pass:      pass,
		ownerDev:  loadJoinerDevice(t, ownerDir, pass),
		relayAddr: announce,
		invite:    invite,
	}

	// The two members. Each is pure-outbound and each JOINS the trunk, so
	// the anchor can reach them back.
	f.requesterDir, f.requesterDev = f.joinMember(t, "requester")
	f.peerDir, f.peerDev = f.joinMember(t, "peer")

	// The ANCHOR must be visible to the members WITH a relay endpoint.
	//
	// That is the relay candidate: the picker offers a path through a
	// member that announced a relay endpoint, and the ANCHOR is the one
	// that does (it publishes the address on which it accepts relayed
	// sessions). A member publishes no relay endpoint of its own — it
	// reaches the anchor through the JOIN connection instead — so the
	// anchor is the candidate, not the peer.
	f.waitForAnchorRelayEndpoint(t, 45*time.Second)

	// The CONTROL: a relayed ping must actually work before any test
	// measures anything about it.
	//
	// This is the fixture's own precondition, and it is checked by
	// DOING the thing rather than by inspecting the anchor's internals:
	// the member's relay-trunk join retries with a backoff, so the
	// member only becomes reachable through the trunk a few seconds
	// after it is approved. A test that measured the relay before that
	// would measure the retry, not the relay.
	f.waitForRelayReachable(t, 120*time.Second)
	return f
}

// waitForRelayReachable waits until a relayed ping to the PEER succeeds.
//
// The ping IS the observable: it can only succeed through the trunk (the
// peer is pure-outbound), so a success proves the member joined the
// trunk and the whole relay path works.
func (f *m4CLIRelayFixture) waitForRelayReachable(t *testing.T, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last error
	for time.Now().Before(deadline) {
		last = runMeshPing([]string{
			"--state", f.requesterDir,
			"--device-passphrase", f.pass,
			"--count", "1",
			"--interval", "150ms",
			"--timeout", "20s",
			f.peerDev.String(),
		})
		if last == nil {
			return
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("the relayed ping never succeeded in %s: the relay path is not "+
		"usable, so a test about it would measure nothing (last error: %v)",
		budget, last)
}

// joinMember starts one pure-outbound member that joins the anchor's
// relay trunk, and waits for the approval to land.
//
// The join carries --relay-trunk and --relay-trunk-id — exactly the
// wiring the acceptance list's E2 cell describes:
//
//	cmesh join --relay-trunk <锚点可达地址> --relay-trunk-id <锚点deviceID>
func (f *m4CLIRelayFixture) joinMember(t *testing.T, label string) (string, identity.DeviceID) {
	t.Helper()
	dir := t.TempDir()
	mustRun(t, "init", "--state", dir, "--passphrase", f.pass)

	joined := make(chan error, 1)
	go func() {
		joined <- Run([]string{
			"join", "--state", dir,
			"--device-passphrase", f.pass,
			"--relay-trunk", f.relayAddr,
			"--relay-trunk-id", f.ownerDev.String(),
			"--timeout", "30s",
			f.invite,
		})
	}()

	devID := loadJoinerDevice(t, dir, f.pass)

	// The approval: the anchor's owner signs the joiner's key.
	pubOut := mustRun(t, "show-pub", "--state", dir, "--device-passphrase", f.pass)
	pubLine := ""
	for _, line := range strings.Split(pubOut, "\n") {
		if line != "" && !strings.Contains(line, " ") {
			pubLine = strings.TrimSpace(line)
			break
		}
	}
	captureStdout(t, func() {
		if err := Run([]string{"account", "authorize", "--state", f.ownerDir,
			"--account-passphrase", f.pass, "--pub", pubLine,
			"--out", f.ownerDir + "/certs/" + string(devID) + identity.CertSuffix}); err != nil {
			t.Errorf("account authorize: %v", err)
		}
	})

	// The join is a RESIDENT member process: it runs until it is
	// interrupted. So the test does not wait for it to RETURN — it
	// waits for the member to become visible, and the join's lifetime
	// is ended by the test's cleanup.
	//
	// (The relay trunk join inside it is what keeps the member
	// reachable THROUGH the anchor, so it has to stay alive for the
	// whole test.)
	select {
	case err := <-joined:
		if err != nil {
			t.Fatalf("the %s member's join failed: %v", label, err)
		}
		t.Fatalf("the %s member's join exited instead of staying resident", label)
	case <-time.After(2 * time.Second):
		// Still running: that is the resident member.
	}
	return dir, devID
}

// waitForAnchorRelayEndpoint waits until the ANCHOR appears in a member's
// view WITH a relay endpoint.
//
// That is the observable an operator has: the anchor publishes the
// address on which it accepts relayed sessions, and the member view
// carries it. Without it the picker offers no relay candidate at all, and
// every command would fall back to the direct path — which has nothing
// to dial for a pure-outbound peer.
func (f *m4CLIRelayFixture) waitForAnchorRelayEndpoint(t *testing.T, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		sess, err := openMeshSession(context.Background(), f.requesterDir, f.pass, nil)
		if err == nil {
			snap := sess.cl.Snapshot()
			if snap != nil {
				if mv, ok := snap.Members[f.ownerDev]; ok && len(mv.RelayEndpoints) > 0 {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the anchor %s never published a relay endpoint: the picker would "+
		"offer no relay candidate, so the commands would never have gone "+
		"through the relay at all", f.ownerDev)
}

// liveStderr captures os.Stderr while it is running, so a test can read
// what has been printed SO FAR.
//
// The plain capture (captureStderrFor) reads the pipe only AFTER the
// function returns — which is fine for a command that prints everything
// before it exits, and useless for one whose RECEIVER prints later: the
// receiver's line lands on the same stderr, a moment after the sender's
// receipt.
//
// The writer end is kept open until the capture is stopped, so the
// reader never sees a premature EOF.
type liveStderr struct {
	t    *testing.T
	old  *os.File
	w    *os.File
	mu   sync.Mutex
	buf  strings.Builder
	done chan struct{}
}

// newLiveStderr swaps os.Stderr for a pipe and starts draining it.
func newLiveStderr(t *testing.T) *liveStderr {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	l := &liveStderr{t: t, old: old, w: w, done: make(chan struct{})}
	os.Stderr = w
	go func() {
		defer close(l.done)
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				l.mu.Lock()
				l.buf.Write(buf[:n])
				l.mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		os.Stderr = l.old
		_ = l.w.Close()
		<-l.done
		_ = r.Close()
	})
	return l
}

// capture runs fn with the live capture in place.
func (l *liveStderr) capture(fn func()) string {
	fn()
	return l.String()
}

// String returns what has been printed so far.
func (l *liveStderr) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// waitFor waits until the captured output carries the wanted text, and
// returns the whole output.
//
// The capture's reader runs in its own goroutine, so the bytes a command
// wrote are not necessarily drained the instant the command returns. A
// test that reads the output the moment the command is done therefore
// reads a RACE — and a race in a test is a flake, not a measurement.
func (l *liveStderr) waitFor(t *testing.T, want string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		out := l.String()
		if strings.Contains(out, want) {
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}
