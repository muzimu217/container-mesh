package mesh

import (
	"testing"
	"time"
)

// TestTheDialBudgetOutlastsTheHandshake pins the relationship between
// the two timeouts that decide whether a connection can be made.
//
// The QUIC config allows a handshake TWENTY seconds (quic-go's own
// default is five, which is too short on a loaded machine). But the
// dialers gave the handshake TEN: a context shorter than the handshake
// it waits for cancels the handshake before it can finish.
//
// The symptom was "quic dial relay 127.0.0.1:56995: context deadline
// exceeded" on a busy machine, with nothing wrong at either end — the
// peer was ready and the dialer had already given up. It showed up in
// the relay's own test suite as an intermittent failure of the transfer
// that came after several successful ones, which is exactly the shape
// that reads like a product defect and is not.
//
// The rule is one line, and it is a property of the CODE, not of the
// machine: every dial budget must be at least the handshake timeout.
func TestTheDialBudgetOutlastsTheHandshake(t *testing.T) {
	// The QUIC config's handshake timeout.
	cfg := quicConfig()
	if cfg.HandshakeIdleTimeout <= 0 {
		t.Fatal("the QUIC config has no handshake timeout")
	}
	if cfg.HandshakeIdleTimeout != quicHandshakeTimeout {
		t.Fatalf("the QUIC config's handshake timeout is %s, want the shared "+
			"%s: one number, so that a dialer and the config cannot disagree",
			cfg.HandshakeIdleTimeout, quicHandshakeTimeout)
	}

	// The direct dial's default budget.
	dc := DialConfig{}
	if dc.HandshakeTimeout <= 0 {
		dc.HandshakeTimeout = quicHandshakeTimeout + 10*time.Second
	}
	if dc.HandshakeTimeout < quicHandshakeTimeout {
		t.Fatalf("the direct dial's budget is %s, shorter than the %s handshake "+
			"it waits for: the handshake is cancelled before it can finish",
			dc.HandshakeTimeout, quicHandshakeTimeout)
	}

	// The relay dial's budget.
	relayBudget := quicHandshakeTimeout + 10*time.Second
	if relayBudget < quicHandshakeTimeout {
		t.Fatalf("the relay dial's budget is %s, shorter than the %s handshake",
			relayBudget, quicHandshakeTimeout)
	}
}
