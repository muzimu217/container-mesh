// Package cli implements the command line interface.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// Version is stamped at build time with -ldflags "-X ...cli.Version=...".
var Version = "dev"

// ExitError carries a specific process exit code.
type ExitError struct {
	Code int
	Err  error
}

func (e ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}

func (e ExitError) Unwrap() error { return e.Err }

// Run dispatches a subcommand.
func Run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return ExitError{Code: 2, Err: errors.New("missing subcommand")}
	}

	switch args[0] {
	case "probe":
		return runProbe(args[1:])
	case "relay":
		return runRelay(args[1:])
	case "send":
		return runSend(args[1:])
	case "recv":
		return runRecv(args[1:])
	case "version", "-v", "--version":
		fmt.Printf("cmesh %s\n", Version)
		return nil
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return ExitError{Code: 2, Err: fmt.Errorf("unknown subcommand %q", args[0])}
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `cmesh — userspace container-to-container messaging and file transfer

Usage:
  cmesh probe [--json] [--skip-network] [--device PATH]
        Report whether this container can create a TUN device, what
        capabilities it holds, and whether outbound networking works.
        Exit codes: 0 = TUN usable, 10 = userspace only, 20 = unusable.

  cmesh relay [--addr :8080] [--path /ws]
        Run the rendezvous server. Only the first and last hop of a transfer
        ever touch it, and it cannot read the traffic.

  cmesh send --relay URL[,URL2,...] --code CODE [--name NAME] [--file PATH]
        Connect as role A, transfer the file (if given), then wait for
        incoming messages until interrupted. While connected, type messages
        on stdin; "/file PATH" offers another file at any time.

  cmesh recv --relay URL[,URL2,...] --code CODE [--name NAME] [--out DIR]
        Connect as role B, receive the file (if one is offered), then wait
        for incoming messages until interrupted.

  cmesh version

Connection behaviour:
  * --relay accepts a comma-separated list; entries are tried in order, so
    listing a nearby relay first (e.g. one inside your region) keeps latency
    low while a second, foreign relay provides failover.
  * Sessions reconnect automatically: if the relay or the network drops,
    both peers re-dial, re-run the encrypted handshake, and continue.
    A file interrupted mid-flight resumes from the receiver's partial data
    on the next attempt.
  * WebSocket keepalive (20s ping, 60s timeout) detects connections killed
    silently by NAT boxes or proxies.

Environment:
  CMESH_RELAY   default --relay value (comma-separated list allowed)
  CMESH_CODE    default --code value
`)
}

// commonFlags are shared between send and recv.
type commonFlags struct {
	relay      string
	code       string
	name       string
	pairWindow time.Duration
}

func addCommon(fs *flag.FlagSet, cf *commonFlags) {
	fs.StringVar(&cf.relay, "relay", envOr("CMESH_RELAY", "ws://127.0.0.1:8080/ws"),
		"relay WebSocket URLs, comma-separated (env CMESH_RELAY)")
	fs.StringVar(&cf.code, "code", os.Getenv("CMESH_CODE"),
		"shared transfer code (env CMESH_CODE)")
	fs.StringVar(&cf.name, "name", hostnameOr("peer"),
		"name advertised to the peer")
	fs.DurationVar(&cf.pairWindow, "pair-window", pairWindowDefault,
		"how long one relay attempt may wait for the partner before rotating to the next relay")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func hostnameOr(def string) string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return def
	}
	return h
}
