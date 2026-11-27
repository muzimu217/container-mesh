// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

// The TSV that `apply` merges into the ledger.
//
// The first line is the header; the seven columns may be in any order, but
// their names are fixed:
//
//	id	mark	note	sha	evidence	tester	date
//
// The state column of the ledger is written from the mark and the note:
// "✔" alone for a pass, "mark — note" for the other three. A row the TSV
// does not mention keeps its line as it is.

import (
	"fmt"
	"strings"
)

// tsvColumns are the seven columns of the TSV, by name.
var tsvColumns = []string{"id", "mark", "note", "sha", "evidence", "tester", "date"}

// tsvRow is one row of the TSV.
type tsvRow struct {
	lineNo   int
	id       string
	mark     string
	note     string
	sha      string
	evidence string
	tester   string
	date     string
}

// parseTSV reads the TSV. Every problem is reported with the line it is on.
func parseTSV(data []byte) ([]tsvRow, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, fmt.Errorf("the TSV is empty: the first line must be the header (%s)", strings.Join(tsvColumns, ", "))
	}

	head := strings.Split(lines[0], "\t")
	col := map[string]int{}
	for i, name := range head {
		name = strings.TrimSpace(name)
		if _, dup := col[name]; dup {
			return nil, fmt.Errorf("TSV line 1: the column %q appears twice", name)
		}
		col[name] = i
	}
	if len(head) != len(tsvColumns) {
		return nil, fmt.Errorf("TSV line 1: the header has %d columns, want %d (%s)", len(head), len(tsvColumns), strings.Join(tsvColumns, ", "))
	}
	for _, name := range tsvColumns {
		if _, ok := col[name]; !ok {
			return nil, fmt.Errorf("TSV line 1: the header has no %q column; it must hold %s", name, strings.Join(tsvColumns, ", "))
		}
	}

	var rows []tsvRow
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		fields := strings.Split(lines[i], "\t")
		if len(fields) != len(tsvColumns) {
			return nil, fmt.Errorf("TSV line %d: %d columns, want %d", i+1, len(fields), len(tsvColumns))
		}
		row := tsvRow{
			lineNo:   i + 1,
			id:       strings.TrimSpace(fields[col["id"]]),
			mark:     strings.TrimSpace(fields[col["mark"]]),
			note:     strings.TrimSpace(fields[col["note"]]),
			sha:      strings.TrimSpace(fields[col["sha"]]),
			evidence: strings.TrimSpace(fields[col["evidence"]]),
			tester:   strings.TrimSpace(fields[col["tester"]]),
			date:     strings.TrimSpace(fields[col["date"]]),
		}
		for _, f := range []struct{ name, value string }{
			{"id", row.id},
			{"mark", row.mark},
			{"note", row.note},
			{"sha", row.sha},
			{"evidence", row.evidence},
			{"tester", row.tester},
			{"date", row.date},
		} {
			if strings.Contains(f.value, "|") {
				return nil, fmt.Errorf("TSV line %d: the %s column contains \"|\", which would split the markdown row; write it without the pipe", row.lineNo, f.name)
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// merge writes the TSV rows into the ledger and returns the new document.
//
// The rows the TSV does not mention keep their line verbatim; the order of
// the table is always the order of the method tables.
func (doc *document) merge(rows []tsvRow) (string, error) {
	existing := map[string]rawRow{}
	for _, row := range doc.rows {
		if len(row.cells) != len(ledgerHeader) {
			return "", fmt.Errorf("line %d: the row has %d cells, want %d; fix the ledger before applying",
				row.lineNo, len(row.cells), len(ledgerHeader))
		}
		id := row.cells[0]
		if _, dup := existing[id]; dup {
			return "", fmt.Errorf("line %d: cell %s appears twice in the ledger; fix the ledger before applying", row.lineNo, id)
		}
		existing[id] = row
	}

	method := map[string]bool{}
	for _, m := range doc.method {
		method[m.id] = true
	}
	updates := map[string]tsvRow{}
	for _, row := range rows {
		if !method[row.id] {
			return "", fmt.Errorf("TSV line %d: %q is not a cell of the method tables", row.lineNo, row.id)
		}
		if _, dup := updates[row.id]; dup {
			return "", fmt.Errorf("TSV line %d: cell %s appears twice in the TSV", row.lineNo, row.id)
		}
		switch row.mark {
		case markPass:
			if row.note != "" {
				return "", fmt.Errorf("TSV line %d: a ✔ row takes no note (the state column is written as ✔ alone); put the remark in the evidence column", row.lineNo)
			}
		case markPart, markFail, markNone:
			if row.note == "" {
				return "", fmt.Errorf("TSV line %d: a %s row needs a note: what is missing, and who is needed", row.lineNo, row.mark)
			}
		default:
			return "", fmt.Errorf("TSV line %d: the mark %q is not one of ✔, ◐, ✘, 未验", row.lineNo, row.mark)
		}
		updates[row.id] = row
	}

	body := make([]string, 0, len(doc.method))
	for _, m := range doc.method {
		if row, ok := updates[m.id]; ok {
			body = append(body, renderTSVRow(row))
			continue
		}
		if row, ok := existing[m.id]; ok {
			body = append(body, row.line)
			continue
		}
		// No row yet: the validation of the result reports it.
	}

	out := make([]string, 0, len(doc.lines))
	out = append(out, doc.lines[:doc.beginIdx]...)
	out = append(out, doc.lines[doc.beginIdx])  // <!-- ledger:begin -->
	out = append(out, doc.lines[doc.headerIdx]) // the header
	out = append(out, doc.lines[doc.sepIdx])    // the separator
	out = append(out, body...)
	out = append(out, doc.lines[doc.endIdx]) // <!-- ledger:end -->
	out = append(out, doc.lines[doc.endIdx+1:]...)
	return strings.Join(out, "\n"), nil
}

// renderTSVRow writes one ledger row.
//
// The state column is the mark alone for ✔ and "mark — note" for the other
// three, so the ledger has exactly one way of spelling a state.
func renderTSVRow(row tsvRow) string {
	state := row.mark
	if row.mark != markPass {
		state = row.mark + " " + stateSep + " " + row.note
	}
	return renderRow([]string{row.id, state, row.sha, row.evidence, row.tester, row.date})
}
