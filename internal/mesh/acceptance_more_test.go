// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

// ---------------------------------------------------------------------------
// M4-4: THE ACCEPTANCE GUARDS THE LEDGER SAID WERE MISSING.
//
// The M4 acceptance list (docs/design/M4-ACCEPTANCE.md §2.2, §2.3, §2.5)
// names eight cells whose only evidence was a PRECONDITION of some other
// test, a "the teardown happens" assertion with the timing deliberately
// widened, or nothing at all:
//
//	B8   run @cat: 4095 / 4096 / 4097 / 65536 / 1048576 bytes, byte exact
//	B10  bench: 4096 / 1 MiB / 8 MiB / 32 MiB and the NON-ALIGNED five,
//	     sent == received in both directions
//	C1   a revoked device's ESTABLISHED direct session is destroyed within
//	     ONE PUSH ROUND-TRIP
//	C2   a revoked member's LIVE RELAY flow is ABORTED, not half-closed
//	E2   a pure-outbound member joins the trunk with BOTH flags
//	E5   3 MiB over the relay — real-machine hairpin, NOT reproducible here
//	E6   after a revocation the REQUESTER sees an actionable refusal reason
//	E7   with the relay available, a ping to a member WITH an endpoint still
//	     reports link=direct
//
// Every test in this file pins ONE observable, and the failure message says
// what was expected and what was seen.
//
// ★ NO WALL-CLOCK THRESHOLDS. (CONVENTIONS.md T-2.4.) "Within one push
// round-trip" is a BOUNDED WAIT: the wait returns as soon as the condition
// holds, and the timeout says which budget it was waiting for and where
// that budget comes from. It is not "the teardown must happen within N
// milliseconds" — that is a stopwatch, and a stopwatch makes a healthy
// machine flaky.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// ---------------------------------------------------------------------------
// B8 — `run @cat` MUST MOVE THE FIVE NAMED SIZES BYTE FOR BYTE.
//
// The acceptance cell names five sizes: 4095 / 4096 / 4097 / 65536 /
// 1048576. The four interesting ones are the ones that sit ON or NEXT TO a
// buffer boundary: 4096 is exactly 4 KiB, 4097 is one byte over it, 65536
// is exactly 64 KiB and 1048576 is exactly 1 MiB.
//
// The guard that existed before this one covered ONE size —
// MaxHelloBytes+1024 = 5120 bytes — which is not on any of those
// boundaries. So the sizes the cell names had never been run.
//
// The assertion is BYTE EXACT: the bytes the remote @cat produced must be
// the bytes that were written, not merely the same COUNT. A count check
// would pass on a transfer that duplicated a chunk and dropped another.
// ---------------------------------------------------------------------------

// b8CatSizes are the five sizes the acceptance cell names.
var b8CatSizes = []int{4095, 4096, 4097, 65536, 1048576}

// TestRunCatMovesEveryAcceptanceSizeByteExact runs `run @cat` at each of
// the five named sizes and compares the result with the file byte for
// byte.
func TestRunCatMovesEveryAcceptanceSizeByteExact(t *testing.T) {
	for _, size := range b8CatSizes {
		size := size
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			f := newDirectFixture(t)
			l := startListener(t, f)

			// The payload is a POSITION-DEPENDENT pattern, so a transfer
			// that mixed up two chunks of the same length cannot pass.
			want := make([]byte, size)
			for i := range want {
				want[i] = byte(i*31 + i/251)
			}
			path := filepath.Join(t.TempDir(), "cat-payload.bin")
			if err := os.WriteFile(path, want, 0o600); err != nil {
				t.Fatal(err)
			}

			link, err := DialDirect(context.Background(), DialConfig{
				Device:           f.bob,
				Resolver:         MemberListAdapter{List: f.members},
				Target:           f.aline.ID(),
				Endpoints:        l.BoundAddrs(),
				Kind:             KindRun,
				HandshakeTimeout: 15 * time.Second,
			})
			if err != nil {
				t.Fatalf("the run session could not be opened: %v", err)
			}
			defer link.Close()

			request, err := json.Marshal(RunRequest{Verb: "@cat", Args: []string{path}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(link.Stream(), "%s\n", request); err != nil {
				t.Fatal(err)
			}

			var got bytes.Buffer
			for {
				tag, payload, err := readRunFrame(link.br)
				if err != nil {
					t.Fatalf("the run stream ended before the exit frame: %v", err)
				}
				switch tag {
				case RunTagOut:
					got.Write(payload)
				case RunTagErr:
					t.Fatalf("the remote @cat failed: %s", payload)
				case RunTagExit:
					if len(payload) < 4 || binary.BigEndian.Uint32(payload) != 0 {
						t.Fatalf("the remote @cat exit payload is %v, want exit code 0", payload)
					}
					if got.Len() != size {
						t.Fatalf("the remote @cat returned %d bytes, want %d: "+
							"the size the acceptance cell names must arrive COMPLETELY",
							got.Len(), size)
					}
					if !bytes.Equal(got.Bytes(), want) {
						t.Fatalf("the remote @cat returned %d bytes that are NOT the file's bytes: "+
							"the first difference is at offset %d (the payload is a position-dependent "+
							"pattern, so a mismatch means the bytes were mangled, not merely counted wrong)",
							size, firstDifference(got.Bytes(), want))
					}
					return
				default:
					t.Fatalf("unknown run tag %q", tag)
				}
			}
		})
	}
}

// firstDifference returns the offset of the first byte that differs, or -1.
func firstDifference(got, want []byte) int {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// B10 — `bench` MUST MOVE BOTH FAMILIES OF SIZE, IN BOTH DIRECTIONS.
//
// The acceptance cell names ten sizes:
//
//	aligned:     4096 / 1 MiB / 8 MiB / 32 MiB
//	non-aligned: 4095 / 4097 / 65537 / 1048577 / 33554431
//
// The guard that existed before this one covered ONLY the non-aligned five.
// The aligned four — including the two big ones — had never been run by
// any default test, so "the bench moves a megabyte" was a claim with
// nothing behind it.
//
// The assertion is on the BYTE COUNTS IN BOTH DIRECTIONS: `sent` and
// `received` must each equal the size. The bench's own content check
// (the payload is a deterministic function of its offset) is what makes
// the count mean something: a transfer that arrived corrupted is
// REJECTED by the receiver rather than counted.
//
// ★ THE BIG SIZES ARE SLOW ENOUGH TO MATTER, so the budget is explicit.
// A bench of 32 MiB in each direction over loopback takes well under a
// second; the budget below is many times that, and it exists so a
// HUNG bench fails instead of waiting for the package's own timeout.
// ---------------------------------------------------------------------------

// b10AlignedSizes and b10NonAlignedSizes are the two families the
// acceptance cell names.
var (
	b10AlignedSizes    = []int64{4096, 1 << 20, 8 << 20, 32 << 20}
	b10NonAlignedSizes = []int64{4095, 4097, 65537, 1048577, 33554431}
)

// TestBenchMovesTheAlignedSizesIntact runs the bench at the ALIGNED sizes
// the acceptance cell names.
//
// The sizes that were already covered (the non-aligned five) are in
// bench_sizes_test.go; this file adds the family that had no guard at all.
func TestBenchMovesTheAlignedSizesIntact(t *testing.T) {
	for _, size := range b10AlignedSizes {
		size := size
		t.Run(sizeName(size), func(t *testing.T) {
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

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			stats, err := RunBench(ctx, DialConfig{
				Device:    f.bob,
				Resolver:  MemberListAdapter{List: f.members},
				Target:    f.aline.ID(),
				Endpoints: l.BoundAddrs(),
			}, BenchConfig{Size: size})
			if err != nil {
				t.Fatalf("the bench at %d bytes failed: %v", size, err)
			}
			if stats.SentBytes != size || stats.ReceivedBytes != size {
				t.Fatalf("the bench at %d bytes moved sent=%d received=%d: "+
					"both directions must move the whole size",
					size, stats.SentBytes, stats.ReceivedBytes)
			}
		})
	}
}

// TestBenchMovesEveryAcceptanceSizeIntact runs the bench at EVERY size the
// acceptance cell names — the aligned four AND the non-aligned five — and
// checks both directions of each.
//
// The two families fail for different reasons, and both are worth having in
// one table:
//
//   - an aligned size can hit a buffer boundary exactly, so a chunked
//     writer that mis-sizes its last chunk loses a byte at the boundary;
//   - a non-aligned size can hit a chunked writer's tail, so the last
//     chunk is short and the length bookkeeping goes wrong.
//
// The bench's receiver REJECTS a corrupted payload (the payload is a
// deterministic function of its offset), so a successful run means the
// bytes were right and not merely numerous.
func TestBenchMovesEveryAcceptanceSizeIntact(t *testing.T) {
	sizes := make([]int64, 0, len(b10AlignedSizes)+len(b10NonAlignedSizes))
	sizes = append(sizes, b10AlignedSizes...)
	sizes = append(sizes, b10NonAlignedSizes...)

	for _, size := range sizes {
		size := size
		t.Run(sizeName(size), func(t *testing.T) {
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

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			stats, err := RunBench(ctx, DialConfig{
				Device:    f.bob,
				Resolver:  MemberListAdapter{List: f.members},
				Target:    f.aline.ID(),
				Endpoints: l.BoundAddrs(),
			}, BenchConfig{Size: size})
			if err != nil {
				t.Fatalf("the bench at %d bytes failed: %v", size, err)
			}
			if stats.SentBytes != size {
				t.Fatalf("the bench at %d bytes sent %d bytes, want %d: "+
					"the size the acceptance cell names must go out COMPLETELY",
					size, stats.SentBytes, size)
			}
			if stats.ReceivedBytes != size {
				t.Fatalf("the bench at %d bytes received %d bytes, want %d: "+
					"the size the acceptance cell names must come back COMPLETELY",
					size, stats.ReceivedBytes, size)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// C1 — A REVOKED DEVICE'S ESTABLISHED DIRECT SESSION IS DESTROYED WITHIN
// ONE PUSH ROUND-TRIP.
//
// The requirement (docs/REQUIREMENTS.md R-1.3) says: "撤销后其已建立的会话
// 在一个推送往返内被销毁（不只是拒绝新连接）" — and it gives the
// judgeable form: "撤销广播后 ≤ 30 s 内旧会话拆毁、新握手被拒；机制是
// 发现节点推送，30 s 是测试断言的宽限上限".
//
// The guard that existed before this one waited TEN seconds and said so:
// "asserts that the teardown HAPPENS, not how fast". The timing was
// deliberately widened, so the cell had no time bound at all.
//
// ★ THE BOUND HERE IS A BOUNDED WAIT, NOT A STOPWATCH.
//
// The mechanism is the watch: the listener re-reads the member view on a
// one-second ticker, because the push of a new member view can take up to
// a second (the snapshot is coalesced). So the honest bound is "the
// teardown happens once the watch has seen the revocation" — and the
// budget below is the REQUIREMENT'S OWN budget (30 s, quoted above),
// with the wait returning the moment the condition holds.
//
// The assertion that matters is not the clock: it is that the session is
// DEAD, and that it is dead with a REASON the operator can read.
// ---------------------------------------------------------------------------

// c1RevocationBudget is the requirement's own grace bound (R-1.3: "撤销
// 广播后 ≤ 30 s 内旧会话拆毁"). It is a budget for a BOUNDED WAIT, not a
// performance threshold: the wait returns as soon as the session is dead.
const c1RevocationBudget = 30 * time.Second

// TestARevokedDeviceLosesItsDirectSessionWithinOnePushRoundTrip pins C1.
//
// The session is established FIRST, then the member is revoked, and the
// established session must die — with the reason the product gives, not a
// bare EOF.
func TestARevokedDeviceLosesItsDirectSessionWithinOnePushRoundTrip(t *testing.T) {
	f := newDirectFixture(t)
	l := startListener(t, f)

	// The session is up and working.
	link := dialTo(t, f, l.BoundAddrs(), KindPing)
	rtts, err := PingExchange(context.Background(), link, 1, 0)
	if err != nil || len(rtts) != 1 {
		t.Fatalf("the pre-revocation ping failed: %v", err)
	}

	// Bob leaves the member list — that is what `cred revoke` does to the
	// roster.
	if err := f.members.Remove(f.bob.ID()); err != nil {
		t.Fatal(err)
	}

	// The teardown must happen. The wait is bounded by the requirement's
	// own budget, and it returns as soon as the session is dead.
	deadline := time.Now().Add(c1RevocationBudget)
	var lastErr error
	for {
		_, lastErr = PingExchange(context.Background(), link, 1, 0)
		if lastErr != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the revoked device's established direct session is still alive after %v "+
				"(the requirement's own grace bound, R-1.3: a revocation destroys the ACTIVE sessions "+
				"within one push round-trip, it does not only refuse the next handshake): the last ping "+
				"still succeeded", c1RevocationBudget)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The teardown must carry a REASON. A bare EOF says "the link ended",
	// which is exactly the class of message this project has been fixing:
	// the operator cannot tell a revocation from a network fault.
	//
	// The reason is the listener's own words ("this device is no longer a
	// member"), delivered as a QUIC application error.
	var appErr *quic.ApplicationError
	if !errors.As(lastErr, &appErr) {
		t.Fatalf("the revoked session ended with %v (%T), want a QUIC application error "+
			"carrying the revocation reason: a bare EOF tells the operator nothing about WHY "+
			"the session died", lastErr, lastErr)
	}
	if !strings.Contains(appErr.Error(), "no longer a member") {
		t.Fatalf("the revocation reason is %q, want it to say the device is no longer a member: "+
			"the requester must be able to act on the reason", appErr.Error())
	}
}

// ---------------------------------------------------------------------------
// C2 — A REVOKED MEMBER'S LIVE RELAY FLOW IS ABORTED, NOT HALF-CLOSED.
//
// The acceptance cell says: "中转活流被强制拆掉（abort，非 FIN）".
//
// The difference is not cosmetic. A FIN is a HALF close: the peer is asked
// to stop, and it may keep the flow alive by ignoring the request. An
// abort (a QUIC RESET_STREAM) makes the read FAIL — the peer's pending
// read returns an error instead of a clean end.
//
// The guard that existed before this one asserted only that the flow
// COUNT went back to zero. A flow that is closed politely satisfies that
// too.
//
// ★ THE ABORT IS OBSERVED AT THE WIRE. The reader on the far side must
// see a CANCELLED STREAM, not an EOF. A QUIC stream error is the only
// thing that says "this was torn down" as opposed to "this ended".
// ---------------------------------------------------------------------------

// TestARevokedMemberLosesItsRelayFlowWithAnAbortNotAFin pins C2.
//
// The flow is a REAL one: a real requester opens a relayed session
// through the real listener, over a real QUIC connection, and the real
// revocation watch tears it down.
func TestARevokedMemberLosesItsRelayFlowWithAnAbortNotAFin(t *testing.T) {
	f := newRelayFixture(t)
	f.startHub(t, true)

	// The target joins the trunk and HOLDS the relayed session, so the
	// only thing that can end it is the trunk's teardown.
	targetSess := f.joinBobHolding(t)

	requester, err := OpenRelay(context.Background(), RelayRequesterConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Kind:      KindPing,
		TargetKey: f.bob.PublicKey(),
		Relay:     f.hub.ID(),
		Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
			link, err := DialRelaySession(ctx, f.alice, MemberListAdapter{List: f.members}, relay, []string{f.hubAddr})
			if err != nil {
				return nil, err
			}
			return link.Session(), nil
		},
	})
	if err != nil {
		t.Fatalf("the relayed session could not be opened: %v", err)
	}
	defer requester.Close()

	select {
	case <-targetSess:
	case <-time.After(relayHandshakeWait):
		t.Fatal("the target never received the relayed session")
	}

	// The requester is revoked, and the watch tears the live flow down.
	if err := f.members.Remove(f.alice.ID()); err != nil {
		t.Fatal(err)
	}
	trunk := f.trunk
	trunk.StartRevocationWatch(context.Background(), 20*time.Millisecond)

	// The flow must be gone.
	deadline := time.Now().Add(c1RevocationBudget)
	for trunk.FlowCount() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the revoked member's relay flow is still live after %v: a revocation must "+
				"tear the ESTABLISHED relay down, not only refuse its next request", c1RevocationBudget)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And it must have been torn down with an ABORT.
	//
	// The requester is the end that reads the relay's records, so its
	// next read is where the teardown shows up. A FIN would read as a
	// clean end; an abort reads as a CANCELLED STREAM.
	buf := make([]byte, 64)
	_ = requester.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err = requester.Read(buf)
	if err == nil {
		t.Fatal("the requester read from a relay flow that was torn down: the teardown must END " +
			"the flow, not merely stop carrying bytes")
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("the requester saw a clean END (%v): a revoked member's relay flow must be "+
			"ABORTED (a cancelled stream), not half-closed with a FIN — a peer that ignores a FIN "+
			"could keep the flow alive", err)
	}
	var streamErr *quic.StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("the requester saw %v (%T), want a QUIC stream error (a cancelled stream): "+
			"that is what distinguishes an ABORT from a FIN", err, err)
	}
}

// ---------------------------------------------------------------------------
// E6 — AFTER THE REVOCATION THE REQUESTER SEES AN ACTIONABLE REASON.
//
// The acceptance cell says: "撤销后中转流被拆；请求方看到可操作的拒绝理由".
//
// The teardown is the half that already had a guard. The OTHER half — what
// the REQUESTER is told — had none: the refusal reasons asserted elsewhere
// in this package belong to the "the target never joined the trunk" case,
// not to the revocation.
//
// ★ WHAT COUNTS AS ACTIONABLE.
//
// Two things have to hold:
//
//  1. The requester must be TOLD that the relay was refused on
//     MEMBERSHIP grounds, and the reason must name the member that left.
//     "stream 0 canceled by remote" is a transport fact: it says the link
//     died and nothing about why, so the operator cannot act on it.
//
//  2. The reason must survive the SENTINEL: the refusal's CLASS is what
//     the path layer branches on (an auth-class refusal is a VERDICT — do
//     not try another relay with the same credentials; a local condition
//     is retryable), so the class and the text have to arrive TOGETHER.
//
// ★ THE KNOWN GAP IS ASSERTED, NOT PAPERED OVER.
//
// The revocation teardown of an ALREADY-OPEN flow reaches the requester as
// a bare transport error: the trunk aborts both legs and the reason
// ("a member of this relay was revoked") stays on the anchor's log. The
// test below therefore asserts the two halves SEPARATELY:
//
//   - the flow is torn down and the requester's next request is refused
//     with the actionable reason (this is the guard);
//   - the anchor's log carries the same reason (this is the guard).
//
// And the gap itself is pinned by
// TestTheRevokedRelayFlowTearsDownWithoutTellingTheRequesterWhy, which
// asserts the CURRENT behaviour and says what is missing — so the ledger
// can record it honestly instead of claiming a cell that is only half
// closed.
// ---------------------------------------------------------------------------

// TestTheRequesterLearnsWhyTheRelayWasRefused pins E6.
//
// The requester asks for a relay towards a member that has left the
// network. The refusal must name the reason AND carry the class, so the
// operator can act on it and the path layer can classify it.
func TestTheRequesterLearnsWhyTheRelayWasRefused(t *testing.T) {
	f := newRelayFixture(t)
	f.startHub(t, true)
	f.joinBobHolding(t)

	// The TARGET is revoked: it left the network.
	if err := f.members.Remove(f.bob.ID()); err != nil {
		t.Fatal(err)
	}

	out := captureRelayLog(t, func() {
		_, err := OpenRelay(context.Background(), RelayRequesterConfig{
			Device:    f.alice,
			Resolver:  MemberListAdapter{List: f.members},
			Target:    f.bob.ID(),
			Kind:      KindPing,
			TargetKey: f.bob.PublicKey(),
			Relay:     f.hub.ID(),
			Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
				link, err := DialRelaySession(ctx, f.alice, MemberListAdapter{List: f.members}, relay, []string{f.hubAddr})
				if err != nil {
					return nil, err
				}
				return link.Session(), nil
			},
		})
		if err == nil {
			t.Fatal("the trunk granted a relay towards a member that is not in the view")
		}

		// ★ THE CLASS. The refusal is a VERDICT (the credentials/the
		// membership are the problem), so the path layer must not try
		// another relay with the same credentials.
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("the refusal is %v, want an auth-class error: the target is not a member, and "+
				"that verdict must not be retried on another relay", err)
		}

		// ★ THE REASON. It must name the member that left, so the
		// operator knows WHICH device to look at.
		if !strings.Contains(err.Error(), f.bob.ID().String()) {
			t.Fatalf("the refusal %q does not name the member that left the network: the operator "+
				"has to be told WHICH device", err.Error())
		}
		if !strings.Contains(err.Error(), "not a member") {
			t.Fatalf("the refusal %q does not say the member is not a member: an actionable reason "+
				"names the situation, not just the failure", err.Error())
		}
	})

	// The anchor must carry the SAME reason on its own log: the operator
	// on the anchor's machine is the one who has to act.
	if !strings.Contains(out, "not a member") {
		t.Fatalf("the anchor's log does not carry the refusal reason:\n%s", out)
	}
	if !strings.Contains(out, f.bob.ID().String()) {
		t.Fatalf("the anchor's log does not name the member that left the network:\n%s", out)
	}
}

// TestTheRevokedRelayFlowTearsDownWithoutTellingTheRequesterWhy records
// the KNOWN GAP of the E6 cell.
//
// The teardown of an ALREADY-OPEN flow reaches the requester as a bare
// transport error — the abort cancels the stream, and the reason ("a
// member of this relay was revoked") stays on the anchor's log.
//
// This test pins that gap so it is VISIBLE: it asserts what the requester
// gets today (a cancelled stream, no reason) and that the anchor's log
// does carry the reason. When the product starts carrying the reason to
// the requester, this test goes red and the ledger row for E6 can be
// upgraded from ◐ to ✔.
func TestTheRevokedRelayFlowTearsDownWithoutTellingTheRequesterWhy(t *testing.T) {
	f := newRelayFixture(t)
	f.startHub(t, true)
	f.joinBobHolding(t)

	requester, err := OpenRelay(context.Background(), RelayRequesterConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Kind:      KindPing,
		TargetKey: f.bob.PublicKey(),
		Relay:     f.hub.ID(),
		Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
			link, err := DialRelaySession(ctx, f.alice, MemberListAdapter{List: f.members}, relay, []string{f.hubAddr})
			if err != nil {
				return nil, err
			}
			return link.Session(), nil
		},
	})
	if err != nil {
		t.Fatalf("the relayed session could not be opened: %v", err)
	}
	defer requester.Close()

	// The requester is revoked.
	if err := f.members.Remove(f.alice.ID()); err != nil {
		t.Fatal(err)
	}

	var log string
	log = captureRelayLog(t, func() {
		f.trunk.StartRevocationWatch(context.Background(), 20*time.Millisecond)
		deadline := time.Now().Add(c1RevocationBudget)
		for f.trunk.FlowCount() > 0 {
			if time.Now().After(deadline) {
				t.Fatalf("the revoked member's relay flow is still live after %v", c1RevocationBudget)
			}
			time.Sleep(20 * time.Millisecond)
		}
		// Give the teardown's log line a moment.
		time.Sleep(100 * time.Millisecond)
	})

	// The requester's next read is where the teardown shows up.
	buf := make([]byte, 64)
	_ = requester.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err = requester.Read(buf)
	if err == nil {
		t.Fatal("the requester read from a relay flow that was torn down")
	}

	// ★ THE ANCHOR'S LOG CARRIES THE REASON. This is the half that IS
	// there today, and it is the operator's only clue.
	if !strings.Contains(log, "revoked") {
		t.Fatalf("the anchor's log does not say that the relay was torn down because a member was "+
			"revoked:\n%s", log)
	}

	// ★ THE GAP. The requester gets a bare transport error.
	//
	// This assertion is written so that it goes RED when the product
	// starts telling the requester — which is the moment the ledger row
	// for E6 may be upgraded from ◐ to ✔.
	if strings.Contains(err.Error(), "revoked") {
		t.Fatalf("★ THE PRODUCT IMPROVED: the requester now sees %q. The ledger row for E6 can be "+
			"upgraded from ◐ to ✔, and this test should be rewritten to assert the reason rather "+
			"than the gap", err.Error())
	}
	var streamErr *quic.StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("the requester saw %v (%T), want a cancelled stream: the flow must be ABORTED, not "+
			"half-closed with a FIN", err, err)
	}
	t.Logf("the requester sees %q and the anchor's log says %q: the reason reaches the anchor but "+
		"not the requester — that is the open half of the E6 cell", err.Error(), strings.TrimSpace(log))
}

// ---------------------------------------------------------------------------
// E7 — WITH THE RELAY AVAILABLE, A PING TO A MEMBER WITH AN ENDPOINT STILL
// REPORTS link=direct.
//
// The acceptance cell says: "中转可用时，`mesh ping` 到有端点的成员仍报
// `link=direct`" — the owner's ruling (D73): "中继只兜底：大文件走直连，
// 直连失败再走中转".
//
// The guards that existed before this one pinned the PICKER's ORDER (the
// direct path is offered first) and the picker's WIRING (wiring a picker
// does not break the direct path). Neither of them pinned the OUTCOME the
// operator reads: the path that was actually USED.
//
// ★ THE LINK CLASS IS A PRODUCT VALUE, NOT A STRING. The CLI prints
// `link=` from the PathKind the path layer reports, so the guard asserts
// on that value. A test that grepped the source for "link=direct" would
// prove nothing about the behaviour.
// ---------------------------------------------------------------------------

// TestTheDirectPathIsStillChosenWhenTheRelayIsAvailable pins E7.
//
// ★ THE RELAY IS REALLY AVAILABLE.
//
// The relay in this test is a REAL trunk with a REAL listener: the target
// has joined it, so a relayed session towards the target WOULD work. That
// is what makes "the direct path is still preferred" a statement about
// preference rather than about the relay being broken.
//
// (A test with an unreachable relay candidate would pass for the wrong
// reason: the relay path would fail, the direct path would be used, and
// the assertion would hold without anything having been preferred.)
//
// The target HAS an endpoint of its own — that is the "到有端点的成员" of
// the acceptance cell — so the direct path CAN be taken.
func TestTheDirectPathIsStillChosenWhenTheRelayIsAvailable(t *testing.T) {
	f := newRelayFixture(t)
	f.startHub(t, true)
	f.joinBobHolding(t)

	// The target (bob) also has a DIRECT endpoint of its own: a listener
	// on its own socket. That is the "has an endpoint" half of the cell.
	targetListener, err := NewListener(context.Background(), ListenerConfig{
		Device:  f.bob,
		Members: MemberListAdapter{List: f.members},
		Addrs:   []string{"127.0.0.1:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(targetListener.Close)

	view := &discovery.MemberSnapshot{
		AccountID: f.members.AccountID,
		Members: map[identity.DeviceID]discovery.MemberView{
			f.alice.ID(): {},
			f.bob.ID(): {
				Cert:      f.members.Members[f.bob.ID()].Cert,
				Endpoints: targetListener.BoundAddrs(),
			},
			f.hub.ID(): {
				Cert: f.members.Members[f.hub.ID()].Cert,
				// The anchor runs a trunk, so it announces the address
				// on which it accepts relayed sessions.
				Endpoints:      []string{f.hubAddr},
				RelayEndpoints: []string{f.hubAddr},
			},
		},
	}
	picker := ViewPicker{
		View:      func() *discovery.MemberSnapshot { return view },
		Self:      f.alice.ID(),
		TargetKey: func(id identity.DeviceID) (ed25519.PublicKey, error) { return f.bob.PublicKey(), nil },
	}

	// The relay candidate must really be on offer — otherwise this test
	// would prove only "the direct path works", which is the control the
	// old guard already had.
	paths, err := picker.Pick(context.Background(), f.bob.ID(), KindPing)
	if err != nil {
		t.Fatal(err)
	}
	relays := 0
	for _, p := range paths {
		if p.Kind == PathRelay {
			relays++
		}
	}
	if relays == 0 {
		t.Fatal("the picker offered no relay candidate, so this test would prove nothing about " +
			"the direct path being preferred WHILE a relay is available")
	}

	// ★ THE RELAY REALLY WORKS. The test proves that the relay path is
	// not merely on offer but USABLE — otherwise "the direct path was
	// chosen" could just mean "the relay path would have failed".
	relayLink, relayUsed, err := dialViaPicker(context.Background(), DialConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Endpoints: nil, // pure-outbound: the direct path has nothing to dial
		Kind:      KindPing,
		Picker: DirectFirstPicker{
			Relays: []Path{{
				Kind:         PathRelay,
				Via:          f.hub.ID(),
				ViaEndpoints: []string{f.hubAddr},
				Hops:         1,
				TargetKey:    f.bob.PublicKey(),
			}},
		},
	})
	if err != nil {
		t.Fatalf("the relay path could not be opened, so this test cannot claim the relay was "+
			"available: %v", err)
	}
	_ = relayLink.Close()
	if relayUsed != PathRelay {
		t.Fatalf("the relay probe went over the %q path, want the relay one: the test's control "+
			"must show the relay really works", relayUsed)
	}

	// The dial goes through the picker, and the path that was actually
	// used is what the CLI prints `link=` from.
	link, used, err := dialViaPicker(context.Background(), DialConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Endpoints: targetListener.BoundAddrs(),
		Kind:      KindPing,
		Picker:    picker,
	})
	if err != nil {
		t.Fatalf("the ping could not be dialled: %v", err)
	}
	defer link.Close()

	// ★ THE ASSERTION THE CELL NAMES.
	if used != PathDirect {
		t.Fatalf("the ping went over the %q path, want the direct one: the relay is only a fallback "+
			"(D73: 大文件走直连，直连失败再走中转), the relay above demonstrably works, and the "+
			"target has an endpoint of its own", used)
	}
	// The link class is what the CLI prints. It is spelled out here so the
	// test pins the OPERATOR-visible value, not only the internal one.
	if linkClass := string(used); linkClass != "direct" {
		t.Fatalf("the reported link class is %q, want \"direct\": the CLI prints this value as "+
			"`link=`, and an operator reading \"link=relay\" for a reachable member would go "+
			"looking for a relay problem that does not exist", linkClass)
	}

	// ★ AND THE PING REALLY WORKED over that path. Without this the test
	// would only say "the path layer chose the direct path", which is the
	// old guard's claim restated.
	stats, err := PingExchangeStats(context.Background(), link, 3, 0)
	if err != nil {
		t.Fatalf("the ping over the direct path failed: %v", err)
	}
	if stats.Received != 3 || stats.Lost != 0 {
		t.Fatalf("the ping over the direct path got %d/%d replies with %d lost: the direct path "+
			"must not merely be CHOSEN, it must work", stats.Received, stats.Sent, stats.Lost)
	}
}

// ---------------------------------------------------------------------------
// E2 — A PURE-OUTBOUND MEMBER JOINS THE TRUNK WITH BOTH FLAGS.
//
// The acceptance cell says:
//
//	cmesh join --relay-trunk <锚点可达地址> --relay-trunk-id <锚点deviceID>
//
// The fixture of the E3 tests already passes BOTH flags to the join, and
// the anchor's relay endpoint really does appear in the member view — but
// all of that is a PRECONDITION of the E3 tests. E2 itself had no test of
// its own: if the flags were wired wrongly, the E3 tests would go red and
// E2 would still have no guard of its own.
//
// ★ WHAT THIS FILE CAN AND CANNOT SEE.
//
// The FLAGS are the CLI's (internal/cli), and this line may not touch
// that package. So the guard here pins the MESH-SIDE contract those flags
// drive: the join reaches the trunk over a REAL relay connection, the
// joiner's identity is PINNED against the trunk's device ID, and the
// member that joined is reachable ONLY through the trunk.
//
// The pinning is the load-bearing half: `--relay-trunk-id` exists so the
// join is pinned against a SPECIFIC anchor, and a join that was not
// pinned would accept any trunk that answered.
//
// The CLI-level half (that the two flags are actually ON the join's
// command line) is covered by internal/cli's own fixture and is recorded
// as such in the ledger.
// ---------------------------------------------------------------------------

// TestAPureOutboundMemberJoinsTheTrunkWithBothFlags pins E2 on the
// mesh side.
//
// The member is pure-outbound: it has NO endpoint of its own, so the
// ONLY way the anchor can reach it is over the join connection it opened.
// That is what makes "it joined the trunk" a fact rather than a claim.
func TestAPureOutboundMemberJoinsTheTrunkWithBothFlags(t *testing.T) {
	f := newRelayFixture(t)
	f.startHub(t, true)

	// The target is PURE-OUTBOUND: it has no endpoint of its own. The
	// anchor reaches it only over the connection it opens.
	//
	// (The fixture's hub is the anchor; bob is the joining member.)
	joinCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	joined := make(chan error, 1)
	go func() {
		joined <- JoinRelayTrunk(joinCtx, RelayJoinConfig{
			Device:   f.bob,
			Resolver: MemberListAdapter{List: f.members},
			// THE TWO FLAGS' MESH-SIDE CONTRACT: the anchor's device ID
			// (--relay-trunk-id) and the anchor's reachable address
			// (--relay-trunk).
			Trunk:     f.hub.ID(),
			Endpoints: []string{f.hubAddr},
			OnSession: func(sess Session, requester identity.DeviceID, kind string) error {
				return nil
			},
		})
	}()

	// The join must land: the anchor's joiners table must hold the member.
	deadline := time.Now().Add(45 * time.Second)
	for f.joiners.Count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the pure-outbound member never joined the trunk")
		}
		select {
		case err := <-joined:
			t.Fatalf("the join returned instead of staying connected: %v", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The joiner's identity is the one that JOINED, and the trunk holds
	// it under its OWN device ID.
	joiner, err := f.joiners.Joiner(f.bob.ID())
	if err != nil {
		t.Fatalf("the anchor does not hold the joining member: %v", err)
	}
	if joiner == nil {
		t.Fatal("the anchor's joiner entry is empty")
	}
	f.joiners.mu.RLock()
	held := f.joiners.joiners[f.bob.ID()]
	f.joiners.mu.RUnlock()
	if held == nil {
		t.Fatal("the anchor's joiner table has no entry for the joining member")
	}
	if held.Device != f.bob.ID() {
		t.Fatalf("the anchor holds the joining member as %s, want %s: the join record must name "+
			"the device that actually joined", held.Device, f.bob.ID())
	}

	// ★ THE PIN. The join is pinned against the trunk's device ID, so the
	// anchor can only be talking to the member it meant to talk to.
	//
	// The pin is enforced by the TLS certificate check on the relay
	// connection (DialRelaySession pins the peer against the member view),
	// and the join record's From must match the certificate's subject.
	// So a join that reached a DIFFERENT anchor would have failed the
	// handshake — the fact that the join succeeded IS the pin.
	if f.hub.ID() == f.bob.ID() {
		t.Fatal("the fixture's anchor and joining member share a device ID: the pin cannot be " +
			"proved with this fixture")
	}

	// ★ THE PIN IS EXERCISED, NOT ASSUMED. The same member tries to open
	// a relay session while pinned against a DIFFERENT trunk — one whose
	// device ID is not the one it was told to pin against — and the dial
	// must be REFUSED.
	//
	// Without this, "the join succeeded" would prove only that the
	// member can reach SOME trunk, and `--relay-trunk-id` would be an
	// unverified claim.
	other, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	otherCert, err := f.acc.IssueCert(other, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.members.Add(*otherCert); err != nil {
		t.Fatal(err)
	}
	pinCtx, pinCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pinCancel()
	if _, err := DialRelaySession(pinCtx, f.bob, MemberListAdapter{List: f.members},
		other.ID(), []string{f.hubAddr}); err == nil {
		t.Fatal("the relay session reached the anchor while pinned against a DIFFERENT trunk: " +
			"the --relay-trunk-id pin is not enforced")
	}

	// The joining member is PURE-OUTBOUND: it is reachable ONLY through
	// the trunk. That is the observable that says "it joined".
	//
	// The direct path has nothing to dial — the member has no endpoint —
	// so a relayed session is the only path that can work.
	sess, err := OpenRelay(context.Background(), RelayRequesterConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Kind:      KindPing,
		TargetKey: f.bob.PublicKey(),
		Relay:     f.hub.ID(),
		Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
			link, err := DialRelaySession(ctx, f.alice, MemberListAdapter{List: f.members}, relay, []string{f.hubAddr})
			if err != nil {
				return nil, err
			}
			return link.Session(), nil
		},
	})
	if err != nil {
		t.Fatalf("the pure-outbound member could not be reached through the trunk it joined: %v", err)
	}
	defer sess.Close()

	// And the direct path really has nothing to dial: the member
	// announced no endpoint, so the direct path is a routing fact away.
	if _, err := DialDirect(context.Background(), DialConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Endpoints: nil,
		Kind:      KindPing,
	}); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("the direct path towards the pure-outbound member returned %v, want ErrNoRoute: "+
			"the member must have NO endpoint of its own, or \"it is reachable only through the "+
			"trunk\" would prove nothing", err)
	}
}

// ---------------------------------------------------------------------------
// E5 — 3 MiB THROUGH THE RELAY, BYTE FOR BYTE.
//
// ★ THIS CELL IS NOT FULLY CLOSED HERE, AND THE LEDGER MUST SAY SO.
//
// The acceptance cell wants 3 MiB over the relay with `receiver verified`
// and equal sha256 digests on both ends, ON THE REAL PUBLIC TOPOLOGY. The
// real-machine measurement (Round 108) showed 1 MiB over the public path
// failing 0/5, and the SECOND factor of that failure was never located.
//
// The mesh-level half of the cell IS closed here: 3 MiB goes over the REAL
// relay listener, over a REAL QUIC connection, with the REAL file handler
// on the target, and the two digests are compared byte for byte.
//
// The real-machine half is NOT reproducible on this machine: it needs the
// authorised host, the public hairpin topology, and the 网主's approval (and
// the second factor of the Round 108 failure has to be found first). The
// ledger row for E5 therefore stays ◐ with the missing half spelled out.
// ---------------------------------------------------------------------------

// TestTheRelayCarriesThreeMebibytesByteForByte is the mesh-level half of
// E5.
//
// It goes through the REAL listener and the REAL file handler — not a
// pipe — and the digest on the receiving side is compared with the one the
// sender computed.
func TestTheRelayCarriesThreeMebibytesByteForByte(t *testing.T) {
	f := newBigFileFixture(t)
	f.startHub(t)
	f.joinBob(t)

	const size = 3 << 20
	took, err := f.sendFileOverRelay(t, size)
	if err != nil {
		t.Fatalf("the %d byte file did not get through the relay (took %s): %v", size, took, err)
	}
	t.Logf("the %d byte file got through the relay in %s", size, took)
}

// ---------------------------------------------------------------------------
// SHARED HELPERS.
// ---------------------------------------------------------------------------

// joinBobHolding makes the target join the trunk and HOLD every relayed
// session it is given.
//
// The existing fixture (relaytrunk_listener_test.go's joinBob) hands the
// session to the test and returns; a session that is then released ends
// the flow on its own, which would make a revocation test meaningless —
// the flow would be over before the revocation.
//
// The channel is returned so the test can WAIT for the session to arrive
// (and thereby know the relay is really carrying bytes) before it revokes
// anybody.
func (f *relayFixture) joinBobHolding(t *testing.T) <-chan Session {
	t.Helper()
	sessions := make(chan Session, 4)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = JoinRelayTrunk(ctx, RelayJoinConfig{
			Device:    f.bob,
			Resolver:  MemberListAdapter{List: f.members},
			Trunk:     f.hub.ID(),
			Endpoints: []string{f.hubAddr},
			OnSession: func(sess Session, requester identity.DeviceID, kind string) error {
				sessions <- sess
				// HOLD the session until the test ends. A session that
				// is released ends the relayed flow, and then a
				// revocation would prove nothing.
				<-time.After(2 * time.Minute)
				return nil
			},
		})
	}()
	deadline := time.Now().Add(45 * time.Second)
	for f.joiners.Count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the target never joined the trunk")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return sessions
}
