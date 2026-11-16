// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build cmeshtest

// Package cmeshtest provides the test-only escape hatch from the outbound
// address policy.
//
// The policy refuses a discovery address that resolves to a loopback,
// private or reserved address. That is right in production and wrong for
// the end-to-end suite, which runs the whole product against a listener on
// 127.0.0.1 through the logical name cmesh-test.127.0.0.1.nip.io.
//
// This file carries the build tag, so it is compiled into the binary only
// when the tag is set. The production binary has no such escape: the tag
// is never set by the release build, the CI build, or `go build ./cmd/cm`.
//
// The shell suite builds its binary with -tags cmeshtest, so the escape
// hatch exists only in a binary that exists only for the test run.
package cmeshtest

// Enabled reports whether this binary was built with the cmeshtest tag.
const Enabled = true
