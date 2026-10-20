// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package p2p2

import (
	"crypto/rand"
	"crypto/sha256"
	"sort"
	"testing"
	"time"
)

// KPI harness for the batched receive path (KPI.md rounds 15-16).
//
// Two clocks are reported for every configuration:
//
//   - CPU µs per transfer, from processCPU. This is the primary metric: it is
//     the work the change is responsible for removing, and it does not move
//     when the machine is busy.
//   - best-of-N wall-clock MiB/s, which is the number the KPI table states and
//     the number a user experiences. It is noisy on a loaded machine, so it is
//     reported as best-of-N and only trusted in an interleaved A/B.
//
// Every sweep interleaves its arms in one loop and rotates the order per round,
// so no arm is systematically first and the ratios are measured under the same
// conditions.

// measureTransfer runs one transfer and returns its CPU time and wall time.
func measureTransfer(tb testing.TB, size int, drop float64, cfg Config) (cpu, wall time.Duration) {
	tb.Helper()
	a, b := pairCfg(tb, cfg, drop)
	msg := make([]byte, size)
	if _, err := rand.Read(msg); err != nil {
		tb.Fatal(err)
	}
	want := sha256.Sum256(msg)

	done := make(chan []byte, 1)
	go func() {
		m, err := b.Recv()
		if err == nil {
			done <- m
		}
		close(done)
	}()

	c0 := processCPU()
	w0 := time.Now()
	if err := a.Send(msg); err != nil {
		tb.Fatalf("send: %v", err)
	}
	select {
	case got := <-done:
		if sha256.Sum256(got) != want {
			tb.Fatal("payload corrupted")
		}
	case <-time.After(180 * time.Second):
		tb.Fatal("message never arrived")
	}
	wall = time.Since(w0)
	cpu = processCPU() - c0

	_ = a.Close()
	_ = b.Close()
	return cpu, wall
}

// kpiArm is one configuration in a sweep.
type kpiArm struct {
	name string
	cfg  func(*Config)
}

// kpiResult holds one arm's samples.
type kpiResult struct {
	name string
	cpu  []time.Duration
	wall []time.Duration
}

// runKPIArms interleaves the arms for rounds rounds, rotating the order.
func runKPIArms(tb testing.TB, size int, drop float64, arms []kpiArm, rounds int) []kpiResult {
	tb.Helper()
	res := make([]kpiResult, len(arms))
	for i, a := range arms {
		res[i] = kpiResult{name: a.name}
	}
	for r := 0; r < rounds; r++ {
		for i := range arms {
			idx := (i + r) % len(arms)
			cfg := DefaultConfig()
			arms[idx].cfg(&cfg)
			cpu, wall := measureTransfer(tb, size, drop, cfg)
			res[idx].cpu = append(res[idx].cpu, cpu)
			res[idx].wall = append(res[idx].wall, wall)
		}
	}
	return res
}

func medDur(v []time.Duration) time.Duration {
	s := append([]time.Duration(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func minDur(v []time.Duration) time.Duration {
	m := v[0]
	for _, x := range v[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

func (r kpiResult) report(tb testing.TB, size int) {
	tb.Helper()
	best := minDur(r.wall)
	tb.Logf("%-24s cpu %6.0f us | wall best %6.2f ms (%6.1f MiB/s)",
		r.name, float64(medDur(r.cpu).Microseconds()),
		float64(best.Microseconds())/1000, float64(size)/best.Seconds()/(1<<20))
}

func kpiItoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// kpiSkipReason returns a reason to skip a throughput measurement, or "".
func kpiSkipReason(tb testing.TB) string {
	tb.Helper()
	if testing.Short() {
		return "KPI sweep skipped in -short mode"
	}
	if raceEnabled {
		return "CPU-time and throughput are meaningless under the race detector"
	}
	if !cpuTimingSupported {
		return "no process CPU clock on this platform"
	}
	return ""
}

// TestKPIReceiveBatchAttribution is the round's primary measurement: how much
// CPU does a 512 KiB transfer cost with each combination of send and receive
// batching, at each loss rate.
//
// The four arms are what separates the two changes. "recv batch only" is the
// round-15 change measured on its own; "both batched" is what ships. The
// difference between them and "both off" is the claim.
func TestKPIReceiveBatchAttribution(t *testing.T) {
	if r := kpiSkipReason(t); r != "" {
		t.Skip(r)
	}
	const size = 512 << 10
	const rounds = 15

	for _, drop := range []float64{0, 0.05, 0.20} {
		arms := []kpiArm{
			{"both off", func(c *Config) { c.DisableBatching = true; c.DisableRecvBatching = true }},
			{"send batch only", func(c *Config) { c.DisableRecvBatching = true }},
			{"recv batch only", func(c *Config) { c.DisableBatching = true }},
			{"both batched", func(c *Config) {}},
		}
		res := runKPIArms(t, size, drop, arms, rounds)
		t.Logf("--- 512 KiB @ %.0f%% loss ---", drop*100)
		for _, r := range res {
			r.report(t, size)
		}
		base := medDur(res[0].cpu)
		if base > 0 {
			for _, r := range res[1:] {
				t.Logf("    %-20s cpu %.2fx of both-off", r.name,
					float64(medDur(r.cpu))/float64(base))
			}
		}
	}
}

// TestKPIReceiveBatchDepthSweep measures the receive depth curve. It is the
// evidence for the default of 32 rather than a copy of the send side's default.
func TestKPIReceiveBatchDepthSweep(t *testing.T) {
	if r := kpiSkipReason(t); r != "" {
		t.Skip(r)
	}
	const size = 512 << 10
	values := []int{1, 4, 8, 16, 32, 64}
	arms := make([]kpiArm, len(values))
	for i, v := range values {
		v := v
		arms[i] = kpiArm{"recv batch " + kpiItoa(v), func(c *Config) { c.RecvBatchSize = v }}
	}
	for _, r := range runKPIArms(t, size, 0, arms, 15) {
		r.report(t, size)
	}
}

// TestKPIReceiveBatchOrdering is the ordering KPI with receive batching on: 20
// messages at each loss rate must arrive byte-exact and in order.
func TestKPIReceiveBatchOrdering(t *testing.T) {
	if testing.Short() {
		t.Skip("KPI sweep skipped in -short mode")
	}
	const count = 20
	const size = 32 << 10

	for _, drop := range []float64{0, 0.05, 0.10, 0.20} {
		cfg := DefaultConfig()
		a, b := pairCfg(t, cfg, drop)

		msgs := make([][]byte, count)
		wants := make([][32]byte, count)
		for i := range msgs {
			msgs[i] = make([]byte, size)
			if _, err := rand.Read(msgs[i]); err != nil {
				t.Fatal(err)
			}
			copy(msgs[i], "msg-"+kpiItoa(i))
			wants[i] = sha256.Sum256(msgs[i])
		}

		sendErr := make(chan error, 1)
		go func() {
			for i := 0; i < count; i++ {
				if err := a.Send(msgs[i]); err != nil {
					sendErr <- err
					return
				}
			}
			sendErr <- nil
		}()

		got := recvN(t, b, count, 120*time.Second)
		if err := <-sendErr; err != nil {
			t.Fatalf("drop=%.2f: %v", drop, err)
		}
		for i := range got {
			if sha256.Sum256(got[i]) != wants[i] {
				t.Fatalf("drop=%.2f: message %d differs (order or content)", drop, i)
			}
		}
		st := b.Stats()
		mean := 0.0
		if st.RecvBatchCalls > 0 {
			mean = float64(st.RecvBatchPackets) / float64(st.RecvBatchCalls)
		}
		t.Logf("drop=%2.0f%%: %d/%d in order, %d datagrams in %d recv calls (mean %.2f)",
			drop*100, count, count, st.RecvBatchPackets, st.RecvBatchCalls, mean)
		_ = a.Close()
		_ = b.Close()
	}
}
