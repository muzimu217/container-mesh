package mesh

import (
	"testing"
	"time"
)

// TestTheDialBudgetOutlastsTheHandshake pins the relationship between
// the two timeouts that decide whether a connection can be made.
//
// The QUIC config allows a handshake a fixed amount of time
// (quic-go's own default is five, which is too short on a loaded
// machine). But the dialers gave the handshake less: a context shorter
// than the handshake it waits for cancels the handshake before it can
// finish.
//
// The symptom was "quic dial relay 127.0.0.1:56995: context deadline
// exceeded" on a busy machine, with nothing wrong at either end — the
// peer was ready and the dialer had already given up. It showed up in
// the relay's own test suite as an intermittent failure of the transfer
// that came after several successful ones, which is exactly the shape
// that reads like a product defect and is not.
//
// THE RULE is one line, and it is a property of the CODE, not of the
// machine:
//
//	the transport's ceiling must be STRICTLY SHORTER than the dial
//	budget it waits for.
//
// There are TWO numbers now, and the ORDER between them is the rule:
//
//	quicTransportHandshakeTimeout (15 s) < quicHandshakeTimeout (20 s)
//	                                        < the dial budget (30 s)
//
// The transport's ceiling is the SMALLEST, and that is deliberate: quic-go
// abandons a handshake that has not completed within
// HandshakeIdleTimeout, whatever the caller's context allows. So if the
// transport's number were the LARGEST, the dial budget's margin would be
// unreachable — the transport would give up first and the failure
// would read "timeout: no recent network activity", which looks like an
// unreachable peer rather than a slow one.
//
// That is not hypothetical: with the two numbers equal, the dial budget's
// "+10 s margin" was decorative, and a loaded machine saw the transport's
// 20-second ceiling instead of the budget's 30.
//
// (The rule used to read "every dial budget must be at least the
// handshake timeout", and the test pinned the two numbers being EQUAL.
// That was the shape of this defect: "one number" was mistaken for
// "consistent", and consistency here is the ORDER.)
func TestTheDialBudgetOutlastsTheHandshake(t *testing.T) {
	// The QUIC config's handshake timeout — the transport's OWN ceiling.
	cfg := quicConfig()
	if cfg.HandshakeIdleTimeout <= 0 {
		t.Fatal("the QUIC config has no handshake timeout")
	}
	if cfg.HandshakeIdleTimeout != quicTransportHandshakeTimeout {
		t.Fatalf("the QUIC config's handshake timeout is %s, want the transport's "+
			"%s: one number, so that the config and the dialers cannot disagree",
			cfg.HandshakeIdleTimeout, quicTransportHandshakeTimeout)
	}

	// THE ORDER THAT MATTERS: the transport must give up BEFORE the
	// caller's budget, or the budget's margin is unreachable.
	if cfg.HandshakeIdleTimeout >= quicHandshakeTimeout {
		t.Fatalf("the transport's handshake timeout is %s, not shorter than the %s "+
			"the dial budget is derived from: the transport would give up first and "+
			"the dialer's margin could never be spent",
			cfg.HandshakeIdleTimeout, quicHandshakeTimeout)
	}

	// The direct dial's default budget.
	dc := DialConfig{}
	if dc.HandshakeTimeout <= 0 {
		dc.HandshakeTimeout = quicHandshakeTimeout + 10*time.Second
	}
	if dc.HandshakeTimeout <= cfg.HandshakeIdleTimeout {
		t.Fatalf("the direct dial's budget is %s, not longer than the %s handshake "+
			"it waits for: the handshake is cancelled before it can finish",
			dc.HandshakeTimeout, cfg.HandshakeIdleTimeout)
	}

	// The relay dial's budget.
	relayBudget := quicHandshakeTimeout + 10*time.Second
	if relayBudget <= cfg.HandshakeIdleTimeout {
		t.Fatalf("the relay dial's budget is %s, not longer than the %s handshake "+
			"it waits for", relayBudget, cfg.HandshakeIdleTimeout)
	}
}
