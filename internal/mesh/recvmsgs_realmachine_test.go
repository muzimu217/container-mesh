// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// The REAL-MACHINE check of the message receiver's delivery receipt.
//
// The unit tests in recvmsgs_ack_test.go pin the receiver's behaviour on
// an in-memory session. This file pins it on the REAL path: two real
// machines, a real public relay, a real QUIC direct link.
//
// It is skipped unless the environment names the two ends of the mesh:
//
//	CMESH_REALMACHINE_STATE    the state directory of THIS device
//	CMESH_REALMACHINE_PASS     this device's passphrase
//	CMESH_REALMACHINE_TARGET   the device ID to send to
//
// The three names are read from the environment, so the test carries no
// topology of its own: it runs wherever it is pointed.

// realMachineConfig is the topology, read from the environment.
type realMachineConfig struct {
	stateDir string
	pass     string
	target   identity.DeviceID
}

// readRealMachineConfig reads the topology, or reports that the test
// must be skipped.
func readRealMachineConfig(t *testing.T) (realMachineConfig, bool) {
	t.Helper()
	state := os.Getenv("CMESH_REALMACHINE_STATE")
	pass := os.Getenv("CMESH_REALMACHINE_PASS")
	target := os.Getenv("CMESH_REALMACHINE_TARGET")
	if state == "" || pass == "" || target == "" {
		return realMachineConfig{}, false
	}
	return realMachineConfig{stateDir: state, pass: pass, target: identity.DeviceID(target)}, true
}

// realMachineSavedConfig is the join configuration the CLI saved: the
// discovery addresses and the account the device joined.
type realMachineSavedConfig struct {
	DiscoveryAddrs []string `json:"discovery_addrs"`
	AccountID      string   `json:"account_id"`
	AccountPub     []byte   `json:"account_pub"`
}

// realMachineView is the live member view, plus the resolver that reads
// it.
type realMachineView struct {
	client   *discovery.Client
	resolver *SnapshotAdapter
}

// openRealMachineView opens the discovery client the same way the CLI
// does, and waits for the first verified snapshot.
//
// The CLI's own resolver is a liveViewResolver over the client; here the
// snapshot is taken once the client has it, which is what a one-shot
// command does.
func openRealMachineView(t *testing.T, cfg realMachineConfig, dev *identity.Device) *realMachineView {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(cfg.stateDir, "discovery.json"))
	if err != nil {
		t.Fatalf("read the saved join config: %v", err)
	}
	var saved realMachineSavedConfig
	if err := json.Unmarshal(blob, &saved); err != nil {
		t.Fatalf("parse the saved join config: %v", err)
	}
	if len(saved.DiscoveryAddrs) == 0 {
		t.Fatalf("the saved join config names no discovery address")
	}
	cl, err := discovery.NewClient(discovery.ClientConfig{
		Addrs:      saved.DiscoveryAddrs,
		Device:     dev,
		AccountID:  identity.DeviceID(saved.AccountID),
		AccountPub: saved.AccountPub,
		NoAnnounce: true,
	})
	if err != nil {
		t.Fatalf("open the discovery client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = cl.Run(ctx) }()

	// Wait for the first verified snapshot: the view has to be there
	// before a dial can pin the target's certificate against it.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if snap := cl.Snapshot(); snap != nil && len(snap.Members) > 0 {
			return &realMachineView{client: cl, resolver: &SnapshotAdapter{Snap: snap, AccountPub: saved.AccountPub}}
		}
		if time.Now().After(deadline) {
			t.Fatal("the member view never arrived")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// realMachineEndpoints returns the target's announced direct endpoints.
func realMachineEndpoints(t *testing.T, view *realMachineView, target identity.DeviceID) []string {
	t.Helper()
	snap := view.resolver.Snap
	mv, ok := snap.Members[target]
	if !ok {
		t.Fatalf("%s is not in the member view", target)
	}
	if len(mv.Endpoints) == 0 {
		t.Fatalf("%s announced no direct endpoint (view: %+v)", target, mv)
	}
	return mv.Endpoints
}

// realMachinePicker builds the picker the CLI uses: the direct path
// first, then the relay candidates.
//
// It is what makes the RELAY path reachable in the check below — the
// public-relay topology is the one the operator actually runs.
func realMachinePicker(view *realMachineView, self identity.DeviceID) ViewPicker {
	return ViewPicker{
		View: func() *discovery.MemberSnapshot { return view.resolver.Snap },
		Self: self,
		TargetKey: func(id identity.DeviceID) (ed25519.PublicKey, error) {
			return view.resolver.KeyFor(id)
		},
	}
}

// TestRealMachineMsgOverCapIsRefused: on the real path, a frame over
// the cap must NOT produce a delivery receipt.
//
// The sender writes the over-cap frame itself (the CLI's own guard
// refuses an over-long message before it reaches the wire, so the frame
// has to be put on the wire by hand), then reads the receiver's answer.
//
// The answer must be a FAILURE line, and it must carry the reason.
func TestRealMachineMsgOverCapIsRefused(t *testing.T) {
	cfg, ok := readRealMachineConfig(t)
	if !ok {
		t.Skip("set CMESH_REALMACHINE_STATE, CMESH_REALMACHINE_PASS and CMESH_REALMACHINE_TARGET to run the real-machine check")
	}

	dev, err := identity.LoadDevice(identity.IdentityPath(cfg.stateDir, identity.DeviceFileName), cfg.pass)
	if err != nil {
		t.Fatalf("load the device: %v", err)
	}
	view := openRealMachineView(t, cfg, dev)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	link, err := DialDirect(ctx, DialConfig{
		Device:           dev,
		Resolver:         view.resolver,
		Target:           cfg.target,
		Endpoints:        realMachineEndpoints(t, view, cfg.target),
		Kind:             KindMsg,
		HandshakeTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial the target over the real path: %v", err)
	}
	defer link.Close()

	// The over-cap frame, written by hand.
	if _, err := link.Session().Write(frameLengthPrefix(MaxMsgBytes + 1)); err != nil {
		t.Fatalf("write the over-cap frame: %v", err)
	}
	if err := link.Session().Close(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	_ = link.Session().SetReadDeadline(time.Now().Add(20 * time.Second))
	ack, err := readLine(link.br, MaxHelloBytes)
	if err != nil {
		t.Fatalf("the receiver never answered: %v", err)
	}
	assertFailedAck(t, string(ack))
	t.Logf("the receiver answered %q", ack)
}

// TestRealMachineNormalMsgIsDelivered: the ordinary path must still
// work on the real topology.
//
// Without this, a fix that broke every ordinary send would still pass
// the failure test above.
func TestRealMachineNormalMsgIsDelivered(t *testing.T) {
	cfg, ok := readRealMachineConfig(t)
	if !ok {
		t.Skip("set CMESH_REALMACHINE_STATE, CMESH_REALMACHINE_PASS and CMESH_REALMACHINE_TARGET to run the real-machine check")
	}

	dev, err := identity.LoadDevice(identity.IdentityPath(cfg.stateDir, identity.DeviceFileName), cfg.pass)
	if err != nil {
		t.Fatalf("load the device: %v", err)
	}
	view := openRealMachineView(t, cfg, dev)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	text := "real-machine delivery check"
	if err := SendMsg(ctx, DialConfig{
		Device:           dev,
		Resolver:         view.resolver,
		Target:           cfg.target,
		Endpoints:        realMachineEndpoints(t, view, cfg.target),
		HandshakeTimeout: 20 * time.Second,
	}, []string{text}); err != nil {
		t.Fatalf("SendMsg over the real path: %v", err)
	}
	t.Logf("delivered %q", text)
}

// TestRealMachineMsgOverCapIsRefusedOverTheRelayPath: the same check
// over the PUBLIC RELAY path.
//
// The relay carries the frames verbatim, so the receiver's frame
// handling is the same code — but the RELAYED session's end-of-stream
// semantics are the relay's own, and that is where a naive EOF check
// would go wrong: a relayed half-close is a CLOSE RECORD, not a raw
// EOF, so a receiver that only understands io.EOF would report a
// truncated relayed frame as a clean end.
//
// The path is forced by giving the picker a view whose direct path is
// unreachable, so the relay is the only way through.
func TestRealMachineMsgOverCapIsRefusedOverTheRelayPath(t *testing.T) {
	cfg, ok := readRealMachineConfig(t)
	if !ok {
		t.Skip("set CMESH_REALMACHINE_STATE, CMESH_REALMACHINE_PASS and CMESH_REALMACHINE_TARGET to run the real-machine check")
	}

	dev, err := identity.LoadDevice(identity.IdentityPath(cfg.stateDir, identity.DeviceFileName), cfg.pass)
	if err != nil {
		t.Fatalf("load the device: %v", err)
	}
	view := openRealMachineView(t, cfg, dev)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// The picker, with the direct path made unreachable so the relay
	// path is the one that carries the frame.
	picker := realMachinePicker(view, dev.ID())
	link, used, err := DialViaPickerPath(ctx, DialConfig{
		Device:           dev,
		Resolver:         view.resolver,
		Target:           cfg.target,
		Endpoints:        []string{"127.0.0.1:1"}, // unreachable: the relay must carry it
		Picker:           picker,
		Kind:             KindMsg,
		HandshakeTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial the target over the relay path: %v", err)
	}
	defer link.Close()
	t.Logf("the frame travelled over %s (via %s)", used.Kind, used.Via)

	// The over-cap frame, written by hand.
	if _, err := link.Session().Write(frameLengthPrefix(MaxMsgBytes + 1)); err != nil {
		t.Fatalf("write the over-cap frame: %v", err)
	}
	if err := link.Session().Close(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	_ = link.Session().SetReadDeadline(time.Now().Add(20 * time.Second))
	ack, err := readLine(bufio.NewReader(link.Session()), MaxHelloBytes)
	if err != nil {
		t.Fatalf("the receiver never answered: %v", err)
	}
	assertFailedAck(t, string(ack))
	t.Logf("the receiver answered %q", ack)
}
