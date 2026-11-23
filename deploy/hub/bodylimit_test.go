package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resetStore gives each test a clean in-memory store. The hub's real
// one is loaded from disk by loadState(); the tests here must not touch
// the state file, so they set the maps directly.
func resetStore() {
	mu.Lock()
	defer mu.Unlock()
	store.Topics = map[string][]message{}
	store.Inboxes = map[string][]message{}
}

// The hub is the coordination channel: an agent posts a report and the
// hub stores it. Two things about that path were wrong, and both of
// them made a failure LOOK like a success.
//
//  1. "body too large or unreadable" — ONE message for TWO different
//     failures. An agent whose report was rejected because it was over
//     the limit got the same text as an agent whose connection broke,
//     so there was no way to tell "shrink your report" from "retry".
//
//  2. The limit was 256 KiB while the nginx in front of the hub
//     allowed 300k. So the band between the two (256 KiB .. 300 KiB)
//     produced the hub's 400, and above 300 KiB produced nginx's 413
//     — an HTML page, not JSON. An agent following the documented curl
//     line (which has no -f) saw exit code 0 in BOTH cases.
//
// The fix: say which limit was hit, and use a status code that means
// "too large" rather than "unreadable".

// The hub must distinguish "too large" from "unreadable".
func TestTheHubSaysWhichLimitWasHit(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantIn     string
	}{
		{
			name:       "a body over the limit says too large, with the number",
			body:       strings.Repeat("a", int(maxBody)+1),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantIn:     "too large",
		},
		{
			name:       "a body under the limit is accepted",
			body:       strings.Repeat("a", 100),
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetStore()
			r := httptest.NewRequest("POST", "/hub/report?from=t&topic=t", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			postMsg(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %q)", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantIn != "" && !strings.Contains(w.Body.String(), tc.wantIn) {
				t.Fatalf("body = %q, want it to mention %q", w.Body.String(), tc.wantIn)
			}
		})
	}
}

// The same must hold on the instruct path — it is the other write
// endpoint, and it had the identical conflated message.
func TestTheInstructPathSaysWhichLimitWasHit(t *testing.T) {
	resetStore()
	r := httptest.NewRequest("POST", "/hub/instruct?to=somebody", strings.NewReader(strings.Repeat("a", int(maxBody)+1)))
	w := httptest.NewRecorder()
	postInstruct(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d (body: %q)", w.Code, http.StatusRequestEntityTooLarge, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "too large") {
		t.Fatalf("body = %q, want it to mention \"too large\"", w.Body.String())
	}
}

// The limit must be discoverable, not folklore: an agent that reads
// the error has to be able to compute the number it must stay under.
func TestTheLimitIsStatedInTheError(t *testing.T) {
	resetStore()
	r := httptest.NewRequest("POST", "/hub/report?from=t&topic=t", strings.NewReader(strings.Repeat("a", int(maxBody)+1)))
	w := httptest.NewRecorder()
	postMsg(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "262144") && !strings.Contains(body, "256") {
		t.Fatalf("body = %q, want it to state the limit", body)
	}
}
