// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build windows

package p2p2

import "time"

// processCPU is unavailable on Windows: the KPI harness's CPU-time attribution
// needs getrusage(2), and syscall.Getrusage does not exist there. The benchmarks
// skip rather than reporting a zero, which would look like an infinite speedup.
func processCPU() time.Duration { return 0 }

const cpuTimingSupported = false
