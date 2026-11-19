package mesh

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/wire"
)

// TestTheRefusalSaysTheTargetHasNotJoinedTheTrunk is the regression test
// for the second defect the real machine found: a refusal that was true
// and said nothing.
//
// The symptom was a TRANSFER hang. A member asked to reach a pure
// outbound target over the relay, and the requester waited for the
// target's hello until the handshake timed out — "mesh: relay: read the
// target's hello: deadline exceeded". The wording pointed at the
// transport, and the transport was fine.
//
// The cause was one step earlier: the target had NEVER JOINED the
// trunk. It had dialled the anchor's PUBLIC address, the cloud's 1:1 IP
// NAT does not map ports, and the reply never found its way back — so
// the join connection was never established, and the anchor's joiners
// table did not have the target.
//
// An operator reading "this node cannot reach <target> right now" has
// no way to tell that apart from "the network is down". The refusal
// must say WHICH.
func TestTheRefusalSaysTheTargetHasNotJoinedTheTrunk(t *testing.T) {
	f := newTrunkFixture(t)

	// The trunk HAS a joiners table — it is EMPTY. That is exactly the
	// state a member that failed to join leaves the anchor in: the
	// trunk is wired, the member is a member, but the member's join
	// connection was never established.
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: NewJoinerTable(),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
		Kind: KindMsg,
	}, &pipeSession{in: make(chan []byte, 4), closed: make(chan struct{}), sendClosed: make(chan struct{})})
	if err == nil {
		t.Fatal("the trunk opened a flow towards a member that never joined it")
	}

	msg := err.Error()
	if !strings.Contains(msg, "has not joined this trunk") {
		t.Fatalf("the refusal does not say the target never joined the trunk: %v", err)
	}
	if !strings.Contains(msg, f.bob.ID().String()) {
		t.Fatalf("the refusal does not name the target: %v", err)
	}
	// And it must be ACTIONABLE: the operator has to be told what to
	// do about it, not just that it happened.
	if !strings.Contains(msg, "--relay-trunk") {
		t.Fatalf("the refusal does not say how to fix it: %v", err)
	}
}

// TestTheRefusalReachesTheRequester: the reason must not stay on the
// anchor's own log. The requester is the one who is stuck, and it is
// already known to be a member (the authorization ran before this
// point), so the reason leaks nothing to a stranger.
//
// The path exercised here is the PRODUCTION one: the requester's
// connection is served by ServeRequester, which is what the listener
// calls. (RelayTrunk.OpenFlow has no production caller — it is the
// lower-level entry point the test fixtures use.)
func TestTheRefusalReachesTheRequester(t *testing.T) {
	f := newTrunkFixture(t)

	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Joiners: NewJoinerTable(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The requester's session: the trunk writes the refusal onto it.
	req, reqPeer := newPipeSessionPair()

	done := make(chan error, 1)
	go func() {
		done <- trunk.ServeRequester(context.Background(), req, NewJoinerTable())
	}()

	// The requester writes its open record.
	_ = reqPeer.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := writeRelayRecord(reqPeer, relayRecord{
		Type: uint8(wire.FrameMeshRelayOpen),
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
		Kind: KindMsg,
	}); err != nil {
		t.Fatalf("the requester could not ask for a relay: %v", err)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the trunk never answered")
	}

	// The refusal record on the requester's side.
	_ = reqPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(reqPeer)
	rec, err := readRelayRecord(br)
	if err != nil {
		t.Fatalf("the requester got no refusal: %v", err)
	}
	if rec.Grant {
		t.Fatal("the requester was granted a relay towards a member that never joined")
	}
	if !strings.Contains(rec.Reason, "has not joined this trunk") {
		t.Fatalf("the refusal the requester sees does not say why: %q", rec.Reason)
	}
}
