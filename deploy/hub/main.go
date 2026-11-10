// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Command cmesh-hub relays agent coordination messages over plain HTTP:
// agents without GitHub credentials POST reports and GET instructions at a
// publicly reachable endpoint — the same outbound-only shape the mesh is
// built for. Transport only; the GitHub issue stays the canonical log and
// the coordinator mirrors between the two.
//
// Config: HUB_TOKEN (required, shared secret), HUB_DIR (state dir),
// HUB_ADDR (listen addr). State persists to one constant file state.json.
package main

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

var (
	hubDir    string
	hubToken  string
	hubAddr   string
	statePath string
)

func main() {
	hubToken = os.Getenv("HUB_TOKEN")
	if hubToken == "" {
		log.Fatal("HUB_TOKEN is required (shared secret: openssl rand -hex 32)")
	}
	hubDir = envOr("HUB_DIR", "/var/lib/cmesh-hub")
	hubAddr = envOr("HUB_ADDR", "127.0.0.1:8787")
	statePath = filepath.Join(hubDir, "state.json")
	if err := os.MkdirAll(hubDir, 0o700); err != nil {
		log.Fatal(err)
	}
	loadState()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/hub/", auth(handleHub))

	srv := &http.Server{
		Addr:              hubAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("cmesh-hub listening on %s (dir %s)", hubAddr, hubDir)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Hub-Token")
		if subtle.ConstantTimeCompare([]byte(tok), []byte(hubToken)) == 1 {
			next(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
