// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

// The result ledger of docs/design/M4-ACCEPTANCE.md.
//
// §2.1–§2.6 of that document name the acceptance cells; §10 records their
// results, one row per cell, between two markers:
//
//	<!-- ledger:begin -->
//	<!-- ledger:end -->
//
// Everything that reads or writes those rows lives in this file, so the
// command and the CI guard cannot disagree about what a valid row is.

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	ledgerBegin = "<!-- ledger:begin -->"
	ledgerEnd   = "<!-- ledger:end -->"

	// wantCells is the size of the acceptance grid. The method tables
	// decide WHICH cells exist; this number catches a grid that grew or
	// shrank without the ledger (and the document's own count) following.
	wantCells = 52

	markPass = "✔"
	markPart = "◐"
	markFail = "✘"
	markNone = "未验"

	// stateSep stands between a non-pass mark and its note.
	stateSep = "—"

	// testerCI is the tester name that marks a cell as guarded by CI.
	testerCI = "CI"
)

// ledgerHeader is the only column order the validator understands.
var ledgerHeader = []string{"格", "状态", "被测 SHA", "证据", "复测人", "日期"}

var (
	sectionRe  = regexp.MustCompile(`^## 2[.\s]`)
	heading2Re = regexp.MustCompile(`^## `)
	tableRe    = regexp.MustCompile(`^### (2\.\d+)\s+(.*?)\s*$`)
	cellIDRe   = regexp.MustCompile(`^[A-F][0-9]+$`)
	shaRe      = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	dateRe     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// methodCell is one row of a method table of §2.1–§2.6.
type methodCell struct {
	id     string
	lineNo int
	table  string
}

// rawRow is one line of the ledger table, not yet validated.
type rawRow struct {
	lineNo int
	line   string
	cells  []string
}

// document is the acceptance document: its lines, the cells of its method
// tables and the rows of its ledger.
type document struct {
	path  string // where the file lives (I/O)
	name  string // how it is named in messages
	lines []string

	method []methodCell
	titles map[string]string // group letter → the "### 2.x" title

	beginIdx  int // index of the begin marker line
	headerIdx int
	sepIdx    int
	endIdx    int
	rows      []rawRow
}

// cell is one validated row of the ledger.
type cell struct {
	id       string
	lineNo   int
	mark     string
	note     string
	sha      string
	evidence []string
	tester   string
	date     string
	guarded  bool // a ✔ cell whose tester is CI and whose evidence names a test
}

// violation is one broken rule, with the place it was broken.
type violation struct {
	doc  *document
	line int // 1-based line, 0 when the problem is the table as a whole
	cell string
	msg  string
}

func (v violation) String() string {
	switch {
	case v.cell != "" && v.line > 0:
		return fmt.Sprintf("%s: cell %s: %s", v.doc.at(v.line), v.cell, v.msg)
	case v.cell != "":
		return fmt.Sprintf("%s: cell %s: %s", v.doc.name, v.cell, v.msg)
	case v.line > 0:
		return fmt.Sprintf("%s: %s", v.doc.at(v.line), v.msg)
	default:
		return fmt.Sprintf("%s: %s", v.doc.name, v.msg)
	}
}

// ledgerError is a ledger that breaks one or more rules. Every broken rule
// is reported: a check that stops at the first problem makes the reader
// fix the file one round trip at a time.
type ledgerError struct {
	violations []violation
}

func (e *ledgerError) Error() string {
	parts := make([]string, 0, len(e.violations))
	for _, v := range e.violations {
		parts = append(parts, v.String())
	}
	return strings.Join(parts, "\n")
}

// at names a line of the document in a message.
func (doc *document) at(lineNo int) string {
	return fmt.Sprintf("%s:%d", doc.name, lineNo)
}

// parseDocument reads the method tables and the ledger out of the document.
//
// The method tables are the rows of §2.1–§2.6 that look like
// "| A1 | …"; the ledger is the table between the two markers.
func parseDocument(text, path, name string) (*document, error) {
	doc := &document{
		path:   path,
		name:   name,
		lines:  strings.Split(text, "\n"),
		titles: map[string]string{},
	}
	method, titles, err := parseMethodTables(doc.lines)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	doc.method, doc.titles = method, titles
	if err := doc.parseLedger(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return doc, nil
}

// parseMethodTables returns the cells of the method tables in document
// order, and the title of the table each group of cells came from.
func parseMethodTables(lines []string) ([]methodCell, map[string]string, error) {
	var cells []methodCell
	titles := map[string]string{}
	inSection := false
	sectionSeen := false
	table, title := "", ""

	for i, line := range lines {
		switch {
		case sectionRe.MatchString(line):
			inSection = true
			sectionSeen = true
			table, title = "", ""
			continue
		case heading2Re.MatchString(line):
			inSection = false
			continue
		}
		if !inSection {
			continue
		}
		if m := tableRe.FindStringSubmatch(line); m != nil {
			table, title = m[1], m[2]
			continue
		}
		row := splitRow(line)
		if len(row) == 0 || !cellIDRe.MatchString(row[0]) {
			continue
		}
		cells = append(cells, methodCell{id: row[0], lineNo: i + 1, table: table})
		if _, seen := titles[row[0][:1]]; !seen && title != "" {
			titles[row[0][:1]] = title
		}
	}

	if !sectionSeen {
		return nil, nil, errors.New("no §2 heading (\"## 2.\") found: the acceptance grid is not where it is expected")
	}
	if len(cells) == 0 {
		return nil, nil, errors.New("the method tables of §2.1–§2.6 hold no row of the form \"| A1 | …\"")
	}
	return cells, titles, nil
}

// parseLedger reads the table between the two markers.
func (doc *document) parseLedger() error {
	begin, end := -1, -1
	for i, line := range doc.lines {
		switch strings.TrimSpace(line) {
		case ledgerBegin:
			if begin >= 0 {
				return fmt.Errorf("line %d: %s appears twice (the first one is at line %d)", i+1, ledgerBegin, begin+1)
			}
			begin = i
		case ledgerEnd:
			if end >= 0 {
				return fmt.Errorf("line %d: %s appears twice (the first one is at line %d)", i+1, ledgerEnd, end+1)
			}
			end = i
		}
	}
	if begin < 0 {
		return fmt.Errorf("no %s line: the ledger of §10 is missing", ledgerBegin)
	}
	if end < 0 {
		return fmt.Errorf("line %d: %s has no closing %s", begin+1, ledgerBegin, ledgerEnd)
	}
	if end < begin+3 {
		return fmt.Errorf("line %d: the ledger table holds %d line(s); want a header, a separator and the %d rows", begin+1, end-begin-1, wantCells)
	}

	doc.beginIdx, doc.headerIdx, doc.sepIdx, doc.endIdx = begin, begin+1, begin+2, end

	header := splitRow(doc.lines[doc.headerIdx])
	if strings.Join(header, "|") != strings.Join(ledgerHeader, "|") {
		return fmt.Errorf("line %d: the ledger header is \"| %s |\", want \"| %s |\"",
			doc.headerIdx+1, strings.Join(header, " | "), strings.Join(ledgerHeader, " | "))
	}
	if !isSeparatorRow(splitRow(doc.lines[doc.sepIdx])) {
		return fmt.Errorf("line %d: the separator of the ledger table is \"| %s |\", want \"| %s |\"",
			doc.sepIdx+1, strings.Join(splitRow(doc.lines[doc.sepIdx]), " | "), strings.Repeat("---|", len(ledgerHeader)-1)+"---")
	}

	for i := doc.sepIdx + 1; i < doc.endIdx; i++ {
		cells := splitRow(doc.lines[i])
		if len(cells) == 0 {
			return fmt.Errorf("line %d: a line that is not a table row sits inside the ledger; only apply writes between the markers", i+1)
		}
		doc.rows = append(doc.rows, rawRow{lineNo: i + 1, line: doc.lines[i], cells: cells})
	}
	return nil
}

// isSeparatorRow reports whether the cells are the dashes of a markdown
// table separator.
func isSeparatorRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if c == "" {
			return false
		}
		dashes := false
		for _, r := range c {
			switch r {
			case '-':
				dashes = true
			case ':':
			default:
				return false
			}
		}
		if !dashes {
			return false
		}
	}
	return true
}

// splitRow splits a markdown table row into its trimmed cells. A pipe
// preceded by a backslash belongs to its cell, as in GitHub-flavoured
// markdown. A line that is not a table row returns nil.
func splitRow(line string) []string {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "|") {
		return nil
	}
	s = s[1:]
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) && s[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if c == '|' {
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if rest := strings.TrimSpace(cur.String()); rest != "" {
		cells = append(cells, rest)
	}
	return cells
}

// renderRow renders cells as a ledger row.
//
// The shape is the one the document uses: a cell that has content is
// written as " content |" and an empty cell as " |", so the empty columns
// of the ledger read as one space between the pipes.
func renderRow(cells []string) string {
	var b strings.Builder
	b.WriteString("|")
	for _, c := range cells {
		if c == "" {
			b.WriteString(" |")
			continue
		}
		b.WriteString(" " + c + " |")
	}
	return b.String()
}

// parseState splits a state cell into its mark and its note.
//
// The problem is returned instead of a bool: a state the ledger refuses has
// to say WHY, and the reason differs ("不适用" is not a mark, a mark without
// a note says nothing, a ✔ with a note is two things at once).
func parseState(s string) (mark, note, problem string) {
	mark, note, _ = strings.Cut(s, stateSep)
	mark, note = strings.TrimSpace(mark), strings.TrimSpace(note)
	switch mark {
	case markPass:
		if note != "" {
			return mark, note, fmt.Sprintf("\"%s\" takes no note: write the state as \"%s\" alone and put the remark in the evidence column", mark, mark)
		}
		return mark, note, ""
	case markPart, markFail, markNone:
		if note == "" {
			return mark, note, fmt.Sprintf("\"%s\" needs a note: write what is missing and who is needed (\"%s — …\")", mark, mark)
		}
		return mark, note, ""
	case "":
		return mark, note, "the state is empty: write \"✔\", or \"◐ — …\", \"✘ — …\", \"未验 — …\""
	}
	return mark, note, fmt.Sprintf("the state \"%s\" is not accepted: write \"%s\", or \"%s — …\", \"%s — …\", \"%s — …\"",
		mark, markPass, markPart, markFail, markNone)
}

// splitEvidence splits the evidence cell into pointers. The documented
// separator is "; "; a stray one leaves an empty pointer, which is
// reported rather than ignored.
func splitEvidence(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// validDate reports whether s is a YYYY-MM-DD date that exists.
func validDate(s string) bool {
	if !dateRe.MatchString(s) {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// validate checks the ledger against the method tables and the repository.
//
// The returned cells are complete only when no violation is returned: a row
// that breaks a rule is reported and left out, so the counts of a broken
// ledger are never shown as if they were the counts of a good one.
func (doc *document) validate(ev *resolver) ([]cell, []violation) {
	var head, rows, tail []violation
	add := func(dst *[]violation, line int, id, format string, args ...any) {
		*dst = append(*dst, violation{doc: doc, line: line, cell: id, msg: fmt.Sprintf(format, args...)})
	}

	method := map[string]methodCell{}
	for _, m := range doc.method {
		if first, dup := method[m.id]; dup {
			add(&head, m.lineNo, m.id, "appears twice in the method tables (the first one is at line %d)", first.lineNo)
			continue
		}
		method[m.id] = m
	}

	seen := map[string]int{}
	var cells []cell
	var order []string

	for _, row := range doc.rows {
		id := ""
		if len(row.cells) > 0 {
			id = row.cells[0]
		}
		if len(row.cells) != len(ledgerHeader) {
			add(&rows, row.lineNo, id, "has %d cells, want %d (格 | 状态 | 被测 SHA | 证据 | 复测人 | 日期)",
				len(row.cells), len(ledgerHeader))
			continue
		}
		if !cellIDRe.MatchString(id) {
			add(&rows, row.lineNo, id, "is not a cell id: want a letter A–F and a number, as in the method tables")
			continue
		}
		if _, known := method[id]; !known {
			add(&rows, row.lineNo, id, "is not one of the %d cells of the method tables (§2.1–§2.6)", wantCells)
			continue
		}
		if first, dup := seen[id]; dup {
			add(&rows, row.lineNo, id, "appears twice: the first row is at line %d", first)
			continue
		}
		seen[id] = row.lineNo
		order = append(order, id)

		c := cell{
			id:       id,
			lineNo:   row.lineNo,
			sha:      row.cells[2],
			evidence: splitEvidence(row.cells[3]),
			tester:   row.cells[4],
			date:     row.cells[5],
		}

		var problems []string
		mark, note, problem := parseState(row.cells[1])
		if problem != "" {
			problems = append(problems, problem)
		}
		c.mark, c.note = mark, note

		if c.sha != "" && !shaRe.MatchString(c.sha) {
			problems = append(problems, fmt.Sprintf("the tested SHA \"%s\" is not 7–40 hex digits", c.sha))
		}
		if mark == markPass && c.sha == "" {
			problems = append(problems, "a ✔ row needs the tested SHA in \"被测 SHA\"")
		}
		if mark == markPass && c.tester == "" {
			problems = append(problems, "a ✔ row needs the tester in \"复测人\"")
		}
		if c.date != "" && !validDate(c.date) {
			problems = append(problems, fmt.Sprintf("the date \"%s\" is not a YYYY-MM-DD date", c.date))
		}
		if mark == markPass && c.date == "" {
			problems = append(problems, "a ✔ row needs the date in \"日期\" as YYYY-MM-DD")
		}
		if mark == markPass && len(c.evidence) == 0 {
			problems = append(problems, "a ✔ row needs at least one evidence pointer in \"证据\"")
		}

		hasTest := false
		for i, p := range c.evidence {
			kind, err := ev.resolve(p)
			if err != nil {
				problems = append(problems, fmt.Sprintf("evidence %d (\"%s\") does not resolve: %v", i+1, p, err))
				continue
			}
			if kind == evidenceTest {
				hasTest = true
			}
		}

		if len(problems) > 0 {
			for _, p := range problems {
				add(&rows, row.lineNo, id, "%s", p)
			}
			continue
		}
		c.guarded = c.tester == testerCI && hasTest
		cells = append(cells, c)
	}

	// The size of the table.
	if len(doc.rows) != wantCells {
		add(&head, 0, "", "the ledger has %d rows, want %d", len(doc.rows), wantCells)
	}

	// The cells the method tables have and the ledger does not.
	for _, m := range doc.method {
		if _, ok := seen[m.id]; ok {
			continue
		}
		add(&tail, 0, m.id, "is in the method tables (§%s) but has no row in the ledger", m.table)
	}

	// The order: the ledger follows the method tables.
	want := make([]string, 0, len(doc.method))
	for _, m := range doc.method {
		if _, ok := seen[m.id]; ok {
			want = append(want, m.id)
		}
	}
	for i := 0; i < len(order) && i < len(want); i++ {
		if order[i] != want[i] {
			add(&rows, seen[order[i]], order[i], "is out of order: the method tables put %s before it", want[i])
			break
		}
	}

	out := make([]violation, 0, len(head)+len(rows)+len(tail))
	out = append(out, head...)
	out = append(out, rows...)
	out = append(out, tail...)
	return cells, out
}

// tally counts the cells of one group by their mark.
type tally struct {
	pass int
	part int
	fail int
	none int
}

func (t *tally) add(mark string) {
	switch mark {
	case markPass:
		t.pass++
	case markPart:
		t.part++
	case markFail:
		t.fail++
	case markNone:
		t.none++
	}
}

func (t tally) total() int { return t.pass + t.part + t.fail + t.none }

func (t tally) String() string {
	return fmt.Sprintf("✔=%d ◐=%d ✘=%d 未验=%d", t.pass, t.part, t.fail, t.none)
}

// groupCells splits the cells by their leading letter and returns the
// groups in alphabetical order.
func groupCells(cells []cell) ([]string, map[string]*tally) {
	byGroup := map[string]*tally{}
	var order []string
	for _, c := range cells {
		g := c.id[:1]
		if byGroup[g] == nil {
			byGroup[g] = &tally{}
			order = append(order, g)
		}
		byGroup[g].add(c.mark)
	}
	sort.Strings(order)
	return order, byGroup
}

// renderCounts prints the four numbers, one line per group, then the total
// and the number of passed cells a CI test guards.
//
// The format is fixed so that a script can read it:
// "awk '/^total/{print $2}' counts".
func renderCounts(cells []cell) string {
	order, groups := groupCells(cells)
	total := tally{}
	guarded := 0
	for _, c := range cells {
		total.add(c.mark)
		if c.mark == markPass && c.guarded {
			guarded++
		}
	}
	var b strings.Builder
	for _, g := range order {
		t := groups[g]
		fmt.Fprintf(&b, "%s %s\n", g, t)
	}
	fmt.Fprintf(&b, "total %s\n", total)
	fmt.Fprintf(&b, "guarded %d\n", guarded)
	return b.String()
}
