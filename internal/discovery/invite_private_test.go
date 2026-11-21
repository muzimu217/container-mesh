package discovery

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestAnInviteWithAPrivateAddressNeedsTheSwitch is the regression test
// for the defect the acceptance-list review found.
//
// The private-deployment switch (CMESH_ALLOW_PRIVATE_DISCOVERY) relaxes
// the outbound address policy so a LAN anchor can be reached. It applied
// to the client's OWN configuration but NOT to the addresses inside an
// INVITE, because the invite was decoded through the unswitchable form.
//
// The consequence was a private deployment that was half-broken: the
// switch was exported, the credential join worked, and the INVITE join
// still refused the very address the operator had just configured —
// with an error that pointed at the invite's shape rather than at the
// switch that would have allowed it.
//
// The two inputs are genuinely different (the client's configuration
// comes from the operator; an invite can be pasted anywhere), so the
// invite keeps the strict policy by DEFAULT and the operator's explicit
// switch is what relaxes it.
func TestAnInviteWithAPrivateAddressNeedsTheSwitch(t *testing.T) {
	inv := Invite{
		AccountID:      identity.DeviceID("acctacctacctacct"),
		AccountPub:     make([]byte, 32),
		DiscoveryAddrs: []string{"127.0.0.1:18090"},
		IssuedAt:       time.Now().UTC(),
	}
	s, err := EncodeInvite(inv)
	if err != nil {
		t.Fatal(err)
	}

	// Without the switch: refused, and the error must SAY that the
	// address is the problem (not "the invite is malformed").
	_, err = DecodeInvite(s)
	if err == nil {
		t.Fatal("an invite naming a private address was accepted without the switch")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:18090") {
		t.Fatalf("the error does not name the offending address: %v", err)
	}

	// With the switch: accepted.
	got, err := DecodeInviteWith(s, true)
	if err != nil {
		t.Fatalf("the invite was refused even with the private-deployment switch on: %v", err)
	}
	if len(got.DiscoveryAddrs) != 1 || got.DiscoveryAddrs[0] != "127.0.0.1:18090" {
		t.Fatalf("the decoded invite carries %v", got.DiscoveryAddrs)
	}
}

// TestAPublicAddressNeedsNoSwitch: the switch must not change the
// behaviour for a public address — the common case has to keep working
// exactly as before.
func TestAPublicAddressNeedsNoSwitch(t *testing.T) {
	inv := Invite{
		AccountID:      identity.DeviceID("acctacctacctacct"),
		AccountPub:     make([]byte, 32),
		DiscoveryAddrs: []string{"175.24.139.148:18090"},
		IssuedAt:       time.Now().UTC(),
	}
	s, err := EncodeInvite(inv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeInvite(s); err != nil {
		t.Fatalf("a public invite was refused: %v", err)
	}
	if _, err := DecodeInviteWith(s, true); err != nil {
		t.Fatalf("a public invite was refused with the switch on: %v", err)
	}
}

// TestTheSwitchDoesNotOpenTheReservedRanges: the switch relaxes PRIVATE
// addresses only. The reserved ranges (TEST-NET, the benchmark range,
// 0.0.0.0, 240/4) must stay refused even with it on — they are never a
// legitimate anchor.
func TestTheSwitchDoesNotOpenTheReservedRanges(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:18090",
		"198.51.100.1:18090",
		"203.0.113.1:18090",
		"192.0.2.1:18090",
		"240.0.0.1:18090",
	} {
		inv := Invite{
			AccountID:      identity.DeviceID("acctacctacctacct"),
			AccountPub:     make([]byte, 32),
			DiscoveryAddrs: []string{addr},
			IssuedAt:       time.Now().UTC(),
		}
		s, err := EncodeInvite(inv)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeInviteWith(s, true); err == nil {
			t.Fatalf("the reserved address %s was accepted with the private switch on", addr)
		}
	}
}

// TestALocalhostNameIsStillRefused: the switch relaxes the LITERAL
// private addresses. A NAME that resolves to a private address is a
// different thing (it is what the egress policy's resolved-address half
// is for), and the invite's own validation must not open it.
func TestALocalhostNameIsStillRefused(t *testing.T) {
	inv := Invite{
		AccountID:      identity.DeviceID("acctacctacctacct"),
		AccountPub:     make([]byte, 32),
		DiscoveryAddrs: []string{"localhost:18090"},
		IssuedAt:       time.Now().UTC(),
	}
	s, err := EncodeInvite(inv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeInviteWith(s, true); err == nil {
		t.Fatal("an invite naming localhost was accepted with the private switch on")
	}
	_ = url.Values{}
}
