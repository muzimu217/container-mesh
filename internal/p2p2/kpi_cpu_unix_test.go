// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build !windows

package p2p2

import (
	"syscall"
	"time"
)

// processCPU returns CPU time consumed by this process so far.
//
// It is the KPI harness's clock. Wall-clock throughput on a laptop that is also
// running an editor is not measurable to better than ±30% — the same
// configuration measured 49.9 and 250.7 MiB/s in one session — while CPU time is
// the work the process actually did and is unaffected by another process taking
// cycles. Since batching exists to remove *work* (syscalls), CPU seconds per
// transfer is the metric that responds to the change and does not need an idle
// machine.
//
// Both endpoints of the benchmark run in this process, so RUSAGE_SELF covers the
// whole transfer.
func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// cpuTimingSupported reports whether processCPU returns a usable value.
const cpuTimingSupported = true
