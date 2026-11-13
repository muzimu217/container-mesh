// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

// Tests for the cockpit: the accepted prototype, shipped as the product's
// own interface.
//
// The prototype was written to the GET /api/mesh contract and carried a
// demo mode; the product version keeps the design and drops the demo. These
// tests pin both halves of that: the page must not reference anything
// outside the binary, and the payload must carry every field the page
// reads.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestCockpitHasNoExternalResources: the page is embedded in the binary and
// must render fully offline — no CDN, no web font, no script from
// elsewhere.
func TestCockpitHasNoExternalResources(t *testing.T) {
	page := string(cockpitHTML)
	for _, bad := range []string{"http://", "https://", "//cdn.", "<script src=", "<link rel=\"stylesheet\""} {
		if strings.Contains(page, bad) {
			t.Errorf("the cockpit references an external resource (%q); it must render offline", bad)
		}
	}
	if len(cockpitHTML) < 50_000 {
		t.Errorf("the cockpit asset is %d bytes — that is too small to be the accepted design", len(cockpitHTML))
	}
}

// TestCockpitHasNoDemoScenarios: the prototype's mock states and their
// switcher are gone. A production cockpit that can paint fiction is a
// cockpit that lies.
func TestCockpitHasNoDemoScenarios(t *testing.T) {
	page := string(cockpitHTML)
	for _, bad := range []string{"BASE_SCENARIOS", "PROTOTYPE STATE SWITCHER", "data-scenario="} {
		if strings.Contains(page, bad) {
			t.Errorf("the cockpit still contains prototype-only content (%q)", bad)
		}
	}
	// …and it does talk to the live endpoint, with the panel token in the
	// header rather than the query string.
	for _, want := range []string{"/api/mesh", "X-Panel-Token", "tokenGate"} {
		if !strings.Contains(page, want) {
			t.Errorf("the cockpit does not reference %q", want)
		}
	}
}

// TestCockpitPayloadCarriesEveryFieldItReads: the page reads these member
// fields; the API must supply them.
func TestCockpitPayloadCarriesEveryFieldItReads(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{PanelToken: "tok", AnchorDeviceID: dev.ID()})
	srv.EnableDiscovery(members)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.StopDiscovery()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/mesh", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Panel-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	// Top level.
	for _, k := range []string{"account_id", "version", "roster_version", "generated_at", "network_name", "summary", "members"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("the payload has no %q", k)
		}
	}
	list, _ := raw["members"].([]any)
	if len(list) == 0 {
		t.Fatal("the payload has no members")
	}
	row, _ := list[0].(map[string]any)
	for _, k := range []string{
		"device_id", "name", "role_label", "is_anchor", "is_local",
		"online", "endpoints", "last_seen", "last_seen_rel", "transport", "env_note",
	} {
		if _, ok := row[k]; !ok {
			t.Errorf("a member row has no %q", k)
		}
	}
	// The derived fields must say something true.
	if row["transport"] != "direct" && row["transport"] != "relay" {
		t.Errorf("transport = %v, want direct or relay", row["transport"])
	}
	if row["env_note"] == "" {
		t.Error("env_note is empty; the cockpit shows it next to the selected node")
	}
	if row["last_seen_rel"] == "" {
		t.Error("last_seen_rel is empty")
	}
}

// TestCockpitIsServedAtPanel: the cockpit is the product's face, so it is
// what /panel serves; the lighter page stays available at /panel-lite.
func TestCockpitIsServedAtPanel(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	srv := New(Config{PanelToken: "tok"})
	srv.EnableDiscovery(members)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.StopDiscovery()

	resp, err := http.Get(ts.URL + "/panel")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /panel = %d, want 200", resp.StatusCode)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "COCKPIT") && !strings.Contains(string(body[:n]), "cockpit") &&
		!strings.Contains(string(body[:n]), "container-mesh") {
		t.Errorf("GET /panel does not look like the cockpit: %.120s", body[:n])
	}

	lite, err := http.Get(ts.URL + "/panel-lite")
	if err != nil {
		t.Fatal(err)
	}
	defer lite.Body.Close()
	if lite.StatusCode != http.StatusOK {
		t.Fatalf("GET /panel-lite = %d, want 200", lite.StatusCode)
	}
}
