// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

// The cockpit's compiled assets.
//
// The accepted prototype is a React + Vite + Three.js application. It is
// built ahead of time (web/) and embedded here, so the product ships ONE
// binary that serves the interface on the machine that runs the anchor —
// no npm, no CDN, nothing to fetch at runtime.
//
// The build output is committed (web/dist is copied into cockpit-dist)
// because the Go binary embeds it: a build that needs the network or a
// toolchain would make the release unreproducible.

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:cockpit-dist
var cockpitDist embed.FS

// cockpitFS is the built app rooted at its index.
var cockpitFS = func() fs.FS {
	sub, err := fs.Sub(cockpitDist, "cockpit-dist")
	if err != nil {
		// The embed is compile-time; this can only fail if the directory is
		// missing, which the build already prevents.
		panic("relay: cockpit assets missing: " + err.Error())
	}
	return sub
}()

// handleCockpit serves the built application, mounted at /panel and
// /panel/ (the app's assets are requested relative to the page, so they
// arrive as /panel/assets/*).
//
// "/panel" without a trailing slash is REDIRECTED to "/panel/", and that is
// not cosmetic: the page references its assets relatively (./assets/x.js).
// For a document at "/panel" the base is "/", so a browser resolves that to
// "/assets/x.js" — which this binary does not serve. The shell loaded, both
// assets 404'd, and the panel rendered a blank page. The startup banner
// prints exactly "/panel", so this was the path every operator took
// (doubao-cloud-vm-01, round 59).
//
// Unknown paths fall back to index.html, because the app is a single page
// with its own routing.
func (s *Server) handleCockpit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/panel" {
		// 308 keeps the method and the page is an idempotent GET — but it is
		// also cached by browsers by default. Cache it and a future revert
		// (or another change of the mount) cannot be taken back cleanly, and
		// a tester disabling the redirect would read stale entries as if it
		// were still on. One header keeps the redirect uncacheable
		// (doubao-cloud-vm-01, round 60).
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/panel/", http.StatusPermanentRedirect)
		return
	}
	p := path.Clean(r.URL.Path)
	p = strings.TrimPrefix(p, "/panel")
	name := strings.TrimPrefix(p, "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	data, err := fs.ReadFile(cockpitFS, name)
	if err != nil {
		// Single-page app: unknown routes are the app's own.
		data, err = fs.ReadFile(cockpitFS, "index.html")
		if err != nil {
			http.Error(w, "cockpit assets missing", http.StatusInternalServerError)
			return
		}
		name = "index.html"
	}
	switch path.Ext(name) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	case ".json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	// The app fetches its own data from this origin; nothing else is
	// allowed to load, and the token never travels in a URL.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; font-src 'self' data:")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
