package cli

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// A markdown table is a header row, a separator row of dashes and the body.
// renderTable draws it with the columns aligned and the numbers on the right,
// the way a terminal table looks. When the table does not fit the screen it
// gives up and the caller keeps the original pipes: a truncated table is
// worse than an ugly one.

// parseTableRow splits a markdown table row into its cells. It returns nil
// when the line is not a table row.
func parseTableRow(line string) []string {
	t := strings.TrimSpace(line)
	if !strings.Contains(t, "|") {
		return nil
	}
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	parts := strings.Split(t, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// isSeparatorRow tells the "---" row from a real one: every cell is dashes
// (with an optional colon on each side).
func isSeparatorRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		t := strings.Trim(c, ":")
		if len(t) < 3 || strings.Trim(t, "-") != "" {
			return false
		}
	}
	return true
}

// numericColumn tells whether every body cell of a column is a number, so the
// column can be right-aligned like a table of figures.
func numericColumn(rows [][]string, col int) bool {
	seen := false
	for _, r := range rows {
		if col >= len(r) || r[col] == "" {
			continue
		}
		seen = true
		s := strings.NewReplacer(",", "", ".", "", "%", "", " ", "", "+", "").Replace(r[col])
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return false
		}
	}
	return seen
}

// renderTable draws the table. It returns the lines and false when the table
// does not fit the width.
func renderTable(rows [][]string, width int) ([]string, bool) {
	if len(rows) < 2 {
		return nil, false
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return nil, false
	}
	// The width of every column: the widest cell.
	widths := make([]int, cols)
	for _, r := range rows {
		for i, c := range r {
			if w := utf8.RuneCountInString(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	total := 0
	for i, w := range widths {
		total += w
		if i > 0 {
			total += 2 // the gap between the columns
		}
	}
	if total > width-4 {
		return nil, false
	}
	right := make([]bool, cols)
	for i := range right {
		right[i] = numericColumn(rows[1:], i)
	}
	pad := func(s string, i int) string {
		gap := widths[i] - utf8.RuneCountInString(s)
		if gap < 0 {
			gap = 0
		}
		if right[i] {
			return strings.Repeat(" ", gap) + s
		}
		return s + strings.Repeat(" ", gap)
	}
	var out []string
	// The header, in bold.
	head := make([]string, cols)
	for i := 0; i < cols; i++ {
		c := ""
		if i < len(rows[0]) {
			c = rows[0][i]
		}
		head[i] = boldf(pad(c, i))
	}
	out = append(out, "  "+strings.Join(head, "  "))
	// The rule under the header.
	rule := make([]string, cols)
	for i := range rule {
		rule[i] = grayf(strings.Repeat("─", widths[i]))
	}
	out = append(out, "  "+strings.Join(rule, "  "))
	// The body.
	for _, r := range rows[1:] {
		cells := make([]string, cols)
		for i := 0; i < cols; i++ {
			c := ""
			if i < len(r) {
				c = r[i]
			}
			cells[i] = pad(c, i)
		}
		out = append(out, "  "+strings.Join(cells, "  "))
	}
	return out, true
}
