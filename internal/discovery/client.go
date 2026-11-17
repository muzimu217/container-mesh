// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

// The client side of discovery: what every mesh node runs. It announces this
// device on a heartbeat and maintains a verified view of the network from
// pushed snapshots.
//
// Trust model, restated because the verification below implements it
// (MESH-DESIGN §5.1): the discovery node is a member and is trusted to be
// online, not to be honest. Its snapshots are therefore not trusted as-is —
// every certificate in a snapshot is re-verified against the ACCOUNT public
// key the client got in its invite. A hostile or compromised node can deny
// service (withhold pushes) but cannot forge a member: it cannot produce a
// cert the account key signed.
//
// Addresses in cfg.Addrs are base URLs ("http://host:port", or bare
// "host:port" which becomes http). Failover is round-robin: a failed
// operation advances the index, so a dead discovery node costs one timeout,
// not the session. TLS/WSS variants arrive with the deployment polish (M5).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wsconn"
)

// Defaults for zero-value ClientConfig fields.
const (
	DefaultDialTimeout   = 10 * time.Second
	maxStreamBackoff     = 30 * time.Second
	streamInitialBackoff = time.Second
	// DefaultLeaveTimeout bounds the LEAVE broadcast of an explicit
	// graceful departure (P-5b). It sits inside the 3 s convergence
	// budget on purpose: the broadcast plus the view push must fit, and a
	// lost LEAVE still converges through the members stream.
	DefaultLeaveTimeout = 2 * time.Second
	// maxErrBodyBytes bounds how much of an error response is echoed into an
	// error message. Error text is actionable (O-4); unbounded reads of a
	// hostile body are not.
	maxErrBodyBytes = 4 << 10
)

// ClientConfig controls a discovery client.
type ClientConfig struct {
	// Addrs are discovery node base URLs. Bare host:port becomes http://.
	Addrs []string
	// Device is this machine's identity; its key signs announces and stream
	// credentials.
	Device *identity.Device
	// AccountID and AccountPub come from the invite. AccountPub verifies
	// every cert in every snapshot — the client-side membership gate.
	AccountID  identity.DeviceID
	AccountPub []byte
	// Endpoints are the candidate addresses announced in heartbeats. Empty is
	// valid: a device with no inbound path (C-3) is visible and can dial out,
	// it just cannot be dialed yet.
	Endpoints []string
	// NoAnnounce turns the client into a view-only participant: it streams
	// snapshots but never registers presence or endpoints. One-shot commands
	// (mesh ping / dial) must use it: the machine's real announcer is its
	// long-running join process, and a second announcer with no endpoint
	// knowledge would overwrite the announced set with an empty one (issue #8
	// D-7 — the server-side guard in Node.Register is the second layer).
	NoAnnounce bool
	// Heartbeat is the announce period (0 = HeartbeatInterval).
	Heartbeat time.Duration
	// DialTimeout bounds both the HTTP register and the stream dial
	// (0 = DefaultDialTimeout).
	DialTimeout time.Duration
	// LeaveTimeout bounds the LEAVE broadcast of an explicit graceful
	// departure (0 = DefaultLeaveTimeout). The bound keeps a wedged
	// network from holding the departure open past the convergence
	// budget: the LEAVE is an accelerator, and the members stream the
	// client closes right after it is the fallback.
	LeaveTimeout time.Duration
	// HTTPClient overrides the client used for register requests (nil = built
	// from DialTimeout). Tests inject fault-injecting transports; callers
	// behind proxies can pin their own dial behavior.
	HTTPClient *http.Client
	// WebSocketDial overrides the members-stream dialer. Tests use it to route
	// a public discovery origin to an in-process server.
	WebSocketDial func(context.Context, wsconn.DialOptions) (*wsconn.Conn, error)
	// EgressPolicy overrides the outbound address policy for the members
	// stream. nil means the default policy (EgressPolicy): a discovery
	// origin that resolves to a loopback, private or reserved address is
	// refused.
	//
	// A caller sets this to a different function when it wants a stricter
	// or a differently-scoped policy.
	EgressPolicy func(net.Addr) error
	// AllowLocalDiscovery turns the outbound address policy off for the
	// members stream. It exists for a harness that runs the product
	// against a listener on 127.0.0.1, and it is deliberately a separate
	// switch rather than a nil EgressPolicy so that "no policy" and "the
	// default policy" cannot be confused.
	//
	// Production never sets it.
	AllowLocalDiscovery bool
	// Logger receives heartbeat and stream outcomes.
	Logger *slog.Logger
}

// Client announces one device and maintains the verified member view.
type Client struct {
	cfg   ClientConfig
	log   *slog.Logger
	httpc *http.Client

	// addrs are normalized base URLs (scheme present, no trailing slash).
	addrs []string

	mu         sync.Mutex
	snap       *MemberSnapshot
	joined     bool
	addrIdx    int
	maxVersion uint64

	// updates is signalled (buffered, coalescing) whenever the view changes.
	updates chan struct{}

	// streamKick wakes the stream reconnect loop out of its backoff wait
	// when an announce succeeded while this device was still unjoined (see
	// kickStreamIfPending). announceNow asks the heartbeat loop for an
	// out-of-band announce when a stream RECONNECT verifies a snapshot (see
	// signalRecoveryAnnounce). Both are buffered 1: a signal nobody is
	// waiting for right now must not block the sender, and a coalesced
	// repeat is harmless — each is "do it now", not a count.
	streamKick  chan struct{}
	announceNow chan struct{}

	// connSeq numbers members-stream connections; only the stream goroutine
	// reads and writes it.
	connSeq int

	// left marks the graceful departure (P-5b). Once it is set the
	// client announces no more, does not re-establish the members stream,
	// and refuses further announces: a heartbeat that landed after the
	// LEAVE would put the member back online.
	left atomic.Bool
	// leftCh is closed when the departure starts. The announce loop and
	// the stream loop select on it so they unwind immediately instead of
	// waiting out a heartbeat period.
	leftCh   chan struct{}
	leftOnce sync.Once

	// announceMu serializes the announce loop against the LEAVE
	// broadcast. A heartbeat that is in flight when the departure starts
	// must not land AFTER the LEAVE: it would resurrect the member.
	announceMu sync.Mutex

	// lastAnnounceTS is the timestamp of the last announce this client
	// sent (Unix nanoseconds, the member's own clock). It is the
	// presence session a LEAVE names.
	lastAnnounceTS atomic.Int64

	// stopAnnounce cancels the announce loop's context. The departure
	// uses it to abort an announce that is in flight, without tearing
	// down the members stream: the LEAVE broadcast and the stream close
	// are separate steps and the stream must still be there when the
	// departure starts.
	stopAnnounceMu sync.Mutex
	stopAnnounce   context.CancelFunc

	// streamConn is the live members-stream connection (nil while the
	// stream is down). The departure closes it, so the anchor sees a
	// graceful close instead of a dead socket.
	streamConn atomic.Pointer[wsconn.Conn]
}

// NewClient validates the configuration and normalizes addresses. Bad
// addresses are a startup error, not a runtime surprise.
func NewClient(cfg ClientConfig) (*Client, error) {
	if len(cfg.Addrs) == 0 {
		return nil, fmt.Errorf("discovery: client needs at least one discovery address")
	}
	if cfg.Device == nil {
		return nil, fmt.Errorf("discovery: client needs a device identity")
	}
	if len(cfg.AccountPub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("discovery: client needs the account public key (got %d bytes)", len(cfg.AccountPub))
	}
	for _, a := range cfg.Addrs {
		if _, err := normalizeBaseURL(a); err != nil {
			return nil, fmt.Errorf("discovery: bad discovery address %q: %w", a, err)
		}
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = HeartbeatInterval
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	if cfg.LeaveTimeout <= 0 {
		cfg.LeaveTimeout = DefaultLeaveTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	addrs := make([]string, 0, len(cfg.Addrs))
	for _, a := range cfg.Addrs {
		addrs = append(addrs, strings.TrimRight(normalizedAddr(a), "/"))
	}
	c := &Client{
		cfg:         cfg,
		log:         cfg.Logger,
		addrs:       addrs,
		updates:     make(chan struct{}, 1),
		streamKick:  make(chan struct{}, 1),
		announceNow: make(chan struct{}, 1),
		leftCh:      make(chan struct{}),
	}
	if cfg.HTTPClient != nil {
		c.httpc = cfg.HTTPClient
	} else {
		c.httpc = &http.Client{Timeout: cfg.DialTimeout, Transport: c.httpTransport()}
	}
	return c, nil
}

// httpTransport returns the transport for the register requests, with the
// outbound address policy applied.
//
// The register request is an HTTP POST to the discovery origin, and it is
// the same shape as the members stream: an operator-supplied origin that
// must not be allowed to reach the host's own loopback or private network.
// The members stream carries the policy; without it here the register path
// was still able to reach a loopback address, which the external tester
// confirmed with a listener that received the request.
//
// The check runs on the resolved address, inside the dialer, so the
// connection is never made.
func (c *Client) httpTransport() *http.Transport {
	return PolicyTransport(c.cfg.DialTimeout, c.egressPolicy())
}

// egressPolicy returns the outbound address policy for the members stream.
//
// The default refuses a resolved loopback, private or reserved address. A
// caller that set EgressPolicy explicitly gets what it asked for. A caller
// that set AllowLocalDiscovery gets no check at all, which is what a local
// test harness needs.
func (c *Client) egressPolicy() func(net.Addr) error {
	if c.cfg.AllowLocalDiscovery {
		return nil
	}
	if c.cfg.EgressPolicy != nil {
		return c.cfg.EgressPolicy
	}
	return EgressPolicy
}

// Updates returns a channel that receives a signal whenever the member view
// changes. Signals coalesce (buffered size 1): read Snapshot() after each.
func (c *Client) Updates() <-chan struct{} { return c.updates }

// Snapshot returns the current verified view, or nil before the first push.
// The returned pointer is treated as immutable after publication.
func (c *Client) Snapshot() *MemberSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap
}

// Joined reports whether this device is a member of the last verified view.
// False until an approval lands, which is the expected state for a joiner
// waiting out-of-band.
func (c *Client) Joined() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.joined
}

// Announce sends one heartbeat, failing over across configured addresses.
// A live node's 4xx is authoritative (not-membership is not fixed by trying
// another node of the same network), so it is returned as-is; only transport
// failures trigger failover.
//
// After a graceful departure (Leave) the client announces no more: a
// heartbeat that landed after the LEAVE would put the member back online.
func (c *Client) Announce(ctx context.Context) error {
	if c.left.Load() {
		return errors.New("discovery: this device left the network; no further announces")
	}
	c.announceMu.Lock()
	defer c.announceMu.Unlock()
	if c.left.Load() {
		return errors.New("discovery: this device left the network; no further announces")
	}
	a := MemberAnnounce{
		DeviceID:  c.cfg.Device.ID(),
		Endpoints: c.cfg.Endpoints,
		Timestamp: time.Now(),
	}
	// The presence session this heartbeat establishes. A LEAVE names it
	// so the node can tell a departure of the CURRENT session from a late
	// one belonging to a connection that is already gone.
	c.lastAnnounceTS.Store(a.Timestamp.UnixNano())
	body, err := json.Marshal(RegisterRequest{Announce: a, Sig: a.Sign(c.cfg.Device)})
	if err != nil {
		return err
	}
	return c.postRegister(ctx, body, "announce")
}

// postRegister sends a signed register body to the discovery node, failing
// over across the configured addresses.
//
// The failure semantics are the announce path's, unchanged: a live node's
// 4xx is authoritative (not-membership is not fixed by trying another node
// of the same network), only transport failures trigger failover.
func (c *Client) postRegister(ctx context.Context, body []byte, what string) error {
	// addrIdx is also written by the stream reconnect loop; read it under
	// the same lock (race detector: Announce's unlocked read raced
	// streamLoop's locked rotate on shared-list runs).
	c.mu.Lock()
	base := c.addrIdx
	c.mu.Unlock()
	var lastErr error
	for i := 0; i < len(c.addrs); i++ {
		idx := (base + i) % len(c.addrs)
		u := c.addrs[idx] + registerPath
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.httpc.Do(req)
		if err != nil {
			lastErr = err
			c.log.Warn(what+" failed; failing over", "addr", c.addrs[idx], "err", err)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			c.mu.Lock()
			c.addrIdx = idx
			c.mu.Unlock()
			_ = resp.Body.Close()
			return nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes))
		_ = resp.Body.Close()
		return &registerRejectedError{
			Addr:   c.addrs[idx],
			Status: resp.StatusCode,
			Body:   printableBody(b),
		}
	}
	return fmt.Errorf("discovery: register failed on all %d addresses: %w", len(c.addrs), lastErr)
}

// Leave performs the explicit graceful departure (P-5b) and takes this
// device out of the mesh.
//
// The departure has three steps, in this order:
//
//  1. The announce loop stops. A heartbeat that landed after the LEAVE
//     would put the member back online — the departure is what the rest of
//     the network must see, not a race with the next heartbeat.
//  2. The LEAVE is broadcast: a signed, timestamped announcement that the
//     discovery node applies like any other announce, so it ends the
//     member's presence and pushes the new view to every other member.
//  3. The members stream is closed. That is the FALLBACK: if the LEAVE was
//     lost on the way, the anchor still sees the stream go away and expires
//     the presence through the transport path (P-5).
//
// The LEAVE is an accelerator, never the only mechanism: step 3 converges on
// its own whenever the transport path is intact, and a lost LEAVE still
// leaves the member offline — the only difference is how fast.
//
// Leave is idempotent: the departure is broadcast once, and calling Leave
// again is harmless.
func (c *Client) Leave(ctx context.Context) error {
	// 1. Stop announcing. cancelAnnounce aborts an announce that is in
	// flight (its request dies with the context), so the announce mutex
	// is free again and no heartbeat can follow the LEAVE.
	c.left.Store(true)
	c.leftOnce.Do(func() { close(c.leftCh) })
	c.cancelAnnounce()

	// The broadcast is bounded: a wedged network must not hold the
	// departure open past the convergence budget.
	lctx := ctx
	if c.cfg.LeaveTimeout > 0 {
		var cancel context.CancelFunc
		lctx, cancel = context.WithTimeout(ctx, c.cfg.LeaveTimeout)
		defer cancel()
	}

	// 2. Broadcast the departure.
	err := c.sendLeave(lctx)

	// 3. Close the members stream: the anchor sees a graceful close and
	// expires the presence even if the LEAVE never arrived.
	if conn := c.streamConn.Load(); conn != nil {
		_ = conn.Close()
	}
	return err
}

// sendLeave broadcasts the explicit departure message.
//
// It names the presence session it ends (the last announce this client
// sent) so the node can tell it apart from a late departure of an older
// session — the member may have reconnected in the meantime, and that new
// connection must not be taken offline.
func (c *Client) sendLeave(ctx context.Context) error {
	c.announceMu.Lock()
	defer c.announceMu.Unlock()
	a := MemberAnnounce{
		DeviceID:  c.cfg.Device.ID(),
		Timestamp: time.Now(),
		Leave:     true,
		Session:   c.lastAnnounceTS.Load(),
	}
	if snap := c.Snapshot(); snap != nil {
		a.View = snap.Version
	}
	body, err := json.Marshal(RegisterRequest{Announce: a, Sig: a.Sign(c.cfg.Device)})
	if err != nil {
		return err
	}
	return c.postRegister(ctx, body, "leave")
}

// cancelAnnounce stops the announce loop. It is safe to call when the loop
// was never started.
func (c *Client) cancelAnnounce() {
	c.stopAnnounceMu.Lock()
	cancel := c.stopAnnounce
	c.stopAnnounceMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// setStopAnnounce records the announce loop's cancel function so a
// departure can abort an in-flight announce.
func (c *Client) setStopAnnounce(cancel context.CancelFunc) {
	c.stopAnnounceMu.Lock()
	c.stopAnnounce = cancel
	c.stopAnnounceMu.Unlock()
}

// Run blocks until ctx is cancelled: heartbeat loop plus snapshot stream,
// each retrying with failover until the context ends. The first announce
// fires immediately so a joiner is visible without waiting one period.
// View-only clients (NoAnnounce) run the stream alone — their machine's
// announcer is the long-running join process. Run returns ctx.Err() on
// cancellation.
//
// A graceful departure (Leave) ends Run with a nil error: leaving is a
// decision, not a failure.
func (c *Client) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The announce loop gets its OWN context. The departure cancels the
	// announce (no further heartbeat may resurrect the member) WITHOUT
	// tearing down the members stream: the LEAVE broadcast and the stream
	// close are separate steps, and the stream must still be alive when
	// the departure starts — otherwise the departure would degrade into
	// the abnormal-disconnect path instead of the explicit broadcast.
	announceCtx, stopAnnounce := context.WithCancel(ctx)
	c.setStopAnnounce(stopAnnounce)
	defer stopAnnounce()
	errc := make(chan error, 1)
	go func() { errc <- c.streamLoop(ctx) }()

	if c.cfg.NoAnnounce {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.leftCh:
			return nil
		}
	}

	firstErr := c.Announce(announceCtx)
	if firstErr != nil {
		// Non-fatal: the loop keeps trying, and a "not a member" failure is
		// the expected pre-approval state — the FIRST retry must already be
		// on the fast cadence so the member becomes visible within seconds
		// of approval (issue #8: approval used to wait out a full heartbeat;
		// netns-probe round 20: passing nil armed the first ticker at the
		// full heartbeat, so approvals landing in the first window waited
		// ~30 s to become dialable).
		c.log.Warn("first announce failed; retrying", "err", firstErr)
	}
	t := time.NewTicker(announceRetryDelay(c.Joined(), firstErr, c.cfg.Heartbeat))
	defer t.Stop()
	for {
		select {
		case <-announceCtx.Done():
			if c.left.Load() {
				return nil // a deliberate departure, not a failure
			}
			return announceCtx.Err()
		case <-c.leftCh:
			return nil
		case <-c.announceNow:
			// A stream recovery asked for an out-of-band announce: the
			// server expired this device's presence when the old stream
			// detached, and until the next announce every subscriber's
			// view shows it offline — waiting for the heartbeat left
			// 0–30 s of stale "offline" after the transport was already
			// back (R40-1, the recovery-visibility batch with C-4).
			err := c.Announce(announceCtx)
			if err != nil {
				c.log.Warn("recovery announce failed", "err", err)
			}
			t.Reset(announceRetryDelay(c.Joined(), err, c.cfg.Heartbeat))
		case <-t.C:
			err := c.Announce(announceCtx)
			if err != nil {
				c.log.Warn("announce failed", "err", err)
			} else {
				// Registered fine but still no verified view of our own
				// membership: the approval that let this announce through
				// has not reached the stream yet — wake it instead of
				// letting it wait out its backoff ladder (doubao round 51:
				// 26.7 s approval→dialable after a long pending wait).
				c.kickStreamIfPending()
			}
			// While unjoined, ANY failure polls fast (approval may land any
			// moment) except a rate-limit rejection, which backs off — the
			// budget is shared per egress address. Once joined, the
			// heartbeat cadence carries endpoint freshness and transient
			// failures do not accelerate it.
			t.Reset(announceRetryDelay(c.Joined(), err, c.cfg.Heartbeat))
		}
	}
}

// announceRetryDelay picks the next announce delay. Before the first
// verified membership view, poll fast no matter WHY the announce failed:
// not-member is the normal pre-approval state, but a transport failure
// (joiner racing a restarting node) used to fall back to a full heartbeat —
// an approval landing in that window waited ~27 s instead of ~3 s
// (netns-probe round 25, 25× the not-member path). Once joined, failures
// keep the heartbeat cadence: a node outage must not turn the whole
// membership into a 3 s request storm.
//
// The one pre-join exception is a rate-limit rejection: the register budget
// is shared per EGRESS address (rounds 33-35 — 120 tokens/min + 2/s refill,
// pairing /ws included), so when the bucket is empty, keeping a 3 s cadence
// wastes the whole site's allowance: round 34 measured D-9b's steady demand
// multiplying rejected requests 8× with zero improvement in worst-case
// approval latency. Back off past the refill window, and jitter BOTH
// pre-join delays so same-site joiners de-synchronize instead of phasing
// onto the same ticks.
func announceRetryDelay(joined bool, err error, heartbeat time.Duration) time.Duration {
	if joined {
		return heartbeat
	}
	if isRateLimited(err) {
		return announceRateBackoff + time.Duration(rand.Int64N(int64(announceRateJitter)))
	}
	return announceFastRetry + time.Duration(rand.Int64N(int64(announceFastJitter)))
}

// RegisterRejectedError is a node's refusal of a register request, with the
// status code kept alongside the text.
//
// It is exported so that the classification can be tested from outside the
// package: the verdict is the product's behaviour, not an implementation
// detail, and the shape that broke CI (a node whose PORT contains "429")
// cannot be produced by a test server that only listens on a real port.
type RegisterRejectedError struct {
	Addr   string
	Status int
	Body   string
}

func (e *RegisterRejectedError) Error() string {
	return fmt.Sprintf("discovery: register rejected by %s: HTTP %d: %s", e.Addr, e.Status, e.Body)
}

// IsRateLimited reports whether err is the limiter's rejection.
//
// It is exported for the same reason as RegisterRejectedError: the rule
// ("the verdict comes from the status code, never from the address") is
// the product's behaviour.
func IsRateLimited(err error) bool { return isRateLimited(err) }

// IsNotAMember reports whether err is the node's refusal on membership
// grounds: this device is not (or no longer) a member of the network.
//
// The node answers a register from a non-member with 401 and the reason
// "device is not a member" (internal/discovery.ErrAnnounceNotMember).
// The caller needs to tell that apart from a network fault, because the
// two demand different things of the operator: one is "you were revoked",
// the other is "the node is unreachable".
//
// The verdict comes from the STATUS CODE. It used to be read out of the
// error text with strings.Contains(err.Error(), "not a member"), which
// couples the client's behaviour to the server's wording: reword the
// message and the client would quietly stop recognising a revocation, and
// tell the operator to go debug their routing instead.
func IsNotAMember(err error) bool {
	if err == nil {
		return false
	}
	var rej *registerRejectedError
	if errors.As(err, &rej) {
		return rej.Status == http.StatusUnauthorized
	}
	return false
}

// registerRejectedError is the internal alias of the exported type, so the
// existing internal call sites read unchanged.
type registerRejectedError = RegisterRejectedError

// isRateLimited reports whether err is the limiter's rejection.
//
// The relay answers 429 with body "too many requests" (the rate bucket) or
// "too many connections" (the in-flight cap) — both mean this egress site's
// shared allowance is exhausted and retrying fast only burns other
// devices' share.
//
// The verdict comes from the status code when there is one. Falling back to
// the text is what this function used to do, and it misfired: the error
// text carries the node's address, so a node on a port that happens to
// contain "429" turned every error into a rate-limit verdict.
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var rej *registerRejectedError
	if errors.As(err, &rej) {
		if rej.Status == http.StatusTooManyRequests {
			return true
		}
		// A node that is not answering 429 may still say so in prose
		// ("too many requests" in the body). That is the limiter's own
		// wording, and it only appears on a refusal — never in a transport
		// error, which has no status code at all.
		body := strings.ToLower(rej.Body)
		return strings.Contains(body, "too many request") || strings.Contains(body, "too many connection")
	}
	return false
}

// streamLoop dials the members stream, applies pushes, and reconnects with
// capped backoff — advancing to the next address each time a dial or stream
// dies — until the context ends.
//
// The backoff is RESET whenever a snapshot was received and verified: the
// server kicks a slow consumer as a cleanup measure, and for the kicked
// client that is not a failure but a reconnect opportunity. Without the
// reset the penalty doubled on every kick (1s→2s→…→30s), so a client under
// a snapshot storm ended up in a 30s reconnect gap — and any state change
// landing in that gap was delivered 30s late (issue #8, netns-probe round
// 7 root cause).
func (c *Client) streamLoop(ctx context.Context) error {
	backoff := streamInitialBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		if c.left.Load() {
			// The departure is done: the stream must not come back, or the
			// member would reappear in the view it just left.
			return nil
		}
		healthy, err := c.streamOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if c.left.Load() {
			return nil
		}
		// A stream that produced a verified snapshot was healthy: reconnect
		// promptly AND reset the penalty ladder. A stream that failed gets
		// the current backoff and pushes the ladder one step further (the
		// original behaviour for real failures).
		//
		// The reset is what keeps a server-side cleanup kick (the server
		// dropping a slow consumer) from being charged to the client as a
		// failure: without it, repeated kicks walked the delay 1s→2s→4s→…
		//→30s and any state change landing in that gap arrived 30s late
		// (issue #8 #7, netns-probe round 7).
		if healthy {
			backoff = streamInitialBackoff
		}
		delay := backoff
		if !healthy {
			backoff *= 2
			if backoff > maxStreamBackoff {
				backoff = maxStreamBackoff
			}
		}
		c.log.Warn("members stream ended; reconnecting", "err", err,
			"healthy", healthy, "next_retry", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-c.leftCh:
			// A departure started while the stream was down: do not dial
			// the member back into the network it just left.
			timer.Stop()
			return nil
		case <-c.streamKick:
			// An announce succeeded while this device was still unjoined:
			// approval just landed, and the remaining backoff wait — built
			// up from pre-approval rejections — is the only thing between
			// us and the snapshot that makes the device dialable. Skip it.
			timer.Stop()
		case <-timer.C:
		}
		// Each failed attempt lands on the next address, so a dead node is
		// skipped on the reconnect instead of re-timeouting forever.
		c.mu.Lock()
		c.addrIdx = (c.addrIdx + 1) % len(c.addrs)
		c.mu.Unlock()
	}
}

func (c *Client) streamOnce(ctx context.Context) (bool, error) {
	if c.left.Load() {
		return false, errors.New("discovery: this device left the network")
	}
	c.connSeq++
	connNo := c.connSeq
	c.mu.Lock()
	idx := c.addrIdx
	c.mu.Unlock()
	u, err := membersWSURL(c.addrs[idx])
	if err != nil {
		return false, err
	}
	options := wsconn.DialOptions{
		URL:         u,
		DialTimeout: c.cfg.DialTimeout,
		HandshakeHeader: func() http.Header {
			ts := time.Now().UTC().Format(time.RFC3339Nano)
			signable := MeshAuthContext + "\x00" + c.cfg.Device.ID().String() + "\x00" + ts
			h := http.Header{}
			h.Set(HeaderMeshDevice, c.cfg.Device.ID().String())
			h.Set(HeaderMeshTime, ts)
			h.Set(HeaderMeshSig, base64.StdEncoding.EncodeToString(c.cfg.Device.Sign([]byte(signable))))
			return h
		}(),
		Liveness: func() *wsconn.Liveness { l := wsconn.DefaultLiveness(); return &l }(),
		// The host check upstream can only judge a literal address. This is
		// the second half: the address the name resolved to, checked just
		// before the socket is connected. Without it a name pointing at a
		// loopback or private address walks straight through the policy.
		EgressPolicy: c.egressPolicy(),
	}
	var conn *wsconn.Conn
	if c.cfg.WebSocketDial != nil {
		conn, err = c.cfg.WebSocketDial(ctx, options)
	} else {
		conn, err = wsconn.Dial(ctx, options)
	}
	if err != nil {
		return false, err
	}
	defer conn.Close()
	// Publish the live connection so a departure can close it: the anchor
	// then sees a graceful close and expires the presence immediately,
	// instead of waiting for the socket to die (the P-5 fallback for a
	// LEAVE that never arrived).
	c.streamConn.Store(conn)
	defer c.streamConn.CompareAndSwap(conn, nil)
	// A departure may have started while the dial was in flight. The
	// connection must not outlive it: close it right away instead of
	// holding a stream the node has already been told to forget.
	if c.left.Load() {
		_ = conn.Close()
		return false, errors.New("discovery: this device left the network")
	}

	// healthy records whether this stream produced a verified snapshot: the
	// caller resets its reconnect penalty on that signal. The stream itself
	// keeps running until it breaks — returning early here would turn the
	// client into a 1-second reconnect loop.
	healthy := false
	// announced pins the recovery signal to the FIRST verified snapshot of
	// this connection — one out-of-band announce per reconnect, not one per
	// push.
	announced := false
	for {
		msg, err := conn.Recv()
		if err != nil {
			return healthy, err
		}
		var snap MemberSnapshot
		if err := json.Unmarshal(msg, &snap); err != nil {
			// A node that cannot speak its own protocol is broken, not
			// hostile in a way verification below wouldn't also catch — but
			// do not poison the view with it either.
			c.log.Warn("undecodable snapshot ignored", "err", err)
			continue
		}
		if err := c.verifySnapshot(&snap); err != nil {
			c.log.Warn("snapshot rejected", "err", err)
			continue
		}
		c.applySnapshot(&snap)
		healthy = true
		if !announced {
			announced = true
			c.signalRecoveryAnnounce(connNo)
		}
	}
}

// signalRecoveryAnnounce asks the heartbeat loop for an out-of-band announce
// when a RECONNECTED stream verifies its first snapshot. The old stream's
// detach expired this device's presence server-side (the fast-offline path),
// so until the next announce every subscriber's view shows it offline no
// matter how fresh its own view already is (R40-1: recovery visibility used
// to be bound by the heartbeat, 0–30 s after the transport was back). The
// first connection ever is exempt: Run's immediate first announce covers it,
// and one extra register per client start would double the join burst on the
// shared egress budget.
func (c *Client) signalRecoveryAnnounce(connNo int) {
	if connNo <= 1 || c.cfg.NoAnnounce {
		return
	}
	select {
	case c.announceNow <- struct{}{}:
	default:
	}
}

// kickStreamIfPending wakes the members-stream loop when an announce
// succeeded while no verified view contains this device. The register gate
// and the stream gate consult the same member list, so the success means an
// approval just landed — and the stream, still backing off from its
// pre-approval rejections, is the only thing delaying the snapshot that
// makes the device dialable (doubao round 51: 26.7 s approval→dialable after
// a 35 s pending wait; the ladder had climbed to its cap). No-op once
// joined; a signal sent in the race window before Joined flips at worst
// skips one backoff step on a later reconnect.
func (c *Client) kickStreamIfPending() {
	if c.Joined() {
		return
	}
	select {
	case c.streamKick <- struct{}{}:
	default:
	}
}

// verifySnapshot enforces the trust model: right account, and every member
// cert genuinely signed by the account key (which also re-checks expiry and
// the ID/key binding).
//
// A member that fails ANY check is DROPPED, not fatal. Two forces point the
// same way: a forged member must simply not exist in the view, and one
// expired certificate (a 365-day TTL guarantees this happens eventually)
// must not blind the whole network — rejecting the entire snapshot would
// let either failure become a self-inflicted denial of service (security
// review F3). Endpoint strings are re-validated here because the node is
// trusted to be online, not honest: what it says about addresses is
// filtered by the same rules announces obey (review F5).
//
// A certificate signed by a DELEGATOR (a member that joined under a team
// credential) is NOT verifiable with the account key — the account never
// signed it. It is accepted only when the snapshot carries a delegation
// record authorising that exact signing key: the delegation is checked
// against the account key, and the certificate against the delegator. A
// delegated certificate with no matching delegation is still a forgery.
func (c *Client) verifySnapshot(snap *MemberSnapshot) error {
	if snap.AccountID != c.cfg.AccountID {
		return fmt.Errorf("snapshot is for account %s, expected %s", snap.AccountID, c.cfg.AccountID)
	}
	accountPub := ed25519.PublicKey(c.cfg.AccountPub)
	delegations := make(map[string]identity.Delegation, len(snap.Delegations))
	for _, d := range snap.Delegations {
		if err := d.Verify(accountPub, time.Now().UTC()); err != nil {
			c.log.Warn("snapshot delegation rejected", "id", d.ID, "reason", err)
			continue
		}
		delegations[string(d.DelegatorPub)] = d
	}
	for id, mv := range snap.Members {
		drop := ""
		switch {
		case len(mv.Cert.SignerPub) > 0:
			d, ok := delegations[string(mv.Cert.SignerPub)]
			if !ok {
				drop = "signed by a delegator that no member of this network is authorised to use"
			} else if err := mv.Cert.VerifySigner(d.DelegatorPub); err != nil {
				drop = "delegated certificate rejected: " + err.Error()
			}
		case mv.Cert.Verify(accountPub) != nil:
			drop = mv.Cert.Verify(accountPub).Error()
		case id != mv.Cert.DeviceID:
			drop = "cert belongs to " + mv.Cert.DeviceID.String()
		default:
			kept := make([]string, 0, len(mv.Endpoints))
			for _, e := range mv.Endpoints {
				if validEndpoint(e) {
					kept = append(kept, e)
				}
			}
			// The endpoint count is capped here as well as on the announce
			// path. Without it, a node that is not trusted to be honest
			// could put hundreds of endpoints in one snapshot, and every
			// one of them would be dialled in turn: the dial cost grows
			// with the list, and each attempt costs a handshake timeout.
			// The measurement behind this: a snapshot with 500 endpoints
			// was accepted whole, and a probe with a three second budget
			// ran for over five seconds.
			if len(kept) > MaxEndpoints {
				kept = kept[:MaxEndpoints]
			}
			mv.Endpoints = kept
			snap.Members[id] = mv
		}
		if drop != "" {
			delete(snap.Members, id)
			c.log.Warn("snapshot member dropped", "device", id, "reason", drop)
		}
	}
	return nil
}

// applySnapshot publishes a verified view. Version must be monotonic (never
// decreasing): snapshots carry the member-list version exactly so a stale
// copy — replayed by a malicious or misbehaving node — cannot resurrect a
// revoked member (review F1). Equal versions are applied: liveness changes
// ride the same version as the membership they describe.
func (c *Client) applySnapshot(snap *MemberSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if snap.Version < c.maxVersion {
		c.log.Warn("stale snapshot ignored", "version", snap.Version, "current", c.maxVersion)
		return
	}
	c.maxVersion = snap.Version
	c.snap = snap
	_, present := snap.Members[c.cfg.Device.ID()]
	c.joined = present
	select {
	case c.updates <- struct{}{}:
	default:
	}
}

// normalizeBaseURL parses a configured address, defaulting the scheme to
// http and requiring that nothing beyond the origin is present.
// NormalizeDiscoveryURL turns a configured discovery address into the base
// URL a request should use. It is the same rule the client applies
// internally (bare host:port, https on port 443, http otherwise), exposed
// so other packages — the credential enrollment path — address the same
// node the same way.
func NormalizeDiscoveryURL(a string) (string, error) {
	u, err := normalizeBaseURL(a)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func normalizeBaseURL(a string) (*url.URL, error) {
	if !strings.Contains(a, "://") {
		// Bare "host:port" defaults to https on the TLS port: a discovery
		// node published on 443 (the one port most restricted egress
		// allows) must be usable without spelling out the scheme. Anything
		// else stays plain http until a deployment opts into TLS.
		if _, port, err := net.SplitHostPort(a); err == nil && port == "443" {
			a = "https://" + a
		} else {
			a = "http://" + a
		}
	}
	u, err := url.Parse(a)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("unsupported scheme %q (http or https)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("no host")
	}
	if host := u.Hostname(); host == "" {
		return nil, fmt.Errorf("no host")
	} else if !allowedDiscoveryHost(host) {
		return nil, fmt.Errorf("disallowed host %q", host)
	}
	if u.Port() == "" {
		return nil, fmt.Errorf("no port")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("unexpected path %q", u.Path)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("unexpected query or fragment")
	}
	return u, nil
}

func normalizedAddr(a string) string {
	u, err := normalizeBaseURL(a)
	if err != nil {
		return a
	}
	u.Path = ""
	return u.String()
}

// membersWSURL derives the stream endpoint: same origin, ws/wss scheme.
func membersWSURL(base string) (string, error) {
	u, err := normalizeBaseURL(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = membersPath
	return u.String(), nil
}

// printableBody makes a server-supplied error body safe for logs and
// terminals: control characters — including CR/LF, which would forge log
// lines, and ANSI escapes — become spaces before the text reaches an error
// message a client will print (security review F8).
func printableBody(b []byte) string {
	var sb strings.Builder
	for _, r := range string(b) {
		if r < 0x20 || r == 0x7f {
			sb.WriteByte(' ')
		} else {
			sb.WriteRune(r)
		}
	}
	return strings.TrimSpace(sb.String())
}

const (
	registerPath = "/mesh/register"
	membersPath  = "/mesh/members"
	// Pre-join announce cadence: fast, with jitter so same-egress joiners
	// de-synchronize (rounds 33-35: the register budget is shared per
	// egress address, and phased tickers waste it together).
	announceFastRetry  = 3 * time.Second
	announceFastJitter = 600 * time.Millisecond
	// Rate-limit rejections back off past the 2/s refill window — retrying
	// fast only burns other devices' share (round 34: 8× rejected requests,
	// zero faster joins).
	announceRateBackoff = 8 * time.Second
	announceRateJitter  = 4 * time.Second
)
