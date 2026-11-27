// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

// The evidence of a ledger row: what a pointer in the "证据" column points
// at, inside the repository.
//
// Four shapes are accepted:
//
//   - a test function: a `func TestX(` in some *_test.go of the repository;
//   - a file with a line number: `path:line`, where the file exists and
//     has at least that many lines;
//   - a file with a symbol: `path:Symbol`, where the file exists and
//     DEFINES that symbol. Several symbols of one file may be listed,
//     separated by commas: `path:TestA,TestB`;
//   - a file: a path that exists in the repository.
//
// The `path:Symbol` shape exists because that is how the evidence was
// written in the first place: a symbol on its own says "somewhere in this
// repository", while the same symbol behind a path says "in THIS file" —
// which is a stronger claim, and one a reader can check.
//
// Anything else is refused, because a pointer that resolves to nothing is a
// claim without a witness.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// evidenceKind is what an evidence pointer turned out to be.
type evidenceKind int

const (
	evidenceNone evidenceKind = iota
	evidenceTest
	evidenceFileLine
	evidenceFile
)

// resolver answers what an evidence pointer points at, inside one
// repository.
type resolver struct {
	root  string
	tests map[string]string // test function name → the file holding it
}

// newResolver indexes the repository: it walks the source tree and reads
// the names of the functions `go test` runs.
func newResolver(root string) (*resolver, error) {
	r := &resolver{root: root, tests: map[string]string{}}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		names, err := testFunctions(path)
		if err != nil {
			if os.IsNotExist(err) {
				// The file went away while we walked. Another line is
				// working in this tree; that is not our problem to
				// report as a broken ledger.
				return nil
			}
			return err
		}
		for _, n := range names {
			if _, dup := r.tests[n]; !dup {
				r.tests[n] = path
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index the repository: %w", err)
	}
	return r, nil
}

// skipDir reports whether a directory is not part of the source tree the
// go tool builds: dot-directories (the go tool ignores them as well),
// node_modules, and .git.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules"
}

// testFunctionRe matches the functions `go test` runs by name.
var testFunctionRe = regexp.MustCompile(`^func ((?:Test|Fuzz|Example)[A-Za-z0-9_]*)\(`)

// testFunctions returns the names of the test functions of one file.
func testFunctions(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var names []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if m := testFunctionRe.FindStringSubmatch(sc.Text()); m != nil {
			names = append(names, m[1])
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return names, nil
}

var (
	identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ★ The forms the evidence actually uses. Each one is a claim about a
// file, and each has to be checked against that file.
//
//	path:12          one line
//	path:12,34       two lines
//	path:12-18       a range of lines
//	path:TestX       a symbol defined in that file
//	path:TestX,TestY two symbols of that file
//	path:TestX/sub   a Go subtest: TestX with the subtest "sub"
//
// The line forms are one "path:lines" shape whose list holds line numbers
// and ranges; the symbol forms hold names.

// lineListRe matches the list of line numbers and ranges after the colon
// of a "path:lines" pointer.
var lineListRe = regexp.MustCompile(`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)

// subtestPointerRe matches a Go subtest path: a test function, a slash, and
// the name of the subtest.
//
// ★ GO REWRITES THE SPACES OF A SUBTEST NAME, so the same subtest is
// written two ways in the wild: "quota exhausted" in the source and
// "quota_exhausted" in a test name. A pointer may use either, so the
// name part accepts spaces as well as underscores.
var subtestPointerRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)/([A-Za-z0-9_ ]+)$`)

// symbolListRe matches "path:Symbol" — the colon is followed by a name
// that starts with a letter, which is what separates it from "path:line".
// The names may be separated by commas or by plus signs: "path:A,B" and
// "path:A+B" both name two symbols of that file.
var symbolListRe = regexp.MustCompile(`^(.+):([A-Za-z_][A-Za-z0-9_]*([,+][A-Za-z_][A-Za-z0-9_]*)*)$`)

// symbolDefRe matches the definition of a symbol in a Go file: the name at
// the start of a line, followed by "(" — a function or a method.
var symbolDefRe = regexp.MustCompile(`^func (\([^)]*\) )?([A-Za-z_][A-Za-z0-9_]*)\(`)

// resolve reports what an evidence pointer points at.
func (r *resolver) resolve(p string) (evidenceKind, error) {
	if p == "" {
		return evidenceNone, errors.New("the pointer is empty (a stray \";\"?)")
	}
	// ★ A POINTER MAY CARRY A REMARK.
	//
	// The evidence column holds a pointer and, sometimes, a note about it
	// in brackets — "internal/cli/discovery_test.go:TestMeshBootstrapApprovalFlow(ping 段)".
	// The remark is for the reader; the pointer is what has to resolve.
	// It is stripped here, and the bare pointer is what gets checked.
	p, _ = splitRemark(p)
	// Split at the LAST colon: the path is everything before it.
	path, rest, ok := splitOnce(p)
	if !ok {
		return r.resolveBare(p)
	}
	// "path:lines" — digits, commas and dashes only, so it cannot be
	// mistaken for a symbol.
	if lineListRe.MatchString(rest) {
		return r.resolveLines(path, rest)
	}
	// "path:TestX/sub" — a Go subtest. The parent function must exist, and
	// the subtest must be named inside it.
	if m := subtestPointerRe.FindStringSubmatch(rest); m != nil {
		return r.resolveSubtest(path, m[1], m[2])
	}
	// "path:Symbol" — the path must hold the symbol, not merely exist.
	if m := symbolListRe.FindStringSubmatch(p); m != nil {
		path, names := m[1], m[2]
		if err := r.checkPath(path); err != nil {
			return evidenceNone, err
		}
		if err := r.checkTracked(path); err != nil {
			return evidenceNone, err
		}
		defined, err := r.symbolsIn(path)
		if err != nil {
			return evidenceNone, err
		}
		for _, name := range strings.FieldsFunc(names, func(r rune) bool { return r == '+' || r == ',' }) {
			if !defined[name] {
				return evidenceNone, fmt.Errorf("%s does not define %s", path, name)
			}
		}
		return evidenceTest, nil
	}
	return r.resolveBare(p)
}

// splitRemark splits a trailing bracketed remark off a pointer.
//
// The evidence is written in Chinese as often as in English, so BOTH
// bracket styles are recognised: "(…)" and "（…）".
//
// Only a remark that opens at the END of the pointer is taken: the text
// before it must still be a pointer on its own. A remark that opens
// anywhere else stays part of the pointer, and then the pointer simply
// does not resolve — which is the honest answer.
func splitRemark(p string) (string, string) {
	for _, pair := range [][2]string{{"(", ")"}, {"（", "）"}} {
		open, close := pair[0], pair[1]
		i := strings.LastIndex(p, open)
		if i <= 0 || !strings.HasSuffix(p, close) {
			continue
		}
		head := p[:i]
		if head == "" {
			return p, ""
		}
		return head, p[i+len(open) : len(p)-len(close)]
	}
	return p, ""
}

// resolveBare handles the pointers that name no file: a bare test function
// name, or a bare file path.
func (r *resolver) resolveBare(p string) (evidenceKind, error) {
	if identRe.MatchString(p) {
		if _, ok := r.tests[p]; ok {
			return evidenceTest, nil
		}
		if err := r.checkPath(p); err != nil {
			return evidenceNone, fmt.Errorf("%q is not a test function of any *_test.go in the repository and not a file of it", p)
		}
		if err := r.checkTracked(p); err != nil {
			return evidenceNone, err
		}
		return evidenceFile, nil
	}
	if err := r.checkPath(p); err != nil {
		return evidenceNone, fmt.Errorf("%q is not a file of the repository and not a test function of it (%v)", p, err)
	}
	if err := r.checkTracked(p); err != nil {
		return evidenceNone, err
	}
	return evidenceFile, nil
}

// splitOnce splits a pointer at its LAST colon: the path is everything
// before it, the rest everything after.
//
// ★ WHY THE LAST COLON: a path may itself contain a colon (a Windows
// drive letter, a URL), and the part after the colon is what says what
// kind of pointer this is — a line, a symbol, a subtest. Those shapes
// never contain a colon, so the last one is the separator.
//
// The known limit: a path that ends with something after a colon AND
// carries a colon in its own name ("docs/a:b.md:12") is split at the
// wrong colon. No such path exists in this repository, and the check
// that follows reports the path as missing rather than silently
// accepting the wrong file.
func splitOnce(p string) (string, string, bool) {
	i := strings.LastIndex(p, ":")
	if i <= 0 || i == len(p)-1 {
		return "", "", false
	}
	return p[:i], p[i+1:], true
}

// resolveLines checks a "path:12,34,56-60" pointer.
func (r *resolver) resolveLines(path, list string) (evidenceKind, error) {
	if err := r.checkPath(path); err != nil {
		return evidenceNone, err
	}
	if err := r.checkTracked(path); err != nil {
		return evidenceNone, err
	}
	n, err := countLines(filepath.Join(r.root, path))
	if err != nil {
		return evidenceNone, err
	}
	for _, item := range strings.Split(list, ",") {
		lo, hi := item, item
		if i := strings.Index(item, "-"); i >= 0 {
			lo, hi = item[:i], item[i+1:]
		}
		first, err := strconv.Atoi(lo)
		if err != nil {
			return evidenceNone, fmt.Errorf("%q is not a line number", lo)
		}
		last, err := strconv.Atoi(hi)
		if err != nil {
			return evidenceNone, fmt.Errorf("%q is not a line number", hi)
		}
		if first < 1 || last < first {
			return evidenceNone, fmt.Errorf("the line range %q is not a range inside the file", item)
		}
		if last > n {
			return evidenceNone, fmt.Errorf("%s has %d lines, so line %d does not exist", path, n, last)
		}
	}
	return evidenceFileLine, nil
}

// resolveSubtest checks a "path:TestX/sub" pointer: the parent function has
// to exist in that file, and the subtest has to be named in it.
func (r *resolver) resolveSubtest(path, parent, sub string) (evidenceKind, error) {
	if err := r.checkPath(path); err != nil {
		return evidenceNone, err
	}
	if err := r.checkTracked(path); err != nil {
		return evidenceNone, err
	}
	defined, err := r.symbolsIn(path)
	if err != nil {
		return evidenceNone, err
	}
	if !defined[parent] {
		return evidenceNone, fmt.Errorf("%s does not define %s", path, parent)
	}
	found, err := r.subtestIn(path, parent, sub)
	if err != nil {
		return evidenceNone, err
	}
	if !found {
		return evidenceNone, fmt.Errorf("%s has no subtest %q inside %s", path, sub, parent)
	}
	return evidenceTest, nil
}

// subtestIn reports whether the file runs a subtest of that name inside the
// given test function.
//
// The name is looked for as a t.Run argument, because that is how a Go
// subtest is declared, and the parent's own body is the region searched —
// so a name used by ANOTHER test does not count.
//
// ★ GO REWRITES THE SPACES OF A SUBTEST NAME. "quota exhausted" runs as
// "quota_exhausted", so a pointer written with underscores names the same
// subtest as one written with spaces. Both are accepted, because both are
// the same test.
func (r *resolver) subtestIn(path, parent, sub string) (bool, error) {
	wanted := []string{`"` + sub + `"`}
	if withSpaces := strings.ReplaceAll(sub, "_", " "); withSpaces != sub {
		wanted = append(wanted, `"`+withSpaces+`"`)
	}
	f, err := os.Open(filepath.Join(r.root, path))
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	inParent := false
	for sc.Scan() {
		line := sc.Text()
		if m := symbolDefRe.FindStringSubmatch(line); m != nil {
			if m[2] == parent {
				inParent = true
				continue
			}
			if inParent {
				// The next function starts: the parent's body ended.
				return false, nil
			}
			continue
		}
		if !inParent || !strings.Contains(line, "t.Run(") {
			continue
		}
		for _, w := range wanted {
			if strings.Contains(line, w) {
				return true, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// symbolsIn returns the names defined by the Go file at the repository
// relative path.
//
// A pointer that names a file AND a symbol is stronger than one that names
// only the symbol: it says the symbol is in THAT file. So the symbols are
// read from the file itself, not from the repository-wide index.
func (r *resolver) symbolsIn(path string) (map[string]bool, error) {
	f, err := os.Open(filepath.Join(r.root, path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if m := symbolDefRe.FindStringSubmatch(sc.Text()); m != nil {
			names[m[2]] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return names, nil
}

// checkPath reports whether p names a file of the repository.
func (r *resolver) checkPath(p string) error {
	if p == "" {
		return errors.New("the path is empty")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("%q is an absolute path: write it relative to the repository root", p)
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return fmt.Errorf("%q leaves the repository", p)
		}
	}
	if clean := filepath.Clean(p); clean != p {
		return fmt.Errorf("%q is not a canonical path: write it as %q", p, filepath.ToSlash(clean))
	}
	info, err := os.Stat(filepath.Join(r.root, p))
	if err != nil {
		return fmt.Errorf("%q does not exist in the repository", p)
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory, not a file", p)
	}
	return nil
}

// ★ A POINTER MUST POINT AT A FILE THE REPOSITORY CARRIES.
//
// The evidence is read by people who clone the repository, and by a CI
// runner that checks out a clean tree. A pointer at a file that only
// exists in one working copy — an untracked scratch file, a path that
// .gitignore hides — resolves here and does NOT resolve there.
//
// That failure points the wrong way: the local run is green and the CI
// run is red, and the red one looks like a defect in the product. So a
// pointer at a file the repository does not carry is refused here.
func (r *resolver) checkTracked(p string) error {
	out, err := exec.Command("git", "-C", r.root, "ls-files", "--error-unmatch", "--", p).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%q is not tracked by git (git says: %s); a pointer must name a file the repository carries", p, strings.TrimSpace(string(out)))
	}
	return nil
}

// countLines counts the lines of a file without holding it in memory.
func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	n := 0
	last := byte('\n')
	for {
		read, err := f.Read(buf)
		for _, b := range buf[:read] {
			if b == '\n' {
				n++
			}
		}
		if read > 0 {
			last = buf[read-1]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if last != '\n' {
		n++
	}
	return n, nil
}
