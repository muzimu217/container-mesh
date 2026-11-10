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
		cfg:     cfg,
		log:     cfg.Logger,
		addrs:   addrs,
		updates: make(chan struct{}, 1),
	}
	c.httpc = &http.Client{Timeout: cfg.DialTimeout}
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
	var lastErr error
	for i := 0; i < len(c.addrs); i++ {
		idx := (c.addrIdx + i) % len(c.addrs)
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

	if err := c.Announce(ctx); err != nil {
		// Non-fatal: the loop keeps trying, and a "not a member" failure is
		// the expected pre-approval state — polled on a fast cadence so the
		// member becomes visible within seconds of approval (issue #8:
		// approval used to wait out a full heartbeat).
		c.log.Warn("first announce failed; retrying on heartbeat", "err", err)
	}
	t := time.NewTicker(announceRetryDelay(nil, c.cfg.Heartbeat))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			err := c.Announce(ctx)
			if err != nil {
				c.log.Warn("announce failed", "err", err)
			}
			// Approval-pending failures retry fast; everything else keeps
			// the heartbeat cadence.
			t.Reset(announceRetryDelay(err, c.cfg.Heartbeat))
		}
	}
}

// announceRetryDelay picks the next announce delay: a not-member rejection
// means the approval may land any moment, so poll on a short cadence
// instead of waiting out a full heartbeat.
func announceRetryDelay(err error, heartbeat time.Duration) time.Duration {
	if err != nil && strings.Contains(err.Error(), "not a member") {
		return 3 * time.Second
	}
	return heartbeat
}

// streamLoop dials the members stream, applies pushes, and reconnects with
// capped backoff — advancing to the next address each time a dial or stream
// dies — until the context ends.
func (c *Client) streamLoop(ctx context.Context) error {
	backoff := streamInitialBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := c.streamOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		c.log.Warn("members stream ended; reconnecting", "err", err, "next_retry", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff *= 2
		if backoff > maxStreamBackoff {
			backoff = maxStreamBackoff
		}
		// Each failed attempt lands on the next address, so a dead node is
		// skipped on the reconnect instead of re-timeouting forever.
		c.mu.Lock()
		c.addrIdx = (c.addrIdx + 1) % len(c.addrs)
		c.mu.Unlock()
	}
}

func (c *Client) streamOnce(ctx context.Context) error {
	c.mu.Lock()
	idx := c.addrIdx
	c.mu.Unlock()
	u, err := membersWSURL(c.addrs[idx])
	if err != nil {
		return err
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
		return err
	}
	defer conn.Close()

	for {
		msg, err := conn.Recv()
		if err != nil {
			return err
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
func (c *Client) verifySnapshot(snap *MemberSnapshot) error {
	if snap.AccountID != c.cfg.AccountID {
		return fmt.Errorf("snapshot is for account %s, expected %s", snap.AccountID, c.cfg.AccountID)
	}
	accountPub := ed25519.PublicKey(c.cfg.AccountPub)
	for id, mv := range snap.Members {
		drop := ""
		if err := mv.Cert.Verify(accountPub); err != nil {
			drop = err.Error()
		} else if id != mv.Cert.DeviceID {
			drop = "cert belongs to " + mv.Cert.DeviceID.String()
		} else {
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
func normalizeBaseURL(a string) (*url.URL, error) {
	if !strings.Contains(a, "://") {
		a = "http://" + a
	}
	u, err := url.Parse(a)
	if err != nil {
		return nil, err
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
)
