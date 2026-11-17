// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/version"
)

// TestInfoVerbReportsTheCmeshVersion pins that the @info verb tells the
// operator which cmesh build the peer is running.
//
// It used to print runtime.Version() — the Go toolchain version — in BOTH
// the "version" and the "go" lines. An operator reading "version go1.27.4"
// learns nothing about the peer, and the field name actively misleads: it
// looks like the product version and is not. External testers reported this
// twice (rounds 58 and 60).
//
// The two fields must therefore mean different things: "version" is the
// cmesh build, "go" is the toolchain the build was made with.
func TestInfoVerbReportsTheCmeshVersion(t *testing.T) {
	// Stand in for a stamped build.
	version.Version = "cmesh-test-9f9d"
	defer func() { version.Version = "dev" }()

	acc, err := identity.GenerateAccount()
	if err != nil {
		t.Fatal(err)
	}
	members := identity.NewMemberList(acc.ID())
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := acc.IssueCert(dev, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := members.Add(*cert); err != nil {
		t.Fatal(err)
	}

	// A session pair: the verb runs on the "server" side.
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		br := bufio.NewReader(server)
		_ = receiveRun(br, server, ListenerConfig{})
	}()

	// Send the request.
	req, err := json.Marshal(RunRequest{Verb: "@info"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(client, "%s\n", req); err != nil {
		t.Fatal(err)
	}

	// Read the output frame.
	br := bufio.NewReader(client)
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	tag, payload, err := readRunFrame(br)
	if err != nil {
		t.Fatalf("read the @info output: %v", err)
	}
	if tag != RunTagOut {
		t.Fatalf("frame tag = %q, want %q", tag, RunTagOut)
	}
	out := string(payload)

	if !strings.Contains(out, "version  cmesh-test-9f9d") {
		t.Fatalf("the @info output does not carry the cmesh version:\n%s", out)
	}
	if !strings.Contains(out, "go       "+runtime.Version()) {
		t.Fatalf("the @info output does not carry the Go toolchain version:\n%s", out)
	}
	// The old behaviour printed the toolchain in both fields, so a build
	// whose Go version differs from its cmesh version is what separates
	// them. Assert they are not the same string in the output.
	if strings.Count(out, runtime.Version()) != 1 {
		t.Fatalf("the Go toolchain version appears more than once:\n%s", out)
	}
}
