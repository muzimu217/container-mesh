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
	"net/http"
	"sort"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

//go:embed panel.html
var panelHTML []byte

// PanelMember is one device's row in the panel payload.
type PanelMember struct {
	DeviceID  identity.DeviceID `json:"device_id"`
	Online    bool              `json:"online"`
	Endpoints []string          `json:"endpoints"`
	LastSeen  string            `json:"last_seen,omitempty"`
	IsAnchor  bool              `json:"is_anchor"`
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

// handlePanel serves the embedded single-file page. It is a static shell:
// no network state travels with it, so it needs no token.
func (s *Server) handlePanel(w http.ResponseWriter, r *http.Request) {
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
	_, _ = w.Write(panelHTML)
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
		AccountID:   members.AccountID,
		Version:     members.CurrentVersion(),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
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
		// LastSeenAt survives the device going offline, so an offline row
		// still says when it was last around (doubao-cloud-vm-01 round 56
		// noted the field was missing for offline members).
		if t, ok := disco.LastSeenAt(id); ok && !t.IsZero() {
			row.LastSeen = t.UTC().Format(time.RFC3339)
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
