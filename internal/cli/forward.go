// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/muzimu217/container-mesh/internal/forward"
	"github.com/muzimu217/container-mesh/internal/mux"
	"github.com/muzimu217/container-mesh/internal/secure"
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
	if len(reverses) > 0 {
		return ExitError{Code: 2, Err: errors.New(
			"-R (reverse forwarding) is not implemented yet; use -L")}
	}
	if *acceptReverse && *allowListenPorts == "" {
		return ExitError{Code: 2, Err: errors.New(
			"--accept-reverse needs --allow-listen-port: binding a port on request " +
				"must be an explicit, bounded permission")}
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
		Logger: slog.Default(),
	})
	defer mgr.Close()

	// ---- node wiring ------------------------------------------------------

	cfg, err := commonNodeConfig(cf)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	// The side that listens (-L) is the initiator: it opens the control stream
	// and the forwarded streams. A pure --accept peer is the responder. With
	// both, -L wins, because two initiators would both wait for a control
	// stream that neither opens.
	if len(specs) > 0 {
		cfg.Role = secure.RoleA
	} else {
		cfg.Role = secure.RoleB
	}
	cfg.UseMux = true
	cfg.OnStream = func(st *mux.Stream) { mgr.Serve(context.Background(), st) }

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

	provider := &nodeProvider{node: node}
	for _, lf := range listeners {
		go func(lf *forward.LocalForward) {
			if err := lf.Serve(ctx, provider); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "cmesh: forward %s: %v\n", lf.Spec().Bind, err)
			}
		}(lf)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- node.Run(ctx) }()

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
