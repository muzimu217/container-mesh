// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package probe inspects what a container is actually allowed to do, with
// emphasis on whether a TUN device can be created.
//
// Every check is empirical where possible. Reading /proc/self/status tells you
// what the capability *bitset* says; it does not tell you whether the seccomp
// profile or the container runtime will let the ioctl through. So the TUN check
// actually opens /dev/net/tun and issues TUNSETIFF.
package probe

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

// Level summarises the outcome for the caller's purposes.
type Level string

const (
	// LevelTUN means a TUN device can be created: the process has both the
	// device node and CAP_NET_ADMIN, and the ioctl succeeded.
	LevelTUN Level = "tun_available"
	// LevelUserspace means TUN is unavailable but outbound networking works,
	// so a userspace transport (this project) is still viable.
	LevelUserspace Level = "userspace_only"
	// LevelUnusable means even outbound networking does not work.
	LevelUnusable Level = "unusable"
)

// Exit codes are distinct so shell scripts can branch on them.
const (
	ExitTUN       = 0
	ExitUserspace = 10
	ExitUnusable  = 20
	ExitError     = 30
)

// ExitCode maps a Level to its documented exit code.
func (l Level) ExitCode() int {
	switch l {
	case LevelTUN:
		return ExitTUN
	case LevelUserspace:
		return ExitUserspace
	default:
		return ExitUnusable
	}
}

// Report is the structured JSON output of a probe run.
type Report struct {
	Timestamp  time.Time `json:"timestamp"`
	Host       HostInfo  `json:"host"`
	TUN        TUNReport `json:"tun"`
	Capability CapReport `json:"capabilities"`
	Network    NetReport `json:"network"`
	Level      Level     `json:"level"`
	Summary    string    `json:"summary"`
	Notes      []string  `json:"notes,omitempty"`
}

// HostInfo is basic environment identification.
type HostInfo struct {
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	UID          int    `json:"uid"`
	EUID         int    `json:"euid"`
	PID          int    `json:"pid"`
	Kernel       string `json:"kernel,omitempty"`
	Container    string `json:"container,omitempty"`
	IsContainer  bool   `json:"is_container"`
	ContainerWhy string `json:"container_evidence,omitempty"`
}

// TUNReport describes TUN availability, checked three ways.
type TUNReport struct {
	DevicePath     string `json:"device_path"`
	DeviceExists   bool   `json:"device_exists"`
	DeviceMode     string `json:"device_mode,omitempty"`
	Openable       bool   `json:"openable"`
	OpenError      string `json:"open_error,omitempty"`
	IoctlAttempted bool   `json:"ioctl_attempted"`
	IoctlOK        bool   `json:"ioctl_ok"`
	IoctlError     string `json:"ioctl_error,omitempty"`
	IfaceName      string `json:"iface_name,omitempty"`
	// Supported is the bottom line: could we create a tun interface?
	Supported bool `json:"supported"`
}

// CapReport describes the capability state.
type CapReport struct {
	CapEffHex        string   `json:"cap_eff_hex"`
	CapEffDecimal    uint64   `json:"cap_eff_decimal"`
	CapBndHex        string   `json:"cap_bnd_hex,omitempty"`
	CapPrmHex        string   `json:"cap_prm_hex,omitempty"`
	NoNewPrivs       bool     `json:"no_new_privs"`
	HasCAPNetAdmin   bool     `json:"has_cap_net_admin"`
	HasCAPNetRaw     bool     `json:"has_cap_net_raw"`
	HasCAPSysAdmin   bool     `json:"has_cap_sys_admin"`
	HasCAPMknod      bool     `json:"has_cap_mknod"`
	MissingCritical  []string `json:"missing_critical,omitempty"`
	ProcStatusParsed bool     `json:"proc_status_parsed"`
	ParseError       string   `json:"parse_error,omitempty"`
}

// NetReport describes outbound connectivity.
type NetReport struct {
	UDPOutbound      CheckResult  `json:"udp_outbound"`
	TCPOutbound      CheckResult  `json:"tcp_outbound"`
	DNSResolution    CheckResult  `json:"dns_resolution"`
	LocalhostListen  CheckResult  `json:"localhost_listen"`
	PublicIPViaSTUN  string       `json:"public_ip_stun,omitempty"`
	STUNError        string       `json:"stun_error,omitempty"`
	STUN             []STUNResult `json:"stun,omitempty"`
	OutboundUsable   bool         `json:"outbound_usable"`
	EffectiveNATNote string       `json:"nat_note,omitempty"`
}

// CheckResult is a single pass/fail probe with diagnostics.
type CheckResult struct {
	OK      bool   `json:"ok"`
	Target  string `json:"target,omitempty"`
	Latency string `json:"latency,omitempty"`
	Error   string `json:"error,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Options tunes the probe.
type Options struct {
	// SkipNetwork disables outbound checks (useful in hermetic tests).
	SkipNetwork bool
	// NetworkTimeout bounds each outbound check.
	NetworkTimeout time.Duration
	// TUNDevice overrides the device path (tests point this at a temp file).
	TUNDevice string
	// UDPServer/TCPAddr override probe targets.
	UDPServer string
	TCPAddr   string
	// STUNServers overrides the STUN server list (empty = default list of
	// Chinese and international servers).
	STUNServers []string
	// IfaceName is the name requested in TUNSETIFF.
	IfaceName string
}

// DefaultOptions returns the standard probe configuration.
func DefaultOptions() Options {
	return Options{
		NetworkTimeout: 3 * time.Second,
		TUNDevice:      "/dev/net/tun",
		UDPServer:      "1.1.1.1:53",
		TCPAddr:        "1.1.1.1:443",
		IfaceName:      "cmesh-probe0",
	}
}

// Run performs the full probe and never returns an error: every failure is
// recorded in the report so the JSON is always usable.
func Run(opts Options) *Report {
	if opts.TUNDevice == "" {
		opts.TUNDevice = "/dev/net/tun"
	}
	if opts.NetworkTimeout == 0 {
		opts.NetworkTimeout = 3 * time.Second
	}
	if opts.IfaceName == "" {
		opts.IfaceName = "cmesh-probe0"
	}

	r := &Report{Timestamp: time.Now().UTC()}
	r.Host = probeHost()
	r.Capability = probeCaps()
	r.TUN = probeTUN(opts)
	if !opts.SkipNetwork {
		r.Network = probeNet(opts)
	} else {
		r.Network = NetReport{OutboundUsable: false, EffectiveNATNote: "network checks skipped"}
	}
	r.Level, r.Summary = classify(r)
	return r
}

// classify decides the overall level from the individual results.
func classify(r *Report) (Level, string) {
	switch {
	case r.TUN.Supported:
		return LevelTUN, "TUN device can be created; kernel networking is available"
	case r.Network.OutboundUsable:
		msg := "no TUN, but outbound TCP/UDP works: userspace transport is the only option"
		if !r.Capability.HasCAPNetAdmin {
			msg += " (CAP_NET_ADMIN absent)"
		}
		return LevelUserspace, msg
	default:
		return LevelUnusable, "no TUN and no working outbound network: this host cannot reach a peer at all"
	}
}

// JSON renders the report as indented JSON.
func (r *Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

func probeHost() HostInfo {
	h := HostInfo{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
		UID:  os.Getuid(),
		EUID: os.Geteuid(),
		PID:  os.Getpid(),
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		h.Kernel = strings.TrimSpace(string(b))
	}
	h.IsContainer, h.ContainerWhy = detectContainer()
	return h
}

// detectContainer looks for unambiguous container markers. Deliberately
// conservative: a false positive would mislead an operator.
func detectContainer() (bool, string) {
	var why []string
	// /.dockerenv is created by Docker.
	if _, err := os.Stat("/.dockerenv"); err == nil {
		why = append(why, "/.dockerenv exists")
	}
	// cgroup v2 path containing /docker/ or /kubepods/ is strong evidence.
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		for _, marker := range []string{"/docker/", "/kubepods/", "/containerd/", "/lxc/", "/buildkit/"} {
			if strings.Contains(s, marker) {
				why = append(why, "cgroup mentions "+marker)
				break
			}
		}
	}
	// PID 1 being something other than a real init is a hint, not proof.
	if b, err := os.ReadFile("/proc/1/comm"); err == nil {
		comm := strings.TrimSpace(string(b))
		switch comm {
		case "tini", "dumb-init", "systemd", "init", "s6-svscan", "runsvdir":
			// ambiguous
		default:
			if comm != "" {
				why = append(why, "pid1 is "+comm)
			}
		}
	}
	return len(why) > 0, strings.Join(why, "; ")
}

// capability bit numbers from linux/capability.h
const (
	capNetAdmin = 12
	capNetRaw   = 13
	capSysAdmin = 21
	capMknod    = 27
)

func probeCaps() CapReport {
	var c CapReport
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		c.ParseError = err.Error()
		return c
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		val := fields[1]
		switch key {
		case "CapEff":
			c.CapEffHex = val
			if v, err := parseHex(val); err == nil {
				c.CapEffDecimal = v
				c.ProcStatusParsed = true
			} else {
				c.ParseError = err.Error()
			}
		case "CapBnd":
			c.CapBndHex = val
		case "CapPrm":
			c.CapPrmHex = val
		case "NoNewPrivs":
			c.NoNewPrivs = val == "1"
		}
	}
	if c.ProcStatusParsed {
		c.HasCAPNetAdmin = c.CapEffDecimal&(1<<capNetAdmin) != 0
		c.HasCAPNetRaw = c.CapEffDecimal&(1<<capNetRaw) != 0
		c.HasCAPSysAdmin = c.CapEffDecimal&(1<<capSysAdmin) != 0
		c.HasCAPMknod = c.CapEffDecimal&(1<<capMknod) != 0
		if !c.HasCAPNetAdmin {
			c.MissingCritical = append(c.MissingCritical, "CAP_NET_ADMIN")
		}
		if !c.HasCAPNetRaw {
			c.MissingCritical = append(c.MissingCritical, "CAP_NET_RAW")
		}
	}
	return c
}

func parseHex(s string) (uint64, error) {
	var v uint64
	if s == "" {
		return 0, fmt.Errorf("empty capability value")
	}
	for _, r := range s {
		var d uint64
		switch {
		case r >= '0' && r <= '9':
			d = uint64(r - '0')
		case r >= 'a' && r <= 'f':
			d = uint64(r-'a') + 10
		case r >= 'A' && r <= 'F':
			d = uint64(r-'A') + 10
		default:
			return 0, fmt.Errorf("invalid hex digit %q in %q", r, s)
		}
		v = v*16 + d
	}
	return v, nil
}
