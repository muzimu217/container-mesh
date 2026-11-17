// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/quic-go/quic-go"
)

// BenchConfig describes one bidirectional direct-link benchmark. Size applies
// independently to each direction. RateBytesPerSecond applies independently
// to each direction; zero means uncapped.
type BenchConfig struct {
	Size               int64
	RateBytesPerSecond int64
}

// BenchStats reports application bytes moved by a completed benchmark.
//
// Loss accounting: quic-go exposes no per-STREAM retransmission counter, but
// it does expose per-CONNECTION loss accounting (ConnectionStats().PacketsLost
// and the ConnectionTracer.LostPacket hook). A bench session runs on a
// dedicated QUIC connection carrying exactly one stream, so the
// per-connection loss count IS the loss this bench stream suffered.
//
// The field is named PacketsLost, NOT Retransmissions, on purpose: the
// number quic-go reports is the count of packets DECLARED LOST, which is not
// the same as the count of packets retransmitted. A packet declared lost may
// turn out to have been merely reordered and later arrive (quic-go calls
// those spurious losses, and they are reported separately). The old name
// "Retransmissions" therefore promised a quantity the number does not
// deliver, and callers that trusted it would over-count recovery work.
//
// PacketsLost holds the connection's declared-lost packet count when the
// link could be instrumented, and nil when it could not (for example when
// the peer answered on a connection this process did not dial). nil means
// "not measured" — it is NOT zero.
type BenchStats struct {
	SentBytes     int64
	ReceivedBytes int64
	Duration      time.Duration
	// PacketsLost is the number of QUIC packets the local side's
	// connection declared lost. Per-CONNECTION, not per-stream.
	PacketsLost *uint64
	// Loss carries the full per-connection loss picture of the LOCAL side
	// (the packets this side sent that quic-go declared lost). It is nil
	// when the connection could not be instrumented.
	Loss *BenchLoss
	// PeerLoss carries the ANSWERING side's loss picture, as reported by
	// the peer over the bench stats frame. It is nil when the peer did not
	// report one.
	PeerLoss *BenchLoss
}

// BenchLoss is the per-connection QUIC loss picture of one bench session.
//
// All counters come from quic-go: the tracer hooks (LostPacket,
// DetectedSpuriousLoss, LossTimerExpired) and ConnectionStats().
type BenchLoss struct {
	// PacketsLost is the number of packets quic-go declared lost, as
	// counted by the congestion controller (ConnectionStats). It is the
	// count that actually drove the congestion window down.
	PacketsLost uint64 `json:"packets_lost"`
	// BytesLost is the number of bytes quic-go declared lost.
	BytesLost uint64 `json:"bytes_lost"`
	// TracerLost is the number of LostPacket tracer events. It is an
	// INDEPENDENT count of the same loss events (tracer path vs
	// ConnectionStats path), so the two cross-check each other.
	//
	// The tracer counts slightly MORE than PacketsLost in one case: a
	// Path-MTU probe packet that is declared lost fires the tracer hook but
	// never reaches the congestion controller
	// (quic-go/internal/ackhandler/sent_packet_handler.go:789-790 skips
	// path probes for OnCongestionEvent). So TracerLost >= PacketsLost,
	// and the difference is the number of path-probe losses.
	TracerLost uint64 `json:"tracer_lost"`
	// TracerLostTimeThreshold / TracerLostReordering split TracerLost by
	// the reason quic-go reported: a time-threshold loss is a packet the
	// timer gave up on, a reordering-threshold loss is a packet the peer's
	// ACK pattern declared missing.
	TracerLostTimeThreshold uint64 `json:"tracer_lost_time_threshold"`
	TracerLostReordering    uint64 `json:"tracer_lost_reordering"`
	// SpuriousLost is the subset of declared-lost packets that was later
	// acknowledged (reordering, not real loss).
	SpuriousLost uint64 `json:"spurious_lost"`
	// PTOCount is the number of probe-timeout expirations (loss detected by
	// timer rather than by an ACK threshold).
	PTOCount uint64 `json:"pto_count"`
	// LossTimerCount is the number of ACK-threshold loss-timer expirations.
	LossTimerCount uint64 `json:"loss_timer_count"`
	// ECNCongested is the number of times the ECN state machine reached
	// "capable" on this path. ECN congestion is a cwnd cutback WITHOUT a
	// packet loss, so it is counted separately from PacketsLost.
	ECNCongested uint64 `json:"ecn_congested"`
	// PacketsSent / PacketsReceived are quic-go's own packet counters.
	PacketsSent     uint64 `json:"packets_sent"`
	PacketsReceived uint64 `json:"packets_received"`
	BytesSent       uint64 `json:"bytes_sent"`
	BytesReceived   uint64 `json:"bytes_received"`
	// MinRTT / SmoothedRTT are quic-go's RTT estimates at the end of the run.
	MinRTT      time.Duration `json:"min_rtt_ns"`
	SmoothedRTT time.Duration `json:"smoothed_rtt_ns"`
	// CwndMin / CwndMax are the smallest and largest congestion window
	// quic-go reported during the run (bytes). CwndSamples is the number of
	// observations.
	CwndMin  uint64 `json:"cwnd_min"`
	CwndMax  uint64 `json:"cwnd_max"`
	CwndMean uint64 `json:"cwnd_mean"`
	CwndLast uint64 `json:"cwnd_last"`
	// CwndSamples is the number of congestion-window observations.
	CwndSamples uint64 `json:"cwnd_samples"`
	// InFlightMean / InFlightMax are the bytes in flight (the sender's own
	// outstanding, unacknowledged data). InFlightMean / CwndMean is the
	// "is the pipe full?" ratio: at 1.0 the flow is congestion-window
	// limited; well below 1.0 the sender is blocked by something else
	// (flow control, the application, the peer's read rate) and a bigger
	// congestion window would not help.
	InFlightMean uint64 `json:"in_flight_mean"`
	InFlightMax  uint64 `json:"in_flight_max"`
	// PeerStreamWindow / PeerConnWindow are the flow-control windows the
	// PEER advertised for our data. They limit what this side may send,
	// independently of the congestion window.
	PeerStreamWindow uint64 `json:"peer_stream_window"`
	PeerConnWindow   uint64 `json:"peer_conn_window"`
}

const (
	benchFrameMax = 64 << 10
	benchTagData  = byte(1)
	benchTagEnd   = byte(2)
	benchTagStats = byte(3)
	benchMaxSize  = int64(8) << 30
)

type benchRequest struct {
	Size               int64 `json:"size"`
	RateBytesPerSecond int64 `json:"rate_bytes_per_second"`
}

type benchWireStats struct {
	SentBytes     int64 `json:"sent_bytes"`
	ReceivedBytes int64 `json:"received_bytes"`
	// Loss carries the answering side's per-connection loss picture, so the
	// dialer can report BOTH directions' loss, not just its own.
	//
	// omitempty keeps the wire format backward compatible: an older peer
	// ignores the field, and a newer peer reading an older frame gets nil
	// (meaning "not measured") rather than a fabricated zero.
	Loss *BenchLoss `json:"loss,omitempty"`
}

// lossReporter is the OPTIONAL capability a session may have: it can report
// the QUIC-level loss accounting of the connection it runs on.
//
// It is an optional interface rather than a Session method because a
// Session is a byte stream and loss accounting is a transport concern. A
// relayed session has no QUIC connection of its own and simply does not
// implement it — the bench then reports "no loss data" instead of a
// fabricated zero.
type lossReporter interface {
	Loss() *BenchLoss
}

// lossOf builds the BenchLoss picture for one QUIC connection, or nil when
// the connection cannot be instrumented.
//
// Two independent sources are combined:
//
//   - quic-go's own ConnectionStats() (per-connection PacketsLost etc.),
//     which the congestion controller maintains; and
//   - the tracer counters installed by tracingConfig.
//
// The two count the same events through different code paths. They differ
// only for Path-MTU probe losses (tracer counts them, the congestion
// controller does not) and for ECN congestion events (the congestion
// controller counts them, the tracer's LostPacket hook does not).
func lossOf(conn *quic.Conn) *BenchLoss {
	if conn == nil {
		return nil
	}
	tracker := trackerForConn(conn)
	if tracker == nil {
		return nil
	}
	s := tracker.read(conn)
	return &BenchLoss{
		PacketsLost:             s.PacketsLost,
		BytesLost:               s.BytesLost,
		TracerLost:              s.TracerLost,
		TracerLostTimeThreshold: s.TracerLostTimeThreshold,
		TracerLostReordering:    s.TracerLostReordering,
		SpuriousLost:            s.Spurious,
		PTOCount:                s.PTO,
		LossTimerCount:          s.LossTimer,
		ECNCongested:            s.ECNCongested,
		PacketsSent:             s.PacketsSent,
		PacketsReceived:         s.PacketsReceived,
		BytesSent:               s.BytesSent,
		BytesReceived:           s.BytesReceived,
		MinRTT:                  s.MinRTT,
		SmoothedRTT:             s.SmoothedRTT,
		CwndMin:                 s.CwndMin,
		CwndMax:                 s.CwndMax,
		CwndMean:                cwndMean(s),
		CwndLast:                s.CwndLast,
		CwndSamples:             s.CwndSamples,
		InFlightMean:            s.InFlightMean,
		InFlightMax:             s.InFlightMax,
		PeerStreamWindow:        s.PeerStreamWindow,
		PeerConnWindow:          s.PeerConnWindow,
	}
}

// cwndMean computes the mean observed congestion window.
//
// The mean is the number that matters for a cwnd-limited flow: the classic
// bound is throughput <= cwnd / RTT, and the MEAN cwnd is what the flow
// actually operated at across the run. The min is the worst single
// observation, the max the best one.
func cwndMean(s lossSnapshot) uint64 {
	if s.CwndSamples == 0 {
		return 0
	}
	return s.CwndSum / s.CwndSamples
}

// RunBench opens a separate authenticated direct-link stream and moves Size
// bytes in both directions concurrently. It deliberately does not use the
// application's file/message streams, so the benchmark has no business-level
// side effects.
func RunBench(ctx context.Context, cfg DialConfig, bench BenchConfig) (BenchStats, error) {
	if err := validateBenchConfig(bench); err != nil {
		return BenchStats{}, err
	}
	cfg.Kind = KindBench
	link, _, err := dialViaPicker(ctx, cfg)
	if err != nil {
		return BenchStats{}, err
	}
	defer link.Close()

	req, err := json.Marshal(benchRequest{
		Size:               bench.Size,
		RateBytesPerSecond: bench.RateBytesPerSecond,
	})
	if err != nil {
		return BenchStats{}, err
	}
	if _, err := fmt.Fprintf(link.Session(), "%s\n", req); err != nil {
		return BenchStats{}, fmt.Errorf("mesh: send bench request: %w", err)
	}
	_ = link.Session().SetReadDeadline(time.Now().Add(10 * time.Second))
	ack, err := readLine(link.br, MaxHelloBytes)
	if err != nil {
		return BenchStats{}, fmt.Errorf("mesh: read bench ack: %w", err)
	}
	if string(ack) != "ok" {
		return BenchStats{}, fmt.Errorf("mesh: bench refused: %s", ack)
	}
	_ = link.Session().SetReadDeadline(time.Time{})

	stats, err := runBenchStream(ctx, link.Session(), link.br, bench)
	if err != nil {
		return BenchStats{}, err
	}
	// The loss picture is read AFTER the transfer, so it covers every
	// retransmission the transfer needed.
	//
	// ConnectionStats() is readable at any moment — it is a set of atomic
	// counters (quic-go/internal/utils/connstats.go), not a value frozen at
	// connection close — so no close/wait dance is needed here.
	stats.Loss = link.Loss()
	if stats.Loss != nil {
		lost := stats.Loss.PacketsLost
		stats.PacketsLost = &lost
	}
	return stats, nil
}

func validateBenchConfig(cfg BenchConfig) error {
	if cfg.Size <= 0 || cfg.Size > benchMaxSize {
		return fmt.Errorf("mesh: bench size must be between 1 B and %d GiB", benchMaxSize/(1<<30))
	}
	if cfg.RateBytesPerSecond < 0 {
		return errors.New("mesh: bench rate cannot be negative")
	}
	return nil
}

func receiveBench(br *bufio.Reader, st Session) error {
	line, err := readLine(br, MaxHelloBytes)
	if err != nil {
		return fmt.Errorf("mesh: bench request: %w", err)
	}
	var req benchRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return errors.New("mesh: bad bench request")
	}
	cfg := BenchConfig{Size: req.Size, RateBytesPerSecond: req.RateBytesPerSecond}
	if err := validateBenchConfig(cfg); err != nil {
		fmt.Fprintf(st, "err: %s\n", err)
		return err
	}
	if _, err := fmt.Fprintln(st, "ok"); err != nil {
		return err
	}
	_, err = runBenchStream(context.Background(), st, br, cfg)
	return err
}

// runBenchStream has one writer goroutine and one reader goroutine by role.
// Each endpoint writes only its own direction, so the bidirectional QUIC
// stream cannot interleave frames within a direction.
func runBenchStream(ctx context.Context, st Session, br *bufio.Reader, cfg BenchConfig) (BenchStats, error) {
	started := time.Now()
	sendDone := make(chan error, 1)
	go func() { sendDone <- sendBenchData(ctx, st, cfg) }()

	var received int64
	var remote *benchWireStats
	peerEnded := false
	resultSent := false
	reader := &benchFrameReader{}
	for remote == nil {
		if err := ctx.Err(); err != nil {
			return BenchStats{}, err
		}
		_ = st.SetReadDeadline(time.Now().Add(time.Second))
		body, err := reader.next(br)
		if err != nil {
			if ne, ok := err.(netError); ok && ne.Timeout() {
				continue
			}
			return BenchStats{}, fmt.Errorf("mesh: read bench data: %w", err)
		}
		if len(body) == 0 {
			return BenchStats{}, errors.New("mesh: empty bench frame")
		}
		switch body[0] {
		case benchTagData:
			n := int64(len(body) - 1)
			if n == 0 || n > benchFrameMax || received > cfg.Size-n {
				return BenchStats{}, errors.New("mesh: invalid bench data length")
			}
			received += n
		case benchTagEnd:
			if peerEnded {
				return BenchStats{}, errors.New("mesh: duplicate bench end")
			}
			peerEnded = true
		case benchTagStats:
			if !peerEnded || !resultSent {
				return BenchStats{}, errors.New("mesh: bench stats arrived before end")
			}
			var wire benchWireStats
			if err := json.Unmarshal(body[1:], &wire); err != nil {
				return BenchStats{}, errors.New("mesh: bad bench stats")
			}
			if wire.SentBytes != cfg.Size || wire.ReceivedBytes != cfg.Size {
				return BenchStats{}, fmt.Errorf("mesh: peer reported sent=%d received=%d, want %d", wire.SentBytes, wire.ReceivedBytes, cfg.Size)
			}
			remote = &wire
		default:
			return BenchStats{}, fmt.Errorf("mesh: unknown bench frame tag %d", body[0])
		}

		if peerEnded && !resultSent {
			if err := <-sendDone; err != nil {
				return BenchStats{}, fmt.Errorf("mesh: send bench data: %w", err)
			}
			// Report OUR OWN loss picture alongside the byte counts, so the
			// dialer can see BOTH directions' loss from one number set.
			//
			// The snapshot is taken right here: our own send direction is
			// finished (sendDone was awaited above), so every packet we
			// sent has either been acked or declared lost by now. The
			// dialer reads its OWN side again after the whole exchange, so
			// the dialer's number is the more complete one.
			var localLoss *BenchLoss
			if lr, ok := st.(lossReporter); ok {
				localLoss = lr.Loss()
			}
			wire, err := json.Marshal(benchWireStats{
				SentBytes:     cfg.Size,
				ReceivedBytes: received,
				Loss:          localLoss,
			})
			if err != nil {
				return BenchStats{}, err
			}
			writer := &benchFrameWriter{}
			if err := writer.writeData(st, benchTagStats, wire); err != nil {
				return BenchStats{}, fmt.Errorf("mesh: send bench stats: %w", err)
			}
			resultSent = true
		}
	}
	_ = remote
	return BenchStats{
		SentBytes:     cfg.Size,
		ReceivedBytes: received,
		Duration:      time.Since(started),
		PacketsLost:   nil,
		// PeerLoss is the ANSWERING side's own loss picture, carried in the
		// bench stats frame. It is nil when the peer did not report one
		// (an older build, or a session with no QUIC connection).
		PeerLoss: remote.Loss,
	}, nil
}

// netError is the small interface needed to recognize a deadline without
// importing the concrete net package into the protocol code.
type netError interface {
	error
	Timeout() bool
}

// benchFrameWriter serializes length-prefixed bench frames through one
// reusable buffer. A large transfer used to allocate a fresh frame buffer per
// 64 KiB on the send path, which dominated the allocation profile; the writer
// keeps one buffer for the whole stream.
//
// The buffer is owned by a single writer goroutine. runBenchStream starts
// exactly one sender per stream, so no synchronisation is needed.
type benchFrameWriter struct {
	buf []byte
}

// writeData emits one data frame carrying a tag byte followed by payload.
func (w *benchFrameWriter) writeData(st io.Writer, tag byte, payload []byte) error {
	bodyLen := 1 + len(payload)
	if bodyLen > benchFrameMax+1 {
		return errors.New("mesh: bench frame too large")
	}
	need := 4 + bodyLen
	if cap(w.buf) < need {
		w.buf = make([]byte, need)
	}
	frame := w.buf[:need]
	binary.BigEndian.PutUint32(frame[:4], uint32(bodyLen))
	frame[4] = tag
	copy(frame[5:], payload)
	return writeFull(st, frame)
}

// writeControl emits one control frame whose body is a single tag byte.
func (w *benchFrameWriter) writeControl(st io.Writer, tag byte) error {
	bodyLen := 1
	need := 4 + bodyLen
	if cap(w.buf) < need {
		w.buf = make([]byte, need)
	}
	frame := w.buf[:need]
	binary.BigEndian.PutUint32(frame[:4], uint32(bodyLen))
	frame[4] = tag
	return writeFull(st, frame)
}

// benchFrameReader reads length-prefixed bench frames into one reusable
// buffer. The returned slice is only valid until the next read, so callers
// must consume it before reading again.
type benchFrameReader struct {
	buf []byte
}

// next reads one frame body. The returned slice aliases the reader's buffer
// and is invalidated by the following call.
func (r *benchFrameReader) next(br *bufio.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(br, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > benchFrameMax+1 {
		return nil, errors.New("mesh: invalid bench frame size")
	}
	if cap(r.buf) < int(n) {
		r.buf = make([]byte, n)
	}
	body := r.buf[:n]
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return body, nil
}

func sendBenchData(ctx context.Context, st Session, cfg BenchConfig) error {
	payload := make([]byte, benchFrameMax)
	writer := &benchFrameWriter{}
	var sent int64
	started := time.Now()
	for sent < cfg.Size {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int64(len(payload))
		if left := cfg.Size - sent; left < n {
			n = left
		}
		if cfg.RateBytesPerSecond > 0 {
			want := started.Add(time.Duration(float64(sent) / float64(cfg.RateBytesPerSecond) * float64(time.Second)))
			if err := waitBenchUntil(ctx, want); err != nil {
				return err
			}
		}
		if err := writer.writeData(st, benchTagData, payload[:n]); err != nil {
			return err
		}
		sent += n
	}
	return writer.writeControl(st, benchTagEnd)
}

func waitBenchUntil(ctx context.Context, when time.Time) error {
	for {
		d := time.Until(when)
		if d <= 0 {
			return nil
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
