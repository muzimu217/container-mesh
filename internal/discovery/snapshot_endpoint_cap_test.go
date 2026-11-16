// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"fmt"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestSnapshotEndpointCountIsCapped pins the endpoint count on the snapshot
// path.
//
// MaxEndpoints bounds the endpoint list on the announce path and on the
// invite path, but a snapshot from the discovery node was not bounded at
// all: the node is trusted to be online, not to be honest, and it can put
// as many endpoints as it likes into one member's row.
//
// The cost is not the list itself but what the list causes: every endpoint
// is dialled in turn, and each attempt costs a handshake timeout. A
// snapshot carrying five hundred endpoints was measured being accepted
// whole, and a probe with a three second budget ran for over five
// seconds.
func TestSnapshotEndpointCountIsCapped(t *testing.T) {
	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// A member with far more endpoints than the cap allows.
	tooMany := make([]string, 0, MaxEndpoints*10)
	for i := 0; i < MaxEndpoints*10; i++ {
		tooMany = append(tooMany, fmt.Sprintf("198.51.100.%d:7000", i%254))
	}

	snap := &MemberSnapshot{
		AccountID: acc.ID(),
		Version:   1,
		Members:   map[identity.DeviceID]MemberView{},
	}
	snap.Members[dev.ID()] = MemberView{
		Cert:      *cert,
		Online:    true,
		Endpoints: tooMany,
	}

	cl := clientForAccount(t, acc)
	if err := cl.verifySnapshot(snap); err != nil {
		t.Fatalf("the snapshot was rejected: %v", err)
	}

	got := len(snap.Members[dev.ID()].Endpoints)
	if got > MaxEndpoints {
		t.Fatalf("the snapshot kept %d endpoints, want at most %d: an unbounded "+
			"list makes the dial cost grow with whatever an untrusted node "+
			"puts in it", got, MaxEndpoints)
	}
}
