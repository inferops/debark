// Package progress renders build/fetch progress from the evidence stream, so
// core/ never needs to know whether it is talking to a terminal: "mpb when
// stdout is a TTY; a plain line per completed unit otherwise".
//
// This is a deliberate line-based fallback rather than vbauerster/mpb,
// which is available (go.mod carries it) but was not adopted here: mpb's
// value is a per-item bar with a known or estimable total, and this
// package's only signal is a live evidence.Event stream where "how many
// files, how many bytes, in total" is not known until the run ends — mpb
// would only ever be able to render a single indeterminate spinner off
// that input, no more informative than the two-line summary below, while
// adding a background render goroutine whose Wait()/Shutdown() ordering
// this Sink's synchronous, always-called-exactly-once Close would have to
// get exactly right to avoid hanging the process on exit. Sink below
// renders the same two behaviours the design specifies (a live-updating
// line on a TTY, one line per completed unit otherwise) using only fmt and
// os — carriage-return redraws instead of mpb's renderer — with no
// container lifecycle to manage. It reads only evidence.TypeFetchFile and
// evidence.TypeProgress events and ignores everything else, so it can be
// fanned in alongside the NDJSON and file sinks via evidence.MultiSink
// without any coordination between them. charmbracelet/lipgloss, by
// contrast, is used
// directly by internal/cli/render, whose package doc explains why that one
// was worth adopting.
package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/internal/cli/render"
)

// Sink renders fetch/build progress to w.
//
// In TTY mode it keeps one line updated in place (carriage return, pad to
// clear, no trailing newline until Close): safe over a real terminal, but
// never used unless the caller has already confirmed w is an interactive
// TTY, because a carriage-return-only line is unreadable in a captured log.
// In line mode it prints one full line per completed unit, which is exactly
// what a CI log, an SSH transcript or `tee` wants.
type Sink struct {
	w     io.Writer
	style render.Style
	tty   bool

	mu          sync.Mutex
	filesDone   int
	bytesDone   int64
	lastLineLen int
	closed      bool
}

// New returns a progress sink. tty should be true only when w is a real,
// interactive terminal (render.IsTerminal) and the run is interactive
// (neither --json nor --json-events,).
func New(w io.Writer, tty bool, style render.Style) *Sink {
	return &Sink{w: w, tty: tty, style: style}
}

// Emit implements evidence.Sink. It never blocks the producer beyond the
// cost of a formatted write, and a write error is silently absorbed like any
// other sink's, per the Sink contract in core/evidence.
func (s *Sink) Emit(e evidence.Event) {
	switch e.Type {
	case evidence.TypeFetchFile:
		s.onFetchFile(e)
	case evidence.TypeProgress:
		s.onProgress(e)
	}
}

// Close finishes the current line so whatever a caller prints next (a build
// summary, a shell prompt) starts on a fresh one.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tty && s.lastLineLen > 0 && !s.closed {
		fmt.Fprintln(s.w)
	}
	s.closed = true
	return nil
}

func (s *Sink) onFetchFile(e evidence.Event) {
	name := attrString(e.Attrs, "filename", "name", "file", "url")
	if name == "" {
		name = e.Msg
	}
	size, hasSize := attrInt64(e.Attrs, "bytes", "size")
	failed := attrString(e.Attrs, "error") != "" || e.Level == evidence.LevelError
	cached := attrBool(e.Attrs, "cached") || attrBool(e.Attrs, "store_hit")

	s.mu.Lock()
	defer s.mu.Unlock()
	if !failed {
		s.filesDone++
		if hasSize {
			s.bytesDone += size
		}
	}

	if s.tty {
		status := "fetched"
		if cached {
			status = "cached "
		}
		if failed {
			status = s.style.Fail("failed ")
		}
		line := fmt.Sprintf("%s %s  %s  %s",
			status, render.Plural(s.filesDone, "file"), render.Bytes(s.bytesDone), name)
		s.redraw(line)
		return
	}

	switch {
	case failed:
		fmt.Fprintf(s.w, "failed  %s: %s\n", name, attrString(e.Attrs, "error"))
	case cached:
		fmt.Fprintf(s.w, "cached  %s\n", name)
	case hasSize:
		fmt.Fprintf(s.w, "fetched %s (%s)\n", name, render.Bytes(size))
	default:
		fmt.Fprintf(s.w, "fetched %s\n", name)
	}
}

func (s *Sink) onProgress(e evidence.Event) {
	label := attrString(e.Attrs, "label", "phase")
	if label == "" {
		label = e.Msg
	}
	current, hasCurrent := attrInt64(e.Attrs, "current", "done")
	total, hasTotal := attrInt64(e.Attrs, "total", "expected")

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tty {
		var line string
		switch {
		case hasCurrent && hasTotal && total > 0:
			pct := int(100 * float64(current) / float64(total))
			line = fmt.Sprintf("%-12s %3d%%  %s", label, pct, render.Bytes(current))
		case hasCurrent:
			line = fmt.Sprintf("%-12s %s", label, render.Bytes(current))
		default:
			line = label
		}
		s.redraw(line)
		return
	}
	if label != "" {
		fmt.Fprintf(s.w, "%s\n", label)
	}
}

// redraw overwrites the previously drawn line in place. Must be called with
// s.mu held.
func (s *Sink) redraw(line string) {
	pad := s.lastLineLen - len(line)
	if pad < 0 {
		pad = 0
	}
	fmt.Fprintf(s.w, "\r%s%s", line, strings.Repeat(" ", pad))
	s.lastLineLen = len(line)
}

func attrString(attrs map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := attrs[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func attrBool(attrs map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := attrs[k]; ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
	}
	return false
}

// attrInt64 reads the first of keys present in attrs as an int64, tolerating
// every numeric type a producer might reasonably use (a Go int/int64 set
// in-process, or a float64 if the same map ever arrived via encoding/json).
func attrInt64(attrs map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		v, ok := attrs[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case int64:
			return n, true
		case int:
			return int64(n), true
		case int32:
			return int64(n), true
		case float64:
			return int64(n), true
		case uint64:
			// A uint64 above math.MaxInt64 would wrap to a negative int64 here.
			// Left as a plain conversion, deliberately: every value that reaches
			// this function is a byte count or an item count produced by
			// debark's own evidence stream in this same process, and the only
			// consumers are render.Bytes and a percentage (onFetchFile,
			// onProgress above) -- a display, not a decision. Reaching the wrap
			// point needs a producer claiming more than 9.2 exabytes, and the
			// cost of it would be one ugly progress line, not a wrong build.
			// Clamping instead would be a behaviour change (a bogus huge total
			// would start rendering as a plausible one) for no real gain.
			// #nosec G115 -- see above: display-only path, in-process producer
			return int64(n), true
		}
	}
	return 0, false
}
