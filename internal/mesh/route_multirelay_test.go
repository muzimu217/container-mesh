package mesh

import (
	"context"
	"errors"
	"testing"

	"github.com/quic-go/quic-go"
)

// TestThePathLayerTriesTheNextRelayWhenOneCannotReachTheTarget is the
// regression test for the fallback blind spot the audit found.
//
// D73 promises "中继只兜底" — a relay is the fallback. But the fallback
// chain only worked ONE hop: the first relay that said "I cannot reach
// that target" was mapped to the REFUSAL class, and tryPaths treats a
// refusal as a VERDICT ("a rejection is a decision, not a network
// fault") and stops. The second relay was never tried.
//
// The distinction the code was missing: "you are not a member" is a
// verdict (presenting the same credentials to another path would be
// wrong), while "I cannot reach that target" is a LOCAL CONDITION — the
// next relay may well reach it, which is the whole point of having
// several.
//
// The test goes through the REAL mapping — the relay answer record the
// trunk sends, through relayRefusal, into tryPaths — because the bug
// was in the mapping, not in the walk.
func TestThePathLayerTriesTheNextRelayWhenOneCannotReachTheTarget(t *testing.T) {
	f := newTrunkFixture(t)

	// The answer the trunk sends when the target has not joined it.
	answer := relayRecord{
		Type:   0,
		Grant:  false,
		Reason: "this node cannot reach the target right now: the target has not joined this trunk",
		Class:  RelayClassUnavailable,
	}
	first := relayRefusal(answer)

	tried := []Path{}
	dialer := func(ctx context.Context, cfg DialConfig, p Path) (*Link, error) {
		tried = append(tried, p)
		if len(tried) == 1 {
			return nil, first
		}
		return nil, errors.New("the second relay also failed (the test stops here)")
	}

	paths := []Path{
		{Kind: PathDirect},
		{Kind: PathRelay, Via: f.hub.ID()},
		{Kind: PathRelay, Via: f.alice.ID()},
	}
	_, _, err := tryPaths(context.Background(), DialConfig{Target: f.bob.ID()}, paths, dialer)
	if err == nil {
		t.Fatal("the test expects the paths to fail")
	}

	// The point: the second relay must have been TRIED.
	if len(tried) < 3 {
		t.Fatalf("only %d path(s) were tried: the chain stopped at the first relay's refusal, so a second relay was never tried", len(tried))
	}
}

// TestARelayThatCannotReachTheTargetIsALocalCondition: the class
// itself.
//
// "I cannot reach that target" must NOT be a verdict. A verdict stops
// the path walk; a local condition lets it continue. The class the
// trunk sends for it is its own, and it maps to ErrRelayUnavailable,
// which the walk continues on.
func TestARelayThatCannotReachTheTargetIsALocalCondition(t *testing.T) {
	err := relayRefusal(relayRecord{
		Reason: "this node cannot reach the target right now",
		Class:  RelayClassUnavailable,
	})
	if errors.Is(err, ErrRefused) {
		t.Fatalf("the refusal is %v: a relay that cannot reach the target is a LOCAL condition, not a verdict, and must not stop the path walk", err)
	}
	if !errors.Is(err, ErrRelayUnavailable) {
		t.Fatalf("the refusal is %v, want ErrRelayUnavailable", err)
	}
}

// TestThePathLayerStillStopsOnACryptoFailure: the other half of the
// distinction. A TLS failure IS a verdict — the same credentials must
// not be presented to another path.
func TestThePathLayerStillStopsOnACryptoFailure(t *testing.T) {
	f := newTrunkFixture(t)

	tried := 0
	dialer := func(ctx context.Context, cfg DialConfig, p Path) (*Link, error) {
		tried++
		// A TLS alert code (0x100 + 42) is what a credential failure
		// looks like on the wire.
		return nil, &quic.TransportError{ErrorCode: quic.TransportErrorCode(0x100 + 42)}
	}

	paths := []Path{
		{Kind: PathDirect},
		{Kind: PathRelay, Via: f.hub.ID()},
		{Kind: PathRelay, Via: f.alice.ID()},
	}
	_, _, err := tryPaths(context.Background(), DialConfig{Target: f.bob.ID()}, paths, dialer)
	if err == nil {
		t.Fatal("the test expects the paths to fail")
	}
	if tried != 1 {
		t.Fatalf("%d path(s) were tried after a credential failure: that is a verdict and must stop the chain", tried)
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("the error is %v, want ErrAuth", err)
	}
}
