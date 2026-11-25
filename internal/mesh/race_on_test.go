// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build race

package mesh

// raceEnabled reports whether the race detector is instrumenting this binary.
//
// Tests that measure wall-clock throughput read it to label their
// numbers: the instrumentation inflates the cost of the syscall-bound
// hot path by an order of magnitude, so the number is not comparable
// with a normal build.
//
// The convention is written down in docs/CONVENTIONS.md §2.2 (performance
// assertions must not be wall-clock thresholds).
const raceEnabled = true
