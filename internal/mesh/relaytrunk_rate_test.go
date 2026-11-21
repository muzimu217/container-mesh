package mesh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestTheRateQuotaIsARateAndNotAFileSizeLimit is the regression test
// for the defect the performance measurement found on the real machine.
//
// P-7 says "单节点被持续转发时，吞吐 ≤ 50 MiB/s" — a RATE. The
// implementation measured it over a FIXED ONE-SECOND WINDOW: everything a
// transfer pushed into its first second was counted, and the window
// reset a second later.
//
// So a file bigger than the cap could never be relayed — the rate limit
// had quietly become a FILE SIZE limit. Measured on the real machine:
// 49 MiB went through every time, 50 MiB failed every time, and raising
// the cap made 64 MiB succeed. The boundary was exactly the cap.
//
// The quota is now a token bucket that refills continuously, so the cap
// is a rate: a hundred megabytes takes two seconds and is allowed,
// while a node asked to push a hundred megabytes in one second is not.
func TestTheRateQuotaIsARateAndNotAFileSizeLimit(t *testing.T) {
	f := newTrunkFixture(t)
	trunk, err := NewRelayTrunk(RelayTrunkConfig{
		Self:    f.hub.ID(),
		Members: MemberListAdapter{List: f.members},
		Enabled: true,
		Quota: RelayQuota{
			MaxStreams:     20,
			MaxBytesPerSec: 64 << 10, // 64 KiB/s
		},
		Dial: func(ctx context.Context, target identity.DeviceID, kind string) (Session, error) {
			_, tgt := newPipeSessionPair()
			return tgt, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Charge the node FOUR times its per-second budget IN ONE GO. Under a
	// fixed one-second window that is indistinguishable from "a single
	// transfer of 4x the cap" — and it is refused. Under a token bucket
	// it is simply four seconds' worth of traffic, which the node may
	// carry over four seconds.
	limit := int64(trunk.quota.MaxBytesPerSec)
	trunk.accountForward(4 * limit)

	// The node is saturated RIGHT NOW (the bucket is empty), so a new
	// flow must be refused — that is the quota doing its job.
	req, _ := newPipeSessionPair()
	_, err = trunk.OpenFlow(context.Background(), RelayOpen{
		From: f.alice.ID(),
		To:   f.bob.ID(),
		Hops: 1,
	}, req)
	if err == nil {
		t.Fatal("a flow was granted although the node's rate budget is exhausted")
	}
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("the refusal is %v, want the quota class", err)
	}

	// And the size of a SINGLE transfer is not what decides: after the
	// budget refills, the node may carry a transfer that is bigger than
	// one second's worth — it just takes longer.
	//
	// The refill is what the old fixed window lacked: the tokens come
	// back with time.
	waitForRateBudget(t, trunk)
}

// waitForRateBudget waits until the node's rate budget has refilled.
//
// The bucket refills at the cap's rate, so a node that spent its whole
// second's budget is usable again a moment later. That is the whole
// difference from the fixed window it replaced.
func waitForRateBudget(t *testing.T, trunk *RelayTrunk) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		trunk.mu.Lock()
		tokens := trunk.bucketTokens
		trunk.mu.Unlock()
		if tokens > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the node's rate budget never came back")
}
