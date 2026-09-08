package render

import (
	"fmt"
	"io"

	"github.com/charmbracelet/lipgloss"
	ltable "github.com/charmbracelet/lipgloss/table"
)

// Table is a minimal left-aligned table: lipgloss/table with every border
// disabled, so it stays a plain space-padded grid — legible over a serial
// console and safe to paste into a ticket, which is why debark never asks
// lipgloss/table for box-drawing borders here (colour and tables
// only; no Bubble Tea).
type Table struct {
	Header []string
	Rows   [][]string
}

// NewTable creates a table with the given column headers.
func NewTable(header ...string) *Table {
	return &Table{Header: header}
}

// AddRow appends one row. Its length should match the header; a short row is
// padded with empty cells and a long one is truncated, so a caller mistake
// degrades instead of panicking.
func (t *Table) AddRow(cells ...string) {
	row := make([]string, len(t.Header))
	copy(row, cells)
	t.Rows = append(t.Rows, row)
}

// cellStyle is the only styling this table applies: two spaces of gap after
// every cell, header included. A package-level value rather than a fresh
// Style per call, because StyleFunc is invoked once per cell and
// lipgloss.NewStyle allocates.
var cellStyle = lipgloss.NewStyle().PaddingRight(2)

// Render writes the table to w.
func (t *Table) Render(w io.Writer) {
	fmt.Fprint(w, t.String())
}

// String renders the table, for golden tests and for Render above.
func (t *Table) String() string {
	lt := ltable.New().
		BorderTop(false).BorderBottom(false).
		BorderLeft(false).BorderRight(false).
		BorderHeader(false).BorderColumn(false).BorderRow(false).
		// Turning the column border off also takes away the separator that
		// was keeping columns apart, so without this every column's WIDEST
		// cell -- and only that one, since every shorter cell is padded out
		// to match it -- runs straight into the next column. It shipped
		// unnoticed because every caller until now happened to have short
		// cells; `snapshot list-bases` was the first table with a cell long
		// enough to set its own column's width.
		StyleFunc(func(_, _ int) lipgloss.Style { return cellStyle })
	if len(t.Header) > 0 {
		lt.Headers(t.Header...)
	}
	if len(t.Rows) > 0 {
		lt.Rows(t.Rows...)
	}
	s := lt.String()
	if s == "" {
		return ""
	}
	return s + "\n"
}
