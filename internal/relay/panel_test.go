// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

// Tests for the panel's data surface (M4 (b)).
//
// The properties that matter:
//   - the roster is never served without the panel token (R-2.4);
//   - the payload matches the live state — online count follows reality,
//     and an offline member STAYS in the list (membership and liveness are
//     different things);
//   - the page itself carries no network data and no external resource.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

func panelStack(t *testing.T, token string) (*httptest.Server, *identity.Account, []*identity.Device, *identity.MemberList) {
	t.Helper()
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	var devs []*identity.Device
	for i := 0; i < 2; i++ {
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
		devs = append(devs, dev)
	}
	srv := New(Config{PanelToken: token, AnchorDeviceID: devs[0].ID()})
	srv.EnableDiscovery(members)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.StopDiscovery)
	return ts, acc, devs, members
}

// TestPanelDataRequiresToken: the roster must not leak to an unauthenticated
// caller, and the panel page itself must not carry any of it.
func TestPanelDataRequiresToken(t *testing.T) {
	ts, _, _, _ := panelStack(t, "s3cret-token")

	resp, err := http.Get(ts.URL + "/api/mesh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/mesh returned %d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "device_id") {
		t.Fatalf("the 401 body leaked roster data: %s", body)
	}

	// The page is a shell: it may be served, but it holds no state.
	page, err := http.Get(ts.URL + "/panel")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("GET /panel returned %d, want 200", page.StatusCode)
	}
	html, _ := io.ReadAll(page.Body)
	text := string(html)
	for _, forbidden := range []string{"http://", "https://", "cdn.", "googleapis"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("the panel page references an external resource (%q) — it must render fully offline", forbidden)
		}
	}
	if strings.Contains(text, "<script src=") || strings.Contains(text, "<link rel=\"stylesheet\"") {
		t.Fatal("the panel page loads external script or style resources")
	}
}

// TestPanelPayloadMatchesRoster: with the token, the payload lists every
// member and reports zero online until someone announces.
func TestPanelPayloadMatchesRoster(t *testing.T) {
	ts, acc, devs, members := panelStack(t, "tok")

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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated /api/mesh returned %d, want 200", resp.StatusCode)
	}
	var data PanelData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data.AccountID != acc.ID() {
		t.Fatalf("account id = %s, want %s", data.AccountID, acc.ID())
	}
	if data.Summary.Members != 2 {
		t.Fatalf("summary.members = %d, want 2", data.Summary.Members)
	}
	if len(data.Members) != 2 {
		t.Fatalf("payload has %d members, want 2", len(data.Members))
	}
	// Offline members STAY in the list — membership is not liveness.
	if data.Summary.Online != 0 {
		t.Fatalf("summary.online = %d before any announce, want 0", data.Summary.Online)
	}
	anchors := 0
	for _, m := range data.Members {
		if m.IsAnchor {
			anchors++
			if m.DeviceID != devs[0].ID() {
				t.Fatalf("anchor row is %s, want %s", m.DeviceID, devs[0].ID())
			}
		}
		if m.Endpoints == nil {
			t.Fatalf("member %s has nil endpoints; the panel must render an empty list, not null", m.DeviceID)
		}
	}
	if anchors != 1 {
		t.Fatalf("payload marks %d rows as the anchor, want 1", anchors)
	}
	_ = members
}

// TestPanelRejectsTokenInQueryString pins the round-56 security note: a
// token in the query string lands in the node's access log and in any
// Referer, so the data endpoint accepts the header form only.
func TestPanelRejectsTokenInQueryString(t *testing.T) {
	ts, _, _, _ := panelStack(t, "tok")

	resp, err := http.Get(ts.URL + "/api/mesh?token=tok")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a query-string token was accepted (HTTP %d); it must be refused", resp.StatusCode)
	}
}

// TestPanelShowsLastSeenForOfflineMember pins the round-56 observation: an
// offline member must still say when it was last around. The node forgets a
// device's presence when its stream detaches, so the timestamp has to
// survive that — in a map that is NOT the presence map.
func TestPanelShowsLastSeenForOfflineMember(t *testing.T) {
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
	dnode := srv.EnableDiscovery(members)
	live := httptest.NewServer(srv.Handler())
	defer live.Close()
	defer srv.StopDiscovery()

	// The member announces, then its stream dies: offline, but the panel
	// must still report when it was last seen.
	now := time.Now().UTC()
	dnode.Register(discovery.MemberAnnounce{
		DeviceID:  dev.ID(),
		Endpoints: []string{"203.0.113.9:7000"},
		Timestamp: now,
	}, now)
	dnode.AttachStream(dev.ID())
	dnode.DetachStream(dev.ID())

	req, err := http.NewRequest(http.MethodGet, live.URL+"/api/mesh", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Panel-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data PanelData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	var row *PanelMember
	for i := range data.Members {
		if data.Members[i].DeviceID == dev.ID() {
			row = &data.Members[i]
		}
	}
	if row == nil {
		t.Fatalf("the offline member is missing from the panel: %+v", data.Members)
	}
	if row.Online {
		t.Fatal("the detached member is reported as online")
	}
	if row.LastSeen == "" {
		t.Fatal("the offline member has no last_seen; the panel must say when it was last around")
	}
}

// TestPanelWithoutTokenMountsNothing: a node that was not given a token
// must not even mount the panel — the roster must be unreachable, not just
// unauthorized.
func TestPanelWithoutTokenMountsNothing(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	srv := New(DefaultConfig())
	srv.EnableDiscovery(members)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.StopDiscovery()

	for _, path := range []string{"/panel", "/api/mesh"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s is mounted on a node without a panel token", path)
		}
	}
}
