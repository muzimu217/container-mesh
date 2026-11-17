// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"net/http"
	"strings"
	"testing"
)

// oldIsRateLimited is the implementation this package used before the
// classification was taken from the status code: it matched "429" anywhere
// in the error text.
//
// It is kept here as the reference the new behaviour is measured against.
// The point of the comparison is not that the old code fails a test — it
// is that the old rule and the new rule DISAGREE on a real input, and the
// input is a node whose port happens to contain those three digits.
func oldIsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "429") || strings.Contains(s, "too many request") ||
		strings.Contains(s, "too many connection")
}

// TestRateLimitRuleChangedOnlyForThePortShape pins the exact scope of the
// change: the two rules agree everywhere except on an address whose port
// contains "429", which is where the old rule was wrong.
func TestRateLimitRuleChangedOnlyForThePortShape(t *testing.T) {
	cases := []struct {
		addr   string
		status int
		body   string
	}{
		// The CI failure: a 503 from a node on port 42933.
		{"http://mesh.example:42933", http.StatusServiceUnavailable, "transport bucket: service unavailable"},
		// A real rate limit, from any port.
		{"http://mesh.example:18090", http.StatusTooManyRequests, "too many requests"},
		// A 404 from a node whose port contains 429.
		{"http://mesh.example:42933", http.StatusNotFound, "nope"},
		// A 503 from an ordinary port.
		{"http://mesh.example:18090", http.StatusServiceUnavailable, "transport bucket: service unavailable"},
	}
	for _, c := range cases {
		err := &RegisterRejectedError{Addr: c.addr, Status: c.status, Body: c.body}
		got := IsRateLimited(err)
		old := oldIsRateLimited(err)
		if got == old {
			continue
		}
		// The only allowed difference is the port shape.
		if !strings.Contains(c.addr, "429") {
			t.Fatalf("the rule changed for an ordinary address: %v (old=%v new=%v)", err, old, got)
		}
		if c.status == http.StatusTooManyRequests {
			t.Fatalf("a real rate limit stopped being recognised: %v", err)
		}
		if got {
			t.Fatalf("a %d from %s is still classified as rate-limited", c.status, c.addr)
		}
		t.Logf("fixed: %v (old said rate-limited, new says no)", err)
	}
}
