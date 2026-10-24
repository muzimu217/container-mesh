// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"flag"
	"fmt"
	"os"

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

	srv := relay.New(relay.Config{
		Addr:        *addr,
		Path:        *path,
		PairTimeout: *pairTimeout,
		MaxRooms:    *maxRooms,
		TLSCert:     *tlsCert,
		TLSKey:      *tlsKey,
		Logger:      logger,
	})
	if err := srv.ListenAndServe(); err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	return nil
}
