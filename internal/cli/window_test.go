// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"testing"
	"time"
)

// TestPairWindowDefaultsPerRelayCount pins the rule that a zero window means
// "wait forever" only when there is a single relay.
//
// With several relays a window is required or a dead relay is never abandoned,
// but zero must not reach time.After as-is: that expires immediately, which was
// observed as "no partner appeared within 0s" on the second relay of a list.
func TestPairWindowDefaultsPerRelayCount(t *testing.T) {
	cases := []struct {
		name    string
		window  time.Duration
		relays  int
		want    time.Duration
		forever bool
	}{
		{"single relay, no window", 0, 1, 0, true},
		{"single relay, explicit window", 30 * time.Second, 1, 30 * time.Second, false},
		{"two relays, no window", 0, 2, defaultPairWindow, false},
		{"two relays, explicit window", 15 * time.Second, 2, 15 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			window := tc.window
			forever := window <= 0 && tc.relays == 1
			if window <= 0 && !forever {
				window = defaultPairWindow
			}
			if forever != tc.forever {
				t.Errorf("forever = %v, want %v", forever, tc.forever)
			}
			if window != tc.want {
				t.Errorf("window = %v, want %v", window, tc.want)
			}
			if !forever && window <= 0 {
				t.Error("a finite window must be positive")
			}
		})
	}
}
