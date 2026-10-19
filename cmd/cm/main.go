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
	"fmt"
	"os"

	"github.com/muzimu217/container-mesh/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		// cli.Run already printed a useful message; only exit non-zero.
		if code, ok := err.(cli.ExitError); ok {
			os.Exit(code.Code)
		}
		fmt.Fprintln(os.Stderr, "cmesh:", err)
		os.Exit(1)
	}
}
