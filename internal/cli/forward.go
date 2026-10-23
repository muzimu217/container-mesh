// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/muzimu217/container-mesh/internal/forward"
	"github.com/muzimu217/container-mesh/internal/mux"
	"github.com/muzimu217/container-mesh/internal/secure"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// shutdownGrace bounds how long teardown may take after a signal. A terminal
// that will not come back is worse than an unclean exit, so the process gives
// up on a clean shutdown rather than hanging.
const shutdownGrace = 3 * time.Second

// runForward implements `cmesh forward`.
func runForward(args []string) error {
	fs := flag.NewFlagSet("forward", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var cf commonFlags
	addCommon(fs, &cf)

	var locals, reverses multiFlag
	fs.Var(&locals, "L", "forward a local port to a service reachable from the peer: [bind:]port:host:port (repeatable)")
	fs.Var(&reverses, "R", "ask the peer to listen and forward back here: [bind:]port:host:port (repeatable)")

	accept := fs.Bool("accept", false,
		"serve forwarded streams arriving from the peer (off by default)")
	acceptReverse := fs.Bool("accept-reverse", false,
		"host listeners the peer asks for with -R (off by default; needs --allow-listen-port)")
	allowHosts := fs.String("allow-host", "",
		"comma-separated hosts/CIDRs a peer may ask us to dial (empty = deny everything)")
	allowPorts := fs.String("allow-port", "",
		"comma-separated ports/ranges for --allow-host (empty = deny everything)")
	allowListenPorts := fs.String("allow-listen-port", "",
		"comma-separated ports/ranges a peer may ask us to bind for -R (empty = deny everything)")
	listenAll := fs.Bool("listen-all", false,
		"allow a -R request to bind a non-loopback address")
	maxStreams := fs.Int("max-streams", 256, "concurrent forwarded streams")
	maxStreamsPerSec := fs.Int("max-streams-per-sec", 50,
		"new forwarded streams per second (0 = unlimited)")
	maxPending := fs.Int("max-pending", 128,
		"local connections accepted but not yet given a stream")
	handshakeTimeout := fs.Duration("handshake-timeout", 10*time.Second,
		"how long a stream may take to send its preamble")
	pipeDrain := fs.Duration("pipe-drain", 5*time.Second,
		"after one side closes, how long the other has to finish before reset")
	dialTimeout := fs.Duration("dial-timeout", 5*time.Second,
		"outbound connect timeout")

	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}

	// ---- validate the request before touching the network ----------------

	if len(locals) == 0 && len(reverses) == 0 && !*accept && !*acceptReverse {
		return ExitError{Code: 2, Err: errors.New(
			"nothing to do: pass -L, -R, --accept or --accept-reverse")}
	}
	reverseSpecs := make([]forward.Spec, 0, len(reverses))
	for _, raw := range reverses {
		sp, err := forward.ParseSpec(raw, true)
		if err != nil {
			return ExitError{Code: 2, Err: err}
		}
		reverseSpecs = append(reverseSpecs, sp)
	}
	// --allow-listen-port bounds which ports this side may BIND on request.
	// It is therefore required only when this side is the one that hosts
	// listeners, i.e. when it has no -R of its own: a pure -R requester never
	// binds anything, it only serves the streams the hoster sends it.
	//
	// Requiring it on both sides would force a requester to name ports it will
	// never use, which teaches operators to pass a wide range "to make it
	// work" -- the opposite of what the whitelist is for.
	hostsListeners := len(reverseSpecs) == 0
	if *acceptReverse && hostsListeners && *allowListenPorts == "" {
		return ExitError{Code: 2, Err: errors.New(
			"--accept-reverse needs --allow-listen-port: hosting listeners on " +
				"request must be an explicit, bounded permission")}
	}

	specs := make([]forward.Spec, 0, len(locals))
	for _, raw := range locals {
		sp, err := forward.ParseSpec(raw, false)
		if err != nil {
			return ExitError{Code: 2, Err: err}
		}
		specs = append(specs, sp)
	}

	ports, err := forward.ParsePortSet(splitCSV(*allowPorts))
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	listenPorts, err := forward.ParsePortSet(splitCSV(*allowListenPorts))
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}

	// The allow-list resolves hostnames here, once, and pins them.
	allow, err := forward.NewAllowList(context.Background(), splitCSV(*allowHosts), ports, nil)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}

	// Refuse a configuration that cannot work, rather than connecting and then
	// denying every request: "nothing will ever be allowed" is a startup error.
	if *accept && allow.Empty() {
		return ExitError{Code: 2, Err: errors.New(
			"--accept needs --allow-host (and usually --allow-port); " +
				"without them every forwarded request is denied")}
	}

	// ---- build the manager ------------------------------------------------

	mgr := forward.New(forward.Config{
		Accept:        *accept,
		AcceptReverse: *acceptReverse,
		ListenAll:     *listenAll,
		Allow:         allow,
		ListenPorts:   listenPorts,
		Limits: forward.Limits{
			MaxPending:       *maxPending,
			MaxStreamsPerSec: *maxStreamsPerSec,
			HandshakeTimeout: *handshakeTimeout,
			PipeDrain:        *pipeDrain,
			DialTimeout:      *dialTimeout,
		},
		Logger: log(),
	})
	defer mgr.Close()

	// Register the -R targets locally. The binding happens on the peer; what we
	// keep is the ID -> local target table, so an inbound reverse stream can be
	// served without ever trusting an address from the wire.
	if err := mgr.PrepareReverse(reverseSpecs); err != nil {
		return ExitError{Code: 2, Err: err}
	}

	// ---- node wiring ------------------------------------------------------

	cfg, err := commonNodeConfig(cf)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	// The side that requests something (-L or -R) is the initiator: it opens
	// the control stream, and -R additionally sends FrameForwardListen over it.
	// A pure --accept / --accept-reverse peer is the responder. If both sides
	// were initiators they would each wait for a control stream the other never
	// opens, which deadlocks.
	if len(specs) > 0 || len(reverseSpecs) > 0 {
		cfg.Role = secure.RoleA
	} else {
		cfg.Role = secure.RoleB
	}
	cfg.UseMux = true
	cfg.OnStream = func(st *mux.Stream) { mgr.Serve(context.Background(), st) }
	// The provider is needed by reverse listeners, which open a stream back to
	// the peer for every accepted connection. It is built before the node so
	// the closure can capture it.
	provider := &nodeProvider{node: nil}
	cfg.OnForwardListen = func(wireSpecs []wire.ForwardListenSpec) []wire.ForwardListenStatus {
		return mgr.BindReverseListeners(provider, wireSpecs)
	}

	node := NewNode(cfg)

	// Bind the listeners BEFORE connecting. A port conflict must fail at
	// startup, not silently after the session is up; and the listener must be
	// created once and survive reconnects, or every drop would briefly unbind
	// the port and race any other process trying to take it.
	listeners := make([]*forward.LocalForward, 0, len(specs))
	for _, sp := range specs {
		lf, err := mgr.AddLocal(sp)
		if err != nil {
			for _, done := range listeners {
				_ = done.Close()
			}
			return ExitError{Code: 1, Err: err}
		}
		listeners = append(listeners, lf)
	}

	// ---- banner -----------------------------------------------------------

	printForwardBanner(os.Stderr, specs, *accept, allow, *maxStreams, *maxStreamsPerSec)

	// ---- run --------------------------------------------------------------

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	provider.node = node
	for _, lf := range listeners {
		go func(lf *forward.LocalForward) {
			if err := lf.Serve(ctx, provider); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "cmesh: forward %s: %v\n", lf.Spec().Bind, err)
			}
		}(lf)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- node.Run(ctx) }()

	// Ask the peer to host the -R listeners. This needs a live mux session, so
	// it runs after Run starts and retries across reconnects: a reconnect
	// rebuilds the session and the peer's listeners must be requested again.
	if len(reverseSpecs) > 0 {
		go registerReverse(ctx, node, mgr, reverseSpecs)
	}

	var ret error
	select {
	case err := <-runErr:
		if err != nil && ctx.Err() == nil {
			ret = err
		}
	case <-ctx.Done():
	}

	// Explicit teardown with a hard deadline. Deferring only mgr.Close would
	// leave the process waiting on whatever goroutine is still blocked in a
	// pipe; a user pressing Ctrl-C expects the port to be released now.
	stop()
	shutdown(mgr, listeners)
	return ret
}

// shutdown releases listeners and the manager, giving up after shutdownGrace.
func shutdown(mgr *forward.Manager, listeners []*forward.LocalForward) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, lf := range listeners {
			_ = lf.Close()
		}
		_ = mgr.Close()
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		fmt.Fprintf(os.Stderr, "cmesh: shutdown did not finish within %s; exiting anyway\n", shutdownGrace)
	}
}

// nodeProvider adapts a Node to forward.Provider.
//
// The adapter exists because Node.Mux returns *mux.Session while forward.Mux
// promises forward.Stream: Go has no covariant return types.
type nodeProvider struct{ node *Node }

func (p *nodeProvider) Current(ctx context.Context) (forward.Mux, error) {
	ms, err := p.node.Mux(ctx)
	if err != nil {
		return nil, err
	}
	return muxSessionAdapter{ms}, nil
}

// muxSessionAdapter presents *mux.Session through forward.Mux.
type muxSessionAdapter struct{ s *mux.Session }

func (a muxSessionAdapter) Open(ctx context.Context, kind byte) (forward.Stream, error) {
	return a.s.Open(ctx, kind)
}

func (a muxSessionAdapter) NumStreams() int { return a.s.NumStreams() }

// printForwardBanner states what is actually in effect.
//
// The point is that an operator can see the effective policy without reading
// the code: a forward bound to 0.0.0.0 or an allow-list covering a /8 is
// visible here rather than discovered later.
func printForwardBanner(w *os.File, specs []forward.Spec, accept bool, allow forward.AllowList, maxStreams, maxPerSec int) {
	for _, sp := range specs {
		host, _, _ := net.SplitHostPort(sp.Bind)
		switch host {
		case "0.0.0.0", "::", "":
			fmt.Fprintf(w, "WARN: binding to %s exposes port to all interfaces\n", sp.Bind)
		}
		fmt.Fprintf(w, "forward  %s -> %s (via the peer)\n", sp.Bind, sp.Target)
	}
	if accept {
		if allow.Empty() {
			fmt.Fprintf(w, "accept   enabled, but no --allow-host: every request will be denied\n")
		} else {
			fmt.Fprintf(w, "accept   enabled (policy: --allow-host in effect, %d streams max, %d/s)\n",
				maxStreams, maxPerSec)
		}
	} else {
		fmt.Fprintf(w, "accept   disabled: this side will not dial targets for the peer\n")
	}
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// splitCSV splits a comma-separated flag value, dropping empties.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// registerReverse asks the peer to host the -R listeners, and keeps asking after
// every reconnect.
//
// A reconnect rebuilds the mux session, so the peer's listeners are gone with
// it. Re-sending the same specs is safe: the peer replaces a listener with the
// same ID rather than leaking the old bind.
func registerReverse(ctx context.Context, node *Node, mgr *forward.Manager, specs []forward.Spec) {
	wireSpecs, err := forward.ReverseSpecs(specs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmesh: -R: %v\n", err)
		return
	}

	var lastSession *mux.Session
	for {
		if ctx.Err() != nil {
			return
		}
		ms, err := node.Mux(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if ms == lastSession {
			// Same session: nothing to re-register. Wait for a change.
			select {
			case <-ctx.Done():
				return
			case <-ms.Done():
			case <-time.After(time.Second):
			}
			continue
		}

		results, err := requestReverse(ctx, node, wireSpecs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cmesh: -R registration failed: %v\n", err)
		} else {
			for _, r := range results {
				if r.Status == wire.StatusOK {
					fmt.Fprintf(os.Stderr, "reverse  %s (hosted by the peer)\n", r.ID)
					continue
				}
				fmt.Fprintf(os.Stderr, "cmesh: -R %s refused: %s (%s)\n",
					r.ID, r.Status.String(), r.Message)
			}
		}
		lastSession = ms

		select {
		case <-ctx.Done():
			return
		case <-ms.Done():
		}
	}
}

// requestReverse sends FrameForwardListen on the control stream and waits for
// the matching result.
//
// It goes through the peer session rather than a fresh mux stream because the
// request is a one-shot control message: peer.Session already dispatches by
// frame type, and adding a second control plane would mean two things could
// disagree about session state.
func requestReverse(ctx context.Context, node *Node, specs []wire.ForwardListenSpec) ([]wire.ForwardListenStatus, error) {
	sess, err := node.waitSession(ctx)
	if err != nil {
		return nil, err
	}
	return sess.RequestForwardListen(ctx, specs)
}
