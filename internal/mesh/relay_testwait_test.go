// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import "time"

// ---------------------------------------------------------------------------
// THE HANDSHAKE WAIT BUDGET. (Round 99.)
//
// The relay tests wait for the TARGET to receive the relayed session. That
// wait is a WALL-CLOCK DEADLINE on a live QUIC handshake, and a
// wall-clock deadline is a statement about the machine as much as about
// the code.
//
// The budgets were inconsistent too — 15 seconds in one file, 20 in
// others — and one of them timed out under the load of the whole package
// running with -race: the test took 45.90 s and died on "the target
// never received the relayed session", while the SAME test passed in
// 1.11 s when run alone.
//
// So: ONE budget, for every relay test, and a generous one.
//
// Why generous is right here: the wait is not measuring latency, it is
// waiting for a handshake to COMPLETE. A test that fails because the
// machine was busy has not found a defect; it has found that it asked a
// timing question and got a timing answer. The latency numbers live in
// the measurement tests, which report rather than assert.
//
// ★ AND THE SAME RULE GOES FOR THE HANDSHAKE'S OWN TIMEOUTS: they are
// BOUNDS, not targets. A handshake that takes longer than the bound is a
// failure worth reporting; a handshake that takes longer than a test's
// impatience is not.
// ---------------------------------------------------------------------------

// relayHandshakeWait is how long a relay test waits for the target to
// receive a relayed session.
//
// It is deliberately generous: the wait is for a handshake to complete,
// not for it to be fast.
const relayHandshakeWait = 60 * time.Second
