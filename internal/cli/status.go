// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// statusMember is the stable, read-only status contract. It deliberately
// mirrors the facts the discovery snapshot carries; runtime metrics belong to
// doctor or a future telemetry API and must not be invented here.
const (
	reachabilityDirect    = "direct"
	reachabilityRelayOnly = "relay-only"
	reachabilityUnknown   = "unknown"
	reachabilityDegraded  = "degraded"

	defaultStatusSnapshotTimeout = 800 * time.Millisecond
)

// statusMember is the stable, read-only status contract. Reachability is
// deliberately conservative: discovery proves membership, liveness, and
// advertised candidates, but it does not prove that a peer can be dialled or
// that a relay data path is available. Those cases remain unknown until a
// transport probe supplies evidence.
type statusMember struct {
	DeviceID     identity.DeviceID `json:"device_id"`
	Online       bool              `json:"online"`
	Reachability string            `json:"reachability"`
	Endpoints    []string          `json:"endpoints"`
	LastSeen     string            `json:"last_seen,omitempty"`
}

type statusReport struct {
	AccountID     identity.DeviceID `json:"account_id"`
	Version       uint64            `json:"version"`
	GeneratedAt   time.Time         `json:"generated_at"`
	Members       []statusMember    `json:"members"`
	Online        int               `json:"online"`
	CurrentDevice identity.DeviceID `json:"current_device_id"`
}

// runStatus prints the last verified membership view. It is intentionally a
// snapshot command: it does not announce, mutate the roster, or create a
// second liveness writer. That keeps `mesh ping` and status observational.
func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "state directory (default ~/.cmesh)")
	pass := fs.String("device-passphrase", "", "passphrase for the device file (for scripting)")
	asJSON := fs.Bool("json", false, "emit structured JSON")
	timeout := fs.Duration("timeout", defaultStatusSnapshotTimeout, "budget for the verified discovery snapshot")
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	sess, err := openMeshSessionWithSnapshotTimeout(ctx, *state, *pass, nil, *timeout)
	if err != nil {
		return ExitError{Code: 1, Err: fmt.Errorf("status: %w", err)}
	}
	snap := sess.cl.Snapshot()
	if snap == nil {
		return ExitError{Code: 1, Err: errors.New("status: discovery returned no verified member view")}
	}

	ids := make([]identity.DeviceID, 0, len(snap.Members))
	for id := range snap.Members {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	report := statusReport{
		AccountID:     snap.AccountID,
		Version:       snap.Version,
		GeneratedAt:   snapVersionTime(snap),
		CurrentDevice: sess.dev.ID(),
		Members:       make([]statusMember, 0, len(ids)),
	}
	for _, id := range ids {
		mv := snap.Members[id]
		row := statusMember{
			DeviceID:     id,
			Online:       mv.Online,
			Reachability: statusReachability(mv),
			Endpoints:    append([]string(nil), mv.Endpoints...),
		}
		if !mv.LastSeen.IsZero() {
			row.LastSeen = mv.LastSeen.UTC().Format(time.RFC3339)
		}
		if mv.Online {
			report.Online++
		}
		report.Members = append(report.Members, row)
	}
	if *asJSON {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return ExitError{Code: 1, Err: err}
		}
		fmt.Fprintln(os.Stdout, string(b))
		return nil
	}
	fmt.Fprintf(os.Stdout, "account: %s  view: v%d  online: %d/%d\n", report.AccountID, report.Version, report.Online, len(report.Members))
	for _, row := range report.Members {
		state := "offline"
		if row.Online {
			state = "online"
		}
		extra := ""
		if len(row.Endpoints) > 0 {
			extra = " reachability=" + row.Reachability + " endpoints=" + strings.Join(row.Endpoints, ",")
		} else {
			extra = " reachability=" + row.Reachability
		}
		if row.LastSeen != "" {
			extra += " last_seen=" + row.LastSeen
		}
		fmt.Fprintf(os.Stdout, "  %-16s %-7s%s\n", row.DeviceID, state, extra)
	}
	return nil
}

// statusReachability reports only what the current discovery snapshot proves.
// An advertised endpoint is a candidate, not a successful direct path, and an
// online member does not imply that this process has a relay data session to
// it. Keeping these cases unknown avoids turning membership facts into
// invented transport telemetry.
func statusReachability(_ discovery.MemberView) string {
	return reachabilityUnknown
}

// MemberSnapshot does not carry a generation timestamp. Keep status honest by
// using the command observation time instead of pretending the discovery node
// supplied one.
func snapVersionTime(_ *discovery.MemberSnapshot) time.Time { return time.Now().UTC() }

// runDoctor is the operational capability report. probe owns its JSON schema
// and exit-code contract, so doctor delegates instead of creating a second,
// drifting implementation.
func runDoctor(args []string) error { return runProbe(args) }
