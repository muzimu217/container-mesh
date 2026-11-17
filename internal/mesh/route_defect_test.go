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

// TestRelayPathIsNotReportedAsNoRoute pins the class of the "relay paths
// are not available yet" failure.
//
// ErrNoRoute means "there is no route to this target", and the fallback
// rule reads it as "try the next path". The relay path is not that: the
// relay trunk has not been built yet, so trying another path cannot help,
// and a caller that sees NoRoute may keep walking a list for nothing.
//
// The class has to be one the fallback rule stops on, and it has to say
// what actually happened.
func TestRelayPathIsNotReportedAsNoRoute(t *testing.T) {
	cfg := DialConfig{Target: "target"}
	_, err := dialPath(context.Background(), cfg, Path{Kind: PathRelay})
	if err == nil {
		t.Fatal("the relay path succeeded with no relay trunk in place")
	}
	if errors.Is(err, ErrNoRoute) {
		t.Fatalf("the unimplemented relay path reports ErrNoRoute (%v): that "+
			"class means 'no route', and the fallback rule would keep walking "+
			"paths that cannot work either", err)
	}
	if !errors.Is(err, ErrRelayUnavailable) {
		t.Fatalf("the unimplemented relay path reports %v, want ErrRelayUnavailable: "+
			"it needs its own class, because ErrNoRoute is a routing fact and "+
			"ErrSessionClosed is a session fact, and neither describes a trunk "+
			"that does not exist", err)
	}
}

// TestTryPathsReportsThePathThatFailed pins which path the caller is told
// about.
//
// The CLI reports the path it took, and a caller that wants to say "the
// direct path did not work and the relay did not either" needs to know
// which one it was on. tryPaths returned an empty Path on failure, so that
// information was thrown away.
func TestTryPathsReportsThePathThatFailed(t *testing.T) {
	cfg := DialConfig{Target: "target"}
	paths := []Path{
		{Kind: PathDirect, Endpoints: []string{"198.51.100.1:7000"}},
		{Kind: PathRelay, Via: "relay-one"},
	}
	dial := func(_ context.Context, _ DialConfig, p Path) (*Link, error) {
		return nil, fmt.Errorf("%w: mesh: cannot reach %s", ErrUnreachable, p.Kind)
	}
	_, failed, err := tryPaths(context.Background(), cfg, paths, dial)
	if err == nil {
		t.Fatal("tryPaths succeeded although every path failed")
	}
	if failed.Kind == "" {
		t.Fatalf("tryPaths returned an empty path on failure: the caller "+
			"cannot report which path was tried (%v)", err)
	}
	if failed.Kind != PathRelay {
		t.Fatalf("tryPaths reported the failed path as %q, want the last one "+
			"tried (%q)", failed.Kind, PathRelay)
	}
}

// TestPickerIsNotAskedAboutANonMember pins the membership check's position.
//
// The picker is told which device the caller wants to reach. If that device
// is not a member, the answer must not reach the picker at all: a picker
// hands the target's identity to a relay, and a revoked device's identity
// has no business being sent anywhere.
func TestPickerIsNotAskedAboutANonMember(t *testing.T) {
	f := newDirectFixture(t)
	// A device that is NOT in the member list.
	outsider, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	asked := false
	picker := pickerFunc(func(_ context.Context, _ identity.DeviceID, _ string) ([]Path, error) {
		asked = true
		return nil, nil
	})

	cfg := DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           outsider.ID(),
		Picker:           picker,
		HandshakeTimeout: time.Second,
	}
	_, _, err = dialViaPicker(context.Background(), cfg)
	if err == nil {
		t.Fatal("the dial succeeded for a device that is not a member")
	}
	if asked {
		t.Fatal("the picker was asked about a device that is not a member: " +
			"the target's identity would have been handed to a relay")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("the failure is %v, want an auth-class error", err)
	}
}

// pickerFunc adapts a function to the PathPicker interface.
type pickerFunc func(context.Context, identity.DeviceID, string) ([]Path, error)

func (f pickerFunc) Pick(ctx context.Context, target identity.DeviceID, kind string) ([]Path, error) {
	return f(ctx, target, kind)
}
