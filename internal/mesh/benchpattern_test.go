// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import "testing"

// The bench payload's pattern must have the properties the content
// check relies on. A pattern that did not would make the check
// vacuously true — it would accept a corrupted payload.

// The pattern must depend on the offset.
func TestTheBenchPatternVariesWithTheOffset(t *testing.T) {
	seen := map[byte]bool{}
	for off := int64(0); off < 256; off++ {
		seen[benchPatternByte(off)] = true
	}
	if len(seen) < 16 {
		t.Fatalf("the pattern produced only %d distinct bytes over 256 offsets; "+
			"a pattern that repeats too fast cannot detect a shifted payload", len(seen))
	}
}

// The pattern must not be the old all-zeros payload.
func TestTheBenchPatternIsNotAllZeros(t *testing.T) {
	buf := make([]byte, 1024)
	benchFillPattern(buf, 0)
	allZero := true
	for _, b := range buf {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("the bench payload is still all zeros; " +
			"a corrupted transfer would be counted as a success")
	}
}

// The check must accept what the filler produced, at any offset.
func TestTheBenchPayloadCheckAcceptsWhatWasFilled(t *testing.T) {
	for _, off := range []int64{0, 1, 4095, 4097, 65537, 1 << 20} {
		buf := make([]byte, 1024)
		benchFillPattern(buf, off)
		if !benchPayloadMatches(buf, off) {
			t.Fatalf("the check rejected the payload it had just filled at offset %d", off)
		}
	}
}

// ★ AND IT MUST REJECT A CORRUPTED PAYLOAD. Without this, the check
// could be vacuously true.
func TestTheBenchPayloadCheckRejectsCorruption(t *testing.T) {
	buf := make([]byte, 1024)
	benchFillPattern(buf, 0)

	// Flip one bit.
	corrupt := append([]byte(nil), buf...)
	corrupt[517] ^= 0x01
	if benchPayloadMatches(corrupt, 0) {
		t.Fatal("the check accepted a payload with a flipped bit")
	}

	// Shift the whole payload by one byte (a mis-framed transfer).
	shifted := append([]byte(nil), buf[1:]...)
	if benchPayloadMatches(shifted, 0) {
		t.Fatal("the check accepted a payload shifted by one byte")
	}
}
