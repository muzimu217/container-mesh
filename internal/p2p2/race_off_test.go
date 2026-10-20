// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build !race

package p2p2

// raceEnabled is false in a normal build; see race_on_test.go.
const raceEnabled = false
