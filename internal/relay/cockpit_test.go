// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

// Tests for the cockpit: the accepted prototype, built and shipped inside
// the binary.
//
// Three things are pinned here: the built app must reference NOTHING
// outside the binary (it has to render inside a container with no network),
// the roster document must carry the fields the cockpit reads, and the
// panel must not carry data that is not the roster's — no demo device
// names, no fake telemetry (D3), and no roster of its own (D4).
//
// The file is organised in three parts: the cockpit's own contract (the
// first tests), then D3 (the demo-content guards) and then D4 (the scene's
// device count).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
//
// The surface is EVERY file of the embedded cockpit — index.html included.
// The old version scanned the JavaScript bundles only, so a demo name
// printed in the page shell was invisible to it (M4-4).
func TestCockpitHasNoDemoControls(t *testing.T) {
	files := cockpitArtifactFiles(t)
	if len(files) == 0 {
		t.Fatal("the embedded cockpit has no files to scan — the cockpit build is missing")
	}
	scanned := 0
	for _, f := range files {
		scanned++
		for _, bad := range cockpitDemoBlacklist {
			// note: the four names of the design-specification page are NOT
			// on this list. They are exempt on purpose — they are printed
			// inside a sentence that says the names there are placeholders
			// and the live roster is elsewhere, and blacklisting them would
			// force that disclaimer to be deleted to make a test pass,
			// which is exactly backwards. The exemption is written down in
			// cockpitDemoExemptions, and TestCockpitDemoNameExemptionStaysOnTheSpecPage
			// pins it: the names may only appear in their region, only as
			// often as the exemption allows, and only next to the
			// disclaimer.
			//
			// The demo NETWORK names are NOT on this list either, but they
			// are on the exemption list with their own scope: the panel
			// does print them, in the specification page and in the
			// multi-mesh modal. Listing them as plain hits of the
			// blacklist would be a guard that is red for ever — and a guard
			// nobody runs guards nothing.
			if bytes.Contains(f.data, []byte(bad)) {
				t.Errorf("the shipped cockpit artifact still contains prototype-only machinery (%q) in %s", bad, f.name)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no file of the embedded cockpit was found to scan — the cockpit build is missing")
	}
}

// TestCockpitDesignSpecCarriesItsDisclaimer: the names of the design
// specification page are deliberately exempt from the demo blacklist
// because they appear only on that page, inside a sentence that says the
// device names there are placeholders, not members of this network. An
// exemption nobody guards is just an open door — someone could later add
// real fake-roster content around those names and the blacklist would
// never notice. So this pins the disclaimer itself: it must stay in the
// artifact, and it must sit near every name it excuses
// (doubao-cloud-vm-01, round 60; M4-4 extended it to the whole
// exemption list).
func TestCockpitDesignSpecCarriesItsDisclaimer(t *testing.T) {
	files := cockpitArtifactFiles(t)
	found := false
	for _, f := range files {
		for _, e := range cockpitDemoExemptions {
			if !bytes.Contains(f.data, []byte(e.token)) {
				continue
			}
			if e.scope[cockpitScopeSpec] == 0 {
				continue
			}
			found = true
			d := cockpitScopeMarkers[cockpitScopeSpec].disclaimer
			if !bytes.Contains(f.data, []byte(d[0])) || !bytes.Contains(f.data, []byte(d[1])) {
				t.Errorf("%s uses %s but no longer says the names on that page are placeholders "+
					"(both %q and %q must be present) — the demo-content exemption has lost its cover",
					f.name, e.token, d[0], d[1])
			}
		}
	}
	if !found {
		t.Error("the design-specification page's placeholder names are gone from the artifact; " +
			"update cockpitDemoExemptions and the exemption note in TestCockpitHasNoDemoControls")
	}
}

// TestCockpitMultiMeshModalStillSaysItIsAPreview pins the sentence that
// keeps the multi-mesh dialog a preview of a future feature.
//
// ★ The dialog prints "当前激活网络: prod-container-mesh (acct_9f82c104 …)",
// which reads as a claim about the network in use — that is why the two
// names are on the exemption list with the multi-mesh scope and NOT on
// the blacklist. This test makes sure the dialog keeps announcing itself
// as a preview, so the exemption keeps its cover.
func TestCockpitMultiMeshModalStillSaysItIsAPreview(t *testing.T) {
	files := cockpitArtifactFiles(t)
	found := false
	d := cockpitScopeMarkers[cockpitScopeMultiMesh].disclaimer
	for _, f := range files {
		if !bytes.Contains(f.data, []byte(d[0])) && !bytes.Contains(f.data, []byte(d[1])) {
			continue
		}
		found = true
		if !bytes.Contains(f.data, []byte(d[0])) || !bytes.Contains(f.data, []byte(d[1])) {
			t.Errorf("%s no longer says the multi-mesh dialog is a preview of a future feature "+
				"(both %q and %q must be present)", f.name, d[0], d[1])
		}
	}
	if !found {
		t.Errorf("the multi-mesh dialog's preview wording is gone from the artifact "+
			"(neither %q nor %q is there)", d[0], d[1])
	}
}

// TestCockpitShippedBinaryCarriesNoDemoContent scans the PRODUCT BINARY —
// the thing an external tester runs `strings` on. The embedded artifact is
// stored in the binary byte for byte, so this scan sees everything the
// page can show plus everything the Go side prints.
//
// The positive control matters as much as the assertions: a scan that
// finds nothing because it looked at the wrong bytes would be green for
// ever. So the test first proves the binary really carries the panel.
//
// ★ It also checks the OTHER direction: the exempt and residue tokens may
// not appear in the binary MORE often than they appear in the artifact.
// The binary embeds the artifact, so an extra hit would mean the Go side
// prints a demo name as well.
func TestCockpitShippedBinaryCarriesNoDemoContent(t *testing.T) {
	bin := buildProductBinary(t)

	// Positive control: the embedded page is IN the binary.
	sentinel := []byte("<title>cmesh — 容器与设备私有组网客户端</title>")
	if !bytes.Contains(bin, sentinel) {
		t.Fatalf("the built binary does not contain the panel's index.html (%q); "+
			"the demo-content scan below would be vacuous", string(sentinel))
	}

	for _, bad := range cockpitDemoBlacklist {
		if n := bytes.Count(bin, []byte(bad)); n > 0 {
			t.Errorf("the product binary contains the demo content %q (%d hit(s)): "+
				"an external `strings cmesh | grep -F %s` scan hits it, want 0",
				bad, n, bad)
		}
	}

	// The exempt and residue tokens may be in the binary — but only as
	// often as they are in the artifact. The artifact is embedded
	// verbatim, so anything more would come from the Go side.
	artifactHits := map[string]int{}
	for _, f := range cockpitArtifactFiles(t) {
		for _, name := range cockpitAllDemoIdentities() {
			artifactHits[name] += bytes.Count(f.data, []byte(name))
		}
	}
	for _, name := range cockpitAllDemoIdentities() {
		want := artifactHits[name]
		if got := bytes.Count(bin, []byte(name)); got != want {
			t.Errorf("the product binary holds %q %d time(s), but the embedded artifact holds it %d time(s): "+
				"the binary carries a demo name the panel does not show", name, got, want)
		}
	}
}

// TestCockpitDemoNameExemptionStaysOnTheSpecPage keeps the exemptions
// honest. Three things have to hold at the same time:
//
//   - every demo identity that still appears in the artifact is written
//     down in the exemption list or the residue list (an identity nobody
//     listed is an identity nobody checked);
//   - an exempt name appears ONLY inside the regions its exemption names
//     — the roster views and the 3D scene are not the specification page,
//     and the multi-mesh modal is not the specification page either;
//   - it appears there no more often than the exemption allows, so an
//     exemption cannot grow into a hiding place.
func TestCockpitDemoNameExemptionStaysOnTheSpecPage(t *testing.T) {
	exempt := map[string]cockpitDemoExemption{}
	for _, e := range cockpitDemoExemptions {
		if _, dup := exempt[e.token]; dup {
			t.Errorf("the exemption list names %q twice", e.token)
		}
		exempt[e.token] = e
	}

	files := cockpitArtifactFiles(t)
	if len(files) == 0 {
		t.Fatal("the embedded cockpit has no files to scan")
	}

	found := 0
	for _, f := range files {
		for _, name := range cockpitPrototypeDemoNames {
			if !bytes.Contains(f.data, []byte(name)) {
				continue
			}
			found++
			e, ok := exempt[name]
			if !ok {
				t.Errorf("%s carries the demo device name %q and the exemption list does not name it; "+
					"either delete it from the product or write the exemption down", f.name, name)
				continue
			}
			cockpitCheckExemptScope(t, f, e)
		}
	}
	if found == 0 {
		t.Errorf("none of the prototype's demo device names is in the artifact any more; " +
			"update cockpitDemoExemptions and the exemption note in TestCockpitHasNoDemoControls")
	}

	// The demo network names go through the same scope check.
	for _, f := range files {
		for _, name := range cockpitDemoNetworkNames {
			if !bytes.Contains(f.data, []byte(name)) {
				continue
			}
			e, ok := exempt[name]
			if !ok {
				t.Errorf("%s names the demo network %q and the exemption list does not name it; "+
					"either delete it from the product or write the exemption down", f.name, name)
				continue
			}
			cockpitCheckExemptScope(t, f, e)
		}
	}
}

// TestCockpitDemoResidueIsOnlyTheKnownOne pins the residue: the demo data
// that is still in the artifact, on the product's live code path, with no
// region excuse.
//
// The residue is `zone_anchor_hub` — the PROTOTYPE's anchor zone id. The
// shipped 3D scene compares a zone id against it in two places, to paint
// the anchor zone differently — but the product's own zone id is
// `zone_self_hosted` (cockpit_model.go), so those comparisons can never
// match. It is inert (TestCockpitPrototypeZoneResidueIsInert), but
// "inert" is not "absent", and an external `strings` scan hits it.
//
// ★ The residue is pinned, not tolerated by silence: it must be exactly
// the documented token, exactly as often as documented, and it must not
// grow. A new prototype string on the live path goes red here.
func TestCockpitDemoResidueIsOnlyTheKnownOne(t *testing.T) {
	files := cockpitArtifactFiles(t)

	for _, r := range cockpitDemoResidues {
		hits := 0
		for _, f := range files {
			hits += bytes.Count(f.data, []byte(r.token))
		}
		if hits != r.count {
			t.Errorf("the demo residue %q appears %d time(s) in the artifact, want %d: "+
				"the residue has changed — update cockpitDemoResidues, or take the data out of the product",
				r.token, hits, r.count)
		}
	}
}

// TestCockpitLivePathNamesNoUnknownIdentity is the completeness guard for
// the LIVE path — the part the previous guard could not see.
//
// The blacklist can only hit what it lists, so a NEW demo name would slip
// through it. This guard reads the panel's TEXT — the strings the operator
// actually reads, which the minifier keeps out of the className values —
// and demands that every identity-like name in there is on the known list.
//
// The names it finds are the ones the panel prints as words: the device
// names of the specification page, and the network names of the
// multi-mesh modal. A name that nobody wrote down goes red here.
//
// The scan is deliberately narrow: identity-like names only (lower-case
// words joined by dashes), and only inside text nodes, so the panel's
// styling classes and the three.js internals do not drown it.
func TestCockpitLivePathNamesNoUnknownIdentity(t *testing.T) {
	known := map[string]string{}
	for _, e := range cockpitDemoExemptions {
		known[e.token] = "exempt"
	}
	for _, r := range cockpitDemoResidues {
		known[r.token] = "residue"
	}
	for _, n := range cockpitProductOwnNames {
		known[n] = "the product's own name"
	}

	files := cockpitArtifactFiles(t)
	for _, f := range files {
		for _, token := range cockpitIdentityNamesInText(f.data) {
			if _, ok := known[token]; !ok {
				t.Errorf("%s shows the identity-like name %q to the operator, and it is on neither the "+
					"exemption list nor the residue list: the criterion's zero-hits would miss it. "+
					"Write it down, or take it out of the product", f.name, token)
			}
		}
	}
}

// cockpitProductOwnNames are the product's own names — they look like the
// demo names but they are the truth.
var cockpitProductOwnNames = []string{
	"container-mesh",
}

// reCockpitTextNode matches a text node of the minified bundle: the
// template literal right after `children:`. The className values are a
// different key, so the styling never reaches this scan.
var reCockpitTextNode = regexp.MustCompile("children:(?:\\[)?`([^`]*)`")

// reCockpitIdentityLike matches an identity-like name: lower-case words
// joined by dashes, at least two of them ("prod-container-mesh",
// "gpu-trainer-02").
var reCockpitIdentityLike = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:-[a-z0-9]+){1,4}\b`)

// cockpitIdentityNamesInText returns the identity-like names the panel
// shows as text.
func cockpitIdentityNamesInText(data []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reCockpitTextNode.FindAllSubmatch(data, -1) {
		text := string(m[1])
		for _, tok := range reCockpitIdentityLike.FindAllString(text, -1) {
			if seen[tok] {
				continue
			}
			seen[tok] = true
			out = append(out, tok)
		}
	}
	sort.Strings(out)
	return out
}

// TestCockpitDemoCriterionMissesOnlyKnownIdentities is the completeness
// guard of D3.
//
// The criterion is "zero hits". A blacklist can only hit what it lists, so
// the list has to be checked against the artifact: every demo identity the
// artifact still carries must be written down in the exemption list or in
// the residue list.
//
// ★ This is what keeps the criterion honest: when the frontend drops the
// names, the lists shrink; when somebody adds a new demo name, this test
// goes red with the name in the message — instead of the name slipping
// through a list that nobody re-checked.
func TestCockpitDemoCriterionMissesOnlyKnownIdentities(t *testing.T) {
	known := map[string]string{}
	for _, e := range cockpitDemoExemptions {
		known[e.token] = "exempt"
	}
	for _, r := range cockpitDemoResidues {
		known[r.token] = "residue"
	}

	files := cockpitArtifactFiles(t)
	for _, f := range files {
		for _, name := range cockpitAllDemoIdentities() {
			if !bytes.Contains(f.data, []byte(name)) {
				continue
			}
			if _, ok := known[name]; !ok {
				t.Errorf("%s carries the demo identity %q, which is on neither the exemption list nor the residue list: "+
					"the criterion's zero-hits would miss it. Write it down, or take it out of the product",
					f.name, name)
			}
		}
	}
}

// TestCockpitPrototypeZoneResidueIsInert pins the one piece of prototype
// data that IS still in the product's live code path.
//
// The shipped 3D scene compares a zone id against "zone_anchor_hub" — the
// PROTOTYPE's anchor zone id — in two places, to paint the anchor zone
// differently. The product's own zone id is "zone_self_hosted"
// (cockpit_model.go), so those comparisons can never match.
//
// That is a leftover, not a fake device: it cannot make the scene paint a
// zone that does not exist. But it IS prototype data in the product, and an
// external `strings` scan of the binary hits it. So the guard says what is
// true — the anchor's payload never carries that zone id — and the finding
// is written down instead of being quietly accepted.
//
// Fixing it means editing web/src/components/IsometricMeshCockpit.tsx and
// rebuilding the artifact, which is the frontend's line, not this one's.
func TestCockpitPrototypeZoneResidueIsInert(t *testing.T) {
	const residue = "zone_anchor_hub"

	files := cockpitArtifactFiles(t)
	present := false
	for _, f := range files {
		if bytes.Contains(f.data, []byte(residue)) {
			present = true
		}
	}
	if !present {
		t.Errorf("the prototype's zone id %q is gone from the artifact; "+
			"update this test and cockpitDemoResidues — the residue it pins no longer exists", residue)
	}

	// The residue is inert only if the anchor never emits that zone id.
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
	var payload MeshResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	for _, z := range payload.Zones {
		if z.ZoneID == residue {
			t.Errorf("the payload carries the prototype's zone id %q; the 3D scene would paint a prototype zone", residue)
		}
	}
	for _, m := range payload.Members {
		if m.ZoneID == residue {
			t.Errorf("member %s carries the prototype's zone id %q; the 3D scene would paint a prototype zone",
				m.DeviceID, residue)
		}
	}
	if len(payload.Zones) == 0 {
		t.Fatal("the payload has no zones")
	}
	if payload.Zones[0].ZoneID != cockpitZoneID {
		t.Errorf("the payload's zone id = %q, want %q (the product's own zone id): "+
			"the prototype's %q must match nothing", payload.Zones[0].ZoneID, cockpitZoneID, residue)
	}
}

// cockpitPrototypeDemoNames is the prototype's demo corpus: every device
// name and device id the design prototype invented. The product ships none
// of them; the design-specification page is allowed to PRINT some of them
// as placeholders (see cockpitDemoExemptions).
//
// Source: design-prototypes/aistudio-v1/src/data/meshSnapshots.ts and
// design-prototypes/aistudio-v1/src/components/IsometricMeshCockpit.tsx.
// The list is written out here on purpose: a test that reads its own
// expectations out of a directory it does not ship would stop guarding
// anything the moment that directory is deleted.
var cockpitPrototypeDemoNames = []string{
	// device ids
	"dev_01_anchor_hk",
	"dev_02_local_ws",
	"dev_03_gpu_fra",
	"dev_04_ci_sandbox",
	"dev_05_home_nas",
	"dev_06_hk_infer",
	"dev_07_hk_sandbox",
	// display names
	"anchor-hk-01",
	"macbook-studio-local",
	"gpu-trainer-02",
	"ci-runner-ephemeral",
	"home-nas-vault",
	"hk-infer-blade-02",
	"hk-sandbox-worker-03",
}

// cockpitPrototypeDemoData is the fake ROSTER data the prototype carried:
// zones, aliases, endpoints, coordinates, images, work directories and the
// telemetry table. Every one of these is invented, so none of them may
// reach the product.
var cockpitPrototypeDemoData = []string{
	// fake zones
	"zone_cloud_prod",
	"zone_edge_sandbox",
	"自托管锚点机房",
	"欧洲云端与本机集群",
	"亚太受限沙箱与家庭内网区",
	// fake aliases
	"香港自托管发现锚点",
	"法兰克福推理容器 A100",
	"东京受限构建沙箱",
	"新加坡家庭私有云归档节点",
	// fake coordinates and cities
	"中国香港 (HKG)",
	"瑞士苏黎世 (ZRH)",
	"德国法兰克福 (FRA)",
	"日本东京 (NRT)",
	"新加坡 (SIN)",
	"22.32°N, 114.17°E",
	// fake endpoints
	"203.0.113.10:41820",
	"[2001:db8:hk::10]:41820",
	"198.51.100.89:41820",
	"100.64.0.88:41820",
	// fake images and runtimes
	"ghcr.io/cmesh-net/anchor",
	"ghcr.io/cmesh-net/ci-agent",
	"pytorch/pytorch:2.4-cuda12.1-cmesh",
	"oci-runc (Docker 26.1)",
	"containerd (Kubernetes Pod",
	"gVisor runsc",
	"Alpine 3.20",
	// fake work directories
	"/var/lib/cmesh-anchor",
	"/workspace/checkpoints",
	"/workspace/build-artifacts",
	// fake telemetry
	"INITIAL_TELEMETRY",
	"42天 18小时",
	"14天 06小时",
	"名册分发 v42",
	"P2P 直连活跃",
	"实时流推送 (1.5s)",
	"香港锚点信标",
	"欧洲直连",
	"99.98",
}

// cockpitPrototypeControls is the prototype's machinery: the scenario
// switcher, the buttons that faked a device's liveness, the default-ON
// toggle that appended two invented Hong Kong machines to the live roster,
// and the write endpoint those buttons called.
var cockpitPrototypeControls = []string{
	"BASE_SCENARIOS",
	"PROTOTYPE STATE SWITCHER",
	"btnSimToggleOnline",
	"btnSimToggleEndpoint",
	"SimToggle",
	"_savedEndpoints",
	"getMeshDataByScenario",
	"simulateHkMultiNodes",
	"EXTRA_HK_CLUSTER_NODES",
	"/api/mesh/action",
	"模拟离线",
	"恢复上线",
	"用户模拟离线",
	"香港自托管公网锚点",
	"法兰克福裸露刀片",
	"苏黎世锥形 NAT",
	"东京全封闭隔离",
	"新加坡 CGNAT 归档",
	"香港3机智能聚合",
}

// cockpitDemoNetworkNames are the demo NETWORKS the panel names: the
// network it claims to be connected to and the networks it promises for
// the future.
//
// Source: web/src/App.tsx:829-835 ("● 当前激活网络: prod-container-mesh
// (acct_9f82c104 …)") and web/src/components/DesignSystemManual.tsx:487.
//
// ★ THE IMPORTANT ONE: `prod-container-mesh` and `acct_9f82c104` are
// printed by the MULTI-MESH MODAL, which a user opens from the panel's
// own toolbar ("多网切换", web/src/App.tsx:537). That is a LIVE path, not
// the design-specification page — so the specification-page exemption
// cannot cover it. The panel shows the operator a network name and an
// account id that do not exist.
//
// They are on the exemption list with their own scope (the specification
// page and the multi-mesh modal), and the guards pin both the scope and
// the count.
var cockpitDemoNetworkNames = []string{
	"prod-container-mesh",
	"acct_9f82c104",
	"ai-agent-sandbox-net",
	"home-private-cloud",
	"corp-dev-mesh",
	"home-lab-mesh",
}

// cockpitDemoBlacklist is what must hit ZERO times in the shipped
// artifact and in the product binary — the criterion's "0 hits", with no
// excuse.
//
// ★ THE CRITERION IS "ZERO HITS", NOT "ZERO DEVICE NAMES". The panel used
// to invent a whole network — a network name, an account id, the networks
// it would support one day — and those strings are as much "demo data" as
// a device name is: an operator reading them believes the network is
// called "prod-container-mesh" and belongs to "acct_9f82c104".
//
// The exempt and residue identities are NOT on this list: they are
// handled by their own guards, which pin their scope and their count.
var cockpitDemoBlacklist = func() []string {
	exempt := map[string]bool{}
	for _, e := range cockpitDemoExemptions {
		exempt[e.token] = true
	}
	out := make([]string, 0, len(cockpitPrototypeDemoNames)+len(cockpitPrototypeDemoData)+
		len(cockpitPrototypeControls))
	for _, name := range cockpitPrototypeDemoNames {
		if exempt[name] {
			continue
		}
		out = append(out, name)
	}
	out = append(out, cockpitPrototypeDemoData...)
	out = append(out, cockpitPrototypeControls...)
	return out
}()

// cockpitDemoExemption is one identifier the product is allowed to print,
// with the reason it is allowed and the regions it is allowed in.
//
// ★ The region matters as much as the count. A name printed on the
// design-specification page is a specification; the same name printed in
// the multi-mesh modal — which the operator opens from the panel's own
// toolbar — is a live claim about the network, and a specification-page
// exemption cannot cover that.
type cockpitDemoExemption struct {
	token string
	why   string
	// scope is the region the token may appear in, and how many times.
	scope map[string]int
}

// cockpitDemoResidue is one demo identity that is still in the artifact,
// on the product's live code path, with no region excuse.
//
// It is the residue of the criterion: the criterion wants 0 hits, and
// every residue is one hit that is still there.
type cockpitDemoResidue struct {
	token string
	why   string
	// count is how many times it appears in the artifact.
	count int
}

// cockpitDemoExemptions is THE exemption list. It is short on purpose: an
// exemption nobody can see is how a fake roster comes back.
//
// ★ EVERY demo identity the panel still carries is on one of the two
// lists below — the exemption list or the residue list. The guard
// TestCockpitDemoCriterionMissesOnlyKnownIdentities checks that: a demo
// identity that nobody wrote down is a defect, and it goes red.
//
// The four device names are the ones the design-specification page prints
// in its "four-dimensional device state matrix" table. That page is a
// specification, not a roster: it says so in the sentence right above the
// table ("本页是设计规范。表格中的设备名…是示意占位，不是本网络里的真实设备"),
// and it points the reader at the real roster.
//
// The demo NETWORK names are the ones the panel invents a network and an
// account with. Two of them (`prod-container-mesh`, `acct_9f82c104`) are
// printed by the multi-mesh modal, which the operator opens from the
// panel's toolbar — a LIVE path, so a specification-page exemption does
// not cover them; they carry the multi-mesh scope as well, and that
// dialog must keep announcing itself as a preview.
//
// ★ The alternative to every exemption — deleting the names — is not on
// the table in this round: the pages' sources are web/src/App.tsx and
// web/src/components/DesignSystemManual.tsx, and the product ships a BUILD
// of them (internal/relay/cockpit-dist). Editing the artifact by hand
// would leave the shipped panel and the repository's source saying
// different things, which is worse than a documented exemption. So the
// names stay, the exemptions are written down here with their scope, and
// the guards below hold them to that scope.
//
// WHAT THE 网主 HAS TO RULE ON: either the frontend drops the names and
// rebuilds the artifact (next round, the frontend's line), or the
// criterion's wording gets an explicit exemption for them. Today they
// are exempt BY THIS TABLE only — the criterion's literal "0 hits" is not
// met.
var cockpitDemoExemptions = []cockpitDemoExemption{
	{"anchor-hk-01", "design-specification page, printed as a placeholder next to the disclaimer", map[string]int{cockpitScopeSpec: 2}},
	{"gpu-trainer-02", "design-specification page, printed as a placeholder next to the disclaimer", map[string]int{cockpitScopeSpec: 2}},
	{"ci-runner-ephemeral", "design-specification page, printed as a placeholder next to the disclaimer", map[string]int{cockpitScopeSpec: 2}},
	{"home-nas-vault", "design-specification page, printed as a placeholder next to the disclaimer", map[string]int{cockpitScopeSpec: 2}},
	{"prod-container-mesh", "the network the panel claims to be connected to; also printed by the multi-mesh modal, which is the LIVE path", map[string]int{cockpitScopeSpec: 1, cockpitScopeMultiMesh: 1}},
	{"acct_9f82c104", "the account the panel claims to belong to; also printed by the multi-mesh modal, which is the LIVE path", map[string]int{cockpitScopeSpec: 1, cockpitScopeMultiMesh: 1}},
	{"ai-agent-sandbox-net", "a network the panel promises for the future; printed by the multi-mesh modal", map[string]int{cockpitScopeMultiMesh: 1}},
	{"home-private-cloud", "a network the panel promises for the future; printed by the multi-mesh modal", map[string]int{cockpitScopeMultiMesh: 1}},
	{"corp-dev-mesh", "a network the panel promises for the future; printed by the design-specification page", map[string]int{cockpitScopeSpec: 1}},
	{"home-lab-mesh", "a network the panel promises for the future; printed by the design-specification page", map[string]int{cockpitScopeSpec: 1}},
}

// cockpitDemoResidues are the demo identities that are still in the
// artifact and that NO region excuse covers.
//
// `zone_anchor_hub` is the PROTOTYPE's anchor zone id. The shipped 3D
// scene compares a zone id against it in two places, to paint the anchor
// zone differently — but the product's own zone id is `zone_self_hosted`
// (cockpit_model.go), so those comparisons can never match.
//
// That makes it a leftover, not a fake device: it cannot make the scene
// paint a zone that does not exist. But it IS prototype data in the
// product, and an external `strings` scan of the binary hits it — so it
// is written down here instead of being quietly accepted.
//
// Removing it means editing web/src/components/IsometricMeshCockpit.tsx
// and rebuilding the artifact, which is the frontend's line, not this
// one's. TestCockpitPrototypeZoneResidueIsInert pins that it is inert in
// the meantime.
var cockpitDemoResidues = []cockpitDemoResidue{
	{"zone_anchor_hub", "the PROTOTYPE's anchor zone id, still compared in the 3D scene's live code path; " +
		"inert (the product's own zone id is zone_self_hosted and never matches it), but strings hits it", 2},
}

// The regions of the artifact. Each is delimited by two markers that
// occur exactly once in the shipped bundle, and each carries the sentence
// that makes the names inside it honest.
var cockpitScopeMarkers = map[string]struct {
	start, end string
	// disclaimer is the pair of sentences the region must carry while
	// anything is exempt inside it. Both halves must be there: one
	// without the other reads as a plain name.
	disclaimer [2]string
}{
	cockpitScopeSpec: {
		start:      "面向 Windows / macOS 桌面应用及移动端扩展的用户态私有组网客户端规范",
		end:        "12. PROTOTYPE PAGES & FACT BOUNDARIES",
		disclaimer: [2]string{"示意占位", "不是本网络里的真实设备"},
	},
	cockpitScopeMultiMesh: {
		start:      "FUTURE EXTENSION SLOT",
		end:        "知悉并返回",
		disclaimer: [2]string{"预留扩展槽位，不伪装成已上线功能", "不把未来功能伪装成当前已上线能力"},
	},
}

// The regions of the artifact.
const (
	// cockpitScopeSpec is the design-specification manual: a
	// specification, not a roster. It says so on the page.
	cockpitScopeSpec = "spec"
	// cockpitScopeMultiMesh is the multi-mesh modal: a LIVE dialog the
	// operator opens from the panel's toolbar.
	cockpitScopeMultiMesh = "multi-mesh"
)

// cockpitAllDemoIdentities is every demo identity the criterion knows
// about: the prototype's device names, its fake roster data, its
// controls, and the demo networks the panel invents.
func cockpitAllDemoIdentities() []string {
	out := make([]string, 0, len(cockpitPrototypeDemoNames)+len(cockpitPrototypeDemoData)+
		len(cockpitPrototypeControls)+len(cockpitDemoNetworkNames))
	out = append(out, cockpitPrototypeDemoNames...)
	out = append(out, cockpitPrototypeDemoData...)
	out = append(out, cockpitPrototypeControls...)
	out = append(out, cockpitDemoNetworkNames...)
	return out
}

// cockpitCheckExemptScope checks one exemption against one file: the token
// may appear only inside the regions the exemption names, and no more
// often there than the exemption allows.
func cockpitCheckExemptScope(t *testing.T, f cockpitArtifactFile, e cockpitDemoExemption) {
	t.Helper()

	// Every region the exemption names must be there, exactly once.
	bounds := map[string][2]int{}
	for scope := range e.scope {
		start, end, ok := cockpitScopeBounds(f.data, scope)
		if !ok {
			t.Errorf("%s carries the exempt name %q but the %q region's markers are not both there exactly once; "+
				"the exemption's scope cannot be checked", f.name, e.token, scope)
			return
		}
		bounds[scope] = [2]int{start, end}
	}

	// And the token must be inside one of them, no more often than the
	// exemption allows.
	inside := map[string]int{}
	for _, at := range bytesIndices(f.data, e.token) {
		in := ""
		for scope, b := range bounds {
			if at >= b[0] && at <= b[1] {
				in = scope
			}
		}
		if in == "" {
			t.Errorf("%s carries the exempt name %q at offset %d, which is OUTSIDE every region it is exempt in "+
				"(%s): a placeholder name outside its region is a fake roster entry",
				f.name, e.token, at, strings.Join(cockpitScopeNames(e.scope), ", "))
			continue
		}
		inside[in]++
	}
	for scope, want := range e.scope {
		if inside[scope] != want {
			t.Errorf("%s prints the exempt name %q %d time(s) inside the %q region, want %d: "+
				"the exemption has grown beyond what it covers",
				f.name, e.token, inside[scope], scope, want)
		}
	}
}

// cockpitScopeNames lists the regions of an exemption's scope, for the
// failure messages.
func cockpitScopeNames(scope map[string]int) []string {
	out := make([]string, 0, len(scope))
	for s := range scope {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// cockpitScopeBounds returns the offsets of a region's two markers. The
// markers must occur exactly once, so a rebuilt artifact cannot silently
// widen or shrink a region.
func cockpitScopeBounds(data []byte, scope string) (int, int, bool) {
	m, ok := cockpitScopeMarkers[scope]
	if !ok {
		return 0, 0, false
	}
	if bytes.Count(data, []byte(m.start)) != 1 || bytes.Count(data, []byte(m.end)) != 1 {
		return 0, 0, false
	}
	start := bytes.Index(data, []byte(m.start))
	end := bytes.Index(data, []byte(m.end))
	if start < 0 || end < 0 || end < start {
		return 0, 0, false
	}
	return start, end, true
}

// cockpitDemoNameIsLive reports whether the token is printed OUTSIDE the
// design-specification region — that is, on a path the operator reaches
// from the panel itself.
func cockpitDemoNameIsLive(data []byte, name string) bool {
	start, end, ok := cockpitScopeBounds(data, cockpitScopeSpec)
	if !ok {
		// Without the region the safest reading is "live".
		return true
	}
	for _, at := range bytesIndices(data, name) {
		if at < start || at > end {
			return true
		}
	}
	return false
}

// cockpitArtifactFile is one file of the embedded cockpit.
type cockpitArtifactFile struct {
	name string
	data []byte
}

// cockpitArtifactFiles reads every file of the embedded cockpit — the page
// shell (index.html) and the assets. The binary embeds these bytes
// verbatim, so this IS the artifact the binary carries.
func cockpitArtifactFiles(t *testing.T) []cockpitArtifactFile {
	t.Helper()
	var out []cockpitArtifactFile
	err := fs.WalkDir(cockpitFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(cockpitFS, p)
		if err != nil {
			return err
		}
		out = append(out, cockpitArtifactFile{name: p, data: data})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// bytesIndices returns the offsets of every occurrence of needle.
func bytesIndices(haystack []byte, needle string) []int {
	var out []int
	for i := 0; ; {
		j := bytes.Index(haystack[i:], []byte(needle))
		if j < 0 {
			return out
		}
		out = append(out, i+j)
		i += j + 1
	}
}

// buildProductBinary builds ./cmd/cm — the binary the release ships — and
// returns its bytes. This is the file an external tester runs `strings`
// on, so it is the surface the acceptance criterion names.
func buildProductBinary(t *testing.T) []byte {
	t.Helper()
	root := repoRoot(t)
	out := filepath.Join(t.TempDir(), "cmesh")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/cm")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building ./cmd/cm failed: %v\n%s", err, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// repoRoot walks up from the test's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s: the module root cannot be found", dir)
		}
		dir = parent
	}
}

// ---------------------------------------------------------------------------
// D4 — the cockpit must not draw a roster of its own (M4-4).
// The shapes this file reads out of the shipped bundle. They are the
// MINIFIED forms of the product's own source (web/src/components/
// IsometricMeshCockpit.tsx and web/src/App.tsx), so they are stable for
// as long as the committed artifact is. A rebuild that renames them makes
// this guard red with a message that says what has to be looked at again —
// which is the right outcome: the scene's device count is a claim about
// the product, and a new build has to be looked at again.
var (
	// The cockpit component takes the roster as a prop: `Sf=({zones:e,members:t,…)`.
	reCockpitPropsMembers = regexp.MustCompile(`([A-Za-z_$][\w$]*)=\(\{[^{}]{0,600}?\bmembers:([A-Za-z_$][\w$]*)\s*[,}]`)
	// The one place a 3D device node is created: `userData={entityType:`device`,deviceId:e.device_id,…}`.
	reCockpitDeviceNode = regexp.MustCompile("userData=\\{entityType:`device`,deviceId:([A-Za-z_$][\\w$]*)\\.device_id[,}]")
	// A forEach over an array.
	reCockpitForEach = regexp.MustCompile(`\.forEach\(`)
	// The device count the 3D view publishes in its accessible label.
	reCockpitSceneCount = regexp.MustCompile("三维名册视图：\\$\\{([A-Za-z_$][\\w$]*)\\.length\\} 台设备")
	// The empty payload the app starts from — it has a `members` field too.
	reCockpitEmptyPayload = regexp.MustCompile("([A-Za-z_$][\\w$]*)=\\{account_id:``[^}]*members:\\[\\]\\}")
	// The state that holds the payload: `[n,r]=(0,_.useState)(Df)`.
	reCockpitUseState = regexp.MustCompile(`\[([A-Za-z_$][\w$]*),([A-Za-z_$][\w$]*)\]=\(0,_\.useState\)\(([A-Za-z_$][\w$]*)\)`)
	// The API body, parsed as it is: `let{data:t,…}=await C(e)`.
	reCockpitFetchData = regexp.MustCompile(`\{data:([A-Za-z_$][\w$]*),needsToken:[A-Za-z_$][\w$]*,error:[A-Za-z_$][\w$]*\}=await [A-Za-z_$][\w$]*\(`)
	// `await res.json()` — the body is used verbatim.
	reCockpitJSONBody = regexp.MustCompile(`await [A-Za-z_$][\w$]*\.json\(\)`)
)

// cockpitSceneFacts is what the shipped bundle says about the scene's
// device count.
type cockpitSceneFacts struct {
	Component   string // the identifier the cockpit component is bound to
	MembersProp string // the identifier the cockpit binds its `members` prop to
	NodeSiteAt  int    // where the 3D device node is created
	LoopAt      int    // where the enclosing forEach starts
	Iterated    string // the identifier the enclosing loop iterates
	BindingExpr string // the expression that identifier was last bound to
	CountArray  string // the identifier the published device count reads .length from
	StateVar    string // the app state the cockpit is handed `members` from
}

// analyzeCockpitScene reads the shipped bundle and reports what the 3D
// scene counts. Every finding it cannot establish comes back as a
// violation — the guard never guesses.
//
// ★ WHAT THIS PROVES, PRECISELY: that in THIS artifact, the array the 3D
// scene iterates is — syntactically — the very same identifier the cockpit
// received as its `members` prop, and that the device count the panel
// publishes is that identifier's `.length`. That is a statement about the
// SHAPE of the code (同形性), not a proof of semantic equivalence: a
// reader who wants to know whether the drawn node count equals the roster
// length still has to look at the rendered scene.
//
// A rebuild that renames the identifiers does NOT break this guard (the
// derivation follows the names), but a rebuild that changes the SHAPE does
// — and then the derivation has to be made again by hand.
func analyzeCockpitScene(bundle string) (cockpitSceneFacts, []string) {
	var facts cockpitSceneFacts
	var violations []string

	// 1. The component's `members` prop.
	props := reCockpitPropsMembers.FindAllStringSubmatch(bundle, -1)
	switch len(props) {
	case 0:
		violations = append(violations,
			"the bundle has no component that takes a `members` prop: the 3D scene's device count cannot come from the roster")
		return facts, violations
	case 1:
		facts.Component = props[0][1]
		facts.MembersProp = props[0][2]
	default:
		violations = append(violations, fmt.Sprintf(
			"the bundle has %d components that take a `members` prop; the scene's device count source is ambiguous",
			len(props)))
		return facts, violations
	}

	// 2. The place the 3D device nodes are created — there must be exactly
	// one, or some of them are not the roster's.
	nodes := reCockpitDeviceNode.FindAllStringSubmatchIndex(bundle, -1)
	switch len(nodes) {
	case 0:
		violations = append(violations,
			"the bundle creates no 3D device node at all: the scene draws no device, whatever the roster says")
		return facts, violations
	case 1:
	default:
		violations = append(violations, fmt.Sprintf(
			"the bundle creates 3D device nodes in %d places; want 1 (the roster's members)", len(nodes)))
		return facts, violations
	}
	facts.NodeSiteAt = nodes[0][0]

	// 3. The loop the node is created in. It must be the loop whose BODY
	// contains the node — the innermost one. (Taking the last `.forEach(`
	// before the node is not the same thing: a doctored bundle can put a
	// derived call in between, and the wrong loop would then be read.)
	loopDot, _, _, ok := jsEnclosingForEach(bundle, facts.NodeSiteAt)
	if !ok {
		violations = append(violations,
			"the 3D device node is not created inside a loop over an array: the scene's device count is not the array's length")
		return facts, violations
	}
	facts.LoopAt = loopDot

	// 4. WHAT the loop iterates. This is where the fakes get caught:
	//
	//	`t.forEach(…)`      — the roster itself.
	//	`t.filter(…).forEach(…)`   — only the online ones: a different
	//	                        number of devices than the roster has.
	//	`t.slice(…).forEach(…)`    — fewer devices than the roster has.
	//	`t.concat([…]).forEach(…)` — a fabricated machine among the real
	//	                        ones.
	//	`[...t].forEach(…)`        — a copy, not the roster.
	//
	// All of them are "the loop iterates something that is not the roster",
	// so the guard asks for the iterated expression to be a plain
	// identifier and for that identifier to be bound to the roster itself.
	iterated, iterStart, ok := jsIdentifierBefore(bundle, loopDot)
	if !ok {
		violations = append(violations, fmt.Sprintf(
			"the array the 3D scene iterates is not a plain identifier (it ends at offset %d): "+
				"it is a call or a derived value, so the scene draws a number of devices the roster does not have",
			loopDot))
		return facts, violations
	}
	facts.Iterated = iterated
	if prev := jsCharBefore(bundle, iterStart); prev == '.' {
		violations = append(violations, fmt.Sprintf(
			"the 3D scene iterates %q, which is a member expression, not the roster itself: "+
				"the scene would draw a number of devices the roster does not have",
			iterated))
		return facts, violations
	}

	// 5. What that identifier was bound to. A DIRECT reference to the
	// members prop is the only thing that counts: `let M=t`. Anything that
	// transforms the array on the way (`t.filter(…)`, `t.slice(…)`,
	// `t.concat(…)`, `[…t]`) is a different number of devices.
	binding, ok := jsLastBinding(bundle, iterated, loopDot)
	if !ok {
		violations = append(violations, fmt.Sprintf(
			"the array the 3D scene iterates (%q) is never bound to anything before the loop: "+
				"the scene's device count cannot be traced to the roster", iterated))
		return facts, violations
	}
	facts.BindingExpr = binding
	if binding != facts.MembersProp {
		violations = append(violations, fmt.Sprintf(
			"the 3D scene iterates %q, which is bound to %q and not to the roster (%q): "+
				"the scene would draw a number of devices the roster does not have",
			iterated, binding, facts.MembersProp))
		return facts, violations
	}

	// 6. The count the panel publishes must be the length of the member
	// array — a plain `.length`, not a filtered subset.
	counts := reCockpitSceneCount.FindAllStringSubmatchIndex(bundle, -1)
	switch len(counts) {
	case 0:
		violations = append(violations,
			"the 3D view does not publish its device count (no `三维名册视图：N 台设备` label): "+
				"nobody can check the scene's device count against the roster")
		return facts, violations
	case 1:
	default:
		violations = append(violations, fmt.Sprintf(
			"the bundle publishes %d device counts for the 3D view; want 1", len(counts)))
		return facts, violations
	}
	facts.CountArray = bundle[counts[0][2]:counts[0][3]]
	if facts.CountArray != facts.MembersProp {
		violations = append(violations, fmt.Sprintf(
			"the 3D view publishes the device count of %q, but the scene draws the members of %q: "+
				"the two numbers are not the same number",
			facts.CountArray, facts.MembersProp))
	}
	// The counted array must look like the roster: the same array is
	// filtered by liveness and the result is mapped to device names.
	countWindow := bundle[max(0, counts[0][0]-1200):min(len(bundle), counts[0][0]+3000)]
	rosterShaped := regexp.MustCompile(regexp.QuoteMeta(facts.CountArray) + `\.filter\([^)]*\)\.map\(`)
	if !strings.Contains(countWindow, facts.CountArray+".filter(") || !rosterShaped.MatchString(countWindow) {
		violations = append(violations, fmt.Sprintf(
			"the array the 3D view counts (%q) is not shaped like the roster: "+
				"it is neither filtered by liveness nor mapped to device names", facts.CountArray))
	}

	// 7. The array the cockpit is handed must be the API payload's
	// `members` field.
	payloads := reCockpitEmptyPayload.FindAllStringSubmatch(bundle, -1)
	if len(payloads) != 1 {
		violations = append(violations, fmt.Sprintf(
			"the bundle has %d empty roster payloads; the state that holds /api/mesh cannot be pinned", len(payloads)))
		return facts, violations
	}
	payloadVar := payloads[0][1]
	stateVar, setter := "", ""
	for _, m := range reCockpitUseState.FindAllStringSubmatch(bundle, -1) {
		if m[3] == payloadVar {
			stateVar, setter = m[1], m[2]
		}
	}
	if stateVar == "" {
		violations = append(violations, fmt.Sprintf(
			"no state holds the roster payload %q; the cockpit's members array cannot be traced to /api/mesh", payloadVar))
		return facts, violations
	}
	facts.StateVar = stateVar
	callSite := regexp.MustCompile(regexp.QuoteMeta(facts.Component) + `,\{[^}]{0,800}?members:` +
		regexp.QuoteMeta(stateVar) + `\.members\b`)
	if !callSite.MatchString(bundle) {
		violations = append(violations, fmt.Sprintf(
			"the cockpit (%s) is not handed `%s.members`: the scene's devices do not come from the API payload",
			facts.Component, stateVar))
	}
	if !reCockpitJSONBody.MatchString(bundle) {
		violations = append(violations,
			"the app does not read the /api/mesh body as it is (`await res.json()`): "+
				"the members array the scene draws cannot be the payload's")
	}
	fetch := reCockpitFetchData.FindAllStringSubmatch(bundle, -1)
	if len(fetch) != 1 {
		violations = append(violations, fmt.Sprintf(
			"the bundle has %d places that take the /api/mesh result; want 1", len(fetch)))
		return facts, violations
	}
	if !regexp.MustCompile(regexp.QuoteMeta(setter) + `\(` + regexp.QuoteMeta(fetch[0][1]) + `\)`).MatchString(bundle) {
		violations = append(violations, fmt.Sprintf(
			"the state that holds the roster is not filled with the /api/mesh result "+
				"(%s is never called with %s)", setter, fetch[0][1]))
	}

	return facts, violations
}

// jsEnclosingForEach finds the innermost `.forEach(` whose parentheses
// contain the given offset, and returns the offset of the dot, the offset
// of the opening parenthesis and the offset of the closing one.
//
// The innermost one is the loop whose BODY the offset is in. Taking the
// last `.forEach(` before the offset instead would let a derived call
// (`M.concat([…]).forEach(…)`) hide the real loop.
func jsEnclosingForEach(text string, site int) (dot, open, close int, ok bool) {
	for _, m := range reCockpitForEach.FindAllStringIndex(text, -1) {
		op := m[1] - 1 // the '(' — the match ends right after it
		cl := jsMatchingParen(text, op)
		if cl < 0 {
			continue
		}
		if op < site && site < cl {
			if open == 0 || op > open {
				dot, open, close = m[0], op, cl
				ok = true
			}
		}
	}
	return dot, open, close, ok
}

// jsIdentifierBefore returns the identifier that ends just before the
// given offset, and the offset where it starts. It fails when the
// character before the offset is not part of an identifier — which is how
// a call (`…).forEach(`) is told apart from a plain reference
// (`t.forEach(`).
func jsIdentifierBefore(text string, end int) (string, int, bool) {
	i := end
	for i > 0 && isJSIdentChar(text[i-1]) {
		i--
	}
	if i == end {
		return "", end, false
	}
	return text[i:end], i, true
}

// jsCharBefore returns the last non-space character before the offset.
func jsCharBefore(text string, at int) byte {
	for i := at - 1; i >= 0; i-- {
		c := text[i]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return c
		}
	}
	return 0
}

// jsLastBinding returns the expression the given identifier was last bound
// to before the given offset.
//
// The LAST binding is the interesting one: a reassignment between the
// declaration and the loop (`M=t.filter(…)`) is exactly the kind of
// transformation that changes the device count.
func jsLastBinding(text string, name string, before int) (string, bool) {
	start, ok := jsLastBindingAt(text, name, before)
	if !ok {
		return "", false
	}
	expr, _ := jsExpressionUntil(text, start)
	return expr, true
}

// jsLastBindingAt returns the offset where the expression of the last
// binding of the given identifier before the given offset starts.
//
// "Last" is the one that counts: a binding BETWEEN the declaration and the
// loop (`M=t.filter(…)` where `M` was declared earlier) is exactly the
// transformation that changes the device count, so the closest one wins.
//
// The pattern requires the binding to sit at a statement boundary and to
// be a declaration or a plain assignment — never a member assignment
// (`a.M=…`), which would bind something else entirely.
func jsLastBindingAt(text string, name string, before int) (int, bool) {
	decl := regexp.MustCompile(`(?:^|[;,{(])[ \t]*(?:let[ \t]+|const[ \t]+|var[ \t]+)?` +
		regexp.QuoteMeta(name) + `[ \t]*=[ \t]*`)
	last := -1
	for _, m := range decl.FindAllStringIndex(text[:before], -1) {
		last = m[1]
	}
	if last < 0 {
		return 0, false
	}
	return last, true
}

// jsExpressionUntil reads the expression that starts at the given offset,
// up to the first `,` or `;` at depth zero (or the end of the text).
func jsExpressionUntil(text string, start int) (string, int) {
	depth := 0
	i := start
	for i < len(text) {
		switch c := text[i]; c {
		case '"', '\'':
			i = jsSkipString(text, i)
			continue
		case '`':
			i = jsSkipTemplate(text, i)
			continue
		case '(', '[', '{':
			depth++
		case ')', ']':
			depth--
		case '}':
			if depth == 0 {
				return strings.TrimSpace(text[start:i]), i
			}
			depth--
		case ',', ';':
			if depth == 0 {
				return strings.TrimSpace(text[start:i]), i
			}
		}
		i++
	}
	return strings.TrimSpace(text[start:]), i
}

// jsMatchingParen returns the offset of the parenthesis that closes the
// one at the given offset, or -1. Strings and template literals are
// skipped, so a bracket inside them does not count.
func jsMatchingParen(text string, open int) int {
	depth := 0
	i := open
	for i < len(text) {
		switch c := text[i]; c {
		case '"', '\'':
			i = jsSkipString(text, i)
			continue
		case '`':
			i = jsSkipTemplate(text, i)
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

// jsSkipString returns the offset just after the string that starts at
// the given offset.
func jsSkipString(text string, i int) int {
	q := text[i]
	i++
	for i < len(text) {
		switch text[i] {
		case '\\':
			i += 2
			continue
		case q:
			return i + 1
		}
		i++
	}
	return i
}

// jsSkipTemplate returns the offset just after the template literal that
// starts at the given offset. A `${…}` interpolation is skipped with its
// own braces.
func jsSkipTemplate(text string, i int) int {
	i++
	for i < len(text) {
		switch text[i] {
		case '\\':
			i += 2
			continue
		case '`':
			return i + 1
		case '$':
			if i+1 < len(text) && text[i+1] == '{' {
				i = jsSkipBraces(text, i+1)
				continue
			}
			i++
			continue
		default:
			i++
		}
	}
	return i
}

// jsSkipBraces returns the offset just after the `}` that closes the `{`
// at the given offset.
func jsSkipBraces(text string, i int) int {
	depth := 0
	for i < len(text) {
		switch c := text[i]; c {
		case '"', '\'':
			i = jsSkipString(text, i)
			continue
		case '`':
			i = jsSkipTemplate(text, i)
			continue
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return i
}

// isJSIdentChar reports whether the byte is part of a JavaScript
// identifier.
func isJSIdentChar(c byte) bool {
	return c == '$' || c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// TestCockpitSceneDrawsOneNodePerRosterMember is the D4 guard: the 3D
// scene's device count must be the roster's member count.
//
// It reads the SHIPPED artifact — the same bytes the binary carries — and
// refuses to pass when any of the links is missing.
//
// ★ WHAT IT PROVES: that in this artifact the array the scene iterates is
// the very identifier the cockpit received as its `members` prop, and that
// the device count the panel publishes is that identifier's `.length`. That
// is a statement about the SHAPE of the code, not a proof that the drawn
// node count equals the roster length — the rendered scene still has to be
// looked at by a human (the panel has no test runner).
func TestCockpitSceneDrawsOneNodePerRosterMember(t *testing.T) {
	files := cockpitArtifactFiles(t)
	var bundle []byte
	for _, f := range files {
		if strings.HasSuffix(f.name, ".js") {
			bundle = f.data
			break
		}
	}
	if bundle == nil {
		t.Fatal("the embedded cockpit has no JavaScript bundle: the scene's device count cannot be read")
	}

	facts, violations := analyzeCockpitScene(string(bundle))
	for _, v := range violations {
		t.Errorf("the 3D scene's device count is not the roster's: %s", v)
	}
	if len(violations) > 0 {
		return
	}
	if facts.BindingExpr != facts.MembersProp {
		t.Errorf("the scene iterates %q, which is bound to %q and not to the roster: "+
			"the scene's device count and the roster's member count are two different numbers",
			facts.Iterated, facts.BindingExpr)
	}
	if facts.CountArray != facts.MembersProp {
		t.Errorf("the panel publishes the device count of %q but the scene draws %q: "+
			"the published number and the drawn number are not the same number",
			facts.CountArray, facts.Iterated)
	}
}

// TestCockpitSceneCountGuardRejectsAFakedRoster proves the guard above can
// fail. A guard that cannot go red is decoration, so the ways the product
// could lie are doctored into the shipped bundle here and each one has to
// be caught FOR THE RIGHT REASON:
//
//   - the scene draws only the ONLINE devices (a subset of the roster:
//     the drawn count is not the roster's length);
//   - the scene draws fewer devices than the roster has (a slice);
//   - the scene draws an extra machine that is not in the roster (the
//     defect of round 58: a DEFAULT-ON toggle that appended two invented
//     Hong Kong devices to the live roster);
//   - the panel publishes a device count that is not the roster's length;
//   - the cockpit is handed something other than the payload's members.
//
// The first three are the ESCAPE CASES: the earlier version of this guard
// read "the loop's receiver is the members prop" off a prefix match, and
// `t.filter(…)`, `t.slice(…)` and `t.concat([…])` all passed that match —
// so the guard was green while the scene lied. They are doctored here on
// the exact text of the shipped bundle, so a future edit to the guard
// cannot quietly reopen that hole.
func TestCockpitSceneCountGuardRejectsAFakedRoster(t *testing.T) {
	files := cockpitArtifactFiles(t)
	var bundle string
	for _, f := range files {
		if strings.HasSuffix(f.name, ".js") {
			bundle = string(f.data)
			break
		}
	}
	if bundle == "" {
		t.Fatal("the embedded cockpit has no JavaScript bundle")
	}

	// The unmodified bundle must pass, or the doctoring below proves
	// nothing.
	if _, violations := analyzeCockpitScene(bundle); len(violations) > 0 {
		t.Fatalf("the shipped bundle does not pass the scene-count guard: %v", violations)
	}

	cases := []struct {
		name  string
		wants string
	}{
		{
			name:  "the scene draws only the online devices",
			wants: "not to the roster",
		},
		{
			name:  "the scene draws fewer devices than the roster has",
			wants: "not to the roster",
		},
		{
			name:  "extra machines in the scene",
			wants: "not to the roster",
		},
		{
			name:  "a published count that is not the roster's length",
			wants: "device count",
		},
		{
			name:  "a cockpit that is not handed the payload's members",
			wants: "is not handed",
		},
	}

	// The cases are doctored on the exact text of the shipped bundle.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doctored, ok := doctorBundle(bundle, c.name)
			if !ok {
				t.Skipf("the shipped bundle no longer has the text this case doctored: nothing to doctor")
			}
			_, violations := analyzeCockpitScene(doctored)
			if len(violations) == 0 {
				t.Fatalf("the guard accepted a bundle with %q; a faked roster must be caught", c.name)
			}
			joined := strings.Join(violations, "; ")
			if !strings.Contains(joined, c.wants) {
				t.Errorf("the guard caught the doctored bundle, but not for the expected reason: %s", joined)
			}
		})
	}
}

// doctorBundle rewrites one piece of the shipped bundle so that the
// doctored copy breaks one of the links.
//
// Every case is a REPLACEMENT of the binding that feeds the scene's device
// loop — the place where a filtered, a sliced or an extended roster would
// have to be introduced.
func doctorBundle(bundle, which string) (string, bool) {
	facts, violations := analyzeCockpitScene(bundle)
	if len(violations) > 0 {
		return "", false
	}
	switch which {
	case "the scene draws only the online devices":
		return doctorSceneBinding(bundle, facts, facts.MembersProp+".filter(e=>e.online)")
	case "the scene draws fewer devices than the roster has":
		return doctorSceneBinding(bundle, facts, facts.MembersProp+".slice(0,1)")
	case "extra machines in the scene":
		return doctorSceneBinding(bundle, facts, facts.MembersProp+".concat([{device_id:`fabricated`}])")
	case "a published count that is not the roster's length":
		old := "三维名册视图：${"
		if !strings.Contains(bundle, old) {
			return "", false
		}
		return strings.Replace(bundle, old, "三维名册视图：${99+", 1), true
	case "a cockpit that is not handed the payload's members":
		old := "members:" + facts.StateVar + ".members"
		if !strings.Contains(bundle, old) {
			return "", false
		}
		return strings.Replace(bundle, old, "members:"+facts.StateVar+".other_members", 1), true
	}
	return "", false
}

// doctorSceneBinding replaces the expression the scene's iterated
// identifier is bound to, keeping the identifier and the loop as they are.
func doctorSceneBinding(bundle string, facts cockpitSceneFacts, expr string) (string, bool) {
	start, ok := jsLastBindingAt(bundle, facts.Iterated, facts.LoopAt)
	if !ok {
		return "", false
	}
	_, end := jsExpressionUntil(bundle, start)
	return bundle[:start] + expr + bundle[end:], true
}

// TestCockpitSceneDeviceCountMatchesTheRosterMembers is the runtime half
// of D4: the payload the anchor serves carries one row per roster member,
// so the scene — which draws one node per payload member — draws the
// network's real device count.
func TestCockpitSceneDeviceCountMatchesTheRosterMembers(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	want := map[string]bool{}
	for i := 0; i < 3; i++ {
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
		want[string(dev.ID())] = true
	}

	srv := New(Config{PanelToken: "tok"})
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
	var payload MeshResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}

	// The number the 3D scene draws is the length of `members`.
	if len(payload.Members) != len(want) {
		t.Fatalf("the payload carries %d members, want %d: the scene would draw %d devices for a network of %d",
			len(payload.Members), len(want), len(payload.Members), len(want))
	}
	if payload.Summary.Members != len(want) {
		t.Fatalf("summary.members = %d, want %d: the panel's headline number and the scene's device count must agree",
			payload.Summary.Members, len(want))
	}
	// And they must be the roster's devices — not a subset, not extras.
	for _, m := range payload.Members {
		if !want[m.DeviceID] {
			t.Errorf("the payload carries %s, which is not in the roster: the scene would draw a machine that does not exist",
				m.DeviceID)
		}
	}
	// Every member sits in the product's own zone, so the scene puts them
	// all in one place — the honest picture of a self-hosted network.
	for _, m := range payload.Members {
		if m.ZoneID != cockpitZoneID {
			t.Errorf("member %s is in zone %q, want %q: the scene would spread the network over places it never collected",
				m.DeviceID, m.ZoneID, cockpitZoneID)
		}
	}
}
