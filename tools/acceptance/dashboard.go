// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package main

// The board fragment.
//
// docs/project-dashboard.html is a single file with its own stylesheet, so
// the fragment reuses the classes that file already defines:
//
//	section, section-head   the section wrapper and its head
//	grid kpis, card kpi     the row of cards
//	label, value            the label and the number of a card
//	matrix, matrix-wrap     the table of the groups
//	progress                a progress bar
//	ok warn bad muted info  the colours
//
// ★ THE PASSING COLOUR IS EARNED. The class "ok" (green) is used ONLY when
// every cell of the ledger passed. A board that paints 3 of 52 green says
// "done" about a grid that is not done, and that is the exact failure this
// ledger exists to prevent: the numbers on the board must come from the
// ledger, and the ledger must be the one the acceptance list describes.

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// renderDashboard returns the HTML fragment for the board.
//
// It is a pure function of the ledger: the same ledger always renders the
// same bytes, so the board and the tool can be compared.
func renderDashboard(cells []cell, titles map[string]string) string {
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
	complete := total.total() > 0 && total.pass == total.total()
	b.WriteString("<section class=\"section\" id=\"ledger\">\n")
	fmt.Fprintf(&b, "  <div class=\"section-head\"><h2>M4 验收台账</h2><p>%d 格 · 数据来自 docs/design/M4-ACCEPTANCE.md §10 · `go run ./tools/acceptance counts`</p></div>\n", total.total())
	b.WriteString("  <section class=\"grid kpis\">\n")
	fmt.Fprintf(&b, "    <div class=\"card kpi\"><div class=\"label\">✔ 判据达成</div><div class=\"value %s\">%d</div><div class=\"label\">判据达成，且有被测 SHA、复测人、日期和证据</div></div>\n",
		passClass(total.pass, total.total(), complete), total.pass)
	fmt.Fprintf(&b, "    <div class=\"card kpi\"><div class=\"label\">◐ 部分达成</div><div class=\"value %s\">%d</div><div class=\"label\">只达成了一部分</div></div>\n",
		countClass(total.part, "warn"), total.part)
	fmt.Fprintf(&b, "    <div class=\"card kpi\"><div class=\"label\">✘ 判据未达成</div><div class=\"value %s\">%d</div><div class=\"label\">真跑了，判据没达成</div></div>\n",
		countClass(total.fail, "bad"), total.fail)
	fmt.Fprintf(&b, "    <div class=\"card kpi\"><div class=\"label\">未验</div><div class=\"value %s\">%d</div><div class=\"label\">没跑，或跑不了</div></div>\n",
		countClass(total.none, "warn"), total.none)
	fmt.Fprintf(&b, "    <div class=\"card kpi\"><div class=\"label\">台账进度</div><div class=\"value %s\">✔ %d / %d</div><div class=\"progress\"><i style=\"width:%s\"></i></div><div class=\"label\">%d 格带 CI 守卫</div></div>\n",
		progressClass(total), total.pass, total.total(), percent(total.pass, total.total()), guarded)
	fmt.Fprintf(&b, "    <div class=\"card kpi\"><div class=\"label\">CI 守卫</div><div class=\"value %s\">%d</div><div class=\"label\">✔ 里复测人写 CI、证据指向测试函数的格数</div></div>\n",
		countClass(guarded, "info"), guarded)
	b.WriteString("  </section>\n")
	b.WriteString("  <article class=\"card matrix-wrap\">\n")
	b.WriteString("    <table class=\"matrix\">\n")
	b.WriteString("      <thead><tr><th>组</th><th>格</th><th>✔</th><th>◐</th><th>✘</th><th>未验</th><th>进度</th></tr></thead>\n")
	b.WriteString("      <tbody>\n")
	for _, g := range order {
		t := groups[g]
		label := g
		if title := titles[g]; title != "" {
			label = g + " · " + html.EscapeString(title)
		}
		fmt.Fprintf(&b, "        <tr><td>%s</td><td>%d</td><td class=\"%s\">%d</td><td class=\"%s\">%d</td><td class=\"%s\">%d</td><td class=\"%s\">%d</td><td><div class=\"progress\"><i style=\"width:%s\"></i></div></td></tr>\n",
			label, t.total(),
			passClass(t.pass, t.total(), complete), t.pass,
			countClass(t.part, "warn"), t.part,
			countClass(t.fail, "bad"), t.fail,
			countClass(t.none, "warn"), t.none,
			percent(t.pass, t.total()))
	}
	b.WriteString("      </tbody>\n")
	b.WriteString("    </table>\n")
	b.WriteString("  </article>\n")
	b.WriteString("</section>\n")
	return b.String()
}

// passClass is the colour of a "✔ N" number.
//
// The passing colour is earned by the WHOLE grid: a group that is 10 of 10
// while another group is 0 of 8 is not a green board.
func passClass(n, total int, complete bool) string {
	switch {
	case complete:
		return "ok"
	case n == 0:
		return "muted"
	default:
		return "warn"
	}
}

// countClass is the colour of a count that is not a pass: the colour of
// its meaning, grey when there is nothing to report.
func countClass(n int, colour string) string {
	if n == 0 {
		return "muted"
	}
	return colour
}

// progressClass is the colour of the headline "✔ N / 52".
func progressClass(t tally) string {
	switch {
	case t.total() > 0 && t.pass == t.total():
		return "ok"
	case t.pass == 0:
		return "bad"
	default:
		return "warn"
	}
}

// percent renders a share of the grid in percent, with one decimal.
func percent(n, total int) string {
	if total <= 0 {
		return "0.0%"
	}
	tenths := n * 1000 / total
	return fmt.Sprintf("%d.%d%%", tenths/10, tenths%10)
}

// extractMarked returns what sits between the two marker lines of a board,
// and whether both markers are there at all.
//
// The content is taken BYTE for byte: the fragment the tool prints is
// pasted as it is, so the board and the tool cannot drift apart silently.
func extractMarked(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	begin, end := -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case ledgerBegin:
			if begin >= 0 {
				return "", false
			}
			begin = i
		case ledgerEnd:
			if end >= 0 {
				return "", false
			}
			end = i
		}
	}
	if begin < 0 || end < 0 || end <= begin {
		return "", false
	}
	return strings.Join(lines[begin+1:end], "\n") + "\n", true
}

// okClassRe finds a class attribute that carries the passing colour.
var okClassRe = regexp.MustCompile(`class="([^"]*\s)?ok(\s[^"]*)?"`)
