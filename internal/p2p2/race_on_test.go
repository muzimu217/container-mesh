// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build race

package p2p2

// raceEnabled reports whether the race detector is instrumenting this binary.
// Tests that assert on wall-clock throughput read it to skip their threshold:
// the instrumentation inflates the cost of the syscall-bound hot path by an
// order of magnitude, so the number is not comparable.
const raceEnabled = true
