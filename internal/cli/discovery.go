// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// Mesh discovery commands: invite / join / serve-discovery.
//
// The three commands are the R2 bootstrap story (O-3/O-5/O-6):
//
//	first machine:   init + account create + account device-add,
//	                 then serve-discovery (it bootstraps an empty member
//	                 list containing itself and becomes the discovery node)
//	other machines:  init + account device-add (needs the account owner's
//	                 help for the cert) — or, for the same operator, copy
//	                 the state dir — then join <invite>
//	approval:        an existing member adds the new device's certificate
//	                 to the member list on the discovery host; serve-discovery
//	                 hot-reloads the file and pushes the new view (R-1.5)
//
// The invite itself carries no secret (R-1.5): possession lets a device ASK,
// membership is what lets it in.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/muzimu217/container-mesh/internal/cmeshtest"
	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/mesh"
	"github.com/muzimu217/container-mesh/internal/relay"
	"github.com/muzimu217/container-mesh/internal/wsconn"
)

// discoveryClientTransportHook is intentionally nil in production. CLI tests
// use it to keep a logical discovery origin in invites while routing requests
// to an in-process listener; the discovery package still validates the logical
// address before either transport is called.
type discoveryClientTransport struct {
	httpClient    *http.Client
	webSocketDial func(context.Context, wsconn.DialOptions) (*wsconn.Conn, error)
}

// joinResolverHook is intentionally nil in production. CLI tests use it to
// observe the resolver a join built, so they can assert on the member view
// that join actually got — the one the relay trunk join pins the trunk's
// certificate against.
var joinResolverHook func(*liveViewResolver)

// joinLifetimeHook is intentionally nil in production. CLI tests use it to
// give an in-process join a LIFETIME they control.
//
// It exists because an APPROVED join is RESIDENT: it runs until it is
// interrupted, so a test that starts one in-process has to be able to end
// it. A real SIGINT would hit the whole test binary (every other join in
// the process would leave the network too), so the test supplies its own
// context instead.
//
// The hook REPLACES the signal context: a test that uses it gives up the
// Ctrl-C handling, which is fine — the signal path has its own
// process-level acceptance test.
var joinLifetimeHook func() (context.Context, context.CancelFunc)

// inboundMessageHook is intentionally nil in production. CLI tests use it
// to observe a message that ARRIVED at this node.
//
// ★ WHY IT EXISTS. (Round 104.)
//
// The M4 acceptance cell B5 says a message must be "delivered" to the
// target — and the sender's own line ("delivered N message(s) to X")
// is the sender's CLAIM about itself. A command that printed that line
// while delivering nothing would satisfy every existing check.
//
// So the test needs the RECEIVER's side of the fact. The receiver
// hands a message to its OnMessage callback; that callback prints to
// stderr, and a test cannot capture another goroutine's stderr without
// racing every other test in the package.
//
// This hook is the seam: it is called with the same arguments the
// print would have used, so a test can observe the arrival without
// touching the process's streams.
//
// It follows the package's existing convention (joinResolverHook,
// joinLifetimeHook, discoveryClientTransportHook): nil in production,
// set by tests.
var inboundMessageHook func(from identity.DeviceID, text string)

var discoveryClientTransportHook discoveryClientTransport

func withDiscoveryClientTransport(cfg discovery.ClientConfig) discovery.ClientConfig {
	if discoveryClientTransportHook.httpClient != nil {
		cfg.HTTPClient = discoveryClientTransportHook.httpClient
	}
	if discoveryClientTransportHook.webSocketDial != nil {
		cfg.WebSocketDial = discoveryClientTransportHook.webSocketDial
	}
	return cfg
}

// discoveryEgressPolicy is the outbound address policy for discovery
// connections: a name that resolves to a loopback, private or reserved
// address is refused, because the discovery origin is an operator-supplied
// value and pointing it at the host's own network is an SSRF shape.
//
// A binary built with the cmeshtest tag drops the policy. The end-to-end
// suite needs it: it runs the whole product against a listener on
// 127.0.0.1 through the logical name cmesh-test.127.0.0.1.nip.io. The
// release build, the CI build and a plain `go build ./cmd/cm` never set the
// tag, so the shipped binary has no such escape.
func discoveryAllowLocal() bool {
	return cmeshtest.Enabled
}

// discoveryAllowPrivate reports whether the operator asked for a PRIVATE
// deployment.
//
// A private deployment is one where the anchor has no public address: it
// lives on a LAN and is reached by a private address (10.x, 192.168.x) or
// by a name that resolves to one. The default outbound policy refuses
// those addresses, because a discovery origin that resolves to a loopback
// or a private address is an SSRF vector — the round-56 finding closed
// that hole.
//
// The switch reopens the door for the operator who explicitly says this
// is their own network, and only for them: it is an environment variable
// rather than a flag because it has to apply to EVERY command that dials
// the anchor (join, msg, send-to, run, ping), and each of those parses
// its own flags.
//
// It is narrower than the test-only escape hatch (AllowLocalDiscovery):
// the reserved and documentation ranges stay refused even with it, and the
// relaxation applies to private, loopback and link-local addresses only.
//
//	CMESH_ALLOW_PRIVATE_DISCOVERY=1
func discoveryAllowPrivate() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CMESH_ALLOW_PRIVATE_DISCOVERY"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// resolvePanelToken decides the panel's data-endpoint token.
//
// The order is: the flag, then the ENVIRONMENT, then a generated one.
//
// The environment fallback matters for SECURITY, not for convenience: a
// token on the command line is visible to every local user through
// /proc/<pid>/cmdline, and the panel's data endpoint hands out the whole
// roster. Measured on the real machine: with the token on the command
// line, an unprivileged local user read it out of /proc and pulled the
// full roster with it. A systemd unit can pass the token in the
// environment instead, where only the account that owns the process can
// read it.
//
// The second return value says whether the token was GENERATED here.
// A generated token is printed once (the operator has no other way to
// learn it); a supplied one is not (it is already in the operator's
// hands, and printing it would put it into the journal).
func resolvePanelToken(flagValue string) (string, bool) {
	tok, generated, err := resolvePanelTokenFrom(flagValue, "")
	if err != nil {
		// resolvePanelToken is the no-file form: there is no file to
		// misconfigure, so it cannot fail.
		panic("cmesh: panel token resolution failed without a token file: " + err.Error())
	}
	return tok, generated
}

// resolvePanelTokenFrom decides the panel's data-endpoint token.
//
// The order is: the file, the flag, the ENVIRONMENT, then a generated
// one.
//
// The FILE comes first because it is the only source that keeps the
// token out of BOTH the command line and the process environment. A
// token on the command line is visible to every local user through
// /proc/<pid>/cmdline; a token in the environment is visible to every
// process running as the same user (the service's own children
// included). systemd's LoadCredential puts the file in a private
// directory that only the service's own process can read, so neither
// applies.
//
// Measured on the real machine: with the token on the command line, an
// unprivileged local user read it and pulled the full roster with it.
//
// A file that was CONFIGURED but cannot be read — or that holds nothing
// — is an ERROR, never a fall-through.
//
// It used to fall through, and that is worse here than it is for the
// credential: the operator configured --panel-token-file, the file was
// missing or empty, and the node started with a DIFFERENT token — the
// environment's, the flag's, or a freshly generated one. The dashboard's
// saved token then silently stopped working, and the operator's panel
// went blank with no error anywhere. A token in the ENVIRONMENT is a
// DOWNGRADE, not a clean zero: the value is there, but every process
// running as the same user can read it out of /proc/<pid>/environ — so
// falling back to it is not "no token", it is "a token with a worse
// exposure", and the operator never asked for that.
func resolvePanelTokenFrom(flagValue, fileValue string) (string, bool, error) {
	if fileValue != "" {
		tok, err := readTokenFile(fileValue)
		if err != nil {
			return "", false, fmt.Errorf("read the panel token file %s: %w", fileValue, err)
		}
		if tok == "" {
			return "", false, fmt.Errorf(
				"the panel token file %s is empty: the token opens the whole roster, "+
					"and an empty file must not fall back to another source", fileValue)
		}
		return tok, false, nil
	}
	if tok := strings.TrimSpace(flagValue); tok != "" {
		return tok, false, nil
	}
	if tok := strings.TrimSpace(os.Getenv("CMESH_PANEL_TOKEN")); tok != "" {
		return tok, false, nil
	}
	// Generated, not defaulted: a fixed or guessable token would make
	// the roster readable by anyone who finds the port.
	return randomToken(), true, nil
}

// readTokenFile reads a token from a file.
//
// The file is read whole and trimmed: systemd's credential files carry
// the value with a trailing newline.
func readTokenFile(path string) (string, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(blob)), nil
}

// resolveCredential decides where the team credential comes from.
//
// The order is: the FILE, the FLAG, the ENVIRONMENT.
//
// The FILE comes first for the same reason the panel token's file does:
// it is the only source that keeps the credential out of BOTH the
// command line and the process environment.
//
// This matters more for the credential than for the panel token. The
// credential IS the authority to join the network — it is not a
// read-only key. The panel token only opens the roster for reading;
// the credential lets a holder mint a device certificate under the
// account. So:
//
//   - on the command line → every local user reads it through
//     /proc/<pid>/cmdline (measured on the real machine: `ps` printed
//     the full credential string);
//   - in the environment → every process running as the same user
//     reads it (the service's own children included).
//
// systemd's LoadCredential puts the file in a private directory that
// only the service's own process can read, so neither applies.
//
// The FLAG beats the ENVIRONMENT: a flag on the command line is a
// deliberate choice by the operator who typed it, while an ambient
// variable may have been exported for a completely different purpose —
// a wrapper script, a CI job, a shell profile — and says nothing about
// what THIS command should do.
func resolveCredential(flagValue, fileValue string) (string, error) {
	return resolveCredentialFrom(flagValue, fileValue, true)
}

// resolveCredentialFrom is resolveCredential with the environment
// switchable.
//
// useEnv says whether the ambient environment may be consulted AT ALL.
// The join turns it off on the invite path, and that is the whole point
// of the switch: an exported CMESH_CRED must not decide which way the
// operator is entering the network.
//
// It used to. The credential was resolved before the "either a
// credential or an invite" check, so on a host that exported CMESH_CRED
// globally — a wrapper script, a CI runner, a shell profile — `cmesh
// join <invite>` failed with "pass either --cred or an invite string,
// not both". The operator's invite was perfectly good; the ambient
// variable broke it.
//
// A FILE that was configured but cannot be read — or that holds nothing
// — is an ERROR, never a fall-through. A fall-through does not just hide
// the mistake: it joins the network with a DIFFERENT credential than the
// operator asked for, and the operator never finds out. The empty file
// is the sharpest case: the secret was truncated or rotated away, and
// the join would quietly use the ambient credential (or none).
func resolveCredentialFrom(flagValue, fileValue string, useEnv bool) (string, error) {
	if fileValue != "" {
		tok, err := readTokenFile(fileValue)
		if err != nil {
			return "", fmt.Errorf("read the credential file %s: %w", fileValue, err)
		}
		if tok == "" {
			return "", fmt.Errorf(
				"the credential file %s is empty: the credential IS the authority to join, "+
					"and an empty file must not fall back to another source", fileValue)
		}
		return tok, nil
	}
	if tok := strings.TrimSpace(flagValue); tok != "" {
		return tok, nil
	}
	if useEnv {
		// The environment is the LAST source, and it is a DOWNGRADE, not a
		// clean zero: the token is there, but every process running as the
		// same user can read it out of /proc/<pid>/environ.
		if tok := strings.TrimSpace(os.Getenv("CMESH_CRED")); tok != "" {
			return tok, nil
		}
	}
	return "", nil
}

// randomToken returns a fresh panel token.
//
// A token is a bearer secret for the roster, so it is generated from the
// system CSPRNG and never derived from anything guessable.
func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// The process cannot proceed with a predictable roster token.
		panic(fmt.Sprintf("cmesh: cannot read randomness for the panel token: %v", err))
	}
	return hex.EncodeToString(b)
}

// portOf returns the port part of a listen address (":8080" → "8080").
func portOf(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return "8080"
}

// stringSlice is a repeatable string flag (--disc a --disc b).
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

// runInvite implements `cmesh invite`: render the join string from the local
// account. Needs the account key (for the public half) and at least one
// discovery address to point joiners at.
func runInvite(args []string) error {
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("account-passphrase", "", "passphrase for the account file (for scripting)")
	var disc stringSlice
	fs.Var(&disc, "disc", "discovery node address(es), repeatable or comma-separated (host:port)")
	ttl := fs.Duration("invite-ttl", 24*time.Hour, "how long the invite stays usable as printed advice (advisory only in this release; enforced expiry is the O-5 follow-up)")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if len(disc) == 0 {
		return ExitError{Code: 2, Err: errors.New(
			"invite needs at least one discovery address: --disc host:port (the address joiners will dial)")}
	}

	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	accPath := identity.IdentityPath(dir, identity.AccountFileName)
	if _, err := os.Stat(accPath); err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("%s not found; run cmesh account create first", accPath)}
	}
	phrase, err := readPassphrase(*pass, "account passphrase")
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	acc, err := identity.LoadAccount(accPath, phrase)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	inv := discovery.Invite{
		AccountID:      acc.ID(),
		AccountPub:     acc.PublicKey(),
		DiscoveryAddrs: []string(disc),
		IssuedAt:       time.Now(),
	}
	// The addresses are VALIDATED before the invite is printed.
	//
	// Without this the invite could name an address that no joiner can
	// ever reach ("malformed endpoint" on the far side), and the
	// operator only found out when a joiner failed — the invite had
	// looked perfectly fine while it was being handed out.
	//
	// The private-deployment switch applies here for the same reason it
	// applies to the invite's own decoding: a LAN anchor is a legitimate
	// anchor, and the operator has said so with the switch.
	s, err := discovery.EncodeInvite(inv)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	// Round-trip it through the decoder: that is the joiner's own view of
	// the invite, so if the joiner would refuse it, the operator finds
	// out HERE rather than when a device fails to join.
	if _, err := discovery.DecodeInviteWith(s, discoveryAllowPrivate()); err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf(
			"the discovery address cannot be used by a joiner: %w", err)}
	}
	fmt.Fprintf(os.Stderr, "invite for account %s (valid advice for ~%s):\n\n", acc.ID(), *ttl)
	fmt.Println(s)
	fmt.Fprintf(os.Stderr, "\nPossession of this string does NOT grant access (R-1.5): after the joiner\nruns `cmesh join`, add their certificate on a member machine first.\n")
	return nil
}

// discoveryConfig is the persisted join state, so later commands (dial, the
// future daemon) can re-derive the same discovery setup without re-inviting.
//
// AccountID/AccountPub/DiscoveryAddrs are what later commands actually need.
// They are derived ONCE, at join time, from whichever entry path was used —
// so the saved file no longer has to encode "was this an invite or a
// credential?" and no reader has to re-parse the right syntax.
//
// Invite is the raw string, kept for diagnostics and backward compatibility.
// It is NOT the source of truth: `join --cred` stores a cmesh1. credential
// there, and DecodeInvite only understands cmesh://join?... — re-parsing it
// made every follow-up command fail with "saved invite is malformed"
// (oh-no-agent, defect B-3: a credential-joined device could not ping, msg,
// send-to or run at all).
type discoveryConfig struct {
	Invite         string    `json:"invite,omitempty"`
	AccountID      string    `json:"account_id,omitempty"`
	AccountPub     []byte    `json:"account_pub,omitempty"`
	DiscoveryAddrs []string  `json:"discovery_addrs,omitempty"`
	Endpoints      []string  `json:"endpoints,omitempty"`
	SavedAt        time.Time `json:"saved_at"`
}

// invite rebuilds the invite view later commands need.
//
// Files written before account_id/discovery_addrs existed carry only the raw
// string, so that case still decodes — it is the only case that needs to.
func (c discoveryConfig) invite() (discovery.Invite, error) {
	if c.AccountID != "" && len(c.DiscoveryAddrs) > 0 {
		return discovery.Invite{
			AccountID:      identity.DeviceID(c.AccountID),
			AccountPub:     c.AccountPub,
			DiscoveryAddrs: c.DiscoveryAddrs,
		}, nil
	}
	if c.Invite == "" {
		return discovery.Invite{}, errors.New("this device has no saved discovery configuration")
	}
	// The private-deployment switch applies here too: an invite saved
	// before the switch was set still names a private anchor, and the
	// later commands (ping, msg, send-to) must be able to reach it.
	return discovery.DecodeInviteWith(c.Invite, discoveryAllowPrivate())
}

// parsePubKeyString accepts a 32-byte public key as either base64 (what
// show-pub prints) or hex (what account show prints). Both encodings were
// live in the docs with different commands expecting different ones, which
// made the documented flow fail on contact (issue #8 D-2); accepting both
// removes the trap without breaking either output.
func parsePubKeyString(s string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s)); err == nil && len(raw) == ed25519.PublicKeySize {
		return raw, nil
	}
	if raw, err := hex.DecodeString(strings.TrimSpace(s)); err == nil && len(raw) == ed25519.PublicKeySize {
		return raw, nil
	}
	return nil, fmt.Errorf("must be base64 or hex of a %d-byte public key", ed25519.PublicKeySize)
}

// liveViewResolver adapts the client's live, verified snapshot to the mesh
// package's MemberResolver: keys come from the CURRENT view, so revocation
// takes effect on the direct-link listener within one push (a removed
// member's certificate no longer pins). The client pointer is atomic because
// the listener's accept goroutines read it while the main goroutine creates
// the client (the listener must bind BEFORE the client, so its resolved
// ports are what join announces).
type liveViewResolver struct {
	ptr        atomic.Pointer[discovery.Client]
	accountPub []byte
}

func (r *liveViewResolver) set(cl *discovery.Client) { r.ptr.Store(cl) }

func (r *liveViewResolver) KeyFor(id identity.DeviceID) (ed25519.PublicKey, error) {
	cl := r.ptr.Load()
	if cl == nil {
		return nil, errors.New("mesh: no verified member view yet")
	}
	snap := cl.Snapshot()
	if snap == nil {
		return nil, errors.New("mesh: no verified member view yet")
	}
	return mesh.SnapshotAdapter{Snap: snap, AccountPub: ed25519.PublicKey(r.accountPub)}.KeyFor(id)
}

// StillMember reports whether the device is still in the current view.
//
// It is the NARROW question ("is it still a member?"), and it exists
// because KeyFor answers a wider one — it also fails for an EXPIRED
// certificate.
//
// Without this method the revocation watch on the direct path falls
// back to KeyFor, and a member whose certificate merely lapsed has its
// live session destroyed and is told it was revoked. That was a real
// defect, and it is worth recording HOW it survived my own fix: the
// capability existed on the mesh side (SnapshotAdapter), but this
// wrapper did not forward it — so **every real member (the production
// shape) answered the wide question**, and only the TEST FIXTURE
// answered the narrow one.
//
// The forwarding has to be explicit: Go's type assertion sees the
// wrapper's method set, not the wrapped adapter's.
func (r *liveViewResolver) StillMember(id identity.DeviceID) bool {
	cl := r.ptr.Load()
	if cl == nil {
		return false
	}
	snap := cl.Snapshot()
	if snap == nil {
		return false
	}
	return mesh.SnapshotAdapter{Snap: snap, AccountPub: ed25519.PublicKey(r.accountPub)}.StillMember(id)
}

// relayMembersResolver pins direct-link certificates against the relay's
// LIVE member list — the serve-discovery listener must follow hot reloads,
// or an approved device could not connect until process restart.
type relayMembersResolver struct{ srv *relay.Server }

func (r relayMembersResolver) KeyFor(id identity.DeviceID) (ed25519.PublicKey, error) {
	return mesh.MemberListAdapter{List: r.srv.MeshMembers()}.KeyFor(id)
}

// StillMember reports whether the device is still in the LIVE member
// list, regardless of whether its certificate has expired.
//
// The relay trunk's revocation watch asks the NARROW question, so this
// wrapper has to forward the capability — **Go's type assertion sees
// the wrapper's method set, not the wrapped adapter's**. Without it the
// watch falls back to KeyFor, whose error also covers "certificate
// expired", and a member whose certificate merely lapsed would have
// its relay flow torn down and be told it was revoked.
func (r relayMembersResolver) StillMember(id identity.DeviceID) bool {
	return mesh.MemberListAdapter{List: r.srv.MeshMembers()}.StillMember(id)
}

// memberEndpoints reports the addresses a member announced for direct
// connections.
func memberEndpoints(srv *relay.Server, id identity.DeviceID) []string {
	node := srv.DiscoNode()
	if node == nil {
		return nil
	}
	return node.Endpoints(id)
}

// memberRelayEndpoints reports the addresses a member announced for
// RELAYS.
//
// The anchor's own view is the node's: it records every announce it
// accepted, so it knows where each member can be reached.
func memberRelayEndpoints(srv *relay.Server, id identity.DeviceID) []string {
	node := srv.DiscoNode()
	if node == nil {
		return nil
	}
	return node.RelayEndpoints(id)
}

// relayAnnounceHost returns the host to publish the relay endpoint on.
//
// It is the announced host when there is one, else the --addr host. The
// announced one is right on a cloud server, where the public IP is a NAT
// mapping and not an address on any local interface.
func relayAnnounceHost(announce, addr string) string {
	host := ""
	if announce != "" {
		if h, _, err := net.SplitHostPort(announce); err == nil {
			host = h
		}
	} else if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return ""
	}
	return host
}

// saveDiscoveryConfig writes the join configuration into the state
// directory.
//
// The file carries the account key and the discovery addresses of the
// network, so it is written with mode 0600.
//
// The mode is FIXED, not merely requested. os.WriteFile does NOT change
// the mode of a file that ALREADY EXISTS: it only applies the mode when
// it creates the file. So a discovery.json restored from a backup with
// mode 0644 stayed world-readable — and every local user could read the
// network's account key and addresses. The write therefore goes through
// a temp file and a rename (the same shape as identity.SaveMembers),
// and the mode is set on the temp file before the rename makes it
// visible.
//
// The rename is also atomic, so a crash mid-write cannot leave a
// truncated configuration behind: a half-written file would fail to
// parse, and every later command (ping, msg, send-to) would refuse to
// run on this device.
//
// The INVITE field is scrubbed of anything that looks like a credential.
//
// The credential path used to store the raw credential string there, and
// the string carries the DELEGATOR PRIVATE KEY (the encoded credential is
// `pub.secret`). It is the most sensitive secret in the project, and it
// was written into a file that stays on disk indefinitely — outliving the
// credential's revocation, and landing in every backup of the state
// directory.
//
// The scrub is HERE, at the writer, rather than only at the call site:
// the writer is the last place that can guarantee the file never holds
// the secret, and a future caller that forgets will not silently write a
// private key to disk.
func saveDiscoveryConfig(dir string, cfg discoveryConfig) (string, error) {
	cfg.Invite = scrubCredentialFromEntry(cfg.Invite)
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "discovery.json")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".discovery.json.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	return path, nil
}

// credentialMarker is what the configuration carries instead of a
// credential: it says HOW the device joined, and it grants nothing.
const credentialMarker = "credential"

// looksLikeCredential reports whether a string is an encoded team
// credential.
//
// The check is on the PREFIX, which is what credential.Encode writes, so
// a credential cannot slip past by being formatted differently.
func looksLikeCredential(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), credential.Prefix+".")
}

// scrubCredentialFromEntry replaces a credential with the marker.
//
// The marker is not a secret and not an invite: it is a note for whoever
// reads the file. The derived fields (account id + discovery addresses)
// are what every later command actually uses, so nothing is lost.
func scrubCredentialFromEntry(entry string) string {
	if looksLikeCredential(entry) {
		return credentialMarker
	}
	return entry
}

// endpointsToSave returns the endpoint list to write into the device's
// configuration file.
//
// It takes the RESOLVED list, not the raw --endpoint flags. An endpoint
// whose port is zero means "use whatever --listen bound", and the announce
// already resolves it against the listener. Saving the raw list would keep
// the zero, and every later one-shot command reads that file and announces
// it — and the discovery node rejects a zero port as a malformed endpoint,
// which takes the member offline in everyone's view for the duration of a
// bench or a ping.
func endpointsToSave(resolved []string) []string {
	out := make([]string, 0, len(resolved))
	for _, e := range resolved {
		if e == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// gracefulLeaveTimeout bounds the departure broadcast of a graceful exit
// (P-5b). It sits inside the 3 s convergence budget on purpose: the
// broadcast plus the view push have to fit, and a broadcast that never
// arrives still converges through the members stream the client closes
// right after it.
const gracefulLeaveTimeout = 2 * time.Second

// runJoin implements `cmesh join <invite>`: announce until the membership
// approval lands, then show the network. With --once it exits after joining;
// otherwise it keeps announcing and printing topology changes until
// interrupted.
func runJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("device-passphrase", "", "passphrase for the device file (for scripting)")
	var eps stringSlice
	fs.Var(&eps, "endpoint", "address(es) other members can reach this device on, repeatable host:port (optional: a device with none is visible but not dialable)")
	var listen stringSlice
	fs.Var(&listen, "listen", "bind UDP endpoint(s) for direct links and answer them, repeatable host:port (port 0 = auto; the bound ports are what gets announced)")
	once := fs.Bool("once", false, "exit as soon as this device is a member")
	// The timeout bounds the APPROVAL WAIT and nothing else. It used to be
	// wrapped onto the signal context, which is the whole member process's
	// lifetime — so an approved member was torn down by the deadline and
	// broadcast a LEAVE (the real-machine finding: `join --timeout 5m` left
	// the network on its own after five minutes, and every later relay
	// request answered "this node cannot reach <target> right now"). A
	// member process is resident: once it is in, it stays in until it is
	// interrupted.
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for approval before giving up (0 = forever); the member keeps running after the approval lands")
	credStr := fs.String("cred", "", "team credential: join with this string instead of an invite (no per-device approval needed). --cred-file and CMESH_CRED are the other sources, in that order of strength, so the credential need not be on the command line")
	credFile := fs.String("cred-file", "", "read the team credential from this file (the credential is authority to join, so it is worth keeping out of the process list); a file that cannot be read or is empty is an error, never a fallback")
	relayTrunk := fs.String("relay-trunk", "", "address of the relay trunk to join (host:port): this device stays connected to it so it can RECEIVE relayed sessions")
	relayTrunkID := fs.String("relay-trunk-id", "", "device ID of the relay trunk (required with --relay-trunk: the join is pinned against it)")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if *once && len(listen) > 0 {
		return ExitError{Code: 2, Err: errors.New(
			"--once exits after approval; --listen needs a serving process (drop --once)")}
	}
	if *timeout < 0 {
		// 0 means "wait forever"; a negative value is a usage error, not a
		// silent "forever".
		return ExitError{Code: 2, Err: errors.New(
			"--timeout cannot be negative (0 means wait forever)")}
	}
	// The two ways in are exclusive: an invite positional argument, or a
	// team credential.
	//
	// The check comes BEFORE the credential is resolved, and that order is
	// the fix for the ambient-variable defect. It used to come after, so
	// an exported CMESH_CRED decided which way the operator was entering
	// the network: on a host that exports the variable globally — a
	// wrapper script, a CI runner, a shell profile — `cmesh join <invite>`
	// failed with "pass either --cred or an invite string, not both". The
	// operator's invite was perfectly good; the ambient variable broke
	// it.
	hasInvite := fs.NArg() >= 1
	explicitCred := strings.TrimSpace(*credStr) != "" || *credFile != ""
	if hasInvite && explicitCred {
		return ExitError{Code: 2, Err: errors.New(
			"pass either --cred (or --cred-file) or an invite string, not both")}
	}
	// The credential can come from the environment or from a file, not
	// only from the command line.
	//
	// The credential IS the authority to join, and a command line is
	// visible to every local user through /proc/<pid>/cmdline — so
	// `cmesh join --cred <string>` handed the network's front door to
	// anyone on the machine. (Measured on the real machine: `ps` showed
	// the full credential string.)
	//
	// The file is the strongest source: systemd's LoadCredential hands
	// the service a private copy that only its own process can read, so
	// the credential is in neither the process list nor the environment.
	//
	// The ENVIRONMENT is consulted only on the credential path: no invite
	// was given, and no credential source was named on the command line.
	// An ambient variable is a DOWNGRADE, not a clean zero — every
	// process running as the same user can read it out of
	// /proc/<pid>/environ — so it is the last source, and it never
	// overrides what the operator asked for.
	useEnv := !hasInvite && !explicitCred
	cred, err := resolveCredentialFrom(*credStr, *credFile, useEnv)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	*credStr = cred
	if *credStr == "" && fs.NArg() != 1 {
		return ExitError{Code: 2, Err: errors.New(
			"usage: cmesh join [flags] <invite-string> | cmesh join --cred <credential> (the invite goes last: flag parsing stops at the first positional argument)")}
	}

	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	phrase, err := readPassphrase(*pass, "device passphrase")
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	// Two ways in. With a credential the device enrolls ITSELF: it mints a
	// certificate under the credential's delegator key and asks the
	// discovery node to admit it. With an invite it asks, and waits for the
	// account owner to approve.
	var inv discovery.Invite
	var dev *identity.Device
	var credJoined *credential.Credential
	if *credStr != "" {
		cred, err := credential.Decode(*credStr)
		if err != nil {
			return ExitError{Code: 1, Err: fmt.Errorf("bad credential: %w", err)}
		}
		// Self-check for a friendly error; the node's check is the real one.
		if err := cred.Verify(cred.Public.AccountPub, time.Now().UTC()); err != nil {
			return ExitError{Code: 1, Err: fmt.Errorf("credential rejected: %w", err)}
		}
		dev, err = ensureDeviceIdentity(dir, phrase)
		if err != nil {
			return ExitError{Code: 1, Err: err}
		}
		if dev.ID() == cred.Public.AccountID {
			return ExitError{Code: 1, Err: errors.New(
				"this device's ID equals the account ID; a credential introduces a device, it does not make one an account")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), enrollTimeout)
		err = enrollWithCredential(ctx, cred, dev)
		cancel()
		if err != nil {
			return ExitError{Code: 1, Err: err}
		}
		fmt.Fprintf(os.Stderr, "enrolled as device %s\n", dev.ID())
		inv = inviteFromCredential(cred)
		credJoined = cred
	} else {
		// The private-deployment switch reaches the INVITE's addresses
		// too, not just the client's own configuration. Without this, a
		// LAN deployment could use a credential but never an invite —
		// and the error ("bad invite: malformed endpoint") pointed at
		// the invite's shape instead of at the switch that would have
		// allowed it.
		inv, err = discovery.DecodeInviteWith(fs.Arg(0), discoveryAllowPrivate())
		if err != nil {
			return ExitError{Code: 1, Err: fmt.Errorf("bad invite: %w", err)}
		}
		devPath := identity.IdentityPath(dir, identity.DeviceFileName)
		if _, err := os.Stat(devPath); err != nil {
			return ExitError{Code: 1, Err: fmt.Errorf(
				"%s not found; this device needs its own identity first: run cmesh init, then ask the account owner to sign it in (cmesh account device-add)", devPath)}
		}
		dev, err = identity.LoadDevice(devPath, phrase)
		if err != nil {
			return ExitError{Code: 1, Err: err}
		}
	}

	if dev.ID() == inv.AccountID {
		return ExitError{Code: 1, Err: errors.New(
			"this device's ID equals the invite's account ID; joining your own account as a device needs the account owner to sign a certificate, not an invite")}
	}
	// Sanity on the invite itself: an issue time in the future means a
	// broken clock or a tampered string (review F10; full TTL enforcement is
	// the O-5 follow-up).
	if inv.IssuedAt.After(time.Now().Add(5 * time.Minute)) {
		return ExitError{Code: 1, Err: errors.New(
			"invite issue time is in the future; check this machine's clock or re-request the invite")}
	}

	// Bind the direct-link listener FIRST (when requested): its resolved
	// ports are what join announces, so discovery never advertises a port
	// that is not open. The listener's key resolver is pointed at the live
	// view as soon as the client exists — until then it cleanly rejects
	// incoming streams ("no verified member view"), which is also correct
	// behaviour before the first snapshot.
	resolver := &liveViewResolver{accountPub: []byte(inv.AccountPub)}
	var listener *mesh.Listener
	// Announcement priority: explicit --endpoint wins (the operator knows
	// their public / NAT-mapped address — a bound wildcard or private IP
	// cannot express it, issue #8 D-1); otherwise the concrete bound
	// addresses are announced (never wildcards — peers cannot dial
	// 0.0.0.0).
	announced := []string(eps)
	inboxDir := filepath.Join(dir, "inbox")
	if len(listen) > 0 {
		l, err := mesh.NewListener(context.Background(), mesh.ListenerConfig{
			Device:  dev,
			Members: resolver,
			Addrs:   []string(listen),
			// R-4.1: member-to-member messages print live.
			OnMessage: func(from identity.DeviceID, text string) {
				fmt.Fprintf(os.Stderr, "📩 from %s: %s\n", from, text)
			},
			// R-4.2: inbound files land in the state dir's inbox/.
			InboxDir: inboxDir,
			OnFile: func(from identity.DeviceID, savedPath string, h mesh.FileHeader) {
				fmt.Fprintf(os.Stderr, "📬 from %s: %s → %s (sha256 %s…, %d bytes)\n",
					from, h.Name, savedPath, h.SHA256[:12], h.Size)
			},
		})
		if err != nil {
			return ExitError{Code: 1, Err: err}
		}
		listener = l
		defer listener.Close()

		// An --endpoint whose port is 0 means "use whatever --listen
		// actually bound" (oh-no-agent: --listen 0 expands and prints the
		// bound port, but --endpoint 127.0.0.1:0 kept announcing port 0 and
		// the anchor refused it as malformed). Resolve those against the
		// listener's bound ports — keeping the announced host, replacing
		// only the port.
		announced = resolvePortZeroEndpoints(announced, l.BoundAddrs(), os.Stderr)

		if len(eps) == 0 {
			announced = l.BoundAddrs()
			if len(announced) == 0 {
				fmt.Fprintf(os.Stderr, "WARNING: listening on %s, but a wildcard address cannot be announced —\n", strings.Join(l.WildcardAddrs(), ", "))
				fmt.Fprintf(os.Stderr, "  other members will NOT be able to dial this device.\n")
				fmt.Fprintf(os.Stderr, "  pass --endpoint <your-public-or-NAT-mapped-address>:<port> to publish one.\n")
			}
		} else if wl := l.WildcardAddrs(); len(wl) > 0 {
			fmt.Fprintf(os.Stderr, "note: wildcard binds %s serve incoming links but are not announced; announcing %s\n",
				strings.Join(wl, ", "), strings.Join(announced, ", "))
		}
		fmt.Fprintf(os.Stderr, "  inbox: %s (inbound files land here)\n", inboxDir)
	}

	cl, err := discovery.NewClient(withDiscoveryClientTransport(discovery.ClientConfig{
		Addrs:                 inv.DiscoveryAddrs,
		Device:                dev,
		AccountID:             inv.AccountID,
		AccountPub:            []byte(inv.AccountPub),
		Endpoints:             announced,
		AllowLocalDiscovery:   discoveryAllowLocal(),
		AllowPrivateDiscovery: discoveryAllowPrivate(),
		Logger:                setupLogging(false),
	}))
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	// The live view resolver is pointed at the client UNCONDITIONALLY.
	//
	// It used to be set only when a listener was configured ("--listen"),
	// which meant a PURE-OUTBOUND member's resolver stayed empty forever:
	// its member view was "no verified member view yet", every membership
	// lookup failed, and the relay trunk join could never authenticate —
	// so the one member shape the trunk exists for (a member with no
	// inbound port) could not use it at all (defect B of the round-61
	// real-machine report).
	//
	// The resolver is what the relay join pins the trunk's certificate
	// against, so it is needed by EVERY member, listener or not.
	resolver.set(cl)
	// The hook lets a test observe the join's OWN resolver — the one the
	// relay join uses. It is nil in production.
	if joinResolverHook != nil {
		joinResolverHook(resolver)
	}
	if listener != nil {
		// D-1b: wildcard-only binds print as "bound, not announced" instead
		// of an empty list that reads like nothing is listening.
		parts := make([]string, 0, 2)
		if bound := listener.BoundAddrs(); len(bound) > 0 {
			parts = append(parts, strings.Join(bound, ", "))
		}
		if wild := listener.WildcardAddrs(); len(wild) > 0 {
			parts = append(parts, "wildcard "+strings.Join(wild, ", ")+" (bound, not announced — pass --endpoint to publish)")
		}
		fmt.Fprintf(os.Stderr, "listening for direct links on %s\n", strings.Join(parts, "; "))
	}

	// Persisted only after every check passed: a failed join must not leave
	// a config file that looks approved (review F9).
	//
	// The account and the discovery addresses are recorded as FIELDS, derived
	// here where both entry paths already agree on them. Later commands read
	// these instead of re-parsing the raw string: `join --cred` stores a
	// cmesh1. credential, which DecodeInvite cannot read, and re-parsing it
	// broke every follow-up command (oh-no-agent, defect B-3).
	//
	// The CREDENTIAL itself is NOT stored.
	//
	// It used to be: the credential path put the raw string into the
	// "invite" field, and the string carries the DELEGATOR PRIVATE KEY
	// (the encoded credential is `pub.secret`). So the most sensitive
	// secret in the project sat in a file on disk indefinitely — it
	// outlived the credential's revocation (nothing removed it), and it
	// landed in every backup or snapshot of the state directory.
	//
	// Nothing needs it there. The derived fields ARE what every later
	// command reads: discoveryConfig.invite() returns them directly when
	// they are set, and the credential path sets them. The raw string was
	// dead weight, and it was the heaviest kind of dead weight.
	entry := fs.Arg(0)
	viaCredential := *credStr != ""
	if viaCredential {
		// A marker, not the secret: it says HOW this device joined, so an
		// operator reading the file can tell the two entry paths apart
		// without the file holding anything that grants access.
		entry = credentialMarker
	}
	cfgPath, err := saveDiscoveryConfig(dir, discoveryConfig{
		Invite:         entry,
		AccountID:      string(inv.AccountID),
		AccountPub:     inv.AccountPub,
		DiscoveryAddrs: inv.DiscoveryAddrs,
		Endpoints:      endpointsToSave(announced),
		SavedAt:        time.Now(),
	})
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	fmt.Fprintf(os.Stderr, "joining account %s via %s\n", inv.AccountID, strings.Join(inv.DiscoveryAddrs, ", "))
	fmt.Fprintf(os.Stderr, "  device   %s\n", dev.ID())
	fmt.Fprintf(os.Stderr, "  config   %s\n", cfgPath)
	if viaCredential {
		// The device enrolled itself: there is no approval to wait for, and
		// the instructions below would send the operator on an errand that
		// is already done.
		fmt.Fprintf(os.Stderr, "\nEnrolled with a team credential — no approval needed.\n\n")
	} else {
		fmt.Fprintf(os.Stderr, "\nWaiting for membership approval (R-1.5). To approve this device:\n")
		fmt.Fprintf(os.Stderr, "  1. copy the line below to the account owner\n")
		fmt.Fprintf(os.Stderr, "  2. owner runs: cmesh account authorize --pub <that value>\n")
		fmt.Fprintf(os.Stderr, "  3. the cert file goes into the discovery host's certs directory\n\n")
		fmt.Fprintf(os.Stderr, "PUBKEY: %s\n\n", base64.StdEncoding.EncodeToString(dev.PublicKey()))
	}

	// SIGINT (Ctrl-C) and SIGTERM (systemd stop, docker stop) both mean
	// "leave the network gracefully" (P-5b): the departure is BROADCAST, so
	// the other members' views say offline right away instead of waiting
	// for this process's socket to die.
	//
	// This context is the PROCESS lifetime, and only that. The approval
	// deadline used to be wrapped onto it, which made --timeout the whole
	// member's lifetime: an approved member was torn down by the deadline
	// and broadcast a LEAVE, so it vanished from the roster's reachability
	// view and the anchor could not reach it any more. The deadline is a
	// property of the WAIT, so it lives on its own context below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if joinLifetimeHook != nil {
		// A test supplies the member's lifetime (see joinLifetimeHook).
		// The signal context is released right away: the test's context is
		// the one that ends this join.
		testCtx, testCancel := joinLifetimeHook()
		stop()
		ctx, stop = testCtx, testCancel
	}

	// The approval deadline: how long the join waits for the account owner
	// to approve this device.
	//
	// It is a property of the WAIT, not of the process: the loop stops
	// watching it the moment the approval lands, so an approved member runs
	// until it is interrupted. A timeout of 0 means "wait forever", which
	// is the shape the flag has always had.
	//
	// The channel is nil when there is no deadline (a nil channel blocks
	// forever in a select, which is exactly "wait forever"), and it is set
	// back to nil when the approval lands — that is the disarm.
	//
	// armDeadline is a closure because the wait can START TWICE: a device
	// that is revoked while it is a member starts waiting for the approval
	// again, and that second wait is bounded by the same flag.
	var deadlineCancels []context.CancelFunc
	armDeadline := func() <-chan struct{} {
		if *timeout <= 0 {
			return nil
		}
		approvalCtx, cancelDeadline := context.WithTimeout(context.Background(), *timeout)
		deadlineCancels = append(deadlineCancels, cancelDeadline)
		return approvalCtx.Done()
	}
	defer func() {
		for _, cancelDeadline := range deadlineCancels {
			cancelDeadline()
		}
	}()
	deadlineCh := armDeadline()

	// The client's own lifetime is NOT the signal context. The graceful
	// departure has to run BEFORE the run loop is torn down: if the signal
	// reached the client directly, the members stream would die first and
	// the departure would degrade into the abnormal-disconnect path instead
	// of the explicit LEAVE broadcast.
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	go func() { _ = cl.Run(runCtx) }()

	// Certificate renewal: the member keeps its own certificate fresh.
	//
	// Before this loop existed, the certificate's expiry was the whole
	// membership: when it lapsed the anchor rejected the device and it fell
	// out of the network SILENTLY — no error, no renewal, just gone (the
	// real-machine finding behind D77). The anchor side already accepted a
	// renewal (MemberList.Add refreshes a certificate for the same device
	// key and account — its comment even says "a re-issue after renewal"),
	// so the missing half was the client: nobody ever asked for a fresh one.
	if credJoined != nil {
		go certRenewLoop(runCtx, credJoined, dev, nil)
	}

	// The relay trunk join: this device stays CONNECTED to the anchor so
	// that the anchor can reach it for relayed sessions.
	//
	// This is what makes a PURE-OUTBOUND member reachable: it has no
	// inbound port, so the anchor cannot dial it. But it can dial the
	// anchor, and it keeps that connection open — so the anchor reaches
	// it BACK over it. That is the whole mechanism of the trunk (D73).
	if *relayTrunk != "" {
		if *relayTrunkID == "" {
			return ExitError{Code: 2, Err: errors.New(
				"--relay-trunk needs --relay-trunk-id: the join is pinned against the trunk's device ID, " +
					"and a member that does not know which anchor it is joining must not join an arbitrary one")}
		}
		go joinRelayTrunk(runCtx, dev, resolver, identity.DeviceID(*relayTrunkID), *relayTrunk, filepath.Join(dir, "inbox"))
	}

	// gracefulLeave is the single exit path of a member that is in the
	// network (P-5b): the LEAVE broadcast tells the other members right
	// away, and the members stream the client closes right after it is the
	// fallback for a broadcast that never arrives.
	gracefulLeave := func() {
		leaveCtx, cancelLeave := context.WithTimeout(context.Background(), gracefulLeaveTimeout)
		err := cl.Leave(leaveCtx)
		cancelLeave()
		if err != nil {
			fmt.Fprintf(os.Stderr, "leave broadcast failed (%v); leaving through the connection close\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "left the network (LEAVE broadcast sent)\n")
		}
		stopRun()
	}

	printSnapshot := func() {
		snap := cl.Snapshot()
		if snap == nil {
			return
		}
		fmt.Fprintf(os.Stderr, "network view v%d: %d member(s)\n", snap.Version, len(snap.Members))
		for id, mv := range snap.Members {
			mark := ""
			if id == dev.ID() {
				mark = " (this device)"
			}
			state := "offline"
			if mv.Online {
				state = "online"
			}
			eps := ""
			if len(mv.Endpoints) > 0 {
				eps = "  " + strings.Join(mv.Endpoints, ",")
			}
			fmt.Fprintf(os.Stderr, "  %-16s %s%s%s\n", id, state, eps, mark)
		}
	}

	approvedAt := time.Time{}
	// Print only when the VIEW actually changes: version (membership) or the
	// online set (liveness). Same-version same-liveness snapshots arrive on
	// every re-push, and re-rendering them produced 8800 lines/s of log spam
	// under a churning server (issue #8, report 1 §3.2). The signature guard
	// keeps online/offline transitions visible while killing the spam; the
	// D-3 server-side fix removes the churn itself.
	viewSig := func() string {
		snap := cl.Snapshot()
		if snap == nil {
			return ""
		}
		// Signature covers membership version AND each member's endpoint
		// set: endpoints arrive via same-version pushes (announce updates
		// don't bump the member-list version), and a member gaining its
		// first dialable endpoint is exactly when the user needs the
		// reprint (issue #8: endpoint arrival used to wait for the next
		// version bump to become visible).
		ids := make([]string, 0, len(snap.Members))
		for id, mv := range snap.Members {
			state := "0"
			if mv.Online {
				state = "1"
			}
			sort.Strings(mv.Endpoints)
			ids = append(ids, fmt.Sprintf("%s:%s:%v", id, state, mv.Endpoints))
		}
		sort.Strings(ids)
		return fmt.Sprintf("v%d|%s", snap.Version, strings.Join(ids, ","))
	}
	lastSig := ""
	for {
		select {
		case <-cl.Updates():
			if cl.Joined() {
				if approvedAt.IsZero() {
					approvedAt = time.Now()
					fmt.Fprintf(os.Stderr, "approved: this device is now a member\n")
					printSnapshot()
					lastSig = viewSig()
					// The approval landed: the deadline has done its job and
					// is DISARMED. From here on this process is a resident
					// member and it runs until it is interrupted — the
					// deadline must not be able to take an approved member
					// out of the network (the real-machine finding: a join
					// with --timeout 5m left the network by itself after
					// five minutes and the anchor could not reach it any
					// more).
					deadlineCh = nil
					if *once {
						// The process is about to exit while it is a
						// member: say goodbye to the network instead of
						// letting the socket die silently.
						gracefulLeave()
						return nil
					}
					continue
				}
				if sig := viewSig(); sig != lastSig {
					printSnapshot()
					lastSig = sig
				}
			} else if !approvedAt.IsZero() {
				// Membership was revoked (R-1.3 says what happens to
				// sessions; here the honest CLI response is to say so).
				fmt.Fprintf(os.Stderr, "membership lost: this device is no longer in the member list\n")
				approvedAt = time.Time{}
				// The device is a PENDING member again, so the approval
				// wait starts over — and it is bounded again: a join that
				// waits forever for a re-approval would hang a shell
				// prompt just like the first wait did.
				deadlineCh = armDeadline()
			}
		case <-deadlineCh:
			// The approval wait is over: the deadline expired.
			//
			// A member that is IN is never affected by the deadline — the
			// approval disarms it — so this arm can only fire while the
			// join is still a PENDING member. The one exception is a race
			// between the deadline firing and the approval being observed:
			// then the device IS a member, and it stays one (the loop
			// disarms the deadline and carries on).
			if cl.Joined() {
				deadlineCh = nil
				continue
			}
			// Give up with an error the operator can act on. The device was
			// never a member, so there is nothing to clean up: the announce
			// was refused by the node all along, and no half-member state
			// exists on the discovery host.
			return ExitError{Code: 1, Err: fmt.Errorf(
				"not approved within %s; the account owner must add this device's certificate to the member list on the discovery host", *timeout)}
		case <-ctx.Done():
			// SIGINT/SIGTERM: the process lifetime is over.
			if cl.Joined() {
				// Graceful departure (P-5b): broadcast the LEAVE so the
				// other members' views say offline now, then let the run
				// loop go.
				gracefulLeave()
				return nil
			}
			return nil
		}
	}
}

// serveDiscoveryGrace bounds the drain after SIGINT/SIGTERM: the HTTP
// listener closes immediately, and member streams plus pairing rooms get
// this long to unwind before the stop gives up on them. The bound keeps a
// wedged peer from hanging the process stop forever; anything still open at
// exit dies with the process anyway.
const serveDiscoveryGrace = 10 * time.Second

// shutdownDiscoveryServer is the anchor's stop path.
//
// It is a NAMED function, not a block inside the signal goroutine, because
// the thing it has to guarantee is a WIRING fact: the relay trunk must
// actually be stopped when the anchor stops.
//
// The trunk used to have no shutdown path at all (CloseAll existed and
// nothing in production called it), and the first fix — adding Shutdown() —
// had the same failure mode in a new place: the capability existed, the
// unit test of the capability was green, and the ANCHOR STILL DID NOT CALL
// IT. A capability that nothing calls has exactly the symptom of a
// capability that does not exist.
//
// So the wiring gets its own test, on this function, which is the real
// entry point the anchor's stop watcher calls.
func shutdownDiscoveryServer(srv *relay.Server, trunk *mesh.RelayTrunk) {
	// The RELAY TRUNK goes down FIRST, and it goes down as ONE call:
	// no new relay is admitted, and every live one is torn down (which
	// gives their stream slots back).
	//
	// It used to have no shutdown path at all: CloseAll existed but
	// nothing in production called it, so an anchor that stopped left
	// its live relay flows running — and their stream slots reserved.
	// The next relay on the same anchor would then be refused for
	// lack of a slot that no longer had a flow behind it.
	if trunk != nil {
		trunk.Shutdown()
	}
	if srv == nil {
		return
	}
	sctx, cancel := context.WithTimeout(context.Background(), serveDiscoveryGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		fmt.Fprintf(os.Stderr, "serve-discovery: shutdown: %v\n", err)
	}
}

// watchDiscoveryStop is the anchor's stop watcher: it waits for the
// context to end (the signal) and then drains the node.
//
// It is a named function for the same reason as shutdownDiscoveryServer:
// the WIRING is the thing under test, and a wiring that lives inside an
// anonymous goroutine cannot be tested without a real signal.
func watchDiscoveryStop(ctx context.Context, stop func(), srv *relay.Server, trunk *mesh.RelayTrunk) {
	<-ctx.Done()
	// Restore default signal handling first: a second SIGTERM while the
	// drain wedges must kill the process instead of being swallowed by
	// the very registration this drain waits on.
	stop()
	shutdownDiscoveryServer(srv, trunk)
}

// watchRelayTrunkSwitch is the operator's relay switch.
//
// SIGHUP toggles the trunk between "carrying relays" and "not carrying
// relays" WITHOUT stopping the node: the members' control plane, the
// panel and the pairing relay keep working, and the trunk simply stops
// accepting new relays (and, when it is turned off, tears down the ones
// it is carrying — a trunk that is switched off must not keep other
// people's traffic).
//
// ★ THIS EXISTS BECAUSE SetEnabled HAD NO CALLER. (Round 98.) The
// capability was written, the unit test of it was green, and no
// production path ever called it — the same shape this project has hit
// three times now (CloseAll, SetEnabled, and the trunk's own Shutdown
// wiring). A capability that nothing calls has exactly the symptom of a
// capability that does not exist.
//
// The signal handler is installed on its own channel, and it is REMOVED
// when the context ends, so a SIGHUP that arrives after the stop does
// not resurrect the default handler's behaviour.
func watchRelayTrunkSwitch(ctx context.Context, trunk *mesh.RelayTrunk) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigCh:
			on := !trunk.Enabled()
			trunk.SetEnabled(on)
			if on {
				fmt.Fprintf(os.Stderr, "  relay trunk: ON — this node carries relays again\n")
				continue
			}
			// Switching OFF must also tear down what is being carried:
			// a trunk that is "off" but still forwarding other members'
			// traffic is worse than one that is off.
			trunk.CloseAll()
			fmt.Fprintf(os.Stderr, "  relay trunk: OFF — this node carries no more relays\n")
		}
	}
}

// runServeDiscovery implements `cmesh serve-discovery`: the pairing relay and
// the mesh discovery node in one process. The first machine to do this
// bootstraps an empty member list containing itself (O-6).
func runServeDiscovery(args []string) error {
	fs := flag.NewFlagSet("serve-discovery", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	addr := fs.String("addr", ":8080", "listen address")
	path := fs.String("path", "/ws", "pairing WebSocket endpoint path")
	membersPath := fs.String("members", "", "member list file (default <state>/members.json)")
	pass := fs.String("device-passphrase", "", "passphrase for the device file (for scripting)")
	tlsCert := fs.String("tls-cert", "", "certificate file; with --tls-key the server speaks wss/https directly")
	tlsKey := fs.String("tls-key", "", "private key file for --tls-cert")
	// Optional but strongly recommended: with it, adopted certificates are
	// verified against the account key before entering the list; without it,
	// the certs directory is trusted as operator-managed (review F2).
	accountPub := fs.String("account-pub", "", "account public key, base64 or hex (`cmesh account show` prints both); enables signature verification for adopted certificates")
	// The discovery node is itself a member; without an announce it shows
	// offline in every member's view forever (issue #8 D-6). A wildcard
	// --addr has no self-describing public address, hence this flag.
	announce := fs.String("announce", "", "address members use to reach THIS node (host:port); announces the node itself into the member view so it is visible and dialable")
	// The panel is the product's face: "how many of my devices are online".
	// It is OFF unless asked for, and its data endpoint is protected by a
	// token — the page itself is a static shell that carries no state.
	panel := fs.Bool("panel", false, "serve the visualization panel at /panel (its data endpoint /api/mesh needs the panel token)")
	panelToken := fs.String("panel-token", "", "token for the panel's data endpoint; generated and printed when --panel is set and this is empty. CMESH_PANEL_TOKEN is the fallback, so a service can keep the token OUT of its command line")
	panelTokenFile := fs.String("panel-token-file", "", "read the panel token from this file (the systemd credential way: the token is in neither the command line nor the process environment)")
	// The relay trunk: this node carries relays between two OTHER members
	// when somebody asks it to (D73). It is off unless it is asked for —
	// an anchor that relays for the whole network without being told is a
	// public relay, and the switch is the operator's decision.
	relayTrunk := fs.Bool("relay-trunk", false, "act as a mesh relay trunk: carry relays between two other members (default off)")
	relayListenPort := fs.Int("relay-port", 0, "UDP port for the relay trunk's own listener (0 = share the direct-link port; ALPN separates the two protocols)")
	// The relay quota (P-7). The defaults are the requirement's numbers
	// (20 concurrent relays, 50 MiB/s); these flags let an operator
	// tighten them without a rebuild — which is what makes the quota
	// testable on a real deployment (defect H of the round-61 real-machine
	// report: there was no way to lower it except by recompiling).
	relayMaxStreams := fs.Int("relay-max-streams", mesh.DefaultRelayMaxStreams, "how many relays this node may carry at once")
	relayMaxBytesPerSec := fs.Int("relay-max-bytes-per-sec", mesh.DefaultRelayBytesPerSec, "how many bytes per second this node may forward for others")
	verbose := fs.Bool("v", false, "verbose (debug) logging")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	// Half a TLS config is worse than none: the server would serve plaintext
	// while the operator believes it is encrypted (review F11).
	if (*tlsCert == "") != (*tlsKey == "") {
		return ExitError{Code: 2, Err: errors.New(
			"--tls-cert and --tls-key must be given together; one alone would silently serve plaintext")}
	}
	var accountPubKey []byte
	if *accountPub != "" {
		raw, err := parsePubKeyString(*accountPub)
		if err != nil {
			return ExitError{Code: 2, Err: fmt.Errorf("--account-pub: %w", err)}
		}
		accountPubKey = raw
	}

	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	devPath := identity.IdentityPath(dir, identity.DeviceFileName)
	if _, err := os.Stat(devPath); err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf(
			"%s not found; a discovery node must itself be a network member: run cmesh init && cmesh account create && cmesh account device-add first — all three accept --state, and this node is reading %s (oh-no-agent round 59: the hint sent first-time operators to ~/.cmesh while their state lived elsewhere)",
			devPath, dir)}
	}
	phrase, err := readPassphrase(*pass, "device passphrase")
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	dev, err := identity.LoadDevice(devPath, phrase)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	if *membersPath == "" {
		*membersPath = filepath.Join(dir, "members.json")
	}
	members, bootstrapped, err := loadOrBootstrapMembers(*membersPath, dev)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	if bootstrapped {
		fmt.Fprintf(os.Stderr, "bootstrapped member list %s with this device (%s)\n", *membersPath, dev.ID())
	}

	relay.SetVersion(Version)

	// Graceful stop: SIGINT (Ctrl-C) and SIGTERM (systemd stop) must end in
	// a controlled drain — HTTP listener closed, member streams and pairing
	// rooms unwound, UDP direct-link socket released — and exit 0, so a
	// systemd restart finds the port free instead of failed-unit residue.
	// The registration happens BEFORE the listening banner prints: a test or
	// operator script can treat that banner as the safe-to-signal point.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	scheme := "ws"
	httpScheme := "http"
	if *tlsCert != "" {
		scheme = "wss"
		httpScheme = "https"
	}
	fmt.Fprintf(os.Stderr, "discovery node listening on %s://%s\n", scheme, strings.TrimPrefix(*addr, ":"))
	fmt.Fprintf(os.Stderr, "  register: POST   %s://%s/mesh/register\n", httpScheme, *addr)
	fmt.Fprintf(os.Stderr, "  members:  WS     %s://%s/mesh/members\n", httpScheme, *addr)
	fmt.Fprintf(os.Stderr, "  members file: %s (edited externally; hot-reloaded)\n", *membersPath)
	fmt.Fprintf(os.Stderr, "  pairing relay and dashboard unchanged: %s%s, /dashboard\n", scheme, *addr)

	srv := relay.New(relay.Config{
		Addr:    *addr,
		Path:    *path,
		TLSCert: *tlsCert,
		TLSKey:  *tlsKey,
		Logger:  setupLogging(*verbose),
	})
	if *panel {
		tok, generated, err := resolvePanelTokenFrom(*panelToken, *panelTokenFile)
		if err != nil {
			// A token file that was CONFIGURED but cannot be read is a
			// mistake to report, not a reason to guess: the node would
			// otherwise run with a DIFFERENT token and the dashboard's
			// saved token would silently stop working.
			return ExitError{Code: 2, Err: err}
		}
		if generated {
			// The generated token is printed ONCE, because the operator
			// has no other way to learn it. It is printed to STDERR, and a
			// service that wants it out of the log entirely can set
			// CMESH_PANEL_TOKEN instead.
			fmt.Fprintf(os.Stderr, "  panel token: %s  (keep it private; the data endpoint rejects anything else)\n", tok)
		}
		srv.SetPanelToken(tok)
		srv.SetAnchorDeviceID(dev.ID())
		// The token is NOT put in the URL: a query string lands in the
		// browser history and in referrer headers. The page has a field
		// for it.
		panelHost := "127.0.0.1"
		if h, _, err := net.SplitHostPort(*addr); err == nil && h != "" && h != "0.0.0.0" && h != "::" {
			panelHost = h
		}
		fmt.Fprintf(os.Stderr, "  panel: %s://%s/panel   (paste the token into the page)\n", httpScheme, net.JoinHostPort(panelHost, portOf(*addr)))
		if !generated {
			// A token that came from the operator (flag or environment)
			// is NOT printed: it is already in the operator's hands, and
			// printing it puts it into the journal — which the
			// systemd-journal group can read.
			fmt.Fprintf(os.Stderr, "  panel token: set (kept out of this log; the data endpoint rejects anything else)\n")
		}
	}
	srv.EnableDiscovery(members)

	// The relay trunk's authorization state machine. The switch decides
	// whether this node relays at all, and it is off by default.
	//
	// The member resolver is the LIVE member list (the same one the direct
	// link pins certificates against), so a revoked device is refused on
	// its next request — no restart, no cached snapshot.
	// relaySelfEPS is the PUBLIC address on which this node accepts
	// relays, published in the member view so a requester can find a
	// trunk to relay through.
	//
	// The relay endpoint is the announced host with the RELAY port. In
	// the default (same port) shape that port IS the direct-link port:
	// ALPN is negotiated per connection, so one socket serves both
	// protocols (D76). With an explicit --relay-port it is that port
	// instead. The host is the announced one for the same reason the
	// direct address is — on a cloud server the public IP is a NAT
	// mapping, not a local interface.
	//
	// trunk and joiners are hoisted out of the --relay-trunk block: the
	// DIRECT-LINK listener needs them, because the relay protocol rides
	// on the direct link's socket when no separate relay port was asked
	// for (D76).
	var relayTrunkInst *mesh.RelayTrunk
	var relayJoiners *mesh.JoinerTable
	var relaySelfEPS []string
	if *relayTrunk {
		// The joiner table: the members that have JOINED this trunk. A
		// joined member is reachable over the connection it opened
		// towards the anchor — the only way a PURE-OUTBOUND member (no
		// inbound port at all) can be reached.
		joiners := mesh.NewJoinerTable()
		trunk, err := mesh.NewRelayTrunk(mesh.RelayTrunkConfig{
			Self:    dev.ID(),
			Members: relayMembersResolver{srv: srv},
			Enabled: true,
			Quota: mesh.RelayQuota{
				MaxStreams:     *relayMaxStreams,
				MaxBytesPerSec: *relayMaxBytesPerSec,
			},
			Joiners: joiners,
			// The trunk's dialer: how it reaches a member that has NOT
			// joined it — a member with direct endpoints of its own.
			//
			// Without it the trunk's data plane was DEAD in this
			// deployment: OpenFlow answered "no dialer installed" and
			// every relay to a non-joined member was refused (defect D
			// of the round-61 real-machine report).
			//
			// The RELAY endpoints are used, not the direct ones: the
			// dial opens a relay-protocol session, and a direct
			// endpoint does not speak it (unless the member serves both
			// on one port, which is the same-port shape).
			Dial: func(ctx context.Context, target identity.DeviceID, kind string) (mesh.Session, error) {
				eps := memberRelayEndpoints(srv, target)
				if len(eps) == 0 {
					eps = memberEndpoints(srv, target)
				}
				if len(eps) == 0 {
					return nil, fmt.Errorf("%w: %s announced no endpoint to reach it on",
						mesh.ErrRelayUnavailable, target)
				}
				link, err := mesh.DialRelaySession(ctx, dev, relayMembersResolver{srv: srv}, target, eps)
				if err != nil {
					return nil, err
				}
				return link.Session(), nil
			},
		})
		if err != nil {
			return ExitError{Code: 1, Err: fmt.Errorf("serve-discovery: --relay-trunk: %w", err)}
		}
		mesh.SetRelayTrunk(trunk)
		relayTrunkInst = trunk
		relayJoiners = joiners
		// The revocation watch: an ESTABLISHED relay must be torn down
		// when one of its ends stops being a member, not just the next
		// request refused. The watch re-checks every second.
		trunk.StartRevocationWatch(ctx, time.Second)

		// The RELAY PORT.
		//
		// Zero (the default) means "the SAME port as the direct link".
		// ALPN is negotiated per connection, so one UDP socket serves
		// both protocols and the operator does not have to open a second
		// port in their security group (D76).
		//
		// An explicit --relay-port still gets a listener of its own, for
		// the deployments that want the two protocols on separate ports.
		relayPort := *relayListenPort
		if relayPort > 0 {
			rl, lerr := mesh.NewRelayListener(ctx, mesh.RelayListenerConfig{
				Device:  dev,
				Members: relayMembersResolver{srv: srv},
				Addrs:   []string{net.JoinHostPort("0.0.0.0", strconv.Itoa(relayPort))},
				Trunk:   trunk,
				Joiners: joiners,
			})
			if lerr != nil {
				fmt.Fprintf(os.Stderr, "  note: cannot bind the relay listener on UDP %d (%v)\n", relayPort, lerr)
			} else {
				defer rl.Close()
				fmt.Fprintf(os.Stderr, "  relay trunk: ON — UDP %d carries relays between members\n", relayPort)
				fmt.Fprintf(os.Stderr, "    (the payload is end-to-end encrypted: the trunk sees routing metadata, not content)\n")
				// ★ THE BUFFER THE KERNEL ACTUALLY GRANTED. (Round 108.)
				// A receive buffer below the floor makes the relay path
				// drop datagrams under burst, and the drop shows up as a
				// transfer that fails as a TIMEOUT with no reason. The
				// operator is told here, where they are looking.
				for _, w := range rl.BufferWarnings() {
					fmt.Fprintf(os.Stderr, "  note: %s\n", w)
				}
				relayHost := relayAnnounceHost(*announce, *addr)
				if relayHost != "" {
					relaySelfEPS = []string{net.JoinHostPort(relayHost, strconv.Itoa(relayPort))}
				} else {
					fmt.Fprintf(os.Stderr, "  note: the relay listener is on UDP %d but no public address is known;\n", relayPort)
					fmt.Fprintf(os.Stderr, "        pass --announce <public-host:port> so members can find this trunk\n")
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "  relay trunk: ON — authorization is live; the relay shares the direct-link port\n")
			fmt.Fprintf(os.Stderr, "    (the payload is end-to-end encrypted: the trunk sees routing metadata, not content)\n")
		}
	} else {
		fmt.Fprintf(os.Stderr, "  relay trunk: off (pass --relay-trunk to carry relays between members)\n")
	}

	// The stop watcher needs the server handle, so it starts here — but the
	// signal registration above already ran, so a signal that arrived in the
	// meantime is not lost: ctx is already done, Shutdown arms the flag
	// ListenAndServe consults when the signal beat the bind, and the process
	// still exits 0 instead of serving a server nobody can stop.
	go watchDiscoveryStop(ctx, stop, srv, relayTrunkInst)

	// ★ THE OPERATOR'S SWITCH ON THE RELAY TRUNK. (Round 98.)
	//
	// D73 promised that a node could stop relaying without being
	// restarted. The capability existed — RelayTrunk.SetEnabled — and
	// NOTHING called it: no flag, no signal, no admin endpoint. So the
	// promise was in the design and not in the product.
	//
	// SIGHUP is the switch. It is the conventional "reload the
	// configuration" signal, and a node that is relaying for the whole
	// network is exactly the node an operator may need to stop relaying
	// for a while — without taking the node down and dropping the
	// members' control plane.
	//
	// The toggle is reported on stderr, because an operator who sends a
	// signal and sees nothing must be able to tell "it did not arrive"
	// from "it arrived and did nothing".
	if relayTrunkInst != nil {
		go watchRelayTrunkSwitch(ctx, relayTrunkInst)
		fmt.Fprintf(os.Stderr, "  relay trunk switch: SIGHUP toggles it (on ⇄ off) while the node keeps running\n")
	}

	// Approval flow: certificates dropped into <state>/certs/ are adopted
	// automatically, and external edits of members.json are reloaded. One
	// goroutine owns the live list; see watchApprovals. It stops with the
	// same signal context as everything else.
	certsDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certsDir, 0o700); err != nil {
		return ExitError{Code: 1, Err: err}
	}
	fmt.Fprintf(os.Stderr, "  certs dir: %s (drop authorized certificates here)\n", certsDir)
	if accountPubKey == nil {
		fmt.Fprintf(os.Stderr, "  NOTE: no --account-pub given; adopted certificates are checked for expiry but NOT signature-verified (pass the account key to close this)\n")
	}
	// Credential enrollment: the node admits devices that present a team
	// credential. It needs the account key to verify one, so without
	// --account-pub the endpoint is not installed at all.
	installEnrollHandler(srv, *membersPath, members, accountPubKey)
	go watchApprovals(ctx, *membersPath, certsDir, members, srv, accountPubKey)

	// D-6: announce ourselves so the node is visible (and dialable) in the
	// member view instead of showing offline forever. The announced address
	// is --announce when given, else the concrete --addr; a wildcard --addr
	// alone cannot be announced — say so instead of staying silently
	// invisible.
	selfEPS := []string{}
	// udpBound records where the direct-link listener actually bound, so
	// the port self-check can report the real UDP port.
	udpBound := []string{}
	if *announce != "" {
		selfEPS = []string{*announce}
	} else if host, port, err := net.SplitHostPort(*addr); err == nil && host != "" {
		selfEPS = []string{net.JoinHostPort(host, port)}
	}
	if len(selfEPS) > 0 {
		// D-6r: the announced address must actually carry a QUIC listener,
		// or members dial an undialable endpoint. HTTP is TCP on this port;
		// the direct link is UDP on the SAME number, so both coexist.
		//
		// The BIND address and the ANNOUNCED address are not always the
		// same: on a cloud server the public IP is a NAT mapping, not an
		// address on any local interface, so binding it fails with
		// "cannot assign requested address". Bind the wildcard on the same
		// port instead and announce the public address — the packet
		// arrives at the same socket either way.
		bindAddrs := selfEPS
		if host, port, err := net.SplitHostPort(selfEPS[0]); err == nil && host != "" {
			if !localAddr(host) {
				bindAddrs = []string{net.JoinHostPort("0.0.0.0", port)}
				fmt.Fprintf(os.Stderr, "  note: %s is a NAT-mapped address, not a local interface; binding UDP 0.0.0.0:%s for the direct link\n", host, port)
			}
		}
		if ml, lerr := mesh.NewListener(context.Background(), mesh.ListenerConfig{
			Device:  dev,
			Members: relayMembersResolver{srv: srv},
			Addrs:   bindAddrs,
			// The relay protocol RIDES on this listener's socket when no
			// separate relay port was asked for: ALPN is negotiated per
			// connection, so one UDP port serves both protocols and the
			// operator does not have to open a second port in their
			// security group (D76).
			//
			// The relay endpoints are nil here: the SAME address serves
			// both, so the member view's direct endpoints are enough.
			RelayTrunk:   relayTrunkInst,
			RelayJoiners: relayJoiners,
			// The discovery node is a full member: it receives messages and
			// files like any other node (a real deployment has the anchor
			// receiving the reports and artifacts of the network it
			// serves). Without these the node would silently discard
			// everything sent to it — the symptom seen in the first real
			// cross-internet test.
			OnMessage: func(from identity.DeviceID, text string) {
				// ★ The hook runs FIRST, so a test observes the arrival
				// even if the print below is changed or removed.
				if inboundMessageHook != nil {
					inboundMessageHook(from, text)
				}
				fmt.Fprintf(os.Stderr, "📩 from %s: %s\n", from, text)
			},
			InboxDir: filepath.Join(dir, "inbox"),
			OnFile: func(from identity.DeviceID, savedPath string, h mesh.FileHeader) {
				fmt.Fprintf(os.Stderr, "📬 from %s: %s → %s (sha256 %s…, %d bytes)\n",
					from, h.Name, savedPath, h.SHA256[:12], h.Size)
			},
		}); lerr != nil {
			fmt.Fprintf(os.Stderr, "note: cannot bind direct-link listener on %s (%v)\n", strings.Join(bindAddrs, ", "), lerr)
			fmt.Fprintf(os.Stderr, "  self-announce dropped: announcing an unreachable address would mislead dialers\n")
			selfEPS = nil
		} else {
			defer ml.Close()
			udpBound = ml.BoundAddrs()
			fmt.Fprintf(os.Stderr, "  mesh direct-link listener: UDP %s\n", strings.Join(ml.BoundAddrs(), ", "))
			// ★ THE BUFFER THE KERNEL ACTUALLY GRANTED. (Round 108.)
			// The relay protocol RIDES on this socket when there is no
			// separate relay port, so this socket's receive buffer IS
			// the relay path's buffer. A buffer below the floor makes the
			// path drop datagrams under burst, and the drop shows up as a
			// transfer that fails as a TIMEOUT with no reason.
			for _, w := range ml.BufferWarnings() {
				fmt.Fprintf(os.Stderr, "  note: %s\n", w)
			}
		}
	}

	// The RELAY ENDPOINT of this node, in the same-port shape.
	//
	// The relay protocol rides on the direct link's socket, so the relay
	// endpoint IS the direct endpoint. It has to be published as the relay
	// endpoint too, because a requester looks for a trunk's RELAY
	// endpoints (ViewPicker) — a trunk with no relay endpoint is not a
	// relay candidate at all.
	//
	// The announcement carries both lists, so the requester's view says
	// "this member accepts relays HERE".
	if len(relaySelfEPS) == 0 && relayTrunkInst != nil && len(selfEPS) > 0 {
		relaySelfEPS = append([]string(nil), selfEPS...)
		fmt.Fprintf(os.Stderr, "  relay trunk: ON — the relay protocol shares the direct-link port (UDP %s)\n",
			strings.Join(selfEPS, ", "))
	}

	// Port self-check: the operator's question is always "which port do I
	// open, and why can nobody reach me?". Answer it here, with the port
	// numbers and the action, instead of leaving it to guesswork.
	printPortCheck(checkAnchorPorts(*addr, true, udpBound))
	if len(selfEPS) > 0 {
		_, port, _ := net.SplitHostPort(*addr)
		// The self-announce must speak the same scheme the node serves:
		// under --tls-* a hardcoded http URL hits the TLS listener and dies
		// with "Client sent an HTTP request to an HTTPS server" every
		// heartbeat, leaving the anchor permanently offline with no
		// endpoints (netns-probe round 31, defect F1).
		selfURL := httpScheme + "://127.0.0.1:" + port
		if host, _, err := net.SplitHostPort(*addr); err == nil && host != "" {
			selfURL = httpScheme + "://" + net.JoinHostPort(host, port)
		}
		// The first announce fires once the relay signals Ready (its
		// listener is bound and accepting) — the loop retries at heartbeat
		// cadence after that, so a transient register failure just waits
		// for the next tick rather than reading like a fault.
		go selfAnnounceLoop(ctx, selfURL, selfEPS, relaySelfEPS, dev, srv.Ready())
	} else if *announce == "" {
		fmt.Fprintf(os.Stderr, "note: --addr is a wildcard and no --announce given: this node will show as offline\n")
		fmt.Fprintf(os.Stderr, "  in member views; pass --announce <members-facing-address>:port to make it visible\n")
	}

	// Blocks until a real error or a graceful stop (Shutdown returns nil
	// from ListenAndServe) — nil here is what makes the process exit 0 after
	// SIGTERM/SIGINT instead of landing in systemd's failed state.
	if err := srv.ListenAndServe(); err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("serve-discovery: %w", err)}
	}
	return nil
}

// resolvePortZeroEndpoints rewrites announced endpoints whose port is 0 to
// the port the listener actually bound, keeping the announced host.
//
// The reason this exists: `--listen 0` already expands and prints the bound
// port, so `--endpoint 127.0.0.1:0` reads as "same port as the listener" to
// anyone writing it — but the literal ":0" used to be announced verbatim and
// the anchor refused it as malformed (oh-no-agent, round 58, defect B-2).
// Wildcard binds announce nothing, so with no bound address to borrow a port
// from the endpoints are returned unchanged.
func resolvePortZeroEndpoints(announced, bound []string, log io.Writer) []string {
	if len(bound) == 0 {
		return announced
	}
	_, boundPort, err := net.SplitHostPort(bound[0])
	if err != nil || boundPort == "" || boundPort == "0" {
		return announced
	}
	out := make([]string, len(announced))
	copy(out, announced)
	for i, a := range announced {
		host, port, err := net.SplitHostPort(a)
		if err != nil || port != "0" {
			continue
		}
		out[i] = net.JoinHostPort(host, boundPort)
		if log != nil {
			fmt.Fprintf(log, "  note: --endpoint port 0 resolved to the bound port %s\n", boundPort)
		}
	}
	return out
}

// localAddr reports whether host is an address of this machine's own
// interfaces. A cloud server's public IP is a NAT mapping and never appears
// on an interface, so binding it directly fails — the wildcard on the same
// port is the correct bind target.
func localAddr(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	// Loopback is local BY DEFINITION: an operator who binds
	// --addr 127.0.0.1 said "loopback only", and the NAT branch below used
	// to widen the UDP direct-link listener to 0.0.0.0 behind their back
	// (kuku round 51 saw the wildcard in /proc/net/udp). The hostname form
	// above already made the same decision for "localhost".
	if ip.IsLoopback() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// selfAnnounceLoop posts the discovery node's own signed announce on the
// heartbeat interval: the node is a member, and members announce. Failures
// are logged, never fatal — a node that cannot reach itself via its own
// published address is a deployment problem worth one log line per attempt,
// not a crash.
func selfAnnounceLoop(ctx context.Context, selfURL string, eps []string, relayEPS []string, dev *identity.Device, ready <-chan struct{}) {
	// Gate the first announce on the listener actually accepting (issue #8
	// rounds 10/11: announcing before the socket accepts races the bind and
	// loses — measured refusal rates 6%→86% across two commits). A 15s
	// fallback covers the listen-failed case without hanging forever.
	select {
	case <-ready:
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
		fmt.Fprintf(os.Stderr, "note: listener not ready within 15s; self-announce deferred\n")
		return
	}
	post := func() {
		a := discovery.MemberAnnounce{
			DeviceID:       dev.ID(),
			Endpoints:      eps,
			RelayEndpoints: relayEPS,
			Timestamp:      time.Now(),
		}
		req := discovery.RegisterRequest{Announce: a, Sig: a.Sign(dev)}
		b, err := json.Marshal(req)
		if err != nil {
			return
		}
		resp, err := http.Post(selfURL+"/mesh/register", "application/json", bytes.NewReader(b))
		if err != nil {
			fmt.Fprintf(os.Stderr, "self-announce failed: %v\n", err)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "self-announce rejected: HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
	post()
	t := time.NewTicker(discovery.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			post()
		}
	}
}

// loadOrBootstrapMembers reads the member list, or creates one bound to the
// device's own certificate when the file does not exist yet. The first node
// in a network thus becomes its own first member.
func loadOrBootstrapMembers(path string, dev *identity.Device) (*identity.MemberList, bool, error) {
	if _, err := os.Stat(path); err == nil {
		m, err := identity.LoadMembers(path)
		if err != nil {
			return nil, false, err
		}
		if !m.Contains(dev.ID()) {
			return nil, false, fmt.Errorf(
				"%s does not contain this device (%s); a discovery node must itself be a member — restore the entry or delete the file to bootstrap a new network", path, dev.ID())
		}
		return m, false, nil
	}
	certPath := identity.IdentityPath(filepath.Dir(path), string(dev.ID())+identity.CertSuffix)
	cert, err := identity.LoadCert(certPath)
	if err != nil {
		return nil, false, fmt.Errorf(
			"no member list at %s and no certificate for this device at %s; run cmesh account device-add first: %w", path, certPath, err)
	}
	m := identity.NewMemberList(cert.AccountID)
	if err := m.Add(*cert); err != nil {
		return nil, false, err
	}
	if err := m.SaveMembers(path); err != nil {
		return nil, false, err
	}
	return m, true, nil
}

// watchApprovals owns the live member list and applies both approval paths:
//
//  1. certificate adoption — every *.cert.json in certsDir not yet in the
//     list is added (Add is idempotent for identical certs; conflicting
//     certs are refused by identity and logged), the list is saved back to
//     membersPath. When accountPub is provided, each certificate is
//     signature-verified against the account key BEFORE it can enter the
//     list — a tampered cert arriving over the copy channel is refused
//     instead of poisoning the roster (review F2);
//  2. external reload — an operator may edit members.json directly; the
//     file wins over in-memory state, but only for the SAME account (a
//     wrong-file clobber is refused, not merged).
//
// Either way the version pushed to clients must be monotonic (they ignore
// decreasing versions to defeat stale-snapshot replays), so a reloaded file
// whose version went backwards is bumped past the current one.
func watchApprovals(ctx context.Context, membersPath, certsDir string, start *identity.MemberList, srv *relay.Server, accountPub []byte) {
	cur := start
	// lastMembers records the mtime of the file AS WE LAST LEFT IT (either
	// read it or wrote it). Our own SaveMembers refreshes the mtime — without
	// re-reading it here, our own save would register as an external edit and
	// the watcher would reload+save in a 2-second loop forever, inflating the
	// member-list version and re-pushing identical snapshots to every member
	// (issue #8 D-3: 201 self-triggered reloads, v3→v208, zero changes).
	var lastMembers time.Time
	if st, err := os.Stat(membersPath); err == nil {
		lastMembers = st.ModTime()
	}
	noteFileState := func() {
		if st, err := os.Stat(membersPath); err == nil {
			lastMembers = st.ModTime()
		}
	}
	// Adopted files are tracked by mtime so a junk file logs once, not on
	// every tick.
	seen := map[string]time.Time{}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	apply := func(m *identity.MemberList, what string) {
		cur = m
		srv.SetMeshMembers(m)
		fmt.Fprintf(os.Stderr, "membership %s: %d member(s), v%d\n", what, m.Count(), m.CurrentVersion())
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			changed := false

			// External edit wins first, so adoption on this tick lands on
			// top of the fresh file rather than a stale list.
			if st, err := os.Stat(membersPath); err == nil && st.ModTime().After(lastMembers) {
				m, err := identity.LoadMembers(membersPath)
				switch {
				case err != nil:
					fmt.Fprintf(os.Stderr, "members file reload failed: %v\n", err)
					noteFileState() // don't retry a broken file every tick forever
				case m.AccountID != cur.AccountID:
					fmt.Fprintf(os.Stderr, "members file reload refused: file is for account %s, network is %s\n", m.AccountID, cur.AccountID)
					noteFileState()
				default:
					if m.Version <= cur.CurrentVersion() {
						m.Version = cur.CurrentVersion() + 1
					}
					apply(m, "reloaded from file")
					changed = true
				}
			}

			ents, err := os.ReadDir(certsDir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				name := e.Name()
				if !strings.HasSuffix(name, identity.CertSuffix) {
					continue
				}
				st, err := e.Info()
				if err != nil {
					continue
				}
				if seen[name] == st.ModTime() {
					continue
				}
				seen[name] = st.ModTime()
				cert, err := identity.LoadCert(filepath.Join(certsDir, name))
				if err != nil {
					fmt.Fprintf(os.Stderr, "cert file %s unreadable: %v\n", name, err)
					continue
				}
				if cert.AccountID != cur.AccountID {
					fmt.Fprintf(os.Stderr, "cert file %s refused: belongs to account %s, network is %s\n", name, cert.AccountID, cur.AccountID)
					continue
				}
				// Signature verification runs against the account key when
				// the operator provided it; expiry is ALWAYS enforced — a
				// lapsed cert must not re-enter the roster through the side
				// door (review F2/F3).
				if accountPub != nil {
					if err := cert.Verify(ed25519.PublicKey(accountPub)); err != nil {
						fmt.Fprintf(os.Stderr, "cert file %s refused: verification failed: %v\n", name, err)
						continue
					}
				} else if cert.Expired(time.Now()) {
					fmt.Fprintf(os.Stderr, "cert file %s refused: certificate expired at %s\n", name, cert.ExpiresAt.Format(time.RFC3339))
					continue
				}
				if err := cur.Add(*cert); err != nil {
					fmt.Fprintf(os.Stderr, "cert file %s rejected: %v\n", name, err)
					continue
				}
				fmt.Fprintf(os.Stderr, "adopted certificate %s: device %s\n", name, cert.DeviceID)
				changed = true
			}
			if changed {
				// The MERGED save: `cur` is this watcher's in-memory roster,
				// and it can be a couple of seconds BEHIND the file — the
				// operator's `cmesh cred revoke` writes the file from
				// another process. A plain save would write that stale
				// view back over the file and ERASE the revocation, and
				// the revoked credential could enrol again. Measured on the
				// real machine: two revoked ids in the file, one
				// enrolment, and the revocation list was gone.
				if err := cur.SaveMembersMerged(membersPath); err != nil {
					fmt.Fprintf(os.Stderr, "members save failed: %v\n", err)
				}
				// Our own save refreshed the file's mtime: record it, or the
				// next tick would treat our write as an external edit (D-3).
				noteFileState()
				srv.SetMeshMembers(cur)
			}
		}
	}
}

// certRenewIntervalHook is nil in production; a test uses it to compress
// the renewal interval so a certificate that would renew in months renews
// in seconds.
var certRenewIntervalHook func(remaining time.Duration) time.Duration

// certRenewLoop re-enrolls this device before its certificate lapses.
//
// The certificate's lifetime used to be the whole membership: when it
// expired the anchor rejected the device and it fell out of the network
// SILENTLY — no error, no renewal, just gone (the real-machine finding
// behind D77). The anchor already accepts a renewal (MemberList.Add
// refreshes the certificate for the same device key and account, and the
// quota is counted from the roster, so a renewal does not consume a slot);
// the missing half was the client: nobody ever asked for a fresh one.
//
// The loop re-enrolls at a quarter of the credential's remaining life,
// bounded to [1 h, 30 d]. Each renewal mints a certificate valid until the
// CREDENTIAL expires — so a member stays exactly as long as its
// credential does, and when the owner turns the credential (`cred revoke`
// or its expiry) the membership really does end, which is the ruling.
//
// Renewal is idempotent on the anchor: the roster entry is refreshed and
// the fresh view is pushed to every member.
func certRenewLoop(ctx context.Context, cred *credential.Credential, dev *identity.Device, transport http.RoundTripper) {
	if cred == nil || dev == nil {
		return
	}
	for {
		remaining := time.Until(cred.Public.ExpiresAt)
		if remaining <= 0 {
			// The credential is over. The current certificate still works
			// until IT lapses; nothing can extend it past its authority.
			fmt.Fprintf(os.Stderr, "credential expired at %s; this device's membership ends when its certificate lapses\n",
				cred.Public.ExpiresAt.UTC().Format(time.RFC3339))
			return
		}
		interval := remaining / 4
		if interval < time.Hour {
			interval = time.Hour
		}
		if interval > 30*24*time.Hour {
			interval = 30 * 24 * time.Hour
		}
		if certRenewIntervalHook != nil {
			interval = certRenewIntervalHook(remaining)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		ectx, cancel := context.WithTimeout(ctx, enrollTimeout)
		err := enrollWithCredentialUsing(ectx, cred, dev, transport)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// An expired credential is TERMINAL, not transient: retrying
			// cannot fix it, and the device's membership now has a definite
			// end (when its current certificate lapses). Any other failure
			// is worth a retry.
			if time.Until(cred.Public.ExpiresAt) <= 0 {
				fmt.Fprintf(os.Stderr, "credential expired at %s; this device's membership ends when its certificate lapses\n",
					cred.Public.ExpiresAt.UTC().Format(time.RFC3339))
				return
			}
			fmt.Fprintf(os.Stderr, "certificate renewal failed (%v); will retry\n", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "certificate renewed (the credential runs to %s)\n",
			cred.Public.ExpiresAt.UTC().Format(time.RFC3339))
	}
}

// joinRelayTrunk keeps this device connected to a relay trunk.
//
// The connection is what makes a pure-outbound member reachable: it has
// no inbound port, so the anchor has to come to it, and it does that over
// this connection.
//
// The trunk's device ID is required: the join is PINNED against the
// member view's key for that device, so a member cannot be tricked into
// joining an impostor.
//
// The RETRY lives HERE, not inside mesh.JoinRelayTrunk. The old code
// looped forever inside the mesh package, so this function's error
// message never printed and a member that could not join failed
// SILENTLY (defect G of the round-61 real-machine report). The retry is
// the caller's job: it has the log line, and it can decide whether a
// failure is worth retrying.
func joinRelayTrunk(ctx context.Context, dev *identity.Device, resolver *liveViewResolver, trunkID identity.DeviceID, endpoint, inboxDir string) {
	// relayTrunkRetryInitial is how long the member waits before
	// dialling the trunk again, both after a failure and after a
	// connection that ended on its own.
	//
	// It is a named constant because the two uses must agree: the
	// backoff after a FAILURE grows from here, and the wait after a
	// connection that ended by itself starts from here too (the
	// accumulated backoff is for failures, and a trunk that restarted
	// is not a failure — it is the one node this member can always
	// reach).
	const relayTrunkRetryInitial = 2 * time.Second
	backoff := relayTrunkRetryInitial
	for {
		err := mesh.JoinRelayTrunk(ctx, mesh.RelayJoinConfig{
			Device:    dev,
			Resolver:  resolver,
			Trunk:     trunkID,
			Endpoints: []string{endpoint},
			OnSession: func(sess mesh.Session, requester identity.DeviceID, kind string) error {
				// The relayed session is served with the SAME session
				// dispatch the direct link uses, so a member-to-member
				// message that arrives through a trunk is handled
				// exactly like one that arrives directly.
				//
				// The KIND comes from the relay's open record: it is the
				// kind the requester asked for, so the target serves the
				// handler that kind always runs.
				if kind == "" {
					kind = mesh.KindMsg
				}
				return mesh.ServeRelayedSession(kind, requester, sess, mesh.ListenerConfig{
					OnMessage: func(from identity.DeviceID, text string) {
						fmt.Fprintf(os.Stderr, "📩 from %s: %s\n", from, text)
					},
					InboxDir: inboxDir,
					OnFile: func(from identity.DeviceID, savedPath string, h mesh.FileHeader) {
						fmt.Fprintf(os.Stderr, "📬 from %s: %s → %s (sha256 %s…, %d bytes)\n",
							from, h.Name, savedPath, h.SHA256[:12], h.Size)
					},
				})
			},
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "relay trunk join failed (%v); retrying in %s\n", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			// The backoff grows, but it is capped: the anchor is the one
			// node this member can always reach, so giving up on it is
			// not an option — the operator just does not want the log
			// line every two seconds forever.
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		// The connection worked, so the failure counter starts over.
		backoff = relayTrunkRetryInitial

		// The connection ended on its own (the trunk restarted). Wait a
		// beat before dialling again, so a restarting anchor is not
		// hammered.
		//
		// The wait is the INITIAL interval, not the accumulated backoff.
		//
		// The backoff only ever GREW (it was declared outside the loop
		// and reset nowhere), so after a few failed attempts it sat at
		// its 30-second cap — and a member whose trunk had been up for
		// a while reconnected after the anchor restarted with THAT wait.
		// Measured by an external tester: the anchor restarted, and the
		// pure-outbound member was unreachable for 15-18 seconds while
		// it waited out a backoff it had accumulated from failures long
		// ago.
		//
		// A successful connection RESETS the backoff (it is the failure
		// counter, after all), and a connection that ends on its own gets
		// the initial interval: the anchor is the one node this member
		// can always reach, so reconnecting promptly matters more than
		// not hammering it.
		backoff = relayTrunkRetryInitial
		select {
		case <-ctx.Done():
			return
		case <-time.After(relayTrunkRetryInitial):
		}
	}
}
