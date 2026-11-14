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
	"net/url"
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

// TestCockpitBundleHasNoBrowserProcessPrimitives: the cockpit is browser code
// and must never contain a Node/Python process-execution primitive. A plain
// `RegExp.exec()` is legal JavaScript library code and is deliberately not
// blacklisted here; this guard targets actual process APIs and module loaders.
func TestCockpitBundleHasNoBrowserProcessPrimitives(t *testing.T) {
	assets, err := fs.ReadDir(cockpitFS, "assets")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range assets {
		if !strings.HasSuffix(a.Name(), ".js") {
			continue
		}
		bundle, err := fs.ReadFile(cockpitFS, "assets/"+a.Name())
		if err != nil {
			t.Fatal(err)
		}
		text := string(bundle)
		for _, primitive := range []string{
			"child_process", "execSync", "spawnSync", "process.spawn", "require(\"child_process\")",
		} {
			if strings.Contains(text, primitive) {
				t.Errorf("the browser bundle contains a process-execution primitive %q in %s", primitive, a.Name())
			}
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

// TestCockpitAssetsLoadThroughTheMount: the panel must work at the URL the
// STARTUP BANNER prints, which is "/panel" — no trailing slash.
//
// This test used to concatenate "/panel/" by hand and then fetch the
// references under it. That asserted the author's assumption instead of the
// browser's behaviour: for a document at "/panel" the base is "/", so a
// browser resolves "./assets/x.js" to "/assets/x.js", which the binary does
// not serve. The shell loaded 200, both assets 404'd, and the panel was a
// blank page — with this test green (doubao-cloud-vm-01, round 59).
//
// The fix is a redirect from /panel to /panel/, and the test now resolves
// references the way an HTTP client does: follow the redirect, then join the
// reference against the URL that actually served the document.
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

	// Ask for the banner's URL exactly, and let the client follow redirects.
	resp, err := http.Get(ts.URL + "/panel")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /panel = %d, want 200 after redirect", resp.StatusCode)
	}
	// The document URL after redirects is what the browser uses as its base.
	base, err := url.Parse(resp.Request.URL.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(base.Path, "/") {
		t.Fatalf("the panel was served at %q, which is not a directory: relative assets resolve to /assets/* and 404", base.Path)
	}
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
		// Resolve against the SERVED URL, the way a browser does.
		target, err := base.Parse(ref)
		if err != nil {
			t.Errorf("cannot resolve %s against %s: %v", ref, base, err)
			continue
		}
		ar, err := http.Get(target.String())
		if err != nil {
			t.Errorf("GET %s: %v", target, err)
			continue
		}
		_ = ar.Body.Close()
		if ar.StatusCode != http.StatusOK {
			t.Errorf("the page at %s references %s, which resolves to %s and returns %d", base.Path, ref, target.Path, ar.StatusCode)
			continue
		}
		if strings.HasSuffix(ref, ".js") && !strings.Contains(ar.Header.Get("Content-Type"), "javascript") {
			t.Errorf("%s was served as %q, not JavaScript", target.Path, ar.Header.Get("Content-Type"))
		}
	}
}

// TestPanelRedirectsToItsSlashForm: the redirect itself is the contract the
// test above depends on, so pin it directly.
func TestPanelRedirectsToItsSlashForm(t *testing.T) {
	members := identity.NewMemberList("acct")
	srv := New(Config{PanelToken: "tok"})
	srv.EnableDiscovery(members)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.StopDiscovery()

	// A client that does NOT follow redirects sees where it is being sent.
	// Go's default client follows the 308 and would hand back the final 200,
	// which is exactly the shape of "assert the assumption, not the
	// behaviour" this file has been burned by before — the no-follow client
	// is constructed explicitly so nobody can quietly revert it
	// (doubao-cloud-vm-01, round 60).
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + "/panel")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Errorf("GET /panel = %d, want %d — without the redirect the browser resolves ./assets to /assets and the panel is blank",
			resp.StatusCode, http.StatusPermanentRedirect)
	}
	if loc := resp.Header.Get("Location"); loc != "/panel/" {
		t.Errorf("Location = %q, want /panel/", loc)
	}
	// And uncacheable: a cached 308 would outlive any later change of the
	// mount and poison a tester's "remove the redirect, see red" check.
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control on the redirect = %q, want no-store", cc)
	}
}

// TestCockpitIndexReferencesEmbeddedAssets ensures the HTML and embedded FS
// agree on every local JS/CSS asset. A local build can appear green while a
// commit forgets a newly hashed bundle; this test makes the embed surface
// itself the source of truth.
func TestCockpitIndexReferencesEmbeddedAssets(t *testing.T) {
	index, err := fs.ReadFile(cockpitFS, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?:src|href)\s*=\s*"(\./assets/[^"?#]+)"`).FindAllStringSubmatch(string(index), -1)
	if len(refs) == 0 {
		t.Fatal("embedded index.html references no local assets")
	}
	for _, match := range refs {
		name := strings.TrimPrefix(match[1], "./")
		if _, err := fs.Stat(cockpitFS, name); err != nil {
			t.Errorf("index.html references %s, but the embedded FS has no such asset: %v", match[1], err)
		}
	}
}

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
			// And the fabricated TELEMETRY, which needed no button at all: a
			// table of uptime/CPU/disk/traffic/SLA numbers for invented
			// devices, used as the fallback for every REAL one, jittered on a
			// 1.5 s timer so it looked live. Any of these strings reaching the
			// bundle means invented measurements are being shown as readings
			// (doubao-cloud-vm-01, round 59).
			"INITIAL_TELEMETRY",
			"实时流推送 (1.5s)",
			"香港锚点信标",
			"欧洲直连",
			"203.0.113.10:41820",
			"dev_01_anchor_hk",
			"42天 18小时",
			"99.98",
			// Browser-side actions are not implemented by the read-only panel.
			// Keeping this endpoint string in the production bundle makes the
			// unavailable controls look like a supported API.
			"/api/mesh/action",
			"模拟离线",
			"恢复上线",
			"用户模拟离线",
			"香港自托管公网锚点",
			"法兰克福裸露刀片",
			"苏黎世锥形 NAT",
			"东京全封闭隔离",
			"新加坡 CGNAT 归档",
		} {
			// note: "anchor-hk-01" is NOT on this list. It survives in one
			// place only — the design-specification page, where it is printed
			// inside a sentence saying the names there are placeholders and
			// the live roster is elsewhere. Blacklisting the token would force
			// that disclaimer to be deleted to make a test pass, which is
			// exactly backwards. TestCockpitDesignSpecCarriesItsDisclaimer
			// pins the disclaimer itself, so the exemption stays deliberate.
			if strings.Contains(text, bad) {
				t.Errorf("the shipped cockpit bundle still contains prototype-only machinery (%q) in %s", bad, a.Name())
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no JavaScript bundle was found to scan — the cockpit build is missing")
	}
}

// TestCockpitDesignSpecCarriesItsDisclaimer: "anchor-hk-01" is deliberately
// exempt from the demo blacklist because it appears only on the design
// specification page, inside a sentence that says the device names there are
// placeholders, not members of this network. An exemption nobody guards is
// just an open door — someone could later add real fake-roster content
// around that name and the blacklist would never notice. So this pins the
// disclaimer itself: it must stay in the bundle, and it must sit near the
// name it excuses (doubao-cloud-vm-01, round 60).
func TestCockpitDesignSpecCarriesItsDisclaimer(t *testing.T) {
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
		text := string(bundle)
		if strings.Contains(text, "anchor-hk-01") {
			if !strings.Contains(text, "示意占位") || !strings.Contains(text, "不是本网络里的真实设备") {
				t.Errorf("%s uses anchor-hk-01 but no longer says the names on that page are placeholders — the demo-content exemption has lost its cover", a.Name())
			}
			found = true
		}
	}
	if !found {
		t.Error("the design-specification page's placeholder names are gone from the bundle; update the exemption note in TestCockpitHasNoDemoControls")
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
