// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/blackevil/container-mesh/internal/probe"
)

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		asJSON      = fs.Bool("json", false, "emit structured JSON (default when --json is set; text otherwise)")
		skipNetwork = fs.Bool("skip-network", false, "skip outbound connectivity checks")
		device      = fs.String("device", "/dev/net/tun", "TUN device path to test")
		iface       = fs.String("iface", "cmesh-probe0", "interface name requested in TUNSETIFF")
		udpTarget   = fs.String("udp", "1.1.1.1:53", "UDP probe target")
		tcpTarget   = fs.String("tcp", "1.1.1.1:443", "TCP probe target")
		quiet       = fs.Bool("quiet", false, "suppress the human summary on stderr")
	)
	if err := fs.Parse(args); err != nil {
		return ExitError{Code: 2, Err: err}
	}

	opts := probe.DefaultOptions()
	opts.SkipNetwork = *skipNetwork
	opts.TUNDevice = *device
	opts.IfaceName = *iface
	opts.UDPServer = *udpTarget
	opts.TCPAddr = *tcpTarget

	rep := probe.Run(opts)

	if *asJSON {
		b, err := rep.JSON()
		if err != nil {
			return ExitError{Code: probe.ExitError, Err: err}
		}
		fmt.Fprintln(os.Stdout, string(b))
	} else if !*quiet {
		printHuman(rep)
	}

	// The exit code is the machine-readable contract.
	return ExitError{Code: rep.Level.ExitCode()}
}

func printHuman(rep *probe.Report) {
	fmt.Printf("host:        %s/%s uid=%d euid=%d\n", rep.Host.OS, rep.Host.Arch, rep.Host.UID, rep.Host.EUID)
	if rep.Host.IsContainer {
		fmt.Printf("container:   yes (%s)\n", rep.Host.ContainerWhy)
	} else {
		fmt.Printf("container:   not detected\n")
	}
	fmt.Printf("tun device:  %s exists=%v openable=%v ioctl=%v\n",
		rep.TUN.DevicePath, rep.TUN.DeviceExists, rep.TUN.Openable, rep.TUN.IoctlOK)
	if rep.TUN.IoctlError != "" {
		fmt.Printf("tun error:   %s\n", rep.TUN.IoctlError)
	}
	fmt.Printf("cap_eff:     %s  net_admin=%v net_raw=%v sys_admin=%v mknod=%v\n",
		rep.Capability.CapEffHex,
		rep.Capability.HasCAPNetAdmin,
		rep.Capability.HasCAPNetRaw,
		rep.Capability.HasCAPSysAdmin,
		rep.Capability.HasCAPMknod,
	)
	if rep.Network.TCPOutbound.Target != "" {
		fmt.Printf("tcp out:     %v %s (%s)\n", rep.Network.TCPOutbound.OK, rep.Network.TCPOutbound.Target, rep.Network.TCPOutbound.Latency)
		fmt.Printf("udp out:     %v %s (%s)\n", rep.Network.UDPOutbound.OK, rep.Network.UDPOutbound.Target, rep.Network.UDPOutbound.Latency)
		fmt.Printf("dns:         %v (%s)\n", rep.Network.DNSResolution.OK, rep.Network.DNSResolution.Latency)
		if rep.Network.PublicIPViaSTUN != "" {
			fmt.Printf("public ip:   %s (via STUN)\n", rep.Network.PublicIPViaSTUN)
		}
	}
	fmt.Printf("level:       %s\n", rep.Level)
	fmt.Printf("summary:     %s\n", rep.Summary)
}
