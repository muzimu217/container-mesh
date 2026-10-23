// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"
)

// Status is the relay's self-report.
//
// Everything here is a number the relay already has or can compute in constant
// time. Nothing identifies a peer: no room labels, no addresses, no codes. A
// dashboard is an observability tool, and an observability tool that leaks the
// thing the protocol exists to hide would be a regression dressed as a feature.
type Status struct {
	Version string `json:"version"`
	// UptimeSeconds is how long this process has been serving.
	UptimeSeconds int64 `json:"uptime_seconds"`
	// WaitingPeers is how many peers are parked, waiting for a partner.
	// Each waiting peer is one role in one room; two waiting peers that match
	// become a session and leave this count.
	WaitingPeers int `json:"waiting_peers"`
	// Rooms is how many room entries exist, i.e. distinct codes currently
	// being waited on. It is not a session count: a room entry is deleted the
	// moment its two peers pair.
	Rooms int `json:"rooms"`
	// ActiveSessions is how many peer pairs are currently spliced together.
	ActiveSessions int64 `json:"active_sessions"`
	// MessagesRelayed and BytesRelayed are cumulative since start.
	MessagesRelayed int64 `json:"messages_relayed"`
	BytesRelayed    int64 `json:"bytes_relayed"`
	// Goroutines is a cheap leak signal: it should track sessions, not grow.
	Goroutines int `json:"goroutines"`
	// Limits lets an operator see the configured ceilings without reading the
	// command line it was started with.
	MaxRooms          int   `json:"max_rooms"`
	MaxMessageBytes   int64 `json:"max_message_bytes"`
	MaxConnsPerIP     int   `json:"max_conns_per_ip"`
	ConnectRatePerMin int   `json:"connect_rate_per_min"`
}

// StatusSnapshot builds a Status under the server lock.
func (s *Server) StatusSnapshot() Status {
	s.mu.Lock()
	rooms := len(s.rooms)
	waiting := 0
	for _, rm := range s.rooms {
		waiting += len(rm.roles)
	}
	s.mu.Unlock()

	return Status{
		Version:           version,
		UptimeSeconds:     int64(time.Since(s.startedAt).Seconds()),
		WaitingPeers:      waiting,
		Rooms:             rooms,
		ActiveSessions:    s.activeSessions.Load(),
		MessagesRelayed:   s.messagesRelayed.Load(),
		BytesRelayed:      s.bytesRelayed.Load(),
		Goroutines:        runtime.NumGoroutine(),
		MaxRooms:          s.cfg.MaxRooms,
		MaxMessageBytes:   s.cfg.MaxMessageBytes,
		MaxConnsPerIP:     s.cfg.MaxConnsPerIP,
		ConnectRatePerMin: s.cfg.ConnectRatePerMin,
	}
}

// version is stamped at build time, mirroring the CLI's.
var version = "dev"

// SetVersion lets the CLI pass its build version through, so the dashboard
// reports the same string `cmesh version` prints.
func SetVersion(v string) { version = v }

// handleStatus serves the JSON snapshot.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s.StatusSnapshot())
}

// handleDashboard serves the single-page status board.
//
// The page is inlined rather than served from disk: the relay is distributed as
// one static binary, and requiring a companion file would break that. It polls
// /status and draws the numbers; there is no build step and no framework,
// because a monitoring page that itself needs a toolchain is a liability.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(dashboardHTML))
}

// dashboardHTML is the whole UI: one page, no dependencies, no external
// requests. It works offline and cannot leak a request to a third party.
const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>container-mesh relay</title>
<style>
  :root {
    --bg: #0d1117;
    --panel: #161b22;
    --line: #21262d;
    --text: #e6edf3;
    --muted: #7d8590;
    --accent: #3fb950;
    --warn: #d29922;
    --idle: #484f58;
  }
  @media (prefers-color-scheme: light) {
    :root {
      --bg: #ffffff;
      --panel: #f6f8fa;
      --line: #d0d7de;
      --text: #1f2328;
      --muted: #656d76;
      --accent: #1a7f37;
      --warn: #9a6700;
      --idle: #d0d7de;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    padding: 32px 24px;
    background: var(--bg);
    color: var(--text);
    font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    -webkit-font-smoothing: antialiased;
  }
  .wrap { max-width: 880px; margin: 0 auto; }
  header {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 16px;
    flex-wrap: wrap;
    margin-bottom: 28px;
  }
  h1 { font-size: 15px; font-weight: 600; margin: 0; letter-spacing: .02em; }
  .dot {
    display: inline-block;
    width: 7px; height: 7px;
    border-radius: 50%;
    background: var(--idle);
    margin-right: 8px;
    vertical-align: middle;
    transition: background .3s;
  }
  .dot.live { background: var(--accent); }
  .dot.stale { background: var(--warn); }
  .meta { color: var(--muted); font-size: 12px; }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
    gap: 1px;
    background: var(--line);
    border: 1px solid var(--line);
    border-radius: 6px;
    overflow: hidden;
  }
  .cell { background: var(--panel); padding: 16px 18px; }
  .cell .k {
    color: var(--muted);
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: .06em;
    margin-bottom: 6px;
  }
  .cell .v {
    font-size: 22px;
    font-weight: 600;
    font-variant-numeric: tabular-nums;
    letter-spacing: -.01em;
  }
  .cell .v small { font-size: 12px; font-weight: 400; color: var(--muted); }
  section { margin-top: 28px; }
  h2 {
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: .06em;
    color: var(--muted);
    margin: 0 0 10px;
  }
  table { width: 100%; border-collapse: collapse; }
  td { padding: 7px 0; border-bottom: 1px solid var(--line); }
  td:first-child { color: var(--muted); width: 45%; }
  td:last-child { text-align: right; font-variant-numeric: tabular-nums; }
  tr:last-child td { border-bottom: none; }
  footer {
    margin-top: 32px;
    padding-top: 14px;
    border-top: 1px solid var(--line);
    color: var(--muted);
    font-size: 11px;
    display: flex;
    justify-content: space-between;
    gap: 12px;
    flex-wrap: wrap;
  }
  .err { color: var(--warn); }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1><span class="dot" id="dot"></span>container-mesh relay</h1>
    <div class="meta"><span id="version">–</span> · up <span id="uptime">–</span></div>
  </header>

  <div class="grid">
    <div class="cell"><div class="k">active sessions</div><div class="v" id="sessions">–</div></div>
    <div class="cell"><div class="k">waiting peers</div><div class="v" id="waiting">–</div></div>
    <div class="cell"><div class="k">rooms</div><div class="v" id="rooms">–</div></div>
    <div class="cell"><div class="k">relayed</div><div class="v" id="bytes">–</div></div>
  </div>

  <section>
    <h2>traffic</h2>
    <table>
      <tr><td>messages forwarded</td><td id="msgs">–</td></tr>
      <tr><td>average message size</td><td id="avgsize">–</td></tr>
      <tr><td>throughput since start</td><td id="rate">–</td></tr>
    </table>
  </section>

  <section>
    <h2>process</h2>
    <table>
      <tr><td>goroutines</td><td id="goroutines">–</td></tr>
      <tr><td>room ceiling</td><td id="maxrooms">–</td></tr>
      <tr><td>per-message ceiling</td><td id="maxmsg">–</td></tr>
      <tr><td>connections per IP</td><td id="maxconns">–</td></tr>
      <tr><td>new connections per minute</td><td id="rateperm">–</td></tr>
    </table>
  </section>

  <footer>
    <span>polling /status every 2s</span>
    <span id="updated">–</span>
  </footer>
</div>

<script>
(function () {
  var $ = function (id) { return document.getElementById(id); };

  function bytes(n) {
    if (n < 1024) return n + " B";
    var u = ["KiB", "MiB", "GiB", "TiB"], i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
    return n.toFixed(n < 10 ? 2 : 1) + " " + u[i];
  }

  function duration(s) {
    var d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600),
        m = Math.floor(s % 3600 / 60), sec = s % 60;
    if (d > 0) return d + "d " + h + "h";
    if (h > 0) return h + "h " + m + "m";
    if (m > 0) return m + "m " + sec + "s";
    return sec + "s";
  }

  function num(n) { return n.toLocaleString(); }

  var failures = 0;

  function paint(s) {
    $("version").textContent = s.version;
    $("uptime").textContent = duration(s.uptime_seconds);
    $("sessions").textContent = num(s.active_sessions);
    $("waiting").textContent = num(s.waiting_peers);
    $("rooms").textContent = num(s.rooms) +
      (s.max_rooms ? "" : "");
    $("bytes").innerHTML = bytes(s.bytes_relayed);
    $("msgs").textContent = num(s.messages_relayed);
    $("avgsize").textContent = s.messages_relayed > 0
      ? bytes(Math.round(s.bytes_relayed / s.messages_relayed)) : "–";
    $("rate").textContent = s.uptime_seconds > 0
      ? bytes(Math.round(s.bytes_relayed / s.uptime_seconds)) + "/s" : "–";
    $("goroutines").textContent = num(s.goroutines);
    $("maxrooms").textContent = num(s.max_rooms);
    $("maxmsg").textContent = bytes(s.max_message_bytes);
    $("maxconns").textContent = num(s.max_conns_per_ip);
    $("rateperm").textContent = num(s.connect_rate_per_min);

    // The dot means "the relay answered recently", not "traffic is flowing".
    // A quiet relay is healthy; colouring it by session count would make an
    // idle server look broken.
    $("dot").className = "dot live";
    $("updated").textContent = "updated " + new Date().toLocaleTimeString();
    failures = 0;
  }

  function fail() {
    failures++;
    // One missed poll is a hiccup; three in a row is a real problem.
    $("dot").className = "dot " + (failures >= 3 ? "stale" : "live");
    $("updated").innerHTML = '<span class="err">no response from /status</span>';
  }

  function tick() {
    fetch("/status", { cache: "no-store" })
      .then(function (r) {
        if (!r.ok) throw new Error(r.status);
        return r.json();
      })
      .then(paint)
      .catch(fail);
  }

  tick();
  setInterval(tick, 2000);
})();
</script>
</body>
</html>
`
