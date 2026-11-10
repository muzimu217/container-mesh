// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Agent-coordination message handlers and store: reports (per topic) and
// instructions (per agent inbox). All state lives in one in-memory store
// persisted to a single constant path (state.json) on every write — no
// variable paths, no database, capped per key.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	mu       sync.Mutex
	store    hubStore
	safeName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	maxBody  = int64(256 << 10)
	maxMsgs  = 500 // per topic / per inbox
)

type message struct {
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Topic string `json:"topic,omitempty"`
	Time  string `json:"time"`
	Body  string `json:"body"`
}

type hubStore struct {
	Topics  map[string][]message `json:"topics"`
	Inboxes map[string][]message `json:"inboxes"`
}

// loadState restores the store from the single constant state path. A
// missing or corrupt file means a fresh start — the hub is transport, not
// a system of record.
func loadState() {
	store.Topics = map[string][]message{}
	store.Inboxes = map[string][]message{}
	b, err := os.ReadFile(statePath)
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, &store); err != nil {
		log.Printf("state.json unreadable; starting fresh: %v", err)
		store.Topics = map[string][]message{}
		store.Inboxes = map[string][]message{}
	}
}

// saveStateLocked persists the whole store. Called with mu held.
func saveStateLocked() {
	b, err := json.Marshal(store)
	if err != nil {
		log.Printf("state marshal failed: %v", err)
		return
	}
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		log.Printf("state save failed: %v", err)
	}
}

func keyOK(name string) bool {
	return safeName.MatchString(strings.TrimSpace(name))
}

func postMsg(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("topic"))
	if !keyOK(name) {
		http.Error(w, "invalid topic", http.StatusBadRequest)
		return
	}
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusBadRequest)
		return
	}
	msg := message{
		From:  from,
		Topic: name,
		Time:  time.Now().UTC().Format(time.RFC3339),
		Body:  string(body),
	}
	mu.Lock()
	store.Topics[name] = append(store.Topics[name], msg)
	if len(store.Topics[name]) > maxMsgs {
		store.Topics[name] = store.Topics[name][len(store.Topics[name])-maxMsgs:]
	}
	saveStateLocked()
	mu.Unlock()
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func getMsgs(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("topic"))
	if !keyOK(name) {
		http.Error(w, "invalid topic", http.StatusBadRequest)
		return
	}
	mu.Lock()
	msgs := store.Topics[name]
	mu.Unlock()
	_ = msgs
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"total": len(msgs), "messages": msgs})
}

func getInbox(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("to"))
	if !keyOK(name) {
		http.Error(w, "invalid agent id", http.StatusBadRequest)
		return
	}
	mu.Lock()
	msgs := store.Inboxes[name]
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"total": len(msgs), "messages": msgs})
}

func postInstruct(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("to"))
	if !keyOK(name) {
		http.Error(w, "invalid agent id", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusBadRequest)
		return
	}
	msg := message{
		To:   name,
		Time: time.Now().UTC().Format(time.RFC3339),
		Body: string(body),
	}
	mu.Lock()
	store.Inboxes[name] = append(store.Inboxes[name], msg)
	if len(store.Inboxes[name]) > maxMsgs {
		store.Inboxes[name] = store.Inboxes[name][len(store.Inboxes[name])-maxMsgs:]
	}
	saveStateLocked()
	mu.Unlock()
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleHub dispatches the four /hub/* operations to their handlers.
func handleHub(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/hub/report":
		postMsg(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/hub/reports":
		getMsgs(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/hub/instruct":
		postInstruct(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/hub/inbox":
		getInbox(w, r)
	default:
		http.Error(w, "unknown endpoint", http.StatusNotFound)
	}
}
