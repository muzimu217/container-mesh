// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"flag"
	"testing"
)

// ★ A FLAG THAT IS ACCEPTED AND IGNORED IS WORSE THAN A FLAG THAT
// DOES NOT EXIST. (Round 107.)
//
// `--p2p-legacy` was parsed into `commonFlags.p2pLegacy` and then never
// assigned to `NodeConfig.P2PLegacy`. So the branch that selects the
// built-in reliability layer was DEAD CODE, and the option the help
// text advertises could not be chosen at all.
//
// The architecture map line found it by grepping for the assignment and
// finding none.
//
// This test pins the wire from the flag to the config.

// TestTheLegacyTransportFlagReachesTheConfig covers the flag → config
// half.
func TestTheLegacyTransportFlagReachesTheConfig(t *testing.T) {
	t.Run("off-by-default", func(t *testing.T) {
		var cf commonFlags
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		addCommon(fs, &cf)
		if err := fs.Parse([]string{}); err != nil {
			t.Fatal(err)
		}
		cfg, err := commonNodeConfig(cf)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.P2PLegacy {
			t.Fatal("the legacy transport must be off by default")
		}
	})

	t.Run("the-flag-turns-it-on", func(t *testing.T) {
		var cf commonFlags
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		addCommon(fs, &cf)
		if err := fs.Parse([]string{"--p2p-legacy"}); err != nil {
			t.Fatal(err)
		}
		if !cf.p2pLegacy {
			t.Fatal("the flag did not reach commonFlags")
		}
		cfg, err := commonNodeConfig(cf)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.P2PLegacy {
			t.Fatal("★ THE DEFECT: `--p2p-legacy` was parsed and then DROPPED, so " +
				"the option the help text advertises could not be chosen at all — " +
				"and the branch that selects the built-in transport was dead code")
		}
	})
}

// TestTheDirectPathNeverRunsTwoReadersOnOneSocket is the half that
// matters for the product.
//
// The QUIC path runs quic-go on the punched socket. The built-in
// transport's reader must NOT be on that socket too, or the datagrams
// are split at random and the QUIC handshake packets are dropped as
// "stray traffic" — the direct path then never works, silently.
func TestTheDirectPathNeverRunsTwoReadersOnOneSocket(t *testing.T) {
	t.Run("the-quic-path-leaves-the-socket-alone", func(t *testing.T) {
		var cf commonFlags
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		addCommon(fs, &cf)
		if err := fs.Parse([]string{"--p2p"}); err != nil {
			t.Fatal(err)
		}
		cfg, err := commonNodeConfig(cf)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.P2PLegacy {
			t.Fatal("the QUIC path must not select the legacy transport")
		}
		// The decision the negotiator makes with these options.
		if !cfg.P2P.SkipBuiltInTransport {
			t.Fatal("★ THE DEFECT: the QUIC path did not ask to skip the built-in " +
				"transport's reader, so the punched socket ends up with TWO readers " +
				"(the built-in readLoop and quic-go's Transport.listen). The " +
				"datagrams are split at random, the QUIC handshake packets are " +
				"dropped as 'stray traffic', and the direct path NEVER succeeds")
		}
	})

	t.Run("the-legacy-path-asks-for-the-socket", func(t *testing.T) {
		var cf commonFlags
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		addCommon(fs, &cf)
		if err := fs.Parse([]string{"--p2p", "--p2p-legacy"}); err != nil {
			t.Fatal(err)
		}
		cfg, err := commonNodeConfig(cf)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.P2PLegacy {
			t.Fatal("the legacy path must select the legacy transport")
		}
		if cfg.P2P.SkipBuiltInTransport {
			t.Fatal("the legacy path needs the built-in transport's reader on the socket")
		}
	})
}
