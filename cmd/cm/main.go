// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Command cmesh is the container-mesh CLI.
//
// Subcommands:
//
//	probe   report TUN / capability / network state as JSON
//	relay   run the rendezvous server
//	send    offer a file (or just chat) to a peer
//	recv    accept a file / receive messages
//	version print the version
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/muzimu217/container-mesh/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		// Subcommands report their own progress, but a usage or validation
		// error carries a message the user needs to see. Printing it here
		// rather than in each subcommand keeps the wording in one place and
		// means a new subcommand cannot silently exit non-zero with no reason.
		//
		// A plain exit (context cancelled by a signal) has nothing to say.
		var exit cli.ExitError
		if errors.As(err, &exit) {
			if exit.Err != nil && !errors.Is(exit.Err, context.Canceled) {
				fmt.Fprintln(os.Stderr, "cmesh:", exit.Err)
			}
			os.Exit(exit.Code)
		}
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "cmesh:", err)
		}
		os.Exit(1)
	}
}
