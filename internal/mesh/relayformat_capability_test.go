// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// ---------------------------------------------------------------------------
// THE CAPABILITY QUESTION, AGAIN.
//
// The round-81 defect was this: a leg's format is asked of it with
// relayFormatOfSession, which asks for the ALPN the leg's connection
// negotiated. A CONNECTION can say. A relayed SESSION could not — so the
// question came back empty and the caller FELL BACK TO THE BINARY FRAME,
// the most dangerous of the two defaults. A leg that had negotiated the
// old protocol was then read as binary while it wrote JSON, and the
// reader found text where a length prefix belongs.
//
// The fix added ALPN() to relaySession. An adversarial review of the
// switch found TWO MORE PLACES WHERE THE SAME QUESTION COULD COME BACK
// EMPTY, and both are fixed here:
//
//   1. ServeRequesterWithFirst HARDCODED the binary frame instead of
//      asking the leg. It is not on the production path today (the
//      listener calls ServeRequesterWithFirstFormat with the negotiated
//      format), but it is an EXPORTED API: a caller that served a
//      requester whose leg negotiated the old protocol read the open
//      record as binary and wrote the ANSWER record in binary.
//
//   2. relayTargetSession had NO ALPN() at all, so it could not say its
//      format either. It is handed to the business layer as a Session,
//      and a Session that cannot say its format is exactly the shape
//      that caused the round-81 defect.
//
// The rule these tests pin: EVERY type that can be handed back as a
// Session says its format. The capability is asked for by type
// assertion; a session that cannot say (the test pipes) falls back to
// the binary frame, which is the same-version deployment's default.
// ---------------------------------------------------------------------------

// TestEverySessionTypeThatCanBeHandedBackCanSayItsFormat is the
// compile-time half of the rule.
//
// A session that CANNOT say its format is the round-81 defect waiting
// to happen. This assertion makes the list of session types explicit:
// every one of them that reaches relayFormatOfSession must implement
// alpnSession.
//
// The pipes are deliberately EXCLUDED: they are the test double, and
// "a pipe cannot say" is the documented fallback. What must not exist
// is a PRODUCTION session type that cannot say.
func TestEverySessionTypeThatCanBeHandedBackCanSayItsFormat(t *testing.T) {
	// The three production session types: the QUIC connection (the
	// leg's own connection), the relayed session (the requester's half
	// of the switch) and the target session (the target's half).
	var (
		_ alpnSession = &quicSession{}
		_ alpnSession = &relaySession{}
		_ alpnSession = &relayTargetSession{}
	)
}

// TestTheTargetSessionSaysItsOwnFormat is the runtime half: the target
// session reports the format it actually speaks.
//
// Its format comes from the ALPN the connection to the trunk negotiated
// (see ServeRelayTarget), so it can say it truthfully.
func TestTheTargetSessionSaysItsOwnFormat(t *testing.T) {
	legacy := &relayTargetSession{format: RelaySpliceFormatJSON}
	if got := legacy.ALPN(); got != RelayALPN {
		t.Fatalf("a JSON target session reports the ALPN %q, want %q", got, RelayALPN)
	}
	modern := &relayTargetSession{format: RelaySpliceFormatBinary}
	if got := modern.ALPN(); got != RelayALPN2 {
		t.Fatalf("a binary target session reports the ALPN %q, want %q", got, RelayALPN2)
	}

	// And the format question must find it.
	got, err := relayFormatOfSession(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got != RelaySpliceFormatJSON {
		t.Fatalf("a JSON target session is read as %v, want JSON", got)
	}
}

// TestTheRequesterEntryPointAsksTheLegRatherThanAssuming is the
// regression test for the exported API that guessed.
//
// The symptom it prevents is the round-81 one: "the join stream broke:
// mesh: frame of 1937009004 bytes exceeds the 1048576 byte cap", where
// 1937009004 is 0x7374696C — the four ASCII bytes "stil" read as a
// big-endian length. A reader on the WRONG format finds text where a
// length prefix belongs.
//
// (The frame length is BIG endian, so the bytes on the wire were
// 73 74 69 6C = "stil"; the little-endian spelling "lits" is what you
// get if you read the number the wrong way round, and it is what a
// log grep would wrongly search for.)
//
// So: a requester whose leg speaks JSON is served through
// ServeRequesterWithFirst, and the open record it writes must be READ
// as JSON. Before the fix, the entry point hardcoded the binary frame,
// so the read failed at the very first record.
func TestTheRequesterEntryPointAsksTheLegRatherThanAssuming(t *testing.T) {
	f := newTrunkFixture(t)

	// A requester leg that negotiated the OLD protocol.
	reqEnd, reqPeer := newALPNPipePair(RelayALPN)

	// A target the trunk can dial.
	tgtEnd, tgtPeer := newALPNPipePair(RelayALPN2)
	_ = tgtPeer

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: NewJoinerTable(),
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tgtEnd, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The requester writes its open record in JSON — the format its leg
	// negotiated.
	open := relayRecord{
		Type: uint8(wire.FrameMeshRelayOpen),
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}
	if err := writeRelayRecordAs(reqPeer, open, RelaySpliceFormatJSON); err != nil {
		t.Fatal(err)
	}

	// The requester is served through the entry point that used to
	// hardcode the binary frame.
	done := make(chan error, 1)
	go func() {
		done <- trunk.ServeRequester(context.Background(), reqEnd, trunk.joiners)
	}()

	// The trunk must have read the open record in JSON. If it read it
	// as binary it would fail at the first record, and the failure
	// would be the "frame of N bytes exceeds the cap" shape.
	select {
	case err := <-done:
		if err != nil && (containsFrameTooLarge(err) || containsBadMagic(err)) {
			t.Fatalf("the requester's leg was read with the WRONG format: %v "+
				"(a reader on the wrong format finds text where a length prefix belongs)",
				err)
		}
		// Any other error is the test's own arrangement (the target
		// is a pipe with nobody on it), not the format bug.
	case <-time.After(2 * time.Second):
		// Still serving: the format was read correctly and the relay
		// is running. That is the pass.
	}
}

// TestTheTargetSessionNeverGuessesItsFormatWhenItCannotSay guards the
// fallback itself: a session that cannot say falls back to the binary
// frame — and the fallback is a DELIBERATE default, not an accident.
//
// The rule from the round-81 lesson: when a capability cannot be
// queried, the default must be the SAFE one. Here the safe default is
// the binary frame, because it is the same-version deployment's format
// and the format that can hold the most payload. What must NOT happen
// is a silent, per-record re-derivation.
func TestTheTargetSessionNeverGuessesItsFormatWhenItCannotSay(t *testing.T) {
	// A pipe cannot say: the question comes back empty.
	got, err := relayFormatOfSession(&pipeSession{})
	if err != nil {
		t.Fatal(err)
	}
	if got != RelaySpliceFormatBinary {
		t.Fatalf("a session that cannot say its format is read as %v, want the binary frame", got)
	}
}

// ---------------------------------------------------------------------------

func containsFrameTooLarge(err error) bool {
	return err != nil && strings.Contains(err.Error(), "exceeds the")
}

func containsBadMagic(err error) bool {
	return err != nil && strings.Contains(err.Error(), "magic")
}

// ---------------------------------------------------------------------------
// The rate budget's wait must be interruptible.
//
// flowQuota.Reserve slept for the whole wait and checked the flow's end
// AFTERWARDS, so a flow that ended while the splice was waiting for the
// budget kept its forwarding goroutine for the entire wait — up to a
// full record's worth of refill (about 15 s at a 64 KiB/s cap). The
// check after the sleep could report the end, but it could never cut
// the wait short.

func TestTheRateBudgetWaitIsInterruptible(t *testing.T) {
	f := newTrunkFixture(t)

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Quota:   RelayQuota{MaxBytesPerSec: 1 << 20}, // 1 MiB/s
	})
	if err != nil {
		t.Fatal(err)
	}

	flow := &relayFlow{id: 1, done: make(chan struct{})}
	q := &flowQuota{trunk: trunk, flow: flow}

	// Drain the bucket so the next charge must wait: the bucket starts
	// with one second's worth of tokens, and a charge of the same size
	// empties it.
	trunk.accountForward(1 << 20)

	// The flow ends while the reserve is waiting.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(flow.done)
	}()

	start := time.Now()
	_, err = q.Reserve(1 << 20)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the reserve returned no error after the flow ended")
	}
	if !errors.Is(err, ErrRelaySpliceFlowEnded) {
		t.Fatalf("the reserve failed with %v, want the flow-ended error", err)
	}
	// The wait for a 1 MiB payload at 1 MiB/s is about a second. The
	// reserve must come back as soon as the flow ends — well under that,
	// not a second later.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("the reserve took %v to notice the flow had ended; the wait is "+
			"not interruptible, so a dead flow keeps its goroutine for the whole budget",
			elapsed)
	}
}

// TestTheReserveStillWaitsWhenTheFlowIsAlive guards the OTHER side of
// the change: the wait must still happen. A reserve that returned
// immediately would let the relay exceed its rate budget.
func TestTheReserveStillWaitsWhenTheFlowIsAlive(t *testing.T) {
	f := newTrunkFixture(t)

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Quota:   RelayQuota{MaxBytesPerSec: 1 << 20}, // 1 MiB/s
	})
	if err != nil {
		t.Fatal(err)
	}

	flow := &relayFlow{id: 1, done: make(chan struct{})}
	q := &flowQuota{trunk: trunk, flow: flow}

	// Drain it.
	trunk.chargeBytes(flow, 1<<20)

	start := time.Now()
	wait, err := q.Reserve(1 << 19) // 512 KiB at 1 MiB/s → about 0.5 s
	if err != nil {
		t.Fatalf("the reserve failed with %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("the reserve returned after %v without waiting for the budget; "+
			"the relay would exceed its rate cap", elapsed)
	}
	if wait != 0 {
		t.Fatalf("a successful reserve reports %v, want 0", wait)
	}
	close(flow.done)
}
