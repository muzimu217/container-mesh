// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Command acceptance keeps the result ledger of the M4 acceptance list
// honest (M4-3).
//
// docs/design/M4-ACCEPTANCE.md names 52 cells in the method tables of
// §2.1–§2.6 and records their results in §10, between two markers:
//
//	<!-- ledger:begin -->
//	<!-- ledger:end -->
//
// This tool reads that table, refuses it when it breaks a rule, and derives
// the numbers other documents show. The parsing and the validation live in
// one place, and the CI guard (the _test.go files of this package) runs the
// same code: a rule cannot be enforced by the command and skipped by the
// test, or the other way round.
//
// Usage:
//
//	go run ./tools/acceptance check               # exit 1 when a rule is broken
//	go run ./tools/acceptance counts              # the four numbers, group by group
//	go run ./tools/acceptance dashboard           # the HTML fragment for the board
//	go run ./tools/acceptance apply results.tsv   # merge a TSV into §10
//
// Every subcommand takes --doc <path> to read another copy of the document
// (a temporary one, to watch a check fail without touching the real file).
// Evidence pointers always resolve against the repository the tool runs in.
//
// ★ THE RULES, in one place:
//
//   - exactly 52 rows, one per cell of the method tables: no more, no
//     fewer, no duplicates, in the order of the method tables;
//   - the state column is one of: ✔, "◐ — <what is missing>", "✘ — <what
//     failed>", "未验 — <what is missing and who is needed>";
//   - a ✔ row carries a tested SHA (7–40 hex digits), a tester, a
//     YYYY-MM-DD date and at least one evidence pointer;
//   - every evidence pointer resolves in the repository: a test function
//     (a `func TestX(` in some *_test.go), a file with a line number
//     (path:line), or a file path.
//
// "不适用", an empty state and a mark without a note are refused: the
// document's own §6.3 says why — a cell nobody verified must say so, and
// "not applicable" is how a gap disappears.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// docRelPath is where the ledger lives inside the repository.
var docRelPath = filepath.Join("docs", "design", "M4-ACCEPTANCE.md")

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main with injectable streams, so the tests can drive the commands
// and read their exit codes.
//
// The exit codes follow the product's own convention (docs/design/
// M4-ACCEPTANCE.md §4.7): 0 for success, 1 for a broken ledger, 2 for a
// wrong invocation.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	var err error
	switch args[0] {
	case "check":
		err = cmdCheck(args[1:], stdout)
	case "counts":
		err = cmdCounts(args[1:], stdout)
	case "dashboard":
		err = cmdDashboard(args[1:], stdout)
	case "apply":
		err = cmdApply(args[1:], stdout)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "acceptance: unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return 2
	}

	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var ue *usageError
	if errors.As(err, &ue) {
		// The flag package has already said what was wrong.
		return 2
	}
	var le *ledgerError
	if errors.As(err, &le) {
		fmt.Fprintf(stderr, "acceptance: %d rule(s) broken:\n", len(le.violations))
		for _, v := range le.violations {
			fmt.Fprintf(stderr, "  %s\n", v)
		}
		return 1
	}
	fmt.Fprintf(stderr, "acceptance: %v\n", err)
	return 1
}

// usageError is a wrong invocation. The message has already been printed.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }

// usage prints how the tool is called.
func usage(w io.Writer) {
	fmt.Fprint(w, `usage: go run ./tools/acceptance <command> [flags]

commands:
  check               validate the ledger of §10; exit 1 when a rule is broken
  counts              print the four numbers (✔ ◐ ✘ 未验), group by group
  dashboard           print the HTML fragment for docs/project-dashboard.html
  apply <file.tsv>    merge a TSV into the ledger of §10

flags:
  --doc <path>        read this document instead of the repository's own
`)
}

// newFlagSet returns the flag set of one subcommand.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: go run ./tools/acceptance %s [--doc <path>]\n", name)
	}
	return fs
}

// docFlag is the --doc flag every subcommand takes.
func docFlag(fs *flag.FlagSet) *string {
	return fs.String("doc", "", "read this document instead of the repository's own")
}

// cmdCheck validates the ledger and reports every broken rule.
func cmdCheck(args []string, stdout io.Writer) error {
	fs := newFlagSet("check")
	doc := docFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return &usageError{errors.New("check takes no positional argument")}
	}
	parsed, _, cells, violations, err := load(*doc)
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		return &ledgerError{violations: violations}
	}
	fmt.Fprintf(stdout, "ok %s: %d cells, 0 problems\n", parsed.name, len(cells))
	return nil
}

// cmdCounts prints the four numbers, group by group.
func cmdCounts(args []string, stdout io.Writer) error {
	fs := newFlagSet("counts")
	doc := docFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return &usageError{errors.New("counts takes no positional argument")}
	}
	_, _, cells, violations, err := load(*doc)
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		return &ledgerError{violations: violations}
	}
	fmt.Fprint(stdout, renderCounts(cells))
	return nil
}

// cmdDashboard prints the HTML fragment for the board.
//
// The numbers of a ledger that breaks a rule are not printed: a board that
// shows counts nobody checked is the false green this tool exists to stop.
func cmdDashboard(args []string, stdout io.Writer) error {
	fs := newFlagSet("dashboard")
	doc := docFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return &usageError{errors.New("dashboard takes no positional argument")}
	}
	parsed, _, cells, violations, err := load(*doc)
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		return &ledgerError{violations: violations}
	}
	fmt.Fprint(stdout, renderDashboard(cells, parsed.titles))
	return nil
}

// cmdApply merges a TSV into the ledger.
//
// The result is validated BEFORE it is written: a ledger that breaks a rule
// must never reach the file.
func cmdApply(args []string, stdout io.Writer) error {
	fs := newFlagSet("apply")
	doc := docFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return &usageError{errors.New("apply takes exactly one TSV file")}
	}
	root, err := findRepoRoot("")
	if err != nil {
		return err
	}
	path, name, err := docPath(root, *doc)
	if err != nil {
		return err
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	parsed, err := parseDocument(string(text), path, name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	rows, err := parseTSV(data)
	if err != nil {
		return err
	}
	merged, err := parsed.merge(rows)
	if err != nil {
		return err
	}
	result, err := parseDocument(merged, path, name)
	if err != nil {
		return err
	}
	ev, err := newResolver(root)
	if err != nil {
		return err
	}
	cells, violations := result.validate(ev)
	if len(violations) > 0 {
		return &ledgerError{violations: violations}
	}
	if err := writeFileAtomic(path, []byte(merged)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s: %d row(s) merged, %d cells, 0 problems\n", name, len(rows), len(cells))
	return nil
}

// load reads the ledger named by docFlag (the repository's own document by
// default) and validates it.
func load(docFlagValue string) (*document, *resolver, []cell, []violation, error) {
	root, err := findRepoRoot("")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	path, name, err := docPath(root, docFlagValue)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	parsed, err := parseDocument(string(text), path, name)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	ev, err := newResolver(root)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cells, violations := parsed.validate(ev)
	return parsed, ev, cells, violations, nil
}

// docPath returns the document to work on and the name it is reported
// under. A document inside the repository is named by its path inside the
// repository, so the messages do not carry the name of the machine they
// were produced on.
func docPath(root, docFlagValue string) (path, name string, err error) {
	path = docFlagValue
	if path == "" {
		path = filepath.Join(root, docRelPath)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	name = path
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		name = filepath.ToSlash(rel)
	}
	return path, name, nil
}

// findRepoRoot returns the repository root: the directory that holds both a
// go.mod and the acceptance document.
//
// The walk stops at the .git directory, so a checkout that sits inside
// another Go project does not resolve to that project. The nearest go.mod
// is the fallback, for a tree that was exported without its .git.
func findRepoRoot(start string) (string, error) {
	if start == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		start = wd
	}
	fallback := ""
	for dir := start; ; {
		if fileExists(filepath.Join(dir, "go.mod")) {
			if fallback == "" {
				fallback = dir
			}
			if fileExists(filepath.Join(dir, docRelPath)) {
				return dir, nil
			}
		}
		if fileExists(filepath.Join(dir, ".git")) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if fallback == "" {
		return "", fmt.Errorf("no go.mod found at or above %s: run this inside the repository", start)
	}
	return fallback, nil
}

// fileExists reports whether a path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeFileAtomic writes a file through a temporary file and a rename, so
// a failure never leaves a half-written document behind.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}
