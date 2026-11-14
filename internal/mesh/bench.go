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
// QUIC retransmits below this stream, but quic-go does not expose a stable
// per-stream retransmission counter, so Retransmissions is nil by design.
type BenchStats struct {
	SentBytes       int64
	ReceivedBytes   int64
	Duration        time.Duration
	Retransmissions *uint64
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
	link, err := DialDirect(ctx, cfg)
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
	if _, err := fmt.Fprintf(link.Stream(), "%s\n", req); err != nil {
		return BenchStats{}, fmt.Errorf("mesh: send bench request: %w", err)
	}
	_ = link.Stream().SetReadDeadline(time.Now().Add(10 * time.Second))
	ack, err := readLine(link.br, MaxHelloBytes)
	if err != nil {
		return BenchStats{}, fmt.Errorf("mesh: read bench ack: %w", err)
	}
	if string(ack) != "ok" {
		return BenchStats{}, fmt.Errorf("mesh: bench refused: %s", ack)
	}
	_ = link.Stream().SetReadDeadline(time.Time{})

	return runBenchStream(ctx, link.Stream(), link.br, bench)
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

func receiveBench(br *bufio.Reader, st *quic.Stream) error {
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
func runBenchStream(ctx context.Context, st *quic.Stream, br *bufio.Reader, cfg BenchConfig) (BenchStats, error) {
	started := time.Now()
	sendDone := make(chan error, 1)
	go func() { sendDone <- sendBenchData(ctx, st, cfg) }()

	var received int64
	var remote *benchWireStats
	peerEnded := false
	resultSent := false
	for remote == nil {
		if err := ctx.Err(); err != nil {
			return BenchStats{}, err
		}
		_ = st.SetReadDeadline(time.Now().Add(time.Second))
		body, err := readBenchFrame(br)
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
			wire, err := json.Marshal(benchWireStats{SentBytes: cfg.Size, ReceivedBytes: received})
			if err != nil {
				return BenchStats{}, err
			}
			if err := writeBenchFrame(st, append([]byte{benchTagStats}, wire...)); err != nil {
				return BenchStats{}, fmt.Errorf("mesh: send bench stats: %w", err)
			}
			resultSent = true
		}
	}
	_ = remote
	return BenchStats{
		SentBytes:       cfg.Size,
		ReceivedBytes:   received,
		Duration:        time.Since(started),
		Retransmissions: nil,
	}, nil
}

// netError is the small interface needed to recognize a deadline without
// importing the concrete net package into the protocol code.
type netError interface {
	error
	Timeout() bool
}

func sendBenchData(ctx context.Context, st *quic.Stream, cfg BenchConfig) error {
	payload := make([]byte, benchFrameMax)
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
		if err := writeBenchFrame(st, append([]byte{benchTagData}, payload[:n]...)); err != nil {
			return err
		}
		sent += n
	}
	return writeBenchFrame(st, []byte{benchTagEnd})
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

func writeBenchFrame(w io.Writer, body []byte) error {
	if len(body) > benchFrameMax+1 {
		return errors.New("mesh: bench frame too large")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if err := writeFull(w, header[:]); err != nil {
		return err
	}
	return writeFull(w, body)
}

func readBenchFrame(br *bufio.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(br, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > benchFrameMax+1 {
		return nil, errors.New("mesh: invalid bench frame size")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return body, nil
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
