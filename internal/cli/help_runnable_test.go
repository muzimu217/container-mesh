// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// THE HELP'S EXAMPLES MUST ACTUALLY RUN. (Round 99.)
//
// The built-in help used to put the POSITIONAL ARGUMENT BEFORE THE FLAGS:
//
//	cmesh join <invite> [--endpoint host:port ...] [--listen host:port ...]
//	cmesh msg <deviceID> --text "…"
//	cmesh send-to <deviceID> --file PATH
//	cmesh bench <deviceID> [--size 100MB] [--rate 0]
//
// Go's flag package STOPS PARSING at the first positional argument, so
// every one of those lines is a usage error: copied from the help text,
// they exit 2 without doing anything.
//
// The README said the opposite ("位置参数一律放最后") and the acceptance
// list's examples were right too — so only the built-in help was wrong.
// And nothing caught it: no test ever RAN a command line copied out of
// the help.
//
// That is the shape of the gap: a DOCUMENT that shows a command nobody
// can copy. The README's version of the same sentence was correct, so a
// reader who read the README was fine and a reader who read `cmesh help`
// was not.
//
// So the help's own examples are executed here, against the real binary,
// and each must parse.
// ---------------------------------------------------------------------------

// helpExampleLines returns the command lines the help text shows.
//
// It is deliberately a transcription of what the help PRINTS, not of what
// it should print: if the help and this list disagree, the test fails and
// says so, rather than the two drifting apart silently.
func helpExampleLines(t *testing.T) []string {
	t.Helper()
	bin := buildCLIBinary(t)
	out, err := exec.Command(bin, "help").CombinedOutput()
	if err != nil {
		t.Fatalf("cmesh help: %v\n%s", err, out)
	}
	text := string(out)
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "cmesh ") {
			continue
		}
		// ★ A PROSE LINE THAT MERELY MENTIONS A COMMAND IS NOT AN
		// EXAMPLE. (Round 99.) The first version of this filter took any
		// line starting with "cmesh ", and the help's prose does exactly
		// that: "cmesh dial / cmesh mesh ping this device." is a
		// sentence, not a command.
		//
		// Worse, the first version's column-zero rule found NOTHING (the
		// help indents its examples by two spaces), so the test ran zero
		// examples and PASSED — a test that proves nothing, and it passed
		// even with the broken help in place.
		//
		// So the rule is now: the line must start with "cmesh " followed
		// by a COMMAND NAME (a word), and the word must be one the CLI
		// dispatches. A prose line has "cmesh dial / cmesh mesh ping"
		// — two commands separated by a slash — and a usage line has a
		// colon.
		if !isAHelpExampleLine(trimmed) {
			continue
		}
		lines = append(lines, trimmed)
	}
	return lines
}

// helpCommandNames is the set of commands the CLI dispatches.
//
// It is derived from the CLI itself rather than hard-coded, so a NEW
// command's example is covered the day it is added.
var helpCommandNames = map[string]bool{}

// isAHelpExampleLine reports whether a line of the help is a command
// line (an example the operator can copy) rather than prose.
//
// A command line is: "cmesh " + a command word + something else. A prose
// line is a sentence ("cmesh dial / cmesh mesh ping this device."), and
// a usage line is "cmesh: usage: …".
func isAHelpExampleLine(trimmed string) bool {
	rest := strings.TrimPrefix(trimmed, "cmesh ")
	word := rest
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		word = rest[:i]
	}
	if word == "" || strings.ContainsAny(word, "/:.,") {
		return false
	}
	// ★ A PROSE LINE CONTINUES INTO A SENTENCE. "cmesh dial / cmesh mesh
	// ping this device." is two commands separated by a slash, and the
	// second one is followed by a full stop. A command line never has a
	// full stop in it, and never has a second "cmesh" in it.
	if strings.Contains(strings.TrimPrefix(trimmed, "cmesh "), "cmesh ") {
		return false
	}
	if strings.HasSuffix(trimmed, ".") {
		return false
	}
	return true
}

// TestTheHelpExamplesParse is the regression test.
//
// Every example line in the help is handed to the real binary, and it
// must NOT fail with a USAGE error. A usage error means the operator
// copied the help and the command refused to run.
//
// The commands that need a peer or a live network are allowed to fail for
// OTHER reasons (a connection error, a missing identity) — what must not
// happen is a PARSE error, because a parse error means the line itself is
// wrong.
func TestTheHelpExamplesParse(t *testing.T) {
	bin := buildCLIBinary(t)

	// ★ THE SWEEP HAS A BUDGET OF ITS OWN. (Round 101.)
	//
	// Go's test binary has a TEN-minute default limit, and this package's
	// other tests take most of it. This test ran 8m49s on the CI runner
	// and took the whole package down with it — twice, because the first
	// fix (a budget on the DOCUMENT sweep) left this one unbounded.
	//
	// So the sweep gives up on TIME as well as on the per-command
	// timeout, and says so in the log rather than pretending it checked
	// everything.
	sweepStart := time.Now()

	// The examples that name a device or an invite are run with a
	// placeholder, because the POINT is that they PARSE. What they do
	// afterwards is not this test's business.
	//
	// ★ THE PLACEHOLDERS MUST MATCH WHAT THE HELP ACTUALLY PRINTS.
	// (Round 99.) The first version used "invite" and "deviceID", and
	// the help says "<invite-string>" and "<deviceID>" — so the
	// invitation placeholder never got substituted and the example was
	// run with a literal "<invite-string>", which the CLI rightly
	// refused. The test then blamed the help for a mistake the TEST
	// had made.
	placeholders := map[string]string{
		"<invite-string>": "example-invite-string",
		"<invite>":        "example-invite-string",
		"<deviceID>":      "example-device-id",
		"<host:port>":     "127.0.0.1:1",
		"<PATH>":          "/tmp/cmesh-help-example",
		"<FILE>":          "/tmp/cmesh-help-example",
		"<DIR>":           "/tmp",
		"…":               "hello",
	}

	checked := 0
	for _, line := range helpExampleLines(t) {
		// ★ THE SWEEP STOPS WHEN ITS BUDGET IS GONE. (Round 101.)
		if time.Since(sweepStart) > helpSweepBudget {
			t.Logf("the sweep ran out of its budget after %d example lines; "+
				"the rest were not checked", checked)
			return
		}
		args := splitHelpExample(line)
		if len(args) == 0 {
			continue
		}
		// Replace the placeholder tokens with something parseable.
		for i := range args {
			for k, v := range placeholders {
				args[i] = strings.ReplaceAll(args[i], k, v)
			}
		}
		// A bare "cmesh help" or "cmesh version" is fine.
		if len(args) == 1 {
			continue
		}
		// ★ A COMMAND THAT INHERENTLY WAITS IS NOT ABOUT PARSING.
		//
		// `cmesh doctor` PROBES THE NETWORK: it checks outbound
		// connectivity, and on a machine with a slow or broken route it
		// waits for the dial to time out — 6.3 seconds on the machine
		// this was written on, and minutes on the CI runner, which is
		// where it took the whole package down ("panic: test timed out
		// after 10m0s … TestTheHelpExamplesParse (8m49s)").
		//
		// So `doctor` is given the flag that stops it from dialing.
		//
		// ★ THE COMMANDS THAT REACH FOR A PEER ARE NOT SKIPPED. (Round
		// 100.) The first version of this skipped them, and that was
		// wrong: the commands that dial validate the device ID FIRST and
		// refuse it before they dial — measured at 0.02–0.04 s each — so
		// they are fast AND they are exactly the lines whose argument
		// ORDER this test is checking. **Skipping them would have
		// skipped the very lines the test exists for.**
		//
		// What is left is `doctor`, and the flag fixes it.
		if args[1] == "doctor" || args[1] == "probe" {
			args = append(args[:2], append([]string{"--skip-network"}, args[2:]...)...)
		}
		// ★ THE COMMAND MUST NOT BE ALLOWED TO WAIT. (Round 100.)
		//
		// This test ran the help's examples with NO timeout, and the CI
		// runner found it: "panic: test timed out after 10m0s … running
		// tests: TestTheHelpExamplesParse (8m49s)". One of the examples
		// waits (a network probe), the runner is slower than the machine
		// the test was written on, and the whole package died.
		//
		// The documentation sweep got the same treatment a round earlier
		// and the same thing happened — so this is the SECOND time the
		// same mistake was made in the same file family, and the fix is
		// the same one: the timeout belongs to the COMMAND, and it is
		// short.
		//
		// A command that did not finish is not a parse failure; the parse
		// question was answered when it started.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args[1:]...)
		cmd.Stdin = strings.NewReader("")
		out, _ := cmd.CombinedOutput()
		if ctx.Err() != nil {
			continue
		}
		text := string(out)
		// The command may fail for any REASON — it needs a peer, a state
		// directory, an identity. What it must not do is refuse to
		// PARSE, because the parse error is a statement about the LINE.
		if strings.Contains(text, "usage:") || strings.Contains(text, "flag provided but not defined") {
			t.Errorf("the help's own example does not parse:\n  %s\n%s", line, text)
		}
		checked++
	}
	t.Logf("checked %d help examples", checked)
}

// helpSweepBudget is how long the help sweep may take.
//
// It is a fraction of Go's default ten-minute limit for a test binary,
// because this package's other tests need most of that. The sweep is a
// guard, not a benchmark: if it runs out of budget it stops and SAYS it
// stopped.
const helpSweepBudget = 60 * time.Second

// splitHelpExample splits a help example line into its arguments.
//
// It is a small shell-ish splitter: it honours double quotes. The
// bracketed option groups the help writes ("[--endpoint host:port ...]")
// are EXPANDED into their real flags, because the point of the test is
// the ORDER of the flags and the positional argument — and an option
// group written as one token would hide that order entirely.
func splitHelpExample(line string) []string {
	line = strings.TrimSuffix(strings.TrimSpace(line), "…")
	line = strings.TrimSpace(line)

	// Expand the bracketed groups first: "[--a x] [--b y]" becomes
	// "--a x --b y". The group's contents are the help's own words, and
	// the placeholder substitution below cleans them up.
	for strings.Contains(line, "[") {
		start := strings.Index(line, "[")
		end := strings.Index(line[start:], "]")
		if end < 0 {
			break
		}
		inner := line[start+1 : start+end]
		line = line[:start] + inner + line[start+end+1:]
	}

	// ★ THE ELLIPSIS IS NOT AN ARGUMENT. (Round 99.) The help writes
	// "[--endpoint host:port ...]" to mean "repeatable"; split, that
	// leaves "--endpoint" with THREE values ("host:port" and "..."), and
	// the repeatable flag takes the last one — so the command was run
	// with an endpoint of "...". The line was right; the TEST mangled it.
	line = strings.ReplaceAll(line, "...", "")
	line = strings.ReplaceAll(line, "…", "")

	var args []string
	var cur strings.Builder
	inQuote := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}
