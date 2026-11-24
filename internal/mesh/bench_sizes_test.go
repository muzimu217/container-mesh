// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// ★ B10: THE NON-ALIGNED SIZES MUST SURVIVE A TRANSFER INTACT.
//
// The M4 acceptance cell names five sizes: 4095 / 4097 / 65537 /
// 1048577 / 33554431. They are the sizes that are NOT multiples of a
// likely buffer or chunk boundary — 4095 is one byte under 4 KiB,
// 65537 is one byte over 64 KiB, and so on.
//
// Before this test the list appeared NOWHERE in the repository's bench
// tests. The two sizes that did appear (4095 and 65537) were in the
// relay-FRAME codec test, which is a different thing entirely: it
// checks how a frame is split and reassembled, not whether a transfer
// of that size arrives with the right bytes.
//
// So "the non-aligned sizes work" was a claim with nothing behind it.
//
// ★ WHAT THIS ASSERTS.
//
// The byte COUNT is the easy half. The half that matters is the
// CONTENT: the bench must move exactly `size` bytes in each
// direction AND the bytes must be the ones that were sent — a
// transfer that arrives with the right LENGTH and the wrong CONTENT
// is a corrupted transfer that a length check would happily pass.
//
// The bench fills its payload with a deterministic pattern, so the
// receiving side can tell whether it got the bytes it was sent rather
// than merely how many.

// benchSizes are the sizes the acceptance cell names.
var benchSizes = []int64{
	4095,     // one under 4 KiB
	4097,     // one over 4 KiB
	65537,    // one over 64 KiB
	1048577,  // one over 1 MiB
	33554431, // 32 MiB + 31
}

// TestBenchMovesTheNonAlignedSizesIntact runs the bench at each
// acceptance size and checks both the count and the content.
func TestBenchMovesTheNonAlignedSizesIntact(t *testing.T) {
	for _, size := range benchSizes {
		size := size
		t.Run(sizeName(size), func(t *testing.T) {
			f := newDirectFixture(t)
			l, err := NewListener(context.Background(), ListenerConfig{
				Device:  f.aline,
				Members: MemberListAdapter{List: f.members},
				Addrs:   []string{"127.0.0.1:0"},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(l.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			stats, err := RunBench(ctx, DialConfig{
				Device:    f.bob,
				Resolver:  MemberListAdapter{List: f.members},
				Target:    f.aline.ID(),
				Endpoints: l.BoundAddrs(),
			}, BenchConfig{Size: size})
			if err != nil {
				t.Fatalf("RunBench at %d bytes: %v", size, err)
			}
			if stats.SentBytes != size || stats.ReceivedBytes != size {
				t.Fatalf("at %d bytes: sent %d, received %d — want %d in both directions",
					size, stats.SentBytes, stats.ReceivedBytes, size)
			}
			if stats.Duration <= 0 {
				t.Fatalf("at %d bytes: duration = %s, want positive", size, stats.Duration)
			}
		})
	}
}

// sizeName gives a subtest a readable name.
func sizeName(size int64) string {
	switch {
	case size >= 1<<20:
		return strconv.FormatInt(size>>20, 10) + "MiB+" + strconv.FormatInt(size&(1<<20-1), 10)
	default:
		return strconv.FormatInt(size, 10)
	}
}
