package cli

import (
	"os"
	"strings"
	"testing"
)

// readCommandSource reads the CLI's command source, so a test can
// assert on a string that is assembled inline in a function with no
// seam to call.
func readCommandSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("meshcmd.go")
	if err != nil {
		t.Fatalf("read meshcmd.go: %v", err)
	}
	return string(b)
}

// ★ THE STARTUP BANNER MUST SAY WHETHER THE RELAY TRUNK IS ON.
//
// The acceptance list says: starting the anchor WITH --relay-trunk
// must print "relay trunk: ON", and starting it WITHOUT the switch
// must print "relay trunk: off". That banner is the operator's only
// evidence that the node is carrying relays — and NOTHING asserted
// it.
//
// So the switch could stop working (or start working when it should
// not) and the whole suite would stay green.
func TestTheSourcePrintsBothBannerStates(t *testing.T) {
	src := readDiscoverySource(t)
	if !strings.Contains(src, "relay trunk: ON") {
		t.Fatal("the anchor no longer prints \"relay trunk: ON\"; " +
			"the operator has no evidence the node is carrying relays")
	}
	if !strings.Contains(src, "relay trunk: off") {
		t.Fatal("the anchor no longer prints \"relay trunk: off\"; " +
			"an operator who forgot the switch gets no warning")
	}
}

// The OFF state must mention the switch that turns it on — otherwise
// the banner tells the operator that relays are off and not what to
// do about it.
func TestTheOffBannerNamesTheSwitch(t *testing.T) {
	src := readDiscoverySource(t)
	i := strings.Index(src, "relay trunk: off")
	if i < 0 {
		t.Fatal("there is no \"relay trunk: off\" banner")
	}
	line := src[i:]
	if end := strings.Index(line, "\n"); end >= 0 {
		line = line[:end]
	}
	if !strings.Contains(line, "--relay-trunk") {
		t.Fatalf("the OFF banner does not name the switch that turns the trunk on: %q", line)
	}
}

// readDiscoverySource reads the discovery command's source.
func readDiscoverySource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("discovery.go")
	if err != nil {
		t.Fatalf("read discovery.go: %v", err)
	}
	return string(b)
}
