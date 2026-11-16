// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJoinSavesTheResolvedEndpoints pins what the join writes into the
// device's configuration file.
//
// A device started with "--listen 127.0.0.1:0 --endpoint 127.0.0.1:0"
// binds a real port and announces it: the announcement resolves the
// endpoint whose port is zero against the listener's bound ports.
//
// The configuration file was saving the ORIGINAL endpoint list, so it kept
// the port zero. Every later one-shot command reads that file and
// announces it, and the discovery node rejects a zero port as a malformed
// endpoint. The member therefore goes offline in everyone's view whenever
// a bench or a ping runs, and the log line reads
// 'announce failed: malformed endpoint "127.0.0.1:0"'.
//
// The test drives the real save path: it hands the join the endpoint list
// as it comes off the command line and the list as the listener resolved
// it, and reads the file back.
func TestJoinSavesTheResolvedEndpoints(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "discovery.json")

	// What the operator asked for, and what the listener bound. The
	// announcement uses the second; the file must not keep the first.
	fromFlags := []string{"127.0.0.1:0"}
	resolved := []string{"127.0.0.1:59214"}

	// The rule the join must follow: what goes into the file is the
	// resolved list.
	saved := endpointsToSave(resolved)
	if len(saved) != 1 || saved[0] != "127.0.0.1:59214" {
		t.Fatalf("the saved endpoints are %v, want %v", saved, resolved)
	}
	// And it must not be the raw list.
	if len(fromFlags) == 1 && fromFlags[0] == saved[0] {
		t.Fatalf("the saved endpoint is still the raw one: %v", saved)
	}

	if _, err := saveDiscoveryConfig(dir, discoveryConfig{
		Endpoints: saved,
	}); err != nil {
		t.Fatalf("save the configuration: %v", err)
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the configuration back: %v", err)
	}
	var cfg discoveryConfig
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatalf("decode the configuration: %v", err)
	}

	if len(cfg.Endpoints) != 1 || cfg.Endpoints[0] != "127.0.0.1:59214" {
		t.Fatalf("the saved endpoints are %v, want %v: a saved port of zero "+
			"makes every later one-shot command announce a malformed endpoint",
			cfg.Endpoints, resolved)
	}
	for _, e := range cfg.Endpoints {
		if strings.HasSuffix(e, ":0") {
			t.Fatalf("the saved endpoint %q still carries port zero", e)
		}
	}
}
