// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// fakePicker records what it was asked and hands back a fixed path list.
type fakePicker struct {
	paths []Path
	err   error
	calls int
}

func (p *fakePicker) Pick(ctx context.Context, target identity.DeviceID, kind string) ([]Path, error) {
	p.calls++
	return p.paths, p.err
}

// fakeDialer stands in for the transport: it decides per path whether the
// link opens, and counts the attempts per path so a test can prove which
// branch ran.
type fakeDialer struct {
	fail   map[PathKind]error
	calls  map[PathKind]int
	opened *Link
}

func (d *fakeDialer) dial(ctx context.Context, cfg DialConfig, p Path) (*Link, error) {
	if d.calls == nil {
		d.calls = map[PathKind]int{}
	}
	d.calls[p.Kind]++
	if err, ok := d.fail[p.Kind]; ok {
		return nil, err
	}
	if d.opened != nil {
		return d.opened, nil
	}
	return &Link{}, nil
}

// TestPickerNilFallsBackToDirect: with no picker configured the dial path
// must be EXACTLY today's — a direct link, and a working one. This is the
// compatibility guarantee the whole picker abstraction rests on: nothing
// changes for a deployment that never configures a picker.
func TestPickerNilFallsBackToDirect(t *testing.T) {
	f := newDirectFixture(t)
	l := startListener(t, f)

	cfg := DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        l.BoundAddrs(),
		Kind:             KindPing,
		HandshakeTimeout: 5 * time.Second,
	}
	if cfg.Picker != nil {
		t.Fatal("a zero DialConfig must carry no picker")
	}
	link, kind, err := dialViaPicker(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dialViaPicker with a nil picker: %v", err)
	}
	t.Cleanup(func() { _ = link.Close() })
	if kind != PathDirect {
		t.Fatalf("a nil picker must dial direct, got %q", kind)
	}
	rtts, err := PingExchange(context.Background(), link, 3, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("ping over the direct link: %v", err)
	}
	if len(rtts) != 3 {
		t.Fatalf("got %d RTTs, want 3", len(rtts))
	}
}

// TestPickerDirectPreferred: when the picker names a direct path first and
// the direct path opens, the relay path must NOT be attempted. A picker
// that always walks its whole list would double the latency of every
// successful direct call.
func TestPickerDirectPreferred(t *testing.T) {
	picker := &fakePicker{paths: []Path{
		{Kind: PathDirect},
		{Kind: PathRelay, Via: identity.DeviceID("relay00000000000")},
	}}
	d := &fakeDialer{}
	paths, err := picker.Pick(context.Background(), "target0000000000", KindPing)
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := tryPaths(context.Background(), DialConfig{}, paths, d.dial)
	if err != nil {
		t.Fatalf("tryPaths: %v", err)
	}
	if got.Kind != PathDirect {
		t.Fatalf("the direct path must win, got %q", got.Kind)
	}
	if d.calls[PathRelay] != 0 {
		t.Fatalf("the relay path must not be attempted after a successful direct path, got %d attempts", d.calls[PathRelay])
	}
	if d.calls[PathDirect] != 1 {
		t.Fatalf("the direct path must be attempted exactly once, got %d", d.calls[PathDirect])
	}
}

// TestPickerRelaysWhenDirectUnreachable: nobody answered on the direct
// path — that is precisely the case a relay exists for. The path layer
// must move on to the relay candidate and report which relay it used.
func TestPickerRelaysWhenDirectUnreachable(t *testing.T) {
	relayID := identity.DeviceID("relay00000000000")
	paths := []Path{
		{Kind: PathDirect},
		{Kind: PathRelay, Via: relayID},
	}
	d := &fakeDialer{fail: map[PathKind]error{PathDirect: fmt.Errorf("quic dial 10.0.0.1:7000: %w", context.DeadlineExceeded)}}
	link, got, err := tryPaths(context.Background(), DialConfig{}, paths, d.dial)
	if err != nil {
		t.Fatalf("tryPaths must fall through to the relay path: %v", err)
	}
	if link == nil {
		t.Fatal("tryPaths must return a link")
	}
	if got.Kind != PathRelay {
		t.Fatalf("the relay path must be used after an unreachable direct path, got %q", got.Kind)
	}
	if got.Via != relayID {
		t.Fatalf("the relay path must name the relay device, got %q", got.Via)
	}
}

// TestPickerAuthFailureDoesNotFallBack: the credentials were rejected.
// Retrying the same credentials over another path repeats the same
// failure and leaks the attempt, so the error must surface unchanged and
// the relay path must not be touched.
func TestPickerAuthFailureDoesNotFallBack(t *testing.T) {
	paths := []Path{
		{Kind: PathDirect},
		{Kind: PathRelay, Via: identity.DeviceID("relay00000000000")},
	}
	d := &fakeDialer{fail: map[PathKind]error{PathDirect: fmt.Errorf("read ack: %w", ErrAuth)}}
	_, _, err := tryPaths(context.Background(), DialConfig{}, paths, d.dial)
	if err == nil {
		t.Fatal("an auth failure must surface as an error")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("the auth class must survive, got %v", err)
	}
	if d.calls[PathRelay] != 0 {
		t.Fatalf("an auth failure must not fall back to the relay path, got %d attempts", d.calls[PathRelay])
	}
}

// TestPickerRefusedDoesNotFallBack: the peer answered and declined. The
// answer does not change on another path, so the refusal surfaces as-is.
func TestPickerRefusedDoesNotFallBack(t *testing.T) {
	paths := []Path{
		{Kind: PathDirect},
		{Kind: PathRelay, Via: identity.DeviceID("relay00000000000")},
	}
	d := &fakeDialer{fail: map[PathKind]error{PathDirect: fmt.Errorf("listener refused: %w", ErrRefused)}}
	_, _, err := tryPaths(context.Background(), DialConfig{}, paths, d.dial)
	if err == nil {
		t.Fatal("a refusal must surface as an error")
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("the refusal class must survive, got %v", err)
	}
	if d.calls[PathRelay] != 0 {
		t.Fatalf("a refusal must not fall back to the relay path, got %d attempts", d.calls[PathRelay])
	}
}

// TestPickerNoRouteFallsBack: "no route at all" on one candidate means
// try the next one — that is the whole point of having candidates.
func TestPickerNoRouteFallsBack(t *testing.T) {
	relayID := identity.DeviceID("relay00000000000")
	paths := []Path{
		{Kind: PathDirect},
		{Kind: PathRelay, Via: relayID},
	}
	d := &fakeDialer{fail: map[PathKind]error{PathDirect: fmt.Errorf("%w: no endpoints", ErrNoRoute)}}
	_, got, err := tryPaths(context.Background(), DialConfig{}, paths, d.dial)
	if err != nil {
		t.Fatalf("tryPaths must fall through to the relay path: %v", err)
	}
	if got.Via != relayID {
		t.Fatalf("the relay path must be used, got %q", got.Via)
	}
}

// TestPickerAllPathsFail: every candidate failed — the caller must get the
// LAST failure, and it must keep its class.
func TestPickerAllPathsFail(t *testing.T) {
	paths := []Path{
		{Kind: PathDirect},
		{Kind: PathRelay, Via: identity.DeviceID("relay00000000000")},
	}
	d := &fakeDialer{fail: map[PathKind]error{
		PathDirect: fmt.Errorf("quic dial: %w", context.DeadlineExceeded),
		PathRelay:  fmt.Errorf("relay dial: %w", context.DeadlineExceeded),
	}}
	_, _, err := tryPaths(context.Background(), DialConfig{}, paths, d.dial)
	if err == nil {
		t.Fatal("tryPaths must fail when every path fails")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("the unreachable class must survive, got %v", err)
	}
}

// TestDirectFirstPickerOrder: the shipped picker names the direct path
// first and the relay candidates after it, in the order it was given.
func TestDirectFirstPickerOrder(t *testing.T) {
	relays := []Path{
		{Kind: PathRelay, Via: identity.DeviceID("relay00000000000")},
		{Kind: PathRelay, Via: identity.DeviceID("relay00000000001")},
	}
	p := DirectFirstPicker{Direct: []string{"127.0.0.1:7000"}, Relays: relays}
	paths, err := p.Pick(context.Background(), "target0000000000", KindPing)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 {
		t.Fatalf("got %d paths, want 3", len(paths))
	}
	if paths[0].Kind != PathDirect {
		t.Fatalf("the direct path must come first, got %q", paths[0].Kind)
	}
	if paths[1].Via != relays[0].Via || paths[2].Via != relays[1].Via {
		t.Fatalf("the relay candidates must keep their order, got %+v", paths)
	}
}
