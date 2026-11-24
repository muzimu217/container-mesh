package mesh

import (
	"context"
	"testing"
	"time"
)

// A payload corrupted on the wire must be REJECTED, not counted as a
// success.
//
// This is the probe for the bench's content check: the sending side is
// made to corrupt one byte after the payload is filled, so the receiver
// gets the right NUMBER of bytes and the wrong ones.
func TestACorruptedBenchPayloadIsRejected(t *testing.T) {
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

	orig := benchFillPattern
	benchFillPattern = func(buf []byte, offset int64) {
		orig(buf, offset)
		if len(buf) > 517 {
			buf[517] ^= 0x01
		}
	}
	defer func() { benchFillPattern = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = RunBench(ctx, DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.aline.ID(),
		Endpoints: l.BoundAddrs(),
	}, BenchConfig{Size: 4097})
	if err == nil {
		t.Fatal("the corrupted bench payload was accepted as a success")
	}
	t.Logf("rejected as it should be: %v", err)
}
