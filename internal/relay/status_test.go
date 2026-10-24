// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestRelay builds a relay with a short pair timeout so tests do not wait.
func newTestRelay(t *testing.T) *Server {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.PairTimeout = 2 * time.Second
	return New(cfg)
}

// TestStatusSnapshotShape checks the JSON contract the dashboard depends on.
func TestStatusSnapshotShape(t *testing.T) {
	s := newTestRelay(t)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q; a stale dashboard is worse than none", cc)
	}

	var got Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MaxRooms <= 0 || got.MaxMessageBytes <= 0 {
		t.Fatalf("limits missing from the snapshot: %+v", got)
	}
	if got.Goroutines <= 0 {
		t.Fatalf("goroutine count = %d", got.Goroutines)
	}
}

// TestStatusLeaksNothing is the security property: the dashboard must not
// become a way to enumerate room labels, which are derived from the code and
// therefore a verifier an attacker could attack offline.
func TestStatusLeaksNothing(t *testing.T) {
	s := newTestRelay(t)

	// Park a peer in a room with a distinctive label.
	const label = "deadbeefdeadbeefdeadbeefdeadbeef"
	room := &room{id: label, roles: map[string]*waiter{"A": {}}}
	s.mu.Lock()
	s.rooms[label] = room
	s.mu.Unlock()

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	body := rec.Body.String()

	if strings.Contains(body, label) {
		t.Fatal("status response contains a room label")
	}
	// Field names like "rooms" and "max_rooms" are fine; what must never appear
	// is an identifier for a specific peer or room. Room labels are 32 hex
	// characters, so any run that long is a leak regardless of which field it
	// arrived in.
	for _, tok := range strings.FieldsFunc(body, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
	}) {
		if len(tok) >= 16 {
			t.Fatalf("status response contains a long hex run (%q), which looks like a room label", tok)
		}
	}
	// No peer addresses either: the relay knows them, but publishing them turns
	// a status page into a target list.
	for _, field := range []string{"remote", "127.0.0.1", "client_ip", "\"addr\""} {
		if strings.Contains(body, field) {
			t.Fatalf("status response contains %q", field)
		}
	}
	// The counts themselves are fine and expected.
	var got Status
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.WaitingPeers != 1 || got.Rooms != 1 {
		t.Fatalf("counts = %+v, want one waiting peer in one room", got)
	}
}

// TestStatusCountsSessionsSeparatelyFromRooms is the distinction that made the
// counters necessary: len(rooms) is not a session count, because a room entry
// is deleted the moment its two peers pair.
func TestStatusCountsSessionsSeparatelyFromRooms(t *testing.T) {
	s := newTestRelay(t)
	s.activeSessions.Add(2)

	got := s.StatusSnapshot()
	if got.ActiveSessions != 2 {
		t.Fatalf("active sessions = %d, want 2", got.ActiveSessions)
	}
	if got.Rooms != 0 {
		t.Fatalf("rooms = %d, want 0 (a paired room leaves the map)", got.Rooms)
	}
}

// TestStatusAccumulatesTraffic checks the byte and message counters.
func TestStatusAccumulatesTraffic(t *testing.T) {
	s := newTestRelay(t)
	s.bytesRelayed.Add(4096)
	s.messagesRelayed.Add(4)

	got := s.StatusSnapshot()
	if got.BytesRelayed != 4096 || got.MessagesRelayed != 4 {
		t.Fatalf("counters = %d bytes / %d messages", got.BytesRelayed, got.MessagesRelayed)
	}
}

// TestDashboardServesSelfContainedPage proves the page needs nothing external:
// no CDN, no font host, no analytics. A monitoring page that phones out is a
// leak in an otherwise self-contained tool.
func TestDashboardServesSelfContainedPage(t *testing.T) {
	s := newTestRelay(t)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "/status") {
		t.Fatal("page does not poll /status")
	}
	for _, external := range []string{"http://", "https://", "//cdn", "fonts.googleapis"} {
		if strings.Contains(body, external) {
			t.Fatalf("page references an external resource: %q", external)
		}
	}
	// It must be dark/light aware rather than hard-coding one scheme.
	if !strings.Contains(body, "prefers-color-scheme") {
		t.Fatal("page does not adapt to the colour scheme")
	}
}

// TestDashboardDotMeansResponsiveNotBusy guards the semantics of the status
// dot: a quiet relay is healthy, so the indicator must reflect "the relay
// answered", never "traffic is flowing".
func TestDashboardDotMeansResponsiveNotBusy(t *testing.T) {
	s := newTestRelay(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `"dot live"`) {
		t.Fatal("page does not set the live state after a successful poll")
	}
	if strings.Contains(body, "active_sessions > 0") {
		t.Fatal("the dot appears to depend on session count; an idle relay would look broken")
	}
}

// TestRootPagePointsAtTheDashboard keeps the endpoints discoverable without
// reading the source.
func TestRootPagePointsAtTheDashboard(t *testing.T) {
	s := newTestRelay(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	for _, want := range []string{"/status", "/dashboard", "/ws"} {
		if !strings.Contains(body, want) {
			t.Fatalf("root page does not mention %q: %q", want, body)
		}
	}
}

// TestUnknownPathIsNotFound keeps the relay from answering 200 to anything.
func TestUnknownPathIsNotFound(t *testing.T) {
	s := newTestRelay(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestSetVersionIsReported proves the CLI's version reaches the dashboard.
func TestSetVersionIsReported(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })

	SetVersion("v9.9.9-test")
	s := newTestRelay(t)
	if got := s.StatusSnapshot().Version; got != "v9.9.9-test" {
		t.Fatalf("version = %q", got)
	}
}
