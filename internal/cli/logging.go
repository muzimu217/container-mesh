// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"log/slog"
	"os"
	"sync"
)

// cliLogger is the one logger the CLI writes structured detail to.
//
// The problem it solves: the CLI has two voices. User-facing progress ("forward
// 127.0.0.1:8080 -> db:5432", "connected: peer=x SAS=123456") is written
// directly by the commands, while internal detail goes through slog. Before
// this file the slog half used slog.Default(), whose text handler emits
// time=... level=INFO msg=... — so one screen showed two unrelated formats,
// and the noisier one won because slog.Default() is Info level.
//
// Now: structured detail is Warning and above by default, so a normal run shows
// only the command's own lines. --verbose turns it to Debug. Both go through the
// same handler, so the format is consistent when it does appear.
var (
	logMu      sync.Mutex
	logLogger  = slog.New(slog.DiscardHandler)
	logVerbose bool
)

// setupLogging installs the process logger. Call once, early, from each
// subcommand that has a --verbose flag.
func setupLogging(verbose bool) *slog.Logger {
	logMu.Lock()
	defer logMu.Unlock()
	logVerbose = verbose

	level := slog.LevelWarn
	if verbose {
		level = slog.LevelDebug
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		// Drop the time= prefix: a terminal already shows when a line arrived,
		// and in a log file the surrounding lines give the context.
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	logLogger = slog.New(h)
	slog.SetDefault(logLogger)
	return logLogger
}

// log returns the configured logger, never nil.
func log() *slog.Logger {
	logMu.Lock()
	defer logMu.Unlock()
	return logLogger
}

// verboseEnabled reports whether --verbose was passed.
func verboseEnabled() bool {
	logMu.Lock()
	defer logMu.Unlock()
	return logVerbose
}

// discardContext is a convenience for callers that need a context but not one
// that carries cancellation.
func discardContext() context.Context { return context.Background() }
