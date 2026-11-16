// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build !cmeshtest

// Package cmeshtest provides the test-only escape hatch from the outbound
// address policy.
//
// This file is the default build: the escape hatch does not exist here.
package cmeshtest

// Enabled reports whether this binary was built with the cmeshtest tag.
const Enabled = false
