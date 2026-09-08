package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/inferops/debark/test/e2e/harness"
)

// printSummaryTable renders the short human summary table the task asks
// for ("a machine-readable result file ... and a short human summary
// table — this is what the nightly CI job publishes"). No table/colour
// library is used, matching this tree's own established pattern
// (internal/cli/command.go's doc comment: go.mod carries none of
// cobra/koanf/mpb/lipgloss, so the CLI hand-rolled its own) — this is the
// same call for the same reason here.
func printSummaryTable(w io.Writer, m resultDoc) {
	headers := []string{"FIXTURE", "RELEASE", "ARCH", "STATUS", "STAGE", "TIME", "NOTE"}
	rows := make([][]string, 0, len(m.Rows))
	for _, r := range m.Rows {
		release := r.Distro
		if r.Version != "" {
			release += " " + r.Version
		}
		note := r.Blocker
		if r.DeterminismOK != nil && !*r.DeterminismOK {
			note = "not byte-identical: " + note
		}
		rows = append(rows, []string{
			r.Fixture,
			release,
			r.Arch,
			strings.ToUpper(string(r.Status)),
			r.Stage,
			fmt.Sprintf("%.1fs", float64(r.TotalMS)/1000),
			truncateForTable(note, 80),
		})
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	// The NOTE column would otherwise force a very wide terminal; cap it so
	// the rest of the table stays readable, since the full text is always
	// in the JSON result and the row's own transcript file.
	const maxNoteWidth = 80
	if widths[len(widths)-1] > maxNoteWidth {
		widths[len(widths)-1] = maxNoteWidth
	}

	printRow(w, headers, widths)
	sep := make([]string, len(headers))
	for i, wd := range widths {
		sep[i] = strings.Repeat("-", wd)
	}
	printRow(w, sep, widths)
	for _, row := range rows {
		printRow(w, row, widths)
	}

	fmt.Fprintf(w, "\n%s\n", m.Summary.line())
	printUnfinishedRows(w, m.Rows)
}

// line is the one-line count breakdown. pass/fail/blocked/skipped are always
// printed, in the order they always have been; timeout and aborted are
// appended only when they happened, so a healthy run's summary line is
// unchanged.
func (s summaryCounts) line() string {
	out := fmt.Sprintf("%d total: %d pass, %d fail, %d blocked, %d skipped",
		s.Total, s.Pass, s.Fail, s.Blocked, s.Skipped)
	if s.Timeout > 0 {
		out += fmt.Sprintf(", %d timeout", s.Timeout)
	}
	if s.Aborted > 0 {
		out += fmt.Sprintf(", %d aborted", s.Aborted)
	}
	return out
}

// printUnfinishedRows gives every row the harness itself stopped its own
// line, below the table and the counts: which row, how far it got, and how
// long it ran before being killed.
//
// This block exists because the alternative was measured: nine rows of the
// 2026-09-03 run reported "exit 1 (usage), want 0 (success)" — the exit
// status of a process the harness had killed — and read as a command-line
// bug in debark that did not exist. A row that never reached a verdict
// must say so in the place a human actually looks.
func printUnfinishedRows(w io.Writer, rows []harness.RowResult) {
	var stopped []harness.RowResult
	timeouts := 0
	for _, r := range rows {
		switch r.Status {
		case StatusTimeout:
			timeouts++
			stopped = append(stopped, r)
		case StatusAborted:
			stopped = append(stopped, r)
		case StatusErrored:
			stopped = append(stopped, r)
		}
	}
	if len(stopped) == 0 {
		return
	}

	fmt.Fprintf(w, "\nThe harness stopped %d row(s) before debark reached a verdict — these are\n", len(stopped))
	fmt.Fprintf(w, "not product results:\n")
	for _, r := range stopped {
		verb := "killed at --row-timeout"
		switch r.Status {
		case StatusAborted:
			verb = "cancelled by signal"
		case StatusErrored:
			verb = "ended by the container runtime or a signal, not by debark"
		}
		release := r.Distro
		if r.Version != "" {
			release += " " + r.Version
		}
		fmt.Fprintf(w, "  %-7s %s / %s / %s: %s after %.1fs, furthest stage %q\n",
			strings.ToUpper(string(r.Status)), r.Fixture, release, r.Arch, verb, float64(r.TotalMS)/1000, r.Stage)
	}
	if timeouts > 1 {
		fmt.Fprintf(w, "\nSeveral rows stopping at the same limit points at the host, not the code: check\n")
		fmt.Fprintf(w, "`docker run --rm debian:bookworm-slim apt-get update -qq` (a healthy host answers\n")
		fmt.Fprintf(w, "in a few seconds) and read hack/matrix/results/README.md before treating any of\n")
		fmt.Fprintf(w, "this run as a debark signal.\n")
	}
}

func printRow(w io.Writer, cells []string, widths []int) {
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = padRight(truncateForTable(c, widths[i]), widths[i])
	}
	fmt.Fprintln(w, strings.Join(parts, "  "))
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func truncateForTable(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
