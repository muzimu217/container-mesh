// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"testing"
	"time"
)

// ccFor builds a controller with the production defaults.
func ccFor(cfg Config) *congestion {
	cfg.fill()
	return newCongestion(cfg)
}

// TestCCSlowStartDoublesPerRoundTrip checks the defining property of slow start:
// an ACK that acknowledges n datagrams grows the window by n, so a window's worth
// of ACKs doubles it once per round trip.
//
// This is the growth that makes a transfer ramp up from 10 datagrams to the
// path's capacity in log2(capacity/10) round trips instead of linearly.
func TestCCSlowStartDoublesPerRoundTrip(t *testing.T) {
	c := ccFor(DefaultConfig())
	// The floor is above the RFC 6928 initial window, so the window starts at the
	// floor and slow start doubles from there.
	if c.window() != ccMinWindow {
		t.Fatalf("initial window = %d, want the %d floor", c.window(), ccMinWindow)
	}
	start := c.window()

	// One round trip: the whole window is acknowledged.
	c.onAck(start, 9, true)
	if got, want := c.window(), 2*start; got != want {
		t.Fatalf("after one round trip of ACKs the window is %d, want %d", got, want)
	}

	// A second round trip doubles again.
	c.onAck(c.window(), 29, true)
	if got, want := c.window(), 4*start; got != want {
		t.Fatalf("after two round trips the window is %d, want %d", got, want)
	}

	// The window must never exceed SendWindow even if ACKs keep arriving.
	for i := 0; i < 100; i++ {
		c.onAck(1000, uint32(1000*(i+1)), true)
	}
	if got := c.window(); got != c.max {
		t.Fatalf("window grew to %d, above the %d ceiling", got, c.max)
	}
}

// TestCCCongestionAvoidanceAddsOnePerRoundTrip checks the additive-increase half
// of AIMD: once cwnd reaches ssthresh, one window of ACKs adds exactly one
// datagram, not a doubling.
func TestCCCongestionAvoidanceAddsOnePerRoundTrip(t *testing.T) {
	c := ccFor(DefaultConfig())
	// Put the controller into congestion avoidance at a known window.
	c.ssthresh = 20
	c.cwnd = 20

	// Ten round trips of a full window of ACKs. AIMD adds one datagram per round
	// trip, so the window must land at 30, not at 20 * 2^10.
	for i := 0; i < 10; i++ {
		c.onAck(c.window(), uint32(100+i), true)
	}
	if got := c.window(); got != 30 {
		t.Fatalf("after ten round trips in congestion avoidance the window is %d, want 30", got)
	}
}

// TestCCLossHalvesWindowAndEntersRecovery checks the multiplicative-decrease
// response to a SACK-detected loss: ssthresh becomes half the window, cwnd drops
// to it, and the controller enters fast recovery.
func TestCCLossHalvesWindowAndEntersRecovery(t *testing.T) {
	c := ccFor(DefaultConfig())
	c.cwnd = 400
	c.ssthresh = 1024

	if !c.onLoss(500, time.Time{}) {
		t.Fatal("the first loss of a period did not reduce the window")
	}
	if got := c.window(); got != 200 {
		t.Fatalf("window after loss = %d, want 200 (half of 400)", got)
	}
	if got := c.threshold(); got != 200 {
		t.Fatalf("ssthresh after loss = %d, want 200", got)
	}
	if !c.inRecovery {
		t.Fatal("a loss declared from ACKs must enter fast recovery")
	}

	// A second loss in the same period is the same congestion event and must not
	// halve the window again. Without this a single ACK declaring several holes
	// lost would collapse the window to the floor.
	if c.onLoss(500, time.Time{}) {
		t.Fatal("a second loss in the same recovery period reduced the window again")
	}
	if got := c.window(); got != 200 {
		t.Fatalf("window after a second loss in the same period = %d, want 200", got)
	}
	if c.reductions != 1 {
		t.Fatalf("reductions = %d, want 1", c.reductions)
	}
}

// TestCCFastRecoveryInflatesAndDeflates checks both halves of Reno fast
// recovery: duplicate ACKs inflate the window so the pipe stays busy, and the
// ACK that covers the recovery point deflates it back to ssthresh.
func TestCCFastRecoveryInflatesAndDeflates(t *testing.T) {
	c := ccFor(DefaultConfig())
	c.cwnd = 400
	c.ssthresh = 1024
	c.onLoss(1000, time.Time{}) // recovery point: nothing sent before 1000 is outstanding

	if got := c.window(); got != 200 {
		t.Fatalf("window after loss = %d, want 200", got)
	}

	// Three duplicate ACKs inflate the window by three: the sender may transmit
	// three new datagrams while the hole is repaired.
	for i := 0; i < 3; i++ {
		if !c.onDupAck() {
			t.Fatalf("duplicate ACK %d did not inflate the window", i+1)
		}
	}
	if got := c.window(); got != 203 {
		t.Fatalf("window after three duplicate ACKs = %d, want 203", got)
	}

	// An ACK below the recovery point is a partial ACK: it must not end recovery.
	c.onAck(1, 900, true)
	if !c.inRecovery {
		t.Fatal("an ACK below the recovery point ended fast recovery")
	}

	// The ACK that covers everything sent before the loss ends recovery and
	// deflates the window to ssthresh.
	c.onAck(1, 999, true)
	if c.inRecovery {
		t.Fatal("the ACK covering the recovery point did not end fast recovery")
	}
	if got := c.window(); got != 200 {
		t.Fatalf("window after deflation = %d, want 200", got)
	}
}

// TestCCRTOsResetToFloorNotToZero checks the timeout response and, more
// importantly, that repeated congestion can never take the window to zero.
//
// A zero window is a deadlock, not a slow path: Send waits for room that no ACK
// will ever create, and the connection only ends through the idle timeout. The
// floor is ccMinWindow, chosen so that fast retransmit still has enough later
// datagrams to fire.
func TestCCRTOsResetToFloorNotToZero(t *testing.T) {
	c := ccFor(DefaultConfig())
	c.cwnd = 600
	c.ssthresh = 1024

	c.onRTO(500, time.Time{})
	if got := c.window(); got != ccMinWindow {
		t.Fatalf("window after RTO = %d, want the %d floor", got, ccMinWindow)
	}
	if got := c.threshold(); got != 300 {
		t.Fatalf("ssthresh after RTO = %d, want 300 (half of 600)", got)
	}

	// Twenty consecutive timeouts must still leave a usable window.
	for i := 0; i < 20; i++ {
		c.onRTO(uint32(500+i), time.Time{})
		if got := c.window(); got < ccMinWindow {
			t.Fatalf("window fell to %d after %d timeouts, below the %d floor",
				got, i+1, ccMinWindow)
		}
	}
	if c.window() == 0 {
		t.Fatal("window reached zero: Send would block forever")
	}
	if c.threshold() < ccMinWindow {
		t.Fatalf("ssthresh fell to %d, below the %d floor", c.threshold(), ccMinWindow)
	}
}

// TestCCSlowStartRestartsAfterRTO checks that the timeout response actually
// re-enters slow start rather than leaving the controller in congestion
// avoidance: after an RTO cwnd is at the floor and ssthresh is above it, so the
// next ACK must double the window rather than add 1/cwnd.
func TestCCSlowStartRestartsAfterRTO(t *testing.T) {
	c := ccFor(DefaultConfig())
	c.cwnd = 600
	c.ssthresh = 1024
	c.onRTO(500, time.Time{})

	if c.cwnd >= c.ssthresh {
		t.Fatalf("after RTO cwnd %v is not below ssthresh %v: not in slow start", c.cwnd, c.ssthresh)
	}
	before := c.window()
	c.onAck(before, 500, true)
	if got := c.window(); got != 2*before {
		t.Fatalf("the first ACK after an RTO grew the window to %d, want %d (slow start)",
			got, 2*before)
	}
}

// TestCCDisabledIsFixedWindow checks the A/B switch: with congestion control off
// the effective window is exactly SendWindow at all times, whatever the loss
// signals say. This is what makes a before/after measurement meaningful — the
// disabled path must be the pre-CC path, not a differently-tuned one.
func TestCCDisabledIsFixedWindow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DisableCongestionControl = true
	c := ccFor(cfg)

	if got := c.window(); got != cfg.SendWindow {
		t.Fatalf("disabled window = %d, want %d", got, cfg.SendWindow)
	}
	c.onLoss(100, time.Time{})
	if got := c.window(); got != cfg.SendWindow {
		t.Fatalf("a loss changed the disabled window to %d", got)
	}
	c.onRTO(200, time.Time{})
	if got := c.window(); got != cfg.SendWindow {
		t.Fatalf("an RTO changed the disabled window to %d", got)
	}
	for i := 0; i < 100; i++ {
		c.onAck(10, uint32(300+i), true)
	}
	if got := c.window(); got != cfg.SendWindow {
		t.Fatalf("ACKs changed the disabled window to %d", got)
	}
}

// TestCCSmallSendWindowFloor checks the degenerate configuration: if SendWindow
// is below ccMinWindow the floor must follow it down, not pin the window above
// the configured ceiling.
func TestCCSmallSendWindowFloor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SendWindow = 4
	c := ccFor(cfg)

	if got := c.window(); got != 4 {
		t.Fatalf("window = %d, want the 4 configured", got)
	}
	c.cwnd = 4
	c.onRTO(10, time.Time{})
	if got := c.window(); got != 4 {
		t.Fatalf("window after RTO = %d, want 4: the floor must not exceed SendWindow", got)
	}
	if got := c.window(); got == 0 {
		t.Fatal("window reached zero")
	}
}

// TestCCZeroAckDoesNotGrow checks that an ACK which acknowledges nothing (a
// duplicate outside recovery) cannot grow the window. The window is driven by
// delivered data, not by ACK arrivals.
func TestCCZeroAckDoesNotGrow(t *testing.T) {
	c := ccFor(DefaultConfig())
	before := c.window()
	if c.onAck(0, 5, true) {
		t.Fatal("an ACK acknowledging nothing grew the window")
	}
	if got := c.window(); got != before {
		t.Fatalf("window moved from %d to %d on an empty ACK", before, got)
	}
	// A duplicate ACK outside recovery is ignored too.
	if c.onDupAck() {
		t.Fatal("a duplicate ACK outside recovery grew the window")
	}
}

// TestCCObservedInRealTransfer is the end-to-end wiring check: a real transfer
// over a lossy path must move the CC counters, and the reported cwnd must stay
// within [floor, ceiling]. The unit tests above drive the controller directly;
// this one proves the sender actually calls it.
func TestCCObservedInRealTransfer(t *testing.T) {
	a, b := pair(t, DefaultConfig(), 0.20)

	msg := pad('C', 200)
	go func() {
		if err := a.Send(msg); err != nil {
			t.Logf("send: %v", err)
		}
	}()
	if _, err := b.Recv(); err != nil {
		t.Fatalf("recv: %v", err)
	}

	// The window is reported live; by the time the message is delivered the
	// sender may have finished and the window may have grown back. Poll for the
	// reduction counter rather than for an instantaneous window value.
	deadline := time.Now().Add(2 * time.Second)
	var st Stats
	for {
		st = a.Stats()
		if st.CCReductions > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !st.CCEnabled {
		t.Fatal("congestion control is reported as disabled on a default config")
	}
	if st.CCReductions == 0 {
		t.Fatal("20% loss produced no congestion response: the sender never called the controller")
	}
	if st.Cwnd < ccMinWindow || st.Cwnd > st.SendWindow {
		t.Fatalf("reported cwnd %d is outside [%d, %d]", st.Cwnd, ccMinWindow, st.SendWindow)
	}
	if st.Ssthresh > st.SendWindow {
		t.Fatalf("reported ssthresh %d is above the %d ceiling", st.Ssthresh, st.SendWindow)
	}
	t.Logf("20%% loss: cwnd=%d ssthresh=%d reductions=%d retrans=%d rto=%d",
		st.Cwnd, st.Ssthresh, st.CCReductions, st.Retrans, st.RTOTimeouts)
}

// TestCCWindowBoundsInFlight is the safety property: with congestion control on,
// in-flight datagrams must never exceed the reported congestion window, and the
// congestion window must never exceed SendWindow. It is the CC counterpart of
// TestSendWindowBoundsInFlight.
func TestCCWindowBoundsInFlight(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SendWindow = 64
	a, b := pair(t, cfg, 0)

	msg := make([]byte, 400*MaxPayload)
	go func() {
		if err := a.Send(msg); err != nil {
			t.Logf("send returned %v", err)
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	var maxInFlight, maxCwnd int
	for time.Now().Before(deadline) {
		st := a.Stats()
		if st.InFlight > maxInFlight {
			maxInFlight = st.InFlight
		}
		if st.Cwnd > maxCwnd {
			maxCwnd = st.Cwnd
		}
		// A snapshot reads InFlight and Cwnd under one lock, so InFlight > Cwnd
		// would mean the window check and the accounting disagree.
		if st.InFlight > st.Cwnd && st.Cwnd > 0 {
			t.Fatalf("in flight %d exceeds the congestion window %d", st.InFlight, st.Cwnd)
		}
		if st.Cwnd > cfg.SendWindow {
			t.Fatalf("cwnd %d exceeds SendWindow %d", st.Cwnd, cfg.SendWindow)
		}
		time.Sleep(200 * time.Microsecond)
	}
	if maxInFlight == 0 {
		t.Fatal("nothing was ever in flight")
	}
	t.Logf("max in flight %d, max cwnd %d (ceiling %d)", maxInFlight, maxCwnd, cfg.SendWindow)
	_ = b.Close()
	_ = a.Close()
}

// TestCCNoReductionAtFloor is deviation 4: once the window is at the floor, an
// ACK-declared loss is not evidence of congestion. On a path that loses
// datagrams randomly a small window loses one in most round trips, and a
// controller that kept halving would sit at the floor for the whole transfer.
func TestCCNoReductionAtFloor(t *testing.T) {
	c := ccFor(DefaultConfig())
	c.cwnd = ccMinWindow
	c.ssthresh = c.max

	if c.onLoss(100, time.Time{}) {
		t.Fatal("a loss at the floor reduced the window")
	}
	if got := c.window(); got != ccMinWindow {
		t.Fatalf("window = %d, want the %d floor", got, ccMinWindow)
	}
	// A timeout at the floor must also leave the threshold alone: the sender is
	// not outrunning the path, it is waiting for a retransmission.
	c.onRTO(200, time.Time{})
	if got := c.threshold(); got != c.max {
		t.Fatalf("ssthresh = %d after a timeout at the floor, want the unchanged %d", got, c.max)
	}
}

// TestCCReductionsAreSpacedOneRoundTrip is deviation 3: one congested window
// produces many loss signals, and only the first of a round trip reduces the
// window.
func TestCCReductionsAreSpacedOneRoundTrip(t *testing.T) {
	c := ccFor(DefaultConfig())
	c.srtt = 10 * time.Millisecond
	c.cwnd = 800
	c.ssthresh = 1024

	base := time.Now()
	if !c.onLoss(100, base) {
		t.Fatal("the first loss did not reduce the window")
	}
	// Recovery exits so a second loss can be considered at all.
	c.onAck(1, 99, true)
	if c.inRecovery {
		t.Fatal("the ACK covering the recovery point did not end recovery")
	}
	// A second loss 2ms later is inside the same round trip.
	if c.onLoss(200, base.Add(2*time.Millisecond)) {
		t.Fatal("a loss inside the same round trip reduced the window again")
	}
	// A loss one round trip later is a new episode.
	if !c.onLoss(300, base.Add(11*time.Millisecond)) {
		t.Fatal("a loss one round trip later did not reduce the window")
	}
	if c.reductions != 2 {
		t.Fatalf("reductions = %d, want 2", c.reductions)
	}
}

// TestCCSsthreshSurvivesLossBurst is the episode rule for timeouts: a burst of
// losses produces one timeout per stuck head, and the threshold must be derived
// from the window the episode started at rather than from the already-reduced
// value. Without it one burst drove ssthresh from 512 to 8 and left the rest of
// the transfer in congestion avoidance from a threshold of 8.
func TestCCSsthreshSurvivesLossBurst(t *testing.T) {
	c := ccFor(DefaultConfig())
	c.cwnd = 512
	c.ssthresh = 1024

	c.onRTO(100, time.Time{})
	first := c.threshold()
	if first != 256 {
		t.Fatalf("ssthresh after the first timeout = %d, want 256 (half of 512)", first)
	}
	for i := 0; i < 20; i++ {
		c.onRTO(uint32(200+i), time.Time{})
	}
	if got := c.threshold(); got != first {
		t.Fatalf("ssthresh fell from %d to %d across one loss burst", first, got)
	}
	if got := c.window(); got != ccMinWindow {
		t.Fatalf("window = %d, want the %d floor", got, ccMinWindow)
	}
}

// TestCCFloorRespectsSmallSendWindow checks that the floor never exceeds the
// configured ceiling, so a small SendWindow still works.
func TestCCFloorRespectsSmallSendWindow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SendWindow = 16
	c := ccFor(cfg)
	if got := c.window(); got != 16 {
		t.Fatalf("window = %d, want the configured 16", got)
	}
	c.onRTO(10, time.Time{})
	if got := c.window(); got != 16 {
		t.Fatalf("window after RTO = %d, want 16", got)
	}
}
