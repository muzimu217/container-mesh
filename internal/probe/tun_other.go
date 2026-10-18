//go:build !linux

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package probe

import (
	"os"
	"runtime"
)

// probeTUN on non-Linux platforms: the TUNSETIFF ioctl is Linux specific.
// On macOS the equivalent is a utun control socket, which is a different
// mechanism and out of scope for the target containers. We still report the
// device existence so the JSON is informative.
func probeTUN(opts Options) TUNReport {
	r := TUNReport{DevicePath: opts.TUNDevice}
	if _, err := os.Stat(opts.TUNDevice); err == nil {
		r.DeviceExists = true
		r.OpenError = "TUNSETIFF is Linux-only; not attempted on " + runtime.GOOS
	} else {
		r.OpenError = err.Error()
	}
	return r
}
