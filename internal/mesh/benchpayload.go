// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

// ★ THE BENCH PAYLOAD IS CHECKABLE, NOT JUST COUNTABLE.
//
// The bench used to send zeros and count what came back. That makes
// "the bytes arrived intact" unprovable: a transfer that arrives with
// the WRONG bytes and the RIGHT count is counted as a success, and the
// acceptance cell (B10) says the bytes must arrive intact.
//
// The payload is now a deterministic function of its offset, so the
// receiver can check each byte against the offset it expects, and the
// two sides agree by construction — no digest is exchanged and no
// round trip is added.
//
// The cost is one pass over the payload on each side, and a bench is
// a throughput tool: the check must not be the thing that limits it.

// benchPatternByte returns the byte the bench payload holds at the
// given offset.
//
// It must not be zero everywhere (that was the old payload) and it
// must vary with the offset (otherwise a truncation that happened to
// land on a boundary would pass).
func benchPatternByte(offset int64) byte {
	// A cheap multiplicative hash: the byte depends on the offset and
	// on NOTHING else, so both sides compute the same value.
	x := uint64(offset)*0x9E3779B97F4A7C15 + 0x165667B19E3779F9
	x ^= x >> 29
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 32
	return byte(x)
}

// benchFillPattern fills buf with the pattern starting at the given
// absolute offset.
//
// ★ IT IS A FUNCTION, NOT A SWAPPED VARIABLE. (Round 108.)
//
// It used to be a `var` so that a test could substitute it and corrupt
// the payload on the wire. But the corruption probe swapped it while
// ANOTHER test's sender goroutine was reading it, and the race
// detector caught it on Linux:
//
//	WARNING: DATA RACE
//	testing.go:1490: race detected during execution of test
//
// ⇒ the probe went red on a healthy tree, for a reason that had
// nothing to do with what it was testing.
//
// The corruption now travels with the one bench it belongs to
// (BenchConfig.CorruptPayload), so NOTHING has to be swapped globally
// and there is nothing for a concurrent sender to race with.
func benchFillPattern(buf []byte, offset int64) {
	for i := range buf {
		buf[i] = benchPatternByte(offset + int64(i))
	}
}

// benchPayloadMatches reports whether the payload is exactly the
// pattern that belongs at the given offset.
func benchPayloadMatches(payload []byte, offset int64) bool {
	for i, b := range payload {
		if b != benchPatternByte(offset+int64(i)) {
			return false
		}
	}
	return true
}
