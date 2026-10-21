// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/muzimu217/container-mesh/internal/p2p"
	"github.com/muzimu217/container-mesh/internal/secure"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// pairWindowDefault bounds a single relay attempt (dial + handshake + wait
// for the partner) before the node rotates to the next relay.
const pairWindowDefault = 60 * time.Second

// progressPrinter renders a single-line progress indicator on w.
func progressPrinter(w *os.File) func(done, total int64) {
	var lastPct int64 = -1
	last := time.Now()
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		pct := done * 100 / total
		// Throttle: redraw at most ~20 times per second or on 1% change.
		if pct == lastPct && time.Since(last) < 50*time.Millisecond {
			return
		}
		lastPct = pct
		last = time.Now()
		fmt.Fprintf(w, "\r  %3d%%  %s / %s", pct, humanBytes(done), humanBytes(total))
		if done >= total {
			fmt.Fprintln(w)
		}
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// p2pOptions builds direct-path settings from the common flags.
func p2pOptions(cf commonFlags) p2p.Options {
	o := p2p.DefaultOptions()
	o.Enabled = cf.p2p
	if strings.TrimSpace(cf.stun) != "" {
		var servers []string
		for _, s := range strings.Split(cf.stun, ",") {
			if s = strings.TrimSpace(s); s != "" {
				servers = append(servers, s)
			}
		}
		if len(servers) > 0 {
			o.STUNServers = servers
		}
	}
	return o
}

// newNode assembles a Node from parsed common flags.
// commonNodeConfig turns the shared connection flags into a NodeConfig.
//
// It is split out of newNode so `forward` can reuse the code-generation and
// relay-parsing logic instead of duplicating it: a second copy would drift, and
// the generated-code path is security-relevant.
func commonNodeConfig(cf commonFlags) (NodeConfig, error) {
	if cf.code == "" {
		// The code is the only shared secret, and the room label derived from
		// it is a verifier a relay can attack offline — so a weak code is a
		// real weakness, not just a bad habit. Generating a strong one and
		// printing it once is safer than making the user invent one, and it
		// removes the temptation to reuse "test123".
		generated, err := secure.GenerateCode()
		if err != nil {
			return NodeConfig{}, fmt.Errorf("cannot generate a code: %w", err)
		}
		fmt.Fprintf(os.Stderr,
			"no --code given; generated one for this session:\n\n  %s\n\n"+
				"Give the same code to the other side (or set --code / CMESH_CODE).\n",
			generated)
		cf.code = generated
	}
	relays, err := splitRelays(cf.relay)
	if err != nil {
		return NodeConfig{}, err
	}
	return NodeConfig{
		Relays:     relays,
		Code:       cf.code,
		Name:       cf.name,
		Role:       secure.RoleA,
		PairWindow: cf.pairWindow,
		P2P:        p2pOptions(cf),
		ExpectSAS:  cf.expectSAS,
	}, nil
}

func newNode(cf commonFlags, role secure.Role, outDir string, oneShot bool) (*Node, error) {
	if cf.code == "" {
		// The code is the only shared secret, and the room label derived from
		// it is a verifier a relay can attack offline — so a weak code is a
		// real weakness, not just a bad habit. Generating a strong one and
		// printing it once is safer than making the user invent one, and it
		// removes the temptation to reuse "test123".
		generated, err := secure.GenerateCode()
		if err != nil {
			return nil, fmt.Errorf("cannot generate a code: %w", err)
		}
		fmt.Fprintf(os.Stderr,
			"no --code given; generated one for this session:\n\n  %s\n\n"+
				"Give the same code to the other side (or set --code / CMESH_CODE).\n",
			generated)
		cf.code = generated
	}
	relays, err := splitRelays(cf.relay)
	if err != nil {
		return nil, err
	}
	cfg := NodeConfig{
		Relays:     relays,
		Code:       cf.code,
		Name:       cf.name,
		Role:       role,
		PairWindow: cf.pairWindow,
		OutDir:     outDir,
		P2P:        p2pOptions(cf),
		ExpectSAS:  cf.expectSAS,
	}
	if oneShot {
		// One-shot commands must terminate on their own, but must also survive
		// a partner that starts a little later: a 10s window makes both sides
		// miss each other whenever their start times drift.
		//
		// The total is deliberately bounded: when the two sides use DIFFERENT
		// codes they land in different rooms (the room label is derived from
		// the code), so they can never meet and both simply wait out the
		// budget. 12 rounds x 45s made that take ten minutes, which reads as a
		// hang. 6 rounds x 25s keeps a comfortable start-time window while
		// failing in about two minutes.
		cfg.MaxConnectAttempts = 6
		cfg.MaxTransferAttempts = 3
		if cfg.PairWindow <= 0 {
			cfg.PairWindow = 25 * time.Second
		}
	}
	return NewNode(cfg), nil
}

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cf commonFlags
	addCommon(fs, &cf)
	file := fs.String("file", "", "file or directory to transfer (optional)")
	message := fs.String("message", "", "message to send after connecting (optional)")
	stay := fs.Bool("stay", true, "keep the session open for further messages")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if *file == "" && *message == "" && !*stay {
		return ExitError{Code: 2, Err: errors.New("nothing to do: pass --file and/or --message, or keep --stay=true to chat")}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	node, err := newNode(cf, secure.RoleA, ".", !*stay)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	runErr := make(chan error, 1)
	go func() { runErr <- node.Run(ctx) }()

	// drainRun stops the node and returns whatever ended it, so a failure to
	// ever connect is reported instead of looking like a clean exit.
	drainRun := func() error {
		stop()
		return <-runErr
	}

	fileOK := false
	msgOK := false
	if *file != "" {
		res, err := node.SendFile(ctx, *file, progressPrinter(os.Stderr))
		if err != nil {
			if rerr := drainRun(); rerr != nil && ctx.Err() == nil {
				return rerr
			}
			return err
		}
		if !res.OK {
			drainRun()
			return fmt.Errorf("receiver rejected the file: %s", res.Message)
		}
		fileOK = true
		fmt.Fprintf(os.Stderr, "file verified by receiver: %s\n", res.Message)
	}

	if *message != "" {
		// In one-shot mode with a completed transfer the receiver may already
		// be gone. SendMessage would then block inside waitSession waiting for
		// a reconnect that cannot happen, so skip the goodbye instead of
		// hanging until the pair window expires.
		if !*stay && fileOK {
			fmt.Fprintln(os.Stderr, "note: trailing message skipped (receiver already finished)")
		} else if err := node.SendMessage(ctx, *message); err != nil {
			if fileOK && ctx.Err() == nil {
				// The receiver already verified the file and (in one-shot
				// mode) may have exited before our trailing message hit the
				// wire. The transfer — the part with delivery guarantees —
				// succeeded, so a lost goodbye is a warning, not a failure.
				fmt.Fprintf(os.Stderr, "warning: message not delivered (receiver left): %v\n", err)
			} else {
				if rerr := drainRun(); rerr != nil && ctx.Err() == nil {
					return rerr
				}
				return err
			}
		} else {
			msgOK = true
			fmt.Fprintf(os.Stderr, "message sent (%d bytes)\n", len(*message))
		}
	}

	if !*stay {
		// One-shot mode is done once the work is done. Do NOT wait for Run to
		// return: after the peer leaves, Run starts a reconnect attempt whose
		// pair window can be tens of seconds, and blocking on it would make a
		// successful transfer appear to hang (observed as a timeout in stress
		// runs). Cancelling is enough — Run is a daemon goroutine and the
		// process exits as soon as main returns.
		stop()
		if fileOK || msgOK {
			return nil
		}
		select {
		case rerr := <-runErr:
			return rerr
		case <-time.After(2 * time.Second):
			return errors.New("nothing was transferred before the session ended")
		}
	}

	fmt.Fprintln(os.Stderr, "session open; type messages, /file PATH to send a file, ctrl-d or ctrl-c to quit")
	err = stdinLoop(ctx, node)
	stop()
	if rerr := <-runErr; rerr != nil && err == nil {
		err = rerr
	}
	return err
}

func runRecv(args []string) error {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cf commonFlags
	addCommon(fs, &cf)
	out := fs.String("out", ".", "directory for received files")
	stay := fs.Bool("stay", true, "keep the session open after the transfer")
	message := fs.String("message", "", "message to send after connecting (optional)")
	maxSize := fs.Int64("max-file-size", 0,
		"reject inbound files larger than this many bytes (0 = default 16 GiB, negative = unlimited)")
	allowHidden := fs.Bool("allow-hidden", false,
		"accept dotfiles (default: refuse, since the receiver writes into a working directory)")
	force := fs.Bool("force", false,
		"overwrite an existing file with the same name (default: refuse)")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	node, err := newNode(cf, secure.RoleB, *out, !*stay)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}
	node.cfg.MaxFileSize = *maxSize
	node.cfg.AllowHidden = *allowHidden
	node.cfg.ForceOverwrite = *force

	var runErr error
	done := make(chan struct{})
	if !*stay {
		// One-shot mode: wait for exactly one inbound file, verify, exit.
		fileCh := make(chan wire.FileResult, 1)
		node.cfg.OnFile = func(res *wire.FileResult, _ string) {
			select {
			case fileCh <- *res:
			default:
			}
		}
		go func() {
			runErr = node.Run(ctx)
			close(done)
		}()

		select {
		case res := <-fileCh:
			// The file is on disk and verified; do not wait for Run to unwind
			// (it may be inside a reconnect attempt with a long pair window).
			stop()
			if !res.OK {
				return fmt.Errorf("verification failed: %s", res.Message)
			}
			fmt.Fprintf(os.Stderr, "file verified: %s\n", res.Message)
			return nil
		case <-done:
			return runErr
		case <-ctx.Done():
			return nil
		}
	}

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- node.Run(ctx) }()

	if *message != "" {
		if err := node.SendMessage(ctx, *message); err != nil {
			stop()
			<-runErrCh
			return err
		}
	}

	fmt.Fprintln(os.Stderr, "session open; waiting for files and messages (ctrl-c to quit)")
	err = stdinLoop(ctx, node)
	stop()
	if rerr := <-runErrCh; rerr != nil && err == nil {
		err = rerr
	}
	return err
}

// stdinLoop forwards terminal input to the session: plain lines become chat
// messages and "/file PATH" offers a file. It returns when the context is
// cancelled or a send fails hard.
func stdinLoop(ctx context.Context, node *Node) error {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if line == "" {
			continue
		}
		if path, ok := strings.CutPrefix(line, "/file "); ok {
			path = strings.TrimSpace(path)
			if path == "" {
				fmt.Fprintln(os.Stderr, "usage: /file PATH")
				continue
			}
			res, err := node.SendFile(ctx, path, progressPrinter(os.Stderr))
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if !res.OK {
				fmt.Fprintf(os.Stderr, "receiver rejected %s: %s\n", path, res.Message)
			} else {
				fmt.Fprintf(os.Stderr, "file verified by receiver: %s\n", res.Message)
			}
			continue
		}
		if err := node.SendMessage(ctx, line); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	// stdin closed (or /dev/null): keep receiving until the context ends.
	<-ctx.Done()
	return nil
}
