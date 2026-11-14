// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/mesh"
)

const defaultBenchSize int64 = 100_000_000

func runBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("device-passphrase", "", "passphrase for the device file (for scripting)")
	sizeText := fs.String("size", "100MB", "bytes per direction; accepts B, KB, MB, GB suffixes")
	rateText := fs.String("rate", "0", "bytes per second per direction; 0 = unlimited")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall budget including discovery and transfer")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}
	if fs.NArg() != 1 {
		return ExitError{Code: 2, Err: errors.New("usage: cmesh bench [flags] <deviceID> (the device ID goes last)")}
	}
	target := identity.DeviceID(fs.Arg(0))
	if !target.Valid() {
		return ExitError{Code: 2, Err: fmt.Errorf("%q is not a valid device ID", fs.Arg(0))}
	}
	size, err := parseBenchBytes(*sizeText, defaultBenchSize)
	if err != nil {
		return ExitError{Code: 2, Err: fmt.Errorf("--size: %w", err)}
	}
	rate, err := parseBenchBytes(*rateText, 0)
	if err != nil {
		return ExitError{Code: 2, Err: fmt.Errorf("--rate: %w", err)}
	}
	if rate < 0 {
		return ExitError{Code: 2, Err: errors.New("--rate cannot be negative")}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	hctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	sess, err := openMeshSession(hctx, *state, *pass, nil)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	eps, err := sess.resolveTarget(hctx, target)
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	started := time.Now()
	stats, err := mesh.RunBench(hctx, mesh.DialConfig{
		Device:    sess.dev,
		Resolver:  sess.resolver,
		Target:    target,
		Endpoints: eps,
	}, mesh.BenchConfig{Size: size, RateBytesPerSecond: rate})
	if err != nil {
		return ExitError{Code: 1, Err: err}
	}
	if stats.Duration <= 0 {
		stats.Duration = time.Since(started)
	}
	fmt.Printf("peer=%s\n", target)
	fmt.Printf("direction=both size_each=%d sent=%d received=%d\n", size, stats.SentBytes, stats.ReceivedBytes)
	fmt.Printf("duration=%s throughput_each=%s retransmissions=n/a\n", stats.Duration.Round(time.Millisecond), rateString(size, stats.Duration))
	fmt.Println("note=QUIC is reliable, but this build does not expose a per-stream retransmission counter; n/a is not zero")
	return nil
}

func rateString(bytes int64, d time.Duration) string {
	if d <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f MiB/s", float64(bytes)/(1<<20)/d.Seconds())
}

func parseBenchBytes(text string, def int64) (int64, error) {
	original := strings.TrimSpace(strings.ToUpper(text))
	text = original
	if text == "" {
		return def, nil
	}
	multiplier := int64(1)
	matchedSuffix := false
	for _, suffix := range []struct {
		name string
		mul  int64
	}{
		{"GIB", 1 << 30}, {"GB", 1_000_000_000}, {"MIB", 1 << 20}, {"MB", 1_000_000},
		{"KIB", 1 << 10}, {"KB", 1_000}, {"B", 1},
	} {
		if strings.HasSuffix(text, suffix.name) {
			text = strings.TrimSpace(strings.TrimSuffix(text, suffix.name))
			multiplier = suffix.mul
			matchedSuffix = true
			break
		}
	}
	if !matchedSuffix && strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, fmt.Errorf("%q has an unsupported suffix; use B, KB, MB, GB, KiB, MiB, or GiB", original)
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("use a non-negative integer with optional B, KB, MB, or GB suffix")
	}
	if value > (1<<62)/multiplier {
		return 0, errors.New("value is too large")
	}
	return value * multiplier, nil
}
