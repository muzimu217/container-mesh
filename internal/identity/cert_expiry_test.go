// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package identity

import (
	"testing"
	"time"
)

// TestExpiredIsTheOneExpiryRule pins that the expiry check has ONE
// definition.
//
// It was written out in five places outside this package — the relay's
// member key lookup, the snapshot builder, the mesh resolver, the cert
// adoption path and the enrollment path — each comparing against
// Cert.ExpiresAt in its own way. The copies had drifted: some compared
// against time.Now().UTC() while others compared against a `now` the
// caller had already captured, so two of them could disagree by the length
// of a request.
//
// The rule now lives here, and the check is on the TYPE so a new copy
// elsewhere is visible.
func TestExpiredIsTheOneExpiryRule(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second)
	future := now.Add(time.Second)

	// A certificate that lapsed a second ago is expired.
	c := &DeviceCert{ExpiresAt: past}
	if !c.Expired(now) {
		t.Fatal("a certificate that lapsed a second ago was not reported expired")
	}
	// One that has not lapsed yet is not.
	c = &DeviceCert{ExpiresAt: future}
	if c.Expired(now) {
		t.Fatal("a certificate that has not lapsed was reported expired")
	}
	// The comparison is against the GIVEN time, not against the wall
	// clock — that is what lets a caller use one consistent instant for a
	// whole view.
	c = &DeviceCert{ExpiresAt: now.Add(time.Hour)}
	if !c.Expired(now.Add(2 * time.Hour)) {
		t.Fatal("the check ignored the given instant")
	}
	if c.Expired(now.Add(-2 * time.Hour)) {
		t.Fatal("the check reported a certificate expired before it lapsed")
	}
}

// TestExpiredUsesTheGivenInstantInTheCallersZone pins that the instant is
// compared in UTC, so a caller in another time zone cannot see a
// certificate as live that is already lapsed.
func TestExpiredUsesTheGivenInstantInTheCallersZone(t *testing.T) {
	// A zone well east of UTC.
	zone := time.FixedZone("east", 8*3600)
	lapsed := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	c := &DeviceCert{ExpiresAt: lapsed}
	// The same instant, expressed in the other zone: 20:00 +08:00 is the
	// same moment, so the certificate is exactly AT its expiry. The rule
	// is "strictly after", so exactly at the expiry is not yet expired.
	atExpiry := lapsed.In(zone)
	if c.Expired(atExpiry) {
		t.Fatal("a certificate exactly at its expiry was reported expired")
	}
	// A hair after the same moment: expired.
	if !c.Expired(atExpiry.Add(time.Millisecond)) {
		t.Fatal("a certificate one millisecond past its expiry was not reported expired")
	}
}
