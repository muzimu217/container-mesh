package probe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseHex(t *testing.T) {
	cases := map[string]uint64{
		"0":                0,
		"a00c05fb":         0xa00c05fb,
		"A00C05FB":         0xa00c05fb,
		"ffffffffffffffff": 0xffffffffffffffff,
	}
	for in, want := range cases {
		got, err := parseHex(in)
		if err != nil {
			t.Errorf("parseHex(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseHex(%q) = %#x, want %#x", in, got, want)
		}
	}
	for _, bad := range []string{"", "xyz", "12g4"} {
		if _, err := parseHex(bad); err == nil {
			t.Errorf("parseHex(%q) should fail", bad)
		}
	}
}

// The container under test reports CapEff=a00c05fb. That value must decode to
// exactly the capabilities the task description claims: no NET_ADMIN, no
// NET_RAW, no SYS_ADMIN, no MKNOD.
func TestKnownCapEffFromTargetContainer(t *testing.T) {
	v, err := parseHex("a00c05fb")
	if err != nil {
		t.Fatal(err)
	}
	if v&(1<<capNetAdmin) != 0 {
		t.Error("a00c05fb should not include CAP_NET_ADMIN")
	}
	if v&(1<<capNetRaw) != 0 {
		t.Error("a00c05fb should not include CAP_NET_RAW")
	}
	if v&(1<<capSysAdmin) != 0 {
		t.Error("a00c05fb should not include CAP_SYS_ADMIN")
	}
	if v&(1<<capMknod) != 0 {
		t.Error("a00c05fb should not include CAP_MKNOD")
	}
	// Sanity: it should include something, e.g. CAP_CHOWN (bit 0).
	if v&1 == 0 {
		t.Error("a00c05fb should include CAP_CHOWN")
	}
}

func TestProbeMissingDevice(t *testing.T) {
	opts := DefaultOptions()
	opts.SkipNetwork = true
	opts.TUNDevice = filepath.Join(t.TempDir(), "definitely-not-here")
	rep := Run(opts)

	if rep.TUN.DeviceExists {
		t.Error("device should not exist")
	}
	if rep.TUN.Supported {
		t.Error("TUN should not be reported as supported")
	}
	if rep.Level != LevelUnusable {
		t.Errorf("level = %s, want %s (network skipped, no tun)", rep.Level, LevelUnusable)
	}
	if rep.Level.ExitCode() != ExitUnusable {
		t.Errorf("exit code = %d", rep.Level.ExitCode())
	}
}

func TestProbeRegularFileIsNotTUN(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "tun")
	if err := os.WriteFile(fake, []byte("not a device"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	opts.SkipNetwork = true
	opts.TUNDevice = fake
	rep := Run(opts)

	if !rep.TUN.DeviceExists {
		t.Fatal("file exists, DeviceExists should be true")
	}
	if rep.TUN.Supported {
		t.Error("a regular file must never be reported as a usable TUN")
	}
}

func TestProbeJSONShape(t *testing.T) {
	opts := DefaultOptions()
	opts.SkipNetwork = true
	opts.TUNDevice = filepath.Join(t.TempDir(), "nope")
	rep := Run(opts)

	b, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	for _, key := range []string{"timestamp", "host", "tun", "capabilities", "network", "level", "summary"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing top-level key %q", key)
		}
	}
	tun, _ := m["tun"].(map[string]any)
	if tun == nil {
		t.Fatal("tun section missing")
	}
	for _, key := range []string{"device_path", "device_exists", "openable", "ioctl_attempted", "ioctl_ok", "supported"} {
		if _, ok := tun[key]; !ok {
			t.Errorf("missing tun key %q", key)
		}
	}
	caps, _ := m["capabilities"].(map[string]any)
	if caps == nil {
		t.Fatal("capabilities section missing")
	}
	for _, key := range []string{"cap_eff_hex", "has_cap_net_admin", "has_cap_net_raw"} {
		if _, ok := caps[key]; !ok {
			t.Errorf("missing capabilities key %q", key)
		}
	}
}

func TestExitCodesAreDistinct(t *testing.T) {
	codes := map[int]bool{
		LevelTUN.ExitCode():       true,
		LevelUserspace.ExitCode(): true,
		LevelUnusable.ExitCode():  true,
	}
	if len(codes) != 3 {
		t.Fatal("the three levels must map to three distinct exit codes")
	}
	if LevelTUN.ExitCode() != 0 {
		t.Error("the usable-TUN case should exit 0")
	}
}

func TestClassify(t *testing.T) {
	tunOK := &Report{TUN: TUNReport{Supported: true}}
	if lvl, _ := classify(tunOK); lvl != LevelTUN {
		t.Errorf("got %s", lvl)
	}

	userspace := &Report{Network: NetReport{OutboundUsable: true}}
	if lvl, _ := classify(userspace); lvl != LevelUserspace {
		t.Errorf("got %s", lvl)
	}

	dead := &Report{}
	if lvl, _ := classify(dead); lvl != LevelUnusable {
		t.Errorf("got %s", lvl)
	}
}

func TestDNSQueryIsWellFormed(t *testing.T) {
	q := buildDNSQuery(0x1234)
	if len(q) < 12 {
		t.Fatal("query too short")
	}
	if q[0] != 0x12 || q[1] != 0x34 {
		t.Error("transaction id not encoded")
	}
	// "example" + "com" labels.
	if !strings.Contains(string(q[12:]), "example") {
		t.Error("missing example label")
	}
}

// TestNetworkProbeLocalOnly verifies the listen check without touching the
// internet.
func TestNetworkProbeLocalOnly(t *testing.T) {
	got := checkListen()
	if !got.OK {
		t.Fatalf("loopback bind failed: %s", got.Error)
	}
}

func TestProbeRespectsShortTimeout(t *testing.T) {
	opts := DefaultOptions()
	opts.SkipNetwork = false
	opts.NetworkTimeout = 150 * time.Millisecond
	// Point at a blackhole address so the check cannot succeed; we only care
	// that the probe returns promptly rather than hanging.
	opts.TCPAddr = "192.0.2.1:9" // TEST-NET-1, guaranteed unroutable
	opts.UDPServer = "192.0.2.1:9"
	opts.TUNDevice = filepath.Join(t.TempDir(), "nope")

	start := time.Now()
	rep := Run(opts)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("probe took %s, expected it to honour the timeout", elapsed)
	}
	if rep.Network.TCPOutbound.OK {
		t.Error("blackhole TCP target should not report success")
	}
}
