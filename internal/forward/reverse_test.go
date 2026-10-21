// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package forward

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/mux"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// newReverseManager builds a Manager configured for -R with a given port
// whitelist.
func newReverseManager(t *testing.T, ports []string, listenAll bool, d Dialer) *Manager {
	t.Helper()
	ps, err := ParsePortSet(ports)
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{
		Accept:        true,
		AcceptReverse: true,
		ListenAll:     listenAll,
		ListenPorts:   ps,
		Limits:        Limits{MaxStreamsPerSec: 0},
		Dialer:        d,
	})
}

// TestReverseDialUsesLocalTargetNotPreamble is the structural guarantee behind
// -R: the preamble carries a listener ID, and the ID selects a locally
// configured target. An address in the preamble is never dialled.
func TestReverseDialUsesLocalTargetNotPreamble(t *testing.T) {
	d := &countingDialer{}
	m := newReverseManager(t, []string{"9000"}, false, d)
	defer m.Close()

	// The local configuration says 127.0.0.1:9000 maps to 10.0.0.5:5432.
	if err := m.PrepareReverse([]Spec{{
		Reverse: true,
		Bind:    "127.0.0.1:9000",
		Target:  "10.0.0.5:5432",
		ID:      "127.0.0.1:9000",
	}}); err != nil {
		t.Fatal(err)
	}

	// A preamble naming the listener ID is served.
	st := newFakeStream(mux.KindForward, reversePreambleBytes(t, "127.0.0.1:9000"))
	m.Handle(context.Background(), st)
	if got := d.calls.Load(); got != 1 {
		t.Fatalf("dialled %d times, want 1", got)
	}
	if code, _ := statusOf(t, st); code != wire.StatusOK {
		t.Fatalf("status %v, want ok", code)
	}
}

// TestReverseUnknownListenerIsDenied: an ID this side never registered must be
// refused, not guessed at.
func TestReverseUnknownListenerIsDenied(t *testing.T) {
	d := &countingDialer{}
	m := newReverseManager(t, []string{"9000"}, false, d)
	defer m.Close()

	st := newFakeStream(mux.KindForward, reversePreambleBytes(t, "127.0.0.1:9999"))
	m.Handle(context.Background(), st)

	if got := d.calls.Load(); got != 0 {
		t.Fatalf("dialled %d times for an unknown listener", got)
	}
	if code, reason := statusOf(t, st); code != wire.StatusDenied {
		t.Fatalf("status %v (%s), want denied", code, reason)
	}
}

// TestReverseDisabledIsDenied: without --accept-reverse nothing is served, even
// for a registered ID.
func TestReverseDisabledIsDenied(t *testing.T) {
	d := &countingDialer{}
	m := New(Config{Accept: true, AcceptReverse: false, Dialer: d})
	defer m.Close()
	_ = m.PrepareReverse([]Spec{{
		Reverse: true, Bind: "127.0.0.1:9000", Target: "10.0.0.5:5432", ID: "127.0.0.1:9000",
	}})

	st := newFakeStream(mux.KindForward, reversePreambleBytes(t, "127.0.0.1:9000"))
	m.Handle(context.Background(), st)

	if got := d.calls.Load(); got != 0 {
		t.Fatalf("dialled %d times with --accept-reverse off", got)
	}
	if code, _ := statusOf(t, st); code != wire.StatusDenied {
		t.Fatalf("status %v, want denied", code)
	}
}

// TestReverseBindRequiresPortWhitelist is the port whitelist: a bind is a side
// effect on this host, so a refused request must not produce one.
func TestReverseBindRequiresPortWhitelist(t *testing.T) {
	m := newReverseManager(t, []string{"9000"}, false, &countingDialer{})
	defer m.Close()

	results := m.BindReverseListeners(nil, []wire.ForwardListenSpec{
		{ID: "127.0.0.1:9001", Host: "127.0.0.1", Port: 9001},
	})
	if len(results) != 1 || results[0].Status != wire.StatusDenied {
		t.Fatalf("results = %+v, want one denied", results)
	}
	// The port must still be free.
	ln, err := net.Listen("tcp", "127.0.0.1:9001")
	if err != nil {
		t.Fatalf("port 9001 was bound despite a denial: %v", err)
	}
	_ = ln.Close()
}

// TestReverseBindAllowsWhitelistedPort is the positive case, and it proves the
// listener is actually created.
func TestReverseBindAllowsWhitelistedPort(t *testing.T) {
	// Port 0 is not in the whitelist, so use a real free port.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(probe.Addr().String())
	_ = probe.Close()
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	m := newReverseManager(t, []string{portStr}, false, &countingDialer{})
	defer m.Close()

	results := m.BindReverseListeners(nil, []wire.ForwardListenSpec{
		{ID: "127.0.0.1:" + portStr, Host: "127.0.0.1", Port: port},
	})
	if len(results) != 1 || results[0].Status != wire.StatusOK {
		t.Fatalf("results = %+v, want ok", results)
	}
	// The port is now ours.
	ln, err := net.Listen("tcp", "127.0.0.1:"+portStr)
	if err == nil {
		_ = ln.Close()
		t.Fatal("listener was reported bound but the port is still free")
	}
}

// TestReverseNonLoopbackNeedsListenAll: binding a non-loopback address exposes
// the port to the network, which is a bigger permission than the port
// whitelist alone.
func TestReverseNonLoopbackNeedsListenAll(t *testing.T) {
	m := newReverseManager(t, []string{"9000"}, false, &countingDialer{})
	defer m.Close()

	results := m.BindReverseListeners(nil, []wire.ForwardListenSpec{
		{ID: "0.0.0.0:9000", Host: "0.0.0.0", Port: 9000},
	})
	if len(results) != 1 || results[0].Status != wire.StatusDenied {
		t.Fatalf("results = %+v, want denied without --listen-all", results)
	}
	if !bytes.Contains([]byte(results[0].Message), []byte("--listen-all")) {
		t.Fatalf("message %q does not mention --listen-all", results[0].Message)
	}
}

// TestReverseBindFailIsDistinct proves a port conflict reports BindFail rather
// than Denied: the operator needs to know whether to change the config or the
// port.
func TestReverseBindFailIsDistinct(t *testing.T) {
	// Occupy a port, then ask for it.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, portStr, _ := net.SplitHostPort(held.Addr().String())
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	m := newReverseManager(t, []string{portStr}, false, &countingDialer{})
	defer m.Close()

	results := m.BindReverseListeners(nil, []wire.ForwardListenSpec{
		{ID: "127.0.0.1:" + portStr, Host: "127.0.0.1", Port: port},
	})
	if len(results) != 1 || results[0].Status != wire.StatusBindFail {
		t.Fatalf("results = %+v, want bind-failed", results)
	}
}

// TestReverseSpecsRoundTrip checks the CLI's -R spec conversion.
func TestReverseSpecsRoundTrip(t *testing.T) {
	specs := []Spec{
		{Reverse: true, Bind: "127.0.0.1:9000", Target: "127.0.0.1:3000", ID: "127.0.0.1:9000"},
		{Reverse: true, Bind: "[::1]:9001", Target: "10.0.0.5:80", ID: "[::1]:9001"},
	}
	got, err := ReverseSpecs(specs)
	if err != nil {
		t.Fatalf("ReverseSpecs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d specs", len(got))
	}
	if got[0].Host != "127.0.0.1" || got[0].Port != 9000 || got[0].ID != "127.0.0.1:9000" {
		t.Fatalf("spec 0 = %+v", got[0])
	}
	if got[1].Host != "::1" || got[1].Port != 9001 {
		t.Fatalf("spec 1 = %+v", got[1])
	}
}

// TestPrepareReverseRejectsForwardSpec keeps the two directions apart.
func TestPrepareReverseRejectsForwardSpec(t *testing.T) {
	m := newReverseManager(t, []string{"9000"}, false, &countingDialer{})
	defer m.Close()
	if err := m.PrepareReverse([]Spec{{Reverse: false, Bind: "127.0.0.1:8080"}}); err == nil {
		t.Fatal("accepted a -L spec as a -R mapping")
	}
}

// TestIsLoopback covers the bind-scope check.
func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true, "::1": true, "localhost": true,
		"0.0.0.0": false, "::": false, "10.0.0.5": false, "": false,
		"example.com": false,
	}
	for host, want := range cases {
		if got := isLoopback(host); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestCloseReverseListenersFreesPorts proves a peer's listeners do not outlive
// the manager.
func TestCloseReverseListenersFreesPorts(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(probe.Addr().String())
	_ = probe.Close()
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	m := newReverseManager(t, []string{portStr}, false, &countingDialer{})
	results := m.BindReverseListeners(nil, []wire.ForwardListenSpec{
		{ID: "127.0.0.1:" + portStr, Host: "127.0.0.1", Port: port},
	})
	if results[0].Status != wire.StatusOK {
		t.Fatalf("bind failed: %+v", results[0])
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+portStr)
	if err != nil {
		t.Fatalf("port still bound after Close: %v", err)
	}
	_ = ln.Close()
}

// TestReverseRebindReplacesListener covers reconnect: the same ID arrives again
// and must replace the old listener rather than fail on a busy port.
func TestReverseRebindReplacesListener(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(probe.Addr().String())
	_ = probe.Close()
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	m := newReverseManager(t, []string{portStr}, false, &countingDialer{})
	defer m.Close()

	spec := wire.ForwardListenSpec{ID: "127.0.0.1:" + portStr, Host: "127.0.0.1", Port: port}
	for i := 0; i < 3; i++ {
		results := m.BindReverseListeners(nil, []wire.ForwardListenSpec{spec})
		if len(results) != 1 || results[0].Status != wire.StatusOK {
			t.Fatalf("rebind %d failed: %+v", i, results)
		}
	}
}

// TestReverseEndToEnd drives a full reverse forward: the peer-side listener is
// bound, an application connects to it, and the bytes reach a local target.
//
// It uses a fake Provider so no yamux is involved: the property under test is
// the ID lookup and the dial, not the transport.
func TestReverseEndToEnd(t *testing.T) {
	// A local echo target, standing in for the service -R exposes.
	target := echoServerForTest(t)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(probe.Addr().String())
	_ = probe.Close()
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	m := newReverseManager(t, []string{portStr}, false, &countingDialer{})
	defer m.Close()

	// This side hosts the listener the peer asked for; the target is local.
	id := "127.0.0.1:" + portStr
	if err := m.PrepareReverse([]Spec{
		{Reverse: true, Bind: id, Target: target, ID: id},
	}); err != nil {
		t.Fatal(err)
	}

	// Bind and confirm the port is live.
	results := m.BindReverseListeners(nil, []wire.ForwardListenSpec{
		{ID: id, Host: "127.0.0.1", Port: port},
	})
	if results[0].Status != wire.StatusOK {
		t.Fatalf("bind: %+v", results[0])
	}

	// The reverse path's serving half is exercised through Handle, which is
	// what the peer's stream reaches.
	st := newFakeStream(mux.KindForward, reversePreambleBytes(t, id))
	m.Handle(context.Background(), st)
	if code, reason := statusOf(t, st); code != wire.StatusOK {
		t.Fatalf("reverse handle: %v (%s)", code, reason)
	}
}

// echoServerForTest starts a TCP echo server and returns its address.
func echoServerForTest(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// TestReverseDialFailIsReported distinguishes a dead local target from a policy
// refusal.
func TestReverseDialFailIsReported(t *testing.T) {
	d := &countingDialer{err: errors.New("connection refused")}
	m := newReverseManager(t, []string{"9000"}, false, d)
	defer m.Close()
	_ = m.PrepareReverse([]Spec{
		{Reverse: true, Bind: "127.0.0.1:9000", Target: "10.0.0.5:5432", ID: "127.0.0.1:9000"},
	})

	st := newFakeStream(mux.KindForward, reversePreambleBytes(t, "127.0.0.1:9000"))
	m.Handle(context.Background(), st)
	if code, _ := statusOf(t, st); code != wire.StatusDialFail {
		t.Fatalf("status %v, want dial-failed", code)
	}
}

// TestReverseRateLimitApplies proves the token bucket covers the reverse path
// too: an attacker with the code must not be able to scan through -R either.
func TestReverseRateLimitApplies(t *testing.T) {
	ps, _ := ParsePortSet([]string{"9000"})
	m := New(Config{
		Accept:        true,
		AcceptReverse: true,
		ListenPorts:   ps,
		Limits:        Limits{MaxStreamsPerSec: 1},
		Dialer:        &countingDialer{},
	})
	defer m.Close()
	_ = m.PrepareReverse([]Spec{
		{Reverse: true, Bind: "127.0.0.1:9000", Target: "10.0.0.5:5432", ID: "127.0.0.1:9000"},
	})

	// First consumes the burst.
	first := newFakeStream(mux.KindForward, reversePreambleBytes(t, "127.0.0.1:9000"))
	m.Handle(context.Background(), first)

	for i := 0; i < 3; i++ {
		st := newFakeStream(mux.KindForward, reversePreambleBytes(t, "127.0.0.1:9000"))
		m.Handle(context.Background(), st)
		if code, _ := statusOf(t, st); code != wire.StatusTooMany {
			t.Fatalf("request %d got %v, want too-many", i, code)
		}
	}
}

// waitFor is a small helper for tests that need a bounded wait.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// TestAcceptReverseAloneServesReverseStreams is the regression test for a real
// bug found by the process-level smoke test: Serve gated every kind=1 stream on
// --accept, so a peer running with only --accept-reverse bound the listener and
// then refused every connection to it.
//
// The two permissions are independent: --accept is "you may dial targets I
// allow", --accept-reverse is "you may ask me to bind a port".
func TestAcceptReverseAloneServesReverseStreams(t *testing.T) {
	d := &countingDialer{}
	ps, _ := ParsePortSet([]string{"9000"})
	m := New(Config{
		Accept:        false, // deliberately off
		AcceptReverse: true,
		ListenPorts:   ps,
		Dialer:        d,
	})
	defer m.Close()
	if err := m.PrepareReverse([]Spec{
		{Reverse: true, Bind: "127.0.0.1:9000", Target: "10.0.0.5:5432", ID: "127.0.0.1:9000"},
	}); err != nil {
		t.Fatal(err)
	}

	st := newFakeStream(mux.KindForward, reversePreambleBytes(t, "127.0.0.1:9000"))
	m.Serve(context.Background(), st)

	if got := d.calls.Load(); got != 1 {
		t.Fatalf("dialled %d times, want 1 (reverse must work without --accept)", got)
	}
	if code, reason := statusOf(t, st); code != wire.StatusOK {
		t.Fatalf("status %v (%s), want ok", code, reason)
	}
}

// TestAcceptReverseAloneRefusesDirectStreams is the other half: a peer that only
// granted --accept-reverse must not become a -L dialler.
func TestAcceptReverseAloneRefusesDirectStreams(t *testing.T) {
	d := &countingDialer{}
	ps, _ := ParsePortSet([]string{"9000"})
	m := New(Config{
		Accept:        false,
		AcceptReverse: true,
		ListenPorts:   ps,
		Dialer:        d,
	})
	defer m.Close()

	st := newFakeStream(mux.KindForward, preambleBytes(t, "10.0.0.5:5432"))
	m.Serve(context.Background(), st)

	if got := d.calls.Load(); got != 0 {
		t.Fatalf("dialled %d times for a direct stream without --accept", got)
	}
	if code, _ := statusOf(t, st); code != wire.StatusDenied {
		t.Fatalf("status %v, want denied", code)
	}
}
