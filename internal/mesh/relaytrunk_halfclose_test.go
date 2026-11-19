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

// TestTheTrunkCarriesAnAnswerThatArrivesAfterTheRequestEnded is the
// regression test for the defect the real-machine round found: the message
// reached the target, but the DELIVERY ACK never came back.
//
// The shape of every request/response protocol in this codebase is:
//
//	send the request -> half-close (FIN) -> wait for the answer
//
// `cmesh msg`, `cmesh run` and `cmesh send-to` all do that. The relay
// trunk's splice used to treat "one direction ended" as "the flow
// ended": it closed BOTH of the flow's sessions the moment the request
// direction hit EOF. The answer was still in flight on the other
// direction, so it was discarded — and the requester saw EOF instead of
// the acknowledgement.
//
// The half-close is not a detail: the real Session is a half close
// (quic.Stream.Close() writes a FIN and leaves the receive side open), so
// a trunk that cannot carry a half close cannot carry a request that
// waits for its answer.
func TestTheTrunkCarriesAnAnswerThatArrivesAfterTheRequestEnded(t *testing.T) {
	f := newTrunkFixture(t)

	// The requester and the target, each as a pair of session ends.
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
		Kind: KindMsg,
	}, aRead); err != nil {
		t.Fatalf("the trunk refused a valid relay: %v", err)
	}

	// The requester sends its request.
	if err := writeRelayRecord(aWrite, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: []byte("the request"),
	}); err != nil {
		t.Fatalf("the requester could not send: %v", err)
	}

	// The target reads the open record and the request.
	cBr := bufio.NewReader(cRead)
	open, err := readRelayRecord(cBr)
	if err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}
	if open.From != f.alice.ID() {
		t.Fatalf("the open record names %q, want the requester %s", open.From, f.alice.ID())
	}
	req, err := readRelayRecord(cBr)
	if err != nil {
		t.Fatalf("the target received no request: %v", err)
	}
	if string(req.Data) != "the request" {
		t.Fatalf("the target received %q, want the request", req.Data)
	}

	// The requester is DONE SENDING: it half-closes.
	if err := aWrite.CloseWrite(); err != nil {
		t.Fatalf("the requester could not half-close: %v", err)
	}

	// The target sees the end of the request. The trunk has to propagate
	// the half close — otherwise the target waits for a request that will
	// never come.
	_ = cRead.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := readRelayRecord(cBr); !errors.Is(err, io.EOF) {
		t.Fatalf("the target did not see the end of the request: %v", err)
	}

	// The target answers. The pause makes the race deterministic: by now
	// the trunk has already processed the requester's FIN, so a trunk that
	// tears the flow down on the first EOF has closed the requester's
	// side and the answer has nowhere to go.
	time.Sleep(150 * time.Millisecond)
	if err := writeRelayRecord(cRead, relayRecord{
		Type: uint8(wire.FrameMeshRelayData),
		Data: []byte("the answer"),
	}); err != nil {
		t.Fatalf("the target could not answer: %v", err)
	}

	// The requester must receive the answer.
	_ = aWrite.SetReadDeadline(time.Now().Add(5 * time.Second))
	ans, err := readRelayRecord(bufio.NewReader(aWrite))
	if err != nil {
		t.Fatalf("the requester never got the answer: %v", err)
	}
	if string(ans.Data) != "the answer" {
		t.Fatalf("the requester got %q, want the answer", ans.Data)
	}
}
