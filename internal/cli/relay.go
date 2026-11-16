// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/muzimu217/container-mesh/internal/relay"
)

func runRelay(args []string) error {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		addr        = fs.String("addr", ":8080", "listen address")
		path        = fs.String("path", "/ws", "WebSocket endpoint path")
		pairTimeout = fs.Duration("pair-timeout", relay.DefaultConfig().PairTimeout, "how long the first peer waits for its partner")
		maxRooms    = fs.Int("max-rooms", relay.DefaultConfig().MaxRooms, "maximum concurrent rooms")
		verbose     = fs.Bool("v", false, "verbose (debug) logging")
		tlsCert     = fs.String("tls-cert", "", "certificate file; with --tls-key the relay serves wss:// directly")
		tlsKey      = fs.String("tls-key", "", "private key file for --tls-cert")
	)
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}

	logger := setupLogging(*verbose)

	// Announce the listening address unconditionally. It is the one line a
	// user needs after starting a relay: without it there is no way to know
	// the process came up, and the log-level filter must not swallow it.
	scheme := "ws"
	if *tlsCert != "" {
		scheme = "wss"
	}
	fmt.Fprintf(os.Stderr, "relay listening on %s://%s%s\n", scheme, *addr, *path)
	fmt.Fprintf(os.Stderr, "  dashboard: http://%s/dashboard\n", *addr)

	// Let the dashboard report the same version string `cmesh version` prints.
	relay.SetVersion(Version)

	// Graceful stop: SIGTERM (systemd stop) and SIGINT (Ctrl-C) must end in a
	// controlled drain — the HTTP listener closed and pairing rooms unwound —
	// and exit 0, so a systemd restart finds the port free. Signal
	// registration happens BEFORE ListenAndServe: a signal that beats the
	// bind is not lost (kuku round 55 measured a hard 143 exit / 6ms kill
	// because this command never wired the Shutdown path that
	// serve-discovery already had).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := relay.New(relay.Config{
		Addr:        *addr,
		Path:        *path,
		PairTimeout: *pairTimeout,
		MaxRooms:    *maxRooms,
		TLSCert:     *tlsCert,
		TLSKey:      *tlsKey,
		Logger:      logger,
	})
	go func() {
		<-ctx.Done()
		// Restore default handling first: a second signal while the drain
		// wedges must still kill the process.
		stop()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			fmt.Fprintf(os.Stderr, "relay: shutdown: %v\n", err)
		}
	}()
	if err := srv.ListenAndServe(); err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	return nil
}
