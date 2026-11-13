// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// T5/T8: two messages that sent operators looking in the wrong place.
//
//   - The handshake error blamed the CODE, but a code mismatch cannot reach
//     it: the relay derives the room from the code, so two sides with
//     different codes never meet and the failure surfaces as "no partner".
//   - The --pair-window help said "0 = wait forever", which is only true
//     with a SINGLE relay; with several relays 0 means the 60 s default
//     (rotating is pointless when there is nowhere to rotate to, but a dead
//     relay must still be abandoned).

import (
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/muzimu217/container-mesh/internal/peer"
)

// peerHandshakeErrorText returns the user-visible handshake error text.
func peerHandshakeErrorText() string { return peer.ErrHandshake.Error() }

// pairWindowHelpText returns the --pair-window help line as the CLI
// prints it.
func pairWindowHelpText() string {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var cf commonFlags
	addCommon(fs, &cf)
	usage := ""
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "pair-window" {
			usage = f.Usage
		}
	})
	return usage
}

func TestHandshakeErrorDoesNotBlameTheCode(t *testing.T) {
	msg := peerHandshakeErrorText()
	if strings.Contains(msg, "same code") || strings.Contains(msg, "use the same") {
		t.Fatalf("the handshake error still blames the code: %q", msg)
	}
	if !strings.Contains(msg, "no partner") {
		t.Fatalf("the handshake error should point at what a wrong code actually looks like: %q", msg)
	}
}

func TestPairWindowHelpStatesTheRealRule(t *testing.T) {
	help := pairWindowHelpText()
	if !strings.Contains(help, "single relay") {
		t.Fatalf("the --pair-window help does not say that 0 waits forever only with one relay: %q", help)
	}
	if !strings.Contains(help, "60s") {
		t.Fatalf("the --pair-window help does not mention the multi-relay default: %q", help)
	}
}
