// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestMemberHasNoEndpointsField pins that the member list does NOT carry
// a field for a member's addresses.
//
// It used to: Member.Endpoints was declared and NEVER written. The only two
// mentions in the whole tree were the declaration and the renewal path
// copying the old (also empty) value forward.
//
// A field that looks like it holds the addresses but never does is worse
// than no field at all. A reader would conclude "this member has no
// endpoints, so it cannot be dialed" — wrong for every member that
// announced one. The addresses live in the discovery node's own view
// (discovery.Node.endpts) and reach the clients through the verified
// snapshot.
//
// The check is on the Go TYPE, not on the JSON: the field carried
// `omitempty`, so an empty one never reached the wire and a JSON-level
// check would pass even with the field present. A field that cannot hold
// anything is the thing being removed, and only the type can say so.
func TestMemberHasNoEndpointsField(t *testing.T) {
	// The reflection check: no field may be named "endpoints" (or any
	// near-spelling of it).
	ty := reflect.TypeOf(Member{})
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		if strings.Contains(strings.ToLower(name), "endpoint") ||
			strings.Contains(strings.ToLower(name), "addr") {
			t.Fatalf("the member type carries a %q field again: %v", name, ty)
		}
	}
	// And the field count, so a rename cannot smuggle one back in.
	if n := ty.NumField(); n != 3 {
		t.Fatalf("the member type has %d fields, want 3 (cert, added_at, last_seen): %v", n, ty)
	}
}

// TestMemberListKeepsNoAddressState pins the same property at the list
// level: adding a member and renewing its certificate must not leave any
// address state behind in the member entries.
//
// The member list is the IDENTITY layer: who belongs, not where they are
// right now. Reachability is the discovery layer's business.
func TestMemberListKeepsNoAddressState(t *testing.T) {
	acc, err := GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	list := NewMemberList(acc.ID())
	dev, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := list.Add(*cert); err != nil {
		t.Fatal(err)
	}
	m, ok := list.Get(dev.ID())
	if !ok {
		t.Fatal("the member is not in the list")
	}
	// The member entry carries the certificate and the timestamps — and
	// nothing else. There is no field to check, which is the point: the
	// type itself has no place to put an address.
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"endpoints", "endpoint", "addresses", "addrs"} {
		if _, ok := decoded[key]; ok {
			t.Fatalf("the member entry carries a %q field: %s", key, b)
		}
	}
	if _, ok := decoded["cert"]; !ok {
		t.Fatalf("the member entry lost its certificate: %s", b)
	}
}
