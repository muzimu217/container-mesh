// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// ★ THE M4 ACCEPTANCE CELLS THAT HAD NO RUNTIME GUARD (M4-4).
//
// The acceptance list (docs/design/M4-ACCEPTANCE.md §2.1 and §2.2) names
// nine cells whose judgement is about what the OPERATOR READS on a real
// command, and every one of them was only half-pinned:
//
//	A3  `account show` prints BOTH `pubkey.b64 ` and `pubkey.hex `
//	A6  invite-join prints `PUBKEY` and later
//	    `approved: this device is now a member`
//	A7  `join --cred` exits 0 and says
//	    `Enrolled with a team credential`
//	A8  a device beyond the credential's quota is refused with
//	    `credential quota exhausted`
//	A10 `join --cred` says `certificate renewed` when it renews
//	A11 SIGTERM to a real `join` process makes the ANCHOR's view say
//	    offline
//	B1  `cmesh status` on a live session lists members, online,
//	    endpoints and last_seen
//	B2  `status --probe <id>` reports `reachability=direct` (or `relay`)
//	B3  a direct `mesh ping` reports `lost 0 (0.0%)`, `p50` and
//	    `link=direct`
//
// The existing guards for these cells were either a pure function (a
// hand-written struct, a resolver with no peer) or an assertion on the
// WRONG observable (the roster changed, but nothing said what the operator
// read). A cell whose judgement is "the operator is told X" cannot be
// pinned by a unit test of a helper: the helper is not where the operator
// reads.
//
// ★ WHY THE FIXTURES ARE REAL PROCESSES.
//
// The observables above are PROCESS observables:
//
//   - the wording lands on the process's stderr, which a caller sees;
//   - the exit code is the process's exit code;
//   - SIGTERM is a SIGNAL to a process, and the LEAVE wiring it triggers
//     lives in the join's signal path (internal/cli/discovery.go), not in
//     any function a test can call directly.
//
// So the fixture runs the REAL BINARY: a real anchor process
// (`serve-discovery`) and real member processes (`join`, `join --cred`).
// In-process `Run` would not do for most of them: it shares the test
// binary's stderr with every other test in the package, and it cannot
// receive a SIGTERM that means "this member left the network" without
// taking the whole test binary down.
//
// The ONE exception is A10, and the acceptance list itself asks for it:
// that cell's own note says "值守（用 `certRenewIntervalHook` 压缩）". The
// renewal interval is floored at one hour in production, so no real
// process can renew inside a test's budget. A10 therefore drives the real
// join function in process, against a REAL anchor process, with the
// product's own documented test hook compressing the interval.
//
// ★ THE MUTATION THAT MAKES EACH GUARD RED.
//
// Named in each test's own comment, so a reader can reproduce the red:
//
//	A3   delete the `pubkey.hex` print in internal/cli/mesh.go
//	A6   delete the `PUBKEY:` print, or the `approved:` print, in
//	     internal/cli/discovery.go
//	A7   delete the "Enrolled with a team credential" print in
//	     internal/cli/discovery.go
//	A8   reword the quota refusal in internal/cli/enroll.go so the
//	     original text "credential quota exhausted" is gone
//	A10  delete the "certificate renewed" print in
//	     internal/cli/discovery.go
//	A11  break the signal→LEAVE wiring in internal/cli/discovery.go
//	     (the `case <-ctx.Done()` arm that calls gracefulLeave)
//	B1   stop printing `endpoints=`/`last_seen=` in
//	     internal/cli/status.go
//	B2   make probeMember always return reachabilityUnknown
//	B3   make the ping's link class unconditional ("direct")

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// ---------------------------------------------------------------------------
// THE REAL-PROCESS FIXTURE.
//
// The anchor is a REAL `serve-discovery` process: it serves the discovery
// HTTP API, the members WebSocket stream and the credential enrollment
// endpoint, and it hot-reloads the roster file. A member is a REAL `join`
// process.
//
// The one switch the product offers for a private deployment is used here:
// CMESH_ALLOW_PRIVATE_DISCOVERY=1. The anchor lives on 127.0.0.1, and the
// default outbound policy refuses a loopback discovery origin on purpose
// (the round-56 SSRF finding). The switch is the OPERATOR'S own escape
// hatch for a LAN deployment — internal/cli/discovery.go:156 — and using it
// keeps the product's policy intact instead of weakening it to make a test
// pass.
// ---------------------------------------------------------------------------

// m4Pass is the passphrase every fixture identity uses.
const m4Pass = "test-pass-phrase"

// m4PanelToken is the panel token the fixture anchor is started with.
//
// The panel's data endpoint (GET /api/mesh) is the ANCHOR'S OWN view of
// its roster, so it is the observable A11 needs: the cell asks what the
// anchor's view says, and the anchor's view is what the panel serves.
const m4PanelToken = "m4-fixture-panel-token"

// m4Bin builds the real command binary once per package run.
//
// The binary is the product: a test that drives `cli.Run` in-process
// exercises the same functions, but it cannot observe the process's own
// stderr, its exit code, or a signal sent to it. Three of the nine cells
// here are about exactly those.
var (
	m4BinOnce sync.Once
	m4BinPath string
	m4BinErr  error
)

// m4Binary returns the path of the built command binary.
func m4Binary(t *testing.T) string {
	t.Helper()
	m4BinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cmesh-m4-bin")
		if err != nil {
			m4BinErr = err
			return
		}
		bin := filepath.Join(dir, "cmesh-under-test")
		cmd := exec.Command("go", "build", "-o", bin, "github.com/muzimu217/container-mesh/cmd/cm")
		if out, err := cmd.CombinedOutput(); err != nil {
			m4BinErr = fmt.Errorf("build cm: %v\n%s", err, out)
			return
		}
		m4BinPath = bin
	})
	if m4BinErr != nil {
		t.Fatalf("build the command binary: %v", m4BinErr)
	}
	return m4BinPath
}

// m4Result is what one real command process produced.
type m4Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// m4Run runs the real binary and collects both streams and the exit code.
//
// The streams are written into buffers the exec package drains on its own
// goroutines, so a command that writes more than a pipe buffer cannot
// deadlock the test.
func m4Run(t *testing.T, env []string, args ...string) m4Result {
	t.Helper()
	cmd := exec.Command(m4Binary(t), args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := m4Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res
		}
		t.Fatalf("run cmesh %s: %v", strings.Join(args, " "), err)
	}
	return res
}

// syncBuffer is a bytes.Buffer that is safe to write from the reader
// goroutine while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// m4Anchor is a real `serve-discovery` process.
type m4Anchor struct {
	dir     string
	pass    string
	dev     identity.DeviceID
	addr    string
	disc    string
	base    string
	account string
	cmd     *exec.Cmd
	stderr  *syncBuffer
}

// newM4Anchor starts a real anchor process and waits until it is healthy.
//
// The account public key is passed with --account-pub, so the credential
// enrollment endpoint is actually MOUNTED: without it the anchor refuses
// to verify any credential, and the A7/A8/A10 cells would be measuring a
// route that does not exist (the acceptance list's own T2 note says so).
func newM4Anchor(t *testing.T) *m4Anchor {
	t.Helper()
	dir := t.TempDir()
	env := m4PrivateEnv()

	m4Run(t, env, "init", "--state", dir, "--passphrase", m4Pass)
	m4Run(t, env, "account", "create", "--state", dir, "--passphrase", m4Pass)
	m4Run(t, env, "account", "device-add", "--state", dir,
		"--device-passphrase", m4Pass, "--account-passphrase", m4Pass)

	// The account public key, in the hex form `account show` prints. It is
	// read back from the product's OWN output, so the fixture cannot drift
	// from the format the product accepts.
	shown := m4Run(t, env, "account", "show", "--state", dir,
		"--account-passphrase", m4Pass)
	accountPub := ""
	for _, line := range strings.Split(shown.Stdout, "\n") {
		if strings.HasPrefix(line, "pubkey.hex ") {
			accountPub = strings.TrimSpace(strings.TrimPrefix(line, "pubkey.hex "))
		}
	}
	if accountPub == "" {
		t.Fatalf("the anchor's account show printed no pubkey.hex line:\n%s", shown.Stdout)
	}

	addr := m4FreePort(t)
	disc := addr
	args := []string{"serve-discovery", "--state", dir,
		"--device-passphrase", m4Pass, "--addr", addr, "--announce", disc,
		"--account-pub", accountPub,
		"--panel", "--panel-token", m4PanelToken}
	cmd := exec.Command(m4Binary(t), args...)
	cmd.Env = append(os.Environ(), env...)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	a := &m4Anchor{
		dir:     dir,
		pass:    m4Pass,
		addr:    addr,
		disc:    disc,
		base:    "http://" + addr,
		account: accountPub,
		cmd:     cmd,
		stderr:  stderr,
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})

	// The anchor is up when it answers /healthz.
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(a.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the anchor never came up: %v\n%s", err, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.dev = m4LoadDevice(t, dir, m4Pass)
	return a
}

// m4PrivateEnv is the environment every fixture process runs with.
//
// CMESH_ALLOW_PRIVATE_DISCOVERY=1 is the product's own switch for a
// deployment whose anchor is on a private or loopback address. It is set
// for the WHOLE fixture, including the join, because the join is what
// dials the anchor.
func m4PrivateEnv() []string {
	return []string{"CMESH_ALLOW_PRIVATE_DISCOVERY=1"}
}

// m4FreePort returns a free loopback TCP port.
func m4FreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return "127.0.0.1:" + port
}

// m4LoadDevice reads a device ID out of a state directory.
func m4LoadDevice(t *testing.T, dir, pass string) identity.DeviceID {
	t.Helper()
	dev, err := identity.LoadDevice(identity.IdentityPath(dir, identity.DeviceFileName), pass)
	if err != nil {
		t.Fatal(err)
	}
	return dev.ID()
}

// m4Member is a real `join` process.
type m4Member struct {
	dir    string
	dev    identity.DeviceID
	cmd    *exec.Cmd
	stderr *syncBuffer
}

// m4StartMember starts a real `join` process and returns it.
//
// The caller supplies the full argument list, so the same helper serves
// the invite path (A6), the credential path (A7/A8/A10) and the SIGTERM
// path (A11).
func m4StartMember(t *testing.T, dir string, args ...string) *m4Member {
	t.Helper()
	cmd := exec.Command(m4Binary(t), args...)
	cmd.Env = append(os.Environ(), m4PrivateEnv()...)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	m := &m4Member{dir: dir, cmd: cmd, stderr: stderr}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	return m
}

// m4WaitFor polls cond until it holds or the budget runs out.
//
// The wait is on the CONDITION, never on a wall-clock number: the cell's
// budget is about "the view converged", and a fixed sleep would measure
// the machine's mood instead (docs/CONVENTIONS.md T-2.4).
func m4WaitFor(t *testing.T, what string, budget time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, budget)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// A3: `account show` MUST PRINT BOTH PUBLIC-KEY FORMS.
// ---------------------------------------------------------------------------

// TestAccountShowPrintsBothPublicKeyForms pins A3 at the RUNTIME.
//
// The acceptance cell says: stdout 出现 `pubkey.b64 ` / `pubkey.hex ` 两行
// — BOTH lines, on STDOUT.
//
// The old guard (internal/cli/cred_test.go,
// TestAccountShowAcceptsBothPassphraseFlags) checked `strings.Contains(out,
// "pubkey.b64")` — a SUBSTRING of the whole stdout. That passes when only
// the b64 line is printed, and it also passes on the stderr narration
// line if the stdout copy ever went away. The `pubkey.hex` form had NO
// assertion at all: the repository did not even mention it in a test.
//
// The two forms exist for two different consumers: a script that pipes the
// key into `cmesh account authorize --pub` wants base64, and an operator
// comparing the key with the one the anchor prints wants hex. Dropping
// either form is a silent breakage of one of them.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// Delete the `fmt.Printf("pubkey.hex %x\n", acc.PublicKey())` line in
// internal/cli/mesh.go. The old guard stays green (it only looks for
// "pubkey.b64"), and this test goes red.
//
// ★ WHY A REAL PROCESS.
//
// The cell's judgement is about the STREAMS: the key lines go to stdout so
// a script can capture stdout alone, and the narration goes to stderr. A
// test that merges the two streams cannot tell which one carried the key.
// The real binary's two streams are separate, so this test can.
func TestAccountShowPrintsBothPublicKeyForms(t *testing.T) {
	dir := t.TempDir()
	env := m4PrivateEnv()
	m4Run(t, env, "init", "--state", dir, "--passphrase", m4Pass)
	m4Run(t, env, "account", "create", "--state", dir, "--passphrase", m4Pass)

	res := m4Run(t, env, "account", "show", "--state", dir, "--account-passphrase", m4Pass)
	if res.ExitCode != 0 {
		t.Fatalf("account show exited %d:\n%s", res.ExitCode, res.Stderr)
	}

	// Both forms must be on STDOUT, as their own lines.
	var b64Line, hexLine string
	for _, line := range strings.Split(res.Stdout, "\n") {
		switch {
		case strings.HasPrefix(line, "pubkey.b64 "):
			b64Line = line
		case strings.HasPrefix(line, "pubkey.hex "):
			hexLine = line
		}
	}
	if b64Line == "" {
		t.Fatalf("account show printed no \"pubkey.b64 \" line on stdout:\n%s", res.Stdout)
	}
	if hexLine == "" {
		t.Fatalf("account show printed no \"pubkey.hex \" line on stdout.\n"+
			"The A3 cell names BOTH forms — a script and an operator each need "+
			"one, and only the b64 line was guarded.\nstdout:\n%s", res.Stdout)
	}

	// And the two lines must be the SAME key in two encodings. A hex line
	// that carries something else is worse than no hex line at all: an
	// operator would paste it into --account-pub and pin the network to a
	// key nobody holds.
	b64 := strings.TrimSpace(strings.TrimPrefix(b64Line, "pubkey.b64 "))
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("the pubkey.b64 line is not base64: %v (%q)", err, b64)
	}
	hexVal := strings.TrimSpace(strings.TrimPrefix(hexLine, "pubkey.hex "))
	rawHex, err := hex.DecodeString(hexVal)
	if err != nil {
		t.Fatalf("the pubkey.hex line is not hex: %v (%q)", err, hexVal)
	}
	if !bytes.Equal(raw, rawHex) {
		t.Fatalf("the two public-key lines disagree.\npubkey.b64 decodes to %x\n"+
			"pubkey.hex decodes to %x\n(the two forms must be the SAME key)", raw, rawHex)
	}
	if len(raw) != 32 {
		t.Fatalf("the account public key is %d bytes, want 32", len(raw))
	}
}

// ---------------------------------------------------------------------------
// A6: THE INVITE JOIN MUST PRINT ITS PUBKEY AND ITS APPROVAL.
// ---------------------------------------------------------------------------

// TestInviteJoinPrintsThePubkeyAndTheApproval pins A6 at the RUNTIME.
//
// The acceptance cell says:
//
//	join <invite> → 打印 `PUBKEY` → 所有者 `account authorize --pub` →
//	证书落 `certs/` → joiner 打印 `approved: this device is now a member`
//
// The old guard (internal/cli/discovery_test.go,
// TestMeshBootstrapApprovalFlow) proved the approval really LANDED — the
// join returned 0, the roster held two members, the certificate was
// adopted. It asserted NOTHING about the two lines the operator reads. The
// strings "PUBKEY" and "approved: this device is now a member" did not
// appear in any test.
//
// That gap is the operator's whole guide through the manual approval path:
// "PUBKEY" is the value they copy to the account owner, and "approved: this
// device is now a member" is the confirmation they wait for. A join that
// approves silently leaves them waiting for a line that never comes.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/discovery.go, delete the
// `fmt.Fprintf(os.Stderr, "PUBKEY: %s\n\n", ...)` print, or the
// `fmt.Fprintf(os.Stderr, "approved: this device is now a member\n")`
// print. The approval still lands, the roster still holds two members, and
// every existing guard stays green.
func TestInviteJoinPrintsThePubkeyAndTheApproval(t *testing.T) {
	a := newM4Anchor(t)
	env := m4PrivateEnv()

	// The anchor's invite, printed by the real command.
	inviteRes := m4Run(t, env, "invite", "--state", a.dir,
		"--account-passphrase", m4Pass, "--disc", a.disc)
	if inviteRes.ExitCode != 0 {
		t.Fatalf("invite exited %d:\n%s", inviteRes.ExitCode, inviteRes.Stderr)
	}
	invite := strings.TrimSpace(inviteRes.Stdout)
	if !strings.Contains(invite, "cmesh://join?") {
		t.Fatalf("the invite is not a join string:\n%s", inviteRes.Stdout)
	}

	// The joiner's side: a device with an identity of its own.
	joinerDir := t.TempDir()
	m4Run(t, env, "init", "--state", joinerDir, "--passphrase", m4Pass)

	// The join is a real process, and it BLOCKS until the approval lands.
	join := m4StartMember(t, joinerDir, "join", "--state", joinerDir,
		"--device-passphrase", m4Pass, "--timeout", "60s", invite)

	// ① The joiner must print the line the account owner needs.
	m4WaitFor(t, "the joiner printed its PUBKEY line", 30*time.Second, func() bool {
		return strings.Contains(join.stderr.String(), "PUBKEY")
	})
	pubLine := ""
	for _, line := range strings.Split(join.stderr.String(), "\n") {
		if strings.HasPrefix(line, "PUBKEY:") {
			pubLine = strings.TrimSpace(strings.TrimPrefix(line, "PUBKEY:"))
			break
		}
	}
	if pubLine == "" {
		t.Fatalf("the joiner printed no \"PUBKEY:\" line.\nThe A6 cell names it: "+
			"it is the value the operator copies to the account owner.\nstderr:\n%s",
			join.stderr.String())
	}
	if _, err := base64.StdEncoding.DecodeString(pubLine); err != nil {
		t.Fatalf("the PUBKEY line is not base64: %v (%q)", err, pubLine)
	}

	// ② The account owner authorizes that key, and the certificate lands in
	// the anchor's certs/ directory — which is what the anchor's watcher
	// adopts.
	joinerDev := m4LoadDevice(t, joinerDir, m4Pass)
	certPath := filepath.Join(a.dir, "certs", string(joinerDev)+identity.CertSuffix)
	authRes := m4Run(t, env, "account", "authorize", "--state", a.dir,
		"--account-passphrase", m4Pass, "--pub", pubLine, "--out", certPath)
	if authRes.ExitCode != 0 {
		t.Fatalf("account authorize exited %d:\n%s", authRes.ExitCode, authRes.Stderr)
	}

	// ③ The joiner must say it is now a member.
	m4WaitFor(t, "the joiner announced its approval", 60*time.Second, func() bool {
		return strings.Contains(join.stderr.String(),
			"approved: this device is now a member")
	})
	got := join.stderr.String()
	if !strings.Contains(got, "approved: this device is now a member") {
		t.Fatalf("the joiner never said it is a member.\nThe A6 cell names the "+
			"line an operator waits for; a join that approves silently leaves "+
			"them waiting.\nstderr:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// A7: THE CREDENTIAL JOIN MUST SAY IT ENROLLED.
// ---------------------------------------------------------------------------

// TestCredentialJoinSaysItEnrolledWithTheTeamCredential pins A7 at the
// RUNTIME.
//
// The acceptance cell says:
//
//	cmesh join --cred <串> 退出码 0，stderr 含 `Enrolled with a team credential`
//
// The old guard (internal/cli/join_cred_test.go,
// TestJoinWithCredentialAdmitsDevice) proved the ENROLLMENT worked — the
// device is in the roster, the delegation is recorded, the roster survives
// a reload — but it drove `enrollWithCredentialUsing` in process, so it
// never saw the command, its exit code, or its stderr.
//
// The line is what tells an operator the ONE-COMMAND join did the thing it
// promises: without it, a member who ran the command and saw nothing but
// an enrolment id has no way to know the approval step was skipped (there
// IS no approval step on this path — that is the whole point).
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// Delete the `fmt.Fprintf(os.Stderr, "\nEnrolled with a team credential —
// no approval needed.\n\n")` print in internal/cli/discovery.go. The
// enrollment still happens, the roster still grows, the old guard stays
// green — and the operator is left waiting for an approval that will never
// be needed.
func TestCredentialJoinSaysItEnrolledWithTheTeamCredential(t *testing.T) {
	a := newM4Anchor(t)
	env := m4PrivateEnv()

	// A credential the anchor's account issued, pointing at the anchor.
	cred := m4MakeCredential(t, a, 5, time.Hour)

	// The joiner: a device with NO identity yet — the credential path
	// creates one, which is what makes it a one-command join.
	//
	// --once makes the join exit as soon as the approval lands, so the
	// process's EXIT CODE is observable (the acceptance list's own T2
	// note says exactly this: "若要一次性验证入网成功，加 --once").
	joinerDir := t.TempDir()
	res := m4Run(t, env, "join", "--state", joinerDir,
		"--device-passphrase", m4Pass, "--once", "--cred", cred)
	if res.ExitCode != 0 {
		t.Fatalf("join --cred exited %d, want 0.\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "Enrolled with a team credential") {
		t.Fatalf("join --cred did not say it enrolled with a team credential.\n"+
			"The A7 cell names that line as the evidence the one-command join "+
			"worked; without it the operator cannot tell whether an approval is "+
			"still pending.\nstderr:\n%s", res.Stderr)
	}

	// And the device really is a member of the anchor's roster — the line
	// is only worth anything if the enrollment behind it happened.
	roster, err := identity.LoadMembers(filepath.Join(a.dir, identity.MembersFileName))
	if err != nil {
		t.Fatal(err)
	}
	joinerDev := m4LoadDevice(t, joinerDir, m4Pass)
	if !roster.Contains(joinerDev) {
		t.Fatalf("the joiner %s is not in the anchor's roster: the line claimed "+
			"an enrollment that did not happen", joinerDev)
	}
}

// ---------------------------------------------------------------------------
// A8: THE CREDENTIAL QUOTA MUST REFUSE THE DEVICE THAT EXCEEDS IT.
// ---------------------------------------------------------------------------

// TestCredentialQuotaRefusesTheThirdDeviceWithTheNamedReason pins A8 at
// the RUNTIME.
//
// The acceptance cell says:
//
//	用 `--max-devices 2` 的凭证，第 3 台入网被拒，文案含
//	`credential quota exhausted`
//
// The old guard (internal/cli/enroll_test.go,
// TestEnrollRefusesBadRequests/quota_exhausted) called the admission
// function directly and asserted only that the message contains the word
// "quota". It never ran the command, so it never saw:
//
//   - the JOINER's exit code (the cell says the third device is REFUSED);
//   - the joiner's own stderr, which is where the operator reads the
//     reason.
//
// The original text is the point: "credential quota exhausted" says WHICH
// limit was hit. A refusal that says "quota" and leaves it there is
// indistinguishable from the relay's stream quota.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/enroll.go, reword the refusal so the original text is
// gone — e.g. "this credential has no device slots left". The joiner then
// sees a refusal with no named reason, and this test goes red on the
// exact wording the cell names.
func TestCredentialQuotaRefusesTheThirdDeviceWithTheNamedReason(t *testing.T) {
	a := newM4Anchor(t)
	env := m4PrivateEnv()

	// A credential with room for TWO devices.
	cred := m4MakeCredential(t, a, 2, time.Hour)

	// The first two devices are admitted.
	for i := 0; i < 2; i++ {
		dir := t.TempDir()
		res := m4Run(t, env, "join", "--state", dir,
			"--device-passphrase", m4Pass, "--once", "--cred", cred)
		if res.ExitCode != 0 {
			t.Fatalf("device %d was refused: exit %d\n%s", i+1, res.ExitCode, res.Stderr)
		}
	}

	// The THIRD one does not fit.
	dir := t.TempDir()
	res := m4Run(t, env, "join", "--state", dir,
		"--device-passphrase", m4Pass, "--once", "--cred", cred)
	if res.ExitCode == 0 {
		t.Fatalf("the third device joined under a credential with room for two.\n"+
			"stderr:\n%s", res.Stderr)
	}
	if !strings.Contains(res.Stderr, "credential quota exhausted") {
		t.Fatalf("the third device was refused, but not with the reason the "+
			"acceptance cell names.\nwant the stderr to contain: credential quota exhausted\n"+
			"got:\n%s", res.Stderr)
	}
}

// ---------------------------------------------------------------------------
// A10: THE MEMBER MUST SAY IT RENEWED ITS CERTIFICATE.
// ---------------------------------------------------------------------------

// TestCredentialJoinSaysItRenewedItsCertificate pins A10 at the RUNTIME.
//
// The acceptance cell says:
//
//	长跑 `join --cred`，观察 stderr 的 `certificate renewed`
//
// and its own note says how the tester is meant to compress the wait:
// "值守（用 `certRenewIntervalHook` 压缩）".
//
// The old guard (internal/cli/cred_renew_test.go,
// TestTheMemberRenewsItsCertificateBeforeItLapses) proved the RENEWAL
// happened — the roster's certificate moved forward — but it called
// `certRenewLoop` directly. It never ran the JOIN, so it never saw the
// line the operator reads, and it never proved that the JOIN starts the
// renewal loop at all.
//
// ★ WHY THE JOIN IS DRIVEN IN A HELPER PROCESS HERE.
//
// The renewal interval is floored at one hour in production
// (internal/cli/discovery.go: `if interval < time.Hour { interval =
// time.Hour }`), so no real process can renew inside a test's budget. The
// acceptance list anticipates this and names the product's own test hook.
//
// Everything else is real: the anchor is a REAL process, the credential is
// issued by the REAL `cmesh cred create`, the enrollment and the renewal
// are REAL HTTP round trips to the real anchor, and the line is printed by
// the real join code to the real stderr.
//
// ★ WHY A HELPER PROCESS AND NOT AN IN-PROCESS JOIN.
//
// certRenewIntervalHook is a PACKAGE-LEVEL variable, and the renewal
// goroutine READS it on every iteration
// (internal/cli/discovery.go:2193) — and that goroutine OUTLIVES the
// join's Run, because the join never waits for it. So a test that sets
// the hook in-process and later restores it races with the goroutine's
// next read, and the race detector is right to say so.
//
// (The race was found by the race detector on this very test: the
// cleanup's write to certRenewIntervalHook raced with
// certRenewLoop's read at discovery.go:2193.)
//
// A helper process removes the race by construction: the hook is written
// ONCE, before any goroutine exists, in a process that then exits. There
// is no second write and no shared state — nothing for the race detector
// to report, and nothing to reason about.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// Delete the `fmt.Fprintf(os.Stderr, "certificate renewed (the credential
// runs to %s)\n", ...)` print in internal/cli/discovery.go. The renewal
// still happens, the roster still moves forward, and the old guard stays
// green.
func TestCredentialJoinSaysItRenewedItsCertificate(t *testing.T) {
	// The HELPER MODE. The helper re-executes this test binary, so it runs
	// the product's own join code in a process of its own.
	if os.Getenv(m4RenewHelperEnv) == "1" {
		m4RenewHelperMain()
		return
	}

	a := newM4Anchor(t)

	// A SHORT credential: the renewal mints a certificate valid until the
	// CREDENTIAL expires, so a short TTL keeps the whole cycle inside the
	// test's budget.
	cred := m4MakeCredential(t, a, 5, 90*time.Second)

	dir := t.TempDir()
	out, err := m4RunRenewHelper(t, dir, cred)
	if err != nil {
		t.Fatalf("the helper join failed: %v\nstderr:\n%s", err, out)
	}
	if !strings.Contains(out, "certificate renewed") {
		t.Fatalf("the joining member never said it renewed its certificate.\n"+
			"The A10 cell names that line: without it a member whose certificate "+
			"lapses has no warning at all.\nstderr:\n%s", out)
	}
}

// m4RenewHelperEnv names the helper mode.
const m4RenewHelperEnv = "CMESH_M4_RENEW_HELPER"

// m4RenewHelperDirEnv and m4RenewHelperCredEnv carry the helper's inputs.
//
// The credential travels in an ENVIRONMENT variable rather than on the
// command line, because a credential is a secret and the product says so
// (internal/cli/discovery.go: the credential is authority to join, and the
// command line is visible to every local user through /proc).
const (
	m4RenewHelperDirEnv  = "CMESH_M4_RENEW_DIR"
	m4RenewHelperCredEnv = "CMESH_M4_RENEW_CRED"
)

// m4RunRenewHelper re-executes this test binary in the helper mode and
// returns what the helper printed on stderr.
//
// The helper is a SEPARATE PROCESS on purpose: the renewal hook is a
// package-level variable, and the renewal goroutine outlives the join, so
// an in-process test would have to restore the variable while that
// goroutine may still be reading it — a real data race, which the race
// detector found on the first attempt at this test.
//
// In a helper process the hook is written ONCE, before any goroutine
// exists, and the process then exits. There is no second write and no
// shared state.
func m4RunRenewHelper(t *testing.T, dir, cred string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestCredentialJoinSaysItRenewedItsCertificate")
	cmd.Env = append(os.Environ(),
		m4RenewHelperEnv+"=1",
		m4RenewHelperDirEnv+"="+dir,
		m4RenewHelperCredEnv+"="+cred,
		"CMESH_ALLOW_PRIVATE_DISCOVERY=1",
	)
	var stderr syncBuffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// The wait is on the LINE, not on a fixed sleep: the renewal happens
	// when the compressed interval elapses, and how long that takes is the
	// machine's business. The budget is generous because the machine is
	// busy, not because the renewal is slow — the compressed interval is
	// 300 ms.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if strings.Contains(stderr.String(), "certificate renewed") {
			// The line is there. The resident join is stopped with a
			// SIGTERM — the product's own graceful departure.
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			return stderr.String(), nil
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-done
			return stderr.String(), errors.New("the helper join never printed the renewal line")
		}
		select {
		case err := <-done:
			return stderr.String(), err
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// m4RenewHelperMain is the helper's body.
//
// It runs the REAL join: the product's own runJoin, with the renewal
// interval compressed by the product's own test hook — the recipe the
// acceptance list names ("用 certRenewIntervalHook 压缩").
func m4RenewHelperMain() {
	dir := os.Getenv(m4RenewHelperDirEnv)
	cred := os.Getenv(m4RenewHelperCredEnv)
	if dir == "" || cred == "" {
		fmt.Fprintln(os.Stderr, "the helper needs a state directory and a credential")
		os.Exit(2)
	}

	// Both hooks are set ONCE, here, before the join starts any goroutine.
	//
	// The renewal interval is compressed by the product's own test hook —
	// the recipe the acceptance list names ("用 certRenewIntervalHook
	// 压缩"). The lifetime hook is not needed here: the helper is stopped
	// with a SIGTERM, which is the product's own graceful departure path
	// (and the same signal A11 exercises).
	certRenewIntervalHook = func(time.Duration) time.Duration { return 300 * time.Millisecond }

	// The join is resident: it runs until it is interrupted.
	if err := Run([]string{"join", "--state", dir,
		"--device-passphrase", m4Pass, "--cred", cred}); err != nil {
		fmt.Fprintf(os.Stderr, "the helper's join failed: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// A11: SIGTERM TO A REAL JOIN MUST MAKE THE ANCHOR'S VIEW SAY OFFLINE.
// ---------------------------------------------------------------------------

// TestSigtermToAJoinProcessMakesTheAnchorViewSayOffline pins A11 at the
// RUNTIME.
//
// The acceptance cell says:
//
//	对 join 进程发 SIGTERM，锚点视图 ≤3s 变 offline（预算 P-5b）
//
// The old guards (internal/discovery/leave_test.go) proved the OTHER half
// of the path: a LEAVE broadcast reaches a peer's view and converges. But
// the wiring the cell names — "对 join 进程发 SIGTERM" — is in the CLI
// (internal/cli/discovery.go: the signal context, and the `case <-ctx.Done()`
// arm that calls gracefulLeave), and NOTHING asserted it.
//
// ★ TWO ASSERTIONS, TWO DIFFERENT FACTS.
//
// ① The ANCHOR's view says offline, within the budget. That is the cell's
//
//	judgement, and the observable is the anchor's OWN panel data endpoint
//	(GET /api/mesh), which serves the anchor's liveness view.
//
// ② The join process says "left the network (LEAVE broadcast sent)". That
//
//	is the WIRING: it proves the signal reached the graceful-departure
//	path and the LEAVE was really broadcast. Without it, a join that
//	merely died (and whose socket closed) would also make the view say
//	offline — the cell's wording is about the SIGTERM being WIRED to the
//	departure, not about the socket happening to close.
//
// ★ WHY A REAL PROCESS.
//
// A signal is a signal to a PROCESS. The in-process join takes its
// lifetime from the joinLifetimeHook, which REPLACES the signal context —
// so an in-process test cannot exercise the signal path at all. The
// product's own comment says so (internal/cli/discovery.go:71-83).
//
// ★ THE "≤3s" IS A BOUNDED WAIT, NOT A WALL-CLOCK ASSERTION.
//
// The cell's budget is about convergence: the anchor's view says offline.
// The test waits for THAT CONDITION, with a budget, and fails if it does
// not happen. It does not assert that it happened in exactly 3.000 s
// (docs/CONVENTIONS.md T-2.4: a wall-clock threshold is a machine mood,
// not a contract). The elapsed time is logged, not asserted.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/discovery.go, make the `case <-ctx.Done():` arm of the
// join loop return WITHOUT calling gracefulLeave(). Then the join never
// broadcasts the LEAVE, never prints "left the network (LEAVE broadcast
// sent)", and the anchor's view converges only through the slow path (the
// members stream's close, and then the heartbeat horizon).
func TestSigtermToAJoinProcessMakesTheAnchorViewSayOffline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the process-level check uses unix process semantics")
	}
	a := newM4Anchor(t)
	env := m4PrivateEnv()

	// The joiner device.
	dir := t.TempDir()
	m4Run(t, env, "init", "--state", dir, "--passphrase", m4Pass)
	joinerDev := m4LoadDevice(t, dir, m4Pass)

	// The anchor authorizes it, so the device is a member and its
	// announcement is accepted.
	pubRes := m4Run(t, env, "show-pub", "--state", dir, "--device-passphrase", m4Pass)
	pub := strings.TrimSpace(pubRes.Stdout)
	if pub == "" {
		t.Fatalf("show-pub printed nothing:\n%s", pubRes.Stdout)
	}
	certPath := filepath.Join(a.dir, "certs", string(joinerDev)+identity.CertSuffix)
	authRes := m4Run(t, env, "account", "authorize", "--state", a.dir,
		"--account-passphrase", m4Pass, "--pub", pub, "--out", certPath)
	if authRes.ExitCode != 0 {
		t.Fatalf("account authorize exited %d:\n%s", authRes.ExitCode, authRes.Stderr)
	}

	inviteRes := m4Run(t, env, "invite", "--state", a.dir,
		"--account-passphrase", m4Pass, "--disc", a.disc)
	invite := strings.TrimSpace(inviteRes.Stdout)

	// The resident join process.
	join := m4StartMember(t, dir, "join", "--state", dir,
		"--device-passphrase", m4Pass, "--timeout", "120s", invite)

	// The anchor's view must show the member ONLINE first — otherwise
	// "it went offline" would be true from the start and the test would
	// measure nothing.
	m4WaitFor(t, "the anchor's view showed the member online", 60*time.Second, func() bool {
		return m4AnchorOnline(a, joinerDev)
	})
	if !m4AnchorOnline(a, joinerDev) {
		t.Fatalf("the anchor's view never showed %s online:\n%s",
			joinerDev, a.stderr.String())
	}

	// The signal. This is the cell's whole subject.
	if err := join.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM the join process: %v", err)
	}

	// ① The convergence: the anchor's view says offline, within the budget.
	start := time.Now()
	m4WaitFor(t, "the anchor's view showed the member offline", 3*time.Second, func() bool {
		return !m4AnchorOnline(a, joinerDev)
	})
	t.Logf("the anchor's view converged to offline in %s (budget 3s)", time.Since(start))
	if m4AnchorOnline(a, joinerDev) {
		t.Fatalf("the anchor's view still shows %s online 3s after SIGTERM",
			joinerDev)
	}

	// ② The wiring: the SIGTERM reached the graceful-departure path, and
	// the LEAVE was really broadcast.
	m4WaitFor(t, "the join reported its graceful departure", 10*time.Second, func() bool {
		return strings.Contains(join.stderr.String(), "left the network (LEAVE broadcast sent)")
	})
	if !strings.Contains(join.stderr.String(), "left the network (LEAVE broadcast sent)") {
		t.Fatalf("the join process did not report a graceful departure after SIGTERM.\n"+
			"The A11 cell is about the SIGNAL being wired to the departure: a join "+
			"that merely dies leaves the anchor to notice the dead socket, which is "+
			"the slow path the cell rules out.\nstderr:\n%s", join.stderr.String())
	}
}

// m4AnchorOnline asks the anchor's OWN panel API whether a member is
// online.
//
// The panel's data endpoint is the anchor's own view of its roster — the
// same liveness state the members stream pushes — so it is the anchor's
// answer, not the test's guess.
func m4AnchorOnline(a *m4Anchor, id identity.DeviceID) bool {
	req, err := http.NewRequest(http.MethodGet, a.base+"/api/mesh", nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Panel-Token", m4PanelToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var doc struct {
		Members []struct {
			DeviceID string `json:"device_id"`
			Online   bool   `json:"online"`
		} `json:"members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return false
	}
	for _, m := range doc.Members {
		if m.DeviceID == string(id) {
			return m.Online
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// THE LIVE MEMBERS: TWO REAL MEMBERS WITH REAL DIRECT-LINK ENDPOINTS.
//
// The B cells need a LIVE session with a peer that has a dialable
// endpoint: `status` must list it, `status --probe` must reach it, and
// `mesh ping` must take the DIRECT path to it.
//
// Each member joins with --listen 127.0.0.1:0, so it binds a real UDP
// socket and announces the bound port — which is exactly the shape the
// acceptance list's own T1/T5 commands describe.
// ---------------------------------------------------------------------------

// m4LiveMember is one live member of the live fixture.
type m4LiveMember struct {
	dir    string
	dev    identity.DeviceID
	member *m4Member
}

// newM4LiveMembers starts the anchor and n live members on it.
//
// Each member is authorized BEFORE its join starts, so the join's very
// first announce is accepted and the member becomes visible at once.
func newM4LiveMembers(t *testing.T, n int) (*m4Anchor, []*m4LiveMember) {
	t.Helper()
	a := newM4Anchor(t)
	env := m4PrivateEnv()

	inviteRes := m4Run(t, env, "invite", "--state", a.dir,
		"--account-passphrase", m4Pass, "--disc", a.disc)
	if inviteRes.ExitCode != 0 {
		t.Fatalf("invite exited %d:\n%s", inviteRes.ExitCode, inviteRes.Stderr)
	}
	invite := strings.TrimSpace(inviteRes.Stdout)

	out := make([]*m4LiveMember, 0, n)
	for i := 0; i < n; i++ {
		dir := t.TempDir()
		m4Run(t, env, "init", "--state", dir, "--passphrase", m4Pass)
		dev := m4LoadDevice(t, dir, m4Pass)

		pubRes := m4Run(t, env, "show-pub", "--state", dir, "--device-passphrase", m4Pass)
		pub := strings.TrimSpace(pubRes.Stdout)
		if pub == "" {
			t.Fatalf("show-pub printed nothing:\n%s", pubRes.Stdout)
		}
		certPath := filepath.Join(a.dir, "certs", string(dev)+identity.CertSuffix)
		authRes := m4Run(t, env, "account", "authorize", "--state", a.dir,
			"--account-passphrase", m4Pass, "--pub", pub, "--out", certPath)
		if authRes.ExitCode != 0 {
			t.Fatalf("account authorize exited %d:\n%s", authRes.ExitCode, authRes.Stderr)
		}

		// --listen 127.0.0.1:0 binds a real UDP socket; the join announces
		// the port it actually bound.
		member := m4StartMember(t, dir, "join", "--state", dir,
			"--device-passphrase", m4Pass, "--listen", "127.0.0.1:0",
			"--timeout", "120s", invite)
		out = append(out, &m4LiveMember{dir: dir, dev: dev, member: member})
	}

	// Every member must be a member of the network before the B cells
	// measure anything.
	for i, m := range out {
		m4WaitFor(t, fmt.Sprintf("member %d announced its approval", i+1), 90*time.Second, func() bool {
			return strings.Contains(m.member.stderr.String(),
				"approved: this device is now a member")
		})
	}
	return a, out
}

// m4StatusRow returns the status line of one member.
//
// The status output is one line per member, and the line is what the cell
// judges.
func m4StatusRow(out string, id identity.DeviceID) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, string(id)) {
			return line
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// B1: `cmesh status` ON A LIVE SESSION.
// ---------------------------------------------------------------------------

// TestStatusOnALiveSessionListsMembersOnlineEndpointsAndLastSeen pins B1 at
// the RUNTIME.
//
// The acceptance cell says:
//
//	cmesh status 退出码 0，列出成员、online、endpoints、last_seen
//
// The old guard (internal/cli/status_test.go,
// TestStatusReportJSONIsStableAndKeepsOfflineMembers) serialized a
// HAND-WRITTEN report struct. It proved the struct has the fields — it
// proved nothing about the command. No Go test had ever run `cmesh status`
// against a live session.
//
// ★ WHY A REAL PROCESS AND A LIVE SESSION.
//
// The cell's judgement is about the command's own output: the exit code,
// the member list, and the per-member fields. A live session is required
// because the fields are only printed when the member view actually
// carries them: `endpoints=` is printed only for a member that announced
// endpoints, and `last_seen=` only for a member the anchor has seen.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/status.go, drop the `endpoints=`/`last_seen=` parts of
// the per-member line. The report struct keeps its fields (the old guard
// stays green) and the command still exits 0 — but the operator's view
// has lost the two things the cell names.
func TestStatusOnALiveSessionListsMembersOnlineEndpointsAndLastSeen(t *testing.T) {
	a, members := newM4LiveMembers(t, 2)

	res := m4Run(t, m4PrivateEnv(), "status",
		"--state", members[0].dir, "--device-passphrase", m4Pass, "--timeout", "15s")
	if res.ExitCode != 0 {
		t.Fatalf("cmesh status exited %d, want 0.\nstdout:\n%s\nstderr:\n%s",
			res.ExitCode, res.Stdout, res.Stderr)
	}

	// The header line: the account, the view version and the online count.
	if !strings.Contains(res.Stdout, "account: ") || !strings.Contains(res.Stdout, "online: ") {
		t.Fatalf("the status output has no account/online header:\n%s", res.Stdout)
	}

	// Every member of the network must be listed, including the anchor.
	for _, id := range []identity.DeviceID{a.dev, members[0].dev, members[1].dev} {
		if m4StatusRow(res.Stdout, id) == "" {
			t.Fatalf("the status output does not list the member %s:\n%s", id, res.Stdout)
		}
	}

	// The PEER's row is the one that must carry the whole contract: it is
	// online, it announced endpoints, and the anchor has seen it.
	peer := m4StatusRow(res.Stdout, members[1].dev)
	if peer == "" {
		t.Fatalf("the status output has no row for the peer %s:\n%s", members[1].dev, res.Stdout)
	}
	for _, want := range []string{"online", "endpoints=", "last_seen="} {
		if !strings.Contains(peer, want) {
			t.Fatalf("the peer's row does not carry %q.\nThe B1 cell names members, "+
				"online, endpoints and last_seen — the operator reads them on this "+
				"line.\nrow: %s\nfull output:\n%s", want, peer, res.Stdout)
		}
	}
}

// ---------------------------------------------------------------------------
// B2: `status --probe` MUST REPORT A REAL REACHABILITY.
// ---------------------------------------------------------------------------

// TestStatusProbeOnALivePeerReportsDirectReachability pins B2 at the
// RUNTIME.
//
// The acceptance cell says:
//
//	`status --probe <id>` → `reachability=direct` 或 `relay`；**无 probe 时
//	必须是 `unknown`，不得伪造**
//
// The old guard (internal/cli/status_test.go,
// TestStatusWithoutProbeRemainsUnknown) tested a PURE FUNCTION with an
// empty member view: it proved that a snapshot with no evidence says
// "unknown". It never probed anything — `reachability=` did not appear in
// any assertion in the repository.
//
// The other half of the cell ("no probe ⇒ unknown") is already guarded by
// the old test and by the live status test above; this test covers the
// half that was missing: a probe against a REAL peer.
//
// ★ WHY A REAL PEER AND A REAL PROCESS.
//
// The probe opens a real authenticated QUIC session to the peer and counts
// the replies. Only a real session can produce "direct" — and only a real
// session can produce "degraded" if the peer is not reachable.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/status.go, make probeMember always return
// reachabilityUnknown (or return "unknown" instead of reachabilityDirect
// after a successful probe). The old guard stays green — it only ever
// checked the no-probe case — and this test goes red.
func TestStatusProbeOnALivePeerReportsDirectReachability(t *testing.T) {
	_, members := newM4LiveMembers(t, 2)

	res := m4Run(t, m4PrivateEnv(), "status",
		"--state", members[0].dir, "--device-passphrase", m4Pass,
		"--probe", members[1].dev.String(), "--timeout", "20s", "--probe-timeout", "15s")
	if res.ExitCode != 0 {
		t.Fatalf("cmesh status --probe exited %d, want 0.\nstdout:\n%s\nstderr:\n%s",
			res.ExitCode, res.Stdout, res.Stderr)
	}

	peer := m4StatusRow(res.Stdout, members[1].dev)
	if peer == "" {
		t.Fatalf("the status output has no row for the probed peer %s:\n%s",
			members[1].dev, res.Stdout)
	}
	// The peer has a real UDP endpoint, so the direct path is the one the
	// probe must take.
	if !strings.Contains(peer, "reachability=direct") {
		t.Fatalf("the probed peer's row does not say reachability=direct.\n"+
			"The B2 cell says the probe must report `direct` (or `relay`) — a peer "+
			"with a dialable endpoint that is reported as anything else means the "+
			"probe invented or hid its result.\nrow: %s\nfull output:\n%s",
			peer, res.Stdout)
	}
	// And it must not be the fabricated "unknown": the probe produced real
	// transport evidence.
	if strings.Contains(peer, "reachability=unknown") {
		t.Fatalf("the probed peer is still reported as reachability=unknown.\n"+
			"The probe reached a real member, so the evidence exists and must be "+
			"reported.\nrow: %s", peer)
	}
}

// ---------------------------------------------------------------------------
// B3: THE DIRECT PING MUST REPORT ITS LOSS, ITS PERCENTILE AND ITS LINK.
// ---------------------------------------------------------------------------

// TestDirectMeshPingReportsLossPercentileAndLink pins B3 at the RUNTIME.
//
// The acceptance cell says:
//
//	--- <id>: N/N pings, lost 0 (0.0%), p50 …, link=direct
//
// The old guard (internal/cli/discovery_test.go,
// TestMeshBootstrapApprovalFlow) asserted only that the output contains
// "5/5 pings" and "p95". The three fragments the cell NAMES — "lost 0
// (0.0%)", "p50" and "link=direct" — had no assertion anywhere in the
// repository: `link=direct` did not appear in a single test.
//
// "link=direct" is the load-bearing one: it is what tells the operator
// the traffic did NOT go through a relay, and a ping that silently took
// the trunk would look like a direct ping with a worse RTT.
//
// ★ WHY A REAL PROCESS.
//
// The line is the command's own stdout, and the ping is a real exchange
// with a real peer over a real direct link.
//
// ★ THE MUTATION THAT MAKES THIS RED.
//
// In internal/cli/meshcmd.go, make the link class unconditional
// (`linkClass = "direct"`), or report "relay" for a direct path. The old
// guard stays green — it only looked for "5/5 pings" and "p95" — and this
// test goes red.
func TestDirectMeshPingReportsLossPercentileAndLink(t *testing.T) {
	_, members := newM4LiveMembers(t, 2)

	res := m4Run(t, m4PrivateEnv(), "mesh", "ping",
		"--state", members[0].dir, "--device-passphrase", m4Pass,
		"--count", "5", "--interval", "150ms", "--timeout", "30s",
		members[1].dev.String())
	if res.ExitCode != 0 {
		t.Fatalf("cmesh mesh ping exited %d, want 0.\nstdout:\n%s\nstderr:\n%s",
			res.ExitCode, res.Stdout, res.Stderr)
	}

	// The summary line is the one that carries the pings.
	summary := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.Contains(line, "pings, lost") {
			summary = line
			break
		}
	}
	if summary == "" {
		t.Fatalf("the ping printed no summary line:\n%s", res.Stdout)
	}
	for _, want := range []string{"5/5 pings", "lost 0 (0.0%)", "p50", "link=direct"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("the ping's summary line does not carry %q.\n"+
				"The B3 cell names the whole line: the loss, the percentile and the "+
				"link class.\nsummary: %s\nfull output:\n%s", want, summary, res.Stdout)
		}
	}
}

// ---------------------------------------------------------------------------
// THE CREDENTIAL HELPER.
// ---------------------------------------------------------------------------

// m4MakeCredential issues a team credential for the anchor's account,
// pointing at the anchor's own discovery address.
//
// The credential is made by the PRODUCT (`cmesh cred create`), so the
// fixture uses the same string an operator would hand out.
func m4MakeCredential(t *testing.T, a *m4Anchor, maxDevices int, ttl time.Duration) string {
	t.Helper()
	res := m4Run(t, m4PrivateEnv(), "cred", "create", "--state", a.dir,
		"--account-passphrase", m4Pass, "--disc", a.disc,
		"--ttl", ttl.String(), "--max-devices", fmt.Sprint(maxDevices))
	if res.ExitCode != 0 {
		t.Fatalf("cred create exited %d:\n%s", res.ExitCode, res.Stderr)
	}
	cred := strings.TrimSpace(res.Stdout)
	if cred == "" {
		t.Fatal("cred create printed no credential")
	}
	if _, err := credential.Decode(cred); err != nil {
		t.Fatalf("the printed credential does not decode: %v", err)
	}
	return cred
}
