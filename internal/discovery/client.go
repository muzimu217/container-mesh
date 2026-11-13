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
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wsconn"
)

// Defaults for zero-value ClientConfig fields.
const (
	DefaultDialTimeout   = 10 * time.Second
	maxStreamBackoff     = 30 * time.Second
	streamInitialBackoff = time.Second
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
	// HTTPClient overrides the client used for register requests (nil = built
	// from DialTimeout). Tests inject fault-injecting transports; callers
	// behind proxies can pin their own dial behavior.
	HTTPClient *http.Client
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
	}
	if cfg.HTTPClient != nil {
		c.httpc = cfg.HTTPClient
	} else {
		c.httpc = &http.Client{Timeout: cfg.DialTimeout}
	}
	return c, nil
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
func (c *Client) Announce(ctx context.Context) error {
	a := MemberAnnounce{
		DeviceID:  c.cfg.Device.ID(),
		Endpoints: c.cfg.Endpoints,
		Timestamp: time.Now(),
	}
	body, err := json.Marshal(RegisterRequest{Announce: a, Sig: a.Sign(c.cfg.Device)})
	if err != nil {
		return err
	}
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
			c.log.Warn("announce failed; failing over", "addr", c.addrs[idx], "err", err)
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
		return fmt.Errorf("discovery: register rejected by %s: HTTP %d: %s",
			c.addrs[idx], resp.StatusCode, printableBody(b))
	}
	return fmt.Errorf("discovery: register failed on all %d addresses: %w", len(c.addrs), lastErr)
}

// Run blocks until ctx is cancelled: heartbeat loop plus snapshot stream,
// each retrying with failover until the context ends. The first announce
// fires immediately so a joiner is visible without waiting one period.
// View-only clients (NoAnnounce) run the stream alone — their machine's
// announcer is the long-running join process. Run returns ctx.Err() on
// cancellation.
func (c *Client) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- c.streamLoop(ctx) }()

	if c.cfg.NoAnnounce {
		<-ctx.Done()
		return ctx.Err()
	}

	firstErr := c.Announce(ctx)
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
		case <-ctx.Done():
			return ctx.Err()
		case <-c.announceNow:
			// A stream recovery asked for an out-of-band announce: the
			// server expired this device's presence when the old stream
			// detached, and until the next announce every subscriber's
			// view shows it offline — waiting for the heartbeat left
			// 0–30 s of stale "offline" after the transport was already
			// back (R40-1, the recovery-visibility batch with C-4).
			err := c.Announce(ctx)
			if err != nil {
				c.log.Warn("recovery announce failed", "err", err)
			}
			t.Reset(announceRetryDelay(c.Joined(), err, c.cfg.Heartbeat))
		case <-t.C:
			err := c.Announce(ctx)
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

// isRateLimited reports whether err is the limiter's rejection. The relay
// answers 429 with body "too many requests" (rate bucket) or "too many
// connections" (in-flight cap) — both mean this egress site's shared
// allowance is exhausted and retrying fast only burns other devices' share.
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "429") || strings.Contains(s, "too many request") ||
		strings.Contains(s, "too many connection")
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
		healthy, err := c.streamOnce(ctx)
		if ctx.Err() != nil {
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
	c.connSeq++
	connNo := c.connSeq
	c.mu.Lock()
	idx := c.addrIdx
	c.mu.Unlock()
	u, err := membersWSURL(c.addrs[idx])
	if err != nil {
		return false, err
	}
	conn, err := wsconn.Dial(ctx, wsconn.DialOptions{
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
	})
	if err != nil {
		return false, err
	}
	defer conn.Close()

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
