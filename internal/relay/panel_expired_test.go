package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestThePanelCountsTheSameMembersTheMembersSee is the regression test
// for the defect the real-machine review found.
//
// The panel counted every roster entry, INCLUDING the ones whose
// certificate had lapsed. The member view does not: the snapshot leaves
// expired certificates out, because a lapsed certificate proves nothing.
//
// So the two numbers disagreed, and they disagreed by a lot. Measured on
// the real machine: the panel said the network had 51 members while the
// members' own view said 23 — twenty-eight of the roster's certificates
// had expired.
//
// The operator asks the panel how big the network is, and that is the
// number they get. The two views must not tell different stories.
func TestThePanelCountsTheSameMembersTheMembersSee(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())

	// One member with a valid certificate.
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

	// One whose certificate has LAPSED.
	old, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	expired, err := acc.IssueCert(old, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expired.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	if err := members.Add(*expired); err != nil {
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
	var data PanelData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}

	if data.Summary.Members != 1 {
		t.Fatalf("summary.members = %d, want 1: an expired certificate is not a member "+
			"any more, and the members' own view already leaves it out", data.Summary.Members)
	}
	if len(data.Members) != 1 {
		t.Fatalf("the payload lists %d members, want 1", len(data.Members))
	}
}
