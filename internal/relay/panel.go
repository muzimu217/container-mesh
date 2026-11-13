// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

// The visualization panel's data surface.
//
// The panel is the product's face: "how many of my devices are online right
// now". Two rules shape this file:
//
//   - The page itself carries NO data. GET /panel serves a static shell
//     (the HTML is embedded in the binary); the numbers come from
//     GET /api/mesh, which is protected by a token. An unauthenticated
//     fetch of the shell reveals nothing about the network.
//   - The roster is never exposed to a non-member. R-2.4 says the discovery
//     layer must not let outsiders enumerate the network, and the panel is
//     the easiest way to break that rule by accident.

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

//go:embed panel.html
var panelHTML []byte

//go:embed assets/cockpit.html
var cockpitHTML []byte

// PanelMember is one device's row in the panel payload.
//
// The field set is a superset of two consumers:
//   - the lightweight panel page (device_id, online, endpoints, last_seen,
//     is_anchor) and the API contract documented in the product brief;
//   - the accepted cockpit prototype, whose view model adds a display name,
//     a role label, whether the row is the local device, the transport it
//     is reachable over, and a pre-rendered relative time.
//
// The prototype's names are emitted verbatim, so its rendering code works
// against the live payload unchanged — that was the point of aligning them
// here rather than translating in the browser.
type PanelMember struct {
	DeviceID  identity.DeviceID `json:"device_id"`
	Online    bool              `json:"online"`
	Endpoints []string          `json:"endpoints"`
	// LastSeen is always present: a consumer must not have to guess
	// whether an absent key means "never" or "not implemented". It is
	// empty for a device that has never announced.
	LastSeen string `json:"last_seen"`
	IsAnchor bool   `json:"is_anchor"`

	// Name is the display name of the device. The mesh has no naming
	// service yet, so the device ID is the name.
	Name string `json:"name"`
	// RoleLabel describes what the device is in this network.
	RoleLabel string `json:"role_label"`
	// IsLocal marks the device running this view (the anchor itself).
	IsLocal bool `json:"is_local"`
	// Transport is how the device is reachable: "direct" when it announced
	// endpoints, "relay" when it has none and must be reached through
	// another member.
	Transport string `json:"transport"`
	// EnvNote is one short human sentence about the device. The cockpit
	// shows it next to the selected node; it must describe what we actually
	// know, never a plausible-sounding guess.
	EnvNote string `json:"env_note"`
	// LastSeenRel is the relative form of LastSeen, pre-rendered so every
	// consumer phrases it the same way.
	LastSeenRel string `json:"last_seen_rel"`
}

// PanelSummary carries the two numbers the product leads with.
type PanelSummary struct {
	Members int `json:"members"`
	Online  int `json:"online"`
}

// PanelData is the whole payload of GET /api/mesh.
type PanelData struct {
	AccountID   identity.DeviceID `json:"account_id"`
	Version     uint64            `json:"version"`
	GeneratedAt string            `json:"generated_at"`
	Summary     PanelSummary      `json:"summary"`
	Members     []PanelMember     `json:"members"`

	// RosterVersion is the same counter as Version under the name the
	// accepted cockpit prototype uses.
	RosterVersion uint64 `json:"roster_version"`
	// NetworkName labels the network in the cockpit. The mesh has no
	// naming service yet, so the account ID is the name.
	NetworkName string `json:"network_name"`
}

// SetPanelToken enables the panel's data surface at runtime.
//
// The relay mounts /panel and /api/mesh when discovery is enabled, but the
// data endpoint stays closed until a token is set: a node that was not
// asked for a panel must not leak its roster to whoever finds the port.
func (s *Server) SetPanelToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panelToken = token
}

// SetAnchorDeviceID marks the discovery node's own row in the panel.
func (s *Server) SetAnchorDeviceID(id identity.DeviceID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anchorDeviceID = id
}

// relativeTime renders a timestamp the way the cockpit shows it.
func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 10*time.Second:
		return "刚刚"
	case d < time.Minute:
		return fmt.Sprintf("%d 秒前", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	}
}

// handlePanel serves the cockpit: the accepted prototype, wired to the live
// API and embedded in the binary.
//
// The page is the product's face and its design was approved as it stands,
// so it is shipped as the accepted design rather than re-implemented: the
// only changes to the prototype are (1) the demo scenarios and their
// switcher are gone, (2) the data comes from GET /api/mesh with the panel
// token in the header, and (3) a token gate asks for the token once.
//
// The lighter single-file page remains available at /panel-lite for
// environments that want the smallest possible surface.
func (s *Server) handlePanel(w http.ResponseWriter, r *http.Request) {
	s.serveEmbedded(w, r, cockpitHTML)
}

// handlePanelLite serves the lightweight single-file page.
func (s *Server) handlePanelLite(w http.ResponseWriter, r *http.Request) {
	s.serveEmbedded(w, r, panelHTML)
}

// serveEmbedded writes an embedded page with the headers every panel
// needs: no-store (the page embeds a token gate, not a snapshot), a strict
// CSP (no external anything), and a content type.
func (s *Server) serveEmbedded(w http.ResponseWriter, r *http.Request, body []byte) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The shell is embedded in the binary; it must never be cached stale
	// next to a newer one, but it also never contains data.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// handleAPIMesh serves the live roster.
//
// Authentication: the panel token, as a header (X-Panel-Token) or a query
// parameter for the browser's first request. The comparison is constant
// time so a wrong token cannot be probed byte by byte.
func (s *Server) handleAPIMesh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if !s.panelAuthorized(r) {
		// 401 with a hint: the operator needs to know a token is missing,
		// not that the endpoint is forbidden.
		w.Header().Set("WWW-Authenticate", `Bearer realm="cmesh panel"`)
		http.Error(w, "panel token required", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	members := s.discoMembers
	disco := s.disco
	anchorID := s.anchorDeviceID
	s.mu.Unlock()
	if members == nil || disco == nil {
		http.Error(w, "discovery is not enabled on this node", http.StatusNotFound)
		return
	}

	data := PanelData{
		AccountID:     members.AccountID,
		Version:       members.CurrentVersion(),
		RosterVersion: members.CurrentVersion(),
		NetworkName:   string(members.AccountID),
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	online := map[identity.DeviceID]bool{}
	endpoints := map[identity.DeviceID][]string{}
	for _, m := range disco.Snapshot() {
		online[m.DeviceID] = true
		endpoints[m.DeviceID] = m.Endpoints
	}
	ids := members.IDs()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if _, ok := members.Get(id); !ok {
			continue // the list changed under us; the next poll is correct
		}
		row := PanelMember{
			DeviceID:  id,
			Online:    online[id],
			Endpoints: endpoints[id],
			IsAnchor:  id == anchorID,
		}
		if row.Endpoints == nil {
			row.Endpoints = []string{}
		}
		// The display name is the device ID: the mesh has no naming
		// service yet, and a made-up name would be a fiction.
		row.Name = id.String()
		switch {
		case row.IsAnchor:
			row.RoleLabel = "自托管锚点 / 发现节点"
		case row.Online:
			row.RoleLabel = "网络成员"
		default:
			row.RoleLabel = "网络成员（当前离线，仍在名册）"
		}
		// The anchor is the device running this view.
		row.IsLocal = row.IsAnchor
		// A device that announced endpoints is reachable directly; one that
		// announced none has to be reached through another member.
		if len(row.Endpoints) > 0 {
			row.Transport = "direct"
		} else {
			row.Transport = "relay"
		}
		// LastSeenAt survives the detach, so an offline member still says
		// when it was last around.
		if t, ok := disco.LastSeenAt(id); ok && !t.IsZero() {
			row.LastSeen = t.UTC().Format(time.RFC3339)
			row.LastSeenRel = relativeTime(t)
		} else {
			row.LastSeenRel = "未知"
		}
		// One honest sentence about the device — the cockpit shows it
		// next to the selected node.
		switch {
		case row.IsAnchor:
			row.EnvNote = "自托管锚点 · 本机发现节点"
		case !row.Online:
			row.EnvNote = "当前离线 · 仍保留名册席位（最后在线 " + row.LastSeenRel + "）"
		case row.Transport == "relay":
			row.EnvNote = "无直连端点 · 经其他成员中转可达"
		default:
			row.EnvNote = fmt.Sprintf("直连可达 · 已公告 %d 个端点", len(row.Endpoints))
		}
		data.Members = append(data.Members, row)
		if row.Online {
			data.Summary.Online++
		}
	}
	data.Summary.Members = len(data.Members)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(data)
}

// panelAuthorized reports whether the request carries the panel token.
//
// The token is accepted in the HEADER only. A query parameter would put it
// into the node's access log and into any Referer the browser sends — and
// the panel's own page keeps it in localStorage and sends the header, so
// nothing legitimate needs the query form (oh-no-agent round 56 flagged the
// mismatch with the documented "the token never travels in a URL").
func (s *Server) panelAuthorized(r *http.Request) bool {
	s.mu.Lock()
	want := s.panelToken
	s.mu.Unlock()
	if want == "" {
		// No token configured: the panel data surface stays closed. A
		// node that was not explicitly given a token must not leak the
		// roster to anyone who finds the port.
		return false
	}
	got := r.Header.Get("X-Panel-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
