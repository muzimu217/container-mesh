// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

// Tests for the cockpit: the accepted prototype, built and shipped inside
// the binary.
//
// Two things are pinned here: the built app must reference NOTHING outside
// the binary (it has to render inside a container with no network), and the
// roster document must carry the fields the cockpit reads.

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestCockpitAssetsAreLocal: the built app may only reference its OWN
// assets. A CDN link or a web font would make the cockpit fail exactly
// where it is most needed — an isolated container.
func TestCockpitAssetsAreLocal(t *testing.T) {
	entries, err := fs.ReadDir(cockpitFS, ".")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == "index.html" {
			found = true
		}
	}
	if !found {
		t.Fatal("the embedded cockpit has no index.html")
	}
	index, err := fs.ReadFile(cockpitFS, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	// Every resource reference must be relative.
	refRe := regexp.MustCompile(`(?:src|href)\s*=\s*"([^"]+)"`)
	for _, m := range refRe.FindAllStringSubmatch(string(index), -1) {
		ref := m[1]
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") ||
			strings.HasPrefix(ref, "//") {
			t.Errorf("the cockpit references an external resource: %s", ref)
		}
		if !strings.HasPrefix(ref, "./") && !strings.HasPrefix(ref, "/") {
			t.Errorf("the cockpit references a relative path that is not rooted: %s", ref)
		}
	}
}

// TestCockpitFetchesOnlyOurEndpoint: the app talks to this origin and
// nothing else.
func TestCockpitFetchesOnlyOurEndpoint(t *testing.T) {
	assets, err := fs.ReadDir(cockpitFS, "assets")
	if err != nil {
		t.Fatal(err)
	}
	sawAPI := false
	for _, a := range assets {
		if !strings.HasSuffix(a.Name(), ".js") {
			continue
		}
		bundle, err := fs.ReadFile(cockpitFS, "assets/"+a.Name())
		if err != nil {
			t.Fatal(err)
		}
		text := string(bundle)
		if strings.Contains(text, "/api/mesh") {
			sawAPI = true
		}
		// A fetch to an absolute origin would phone home.
		if regexp.MustCompile(`fetch\(\s*["']https?://`).MatchString(text) {
			t.Errorf("the cockpit fetches from an external origin: %s", a.Name())
		}
	}
	if !sawAPI {
		t.Error("the cockpit bundle does not reference /api/mesh — it may not be wired to the roster")
	}
}

// TestCockpitPayloadCarriesEveryFieldItReads: the page reads these
// member fields; the API must supply them.
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
	for _, k := range []string{
		"account_id", "version", "roster_version", "generated_at", "network_name",
		"discovery_status", "is_cached", "last_sync_at", "retry_in_seconds",
		"summary", "zones", "members",
	} {
		if _, ok := raw[k]; !ok {
			t.Errorf("the payload has no %q", k)
		}
	}
	zones, _ := raw["zones"].([]any)
	if len(zones) != 1 {
		t.Fatalf("the payload has %d zones, want 1 (this deployment is one place)", len(zones))
	}
	zone, _ := zones[0].(map[string]any)
	for _, k := range []string{
		"zone_id", "zone_name", "zone_code", "environment_type", "location_label",
		"geo_lat", "geo_lng", "geo_coord_label", "description", "position_3d",
	} {
		if _, ok := zone[k]; !ok {
			t.Errorf("a zone has no %q", k)
		}
	}
	list, _ := raw["members"].([]any)
	if len(list) != 1 {
		t.Fatalf("the payload has %d members, want 1", len(list))
	}
	row, _ := list[0].(map[string]any)
	for _, k := range []string{
		"device_id", "display_name", "zone_id", "zone_name", "geo_lat", "geo_lng",
		"geo_city", "geo_coord_label", "role", "is_anchor", "is_local",
		"container_archetype", "has_public_inbound", "nat_type_label", "in_roster",
		"online", "endpoints", "last_seen", "transport", "peers_connected",
		"active_comm_summary", "diagnostic_reason", "diagnostic_suggestion",
		"container_config",
	} {
		if _, ok := row[k]; !ok {
			t.Errorf("a member row has no %q", k)
		}
	}
	if row["transport"] != "direct" && row["transport"] != "relay" &&
		row["transport"] != "pending" && row["transport"] != "unavailable" {
		t.Errorf("transport = %v, want one of direct/relay/pending/unavailable", row["transport"])
	}
	if row["geo_city"] == "" {
		t.Error("geo_city is empty; the cockpit shows it")
	}
}

// TestCockpitDoesNotInventGeography: cmesh does not collect locations, so
// the payload must say "not registered" rather than name a city.
func TestCockpitDoesNotInventGeography(t *testing.T) {
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

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/mesh", nil)
	req.Header.Set("X-Panel-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw struct {
		Zones []struct {
			GeoCoordLabel string `json:"geo_coord_label"`
		} `json:"zones"`
		Members []struct {
			GeoCity string `json:"geo_city"`
		} `json:"members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Zones) == 0 || !strings.Contains(raw.Zones[0].GeoCoordLabel, "未登记") {
		t.Errorf("the zone does not say the location is unregistered: %+v", raw.Zones)
	}
	if len(raw.Members) == 0 || !strings.Contains(raw.Members[0].GeoCity, "未登记") {
		t.Errorf("a member does not say its location is unregistered: %+v", raw.Members)
	}
}

// TestCockpitIsServedAtPanel: the built app is what /panel serves.
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
	body := make([]byte, 2048)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "<div id=") && !strings.Contains(string(body[:n]), "<script") {
		t.Errorf("GET /panel does not look like the built app: %.120s", body[:n])
	}
}

// TestCockpitAssetsLoadThroughTheMount: the page is served at /panel and its
// assets are requested relative to it (/panel/assets/*). net/http's mux
// treats "/panel" as an exact pattern, so without an explicit "/panel/"
// route those asset requests fell through to the catch-all and 404'd — the
// shell loaded and the app rendered nothing. A 200 on the page alone does
// not prove the cockpit works; every reference it makes has to resolve.
func TestCockpitAssetsLoadThroughTheMount(t *testing.T) {
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
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?:src|href)\s*=\s*"([^"]+)"`).FindAllStringSubmatch(string(page), -1)
	if len(refs) == 0 {
		t.Fatal("the served page references no assets")
	}
	for _, m := range refs {
		ref := m[1]
		if strings.HasPrefix(ref, "http") || strings.HasPrefix(ref, "//") {
			t.Errorf("external reference in the served page: %s", ref)
			continue
		}
		// Resolve relative to /panel, the way a browser does.
		u := ts.URL + "/panel/" + strings.TrimPrefix(ref, "./")
		ar, err := http.Get(u)
		if err != nil {
			t.Errorf("GET %s: %v", u, err)
			continue
		}
		_ = ar.Body.Close()
		if ar.StatusCode != http.StatusOK {
			t.Errorf("the page references %s but the mount serves it as %d", ref, ar.StatusCode)
		}
		if strings.HasSuffix(ref, ".js") && ar.Header.Get("Content-Type") == "" {
			t.Errorf("%s was served with no Content-Type", ref)
		}
	}
}

// TestCockpitHasNoDemoControls: the prototype's demo scenarios and its
// single-node "simulate" buttons are gone. Those buttons mutated the
// in-memory roster to fake a device's liveness or endpoints — still visible
// and clickable after the scenario switcher was removed
// (doubao-cloud-vm-01, round 58). A production cockpit must not be able to
// paint a device as something it is not.
//
// The check runs against the SHIPPED bundle, not the old single-file page:
// that page is no longer served by any route, and a blacklist that scans a
// file nothing serves proves nothing about the binary.
func TestCockpitHasNoDemoControls(t *testing.T) {
	assets, err := fs.ReadDir(cockpitFS, "assets")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, a := range assets {
		if !strings.HasSuffix(a.Name(), ".js") {
			continue
		}
		bundle, err := fs.ReadFile(cockpitFS, "assets/"+a.Name())
		if err != nil {
			t.Fatal(err)
		}
		text := string(bundle)
		scanned++
		for _, bad := range []string{
			"BASE_SCENARIOS",
			"PROTOTYPE STATE SWITCHER",
			"btnSimToggleOnline",
			"btnSimToggleEndpoint",
			"SimToggle",
			"_savedEndpoints",
			"getMeshDataByScenario",
			// The prototype also shipped a DEFAULT-ON toggle that appended
			// two synthetic Hong Kong devices to the live roster, so every
			// operator opening the panel saw two machines that do not exist.
			// Blacklisting button ids alone missed it (doubao-cloud-vm-01,
			// round 58, second pass): the fabricated DEVICES are the defect.
			"simulateHkMultiNodes",
			"EXTRA_HK_CLUSTER_NODES",
			"dev_06_hk_infer",
			"dev_07_hk_sandbox",
			"hk-infer-blade-02",
			"香港3机智能聚合",
		} {
			if strings.Contains(text, bad) {
				t.Errorf("the shipped cockpit bundle still contains prototype-only machinery (%q) in %s", bad, a.Name())
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no JavaScript bundle was found to scan — the cockpit build is missing")
	}
}

// TestCockpitExposesItsCanvasLabelsAsText: the 3D view draws ANCHOR CORE and
// the offline dock with canvas fillText, so those statements exist only as
// pixels — a screen reader and a headless check both see an empty element.
// The mirror carries the same facts as text (doubao-cloud-vm-01).
func TestCockpitExposesItsCanvasLabelsAsText(t *testing.T) {
	assets, err := fs.ReadDir(cockpitFS, "assets")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range assets {
		if !strings.HasSuffix(a.Name(), ".js") {
			continue
		}
		bundle, err := fs.ReadFile(cockpitFS, "assets/"+a.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(bundle), "cockpit-canvas-mirror") {
			found = true
		}
	}
	if !found {
		t.Error("the cockpit has no text mirror for what the canvas draws — the panel cannot be verified without a screenshot")
	}
}
