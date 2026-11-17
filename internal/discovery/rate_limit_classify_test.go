// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"fmt"
	"net/http"
	"testing"
)

// TestRateLimitClassificationIgnoresTheAddress pins that the rate-limit
// verdict is read from the RESPONSE, not from the whole error string.
//
// isRateLimited matched "429" anywhere in the error text. The text carries
// the node's ADDRESS, and an address may contain the digits "429" — so a
// node on port 42933 that answers 503 (a transport failure, which must
// poll fast) was classified as rate-limited and backed off 8–12 s instead
// of 3 s.
//
// It surfaced as a CI failure in a cadence test, on a runner that happened
// to hand out port 42933. The same misclassification would silently slow
// every join against a node on such a port.
//
// The verdict must come from the status code, not from the text.
func TestRateLimitClassificationIgnoresTheAddress(t *testing.T) {
	// A transport failure against a node whose PORT contains "429".
	err := &registerRejectedError{
		Addr:   "http://mesh.example:42933",
		Status: http.StatusServiceUnavailable,
		Body:   "transport bucket: service unavailable",
	}
	if isRateLimited(err) {
		t.Fatalf("a 503 from a node on port 42933 was classified as rate-limited: %v", err)
	}
	// The same for a few other ports that contain the digit sequence.
	for _, port := range []string{"4290", "1429", "42933", "42999"} {
		err := &registerRejectedError{
			Addr:   "http://mesh.example:" + port,
			Status: http.StatusServiceUnavailable,
			Body:   "upstream unavailable",
		}
		if isRateLimited(err) {
			t.Errorf("a 503 from port %s was classified as rate-limited", port)
		}
	}
}

// TestRateLimitClassificationStillSeesRealRateLimits keeps the other half:
// a real rate-limit refusal must still be recognised.
func TestRateLimitClassificationStillSeesRealRateLimits(t *testing.T) {
	cases := []struct {
		status int
		body   string
	}{
		{http.StatusTooManyRequests, "too many requests"},
		{http.StatusTooManyRequests, ""},
		{http.StatusServiceUnavailable, "too many requests"},
		{http.StatusServiceUnavailable, "too many connections"},
	}
	for _, c := range cases {
		err := &registerRejectedError{
			Addr:   "http://mesh.example:18090",
			Status: c.status,
			Body:   c.body,
		}
		if !isRateLimited(err) {
			t.Errorf("a real rate-limit refusal was not recognised: HTTP %d %q", c.status, c.body)
		}
	}
}

// TestRateLimitClassificationNeedsAStatusCode pins the rule itself: the
// verdict comes from a status code, and a bare mention of the number in
// prose is not one.
func TestRateLimitClassificationNeedsAStatusCode(t *testing.T) {
	notRateLimits := []error{
		// A transport failure: no status code at all, and the address
		// carries the digits.
		fmt.Errorf("discovery: register failed on all 1 addresses: dial tcp 127.0.0.1:42933: connect: connection refused"),
		// A refusal whose BODY merely mentions the number.
		&registerRejectedError{
			Addr:   "http://mesh.example:18090",
			Status: http.StatusServiceUnavailable,
			Body:   "upstream said 429 requests per minute",
		},
		&registerRejectedError{
			Addr:   "http://mesh.example:18090",
			Status: http.StatusNotFound,
			Body:   "no such thing 429",
		},
	}
	for _, err := range notRateLimits {
		if isRateLimited(err) {
			t.Errorf("a non rate-limit error was classified as rate-limited: %v", err)
		}
	}
}
