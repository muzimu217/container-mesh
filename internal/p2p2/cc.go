// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import "time"

// Congestion control: a simplified Reno (RFC 5681).
//
// The sender already had a fixed maximum window (Config.SendWindow) and an RTO.
// What it lacked was any response to the network asking it to slow down, so on a
// path whose bandwidth-delay product is smaller than the configured window the
// sender kept the bottleneck queue full and every datagram above the path's
// capacity was dropped and then retransmitted. That is not only wasteful; it is
// unfair, because two flows sharing the bottleneck then divide it in proportion
// to window/RTT instead of converging on an equal share.
//
// The state machine is the textbook one:
//
//	slow start          cwnd < ssthresh: +1 datagram per acknowledged datagram,
//	                    which doubles the window once per round trip.
//	congestion avoid.   cwnd >= ssthresh: +1/cwnd per acknowledged datagram,
//	                    which adds one datagram per round trip.
//	fast recovery       a loss declared from ACKs (three later arrivals, or the
//	                    reordering timer) halves cwnd, sets ssthresh to the new
//	                    cwnd, and inflates cwnd by one per further duplicate ACK
//	                    so the sender can keep the pipe busy while the hole is
//	                    repaired. The first ACK that covers everything sent
//	                    before the loss deflates cwnd back to ssthresh.
//	RTO                 a timeout is a stronger signal than a SACK hole: the
//	                    window drops to the floor and slow start restarts.
//
// Four deliberate deviations from RFC 5681, each one measured:
//
//  1. The window is counted in datagrams, not bytes. Every datagram carries
//     MaxPayload bytes except the last fragment of a message, and the send path
//     already counts datagrams (SendWindow, inFlight). Counting bytes would add a
//     second unit to every check and buy nothing: the payload is constant by
//     construction.
//
//  2. The floor is ccMinWindow = 128 datagrams, not 1. Two effects set it:
//
//     - Fast retransmit needs three later arrivals to declare a hole, so a window
//     near the threshold turns every loss into an RTO. At a floor of 8 the
//     loopback KPI at 5% loss fell from 52.6 to 5.2 MiB/s, because each lost
//     datagram in an eight-datagram window stalled the ACK clock.
//
//     - A window is also the sample size for "is this path congested". On a
//     path that loses datagrams randomly, a small window loses one in most
//     round trips, and a controller that treats every loss as congestion pins
//     itself at the floor forever. 128 datagrams is large enough that a random
//     loss is a small fraction of the window, so the multiplicative decrease is
//     a response to the loss *rate* rather than to the existence of loss.
//
//     The floor is also capped by SendWindow, so a configuration with a small
//     ceiling keeps working.
//
//  3. Reductions are spaced at least one smoothed RTT apart. One congested
//     window produces losses across many datagrams, and each hole arrives as its
//     own signal; without the spacing a single overrun halves the window once per
//     hole (measured: 13 reductions in 200ms, cwnd 512 -> 12 for one congestion
//     event). One reduction per round trip is the Reno intent — the halving is
//     what the *next* round trip measures.
//
//  4. A loss declared from ACKs does not reduce the window when the window is
//     already at the floor, and a timeout does not reduce ssthresh twice for the
//     same congestion episode. Both are the same principle: repeated signals
//     inside one episode are the consequence of the event, not new evidence about
//     the path. Without them, a burst of losses collapses the threshold to the
//     floor and the rest of the transfer runs in congestion avoidance from 8.
//
// The arithmetic is integer throughout. The usual float64 cwnd accumulates
// 1/cwnd increments and then truncates for the window check, so a round trip of
// congestion avoidance can add 0.999999 and be read as 0 — a window that
// silently never grows. credit is the same accumulator in units of 1/cwnd
// without the rounding: one acknowledged datagram adds one credit, and a full
// window of credit buys one datagram of window.

const (
	// ccInitialWindow is the window before any feedback, in datagrams. Ten is
	// RFC 6928's initial window for a 1200-byte datagram (it recommends 10 MSS
	// for segments of 1460 bytes or less), and it is enough for a short request
	// to complete without waiting a round trip for window growth.
	ccInitialWindow = 10

	// ccMinWindow is the floor the window may be reduced to. See deviation 2
	// above for the two effects that set it.
	ccMinWindow = 128

	// ccBetaNum/ccBetaDen are the multiplicative-decrease factor: a loss event
	// halves the window (beta = 0.5), the Reno value.
	ccBetaNum = 1
	ccBetaDen = 2
)

// congestion is the congestion-control state of one sender. Every field is
// guarded by the owning sender's mutex; the type has no lock of its own.
type congestion struct {
	enabled bool
	max     int // Config.SendWindow: the ceiling the window may never exceed
	min     int // the floor, min(ccMinWindow, max)

	cwnd     int // datagrams in flight the path has been shown to carry
	ssthresh int // slow start ends here
	credit   int // congestion-avoidance credit, in units of 1/cwnd datagrams

	// inRecovery is Reno's fast-recovery state. recoveryPoint is the next
	// sequence number to be allocated when the loss was declared, so every
	// datagram below recoveryPoint was sent before the loss event. An ACK that
	// covers all of them ends recovery: that is the ACK that fills the hole.
	//
	// The comparison is modular, like every other sequence comparison in this
	// package, which is valid as long as the two values are within 2^31 of each
	// other — trivially true here, because largestAcked never lags nextSeq by
	// more than the send window.
	inRecovery    bool
	recoveryPoint uint32

	// episodePeak is the largest window seen since the current congestion episode
	// began. ssthresh is derived from it, never from the already-reduced window:
	// one episode can produce many signals (several holes declared lost by one
	// ACK, then one timeout per stuck head), and deriving the threshold from the
	// reduced value ratchets it down to the floor.
	episodePeak int
	// episodeReducedAt is the peak the last timeout reduction was computed from.
	// A timeout only lowers ssthresh when the window has since grown past that
	// peak; otherwise it is the same episode still producing stuck heads.
	episodeReducedAt int

	// lastReduction is when the window was last reduced; see deviation 3.
	lastReduction time.Time

	// srtt is the sender's smoothed round trip, used only to space reductions.
	srtt time.Duration

	// reductions counts window reductions, for diagnostics and KPI runs. It is
	// the number of times the sender responded to congestion, not the number of
	// lost datagrams.
	reductions uint64
}

func newCongestion(cfg Config) *congestion {
	c := &congestion{
		enabled: !cfg.DisableCongestionControl,
		max:     cfg.SendWindow,
	}
	if c.max < 1 {
		c.max = 1
	}
	c.min = ccMinWindow
	if c.min > c.max {
		c.min = c.max
	}
	c.cwnd = ccInitialWindow
	if c.cwnd > c.max {
		c.cwnd = c.max
	}
	if c.cwnd < c.min {
		c.cwnd = c.min
	}
	// ssthresh starts at the ceiling: there is no reason to leave slow start
	// before the path has shown a limit, and the window is capped by max anyway.
	c.ssthresh = c.max
	return c
}

// window returns the effective send window in datagrams: min(cwnd, SendWindow),
// never below one. With congestion control disabled it is the configured window,
// which is exactly the pre-CC behaviour.
func (c *congestion) window() int {
	if !c.enabled {
		return c.max
	}
	if c.cwnd > c.max {
		return c.max
	}
	if c.cwnd < 1 {
		return 1
	}
	return c.cwnd
}

// threshold reports ssthresh for diagnostics.
func (c *congestion) threshold() int {
	if !c.enabled {
		return c.max
	}
	return c.ssthresh
}

// reductionGap is the minimum interval between window reductions: one smoothed
// round trip. It is the pacing of AIMD, not an extra penalty — the point of
// halving is to let the next round trip show whether the halving was enough, and
// signals arriving inside the same round trip describe the same congested window.
func (c *congestion) reductionGap() time.Duration {
	if c.srtt <= 0 {
		return 0
	}
	return c.srtt
}

// onAck applies one ACK that newly acknowledged n datagrams. It returns whether
// the effective window grew, so the caller can wake a Send parked on the window.
//
// Recovery exit is checked before growth: the ACK that covers the recovery point
// ends the episode, and its growth is the deflation back to ssthresh rather than
// a slow-start step.
func (c *congestion) onAck(n int, largestAcked uint32, haveAck bool) bool {
	if !c.enabled || n <= 0 {
		return false
	}
	before := c.window()

	if c.inRecovery {
		if haveAck && !seqBefore(largestAcked, c.recoveryPoint-1) {
			c.inRecovery = false
			c.cwnd = c.ssthresh
			c.credit = 0
		}
		return c.window() > before
	}

	for i := 0; i < n && c.cwnd < c.max; i++ {
		if c.cwnd < c.ssthresh {
			c.cwnd++ // slow start: one datagram of window per acknowledged one
			continue
		}
		// Congestion avoidance: a full window of credit buys one datagram.
		c.credit++
		if c.credit >= c.cwnd {
			c.credit -= c.cwnd
			c.cwnd++
		}
	}
	if c.cwnd > c.episodePeak {
		c.episodePeak = c.cwnd
	}
	return c.window() > before
}

// onDupAck applies one duplicate ACK (one that acknowledged no new datagram).
// During fast recovery each duplicate inflates the window by one, which is what
// lets the sender keep transmitting while the hole is being repaired. Outside
// recovery a duplicate carries no new information and is ignored.
func (c *congestion) onDupAck() bool {
	if !c.enabled || !c.inRecovery {
		return false
	}
	before := c.window()
	if c.cwnd < c.max {
		c.cwnd++
	}
	return c.window() > before
}

// onLoss applies the response to a loss event declared from ACKs or the
// reordering timer. It returns whether the window changed, so the caller can
// re-check a parked Send.
//
// nextSeq is the sender's next sequence number, i.e. one past the highest
// datagram sent so far; it marks the end of the recovery period. now is the
// time the loss was declared, used to space reductions one round trip apart.
func (c *congestion) onLoss(nextSeq uint32, now time.Time) bool {
	if !c.enabled || c.inRecovery {
		return false
	}
	// Deviation 4: at the floor, an ACK-declared loss is not evidence of
	// congestion. See the type comment.
	if c.cwnd <= c.min {
		return false
	}
	// Deviation 3: one reduction per round trip.
	if !c.lastReduction.IsZero() && now.Sub(c.lastReduction) < c.reductionGap() {
		return false
	}
	if c.cwnd > c.episodePeak {
		c.episodePeak = c.cwnd
	}
	c.ssthresh = c.cwnd * ccBetaNum / ccBetaDen
	if c.ssthresh < c.min {
		c.ssthresh = c.min
	}
	c.cwnd = c.ssthresh
	c.credit = 0
	c.inRecovery = true
	c.episodeReducedAt = c.episodePeak
	c.recoveryPoint = nextSeq
	c.lastReduction = now
	c.reductions++
	return true
}

// onRTO applies the timeout response: ssthresh halves, the window drops to the
// floor, and slow start restarts from there. Recovery is abandoned because the
// RTO already covers everything the recovery period was waiting for.
//
// Two things are deliberately *not* done:
//
//   - ssthresh is not halved when the window is already at the floor. At that
//     point the sender is not outrunning the path; it is waiting for a
//     retransmission, and halving again only lengthens the next wait.
//
//   - ssthresh is not halved twice for the same episode. A burst of losses
//     produces one timeout per stuck head, and deriving the threshold from the
//     already-reduced window on each of them drives it to the floor: measured on
//     a simulated path as cwnd 512 -> ssthresh 8 from one loss burst, after which
//     the whole transfer ran in congestion avoidance from a threshold of 8.
func (c *congestion) onRTO(nextSeq uint32, now time.Time) {
	if !c.enabled {
		return
	}
	if c.cwnd <= c.min {
		c.inRecovery = false
		return
	}
	peak := c.episodePeak
	if peak < c.cwnd {
		peak = c.cwnd
	}
	if peak > c.episodeReducedAt {
		next := peak * ccBetaNum / ccBetaDen
		if next < c.min {
			next = c.min
		}
		c.ssthresh = next
		c.episodeReducedAt = peak
	}
	c.cwnd = c.min
	c.credit = 0
	c.inRecovery = false
	c.recoveryPoint = nextSeq
	c.lastReduction = now
	c.reductions++
}
