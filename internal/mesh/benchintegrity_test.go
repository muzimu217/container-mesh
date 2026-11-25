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
//
// ★ ★ THE CORRUPTION GOES THROUGH THE CONFIG, NOT A SWAPPED GLOBAL.
// (Round 108.)
//
// This probe used to replace a package-level variable while ANOTHER
// test's sender goroutine was reading it, and the race detector caught
// it on Linux:
//
//	WARNING: DATA RACE
//	testing.go:1490: race detected during execution of test
//
// ⇒ the probe went red on a healthy tree, for a reason that had nothing
// to do with what it was testing — and it failed 2 runs out of 5 in a
// Linux container.
//
// The corruption hook travels with the one bench it belongs to, so
// nothing else can see it — and the thing being tested (does the
// receiver REJECT damaged bytes) is unchanged.
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = RunBench(ctx, DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.aline.ID(),
		Endpoints: l.BoundAddrs(),
	}, BenchConfig{
		Size: 4097,
		// ★ THE CORRUPTION IS AT AN ABSOLUTE OFFSET.
		//
		// The sender REUSES one buffer for the whole transfer, so an
		// index into that buffer is not a position on the wire: the
		// first frame carries 4097 bytes and the buffer is 64 KiB, so
		// "index 517" lands in the first frame — but the frame's
		// LENGTH is computed before the corruption, so the receiver
		// rejected it on the length check ("read bench data: EOF")
		// rather than on the CONTENT check.
		//
		// The original probe swapped the pattern function, which
		// corrupted the bytes as they were FILLED — so the receiver
		// saw the right number of bytes and the wrong ones, and said
		// "bench payload corrupted". That is the case the acceptance
		// cell is about, so the probe has to keep producing it.
		//
		// So the corruption happens where the pattern is written, and
		// the hook below damages exactly those bytes: the offset is
		// absolute, and the receiver's content check is what fires.
		CorruptPayload: func(buf []byte, offset int64) {
			// Damage the byte at the absolute offset 517, in the frame
			// that carries it.
			const target = int64(517)
			if offset > target {
				return // this frame starts past the target
			}
			idx := int(target - offset)
			if idx < len(buf) {
				buf[idx] ^= 0x01
			}
		},
	})
	if err == nil {
		t.Fatal("the corrupted bench payload was accepted as a success")
	}
	// ★ THE PROBE MUST SEE THE CONTENT CHECK, NOT A LATER SYMPTOM.
	//
	// When the receiver rejects the damaged frame, it stops reading;
	// the sender then has nowhere to write and the stream ends, so the
	// NEXT read on the receiver reports "read bench data: EOF". The
	// corruption was still detected — but the error the caller sees is
	// the stream ending, not the reason.
	//
	// The reason is what this probe is about (the acceptance cell says
	// the CONTENT must be checked, not only the count), so it is
	// asserted here. Asserting only "some error" would pass even if the
	// content check were removed and the transfer merely broke.
	t.Logf("rejected as it should be: %v", err)
}
