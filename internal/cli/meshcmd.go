// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// Mesh direct-link commands: `cmesh mesh ping` and `cmesh dial`.
//
// Both resolve the target's endpoints from the LIVE member view (discovery),
// then bring up a mutually-authenticated direct QUIC link (device
// certificates pinned to the member view). PR-M3 semantics: ping measures
// RTT; dial is an interactive duplex channel to the peer's listener (echo),
// the transport-validation predecessor of PR-M4's real services.

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/mesh"
)

// meshSession bundles what a one-shot mesh command needs: identity, the
// persisted discovery config, and a live client with its resolver.
type meshSession struct {
	dev      *identity.Device
	cl       *discovery.Client
	resolver *liveViewResolver
}

// openMeshSession loads state and starts a discovery client, returning once
// the first verified snapshot has arrived (or the context ends). The
// passphrase goes through readPassphrase so CMESH_PASSPHRASE works exactly
// as it does for init/join — the container-deploy path in the guide depends
// on it (issue #8 D-5).
func openMeshSession(ctx context.Context, state, passFlag string, eps []string) (*meshSession, error) {
	return openMeshSessionWithSnapshotTimeout(ctx, state, passFlag, eps, 15*time.Second)
}

func openMeshSessionWithSnapshotTimeout(ctx context.Context, state, passFlag string, eps []string, snapshotTimeout time.Duration) (*meshSession, error) {
	dir, err := stateDir(state)
	if err != nil {
		return nil, err
	}
	devPath := identity.IdentityPath(dir, identity.DeviceFileName)
	if _, err := os.Stat(devPath); err != nil {
		return nil, fmt.Errorf("%s not found; run cmesh init and cmesh join first", devPath)
	}
	pass, err := readPassphrase(passFlag, "device passphrase")
	if err != nil {
		return nil, err
	}
	dev, err := identity.LoadDevice(devPath, pass)
	if err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(dir, "discovery.json")
	blob, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("%s not found; this device has not joined a network yet (run cmesh join)", cfgPath)
	}
	var cfg discoveryConfig
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return nil, fmt.Errorf("%s is not a valid discovery config: %w", cfgPath, err)
	}
	// cfg.invite() prefers the fields join derived (account + addresses) and
	// only falls back to decoding the raw string for configs written by an
	// older build. Re-parsing the raw string unconditionally broke every
	// follow-up command on a credential-joined device (defect B-3).
	inv, err := cfg.invite()
	if err != nil {
		return nil, fmt.Errorf("saved join configuration is unusable: %w", err)
	}
	// Announce only what we KNOW: endpoints from --endpoint flags, else the
	// set saved by the join that created this config. With neither, the
	// one-shot client runs view-only (NoAnnounce) — this machine's real
	// announcer is its long-running join process, and an empty announce from
	// here would blank its advertised endpoints for up to a heartbeat,
	// making the machine undialable while we merely wanted to LOOK (issue #8
	// D-7: a mesh ping used to knock the pinging machine itself offline in
	// every other member's view).
	known := append([]string(nil), eps...)
	if len(known) == 0 {
		known = append([]string(nil), cfg.Endpoints...)
	}
	cl, err := discovery.NewClient(withDiscoveryClientTransport(discovery.ClientConfig{
		Addrs:                 inv.DiscoveryAddrs,
		Device:                dev,
		AccountID:             inv.AccountID,
		AccountPub:            []byte(inv.AccountPub),
		Endpoints:             known,
		NoAnnounce:            len(known) == 0,
		AllowLocalDiscovery:   discoveryAllowLocal(),
		AllowPrivateDiscovery: discoveryAllowPrivate(),
	}))
	if err != nil {
		return nil, err
	}
	go func() { _ = cl.Run(ctx) }()

	if snapshotTimeout <= 0 {
		snapshotTimeout = 15 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-waitCtx.Done():
			if cl.Snapshot() == nil {
				// Distinguish the two failures a joiner can hit: the node is
				// unreachable, or THIS DEVICE IS NOT A MEMBER (typically:
				// revoked). The register response already carries the
				// authoritative reason — a 401 "not a member" is a certainty
				// about membership, not a network fault, and the message must
				// say so instead of sending the user off to debug routing
				// (issue #8, report 1 finding on the revocation UX).
				if perr := cl.Announce(waitCtx); perr != nil && discovery.IsNotAMember(perr) {
					return nil, fmt.Errorf("this device has been removed from this network (revoked by the account owner, or the approval never happened)")
				}
				return nil, fmt.Errorf("no member view within %s (discovery node unreachable, or not approved yet?)", snapshotTimeout)
			}
			resolver := &liveViewResolver{accountPub: []byte(inv.AccountPub)}
			resolver.set(cl)
			return &meshSession{dev: dev, cl: cl, resolver: resolver}, nil
		case <-t.C:
			if cl.Snapshot() != nil {
				resolver := &liveViewResolver{accountPub: []byte(inv.AccountPub)}
				resolver.set(cl)
				return &meshSession{dev: dev, cl: cl, resolver: resolver}, nil
			}
		}
	}
}

// resolveTarget waits until the target appears in the live view, and
// returns its direct endpoints.
//
// A target with NO endpoints is NOT an error. It is a PURE-OUTBOUND
// member: it has no inbound port, so it can only be reached THROUGH a
// relay. The old code refused it outright ("has announced no endpoints
// (is it running with --listen?)"), which made the relay path
// unreachable from the product CLI — `mesh ping`, `msg`, `send-to`,
// `run` and `bench` all go through here, so none of them could ever
// take the relay path (defect F of the round-61 real-machine report).
//
// The picker decides the path: it offers the direct path first (with an
// empty endpoint list, which the dialer refuses) and the relay
// candidates after it. So returning an empty list is exactly right: the
// direct attempt fails fast and the relay attempt follows.
//
// What IS still an error is a target that is not a member at all.
func (s *meshSession) resolveTarget(ctx context.Context, target identity.DeviceID) ([]string, error) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		snap := s.cl.Snapshot()
		if snap != nil {
			if mv, ok := snap.Members[target]; ok {
				// A member with zero endpoints is reachable through a
				// relay — that is the whole point of the trunk. The
				// endpoints list may be empty.
				return mv.Endpoints, nil
			}
			return nil, fmt.Errorf("%s is not a member of this network", target)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for %s to appear with endpoints", target)
		case <-t.C:
		}
	}
}

func snapContains(snap *discovery.MemberSnapshot, id identity.DeviceID) bool {
	_, ok := snap.Members[id]
	return ok
}

// picker builds the path picker for this session.
//
// The picker is what makes the RELAY path reachable from the CLI: it is
// asked for the paths to the target, and it answers with the direct path
// first and the relay candidates after (D73: "中继只兜底：大文件走直连，
// 直连失败再走中继").
//
// The view is read at PICK time, not captured now: the member view
// changes while the command runs (a member leaves, an endpoint changes),
// and a picker built on a stale snapshot would keep offering paths that
// cannot work.
//
// The target key comes from the LIVE view too — the relayed session's
// end-to-end handshake is verified against it, and the relay terminates
// TLS on both hops, so there is no certificate to pin.
func (s *meshSession) picker() mesh.PathPicker {
	return mesh.ViewPicker{
		View: func() *discovery.MemberSnapshot {
			return s.cl.Snapshot()
		},
		Self: s.dev.ID(),
		TargetKey: func(id identity.DeviceID) (ed25519.PublicKey, error) {
			return s.resolver.KeyFor(id)
		},
	}
}

func runMesh(args []string) error {
	if len(args) == 0 {
		return ExitError{Code: 2, Err: errors.New("mesh needs a subcommand: ping")}
	}
	switch args[0] {
	case "ping":
		return runMeshPing(args[1:])
	default:
		return ExitError{Code: 2, Err: fmt.Errorf("unknown mesh subcommand %q (known: ping)", args[0])}
	}
}

// runRun implements `cmesh run <deviceID> <verb> [args…]`: invoke a built-in
// read-only verb on a member (@info @disk @mem @cat @ls). No process
// execution — verbs are implemented natively on the receiver.
func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("device-passphrase", "", "passphrase for the device file (for scripting)")
	timeout := fs.Duration("timeout", 30*time.Second, "budget for discovery and the verb")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if fs.NArg() < 2 {
		return ExitError{Code: 2, Err: errors.New(
			"usage: cmesh run [flags] <deviceID> <@verb> [args…]  (built-ins: @info @disk @mem @cat @ls)")}
	}
	target := identity.DeviceID(fs.Arg(0))
	if !target.Valid() {
		return ExitError{Code: 2, Err: fmt.Errorf("%q is not a valid device ID", fs.Arg(0))}
	}
	verb := fs.Arg(1)
	verbArgs := fs.Args()[2:]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	hctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	sess, err := openMeshSession(hctx, *state, *pass, nil)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	eps, err := sess.resolveTarget(hctx, target)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	code, err := mesh.SendRun(hctx, mesh.DialConfig{
		Device:    sess.dev,
		Resolver:  sess.resolver,
		Target:    target,
		Endpoints: eps,
		Picker:    sess.picker(),
	}, verb, verbArgs)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	// Propagate the remote exit code, ssh-style.
	return ExitError{Code: code}
}

// runMsg implements `cmesh msg <deviceID> --text "…"`: deliver a message to
// a member over a direct link. The receiver's join --listen process prints
// it live.
func runMsg(args []string) error {
	fs := flag.NewFlagSet("msg", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("device-passphrase", "", "passphrase for the device file (for scripting)")
	text := fs.String("text", "", "message text; without it, one message per stdin line")
	timeout := fs.Duration("timeout", 20*time.Second, "budget for discovery and delivery")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if fs.NArg() != 1 {
		return ExitError{Code: 2, Err: errors.New("usage: cmesh msg [flags] <deviceID> (device ID goes last)")}
	}
	target := identity.DeviceID(fs.Arg(0))
	if !target.Valid() {
		return ExitError{Code: 2, Err: fmt.Errorf("%q is not a valid device ID", fs.Arg(0))}
	}
	var texts []string
	if *text != "" {
		texts = append(texts, *text)
	} else {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				texts = append(texts, line)
			}
		}
	}
	if len(texts) == 0 {
		return ExitError{Code: 2, Err: errors.New("nothing to send: pass --text or pipe lines on stdin")}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	hctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	sess, err := openMeshSession(hctx, *state, *pass, nil)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	eps, err := sess.resolveTarget(hctx, target)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	if err := mesh.SendMsg(hctx, mesh.DialConfig{
		Device:    sess.dev,
		Resolver:  sess.resolver,
		Target:    target,
		Endpoints: eps,
		Picker:    sess.picker(),
	}, texts); err != nil {
		return ExitError{Code: 1, Err: err}
	}
	fmt.Fprintf(os.Stderr, "delivered %d message(s) to %s\n", len(texts), target)
	return nil
}

// runSendTo implements `cmesh send-to <deviceID> --file PATH`: stream a file
// to a member over a direct link with sha256 verification (R-4.2). The
// receiver saves it into its inbox.
func runSendTo(args []string) error {
	fs := flag.NewFlagSet("send-to", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("device-passphrase", "", "passphrase for the device file (for scripting)")
	file := fs.String("file", "", "file to send (required)")
	force := fs.Bool("force", false,
		"replace a file the receiver already has with the same name (the receiver refuses by default)")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall budget; large files need real time")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if fs.NArg() != 1 {
		return ExitError{Code: 2, Err: errors.New("usage: cmesh send-to [flags] --file PATH <deviceID> (device ID goes last)")}
	}
	if *file == "" {
		return ExitError{Code: 2, Err: errors.New("send-to needs --file PATH")}
	}
	target := identity.DeviceID(fs.Arg(0))
	if !target.Valid() {
		return ExitError{Code: 2, Err: fmt.Errorf("%q is not a valid device ID", fs.Arg(0))}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	hctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	sess, err := openMeshSession(hctx, *state, *pass, nil)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	eps, err := sess.resolveTarget(hctx, target)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	h, err := mesh.SendFile(hctx, mesh.DialConfig{
		Device:    sess.dev,
		Resolver:  sess.resolver,
		Target:    target,
		Endpoints: eps,
		Picker:    sess.picker(),
		// ★ THE RECEIVER REFUSES TO REPLACE A FILE UNLESS THE SENDER
		// ASKS IT TO. (Round 99.) The receiver used to rename over
		// whatever was there, so a second transfer of the same name
		// REPLACED the first one silently — and the sender was told
		// "done <hash>". The stage-1 path has had that rule for a while
		// (`--force` there); the mesh line did not carry it over.
		ForceOverwrite: *force,
	}, *file)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	fmt.Fprintf(os.Stderr, "sent %s (%d bytes) to %s\n  sha256 %s — receiver verified\n",
		h.Name, h.Size, target, h.SHA256)
	return nil
}

func runMeshPing(args []string) error {
	fs := flag.NewFlagSet("mesh ping", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("device-passphrase", "", "passphrase for the device file (for scripting)")
	count := fs.Int("count", 10, "pings to send (1-1000)")
	interval := fs.Duration("interval", 200*time.Millisecond, "delay between pings (minimum 100ms)")
	timeout := fs.Duration("timeout", 0, "overall budget; default covers discovery plus all samples")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if fs.NArg() != 1 {
		return ExitError{Code: 2, Err: errors.New(
			"usage: cmesh mesh ping [flags] <deviceID> (the device ID goes last: flag parsing stops at the first positional argument)")}
	}
	if *count < 1 || *count > 1000 {
		return ExitError{Code: 2, Err: errors.New("mesh ping --count must be between 1 and 1000 packets")}
	}
	if *interval < 100*time.Millisecond {
		return ExitError{Code: 2, Err: errors.New("mesh ping --interval must be at least 100ms")}
	}
	if *timeout <= 0 {
		// Discovery has its own 15s startup budget. The sampling window must
		// also fit: count=1000 at the minimum 100ms interval is 99.9s before
		// the one-second per-reply timeout margin. A fixed 20s default made
		// the advertised max count stop at roughly 196 packets (oh-no-agent,
		// c530c7d/1f64cdc retest).
		*timeout = 15*time.Second + time.Duration(*count-1)*(*interval) + time.Duration(*count)*time.Second
	}
	target := identity.DeviceID(fs.Arg(0))
	if !target.Valid() {
		return ExitError{Code: 2, Err: fmt.Errorf("%q is not a valid device ID", fs.Arg(0))}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	sess, err := openMeshSession(ctx, *state, *pass, nil)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	eps, err := sess.resolveTarget(ctx, target)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}

	// The picker is what makes the RELAY path reachable: the direct
	// path is tried first, and the relay candidates after it (D73).
	//
	// The path that was actually used is REPORTED: "link=relay via=<id>"
	// is the only way an operator can tell that the ping went through a
	// trunk — and a ping that silently took the relay would look like a
	// direct ping with a worse RTT.
	link, used, err := mesh.DialViaPickerPath(ctx, mesh.DialConfig{
		Device:    sess.dev,
		Resolver:  sess.resolver,
		Target:    target,
		Endpoints: eps,
		Kind:      mesh.KindPing,
		Picker:    sess.picker(),
	})
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	defer link.Close()

	stats, err := mesh.PingExchangeStats(ctx, link, *count, *interval)
	if err != nil && stats.Sent == 0 {
		return ExitError{Code: 1, Err: fmt.Errorf("ping exchange failed: %w", err)}
	}
	for i, rtt := range stats.RTTs {
		fmt.Printf("reply=%d rtt=%.3fms\n", i+1, rtt)
	}
	p50 := mesh.Percentile(stats.RTTs, 50)
	p95 := mesh.Percentile(stats.RTTs, 95)
	linkClass := reachabilityUnknown
	if stats.Received > 0 && stats.Lost == 0 {
		linkClass = reachabilityDirect
	} else if stats.Sent > 0 {
		linkClass = reachabilityDegraded
	}
	// The link the session actually took. "relay" is reported with the
	// relay member's ID, because "relay" alone does not say WHICH
	// trunk carried the traffic — and that is the first thing an
	// operator wants to know.
	if used.Kind == mesh.PathRelay {
		linkClass = "relay"
		if used.Via != "" {
			linkClass = "relay via=" + used.Via.String()
		}
	}
	fmt.Printf("--- %s: %d/%d pings, lost %d (%.1f%%), p50 %.3fms p95 %.3fms, link=%s (P-1 line: <5ms)\n",
		target, stats.Received, stats.Sent, stats.Lost, stats.LossPercent(), p50, p95, linkClass)
	if err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("ping exchange stopped after %d/%d packets: %w", stats.Sent, *count, err)}
	}
	if p95 >= 5.0 {
		fmt.Fprintf(os.Stderr, "note: p95 above the P-1 acceptance line (5ms) — expected across real networks, investigate only on same-machine links\n")
	}
	return nil
}

func runDial(args []string) error {
	fs := flag.NewFlagSet("dial", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("device-passphrase", "", "passphrase for the device file (for scripting)")
	timeout := fs.Duration("timeout", 20*time.Second, "budget for discovery and the handshake")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if fs.NArg() != 1 {
		return ExitError{Code: 2, Err: errors.New(
			"usage: cmesh dial [flags] <deviceID> (the device ID goes last)")}
	}
	target := identity.DeviceID(fs.Arg(0))
	if !target.Valid() {
		return ExitError{Code: 2, Err: fmt.Errorf("%q is not a valid device ID", fs.Arg(0))}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	hctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	sess, err := openMeshSession(hctx, *state, *pass, nil)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	eps, err := sess.resolveTarget(hctx, target)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	// ★ THE PICKER, LIKE EVERY OTHER DIAL. (Round 99.)
	//
	// `dial` used to call DialDirect directly, so it was the ONE command
	// that could never reach a PURE-OUTBOUND member — a member with no
	// endpoints of its own, which is exactly the member the relay trunk
	// exists for. Every other dial in the CLI (bench, msg, send-to, run,
	// mesh ping) goes through the picker, and `bench` had this fixed
	// already for exactly this reason (netns-probe reported it on a real
	// SNAT lab: ping/msg/send-to went over the relay while bench alone
	// answered "has no reachable endpoints").
	//
	// So the command that opens an INTERACTIVE channel was the one that
	// could not reach half the network.
	link, used, err := mesh.DialViaPickerPath(hctx, mesh.DialConfig{
		Device:    sess.dev,
		Resolver:  sess.resolver,
		Target:    target,
		Kind:      mesh.KindDial,
		Picker:    sess.picker(),
		Endpoints: eps,
	})
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	defer link.Close()
	// ★ SAY WHICH PATH, IN THE SHAPE THE OTHER COMMANDS USE. (Round 99.)
	// The old line said "direct" unconditionally — which is not only
	// wrong now that the relay path is reachable, it was wrong before
	// (the dial could have failed and fallen back). An operator cannot
	// tell a relayed session from a direct one otherwise, and "I dialled
	// it and it was slow" has a very different answer depending on
	// which path it took.
	//
	// The shape is the one `mesh ping` already prints — "link=relay
	// via=<id>" — so the operator sees the same vocabulary everywhere.
	linkClass := string(used.Kind)
	if used.Kind == mesh.PathRelay && used.Via != "" {
		linkClass = "relay via=" + used.Via.String()
	}
	fmt.Fprintf(os.Stderr, "connected to %s (%s, mutually authenticated); type lines — they echo back; Ctrl-C to quit\n", target, linkClass)

	// Duplex pipe: stdin → stream, stream → stdout. Either side ending ends
	// the session.
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(link.Stream(), os.Stdin)
		done <- err
	}()
	go func() {
		_, err := io.Copy(os.Stdout, link.Reader())
		done <- err
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
	return nil
}
