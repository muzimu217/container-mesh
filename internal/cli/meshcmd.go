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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      inv.DiscoveryAddrs,
		Device:     dev,
		AccountID:  inv.AccountID,
		AccountPub: []byte(inv.AccountPub),
		Endpoints:  known,
		NoAnnounce: len(known) == 0,
	})
	if err != nil {
		return nil, err
	}
	go func() { _ = cl.Run(ctx) }()

	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
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
				if perr := cl.Announce(waitCtx); perr != nil && strings.Contains(perr.Error(), "not a member") {
					return nil, fmt.Errorf("this device has been removed from this network (revoked by the account owner, or the approval never happened)")
				}
				return nil, fmt.Errorf("no member view within 15s (discovery node unreachable, or not approved yet?)")
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

// resolveTarget waits until the target appears in the live view with at
// least one endpoint, or the context ends.
func (s *meshSession) resolveTarget(ctx context.Context, target identity.DeviceID) ([]string, error) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		snap := s.cl.Snapshot()
		if snap != nil {
			if mv, ok := snap.Members[target]; ok {
				if len(mv.Endpoints) > 0 {
					return mv.Endpoints, nil
				}
				// A member with zero endpoints is in a STABLE state — the
				// announce carries endpoints atomically, so waiting cannot
				// grow them. Say the actionable thing immediately instead
				// of stalling to the deadline (issue #8 D-4: an online
				// restricted container used to wait the full --timeout for
				// a hint that only fired for offline targets).
				return nil, fmt.Errorf("%s is a member but has announced no endpoints (is it running with --listen?)", target)
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
	count := fs.Int("count", 10, "pings to send")
	interval := fs.Duration("interval", 200*time.Millisecond, "delay between pings")
	timeout := fs.Duration("timeout", 20*time.Second, "overall budget including discovery")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if fs.NArg() != 1 {
		return ExitError{Code: 2, Err: errors.New(
			"usage: cmesh mesh ping [flags] <deviceID> (the device ID goes last: flag parsing stops at the first positional argument)")}
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

	link, err := mesh.DialDirect(ctx, mesh.DialConfig{
		Device:    sess.dev,
		Resolver:  sess.resolver,
		Target:    target,
		Endpoints: eps,
		Kind:      mesh.KindPing,
	})
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	defer link.Close()

	rtts, err := mesh.PingExchange(ctx, link, *count, *interval)
	if err != nil && len(rtts) == 0 {
		return ExitError{Code: 1, Err: fmt.Errorf("ping exchange failed: %w", err)}
	}
	for i, rtt := range rtts {
		fmt.Printf("seq=%d rtt=%.3fms\n", i+1, rtt)
	}
	p50 := mesh.Percentile(rtts, 50)
	p95 := mesh.Percentile(rtts, 95)
	fmt.Printf("--- %s: %d/%d pings, p50 %.3fms p95 %.3fms (P-1 line: <5ms)\n",
		target, len(rtts), *count, p50, p95)
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
	link, err := mesh.DialDirect(hctx, mesh.DialConfig{
		Device:    sess.dev,
		Resolver:  sess.resolver,
		Target:    target,
		Endpoints: eps,
		Kind:      mesh.KindDial,
	})
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	defer link.Close()
	fmt.Fprintf(os.Stderr, "connected to %s (direct, mutually authenticated); type lines — they echo back; Ctrl-C to quit\n", target)

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
