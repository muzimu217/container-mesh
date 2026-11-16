// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/relay"
)

// TestServeRelayGracefulStopReleasesPort is the acceptance for the relay's
// graceful stop.
//
// The defect it guards against was that `cmesh relay` never wired
// relay.Server.Shutdown: a SIGTERM ended the process with a hard 143 and the
// listener stayed bound, so a systemd restart could not rebind the port.
//
// The assertion is on the drain behaviour (the port is released and the
// serve call returns nil), which is the part that was actually broken. The
// signal delivery itself is covered by the process-level acceptance on the
// external side, and by TestServeDiscoverySIGTERMExitsZero for the sibling
// command in this package.
func TestServeRelayGracefulStopReleasesPort(t *testing.T) {
	addr := freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	// stop restores default signal handling; the serve core only calls it
	// when the context ends, mirroring the real command.
	stop := cancel

	served := make(chan error, 1)
	go func() {
		served <- serveRelay(ctx, stop, relay.Config{
			Addr: addr,
			Path: "/ws",
		})
	}()

	// Wait until the listener actually accepts requests: stopping before the
	// bind would exercise the pre-bind shutdown path, not the drain.
	healthy := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			healthy = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthy {
		cancel()
		<-served
		t.Fatal("relay never answered /healthz")
	}

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveRelay returned %v, want nil after a graceful stop", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("relay did not stop within 20s of the stop signal")
	}

	// The port is released: bindable again immediately, so a restarting unit
	// does not collide with a lingering listener.
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s not reusable after clean stop: %v", addr, err)
	}
	_ = l.Close()
}
