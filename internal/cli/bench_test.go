// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import "testing"

func TestParseBenchBytes(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int64
	}{
		{"100MB", 100_000_000},
		{"1 MiB", 1 << 20},
		{"7KB", 7_000},
		{"42", 42},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got, err := parseBenchBytes(tc.text, 0)
			if err != nil || got != tc.want {
				t.Fatalf("parseBenchBytes(%q) = %d, %v; want %d", tc.text, got, err, tc.want)
			}
		})
	}
	for _, text := range []string{"-1", "1XB", "nope"} {
		t.Run(text, func(t *testing.T) {
			if _, err := parseBenchBytes(text, 0); err == nil {
				t.Fatalf("parseBenchBytes(%q) succeeded", text)
			}
		})
	}
}
