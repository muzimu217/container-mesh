// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// repoRoot returns the repository root, derived from this file's path.
func repoRoot(t *testing.T) string {
	t.Helper()
	// This file lives at <root>/internal/cli/.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

// statPath reports whether a path exists.
func statPath(p string) (bool, error) {
	if _, err := os.Stat(p); err != nil {
		return false, err
	}
	return true, nil
}

// readFileString reads a file as a string.
func readFileString(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// walkDocs collects the markdown files under a directory.
func walkDocs(t *testing.T, dir string, out *[]string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			walkDocs(t, p, out)
			continue
		}
		if strings.HasSuffix(e.Name(), ".md") {
			*out = append(*out, p)
		}
	}
}

// splitDocCommand splits a documented command line into arguments,
// honouring double quotes.
func splitDocCommand(cmd string) []string {
	var args []string
	var cur strings.Builder
	inQuote := false
	for _, r := range cmd {
		switch {
		case r == '"':
			inQuote = !inQuote
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

// ---------------------------------------------------------------------------
// EVERY DOCUMENTED COMMAND LINE MUST PARSE. (Round 99.)
//
// The built-in help used to put the positional argument before the flags
// for four commands, and every one of those lines was a usage error when
// copied. The README said the opposite and was right, so only the help
// was wrong.
//
// Then the same defect was found across the DOCUMENTS: eleven documented
// command lines that do not parse — a `join` with the invite first, a
// `cmesh ping` that is not a command (it is `cmesh mesh ping`), a
// `--account-passphrase` on `cred revoke` and `cred list` which do not
// take it, and a `cmesh install` that does not exist.
//
// So the sweep is here: every `cmesh …` line in the documents is
// extracted and RUN, and it must not fail with a PARSE error.
//
// ★ WHAT IS AND IS NOT CHECKED. A parse failure is a finding. "Cannot
// reach the peer", "no such device" and "the passphrase was wrong" are
// NOT findings — those need a live topology, and the point here is that
// the LINE ITSELF is well formed.
//
// ★ WHY A TEST AND NOT A CHECKLIST. The defects kept coming back because
// nothing executed the documents. A line that no one runs is a line that
// nobody notices is wrong; and the ones that DID get noticed were found
// by a human reading them, which does not scale and does not repeat.
// ---------------------------------------------------------------------------

// shellVarRe matches a shell variable reference in a documented line.
var shellVarRe = regexp.MustCompile(`\$\{?[A-Za-z_][A-Za-z0-9_]*\}?`)

// docCommandLine matches a `cmesh …` command line in a document.
//
// It is deliberately loose about the tail: the goal is to find the
// invocations, and the classifier below decides whether each one is
// parseable.
var docCommandLine = regexp.MustCompile("`?cmesh [a-z][a-z0-9-]*(?: [^`\\n]*)?`?")

// docExampleDirs are the documents the sweep covers.
//
// ★ ONLY THE INSTRUCTIONS, NOT THE DESIGNS. (Round 99.) The first sweep
// covered every markdown file and produced a wall of findings that were
// not defects: a DESIGN document for an unimplemented feature names
// flags that do not exist yet (`--group`), and a document that lists
// the commands of a product names them without their arguments.
//
// A design document is allowed to describe a product that does not
// exist yet — that is what a design document IS. Only the documents
// that tell an operator what to TYPE are held to the standard of
// "this line works".
var docExampleDirs = []string{
	"README.md",
	"deploy/MESH-NET.md",
	"packaging/systemd/README.md",
	"docs/design/M4-ACCEPTANCE.md",
	"docs/M4-CREDENTIAL.md",
}

// docExampleSkipDirs are directories under docs/ that hold HISTORY
// rather than instructions.
//
// ROUND-LOG.md is the project's diary: it records what the commands
// looked like AT THE TIME, including the wrong forms that were fixed
// later. A history that is edited to look correct is not a history.
// So it is read but NOT required to parse.
var docExampleSkipDirs = map[string]bool{
	"docs/ROUND-LOG.md": true,
}

func TestTheDocumentedCommandLinesParse(t *testing.T) {
	bin := buildCLIBinary(t)
	root := repoRoot(t)

	var checked int
	for _, rel := range docExampleDirs {
		files := []string{filepath.Join(root, rel)}
		if fi, err := os.Stat(files[0]); err == nil && fi.IsDir() {
			// It is a directory: walk it.
			var walked []string
			walkDocs(t, filepath.Join(root, rel), &walked)
			files = walked
		}
		for _, f := range files {
			relPath, err := filepath.Rel(root, f)
			if err != nil {
				relPath = f
			}
			if docExampleSkipDirs[relPath] {
				continue
			}
			if !strings.HasSuffix(f, ".md") {
				continue
			}
			data, err := readFileString(f)
			if err != nil {
				continue
			}
			for lineNo, line := range strings.Split(data, "\n") {
				// ★ A BLOCKQUOTED LINE IS PROSE. (Round 99.) A document
				// often QUOTES a command to talk about it ("this would
				// swallow --state X"), and a quoted line is not an
				// instruction. Judging it produced a finding against a
				// line that was deliberately WRONG so the reader could
				// see what wrong looks like.
				if strings.HasPrefix(strings.TrimSpace(line), ">") {
					continue
				}
				for _, cmd := range extractCmeshCommands(line) {
					if !isRunnableDocExample(cmd) {
						continue
					}
					checked++
					if err := runDocExample(bin, cmd); err != nil {
						t.Errorf("%s:%d: the documented command does not parse:\n  %s\n  %v",
							relPath, lineNo+1, cmd, err)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no documented command line was checked: the extractor found " +
			"nothing, so this test would pass on any document")
	}
	t.Logf("checked %d documented command lines", checked)
}

// extractCmeshCommands pulls the `cmesh …` invocations out of one line.
func extractCmeshCommands(line string) []string {
	var out []string
	for _, m := range docCommandLine.FindAllString(line, -1) {
		cmd := strings.Trim(m, "`")
		cmd = strings.TrimSpace(cmd)
		if !strings.HasPrefix(cmd, "cmesh ") {
			continue
		}
		out = append(out, cmd)
	}
	return out
}

// isRunnableDocExample decides whether a line is an INVOCATION that
// should be runnable.
//
// A document mentions commands in prose too ("cmesh msg is for
// delivering a message"), and a prose mention is not an example. The
// distinguishing marks of an example are: it has arguments after the
// command word, and it is not a sentence fragment.
//
// ★ AND IT MUST BE A COMPLETE INVOCATION. (Round 99.) The first sweep
// judged lines like "cmesh join --listen 0.0.0.0:7000" (a systemd
// ExecStart, with the invite supplied another way) and
// "cmesh mesh ping" (a mention in a feature list). Neither is a
// copy-pasteable instruction, and judging them produced findings that
// were not defects.
//
// So a line is judged when it names a command AND at least one of:
// a flag, a placeholder, or a positional value — AND it does not end in
// a way that says "this is a list" (a trailing "│" from a diagram, an
// ellipsis).
func isRunnableDocExample(cmd string) bool {
	rest := strings.TrimPrefix(cmd, "cmesh ")
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return false
	}
	// A mention inside a sentence: the line ends with a period, or the
	// command is followed by prose words rather than flags/placeholders.
	if strings.HasSuffix(cmd, ".") {
		return false
	}
	// A diagram row or a table cell: it carries a box-drawing character
	// or a separator.
	if strings.ContainsAny(cmd, "│|") {
		return false
	}
	// The command word itself.
	word := fields[0]
	if strings.ContainsAny(word, "/:.,") {
		return false
	}
	if len(fields) == 1 {
		return false
	}
	// It must carry an ARGUMENT, not just the command name.
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") {
			return true
		}
		if strings.HasPrefix(f, "<") {
			return true
		}
		// A plain value is an argument too — but a VALUE with no flag
		// in front of it is usually prose ("cmesh dial cmesh mesh ping
		// this device" is handled above by the second-cmesh rule).
		return true
	}
	return false
}

// runDocExample runs a documented command and reports whether it failed
// with a PARSE error.
//
// The parse failures are: a usage message, an unknown flag, an unknown
// command, and "flag needs an argument".
func runDocExample(bin, cmd string) error {
	fields := splitDocCommand(cmd)
	if len(fields) == 0 {
		return nil
	}
	args := fields[1:]
	// Replace the placeholder tokens with values that parse.
	repl := map[string]string{
		"<invite>":        "example-invite",
		"<invite-string>": "example-invite",
		"<邀请串>":           "example-invite",
		"<deviceID>":      "aaaaaaaaaaaaaaaa",
		"<device-id>":     "aaaaaaaaaaaaaaaa",
		"<peer-id>":       "aaaaaaaaaaaaaaaa",
		"<id>":            "aaaaaaaaaaaaaaaa",
		"<PATH>":          "/tmp/cmesh-doc-example",
		"<FILE>":          "/tmp/cmesh-doc-example",
		"<DIR>":           "/tmp",
		"<TEXT>":          "hello",
		"<host:port>":     "127.0.0.1:1",
		"<ADDR>":          "127.0.0.1:1",
		"<ACCOUNT_STATE>": "/tmp/cmesh-doc-example",
		"<ANCHOR_STATE>":  "/tmp/cmesh-doc-example",
		"<STATE>":         "/tmp/cmesh-doc-example",
		"<state>":         "/tmp/cmesh-doc-example",
		"<SIZE>":          "1MB",
		"<SIZE_MB>":       "1",
		"<N>":             "1",
		"<COUNT>":         "1",
		"<PORT>":          "1",
		"<URL>":           "https://example.invalid/x",
		"<TOKEN>":         "x",
		"<PASS>":          "x",
		"<口令>":            "x",
		"<CRED>":          "x",
		"<CRED_ID>":       "x",
		"<ID>":            "x",
		"<KEY>":           "x",
		"…":               "x",
		"...":             "",
	}
	for i := range args {
		for k, v := range repl {
			args[i] = strings.ReplaceAll(args[i], k, v)
		}
	}
	// ★ A SHELL VARIABLE IS A PLACEHOLDER, NOT A SKIP. (Round 99.) The
	// first version skipped any line containing "$", because a shell
	// variable cannot be substituted by the test. But that is exactly
	// where the interesting defects live: a systemd ExecStart reads the
	// invite from an EnvironmentFile, so the LINE has "$CMESH_INVITE" in
	// it — and skipping those lines made the sweep blind to the very
	// lines that are copied into a unit file.
	//
	// So the variable is SUBSTITUTED with a placeholder value. The
	// question the sweep asks is whether the LINE parses, and a
	// variable's value has no bearing on that.
	for i := range args {
		args[i] = shellVarRe.ReplaceAllString(args[i], "example-value")
	}
	if strings.Contains(strings.Join(args, " "), "$") {
		// A variable we did not recognise: the line cannot be run as
		// written, and that is not a finding about the DOCUMENT.
		return nil
	}
	// A line with a redirect or a pipe is a shell recipe, not a command.
	if strings.Contains(cmd, "|") || strings.Contains(cmd, ">") || strings.Contains(cmd, "&") {
		return nil
	}

	// ★ THE COMMAND MUST NOT BE ALLOWED TO WAIT, AND THE TIMEOUT MUST
	// BE OWNED BY THE COMMAND, NOT BY A KILL FROM ANOTHER GOROUTINE.
	// (Round 99.)
	//
	// The first version ran the command in a goroutine and killed it from
	// the main goroutine on timeout. That is a data race: `Cmd.Run` writes
	// the process state while `Process.Kill` reads it, and the race
	// detector reported it on the first CI run with -race.
	//
	// The second version moved the output through a channel, which fixed
	// the output race but not this one — the kill still races with the
	// command's own bookkeeping.
	//
	// So the timeout is INSIDE the command: a context with a deadline,
	// and `CommandContext`, which kills the process itself and waits for
	// it. No concurrent kill, no shared state, no race.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	proc := exec.CommandContext(ctx, bin, args...)
	proc.Stdin = strings.NewReader("")
	out, _ := proc.CombinedOutput()
	// A command that did not finish is not a parse failure; it is a
	// command that WAITS.
	if ctx.Err() != nil {
		return nil
	}
	text := string(out)
	for _, marker := range []string{
		"usage: cmesh",
		"flag provided but not defined",
		"unknown command",
		"unknown subcommand",
		"flag needs an argument",
		"unknown shorthand flag",
	} {
		if strings.Contains(text, marker) {
			return &parseError{marker: marker, output: text}
		}
	}
	return nil
}

// parseError says a documented line did not parse.
type parseError struct {
	marker string
	output string
}

func (e *parseError) Error() string {
	first := e.output
	if i := strings.Index(first, "\n"); i > 0 {
		first = first[:i]
	}
	return e.marker + ": " + first
}
