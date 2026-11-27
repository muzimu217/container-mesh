// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

// The CI guard of the acceptance ledger.
//
// ★ THE GUARD MUST BE ABLE TO FAIL. A test that cannot fail is a
// declaration, not a test. So the rules are checked twice: once against the
// real ledger (which must pass), and once against a copy of it that is
// broken in one specific way (which must NOT pass, and must say which row
// is broken).
//
// The parsing and the validation are the ones the command uses — the same
// functions, the same messages. A rule cannot be enforced by the command
// and skipped by the test.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// repoRoot returns the repository root, derived from this file's path.
//
// This file lives at <root>/tools/acceptance/, so the root is two levels
// up. The lookup itself walks up to the directory that holds both a go.mod
// and the acceptance document, so a package that moves breaks loudly
// instead of resolving the evidence against the wrong tree.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := findRepoRoot("")
	if err != nil {
		t.Fatalf("the repository root was not found: %v", err)
	}
	if !fileExists(filepath.Join(root, docRelPath)) {
		t.Fatalf("the repository root %s does not hold %s", root, docRelPath)
	}
	return root
}

// readDoc reads the acceptance document of the repository.
func readDoc(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), docRelPath)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ledgerRowLines returns the data rows of the ledger, in order.
func ledgerRowLines(t *testing.T, text string) []string {
	t.Helper()
	lines := strings.Split(text, "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case ledgerBegin:
			begin = i
		case ledgerEnd:
			end = i
		}
	}
	if begin < 0 || end < 0 {
		t.Fatal("the document has no ledger markers")
	}
	return lines[begin+3 : end]
}

// setLedgerRows replaces the data rows of the ledger.
func setLedgerRows(t *testing.T, text string, rows []string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case ledgerBegin:
			begin = i
		case ledgerEnd:
			end = i
		}
	}
	if begin < 0 || end < 0 {
		t.Fatal("the document has no ledger markers")
	}
	out := append([]string{}, lines[:begin+3]...)
	out = append(out, rows...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}

// rowLine returns the line of the ledger row with that cell id.
//
// Only the rows of the ledger count: the method tables hold rows with the
// same first cell, and a message about the ledger must not point at one of
// those.
func rowLine(t *testing.T, text, id string) int {
	t.Helper()
	lines := strings.Split(text, "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case ledgerBegin:
			begin = i
		case ledgerEnd:
			end = i
		}
	}
	if begin < 0 || end < 0 {
		t.Fatal("the document has no ledger markers")
	}
	for i := begin + 3; i < end; i++ {
		cells := splitRow(lines[i])
		if len(cells) > 0 && cells[0] == id {
			return i + 1
		}
	}
	t.Fatalf("the ledger has no row for %s", id)
	return 0
}

// rowLineN returns the line of the n-th ledger row with that cell id.
func rowLineN(t *testing.T, text, id string, n int) int {
	t.Helper()
	lines := strings.Split(text, "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case ledgerBegin:
			begin = i
		case ledgerEnd:
			end = i
		}
	}
	if begin < 0 || end < 0 {
		t.Fatal("the document has no ledger markers")
	}
	seen := 0
	for i := begin + 3; i < end; i++ {
		cells := splitRow(lines[i])
		if len(cells) > 0 && cells[0] == id {
			seen++
			if seen == n {
				return i + 1
			}
		}
	}
	t.Fatalf("the ledger has no %d-th row for %s", n, id)
	return 0
}

// messages turns violations into the lines the command would print.
func messages(violations []violation) []string {
	out := make([]string, 0, len(violations))
	for _, v := range violations {
		out = append(out, v.String())
	}
	return out
}

// renderRow renders cells as a ledger row. The renderer lives in the
// production code: the test and the command must write the same shape.

// mutateRow replaces one row of the ledger, by cell id.
func mutateRow(t *testing.T, text, id string, fn func(cells []string) []string) string {
	t.Helper()
	rows := ledgerRowLines(t, text)
	found := false
	for i, line := range rows {
		cells := splitRow(line)
		if len(cells) == 0 || cells[0] != id {
			continue
		}
		rows[i] = renderRow(fn(cells))
		found = true
		break
	}
	if !found {
		t.Fatalf("the ledger has no row for %s", id)
	}
	return setLedgerRows(t, text, rows)
}

// violationsOf runs the validation the command runs, over a document text,
// and returns the messages it would print.
func violationsOf(t *testing.T, text string) []string {
	t.Helper()
	doc, err := parseDocument(text, "M4-ACCEPTANCE.md", "M4-ACCEPTANCE.md")
	if err != nil {
		return []string{err.Error()}
	}
	ev, err := newResolver(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	_, violations := doc.validate(ev)
	out := make([]string, 0, len(violations))
	for _, v := range violations {
		out = append(out, v.String())
	}
	return out
}

// TestTheRealLedgerPassesTheCheck: the ledger in the repository must hold
// every rule.
//
// The guard also refuses to run on an empty ledger: a check that finds no
// row has checked nothing, and must not report success.
func TestTheRealLedgerPassesTheCheck(t *testing.T) {
	text := readDoc(t)

	rows := ledgerRowLines(t, text)
	if len(rows) == 0 {
		t.Fatal("no ledger row was found: the guard would pass on any document")
	}
	if len(rows) != wantCells {
		t.Fatalf("the ledger has %d rows, want %d", len(rows), wantCells)
	}

	violations := violationsOf(t, text)
	if len(violations) > 0 {
		t.Fatalf("the ledger of the repository breaks %d rule(s):\n%s", len(violations), strings.Join(violations, "\n"))
	}

	// The command itself must say so, and exit 0.
	var stdout, stderr strings.Builder
	if code := run([]string{"check"}, &stdout, &stderr); code != 0 {
		t.Fatalf("check exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "0 problems") {
		t.Fatalf("check did not report the ledger as sound: %q", stdout.String())
	}
}

// TestTheMethodTablesHold52Cells: the grid is 52 cells, and the method
// tables are what says so.
//
// The distribution is written down in docs/PLAN.md and docs/ROUND-LOG.md;
// a grid that silently changed shape would make every number on the board
// meaningless.
func TestTheMethodTablesHold52Cells(t *testing.T) {
	doc, err := parseDocument(readDoc(t), "M4-ACCEPTANCE.md", "M4-ACCEPTANCE.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.method) != wantCells {
		t.Fatalf("the method tables hold %d cells, want %d", len(doc.method), wantCells)
	}

	want := map[string]int{"A": 11, "B": 10, "C": 8, "D": 6, "E": 7, "F": 10}
	got := map[string]int{}
	for _, m := range doc.method {
		got[m.id[:1]]++
	}
	for group, n := range want {
		if got[group] != n {
			t.Errorf("group %s holds %d cells, want %d", group, got[group], n)
		}
	}

	// The cells, in the order of the method tables.
	wantOrder := []string{}
	for _, group := range []string{"A", "B", "C", "D", "E", "F"} {
		for i := 1; i <= want[group]; i++ {
			wantOrder = append(wantOrder, fmt.Sprintf("%s%d", group, i))
		}
	}
	for i, m := range doc.method {
		if m.id != wantOrder[i] {
			t.Fatalf("cell %d of the method tables is %s, want %s", i+1, m.id, wantOrder[i])
		}
	}
}

// TestTheCheckFailsOnEveryBrokenRule: a copy of the real ledger, broken
// in one specific way each time, must be refused — and the refusal must
// name the row.
//
// Every case here is a rule the ledger has; a rule without a case is a
// rule nobody checked.
func TestTheCheckFailsOnEveryBrokenRule(t *testing.T) {
	base := readDoc(t)

	cases := []struct {
		name   string
		id     string // the row the message must point at
		line   func(t *testing.T, text string) int
		want   string // the fragment the message must contain
		mutate func(t *testing.T, text string) string
	}{
		{
			name: "51 rows",
			want: "the ledger has 51 rows, want 52",
			mutate: func(t *testing.T, text string) string {
				rows := ledgerRowLines(t, text)
				return setLedgerRows(t, text, rows[:len(rows)-1])
			},
		},
		{
			name: "53 rows",
			id:   "F10",
			line: func(t *testing.T, text string) int { return rowLineN(t, text, "F10", 2) },
			want: "the ledger has 53 rows, want 52",
			mutate: func(t *testing.T, text string) string {
				rows := ledgerRowLines(t, text)
				return setLedgerRows(t, text, append(rows, rows[len(rows)-1]))
			},
		},
		{
			name: "an unknown cell id",
			id:   "A12",
			want: "is not one of the 52 cells of the method tables",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A1", func(cells []string) []string {
					cells[0] = "A12"
					return cells
				})
			},
		},
		{
			name: "a cell id that is not one at all",
			id:   "G9",
			want: "is not a cell id",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A1", func(cells []string) []string {
					cells[0] = "G9"
					return cells
				})
			},
		},
		{
			name: "a duplicated cell id",
			id:   "A1",
			line: func(t *testing.T, text string) int { return rowLineN(t, text, "A1", 2) },
			want: "appears twice",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A2", func(cells []string) []string {
					cells[0] = "A1"
					return cells
				})
			},
		},
		{
			name: "a ✔ row without a SHA",
			id:   "A1",
			want: "a ✔ row needs the tested SHA",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A1", func(cells []string) []string {
					cells[1] = markPass
					cells[2] = ""
					cells[3] = "TestTheRealLedgerPassesTheCheck"
					cells[4] = "CI"
					cells[5] = "2026-11-26"
					return cells
				})
			},
		},
		{
			name: "a ✔ row without a date",
			id:   "A1",
			want: "a ✔ row needs the date",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A1", func(cells []string) []string {
					cells[1] = markPass
					cells[2] = "c38cdd3775c2441c2cfcc33ab6f24dfb8d879e6c"
					cells[3] = "TestTheRealLedgerPassesTheCheck"
					cells[4] = "CI"
					cells[5] = ""
					return cells
				})
			},
		},
		{
			name: "a ✔ row without a tester",
			id:   "A1",
			want: "a ✔ row needs the tester",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A1", func(cells []string) []string {
					cells[1] = markPass
					cells[2] = "c38cdd3775c2441c2cfcc33ab6f24dfb8d879e6c"
					cells[3] = "TestTheRealLedgerPassesTheCheck"
					cells[4] = ""
					cells[5] = "2026-11-26"
					return cells
				})
			},
		},
		{
			name: "a ✔ row without evidence",
			id:   "A1",
			want: "a ✔ row needs at least one evidence pointer",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A1", func(cells []string) []string {
					cells[1] = markPass
					cells[2] = "c38cdd3775c2441c2cfcc33ab6f24dfb8d879e6c"
					cells[3] = ""
					cells[4] = "CI"
					cells[5] = "2026-11-26"
					return cells
				})
			},
		},
		{
			name: "a ✔ row whose evidence resolves to nothing",
			id:   "A1",
			want: "does not resolve",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A1", func(cells []string) []string {
					cells[1] = markPass
					cells[2] = "c38cdd3775c2441c2cfcc33ab6f24dfb8d879e6c"
					cells[3] = "TestNoSuchFunctionInThisRepository"
					cells[4] = "CI"
					cells[5] = "2026-11-26"
					return cells
				})
			},
		},
		{
			name: "a date that is not a date",
			id:   "A1",
			want: "is not a YYYY-MM-DD date",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A1", func(cells []string) []string {
					cells[1] = markPass
					cells[2] = "c38cdd3775c2441c2cfcc33ab6f24dfb8d879e6c"
					cells[3] = "TestTheRealLedgerPassesTheCheck"
					cells[4] = "CI"
					cells[5] = "2026-13-45"
					return cells
				})
			},
		},
		{
			name: "the state 不适用",
			id:   "A1",
			want: "is not accepted",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A1", func(cells []string) []string {
					cells[1] = "不适用"
					return cells
				})
			},
		},
		{
			name: "a ◐ without a note",
			id:   "A1",
			want: "needs a note",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A1", func(cells []string) []string {
					cells[1] = markPart
					return cells
				})
			},
		},
		{
			name: "an empty state",
			id:   "A1",
			want: "the state is empty",
			mutate: func(t *testing.T, text string) string {
				return mutateRow(t, text, "A1", func(cells []string) []string {
					cells[1] = ""
					return cells
				})
			},
		},
		{
			name: "rows out of order",
			id:   "A2",
			want: "is out of order",
			mutate: func(t *testing.T, text string) string {
				rows := ledgerRowLines(t, text)
				rows[0], rows[1] = rows[1], rows[0]
				return setLedgerRows(t, text, rows)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := tc.mutate(t, base)
			msgs := violationsOf(t, mutated)
			if len(msgs) == 0 {
				t.Fatalf("the broken ledger was accepted: a check that cannot fail is not a check")
			}
			joined := strings.Join(msgs, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("the broken ledger was refused, but not for this reason (%q):\n%s", tc.want, joined)
			}
			if tc.id != "" {
				line := 0
				if tc.line != nil {
					line = tc.line(t, mutated)
				} else {
					line = rowLine(t, mutated, tc.id)
				}
				ref := fmt.Sprintf(":%d: cell %s:", line, tc.id)
				if !strings.Contains(joined, ref) {
					t.Fatalf("the broken ledger was refused, but the message does not point at line %d of %s (%q):\n%s", line, tc.id, ref, joined)
				}
			}
		})
	}
}

// TestTheCommandFailsOnABrokenCopy: the command itself must exit non-zero
// on a broken copy — the way a reader of the ledger finds out.
func TestTheCommandFailsOnABrokenCopy(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "M4-ACCEPTANCE.md")
	text := mutateRow(t, readDoc(t), "A1", func(cells []string) []string {
		cells[1] = "不适用"
		return cells
	})
	if err := os.WriteFile(broken, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := run([]string{"check", "--doc", broken}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("check exited %d on a broken copy, want 1\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "不适用") {
		t.Fatalf("the failure does not name the broken state:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), ": cell A1:") {
		t.Fatalf("the failure does not point at the broken row:\n%s", stderr.String())
	}
}

// TestApplyMergesThreeRows: a TSV of three rows changes three rows. The
// other 49 keep their line, and the order of the table does not move.
func TestApplyMergesThreeRows(t *testing.T) {
	base := readDoc(t)
	dir := t.TempDir()
	docPath := filepath.Join(dir, "M4-ACCEPTANCE.md")
	if err := os.WriteFile(docPath, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	tsv := filepath.Join(dir, "three.tsv")
	if err := os.WriteFile(tsv, []byte(
		"id\tmark\tnote\tsha\tevidence\ttester\tdate\n"+
			"A1\t✔\t\tc38cdd3775c2441c2cfcc33ab6f24dfb8d879e6c\tTestTheRealLedgerPassesTheCheck\tCI\t2026-11-26\n"+
			"B2\t◐\t缺中转复测\t\t\t\t\n"+
			"C3\t✘\t撤销后新握手仍被接受\t\t\t\t\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	if code := run([]string{"apply", "--doc", docPath, tsv}, &stdout, &stderr); code != 0 {
		t.Fatalf("apply exited %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}

	after, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	afterRows := ledgerRowLines(t, string(after))
	baseRows := ledgerRowLines(t, base)
	if len(afterRows) != len(baseRows) {
		t.Fatalf("the ledger has %d rows after the merge, want %d", len(afterRows), len(baseRows))
	}

	changed := map[string]bool{"A1": true, "B2": true, "C3": true}
	for i, line := range afterRows {
		cells := splitRow(line)
		if len(cells) == 0 {
			t.Fatalf("row %d is not a table row: %q", i+1, line)
		}
		id := cells[0]
		if !changed[id] {
			if line != baseRows[i] {
				t.Errorf("row %s changed although the TSV did not mention it:\n got %q\nwant %q", id, line, baseRows[i])
			}
			continue
		}
		want := map[string]string{
			"A1": "| A1 | ✔ | c38cdd3775c2441c2cfcc33ab6f24dfb8d879e6c | TestTheRealLedgerPassesTheCheck | CI | 2026-11-26 |",
			"B2": "| B2 | ◐ — 缺中转复测 | | | | |",
			"C3": "| C3 | ✘ — 撤销后新握手仍被接受 | | | | |",
		}[id]
		if line != want {
			t.Errorf("row %s is %q, want %q", id, line, want)
		}
	}

	// The order of the table did not move.
	for i, line := range afterRows {
		cells := splitRow(line)
		if cells[0] != splitRow(baseRows[i])[0] {
			t.Fatalf("row %d is %s, want %s: the merge moved the order", i+1, cells[0], splitRow(baseRows[i])[0])
		}
	}

	// Applying the same TSV again changes nothing.
	if err := os.WriteFile(tsv, []byte(
		"id\tmark\tnote\tsha\tevidence\ttester\tdate\n"+
			"A1\t✔\t\tc38cdd3775c2441c2cfcc33ab6f24dfb8d879e6c\tTestTheRealLedgerPassesTheCheck\tCI\t2026-11-26\n"+
			"B2\t◐\t缺中转复测\t\t\t\t\n"+
			"C3\t✘\t撤销后新握手仍被接受\t\t\t\t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"apply", "--doc", docPath, tsv}, &stdout, &stderr); code != 0 {
		t.Fatalf("the second apply exited %d\nstderr: %s", code, stderr.String())
	}
	again, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(after) {
		t.Fatal("applying the same TSV twice changed the document: the merge is not idempotent")
	}
}

// TestApplyRefusesToWriteABrokenLedger: a merge that breaks a rule must
// not reach the file.
func TestApplyRefusesToWriteABrokenLedger(t *testing.T) {
	base := readDoc(t)
	dir := t.TempDir()
	docPath := filepath.Join(dir, "M4-ACCEPTANCE.md")
	if err := os.WriteFile(docPath, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	tsv := filepath.Join(dir, "broken.tsv")
	// A ✔ row without a SHA: the result breaks a rule.
	if err := os.WriteFile(tsv, []byte(
		"id\tmark\tnote\tsha\tevidence\ttester\tdate\n"+
			"A1\t✔\t\t\tTestTheRealLedgerPassesTheCheck\tCI\t2026-11-26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	if code := run([]string{"apply", "--doc", docPath, tsv}, &stdout, &stderr); code != 1 {
		t.Fatalf("apply exited %d on a broken merge, want 1\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	after, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != base {
		t.Fatal("a broken merge reached the document")
	}
}

// TestTheCountsAgreeWithTheLedger: the counts are the numbers other
// documents show, so their FORMAT is fixed — but their VALUES are not.
//
// ★ A SNAPSHOT IS NOT A CONTRACT. The first version of this test wrote the
// counts of the empty ledger down as a literal ("total ✔=0 … 未验=52"), and
// the first real ledger filled in broke it. A snapshot of the DATA is not a
// rule; a snapshot of the SHAPE is. So what is asserted here is:
//
//   - the format: one line per group, then the total, then the guarded
//     count, each in the documented shape;
//   - the consistency: every number is the count of the rows of the
//     ledger, and the four numbers of the total add up to 52.
func TestTheCountsAgreeWithTheLedger(t *testing.T) {
	doc, err := parseDocument(readDoc(t), "M4-ACCEPTANCE.md", "M4-ACCEPTANCE.md")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := newResolver(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	cells, violations := doc.validate(ev)
	if len(violations) > 0 {
		t.Fatalf("the ledger breaks %d rule(s):\n%s", len(violations), strings.Join(messages(violations), "\n"))
	}
	if len(cells) != wantCells {
		t.Fatalf("the ledger holds %d cells, want %d", len(cells), wantCells)
	}

	out := renderCounts(cells)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 8 {
		t.Fatalf("the counts hold %d lines, want 8 (six groups, the total, the guarded count):\n%s", len(lines), out)
	}

	// The counts of the rows, counted by hand.
	wantByGroup := map[string]map[string]int{}
	wantTotal := map[string]int{}
	wantGuarded := 0
	for _, c := range cells {
		g := c.id[:1]
		if wantByGroup[g] == nil {
			wantByGroup[g] = map[string]int{}
		}
		wantByGroup[g][c.mark]++
		wantTotal[c.mark]++
		if c.mark == markPass && c.guarded {
			wantGuarded++
		}
	}

	groupRe := regexp.MustCompile(`^([A-F]) ✔=(\d+) ◐=(\d+) ✘=(\d+) 未验=(\d+)$`)
	seenGroups := 0
	for _, line := range lines[:6] {
		m := groupRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("the group line %q is not in the documented shape", line)
		}
		seenGroups++
		g := m[1]
		want := wantByGroup[g]
		for i, mark := range []string{markPass, markPart, markFail, markNone} {
			got, err := strconv.Atoi(m[i+2])
			if err != nil {
				t.Fatal(err)
			}
			if got != want[mark] {
				t.Errorf("group %s: the counts say %s=%d, the rows say %d", g, mark, got, want[mark])
			}
		}
	}
	if seenGroups != 6 {
		t.Fatalf("the counts hold %d group lines, want 6", seenGroups)
	}

	totalRe := regexp.MustCompile(`^total ✔=(\d+) ◐=(\d+) ✘=(\d+) 未验=(\d+)$`)
	m := totalRe.FindStringSubmatch(lines[6])
	if m == nil {
		t.Fatalf("the total line %q is not in the documented shape", lines[6])
	}
	total := 0
	for i, mark := range []string{markPass, markPart, markFail, markNone} {
		got, err := strconv.Atoi(m[i+1])
		if err != nil {
			t.Fatal(err)
		}
		if got != wantTotal[mark] {
			t.Errorf("the total says %s=%d, the rows say %d", mark, got, wantTotal[mark])
		}
		total += got
	}
	if total != wantCells {
		t.Errorf("the four numbers of the total add up to %d, want %d", total, wantCells)
	}

	if lines[7] != fmt.Sprintf("guarded %d", wantGuarded) {
		t.Errorf("the guarded count is %q, want %d", lines[7], wantGuarded)
	}
}

// TestTheBoardIsGreenOnlyWhenEverythingPassed: the passing colour is
// earned. A board that paints 51 of 52 green says "done" about a grid that
// is not done.
//
// ★ THE PROPERTY, NOT A SNAPSHOT. The first version of this test built an
// incomplete ledger by rewriting every row but one — which stopped working
// the moment the ledger held real data (the rows it rewrote were no longer
// in a state it could rewrite). The property is:
//
//	not every cell passed  ⇒  the board carries no "ok" class
//	every cell passed      ⇒  the board carries the "ok" class
//
// So the test builds both states itself, whatever the ledger holds.
func TestTheBoardIsGreenOnlyWhenEverythingPassed(t *testing.T) {
	base := readDoc(t)

	// Every cell passed.
	rows := ledgerRowLines(t, base)
	for i, line := range rows {
		cells := splitRow(line)
		cells[1] = markPass
		cells[2] = "c38cdd3775c2441c2cfcc33ab6f24dfb8d879e6c"
		cells[3] = "TestTheRealLedgerPassesTheCheck"
		cells[4] = "CI"
		cells[5] = "2026-11-26"
		rows[i] = renderRow(cells)
	}
	complete := setLedgerRows(t, base, rows)
	if violations := violationsOf(t, complete); len(violations) > 0 {
		t.Fatalf("the complete ledger is not accepted:\n%s", strings.Join(violations, "\n"))
	}
	doc, err := parseDocument(complete, "M4-ACCEPTANCE.md", "M4-ACCEPTANCE.md")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := newResolver(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	cells, violations := doc.validate(ev)
	if len(violations) > 0 {
		t.Fatalf("the complete ledger is not accepted:\n%s", strings.Join(messages(violations), "\n"))
	}
	board := renderDashboard(cells, doc.titles)
	if !okClassRe.MatchString(board) {
		t.Fatal("the board of a complete ledger does not use the passing colour")
	}
	if !strings.Contains(board, "✔ 52 / 52") {
		t.Fatalf("the board of a complete ledger does not show the progress:\n%s", board)
	}

	// ★ ONE CELL SHORT OF EVERYTHING. Only the FIRST row is changed, so the
	// test does not depend on what the other rows happen to hold.
	//
	// The number the board shows is counted by the test, not written down:
	// the ledger holds real data now, so "51" would be a snapshot of a
	// moment that has passed.
	for _, mark := range []string{markPart, markFail, markNone} {
		rows = ledgerRowLines(t, base)
		rowCells := splitRow(rows[0])
		rowCells[1] = mark + " " + stateSep + " 少一格"
		rows[0] = renderRow(rowCells)
		partial := setLedgerRows(t, base, rows)
		doc, err = parseDocument(partial, "M4-ACCEPTANCE.md", "M4-ACCEPTANCE.md")
		if err != nil {
			t.Fatal(err)
		}
		cells, violations = doc.validate(ev)
		if len(violations) > 0 {
			t.Fatalf("the almost complete ledger is not accepted:\n%s", strings.Join(messages(violations), "\n"))
		}
		board = renderDashboard(cells, doc.titles)
		if okClassRe.MatchString(board) {
			t.Fatalf("the board of a ledger with one %s cell uses the passing colour:\n%s", mark, board)
		}

		// The board shows the progress it counted: the number of passed
		// cells over 52.
		passed := 0
		for _, c := range cells {
			if c.mark == markPass {
				passed++
			}
		}
		want := fmt.Sprintf("✔ %d / %d", passed, wantCells)
		if !strings.Contains(board, want) {
			t.Fatalf("the board of a ledger with one %s cell shows %q, want %q:\n%s", mark, board, want, board)
		}
	}
}

// TestTheBoardShowsTheLedger: the board must show what the tool prints.
//
// The board does not carry the fragment yet (that is M4-6), so the test
// says so and skips rather than passing on nothing.
func TestTheBoardShowsTheLedger(t *testing.T) {
	boardPath := filepath.Join(repoRoot(t), "docs", "project-dashboard.html")
	raw, err := os.ReadFile(boardPath)
	if err != nil {
		t.Skipf("the board %s cannot be read: %v", boardPath, err)
	}
	inner, ok := extractMarked(string(raw))
	if !ok {
		t.Skip("the board has no <!-- ledger:begin --> / <!-- ledger:end --> pair yet: " +
			"the board does not carry the ledger fragment, so there is nothing to compare")
	}

	doc, err := parseDocument(readDoc(t), "M4-ACCEPTANCE.md", "M4-ACCEPTANCE.md")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := newResolver(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	cells, violations := doc.validate(ev)
	if len(violations) > 0 {
		t.Fatalf("the ledger breaks %d rule(s):\n%s", len(violations), strings.Join(messages(violations), "\n"))
	}
	want := renderDashboard(cells, doc.titles)
	if inner != want {
		t.Fatalf("the board and the tool disagree about the ledger.\n"+
			"the board holds %d bytes, the tool prints %d; the first difference is at line %d\n"+
			"run `go run ./tools/acceptance dashboard` and paste its output between the markers",
			len(inner), len(want), firstDifferentLine(inner, want))
	}
}

// firstDifferentLine returns the number of the first line where two texts
// differ.
func firstDifferentLine(a, b string) int {
	la := strings.Split(a, "\n")
	lb := strings.Split(b, "\n")
	for i := 0; i < len(la) && i < len(lb); i++ {
		if la[i] != lb[i] {
			return i + 1
		}
	}
	if len(la) != len(lb) {
		return len(la) + 1
	}
	return 0
}

// TestEvidencePointersResolve: the shapes an evidence pointer may have, and
// the ones that must be refused.
//
// ★ EVERY FORM HAS ITS NEGATIVE CASE. A form that is only ever tested in
// the positive direction is a rule nobody checked: the guard would pass on a
// resolver that accepts everything.
func TestEvidencePointersResolve(t *testing.T) {
	ev, err := newResolver(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}

	ok := []struct {
		pointer string
		kind    evidenceKind
	}{
		// A bare test function, and a bare file.
		{"TestTheRealLedgerPassesTheCheck", evidenceTest},
		{"TestTheDocumentedCommandLinesParse", evidenceTest},
		{"docs/design/M4-ACCEPTANCE.md", evidenceFile},
		{"LICENSE", evidenceFile},

		// "path:line" — one line, several lines, a range.
		{"docs/design/M4-ACCEPTANCE.md:1", evidenceFileLine},
		{"internal/cli/doc_examples_test.go:18", evidenceFileLine},
		{"docs/PLAN.md:26,75", evidenceFileLine},
		{"docs/ROUND-LOG.md:1429-1433", evidenceFileLine},

		// "path:Symbol" — the symbol has to be IN that file.
		{"internal/cli/mesh_test.go:TestCLIInitCreatesIdentity", evidenceTest},
		{"internal/cli/cred_test.go:TestAccountShowAcceptsBothPassphraseFlags", evidenceTest},

		// Several symbols of one file, separated by commas or by plus signs.
		{"internal/cli/cred_status_test.go:TestAnExpiredButNotRevokedCredentialSaysExpired,TestTheJoinerAsksTheAnchorWhyItCannotEnrol", evidenceTest},
		{"internal/cli/m4acceptance_runtime_test.go:joinMember+waitForRelayReachable", evidenceTest},

		// A subtest of a test. Go rewrites the spaces of a subtest name, so
		// "quota exhausted" and "quota_exhausted" name the same subtest.
		{"internal/cli/enroll_test.go:TestEnrollRefusesBadRequests/quota_exhausted", evidenceTest},
		{"internal/cli/enroll_test.go:TestEnrollRefusesBadRequests/quota exhausted", evidenceTest},

		// A remark next to the pointer: the remark is for the reader, the
		// pointer is what has to resolve.
		{"internal/cli/discovery_test.go:TestMeshBootstrapApprovalFlow(ping 段)", evidenceTest},
		{"internal/cli/discovery.go:1062-1134（join 的旗标接线）", evidenceFileLine},
	}
	for _, tc := range ok {
		kind, err := ev.resolve(tc.pointer)
		if err != nil {
			t.Errorf("the pointer %q does not resolve: %v", tc.pointer, err)
			continue
		}
		if kind != tc.kind {
			t.Errorf("the pointer %q is a %v, want %v", tc.pointer, kind, tc.kind)
		}
	}

	bad := []struct {
		pointer string
		why     string
	}{
		{"TestNoSuchFunctionInThisRepository", "a test that does not exist"},
		{"repoRoot", "a helper, not a test `go test` runs"},
		{"docs/design/M4-ACCEPTANCE.md:100000", "more lines than the file has"},
		{"docs/design/M4-ACCEPTANCE.md:0", "line numbers start at 1"},
		{"docs/design/M4-ACCEPTANCE.md:1-100000", "the end of the range is past the file"},
		{"does/not/exist.md", "a file that does not exist"},
		{"/etc/passwd", "outside the repository"},
		{"../outside.md", "outside the repository"},
		{"./docs/PLAN.md", "not the canonical path"},
		{"", "an empty pointer"},

		// ★ The symbol has to be IN the file the pointer names. The symbol
		// exists — in another file. A pointer that names the wrong file
		// is a claim about the wrong witness.
		{"internal/cli/cred_test.go:TestCLIInitCreatesIdentity", "the symbol is in another file"},
		{"internal/cli/mesh_test.go:TestNoSuchSymbolAnywhere", "the symbol does not exist"},
		{"docs/PLAN.md:TestCLIInitCreatesIdentity", "the file holds no Go symbol"},
		{"internal/cli/cred_status_test.go:TestAnExpiredButNotRevokedCredentialSaysExpired,TestNoSuchSymbol", "one of the symbols does not exist"},

		// A subtest that does not exist, and a parent that does not exist.
		{"internal/cli/enroll_test.go:TestEnrollRefusesBadRequests/no_such_subtest", "the subtest does not exist"},
		{"internal/cli/enroll_test.go:TestNoSuchParent/no_such_subtest", "the parent test does not exist"},
	}
	for _, tc := range bad {
		if kind, err := ev.resolve(tc.pointer); err == nil {
			t.Errorf("the pointer %q was accepted as a %v, but %s", tc.pointer, kind, tc.why)
		}
	}
}

// TestAnUntrackedFileIsNotEvidence: a pointer at a file the repository does
// not carry is refused.
//
// ★ WHY THIS RULE EXISTS. The evidence is read by people who clone the
// repository, and by a CI runner that checks out a clean tree. A pointer at
// a file that only exists in one working copy resolves here and does NOT
// resolve there — and that failure points the wrong way: the local run is
// green and the CI run is red, and the red one looks like a defect in the
// product.
//
// So the rule is checked with a file that really is untracked: the test
// writes one, and the resolver has to refuse it.
func TestAnUntrackedFileIsNotEvidence(t *testing.T) {
	root := repoRoot(t)
	ev, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}

	// A file that is in the repository but NOT tracked: the tool's own
	// scratch copy of the acceptance document.
	untracked := filepath.Join(root, "tools", "acceptance", "untracked-probe.md")
	if err := os.WriteFile(untracked, []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(untracked)

	rel := "tools/acceptance/untracked-probe.md"
	if kind, err := ev.resolve(rel); err == nil {
		t.Errorf("the pointer at the untracked file %q was accepted as a %v", rel, kind)
	}
	if kind, err := ev.resolve(rel + ":1"); err == nil {
		t.Errorf("the pointer at a line of the untracked file %q was accepted as a %v", rel, kind)
	}

	// The same file, tracked, is accepted: the rule is about the TRACKING,
	// not about the file.
	tracked := "docs/design/M4-ACCEPTANCE.md"
	if _, err := ev.resolve(tracked); err != nil {
		t.Errorf("the pointer at the tracked file %q was refused: %v", tracked, err)
	}
}

// TestTheSubtestNameIsAcceptedWithSpacesAndWithUnderscores: Go rewrites
// the spaces of a subtest name, so the two spellings name one subtest.
//
// The repository says so in its own source: the subtest is declared as
// t.Run("quota exhausted", …) and `go test` prints it as
// TestEnrollRefusesBadRequests/quota_exhausted. A pointer written either
// way names the same test, so both have to resolve.
func TestTheSubtestNameIsAcceptedWithSpacesAndWithUnderscores(t *testing.T) {
	ev, err := newResolver(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	const pointer = "internal/cli/enroll_test.go:TestEnrollRefusesBadRequests/"
	for _, name := range []string{"quota_exhausted", "quota exhausted"} {
		kind, err := ev.resolve(pointer + name)
		if err != nil {
			t.Errorf("the subtest pointer %q does not resolve: %v", pointer+name, err)
			continue
		}
		if kind != evidenceTest {
			t.Errorf("the subtest pointer %q is a %v, want %v", pointer+name, kind, evidenceTest)
		}
	}
}
