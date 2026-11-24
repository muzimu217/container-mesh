// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Agent-coordination message handlers and store: reports (per topic) and
// instructions (per agent inbox). All state lives in one in-memory store
// persisted to a single constant path (state.json) on every write — no
// variable paths, no database, capped per key.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
//
// ★ IT RETURNS ITS FAILURE. (Round 102.) The hub is the coordination
// channel's only record, and it used to swallow a failed save: it
// logged it and carried on, and the caller answered {"ok":true}.
//
// That is not a theoretical gap. On the authorized host an operator's
// cleanup script rewrote state.json as root while the service runs as
// a dedicated low-privilege user. From then on EVERY write failed with
// "permission denied" — and the hub kept answering {"ok":true} for two
// hours. The messages existed only in this process's memory; a restart
// lost all of them.
//
// "ok":true is not proof of delivery (the hub creates a phantom inbox
// for any "to=" value), and it is not proof of persistence either.
//
// ★ THE WRITE IS ATOMIC, THROUGH A TEMP FILE AND A RENAME.
//
// Writing the file in place keeps whatever ownership and mode the
// PREVIOUS file had — which is how a cleanup script running as root
// left a file the service could read but not write, and how that state
// survived every later write. Writing a fresh temp file and renaming
// it into place gives the new file the ownership and mode of the
// PROCESS THAT WROTE IT — the service user — every time.
func saveStateLocked() error {
	// An unset path is a programming error, not a transient failure:
	// without it the rename below would fail with a confusing "no such
	// file or directory" for the empty target.
	if statePath == "" {
		return fmt.Errorf("no state path is configured")
	}
	b, err := json.Marshal(store)
	if err != nil {
		log.Printf("state marshal failed: %v", err)
		return fmt.Errorf("marshal state: %w", err)
	}
	dir := filepath.Dir(statePath)
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		log.Printf("state save failed: %v", err)
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	// The rename must not leave the temp file behind on any failure.
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		log.Printf("state save failed: %v", err)
		return fmt.Errorf("chmod temp state: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		log.Printf("state save failed: %v", err)
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		log.Printf("state save failed: %v", err)
		return fmt.Errorf("sync temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		log.Printf("state save failed: %v", err)
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpName, statePath); err != nil {
		log.Printf("state save failed: %v", err)
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
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
		// ★ SAY WHICH FAILURE IT WAS. (Round 102, found by the audit
		// line.) The old message was "body too large or unreadable" —
		// ONE text for TWO failures, so an agent could not tell "your
		// report is over the limit, shrink it" from "the connection
		// broke, retry". Both write paths said it.
		//
		// The status code says it too: 413 means the entity was
		// understood and refused for being too big, which is exactly
		// what happened. 400 says the request was malformed, which it
		// was not.
		//
		// The limit is IN the message, because an agent has to be able
		// to compute the number it must stay under.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, fmt.Sprintf("body too large: the limit is %d bytes", maxBody),
				http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "body unreadable", http.StatusBadRequest)
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
	saveErr := saveStateLocked()
	mu.Unlock()
	// ★ A MESSAGE THAT WAS NOT STORED MUST NOT BE REPORTED AS STORED.
	// The old code answered {"ok":true} whether or not the save
	// worked. See saveStateLocked for what that cost on the host.
	if saveErr != nil {
		http.Error(w, "the message was accepted but could not be saved: "+saveErr.Error(),
			http.StatusInternalServerError)
		return
	}
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
		// ★ SAY WHICH FAILURE IT WAS. (Round 102, found by the audit
		// line.) The old message was "body too large or unreadable" —
		// ONE text for TWO failures, so an agent could not tell "your
		// report is over the limit, shrink it" from "the connection
		// broke, retry". Both write paths said it.
		//
		// The status code says it too: 413 means the entity was
		// understood and refused for being too big, which is exactly
		// what happened. 400 says the request was malformed, which it
		// was not.
		//
		// The limit is IN the message, because an agent has to be able
		// to compute the number it must stay under.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, fmt.Sprintf("body too large: the limit is %d bytes", maxBody),
				http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "body unreadable", http.StatusBadRequest)
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
	saveErr := saveStateLocked()
	mu.Unlock()
	if saveErr != nil {
		http.Error(w, "the instruction was accepted but could not be saved: "+saveErr.Error(),
			http.StatusInternalServerError)
		return
	}
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

// checkStateWritable reports whether the hub can persist its state.
//
// It is called once at startup, before the listener opens.
//
// ★ IT MUST NOT TOUCH THE REAL STATE FILE.
//
// The first version of this check renamed its probe file ONTO
// statePath — and that is how a CHECK destroyed the thing it was
// checking: every start of the hub replaced the whole channel history
// with the two bytes "{}". The bug was found because the state file
// came back as 159 bytes.
//
// What the check has to establish is narrow: "can this process write
// in the state DIRECTORY, and can it rename a file there". That is all
// the atomic save needs — it never writes INTO statePath, it always
// renames a fresh file over it. The ownership and mode of statePath
// itself are irrelevant to that, and are in fact repaired by the first
// successful save.
//
// So the probe writes and renames a file with its OWN name and removes
// it again. It never names statePath.
func checkStateWritable() error {
	if statePath == "" {
		return fmt.Errorf("no state path is configured")
	}
	dir := filepath.Dir(statePath)

	// If statePath is a DIRECTORY, the save cannot work: the atomic
	// save renames a file onto it, and a rename onto a directory fails.
	// This is checked without writing anything.
	if info, err := os.Stat(statePath); err == nil && info.IsDir() {
		return fmt.Errorf("%s is a directory, so the state cannot be written", statePath)
	}

	tmp, err := os.CreateTemp(dir, ".state-probe-*")
	if err != nil {
		return fmt.Errorf("cannot create a file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write([]byte("{}")); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot write in %s: %w", dir, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot close the probe file: %w", err)
	}
	// Rename to a name of its own — never onto statePath.
	probe := name + ".renamed"
	defer os.Remove(probe)
	if err := os.Rename(name, probe); err != nil {
		return fmt.Errorf("cannot rename a file in %s: %w", dir, err)
	}
	return nil
}
