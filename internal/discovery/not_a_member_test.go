// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import (
	"fmt"
	"net/http"
	"testing"
)

// TestNotAMemberVerdictComesFromTheStatusCode pins how the client tells a
// revocation from a network fault.
//
// The node answers a register from a non-member with 401. The client used
// to read that out of the error TEXT with strings.Contains(err.Error(),
// "not a member"), which couples the client's behaviour to the server's
// wording: reword the message and the client quietly stops recognising a
// revocation, and tells the operator to go debug their routing instead
// (issue #8, the revocation UX finding).
//
// The verdict is the status code. The text is not consulted, so the
// server's wording can change without changing what the operator sees.
func TestNotAMemberVerdictComesFromTheStatusCode(t *testing.T) {
	// The real shape: a 401 from the node.
	err := error(&RegisterRejectedError{
		Addr:   "http://mesh.example:18090",
		Status: http.StatusUnauthorized,
		Body:   "discovery: device is not a member",
	})
	if !IsNotAMember(err) {
		t.Fatalf("a 401 was not recognised as a membership refusal: %v", err)
	}

	// The same status with a reworded body — still a membership refusal.
	err = &RegisterRejectedError{
		Addr:   "http://mesh.example:18090",
		Status: http.StatusUnauthorized,
		Body:   "you are not on this network",
	}
	if !IsNotAMember(err) {
		t.Fatalf("a 401 with a reworded body was not recognised: %v", err)
	}

	// A body that says "not a member" on a DIFFERENT status is not a
	// membership refusal: the status is what decides.
	err = &RegisterRejectedError{
		Addr:   "http://mesh.example:18090",
		Status: http.StatusBadRequest,
		Body:   "device is not a member",
	}
	if IsNotAMember(err) {
		t.Fatalf("a 400 was read as a membership refusal: %v", err)
	}

	// A transport failure is not a membership refusal either — the
	// operator must be told the node is unreachable, not that they were
	// revoked.
	err = fmt.Errorf("discovery: register failed on all 1 addresses: dial tcp 127.0.0.1:18090: connect: connection refused")
	if IsNotAMember(err) {
		t.Fatalf("a transport failure was read as a membership refusal: %v", err)
	}
	if IsNotAMember(nil) {
		t.Fatal("a nil error was read as a membership refusal")
	}
}
