// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

// raceNote labels a wall-clock measurement with whether it is comparable.
//
// The honest label matters because these numbers are EVIDENCE, and
// evidence that is read out of context is worse than no evidence. Under
// the race detector the syscall-bound hot path of this relay is
// instrumented, and the instrumentation dominates every measurement:
// the same quota A/B that reads 73.9 vs 181.0 MiB/s in a normal build
// reads 53.23 vs 53.26 MiB/s (a 1.00x "separation") under `-race`.
//
// So a measurement made under the detector says so, and a reader knows
// not to compare it with a normal-build number.
//
// The same convention lives in internal/p2p2; see race_on_test.go there.
func raceNote() string {
	if raceEnabled {
		return " (MEASURED UNDER -race: the syscall-bound hot path is inflated by the " +
			"instrumentation, so these numbers are NOT comparable with a normal build)"
	}
	return ""
}
