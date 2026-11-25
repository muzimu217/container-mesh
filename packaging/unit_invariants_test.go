// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ THE UNIT FILES ARE THE DEPLOYMENT CONTRACT, AND THEY ARE CHECKED
// HERE.
//
// The systemd units are what turn a directory of binaries into a service.
// Everything about them — the start limit, the restart policy, the log
// destination, the preconditions, the path the binary is executed from —
// is a decision an operator finds out about by reading a journal at 3am.
//
// So the decisions are pinned by TESTS. Each one below corresponds to a
// systemd semantic that is EASY to get wrong and SILENT when you do:
//
//   - StartLimitIntervalSec / StartLimitBurst are [Unit] directives.
//     systemd has NO Service.StartLimit* alias, so in [Service] they are
//     ignored with a warning that nobody reads — and the start limit the
//     operator believes in is not the one systemd applies.
//
//   - ExecStartPost does NOT run when ExecStart fails. A unit that
//     depends on its ExecStartPost having run is wrong.
//
//   - ExecStartPre's failure is a start FAILURE, and Restart=on-failure
//     covers it — so a pure precondition written as ExecStartPre restarts
//     forever. A precondition belongs in ExecCondition, which does not
//     restart the unit.
//
//   - The log destination is a default, and defaults are changeable by
//     whoever configures the machine. A unit that depends on the default
//     is a unit whose diagnostics can vanish.
//
// The parser below is deliberately small: it reads the unit files as the
// flat key=value files they are.

// unitFile is a parsed systemd unit.
type unitFile struct {
	path string
	// sections maps a section name to its key/value pairs, in file order.
	sections map[string][][2]string
	// order is the section names in file order.
	order []string
}

// section returns the key/value pairs of a section.
func (u *unitFile) section(name string) [][2]string {
	return u.sections[name]
}

// value returns the value of a key in a section ("" when absent).
func (u *unitFile) value(section, key string) string {
	for _, kv := range u.sections[section] {
		if kv[0] == key {
			return kv[1]
		}
	}
	return ""
}

// hasKey reports whether a section carries a key.
func (u *unitFile) hasKey(section, key string) bool {
	for _, kv := range u.sections[section] {
		if kv[0] == key {
			return true
		}
	}
	return false
}

// keys returns the keys of a section.
func (u *unitFile) keys(section string) []string {
	var out []string
	for _, kv := range u.sections[section] {
		out = append(out, kv[0])
	}
	return out
}

// parseUnit reads a unit file.
//
// A comment (# or ;) and a blank line are ignored. A line that is not a
// section header and not a key=value pair is a PARSE ERROR, not
// something to skip: a unit file with a typo in it is exactly the kind
// of thing these tests exist to catch, and silently skipping the line
// would let the typo through.
func parseUnit(t *testing.T, path string) *unitFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	u := &unitFile{path: path, sections: map[string][][2]string{}}
	section := ""
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			if !strings.HasSuffix(trimmed, "]") {
				t.Fatalf("%s:%d: malformed section header %q", path, i+1, trimmed)
			}
			section = strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")
			if _, seen := u.sections[section]; !seen {
				u.order = append(u.order, section)
				u.sections[section] = nil
			}
			continue
		}
		eq := strings.Index(trimmed, "=")
		if eq < 0 {
			t.Fatalf("%s:%d: not a key=value line: %q", path, i+1, trimmed)
		}
		key := strings.TrimSpace(trimmed[:eq])
		value := strings.TrimSpace(trimmed[eq+1:])
		u.sections[section] = append(u.sections[section], [2]string{key, value})
	}
	return u
}

// packagedUnits returns the unit files this repository ships, parsed.
//
// The list is explicit rather than a directory glob, because a glob would
// silently shrink the set of checked units when a file is renamed — and
// a test suite that checks fewer things over time looks exactly like a
// test suite that is passing.
func packagedUnits(t *testing.T) []*unitFile {
	t.Helper()
	names := []string{
		"cmesh-discovery.service",
		"cmesh-member.service",
	}
	var out []*unitFile
	for _, name := range names {
		path := filepath.Join("systemd", name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the shipped unit %s is missing: %v", path, err)
		}
		out = append(out, parseUnit(t, path))
	}
	return out
}

// ★ THE START LIMIT MUST BE IN [Unit].
//
// systemd has no Service.StartLimit* alias, so a StartLimitIntervalSec
// written under [Service] is IGNORED — with a warning on stderr that
// nobody reads in production.
//
// The consequence is not cosmetic. systemd's default is a burst of 5
// starts in a 10-second window, and with RestartSec=5s the restarts
// land at 0, 5, 10, 15 … so any 10-second window holds at most 3
// starts and the burst of 5 is UNREACHABLE. The limit never fires, and a
// configuration error (an empty passphrase, an unreadable credential, a
// read-only state directory) restarts FOREVER, filling the journal
// instead of stopping where an operator can see it.
//
// The window has to be longer than RestartSec * (Burst - 1) for the
// burst to be reachable at all: 5 * 4 = 20 seconds, and both units use
// 60.
//
// Measured on a real systemd 255 with ExecStart=/bin/false,
// Restart=on-failure, RestartSec=5s:
//
//	in [Service]  →  NRestarts=18 after 95s, STILL looping
//	in [Unit]     →  NRestarts=5, then ActiveState=failed (stopped)
func TestTheStartLimitLivesInTheUnitSection(t *testing.T) {
	for _, u := range packagedUnits(t) {
		for _, key := range []string{"StartLimitIntervalSec", "StartLimitBurst"} {
			if u.hasKey("Service", key) {
				t.Errorf("%s: %s is in [Service], where systemd IGNORES it "+
					"(there is no Service.StartLimit* alias); it belongs in [Unit]",
					u.path, key)
			}
			if !u.hasKey("Unit", key) {
				t.Errorf("%s: %s is not in [Unit]; systemd's default window "+
					"(10s) makes the burst of 5 unreachable with RestartSec=5s, "+
					"so a broken configuration restarts forever", u.path, key)
			}
		}
	}
}

// ★ THE START LIMIT MUST ACTUALLY BE REACHABLE.
//
// The arithmetic above is only true when the window is longer than
// RestartSec * (Burst - 1). This test computes it, so a unit that
// tightens RestartSec without widening the window is caught here rather
// than in a journal at 3am.
func TestTheStartLimitWindowIsReachable(t *testing.T) {
	for _, u := range packagedUnits(t) {
		restartSec := u.value("Service", "RestartSec")
		burst := u.value("Unit", "StartLimitBurst")
		window := u.value("Unit", "StartLimitIntervalSec")
		if restartSec == "" || burst == "" || window == "" {
			t.Errorf("%s: the restart policy is incomplete "+
				"(RestartSec=%q StartLimitBurst=%q StartLimitIntervalSec=%q); "+
				"without all three the limit cannot be reasoned about",
				u.path, restartSec, burst, window)
			continue
		}
		sec, ok := parseSeconds(restartSec)
		if !ok {
			t.Errorf("%s: RestartSec=%q is not a duration this test understands",
				u.path, restartSec)
			continue
		}
		win, ok := parseSeconds(window)
		if !ok {
			t.Errorf("%s: StartLimitIntervalSec=%q is not a duration this test understands",
				u.path, window)
			continue
		}
		burstN, ok := parseInt(burst)
		if !ok {
			t.Errorf("%s: StartLimitBurst=%q is not an integer", u.path, burst)
			continue
		}
		need := sec * float64(burstN-1)
		if win <= need {
			t.Errorf("%s: the start-limit window (%gs) is not longer than "+
				"RestartSec*(Burst-1) (%gs), so a burst of %d starts is "+
				"UNREACHABLE and a broken configuration restarts forever",
				u.path, win, need, burstN)
		}
	}
}

// ★ A PURE PRECONDITION MUST NOT BE AN ExecStartPre.
//
// ExecStartPre's failure is a start FAILURE, and Restart=on-failure
// covers it — so a precondition written that way retries forever.
// ExecCondition is the right directive: systemd does not restart a unit
// whose CONDITION failed, so the refusal is stable and the operator
// sees it once, with the reason.
//
// The member unit already uses ExecCondition for its relay trunk. This
// test makes sure that stays true, and that no unit grows an
// ExecStartPre by accident.
func TestNoUnitUsesExecStartPreForAPrecondition(t *testing.T) {
	for _, u := range packagedUnits(t) {
		if u.hasKey("Service", "ExecStartPre") {
			t.Errorf("%s: ExecStartPre is used; a precondition written as "+
				"ExecStartPre is a start failure, and Restart=on-failure makes "+
				"it retry forever. Use ExecCondition, which does not restart "+
				"the unit", u.path)
		}
	}
}

// ★ THE LOG DESTINATION MUST BE STATED, NOT INHERITED.
//
// systemd's default for a system service is the journal, and the journal
// rotates on its own (SystemMaxUse). So the answer to "where do the
// logs go" is "the journal" — but only as long as the DEFAULT holds.
//
// The default is a MANAGER-wide setting (DefaultStandardOutput in
// systemd/system.conf), and a machine that hardens it to "null" throws
// the service's diagnostics away. A unit that says where its output
// goes is immune to that.
//
// SyslogIdentifier matters for the same reason from the other side: it
// gives the output a stable tag, so `journalctl -t cmesh-discovery`
// finds it without knowing the unit's name.
func TestTheUnitsStateTheirLogDestination(t *testing.T) {
	for _, u := range packagedUnits(t) {
		if got := u.value("Service", "StandardOutput"); got != "journal" {
			t.Errorf("%s: StandardOutput=%q; it should be journal, stated "+
				"explicitly so the machine's DefaultStandardOutput cannot "+
				"silently discard the service's diagnostics", u.path, got)
		}
		if got := u.value("Service", "StandardError"); got != "journal" {
			t.Errorf("%s: StandardError=%q; it should be journal, stated "+
				"explicitly so the machine's DefaultStandardError cannot "+
				"silently discard the service's diagnostics", u.path, got)
		}
		if u.value("Service", "SyslogIdentifier") == "" {
			t.Errorf("%s: SyslogIdentifier is not set; without it the output "+
				"has no stable tag and `journalctl -t` cannot find it", u.path)
		}
	}
}

// ★ A UNIT MUST COME BACK AFTER A REBOOT.
//
// "Restart=on-failure" covers a crash. A REBOOT is not a crash — the
// unit is simply not running anymore, and systemd starts it again
// because of [Install] WantedBy.
//
// The unit therefore needs WantedBy=multi-user.target (which is what
// `systemctl enable` writes the symlink for) and an ordering that puts
// it after the network is up (the anchor announces an address, so it
// must not start before the network exists).
func TestTheUnitsComeBackAfterAReboot(t *testing.T) {
	for _, u := range packagedUnits(t) {
		if got := u.value("Install", "WantedBy"); got != "multi-user.target" {
			t.Errorf("%s: WantedBy=%q; a unit that is not wanted by "+
				"multi-user.target is not started at boot, so the host comes "+
				"back with the service DOWN", u.path, got)
		}
		if !u.hasKey("Unit", "After") {
			t.Errorf("%s: [Unit] has no After=; without an ordering the unit "+
				"may start before the network the anchor announces on", u.path)
		}
		after := u.value("Unit", "After")
		if !strings.Contains(after, "network-online.target") {
			t.Errorf("%s: After=%q does not order the unit after "+
				"network-online.target", u.path, after)
		}
		if u.value("Service", "Restart") == "" {
			t.Errorf("%s: Restart is not set; a crashed service would stay "+
				"down until someone noticed", u.path)
		}
		if u.value("Service", "RestartSec") == "" {
			t.Errorf("%s: RestartSec is not set", u.path)
		}
	}
}

// ★ THE BINARY PATH MUST BE THE MANAGED LAYOUT.
//
// The units execute a fixed path. The installers manage a versioned
// layout behind a `current` symlink, so the path has to be that
// symlink — and it has to be under the managed root, or an upgrade that
// rotates `current` would not be picked up by the running service.
//
// The check is deliberately about the SHAPE (a managed root, a
// `current` symlink), not about one exact path: the root is a
// deployment choice, and the point is that the unit follows it.
func TestTheUnitsExecuteTheManagedLayout(t *testing.T) {
	for _, u := range packagedUnits(t) {
		exec := u.value("Service", "ExecStart")
		if exec == "" {
			t.Fatalf("%s: ExecStart is empty", u.path)
		}
		if !strings.Contains(exec, "/current/cmesh") {
			t.Errorf("%s: ExecStart=%q does not execute the managed `current` "+
				"symlink; an upgrade that rotates `current` would not reach the "+
				"running service", u.path, exec)
		}
	}
}

// ★ THE ANCHOR MUST REFUSE TO START WITHOUT ITS LOAD-BEARING CONFIG.
//
// The anchor enrolls members. Without the account public key it cannot
// verify a team credential, so the enrollment endpoint is NOT installed
// at all — and the node comes up looking perfectly healthy while every
// member's join is refused.
//
// The failure is silent in the worst way: the unit is active, the
// listener is up, and the operator has to discover the problem from a
// member's error message.
//
// The member unit already refuses to start when its relay trunk is
// unconfigured, with an ExecCondition so the refusal is stable and the
// reason is printed once. The anchor needs the same treatment for the
// values it cannot work without.
//
// The state directory is in the same list: with an empty CMESH_STATE the
// CLI falls back to ~/.cmesh, which is NOT the directory the unit
// declares (StateDirectory=) and NOT the one the hardening makes
// writable — so the anchor would look for its identity in the wrong
// place and fail with a message that points at the wrong path.
func TestTheAnchorRefusesToStartWithoutItsLoadBearingConfiguration(t *testing.T) {
	units := packagedUnits(t)
	var anchor *unitFile
	for _, u := range units {
		if strings.Contains(u.path, "cmesh-discovery.service") {
			anchor = u
		}
	}
	if anchor == nil {
		t.Fatal("the anchor unit (cmesh-discovery.service) is missing")
	}
	conditions := anchor.section("Service")
	var joined string
	for _, kv := range conditions {
		if kv[0] == "ExecCondition" {
			joined += kv[1] + "\n"
		}
	}
	if joined == "" {
		t.Fatalf("%s: the anchor has no ExecCondition; with an empty "+
			"CMESH_ACCOUNT_PUB it comes up looking healthy while enrolling "+
			"nobody (the enrollment endpoint is simply not installed)", anchor.path)
	}
	for _, want := range []string{"CMESH_ACCOUNT_PUB", "CMESH_STATE"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s: the anchor's ExecCondition does not check %s; "+
				"the failure it prevents is silent", anchor.path, want)
		}
	}
}

// ★ THE MEMBER MUST REFUSE TO START WITHOUT ITS STATE DIRECTORY.
//
// The member's state holds a device private key and credential
// material, and it MUST be its own directory: the member unit declares
// StateDirectory=cmesh-member and makes /var/lib/cmesh-member writable.
// With an empty CMESH_STATE the CLI falls back to ~/.cmesh — which is
// the ANCHOR's directory, read-only under the unit's hardening, and the
// wrong trust domain besides.
//
// The member already refuses to start when its relay trunk is
// unconfigured. The state directory belongs in the same guard: it is
// the same class of "the unit starts and then fails in a way that
// points at the wrong path" failure.
func TestTheMemberRefusesToStartWithoutItsStateDirectory(t *testing.T) {
	units := packagedUnits(t)
	var member *unitFile
	for _, u := range units {
		if strings.Contains(u.path, "cmesh-member.service") {
			member = u
		}
	}
	if member == nil {
		t.Fatal("the member unit (cmesh-member.service) is missing")
	}
	var joined string
	for _, kv := range member.section("Service") {
		if kv[0] == "ExecCondition" {
			joined += kv[1] + "\n"
		}
	}
	if !strings.Contains(joined, "CMESH_STATE") {
		t.Errorf("%s: the member's ExecCondition does not check CMESH_STATE; "+
			"an empty value silently moves the member's device key into the "+
			"anchor's directory, which the unit makes read-only", member.path)
	}
}

// ★ AN ExecCondition's MESSAGE MUST NOT BE SHELL CODE.
//
// The ExecCondition runs `/bin/sh -c '<script>'`, so the script is
// SHELL — and a backtick in it is COMMAND SUBSTITUTION.
//
// A message that reads naturally in Markdown ("run `cmesh account show`
// to get the key") therefore executes `cmesh account show` — or, worse,
// prints "sh: 1: cmesh: not found" into the journal and drops the
// instruction the operator needed.
//
// Measured on real systemd 249 with the message
//
//	Put the key from `cmesh account show --state /var/lib/cmesh` into ...
//
// the unit's journal carried:
//
//	/bin/sh: 1: cmesh: not found
//	cmesh-discovery: CMESH_ACCOUNT_PUB is not set; ... Put the key from  into /etc/cmesh/discovery.env
//
// — the command ran, and the instruction arrived with a HOLE in it.
//
// So the message must not contain a backtick. This test is the guard.
func TestNoExecConditionMessageContainsShellCommandSubstitution(t *testing.T) {
	for _, u := range packagedUnits(t) {
		for _, kv := range u.section("Service") {
			if kv[0] != "ExecCondition" {
				continue
			}
			if strings.Contains(kv[1], "`") {
				t.Errorf("%s: the ExecCondition message contains a backtick; "+
					"the condition runs under /bin/sh, so a backtick is COMMAND "+
					"SUBSTITUTION: the command runs, and the operator's "+
					"instruction arrives with a hole in it", u.path)
			}
		}
	}
}

// parseSeconds parses a systemd duration into seconds.
//
// It handles the forms the units actually use: a bare number (seconds)
// and a number with a unit suffix (s, sec, m, min, h).
func parseSeconds(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	units := []struct {
		suffix string
		scale  float64
	}{
		{"ms", 0.001},
		{"s", 1},
		{"sec", 1},
		{"m", 60},
		{"min", 60},
		{"h", 3600},
	}
	for _, u := range units {
		if strings.HasSuffix(v, u.suffix) {
			n, ok := parseInt(strings.TrimSpace(strings.TrimSuffix(v, u.suffix)))
			if !ok {
				return 0, false
			}
			return float64(n) * u.scale, true
		}
	}
	n, ok := parseInt(v)
	if !ok {
		return 0, false
	}
	return float64(n), true
}

// parseInt parses a plain non-negative integer.
func parseInt(v string) (int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}
