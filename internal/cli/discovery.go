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
	"strings"
	"sync/atomic"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/mesh"
	"github.com/muzimu217/container-mesh/internal/relay"
)

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
	s, err := discovery.EncodeInvite(inv)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	fmt.Fprintf(os.Stderr, "invite for account %s (valid advice for ~%s):\n\n", acc.ID(), *ttl)
	fmt.Println(s)
	fmt.Fprintf(os.Stderr, "\nPossession of this string does NOT grant access (R-1.5): after the joiner\nruns `cmesh join`, add their certificate on a member machine first.\n")
	return nil
}

// discoveryConfig is the persisted join state, so later commands (dial, the
// future daemon) can re-derive the same discovery setup without re-inviting.
type discoveryConfig struct {
	Invite    string    `json:"invite"`
	Endpoints []string  `json:"endpoints,omitempty"`
	SavedAt   time.Time `json:"saved_at"`
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

func saveDiscoveryConfig(dir string, cfg discoveryConfig) (string, error) {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "discovery.json")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, b, 0o600)
}

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
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for approval before giving up (0 = forever)")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if *once && len(listen) > 0 {
		return ExitError{Code: 2, Err: errors.New(
			"--once exits after approval; --listen needs a serving process (drop --once)")}
	}
	if fs.NArg() != 1 {
		return ExitError{Code: 2, Err: errors.New(
			"usage: cmesh join [flags] <invite-string> (the invite goes last: flag parsing stops at the first positional argument)")}
	}

	inv, err := discovery.DecodeInvite(fs.Arg(0))
	if err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("bad invite: %w", err)}
	}

	dir, err := stateDir(*state)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	devPath := identity.IdentityPath(dir, identity.DeviceFileName)
	if _, err := os.Stat(devPath); err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf(
			"%s not found; this device needs its own identity first: run cmesh init, then ask the account owner to sign it in (cmesh account device-add)", devPath)}
	}
	phrase, err := readPassphrase(*pass, "device passphrase")
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	dev, err := identity.LoadDevice(devPath, phrase)
	if err != nil {
		return ExitError{Code: 1, Err: err}
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
	if len(listen) > 0 {
		l, err := mesh.NewListener(context.Background(), mesh.ListenerConfig{
			Device:  dev,
			Members: resolver,
			Addrs:   []string(listen),
		})
		if err != nil {
			return ExitError{Code: 1, Err: err}
		}
		listener = l
		defer listener.Close()
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
	}

	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      inv.DiscoveryAddrs,
		Device:     dev,
		AccountID:  inv.AccountID,
		AccountPub: []byte(inv.AccountPub),
		Endpoints:  announced,
		Logger:     setupLogging(false),
	})
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	if listener != nil {
		resolver.set(cl)
		fmt.Fprintf(os.Stderr, "listening for direct links on %s\n", strings.Join(listener.BoundAddrs(), ", "))
	}

	// Persisted only after every check passed: a failed join must not leave
	// a config file that looks approved (review F9).
	cfgPath, err := saveDiscoveryConfig(dir, discoveryConfig{
		Invite:    fs.Arg(0),
		Endpoints: []string(eps),
		SavedAt:   time.Now(),
	})
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	fmt.Fprintf(os.Stderr, "joining account %s via %s\n", inv.AccountID, strings.Join(inv.DiscoveryAddrs, ", "))
	fmt.Fprintf(os.Stderr, "  device   %s\n", dev.ID())
	fmt.Fprintf(os.Stderr, "  config   %s\n", cfgPath)
	fmt.Fprintf(os.Stderr, "\nWaiting for membership approval (R-1.5). To approve this device:\n")
	fmt.Fprintf(os.Stderr, "  1. copy the line below to the account owner\n")
	fmt.Fprintf(os.Stderr, "  2. owner runs: cmesh account authorize --pub <that value>\n")
	fmt.Fprintf(os.Stderr, "  3. the cert file goes into the discovery host's certs directory\n\n")
	fmt.Fprintf(os.Stderr, "PUBKEY: %s\n\n", base64.StdEncoding.EncodeToString(dev.PublicKey()))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	go func() { _ = cl.Run(ctx) }()

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
			fmt.Fprintf(os.Stderr, "  %-16s %s%s\n", id, state, mark)
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
		online := make([]string, 0, len(snap.Members))
		for id, mv := range snap.Members {
			if mv.Online {
				online = append(online, id.String())
			}
		}
		sort.Strings(online)
		return fmt.Sprintf("v%d|%s", snap.Version, strings.Join(online, ","))
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
					if *once {
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
			}
		case <-ctx.Done():
			if cl.Joined() {
				return nil
			}
			if ctx.Err() == context.DeadlineExceeded {
				return ExitError{Code: 1, Err: fmt.Errorf(
					"not approved within %s; the account owner must add this device's certificate to the member list on the discovery host", *timeout)}
			}
			return nil
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
			"%s not found; a discovery node must itself be a network member: run cmesh init && cmesh account create && cmesh account device-add first", devPath)}
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
	scheme := "ws"
	if *tlsCert != "" {
		scheme = "wss"
	}
	fmt.Fprintf(os.Stderr, "discovery node listening on %s://%s\n", scheme, strings.TrimPrefix(*addr, ":"))
	fmt.Fprintf(os.Stderr, "  register: POST   http://%s/mesh/register\n", *addr)
	fmt.Fprintf(os.Stderr, "  members:  WS     http://%s/mesh/members\n", *addr)
	fmt.Fprintf(os.Stderr, "  members file: %s (edited externally; hot-reloaded)\n", *membersPath)
	fmt.Fprintf(os.Stderr, "  pairing relay and dashboard unchanged: %s%s, /dashboard\n", scheme, *addr)

	srv := relay.New(relay.Config{
		Addr:    *addr,
		Path:    *path,
		TLSCert: *tlsCert,
		TLSKey:  *tlsKey,
		Logger:  setupLogging(*verbose),
	})
	srv.EnableDiscovery(members)

	// Approval flow: certificates dropped into <state>/certs/ are adopted
	// automatically, and external edits of members.json are reloaded. One
	// goroutine owns the live list; see watchApprovals.
	certsDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certsDir, 0o700); err != nil {
		return ExitError{Code: 1, Err: err}
	}
	fmt.Fprintf(os.Stderr, "  certs dir: %s (drop authorized certificates here)\n", certsDir)
	if accountPubKey == nil {
		fmt.Fprintf(os.Stderr, "  NOTE: no --account-pub given; adopted certificates are checked for expiry but NOT signature-verified (pass the account key to close this)\n")
	}
	go watchApprovals(context.Background(), *membersPath, certsDir, members, srv, accountPubKey)

	// D-6: announce ourselves so the node is visible (and dialable) in the
	// member view instead of showing offline forever. The announced address
	// is --announce when given, else the concrete --addr; a wildcard --addr
	// alone cannot be announced — say so instead of staying silently
	// invisible.
	selfEPS := []string{}
	if *announce != "" {
		selfEPS = []string{*announce}
	} else if host, port, err := net.SplitHostPort(*addr); err == nil && host != "" {
		selfEPS = []string{net.JoinHostPort(host, port)}
	}
	if len(selfEPS) > 0 {
		_, port, _ := net.SplitHostPort(*addr)
		selfURL := "http://127.0.0.1:" + port
		if host, _, err := net.SplitHostPort(*addr); err == nil && host != "" {
			selfURL = "http://" + net.JoinHostPort(host, port)
		}
		go selfAnnounceLoop(context.Background(), selfURL, selfEPS, dev)
	} else {
		fmt.Fprintf(os.Stderr, "note: --addr is a wildcard and no --announce given: this node will show as offline\n")
		fmt.Fprintf(os.Stderr, "  in member views; pass --announce <members-facing-address>:port to make it visible\n")
	}

	if err := srv.ListenAndServe(); err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("serve-discovery: %w", err)}
	}
	return nil
}

// selfAnnounceLoop posts the discovery node's own signed announce on the
// heartbeat interval: the node is a member, and members announce. Failures
// are logged, never fatal — a node that cannot reach itself via its own
// published address is a deployment problem worth one log line per attempt,
// not a crash.
func selfAnnounceLoop(ctx context.Context, selfURL string, eps []string, dev *identity.Device) {
	post := func() {
		a := discovery.MemberAnnounce{DeviceID: dev.ID(), Endpoints: eps, Timestamp: time.Now()}
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
				} else if time.Now().UTC().After(cert.ExpiresAt) {
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
				if err := cur.SaveMembers(membersPath); err != nil {
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
