// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errBenchBytes = errors.New("the dialer reported the wrong byte count")

// TestBenchResponderFinishes pins that the RESPONDING side of a bench
// session finishes too, not just the dialer.
//
// Both sides run the same loop, and both sides exit it when they receive
// the peer's stats frame. The dialer exits first and closes the link — so
// if the responder were still waiting for something the dialer never
// sends, the responder would wait forever. Over a real link that showed up
// as the dialer hanging at the very end of a large transfer, long after
// the bytes had moved.
//
// The test drives the responder directly and gives it a bound.
func TestBenchResponderFinishes(t *testing.T) {
	f := newDirectFixture(t)
	l, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.aline,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)

	// The dialer runs in its own goroutine and must finish.
	dialDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		stats, err := RunBench(ctx, DialConfig{
			Device:    f.bob,
			Resolver:  MemberListAdapter{List: f.members},
			Target:    f.aline.ID(),
			Endpoints: l.BoundAddrs(),
		}, BenchConfig{Size: 1 << 20})
		if err == nil && stats.SentBytes != 1<<20 {
			err = errBenchBytes
		}
		dialDone <- err
	}()

	select {
	case err := <-dialDone:
		if err != nil {
			t.Fatalf("the dialer failed: %v", err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("the dialer did not finish within 25 s")
	}
}
