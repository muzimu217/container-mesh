// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/muzimu217/container-mesh/internal/peer"
	"github.com/muzimu217/container-mesh/internal/secure"
	"github.com/muzimu217/container-mesh/internal/wire"
	"github.com/muzimu217/container-mesh/internal/wsconn"
)

// NodeConfig configures a Node.
type NodeConfig struct {
	// Relays is an ordered list of relay WebSocket URLs. The node walks the
	// list, retrying the next entry after a failure, so a deployment can
	// offer several relays (e.g. one inside China, one abroad) and every
	// client converges on the first one that is alive.
	Relays []string
	// Code is the shared transfer code.
	Code string
	// Name is advertised to the peer.
	Name string
	// Role is A (sender side) or B (receiver side).
	Role secure.Role
	// PairWindow bounds how long one relay attempt may take, including the
	// wait for the partner to show up. After it expires the node moves to the
	// next relay. Default 60s.
	PairWindow time.Duration
	// OutDir receives inbound files.
	OutDir string
	// MaxFileSize rejects inbound offers above this many bytes (0 = peer
	// default, negative = unlimited).
	MaxFileSize int64
	// Liveness configures WebSocket keepalive. Default 20s ping / 60s idle.
	Liveness wsconn.Liveness
	// OnMessage receives inbound chat messages.
	OnMessage func(text string)
	// OnFile is called when an inbound file finished (verified or not).
	OnFile func(res *wire.FileResult, name string)
	// OnStatus reports lifecycle events for logging/UI.
	OnStatus func(event, detail string)
	// MaxConnectAttempts caps total dial attempts when non-zero; 0 retries
	// forever (daemon behaviour). One-shot commands use a small cap so they
	// fail instead of hanging.
	MaxConnectAttempts int
	// MaxTransferAttempts caps how many times SendFile may re-offer after a
	// transport failure when non-zero; 0 keeps retrying until the file is
	// verified (the auto-resume behaviour). One-shot commands use a small
	// cap so a missing receiver terminates the process.
	MaxTransferAttempts int
}

// Node is a long-lived peer endpoint: it keeps a session connected by
// rotating through the relay list with backoff, re-running the SPAKE2
// handshake after every drop. Messages and file offers flow again as soon as
// both sides re-meet; a file that was interrupted mid-flight resumes from the
// receiver's .part file on the next SendFile attempt.
type Node struct {
	cfg NodeConfig
	log *slog.Logger

	mu       sync.Mutex
	sess     *peer.Session
	terminal error
	notify   chan struct{}
	runDone  chan struct{}
	relayIdx int
}

// NewNode builds a node; Run drives it.
func NewNode(cfg NodeConfig) *Node {
	if cfg.PairWindow <= 0 {
		cfg.PairWindow = 60 * time.Second
	}
	if cfg.Liveness.PingEvery == 0 {
		cfg.Liveness = wsconn.DefaultLiveness()
	}
	if cfg.OnStatus == nil {
		cfg.OnStatus = func(event, detail string) {
			fmt.Fprintf(os.Stderr, "[cmesh] %s: %s\n", event, detail)
		}
	}
	return &Node{
		cfg:     cfg,
		log:     slog.Default(),
		notify:  make(chan struct{}, 1),
		runDone: make(chan struct{}),
	}
}

func (n *Node) status(event, detail string) { n.cfg.OnStatus(event, detail) }

// Run connects and keeps the node connected until ctx is cancelled or a
// permanent (protocol/code) error occurs. It returns nil after cancellation.
func (n *Node) Run(ctx context.Context) error {
	defer close(n.runDone)

	attempts := 0
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			n.finish(nil)
			return nil
		}
		if n.cfg.MaxConnectAttempts > 0 && attempts >= n.cfg.MaxConnectAttempts {
			err := fmt.Errorf("could not connect after %d attempts across %d relay(s)", attempts, len(n.cfg.Relays))
			n.finish(err)
			return err
		}

		relayURL := n.cfg.Relays[n.relayIdx%len(n.cfg.Relays)]
		n.relayIdx++
		attempts++
		n.status("connecting", relayURL)

		sess, err := n.dialOne(ctx, relayURL)
		if err != nil {
			if ctx.Err() != nil {
				n.finish(nil)
				return nil
			}
			if isPermanent(err) {
				n.finish(err)
				return err
			}
			n.status("connect-failed", err.Error())
			// Short pause between relays of the same round; a longer one
			// before starting the list over.
			if n.relayIdx%len(n.cfg.Relays) == 0 {
				if !sleepCtx(ctx, backoff) {
					n.finish(nil)
					return nil
				}
				backoff = min(backoff*2, 15*time.Second)
			} else {
				if !sleepCtx(ctx, 300*time.Millisecond) {
					n.finish(nil)
					return nil
				}
			}
			continue
		}

		// Deliberately do NOT reset the failure counter on a successful dial:
		// a lone peer can "connect" forever (park, expire, repeat) without its
		// partner ever arriving, which would defeat MaxConnectAttempts. The
		// counter bounds total attempts for one-shot commands; stay mode runs
		// with the cap disabled.
		backoff = time.Second
		// Print the short authentication string so the two operators can
		// compare it out of band. A malicious relay can route the connection
		// (it knows the room label) but cannot make both sides derive the same
		// SAS without the code, so a mismatch is the signal to abort.
		n.status("connected", fmt.Sprintf("%s  peer=%s  SAS=%s  fingerprint=%s",
			relayURL, sess.PeerName, sess.ShortAuth(), sess.Fingerprint()))
		n.setSession(sess)

		// The session's read pump dispatches messages, offers and pings via
		// the handler installed in dialOne; here we only wait for it to end.
		select {
		case <-sess.Done():
		case <-ctx.Done():
		}
		n.clearSession(sess)
		_ = sess.Close()
		if ctx.Err() != nil {
			n.finish(nil)
			return nil
		}
		n.status("disconnected", fmt.Sprint(sess.Err()))
		if !sleepCtx(ctx, 500*time.Millisecond) {
			n.finish(nil)
			return nil
		}
	}
}

func (n *Node) finish(err error) {
	n.mu.Lock()
	n.terminal = err
	n.mu.Unlock()
}

// dialOne performs one full connection attempt against a single relay:
// dial, SPAKE2 handshake and hello, bounded by the pair window.
func (n *Node) dialOne(ctx context.Context, relayURL string) (*peer.Session, error) {
	u, err := url.Parse(relayURL)
	if err != nil {
		return nil, fmt.Errorf("bad relay URL %q: %w", relayURL, err)
	}
	q := u.Query()
	// The relay gets a one-way label, never the code itself: the code is the
	// only shared secret, and a relay that knows it can complete the PAKE with
	// each side separately (an active MITM). See secure.RoomID.
	q.Set("room", secure.RoomID(n.cfg.Code))
	q.Set("role", n.cfg.Role.String())
	u.RawQuery = q.Encode()

	dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	defer cancelDial()
	lv := n.cfg.Liveness
	conn, err := wsconn.Dial(dialCtx, wsconn.DialOptions{
		URL:         u.String(),
		DialTimeout: 10 * time.Second,
		Liveness:    &lv,
	})
	if err != nil {
		return nil, err
	}

	type result struct {
		sess *peer.Session
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := peer.Handshake(conn, peer.Config{
			Name:        n.cfg.Name,
			Code:        n.cfg.Code,
			Role:        n.cfg.Role,
			Logger:      n.log,
			MaxFileSize: n.cfg.MaxFileSize,
		})
		if err == nil {
			err = s.Start(n.handler())
		}
		if err != nil {
			_ = conn.Close()
			ch <- result{nil, err}
			return
		}
		ch <- result{s, nil}
	}()

	select {
	case r := <-ch:
		return r.sess, r.err
	case <-time.After(n.cfg.PairWindow):
		_ = conn.Close()
		r := <-ch // the handshake goroutine unblocks once the socket closes
		if r.sess != nil {
			_ = r.sess.Close()
		}
		return nil, fmt.Errorf("no partner appeared within %s", n.cfg.PairWindow)
	case <-ctx.Done():
		_ = conn.Close()
		r := <-ch
		if r.sess != nil {
			_ = r.sess.Close()
		}
		return nil, ctx.Err()
	}
}

// handler builds the read-pump dispatch for every session this node owns.
func (n *Node) handler() peer.Handler {
	return peer.Handler{
		OnMessage: func(text string) {
			if h := n.cfg.OnMessage; h != nil {
				h(text)
			} else {
				fmt.Printf("<< %s\n", text)
			}
		},
		OnError: func(text string) {
			fmt.Fprintf(os.Stderr, "peer reported an error: %s\n", text)
		},
		OnOffer: func(tr *peer.InboundTransfer) bool {
			// Auto-accept every offer: the SPAKE2 handshake already proves
			// the sender knows the code. The transfer runs on its own
			// goroutine so the read pump keeps serving other traffic.
			go func() {
				res, err := tr.Accept(n.cfg.OutDir, progressPrinter(os.Stderr))
				if err != nil {
					n.status("file-error", err.Error())
					return
				}
				if h := n.cfg.OnFile; h != nil {
					h(res, tr.Offer.Name)
				} else if res.OK {
					fmt.Fprintf(os.Stderr, "received file: verified (%s)\n", res.Message)
				} else {
					fmt.Fprintf(os.Stderr, "file receive failed: %s\n", res.Message)
				}
			}()
			return true
		},
	}
}

// setSession publishes a fresh session to waiters.
func (n *Node) setSession(s *peer.Session) {
	n.mu.Lock()
	n.sess = s
	n.mu.Unlock()
	select {
	case n.notify <- struct{}{}:
	default:
	}
}

func (n *Node) clearSession(s *peer.Session) {
	n.mu.Lock()
	if n.sess == s {
		n.sess = nil
	}
	n.mu.Unlock()
}

// waitSession blocks until a session exists, the node terminated, or ctx is
// done.
func (n *Node) waitSession(ctx context.Context) (*peer.Session, error) {
	for {
		n.mu.Lock()
		s := n.sess
		term := n.terminal
		n.mu.Unlock()
		if s != nil {
			return s, nil
		}
		if term != nil {
			return nil, term
		}
		select {
		case <-n.notify:
		case <-n.runDone:
			// Loop once more to pick up the terminal error (or a session set
			// racing the shutdown).
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// SendMessage sends a chat message on the current session.
func (n *Node) SendMessage(ctx context.Context, text string) error {
	sess, err := n.waitSession(ctx)
	if err != nil {
		return err
	}
	return sess.SendMessage(text)
}

// SendFile offers a file and waits for the receiver's verification.
//
// If the connection drops mid-transfer the node reconnects (Run) and the
// offer is repeated; the receiver resumes from its .part file, so only the
// missing tail crosses the wire. It retries until the file is verified, ctx
// is cancelled, or MaxTransferAttempts is exhausted. A rejection (receiver
// said no) is final and returned.
func (n *Node) SendFile(ctx context.Context, path string, progress func(sent, total int64)) (*wire.FileResult, error) {
	for attempt := 1; ; attempt++ {
		if n.cfg.MaxTransferAttempts > 0 && attempt > n.cfg.MaxTransferAttempts {
			return nil, fmt.Errorf("gave up after %d transfer attempts", attempt-1)
		}
		sess, err := n.waitSession(ctx)
		if err != nil {
			return nil, err
		}
		res, err := sess.SendFile(path, progress)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		n.status("transfer-retry", fmt.Sprintf("%s (attempt %d): %v", path, attempt, err))
	}
}

// splitRelays parses a comma-separated relay list.
func splitRelays(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		u, err := url.Parse(part)
		if err != nil || u.Scheme != "ws" && u.Scheme != "wss" {
			return nil, fmt.Errorf("relay %q must be a ws:// or wss:// URL", part)
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, errors.New("no relay URLs given")
	}
	return out, nil
}

func isPermanent(err error) bool {
	return errors.Is(err, peer.ErrHandshake) || errors.Is(err, peer.ErrProtocol)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
