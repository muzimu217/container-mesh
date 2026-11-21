package mesh

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// The DATA INTEGRITY and RESOURCE tests for the trunk's forwarding path.
//
// Each one is a defect found by reading the forward loop with the
// question "what does the trunk do when the transfer is NOT perfectly
// clean". The happy path was already covered (TestTrunkOpenFlowForwardsBytes
// and friends); these cover the paths where bytes stop arriving halfway,
// where the rate budget runs dry mid-transfer, and where the record on
// the wire lies about its own length.

// TestATruncatedRelayRecordIsNotACleanEnd is the regression test for the
// data-loss defect in the forward loop's framing.
//
// A relay record is length-prefixed. io.ReadFull — which readFrame used
// for the BODY — returns io.EOF both for "nothing was read" AND for "the
// body of a truncated record read ZERO bytes". The forward loop treated
// every io.EOF as "the sender is done sending": a record whose header
// promised 500 bytes and whose body never came was forwarded as a GRACEFUL
// end, the tail was dropped, and the far side was told the message was
// complete. Silent truncation, dressed up as a clean finish.
//
// The message path learned this lesson long ago — that is why
// readMsgFrame reads the header and the body separately and reports
// cleanEnd only for an EOF at a RECORD BOUNDARY. The relay path now uses
// the same reader.
func TestATruncatedRelayRecordIsNotACleanEnd(t *testing.T) {
	f := newTrunkFixture(t)
	aRead, aWrite := newPipeSessionPair()
	tRead, cRead := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tRead, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, aRead); err != nil {
		t.Fatal(err)
	}

	// The requester sends one GOOD record, then a header that promises
	// 500 bytes and a FIN instead of the body.
	go func() {
		_ = writeRelayRecord(aWrite, relayRecord{
			Type: uint8(wire.FrameMeshRelayData),
			Data: []byte("the part that arrived"),
		})
		var head [4]byte
		binary.BigEndian.PutUint32(head[:], 500)
		_, _ = aWrite.Write(head[:])
		_ = aWrite.Close() // half close: FIN, no body
	}()

	cBr := bufio.NewReader(cRead)
	_ = cRead.SetReadDeadline(time.Now().Add(5 * time.Second))

	open, err := readRelayRecord(cBr)
	if err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the target's first record is type %d, want the open record", open.Type)
	}
	rec, err := readRelayRecord(cBr)
	if err != nil {
		t.Fatalf("the target lost the record that DID arrive: %v", err)
	}
	if string(rec.Data) != "the part that arrived" {
		t.Fatalf("the target received %q", rec.Data)
	}

	// The truncated tail must be a TEARDOWN, not a graceful finish. On a
	// graceful finish the target's read returns io.EOF and a request/response
	// protocol above believes the sender completed. An aborted session is a
	// read ERROR — that difference is the whole test.
	_, err = readRelayRecord(cBr)
	if err == nil {
		t.Fatal("the trunk forwarded the truncation as if the stream were over")
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("the truncated record reached the target as a CLEAN end: " +
			"the tail was dropped silently and the target was told the message was complete")
	}
}

// TestTheReaderTellsACleanEndFromATruncatedRecord pins the framing rule
// at the reader itself, where the forward loop gets it from.
//
//   - EOF before ANY header byte: the sender half-closed at a record
//     boundary — a clean end.
//   - A header that promises bytes and a body that never comes: a
//     TRUNCATED record — never a clean end, whatever io.ReadFull
//     happens to return for a zero-byte read.
func TestTheReaderTellsACleanEndFromATruncatedRecord(t *testing.T) {
	// Clean end: nothing at all on the wire. The SENDER half-closes; the
	// reading end sees the stream end.
	sender, reader := newPipeSessionPair()
	_ = sender.Close()
	_, err := readRelayRecord(bufio.NewReader(reader))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("an empty stream is %v, want io.EOF (a clean end)", err)
	}

	// Truncated: a header promising 100 bytes, then the stream ends.
	truncPipe := newTruncationPipe()
	_, err = readRelayRecord(bufio.NewReader(truncPipe))
	if err == nil {
		t.Fatal("a record whose body never arrived read as a success")
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("a truncated record reads as io.EOF: the forward loop would " +
			"forward it as a clean half close and the tail would be lost")
	}
}

// newTruncationPipe is a pipe whose READING end delivers exactly one
// truncated record: a header that promised 100 bytes, then the stream
// ends before the body.
//
// The header is written by the SENDER end (a write lands in the peer's
// inbound queue — that is what "the wire" is here), and the sender's
// half close is what the reader sees after the header.
func newTruncationPipe() *pipeSession {
	sender, reader := newPipeSessionPair()
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], 100)
	_, _ = sender.Write(head[:])
	_ = sender.Close()
	return reader
}

// TestTheRateQuotaThrottlesInsteadOfKilling is the regression test for
// the defect that made the rate cap a KILL SWITCH.
//
// The token bucket refills continuously, but the forward loop treated an
// empty bucket as a refusal: the flow was torn down mid-transfer. A
// transfer faster than the cap DIED at the cap instead of being slowed
// to it — which is the exact "file size limit" behaviour the bucket was
// introduced to remove. P-7 is a sustained-rate requirement: 100 MiB at
// a 50 MiB/s cap takes two seconds and MUST arrive.
func TestTheRateQuotaThrottlesInsteadOfKilling(t *testing.T) {
	f := newTrunkFixture(t)
	aRead, aWrite := newPipeSessionPair()
	tRead, cRead := newPipeSessionPair()

	const capPerSec = 64 << 10 // 64 KiB/s: small enough to exhaust in a test
	const chunk = 16 << 10     // 16 KiB records
	const chunks = 8           // 128 KiB total = 2 seconds' worth

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Quota: RelayQuota{
			MaxStreams:     20,
			MaxBytesPerSec: capPerSec,
		},
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tRead, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, aRead); err != nil {
		t.Fatalf("the trunk refused the relay outright: %v", err)
	}

	go func() {
		for i := 0; i < chunks; i++ {
			if err := writeRelayRecord(aWrite, relayRecord{
				Type: uint8(wire.FrameMeshRelayData),
				Data: make([]byte, chunk),
			}); err != nil {
				return
			}
		}
		_ = aWrite.Close()
	}()

	cBr := bufio.NewReader(cRead)
	_ = cRead.SetReadDeadline(time.Now().Add(30 * time.Second))

	open, err := readRelayRecord(cBr)
	if err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the target's first record is type %d, want the open record", open.Type)
	}

	// Every chunk must arrive. The cap slows the transfer; it must not
	// cut it.
	for i := 0; i < chunks; i++ {
		rec, err := readRelayRecord(cBr)
		if err != nil {
			t.Fatalf("chunk %d of %d never arrived: %v — the rate cap killed a "+
				"transfer it should have slowed", i+1, chunks, err)
		}
		if rec.Type == uint8(wire.FrameMeshRelayClose) {
			t.Fatalf("chunk %d of %d was followed by a CLOSE record: the rate "+
				"quota tore the flow down instead of throttling it", i+1, chunks)
		}
	}

	// The FIN the requester sent must still arrive, AFTER the data.
	_ = cRead.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := readRelayRecord(cBr); !errors.Is(err, io.EOF) {
		t.Fatalf("after all the data the target saw %v, want the forwarded FIN", err)
	}
}

// TestTheRateQuotaIsCheckedWhereRelaysAreGranted pins the admission check
// to the path that actually grants relays.
//
// The rate check lived in OpenFlow, and OpenFlow has no production caller:
// the serving path is ServeRequesterWithFirst, which only counted STREAMS.
// A node whose forwarding budget was spent still GRANTED new relays — and
// then killed them on the first record. The recorded decision is the
// opposite: when a node is saturated it refuses NEW requests, and it says
// so with the quota class so the requester can try another relay.
func TestTheRateQuotaIsCheckedWhereRelaysAreGranted(t *testing.T) {
	f := newTrunkFixture(t)
	trunkSide, aSide := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Quota: RelayQuota{
			MaxStreams:     20,
			MaxBytesPerSec: 64 << 10,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Spend the whole bucket, four times over.
	trunk.accountForward(4 * int64(trunk.quota.MaxBytesPerSec))

	// Write a VALID open record: the refusal must come from the rate
	// check, not from a malformed request.
	if err := writeRelayRecord(aSide, relayRecord{
		Type: uint8(wire.FrameMeshRelayOpen),
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}); err != nil {
		t.Fatal(err)
	}

	err = trunk.ServeRequesterWithFirst(context.Background(), trunkSide, mustNotDial{}, bufio.NewReader(trunkSide), nil)
	if err == nil {
		t.Fatal("a relay was granted although the node's forwarding budget is spent")
	}
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("the refusal is %v, want the quota class", err)
	}
}

// TestTheSecondCannotReachTheTargetIsAlsoUnavailable is the regression
// test for the refusal-class defect that silently broke the multi-relay
// fallback.
//
// The trunk answers "cannot reach the target" on TWO paths. One was
// re-classed RelayClassUnavailable (a LOCAL condition: another relay may
// reach the target). The second path — the OPEN record that cannot be
// written to the target — still answered RelayClassRefused, which the
// path layer treats as a VERDICT: the fallback chain stops there and the
// second relay is never tried. Same local condition, two different
// answers.
func TestTheSecondCannotReachTheTargetIsAlsoUnavailable(t *testing.T) {
	f := newTrunkFixture(t)
	aRead, aWrite := newPipeSessionPair()
	tRead, _ := newPipeSessionPair()

	// The trunk WILL reach the target's dial, but the connection dies
	// before the open record can be written onto it.
	_ = tRead.Abort()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		_ = writeRelayRecord(aWrite, relayRecord{
			Type: uint8(wire.FrameMeshRelayOpen),
			From: f.alice.ID(),
			To:   f.bob.ID(),
			Hops: 1,
		})
	}()

	served := make(chan error, 1)
	go func() {
		served <- trunk.ServeRequesterWithFirst(context.Background(), aRead, abortedDial{sess: tRead}, bufio.NewReader(aRead), nil)
	}()

	_ = aWrite.SetReadDeadline(time.Now().Add(5 * time.Second))
	ans, err := readRelayRecord(bufio.NewReader(aWrite))
	if err != nil {
		t.Fatalf("the requester received no answer: %v", err)
	}
	if ans.Grant {
		t.Fatal("the relay was granted although the target could not be written to")
	}
	if ans.Class != RelayClassUnavailable {
		t.Fatalf("the refusal class is %q, want %q: a broken target stream is a "+
			"LOCAL condition, and a verdict here stops the multi-relay fallback chain",
			ans.Class, RelayClassUnavailable)
	}
	if err := <-served; err == nil {
		t.Fatal("serving returned no error although the target was unreachable")
	}
}

// TestTheRequesterIsWhoItsCertificateSays is the regression test for the
// identity-binding defect in the requester path.
//
// The JOIN path checks that the record's From matches the connection's
// certificate; the REQUESTER path never did. The open record's From is
// attacker-controlled bytes, so any member could ask the trunk to carry a
// relay "on behalf of" a DIFFERENT member — the trunk would re-check that
// member's authorization and touch the target's identity for a device
// that never made the request. The end-to-end handshake still fails (the
// trunk holds no key), so this is unauthorized work and a third-party
// identity lookup, not impersonation — but the authorization rule
// "requester first" is only real if the requester is the connection.
func TestTheRequesterIsWhoItsCertificateSays(t *testing.T) {
	f := newTrunkFixture(t)
	trunkSide, aSide := newPipeSessionPair()

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The connection's certificate says ALICE; the record claims BOB.
	open := relayRecord{
		Type: uint8(wire.FrameMeshRelayOpen),
		From: f.bob.ID(),
		To:   f.hub.ID(),
		Hops: 1,
	}
	h := &RelayHandler{trunk: trunk, joiners: NewJoinerTable()}
	h.serveRequester(trunk, nil, trunkSide, bufio.NewReader(trunkSide), &open, f.alice.ID())

	_ = aSide.SetReadDeadline(time.Now().Add(5 * time.Second))
	ans, err := readRelayRecord(bufio.NewReader(aSide))
	if err != nil {
		t.Fatalf("the requester received no answer: %v", err)
	}
	if ans.Grant {
		t.Fatal("the relay was granted for a claimed identity")
	}
	if ans.Class != RelayClassAuth {
		t.Fatalf("the refusal class is %q, want %q", ans.Class, RelayClassAuth)
	}
}

// mustNotDial is a RelayJoiner whose use is a test failure.
type mustNotDial struct{}

func (mustNotDial) Open(ctx context.Context, id identity.DeviceID) (Session, error) {
	return nil, errors.New("must not dial the target")
}

// abortedDial is a RelayJoiner that "reaches" the target with a session
// that is already dead: the dial succeeds, nothing can be written onto it.
type abortedDial struct{ sess Session }

func (a abortedDial) Open(ctx context.Context, id identity.DeviceID) (Session, error) {
	return a.sess, nil
}

// TestOversizedRecordsAlsoObeyTheRateCap pins the hole the adversarial
// review found in the throttle.
//
// chargeBytes carried an oversized-record fast path: one record bigger
// than a whole second's budget was charged and forwarded WITHOUT
// waiting, on the reasoning that waiting cannot help (the bucket never
// exceeds the cap). The reasoning holds for ONE record and fails for a
// STREAM of them: every oversized record re-entered the same branch and
// ignored the bucket again, so a sender pushing back-to-back full-size
// records ran at LINE RATE — the cap simply did not exist for it. A
// node configured under 1 MiB/s (the record cap) had no rate limiting
// at all.
//
// The pacing rule the fix encodes: an oversized record waits until the
// bucket can cover it minus one full cap (the burst allowance), and the
// wait is computed against the PRE-charge balance. Charging first and
// then returning the deficit as a wait live-locks — each retry
// re-charges, consuming exactly the refill, so the record never moves.
func TestOversizedRecordsAlsoObeyTheRateCap(t *testing.T) {
	f := newTrunkFixture(t)
	aRead, aWrite := newPipeSessionPair()
	tRead, cRead := newPipeSessionPair()

	const capPerSec = 64 << 10 // 64 KiB/s — below the 1 MiB record cap
	const chunk = 128 << 10    // 128 KiB records: 2 seconds' budget EACH
	const chunks = 8           // 1 MiB total: >= 14 s at the cap

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Quota:   RelayQuota{MaxStreams: 20, MaxBytesPerSec: capPerSec},
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tRead, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, aRead); err != nil {
		t.Fatal(err)
	}

	go func() {
		for i := 0; i < chunks; i++ {
			if err := writeRelayRecord(aWrite, relayRecord{
				Type: uint8(wire.FrameMeshRelayData),
				Data: make([]byte, chunk),
			}); err != nil {
				return
			}
		}
		_ = aWrite.Close()
	}()

	start := time.Now()
	cBr := bufio.NewReader(cRead)
	_ = cRead.SetReadDeadline(time.Now().Add(60 * time.Second))

	open, err := readRelayRecord(cBr)
	if err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the target's first record is type %d, want the open record", open.Type)
	}
	for i := 0; i < chunks; i++ {
		rec, err := readRelayRecord(cBr)
		if err != nil {
			t.Fatalf("oversized chunk %d of %d never arrived: %v", i+1, chunks, err)
		}
		if rec.Type == uint8(wire.FrameMeshRelayClose) {
			t.Fatalf("oversized chunk %d was followed by a CLOSE", i+1)
		}
	}
	elapsed := time.Since(start)
	// The floor is generous (pacing plus scheduling), but the defect ran
	// the same bytes through in ~6 ms — no floor under a second can pass
	// with the hole present.
	if elapsed < 5*time.Second {
		t.Fatalf("%d KiB went through a %d KiB/s cap in %s: the oversized "+
			"record fast path bypassed the throttle entirely",
			chunks*chunk>>10, capPerSec>>10, elapsed)
	}
}

// TestAThrottleThatCannotEverReleaseTheRecordIsALivelock pins the
// livelock the adversarial review found.
//
// The throttle's oversized-record rule waits until the bucket can cover
// the record minus one cap (the burst allowance). But the bucket's
// CEILING was exactly one cap — so the balance could never exceed one
// cap, and the condition could only ever hold for a record no bigger
// than TWO caps.
//
// A record is bounded by maxRelayRecord (1 MiB) and the cap is one
// operator flag away: a metered node running
// --relay-max-bytes-per-sec below ~384 KiB/s would have waited FOREVER
// on every large file, holding its stream slot for ever and stalling the
// trunk.
//
// The fix: the ceiling is the cap PLUS one record's worth.
func TestAThrottleThatCannotEverReleaseTheRecordIsALivelock(t *testing.T) {
	f := newTrunkFixture(t)
	aRead, aWrite := newPipeSessionPair()
	tRead, cRead := newPipeSessionPair()

	// A cap below half a record: the condition could never hold.
	//
	// The record is 600 KiB and the cap is 256 KiB/s, so the record is
	// more than twice the cap: with a ceiling of one cap the balance
	// could never reach "one cap + the record", and the forwarder would
	// wait for ever.
	const capPerSec = 256 << 10 // 256 KiB/s
	// The record must fit the envelope's payload limit (the JSON
	// envelope's maxRelayPlaintext is 3/4 of the 1 MiB record cap),
	// and it must be bigger than TWO caps so that the livelock's
	// condition can never hold.
	const chunk = 600 << 10 // 600 KiB: 2.3 × the cap, inside the limit

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Quota: RelayQuota{
			MaxStreams:     20,
			MaxBytesPerSec: capPerSec,
		},
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			return tRead, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, aRead); err != nil {
		t.Fatal(err)
	}

	go func() {
		_ = writeRelayRecord(aWrite, relayRecord{
			Type: uint8(wire.FrameMeshRelayData),
			Data: make([]byte, chunk),
		})
		_ = aWrite.Close()
	}()

	cBr := bufio.NewReader(cRead)
	// A generous deadline: the record must arrive, SLOWLY — the cap is
	// 256 KiB/s and the record is 600 KiB, so it takes about two and a
	// half
	// seconds at full speed. Under -race everything is several times
	// slower, so the budget is generous; what must NOT happen is an
	// INFINITE wait (the livelock), which no deadline would end.
	_ = cRead.SetReadDeadline(time.Now().Add(90 * time.Second))

	open, err := readRelayRecord(cBr)
	if err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}
	if open.Type != uint8(wire.FrameMeshRelayOpen) {
		t.Fatalf("the target's first record is type %d, want the open record", open.Type)
	}
	rec, err := readRelayRecord(cBr)
	if err != nil {
		t.Fatalf("the oversized record NEVER arrived against a %d KiB/s cap: "+
			"the relay HANGS for ever (the throttle's condition could never "+
			"be met because the bucket's ceiling was one cap): %v", capPerSec>>10, err)
	}
	if len(rec.Data) != chunk {
		t.Fatalf("the record carried %d bytes, want %d", len(rec.Data), chunk)
	}
}
