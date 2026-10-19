// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"flag"
	"fmt"
	"log/slog"
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
	)
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	srv := relay.New(relay.Config{
		Addr:        *addr,
		Path:        *path,
		PairTimeout: *pairTimeout,
		MaxRooms:    *maxRooms,
		Logger:      log,
	})
	if err := srv.ListenAndServe(); err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	return nil
}
