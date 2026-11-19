package mesh

import (
	"bufio"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/wire"
)

// TestTheRelayCarriesAWriteBiggerThanOneRecord is the regression test for
// the defect the real machine found: a file bigger than about a megabyte
// could not be sent through the relay at all.
//
// The relay record is a JSON envelope, and JSON carries a []byte as
// base64 — four bytes of wire for every three bytes of payload. The file
// sender writes 1 MiB chunks, so one chunk became a 1398158-byte record
// against a 1048576-byte limit, and the transfer died with "record too
// large". The very same file went through the direct link without
// complaint.
//
// The fix splits a write that does not fit into several records. The
// split is invisible to the business layer: the end-to-end seal is
// counter-based, so each piece is opened on its own, and the receiver
// reads a STREAM — the same bytes in the same order.
func TestTheRelayCarriesAWriteBiggerThanOneRecord(t *testing.T) {
	f := newTrunkFixture(t)

	trunkToA, aSide := newPipeSessionPair()

	joiners := NewJoinerTable()
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: joiners,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = trunk.ServeRequester(context.Background(), trunkToA, joiners)
	}()

	target := newJoinedTarget(f.bob.ID())
	joiners.Add(&TrunkJoiner{
		Device: f.bob.ID(),
		sess:   trunkToA,
		br:     bufio.NewReader(trunkToA),
		open:   target.open,
	})

	// The target reads the whole payload.
	payload := make([]byte, 1<<20+512*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	got := make([]byte, len(payload))
	readDone := make(chan error, 1)
	go func() {
		sess, _, err := target.serveOne(t, RelayTargetConfig{
			Device:       f.bob,
			Requester:    f.alice.ID(),
			RequesterKey: f.alice.PublicKey(),
		})
		if err != nil {
			readDone <- err
			return
		}
		_, err = io.ReadFull(sess, got)
		readDone <- err
	}()

	sess, err := OpenRelay(context.Background(), RelayRequesterConfig{
		Device:    f.alice,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.bob.ID(),
		Kind:      KindFile,
		TargetKey: f.bob.PublicKey(),
		Relay:     f.hub.ID(),
		Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
			return aSide, nil
		},
	})
	if err != nil {
		t.Fatalf("the relayed session could not be opened: %v", err)
	}

	// ONE write, bigger than one relay record.
	if _, err := sess.Write(payload); err != nil {
		t.Fatalf("the payload could not be written through the relay: %v", err)
	}

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("the target did not receive the whole payload: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the target never received the payload")
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("the payload differs at byte %d: got %d, want %d", i, got[i], payload[i])
		}
	}
}

// TestAFinishedRelayFreesTheTrunksSlot is the regression test for the
// defect the real machine found: the trunk refused every new relay after
// twenty of them ("this node is carrying as many relays as it may").
//
// The slot is the trunk's quota, and a relay that has ENDED must give it
// back. The slot leaked because the TARGET's side of the flow was never
// closed: the trunk's splice waits for both directions to end, and the
// target's direction could only end if the target closed its side. The
// real target's handler (receiveMsgs and friends) writes its answer and
// RETURNS — it does not close the session — so nothing ever ended that
// direction, the flow never ended, the slot never came back, and a relay
// that had worked perfectly well stopped working for a reason that had
// nothing to do with the network.
//
// The target here is driven through serveRelayedSession — the REAL entry
// point of a member that joined the trunk — so the test exercises the
// close that the real target performs when its handler returns.
func TestAFinishedRelayFreesTheTrunksSlot(t *testing.T) {
	f := newTrunkFixture(t)

	joiners := NewJoinerTable()
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: joiners,
	})
	if err != nil {
		t.Fatal(err)
	}

	target := newJoinedTarget(f.bob.ID())
	joiners.Add(&TrunkJoiner{
		Device: f.bob.ID(),
		sess:   nil,
		br:     nil,
		open:   target.open,
	})

	// More relays than the quota allows: if the slots leak, the last ones
	// are refused.
	const relays = DefaultRelayMaxStreams + 5
	for i := 0; i < relays; i++ {
		trunkToA, aSide := newPipeSessionPair()
		go func() {
			_ = trunk.ServeRequester(context.Background(), trunkToA, joiners)
		}()

		// The target serves the relayed session the way a real member
		// that joined the trunk does. Its handler answers and RETURNS —
		// it does not close the session itself.
		targetDone := make(chan error, 1)
		go func() {
			// BOTH ends of the target's session are drained: the double
			// hands them out on bounded channels, and a test that leaves
			// one of them unread blocks the NEXT relay.
			select {
			case <-target.trunkEnds:
			case <-time.After(10 * time.Second):
				targetDone <- errors.New("the trunk never dialled the target")
				return
			}
			var targetEnd *pipeSession
			select {
			case targetEnd = <-target.sessions:
			case <-time.After(10 * time.Second):
				targetDone <- errors.New("the trunk never opened a session towards the target")
				return
			}
			serveRelayedSession(targetEnd, RelayJoinConfig{
				Device:   f.bob,
				Resolver: MemberListAdapter{List: f.members},
				OnSession: func(sess Session, requester identity.DeviceID, kind string) error {
					br := bufio.NewReader(sess)
					if _, err := readRelayRecord(br); err != nil {
						return err
					}
					return writeRelayRecord(sess, relayRecord{
						Type: uint8(wire.FrameMeshRelayData),
						Data: []byte("the answer"),
					})
				},
			})
			targetDone <- nil
		}()

		sess, err := OpenRelay(context.Background(), RelayRequesterConfig{
			Device:    f.alice,
			Resolver:  MemberListAdapter{List: f.members},
			Target:    f.bob.ID(),
			Kind:      KindMsg,
			TargetKey: f.bob.PublicKey(),
			Relay:     f.hub.ID(),
			Dial: func(ctx context.Context, relay identity.DeviceID, endpoints []string) (Session, error) {
				return aSide, nil
			},
		})
		if err != nil {
			t.Fatalf("relay %d could not be opened: %v", i, err)
		}

		// The requester sends its request and half-closes.
		if _, err := sess.Write([]byte("the request")); err != nil {
			t.Fatalf("relay %d: the requester could not send: %v", i, err)
		}
		if err := sess.Close(); err != nil {
			t.Fatalf("relay %d: the requester could not close: %v", i, err)
		}

		select {
		case err := <-targetDone:
			if err != nil {
				t.Fatalf("relay %d: the target failed: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("relay %d: the target never finished", i)
		}

		// The slot must come back.
		waitFlowCount(t, trunk, 0)
	}
}

// waitFlowCount waits for the trunk's live flow count to reach want.
func waitFlowCount(t *testing.T, trunk *RelayTrunk, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if trunk.FlowCount() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the trunk carries %d flows, want %d", trunk.FlowCount(), want)
}
