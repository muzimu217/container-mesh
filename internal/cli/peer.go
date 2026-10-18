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

	"github.com/blackevil/container-mesh/internal/secure"
	"github.com/blackevil/container-mesh/internal/wire"
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

// newNode assembles a Node from parsed common flags.
func newNode(cf commonFlags, role secure.Role, outDir string, oneShot bool) (*Node, error) {
	if cf.code == "" {
		return nil, errors.New("--code is required (or set CMESH_CODE)")
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
	}
	if oneShot {
		// One-shot commands must terminate on their own, but must also survive
		// a partner that starts a little later: a 10s window across the public
		// internet makes both sides miss each other whenever their start times
		// drift. Keep a generous per-relay window and a bounded number of
		// rounds; stop re-offering a file nobody accepts.
		cfg.MaxConnectAttempts = 12
		cfg.MaxTransferAttempts = 3
		cfg.PairWindow = min(cfg.PairWindow, 45*time.Second)
	}
	return NewNode(cfg), nil
}

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cf commonFlags
	addCommon(fs, &cf)
	file := fs.String("file", "", "file to transfer (optional)")
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
		if err := node.SendMessage(ctx, *message); err != nil {
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
			fmt.Fprintf(os.Stderr, "message sent (%d bytes)\n", len(*message))
		}
	}

	if !*stay {
		return drainRun()
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
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	node, err := newNode(cf, secure.RoleB, *out, !*stay)
	if err != nil {
		return ExitError{Code: 2, Err: err}
	}

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
			stop()
			<-done
			if !res.OK {
				return fmt.Errorf("verification failed: %s", res.Message)
			}
			fmt.Fprintf(os.Stderr, "file verified: %s\n", res.Message)
			return nil
		case <-done:
			return runErr
		case <-ctx.Done():
			<-done
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
