// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cmeshtest

import "testing"

// TestProductionBuildHasNoEscapeHatch is the guard on the guard.
//
// The end-to-end suite needs the outbound address policy switched off,
// because it runs the product against a listener on 127.0.0.1. The escape
// hatch is therefore behind a build tag. If that tag ever leaked into the
// release build, the shipped binary would accept a discovery origin
// pointing at the operator's own network.
//
// This test runs in the default (untagged) build, which is what a release
// build compiles, and requires the hatch to be off.
func TestProductionBuildHasNoEscapeHatch(t *testing.T) {
	if Enabled {
		t.Fatal("this binary was built with the cmeshtest tag: the shipped " +
			"binary must never carry the outbound-address escape hatch")
	}
}
