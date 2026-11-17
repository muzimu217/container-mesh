// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/logging"
)

// lossTracker counts QUIC-level loss events observed on ONE connection.
//
// quic-go exposes no per-STREAM retransmission counter, but it does expose
// per-CONNECTION loss accounting in two INDEPENDENT ways:
//
//  1. conn.ConnectionStats().PacketsLost / .BytesLost
//     (quic-go/connection.go:800-813), incremented in
//     internal/congestion/cubic_sender.go:196-198 on every congestion event.
//  2. logging.ConnectionTracer.LostPacket
//     (quic-go/logging/connection_tracer.go:28), fired from
//     internal/ackhandler/sent_packet_handler.go:759 (time threshold) and
//     :769 (reordering threshold).
//
// Because the two paths are independent code paths counting the same events,
// their agreement is a cross-check that the number is real.
//
// The tracer is created once per connection by Config.Tracer, which quic-go
// calls at transport.go:294 (client) and server.go:709 (server). The hook
// therefore yields the PER-CONNECTION loss count this benchmark needs.
//
// A bench session runs on a dedicated QUIC connection carrying exactly one
// stream (RunBench opens a fresh link), so per-connection loss is the loss
// the bench stream suffered. This is stated explicitly in the bench output
// so the number is never mistaken for a QUIC stream-level counter.
//
// SCOPE: this file instruments ONLY the direct (QUIC) path. The relay path
// carries bytes over WebSocket and has no QUIC connection of its own, so it
// simply does not implement the optional lossReporter interface (see
// bench.go). Nothing here touches the relay.
type lossTracker struct {
	// lost counts packets declared lost (packet-threshold or time-threshold).
	lost atomic.Uint64
	// lostTimeThreshold / lostReorderThreshold split `lost` by the reason
	// quic-go reported. The split is what tells an operator whether the
	// link is losing packets (reordering threshold) or merely reordering
	// them (time threshold).
	lostTimeThreshold atomic.Uint64
	lostReorder       atomic.Uint64
	// spurious counts packets declared lost that were later acknowledged
	// (a reordering artefact, not real loss).
	spurious atomic.Uint64
	// pto counts loss-detection timer expirations in PTO mode.
	pto atomic.Uint64
	// lossTimer counts loss-detection timer expirations in ACK-threshold mode.
	lossTimer atomic.Uint64
	// ecnCongested counts ECN state transitions to "capable", i.e. the
	// number of times ECN congestion was POSSIBLE on this path. ECN
	// congestion is not a packet loss: it produces a congestion event with
	// lostBytes == 0, so it must not be counted as one. quic-go's tracer
	// exposes the ECN state machine but not the CE count itself, so this
	// counter is deliberately an indicator, not an exact event count.
	ecnCongested atomic.Uint64
	// cwndMin / cwndMax record the smallest and largest congestion window
	// quic-go reported during the run. A collapsing cwnd under loss is the
	// direct evidence of the congestion-control response.
	cwndMin atomic.Int64
	cwndMax atomic.Int64
	// cwndSum accumulates every observed cwnd, so the MEAN can be computed.
	// The mean is what a cwnd-limited flow actually operates at, and the
	// classic bound is throughput <= cwnd / RTT.
	cwndSum atomic.Int64
	// cwndLast is the most recent observation — the steady-state cwnd at
	// the end of the run.
	cwndLast atomic.Int64
	// cwndSamples counts how many UpdatedMetrics callbacks arrived.
	cwndSamples atomic.Uint64
	// peerStreamWindow / peerConnWindow are the flow-control windows the
	// PEER advertised (ReceivedTransportParameters). They are the windows
	// that limit what THIS side may send, so they are the other candidate
	// for "what is actually limiting the transfer".
	peerStreamWindow atomic.Int64
	peerConnWindow   atomic.Int64
	// inFlightSum / inFlightMax track BYTES IN FLIGHT, the quantity that
	// decides whether a flow is congestion-window limited.
	//
	// The decisive test for "is this flow cwnd-limited?" is not cwnd/RTT
	// (a bound), but the ratio bytesInFlight / cwnd: when the sender keeps
	// the pipe full, bytesInFlight sits AT the cwnd, and the flow is
	// limited by the congestion window. When bytesInFlight is much smaller
	// than cwnd, the sender is blocked by something else (flow control,
	// the application, the peer's read rate) and a bigger cwnd would not
	// help at all.
	inFlightSum atomic.Int64
	inFlightMax atomic.Int64
}

// tracer returns a quic-go ConnectionTracer that feeds this tracker.
//
// Only the hooks this build needs are installed; every other hook stays nil,
// which quic-go handles (each call site is guarded by a nil check).
func (t *lossTracker) tracer() *logging.ConnectionTracer {
	return &logging.ConnectionTracer{
		LostPacket: func(_ logging.EncryptionLevel, _ logging.PacketNumber, reason logging.PacketLossReason) {
			t.lost.Add(1)
			switch reason {
			case logging.PacketLossTimeThreshold:
				t.lostTimeThreshold.Add(1)
			case logging.PacketLossReorderingThreshold:
				t.lostReorder.Add(1)
			}
		},
		DetectedSpuriousLoss: func(logging.EncryptionLevel, logging.PacketNumber, uint64, time.Duration) {
			t.spurious.Add(1)
		},
		LossTimerExpired: func(timerType logging.TimerType, _ logging.EncryptionLevel) {
			switch timerType {
			case logging.TimerTypePTO:
				t.pto.Add(1)
			case logging.TimerTypeACK:
				t.lossTimer.Add(1)
			}
		},
		ReceivedTransportParameters: func(p *logging.TransportParameters) {
			if p == nil {
				return
			}
			t.peerStreamWindow.Store(int64(p.InitialMaxStreamDataBidiLocal))
			t.peerConnWindow.Store(int64(p.InitialMaxData))
		},
		ECNStateUpdated: func(state logging.ECNState, _ logging.ECNStateTrigger) {
			// This counts the transition INTO the capable state, which the
			// tracker fires at most once per connection (quic-go
			// ackhandler/ecn.go: capability is confirmed once and the state
			// machine stays there). It is therefore an indicator that the
			// path CAN carry congestion marks — not a count of congestion
			// events, and not a packet loss.
			//
			// A real CE event on a confirmed path reaches the congestion
			// controller (cwnd cutback) but NEITHER counter here: it does
			// not declare a packet lost, so it explains no difference
			// between PacketsLost and TracerLost.
			if state == logging.ECNStateCapable {
				t.ecnCongested.Add(1)
			}
		},
		UpdatedMetrics: func(_ *logging.RTTStats, cwnd, bytesInFlight logging.ByteCount, _ int) {
			v := int64(cwnd)
			if f := int64(bytesInFlight); f > 0 {
				t.inFlightSum.Add(f)
				for {
					old := t.inFlightMax.Load()
					if f <= old || t.inFlightMax.CompareAndSwap(old, f) {
						break
					}
				}
			}
			// cwndMax: monotonic high-water mark.
			for {
				old := t.cwndMax.Load()
				if v <= old || t.cwndMax.CompareAndSwap(old, v) {
					break
				}
			}
			// cwndMin: monotonic low-water mark. 0 means "no observation
			// yet", so the first observation always wins.
			for {
				old := t.cwndMin.Load()
				if old == 0 {
					if t.cwndMin.CompareAndSwap(0, v) {
						break
					}
					continue
				}
				if v >= old || t.cwndMin.CompareAndSwap(old, v) {
					break
				}
			}
			t.cwndSum.Add(v)
			t.cwndLast.Store(v)
			t.cwndSamples.Add(1)
		},
	}
}

// lossSnapshot is an immutable copy of the loss state.
type lossSnapshot struct {
	// TracerLost is the number of LostPacket tracer events. quic-go fires it
	// from internal/ackhandler/sent_packet_handler.go:759 and :769, i.e. it
	// counts the SAME loss events that ConnectionStats().PacketsLost
	// counts — but through an independent code path, so the two numbers
	// cross-check each other.
	TracerLost uint64
	// TracerLostTimeThreshold / TracerLostReordering split TracerLost by
	// the reason quic-go reported.
	TracerLostTimeThreshold uint64
	TracerLostReordering    uint64
	// Spurious is the subset of lost packets that was later acknowledged.
	Spurious uint64
	// PTO is the number of probe-timeout expirations.
	PTO uint64
	// LossTimer is the number of ACK-threshold loss-timer expirations.
	LossTimer uint64
	// ECNCongested is the number of ECN state transitions to "capable".
	ECNCongested uint64
	// PacketsLost / BytesLost come from quic-go's own ConnectionStats.
	PacketsLost uint64
	BytesLost   uint64
	// PacketsSent / PacketsReceived are quic-go's own counters.
	PacketsSent     uint64
	PacketsReceived uint64
	BytesSent       uint64
	BytesReceived   uint64
	// MinRTT / SmoothedRTT are quic-go's own RTT estimates.
	MinRTT      time.Duration
	SmoothedRTT time.Duration
	// CwndMin / CwndMax are the smallest and largest congestion window
	// quic-go reported during the run (bytes).
	CwndMin uint64
	CwndMax uint64
	// CwndSum is the sum of every observed congestion window and
	// CwndSamples the number of observations, so CwndMean = sum/samples.
	CwndSum uint64
	// CwndLast is the final observed congestion window.
	CwndLast uint64
	// CwndSamples is the number of congestion-window observations.
	CwndSamples uint64
	// PeerStreamWindow / PeerConnWindow are the flow-control windows the
	// PEER advertised to us (its receive window for our data). They limit
	// what this side may send, independently of the congestion window.
	PeerStreamWindow uint64
	PeerConnWindow   uint64
	// InFlightMean / InFlightMax are the bytes in flight (the sender's own
	// outstanding, unacknowledged data). InFlightMean / CwndMean is the
	// "is the pipe full?" ratio: at 1.0 the flow is congestion-window
	// limited; well below 1.0 the sender is blocked by something else.
	InFlightMean uint64
	InFlightMax  uint64
}

// trackerMu guards the process-wide registry of trackers so a caller can look
// up the tracker belonging to a given connection.
var (
	trackerMu sync.Mutex
	// trackers maps a connection-tracing id to its loss tracker.
	trackers = map[quic.ConnectionTracingID]*lossTracker{}
)

func registerTracker(id quic.ConnectionTracingID, t *lossTracker) {
	trackerMu.Lock()
	defer trackerMu.Unlock()
	trackers[id] = t
}

func lookupTracker(id quic.ConnectionTracingID) *lossTracker {
	trackerMu.Lock()
	defer trackerMu.Unlock()
	return trackers[id]
}

// dropTracker removes a tracker from the registry.
//
// It MUST be called when the connection closes, otherwise the registry grows
// for the lifetime of the process. quic-go calls ConnectionTracer.Close when
// the connection's run loop ends (connection.go:724-727) — except on the
// version-negotiation recreate path, where the tracer is deliberately NOT
// closed because the connection is re-created (connection.go:2005-2009).
// Dropping on Close therefore drops exactly the trackers whose connection is
// really gone.
func dropTracker(id quic.ConnectionTracingID) {
	trackerMu.Lock()
	defer trackerMu.Unlock()
	delete(trackers, id)
}

// trackerForConn returns the loss tracker registered for a connection, or nil.
//
// quic-go sets quic.ConnectionTracingKey on the context it hands to
// Config.Tracer (transport.go:286, server.go:703) and derives the
// connection's own context from it (transport.go:306 via
// context.WithoutCancel, server.go:730 via newConnection's ctx argument), so
// Conn.Context() carries the same id.
//
// NOTE: quic-go marks both ConnectionTracingKey and ConnectionTracingID as
// deprecated in favour of Transport.ConnContext. The deprecation is about
// applications wanting their OWN tracing key; the pair still works in
// v0.55.0 and is the only way to correlate a tracer with a connection
// without owning the Transport. If quic-go ever removes it, the alternative
// is to key the tracker by the QUIC connection ID (the Tracer hook receives
// it) — but that ID can be rotated, so the tracing id is the right key
// today.
func trackerForConn(conn *quic.Conn) *lossTracker {
	if conn == nil {
		return nil
	}
	id, ok := conn.Context().Value(quic.ConnectionTracingKey).(quic.ConnectionTracingID)
	if !ok {
		return nil
	}
	return lookupTracker(id)
}

// read combines the tracer counters with quic-go's own ConnectionStats.
//
// ConnectionStats() is readable at ANY time — it is a set of atomic counters
// (internal/utils/connstats.go), not a snapshot taken at connection close.
// Reading it after the transfer therefore yields the loss that the transfer
// actually suffered, without needing to close the connection first.
func (t *lossTracker) read(conn *quic.Conn) lossSnapshot {
	s := lossSnapshot{
		TracerLost:              t.lost.Load(),
		TracerLostTimeThreshold: t.lostTimeThreshold.Load(),
		TracerLostReordering:    t.lostReorder.Load(),
		Spurious:                t.spurious.Load(),
		PTO:                     t.pto.Load(),
		LossTimer:               t.lossTimer.Load(),
		ECNCongested:            t.ecnCongested.Load(),
		CwndMin:                 uint64(t.cwndMin.Load()),
		CwndMax:                 uint64(t.cwndMax.Load()),
		CwndSum:                 uint64(t.cwndSum.Load()),
		CwndLast:                uint64(t.cwndLast.Load()),
		CwndSamples:             t.cwndSamples.Load(),
		InFlightMean:            uint64(t.inFlightSum.Load()) / max(t.cwndSamples.Load(), 1),
		InFlightMax:             uint64(t.inFlightMax.Load()),
		PeerStreamWindow:        uint64(t.peerStreamWindow.Load()),
		PeerConnWindow:          uint64(t.peerConnWindow.Load()),
	}
	if conn != nil {
		cs := conn.ConnectionStats()
		s.PacketsLost = cs.PacketsLost
		s.BytesLost = cs.BytesLost
		s.PacketsSent = cs.PacketsSent
		s.PacketsReceived = cs.PacketsReceived
		s.BytesSent = cs.BytesSent
		s.BytesReceived = cs.BytesReceived
		s.MinRTT = cs.MinRTT
		s.SmoothedRTT = cs.SmoothedRTT
	}
	return s
}

// tracingConfig returns a copy of base whose Tracer hook records loss events
// into a fresh per-connection tracker, registered under the connection's
// tracing id so the owning code can retrieve it afterwards.
//
// ISOLATION (this is the "does the global hook affect both paths?" question):
//
//   - The hook is per-CONNECTION, not per-process-global state: quic-go calls
//     Config.Tracer exactly ONCE per connection, and the hook creates a fresh
//     tracker for that connection. Two connections never share a tracker.
//
//   - The hook is installed on the config used by the DIRECT path only
//     (quicConfig() in direct.go). The relay path never dials QUIC, so no
//     relay connection ever passes through this hook. There is no cross-path
//     contamination.
//
//   - The hook chains to any pre-existing tracer: a caller that already set
//     base.Tracer keeps its tracer AND gets the loss counters. The repo sets
//     no tracer today, so in practice this is a no-op, but it keeps the
//     helper safe to reuse.
//
//   - The only process-wide state is the tracker registry, which is keyed by
//     the per-connection tracing id and pruned on connection close.
func tracingConfig(base *quic.Config) *quic.Config {
	cfg := *base
	prev := cfg.Tracer
	cfg.Tracer = func(ctx context.Context, p logging.Perspective, connID quic.ConnectionID) *logging.ConnectionTracer {
		tracker := &lossTracker{}
		if v, ok := ctx.Value(quic.ConnectionTracingKey).(quic.ConnectionTracingID); ok {
			id := v
			registerTracker(id, tracker)
			// Prune the registry when the connection is really gone. quic-go
			// calls Tracer.Close at connection.go:724-727, except on the
			// version-negotiation recreate path where the connection is
			// re-created and the tracer must stay alive.
			tracer := tracker.tracer()
			tracer.Close = func() {
				dropTracker(id)
			}
			if prev != nil {
				if outer := prev(ctx, p, connID); outer != nil {
					chainTracers(tracer, outer)
				}
			}
			return tracer
		}
		// No tracing id: the connection cannot be looked up later, so the
		// counters would be unreachable. Still return a tracer (so the loss
		// events are recorded) but do not register it.
		tracer := tracker.tracer()
		if prev != nil {
			if outer := prev(ctx, p, connID); outer != nil {
				chainTracers(tracer, outer)
			}
		}
		return tracer
	}
	return &cfg
}

// chainTracers installs the outer tracer's hooks onto inner, where inner
// does not already define them.
//
// The merged tracer is the one the loss tracker fills in. Both trackers then
// observe the same events, which is what makes the two counters independent
// cross-checks of the same events.
func chainTracers(inner, outer *logging.ConnectionTracer) {
	if outer.StartedConnection != nil && inner.StartedConnection == nil {
		inner.StartedConnection = outer.StartedConnection
	}
	if outer.NegotiatedVersion != nil && inner.NegotiatedVersion == nil {
		inner.NegotiatedVersion = outer.NegotiatedVersion
	}
	if outer.ClosedConnection != nil && inner.ClosedConnection == nil {
		inner.ClosedConnection = outer.ClosedConnection
	}
	if outer.SentTransportParameters != nil && inner.SentTransportParameters == nil {
		inner.SentTransportParameters = outer.SentTransportParameters
	}
	if outer.ReceivedTransportParameters != nil && inner.ReceivedTransportParameters == nil {
		inner.ReceivedTransportParameters = outer.ReceivedTransportParameters
	}
	if outer.SentLongHeaderPacket != nil && inner.SentLongHeaderPacket == nil {
		inner.SentLongHeaderPacket = outer.SentLongHeaderPacket
	}
	if outer.SentShortHeaderPacket != nil && inner.SentShortHeaderPacket == nil {
		inner.SentShortHeaderPacket = outer.SentShortHeaderPacket
	}
	if outer.ReceivedLongHeaderPacket != nil && inner.ReceivedLongHeaderPacket == nil {
		inner.ReceivedLongHeaderPacket = outer.ReceivedLongHeaderPacket
	}
	if outer.ReceivedShortHeaderPacket != nil && inner.ReceivedShortHeaderPacket == nil {
		inner.ReceivedShortHeaderPacket = outer.ReceivedShortHeaderPacket
	}
	if outer.BufferedPacket != nil && inner.BufferedPacket == nil {
		inner.BufferedPacket = outer.BufferedPacket
	}
	if outer.DroppedPacket != nil && inner.DroppedPacket == nil {
		inner.DroppedPacket = outer.DroppedPacket
	}
	if outer.AcknowledgedPacket != nil && inner.AcknowledgedPacket == nil {
		inner.AcknowledgedPacket = outer.AcknowledgedPacket
	}
	if outer.UpdatedMTU != nil && inner.UpdatedMTU == nil {
		inner.UpdatedMTU = outer.UpdatedMTU
	}
	if outer.UpdatedCongestionState != nil && inner.UpdatedCongestionState == nil {
		inner.UpdatedCongestionState = outer.UpdatedCongestionState
	}
	if outer.UpdatedPTOCount != nil && inner.UpdatedPTOCount == nil {
		inner.UpdatedPTOCount = outer.UpdatedPTOCount
	}
	if outer.UpdatedKeyFromTLS != nil && inner.UpdatedKeyFromTLS == nil {
		inner.UpdatedKeyFromTLS = outer.UpdatedKeyFromTLS
	}
	if outer.UpdatedKey != nil && inner.UpdatedKey == nil {
		inner.UpdatedKey = outer.UpdatedKey
	}
	if outer.DroppedEncryptionLevel != nil && inner.DroppedEncryptionLevel == nil {
		inner.DroppedEncryptionLevel = outer.DroppedEncryptionLevel
	}
	if outer.DroppedKey != nil && inner.DroppedKey == nil {
		inner.DroppedKey = outer.DroppedKey
	}
	if outer.SetLossTimer != nil && inner.SetLossTimer == nil {
		inner.SetLossTimer = outer.SetLossTimer
	}
	if outer.LossTimerCanceled != nil && inner.LossTimerCanceled == nil {
		inner.LossTimerCanceled = outer.LossTimerCanceled
	}
	if outer.ECNStateUpdated != nil && inner.ECNStateUpdated == nil {
		inner.ECNStateUpdated = outer.ECNStateUpdated
	}
	if outer.ChoseALPN != nil && inner.ChoseALPN == nil {
		inner.ChoseALPN = outer.ChoseALPN
	}
	if outer.Debug != nil && inner.Debug == nil {
		inner.Debug = outer.Debug
	}
}
