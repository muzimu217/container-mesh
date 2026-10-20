// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"
)

// Benchmarks for the receive-batching KPI. These are the rigorous form of the
// attribution test: `go test -bench` reports ns/op over many iterations, which
// averages out the per-transfer noise that makes a 15-round test unreliable on a
// busy machine.
//
// The four arms are the same as the attribution test: send batching and receive
// batching, each on or off. b.ReportMetric also reports CPU microseconds per
// transfer, so the work removed by batching is visible separately from the wall
// clock.

func benchTransferCfg(b *testing.B, size int, drop float64, mutate func(*Config)) {
	b.Helper()
	b.SetBytes(int64(size))
	b.ReportAllocs()

	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}

	msg := make([]byte, size)
	if _, err := rand.Read(msg); err != nil {
		b.Fatal(err)
	}
	want := sha256.Sum256(msg)

	b.ResetTimer()
	var cpuTotal time.Duration
	for i := 0; i < b.N; i++ {
		a, bb := pairCfg(b, cfg, drop)

		done := make(chan []byte, 1)
		go func() {
			m, err := bb.Recv()
			if err == nil {
				done <- m
			}
			close(done)
		}()

		c0 := processCPU()
		if err := a.Send(msg); err != nil {
			b.Fatalf("send: %v", err)
		}
		select {
		case got := <-done:
			if sha256.Sum256(got) != want {
				b.Fatal("payload corrupted")
			}
		case <-time.After(180 * time.Second):
			b.Fatal("message never arrived")
		}
		cpuTotal += processCPU() - c0

		_ = a.Close()
		_ = bb.Close()
	}
	b.StopTimer()
	if b.N > 0 {
		b.ReportMetric(float64(cpuTotal.Microseconds())/float64(b.N), "cpu-us/op")
	}
}

// BenchmarkTransferBatching is the four-arm comparison at 0% loss.
func BenchmarkTransferBatching(b *testing.B) {
	arms := []struct {
		name string
		cfg  func(*Config)
	}{
		{"both_off", func(c *Config) { c.DisableBatching = true; c.DisableRecvBatching = true }},
		{"send_only", func(c *Config) { c.DisableRecvBatching = true }},
		{"recv_only", func(c *Config) { c.DisableBatching = true }},
		{"both_on", nil},
	}
	for _, arm := range arms {
		arm := arm
		b.Run(arm.name, func(b *testing.B) { benchTransferCfg(b, 512<<10, 0, arm.cfg) })
	}
}

// BenchmarkTransferBatchingLoss5 is the same comparison at 5% loss, where the
// transfer is bounded by retransmission round trips rather than by syscalls.
func BenchmarkTransferBatchingLoss5(b *testing.B) {
	arms := []struct {
		name string
		cfg  func(*Config)
	}{
		{"both_off", func(c *Config) { c.DisableBatching = true; c.DisableRecvBatching = true }},
		{"send_only", func(c *Config) { c.DisableRecvBatching = true }},
		{"recv_only", func(c *Config) { c.DisableBatching = true }},
		{"both_on", nil},
	}
	for _, arm := range arms {
		arm := arm
		b.Run(arm.name, func(b *testing.B) { benchTransferCfg(b, 512<<10, 0.05, arm.cfg) })
	}
}

// BenchmarkTransferBatchingLoss20 is the same comparison at 20% loss.
func BenchmarkTransferBatchingLoss20(b *testing.B) {
	arms := []struct {
		name string
		cfg  func(*Config)
	}{
		{"both_off", func(c *Config) { c.DisableBatching = true; c.DisableRecvBatching = true }},
		{"send_only", func(c *Config) { c.DisableRecvBatching = true }},
		{"recv_only", func(c *Config) { c.DisableBatching = true }},
		{"both_on", nil},
	}
	for _, arm := range arms {
		arm := arm
		b.Run(arm.name, func(b *testing.B) { benchTransferCfg(b, 512<<10, 0.20, arm.cfg) })
	}
}

// BenchmarkTransferRecvDepth is the receive-depth curve at 0% loss.
func BenchmarkTransferRecvDepth(b *testing.B) {
	for _, depth := range []int{1, 4, 8, 16, 32, 64} {
		depth := depth
		b.Run("depth"+kpiItoa(depth), func(b *testing.B) {
			benchTransferCfg(b, 512<<10, 0, func(c *Config) { c.RecvBatchSize = depth })
		})
	}
}

// BenchmarkTransferSendDepth is the send-depth curve at 0% loss, for comparison
// with the receive curve: the two knobs are separate and their defaults should
// be justified separately.
func BenchmarkTransferSendDepth(b *testing.B) {
	for _, depth := range []int{8, 16, 32, 64, 128} {
		depth := depth
		b.Run("depth"+kpiItoa(depth), func(b *testing.B) {
			benchTransferCfg(b, 512<<10, 0, func(c *Config) { c.BatchSize = depth })
		})
	}
}
