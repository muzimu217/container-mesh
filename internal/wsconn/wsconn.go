// Package wsconn wraps a WebSocket connection so it looks like a reliable
// message pipe. It is the only transport in this project: outbound TCP/TLS to
// the relay, which works from a container with no inbound connectivity and no
// TUN device.
//
// Design note: WebSocket is chosen deliberately over raw TCP or UDP.
//
//   - It survives HTTP proxies and CDN fronting (Cloudflare, nginx, corporate
//     egress proxies), which is the normal situation for the target containers.
//   - It has no 100 MiB request-body ceiling: that limit applies to HTTP
//     request bodies, not to frames on an upgraded stream.
//   - It gives message framing for free, so the relay can be a dumb pipe.
//
// Liveness: long-lived sessions behind NAT boxes and transparent proxies die
// silently all the time — the mapping expires, the middlebox drops the flow,
// and neither endpoint ever learns about it. Every Conn with Liveness enabled
// therefore sends WebSocket ping frames on a timer and enforces a read
// deadline that pongs (and data) keep pushing forward. A dead connection is
// detected within IdleTimeout instead of hanging forever.
package wsconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Liveness configures WebSocket-level keepalive. Zero PingEvery disables it.
type Liveness struct {
	// PingEvery is how often a WebSocket ping is sent.
	PingEvery time.Duration
	// IdleTimeout closes the connection when nothing has been read — neither
	// data nor pong — for this long. Defaults to 3*PingEvery.
	IdleTimeout time.Duration
}

func (l Liveness) timeout() time.Duration {
	if l.IdleTimeout > 0 {
		return l.IdleTimeout
	}
	return 3 * l.PingEvery
}

// DefaultLiveness is the keepalive used by both peers and relay unless
// overridden. 20s pings keep most NAT mappings alive; 60s without any reply
// means the path is gone.
func DefaultLiveness() Liveness {
	return Liveness{PingEvery: 20 * time.Second, IdleTimeout: 60 * time.Second}
}

// Conn is a message-oriented connection.
type Conn struct {
	ws *websocket.Conn

	writeMu sync.Mutex
	readMu  sync.Mutex
	closed  bool
	closeMu sync.Mutex

	live Liveness
	// stopLive shuts the ping ticker down; liveDone reports its exit.
	stopLive chan struct{}
	liveDone chan struct{}
}

// DialOptions controls outbound connection behaviour.
type DialOptions struct {
	// URL is the relay WebSocket endpoint, e.g. ws://host:8080/ws
	URL string
	// DialTimeout bounds the TCP+TLS+upgrade handshake.
	DialTimeout time.Duration
	// HandshakeHeader carries any extra HTTP headers (e.g. auth).
	HandshakeHeader http.Header
	// InsecureSkipVerify disables TLS certificate verification. Only for
	// self-signed relays in a lab; never in production.
	InsecureSkipVerify bool
	// Liveness enables keepalive on this connection. Recommended for anything
	// long-lived; leave nil only in tests that manage deadlines by hand.
	Liveness *Liveness
}

// Dial opens a client connection.
func Dial(ctx context.Context, opts DialOptions) (*Conn, error) {
	d := websocket.Dialer{
		HandshakeTimeout: opts.DialTimeout,
		ReadBufferSize:   64 << 10,
		WriteBufferSize:  64 << 10,
		// TCP keepalive keeps NAT mappings warm and turns some silent
		// blackholes into errors minutes earlier than WS pings would.
		NetDial: (&net.Dialer{
			Timeout:   opts.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).Dial,
	}
	if opts.InsecureSkipVerify {
		d.TLSClientConfig = insecureTLSConfig()
	}
	ws, resp, err := d.DialContext(ctx, opts.URL, opts.HandshakeHeader)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("wsconn: dial %s: HTTP %d: %w", opts.URL, resp.StatusCode, err)
		}
		return nil, fmt.Errorf("wsconn: dial %s: %w", opts.URL, err)
	}
	c := newConn(ws)
	if opts.Liveness != nil && opts.Liveness.PingEvery > 0 {
		c.EnableLiveness(*opts.Liveness)
	}
	return c, nil
}

// Upgrade turns an inbound HTTP request into a Conn.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	up := websocket.Upgrader{
		ReadBufferSize:  64 << 10,
		WriteBufferSize: 64 << 10,
		// The relay is a public endpoint; peers authenticate with the PAKE
		// code, not with the Origin header. Allow any origin so that a
		// browser-based or proxied client is not rejected.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return nil, fmt.Errorf("wsconn: upgrade: %w", err)
	}
	return newConn(ws), nil
}

func newConn(ws *websocket.Conn) *Conn {
	// No read deadline by default: transfers may legitimately stall while the
	// sender hashes a large file. Keepalive is opt-in via EnableLiveness.
	ws.SetReadLimit(16 << 20) // 16 MiB per message, well above our 1 MiB frames
	return &Conn{ws: ws}
}

// EnableLiveness starts the ping ticker and read-deadline enforcement. Call
// it once, immediately after Dial/Upgrade. It is safe to call twice; the
// second call is a no-op.
func (c *Conn) EnableLiveness(l Liveness) {
	if l.PingEvery <= 0 {
		return
	}
	timeout := l.timeout()
	// A pong proves the peer's read side is alive even when no data flows.
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(timeout))
	})
	_ = c.ws.SetReadDeadline(time.Now().Add(timeout))

	// stopLive is guarded by closeMu, NOT readMu: Close() must never wait on
	// readMu, because a Recv blocked in ReadMessage holds it indefinitely and
	// Close is exactly the call meant to break that block.
	c.closeMu.Lock()
	if c.stopLive != nil {
		c.closeMu.Unlock()
		return
	}
	c.live = l
	c.stopLive = make(chan struct{})
	c.liveDone = make(chan struct{})
	stop, done := c.stopLive, c.liveDone
	c.closeMu.Unlock()

	go func() {
		defer close(done)
		t := time.NewTicker(l.PingEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				// WriteControl is safe alongside data writes and gives up
				// after its own deadline; a failure means the TCP path is
				// gone, so force the read side to notice too.
				if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					_ = c.ws.Close()
					return
				}
			}
		}
	}()
}

// Send writes one binary message. It is safe for concurrent use.
func (c *Conn) Send(b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return fmt.Errorf("wsconn: write: %w", err)
	}
	return nil
}

// Recv reads one binary message. It is safe for concurrent use but only one
// reader should be active at a time (the protocol guarantees this).
//
// With liveness enabled the read deadline is refreshed on entry (any delivered
// message is itself proof of life) and by pong frames arriving mid-read.
func (c *Conn) Recv() ([]byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		if c.live.PingEvery > 0 {
			_ = c.ws.SetReadDeadline(time.Now().Add(c.live.timeout()))
		}
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			return nil, err
		}
		if mt == websocket.BinaryMessage {
			return data, nil
		}
		// Ignore text frames; ping/pong control frames are handled by
		// gorilla (pong auto-reply) and our pong handler.
	}
}

// SetReadDeadline bounds the next Recv call. Zero clears it. It must not be
// used on connections with liveness enabled — the liveness ticker owns the
// deadline there.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.ws.SetReadDeadline(t)
}

// Close closes the underlying socket and stops the liveness ticker. Safe to
// call more than once. It never waits on a blocked Recv — the gorilla read
// is unblocked by the socket close itself.
func (c *Conn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.stopLive != nil {
		close(c.stopLive)
	}
	// Best-effort graceful close; ignore the error because the peer may
	// already be gone.
	_ = c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	return c.ws.Close()
}

// RemoteAddr reports the peer address, when known.
func (c *Conn) RemoteAddr() net.Addr {
	return c.ws.RemoteAddr()
}

// ErrClosed is returned when the peer closed the connection.
var ErrClosed = errors.New("wsconn: connection closed")
