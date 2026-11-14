// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

func TestStatusReportJSONIsStableAndKeepsOfflineMembers(t *testing.T) {
	now := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	report := statusReport{
		AccountID:     identity.DeviceID("acct123456789012"),
		Version:       7,
		GeneratedAt:   now,
		CurrentDevice: identity.DeviceID("local12345678901"),
		Online:        1,
		Members: []statusMember{
			{DeviceID: identity.DeviceID("offline12345678"), Online: false, Endpoints: []string{"127.0.0.1:9001"}, LastSeen: "2026-11-14T07:59:00Z"},
			{DeviceID: identity.DeviceID("online123456789"), Online: true, Endpoints: []string{"127.0.0.1:9002"}, LastSeen: "2026-11-14T08:00:00Z"},
		},
	}
	blob, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(blob)
	for _, want := range []string{"\"version\":7", "offline12345678", "\"online\":false", "\"last_seen\":\"2026-11-14T07:59:00Z\""} {
		if !strings.Contains(text, want) {
			t.Errorf("status JSON has no %q: %s", want, text)
		}
	}
}

func TestStatusReportJSONOmitsNeverSeenTimestamp(t *testing.T) {
	report := statusReport{
		AccountID: identity.DeviceID("acct123456789012"),
		Members:   []statusMember{{DeviceID: identity.DeviceID("neverseen1234567"), Online: false}},
	}
	blob, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "last_seen") {
		t.Fatalf("status JSON invented a last_seen value: %s", blob)
	}
}

func TestStatusAndDoctorAreRegistered(t *testing.T) {
	if err := Run([]string{"status", "--help"}); err == nil {
		t.Fatal("status --help should stop flag parsing with usage output")
	}
	if err := Run([]string{"doctor", "--help"}); err == nil {
		t.Fatal("doctor --help should stop flag parsing with usage output")
	}
}
