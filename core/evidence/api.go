// Frozen public API of the evidence package (the sinks; every
// package emits).

package evidence

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/inferops/debark/core/dferr"
)

// New builds an event with the schema and timestamp filled in.
func New(typ, msg string, attrs map[string]any) Event {
	return Event{
		Schema: SchemaVersion,
		TS:     time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z"),
		Type:   typ,
		Msg:    msg,
		Attrs:  attrs,
	}
}

// Emit is a convenience for the common case: build and send in one call. It
// tolerates a nil sink, so no caller needs a guard.
func Emit(s Sink, typ, msg string, attrs map[string]any) {
	if s == nil {
		return
	}
	s.Emit(New(typ, msg, attrs))
}

// Warn emits a warning-level event.
func Warn(s Sink, typ, msg string, attrs map[string]any) {
	if s == nil {
		return
	}
	e := New(typ, msg, attrs)
	e.Level = LevelWarn
	s.Emit(e)
}

// Collector accumulates events in memory. The engine uses one so it can write
// evidence.json into the bundle at the end, after the bundle directory exists.
type Collector struct {
	mu      sync.Mutex
	events  []Event
	context map[string]any
}

// NewCollector returns an in-memory sink. ctx is the run identity written into
// the document header; it must never contain a machine fingerprint or anything
// the operator did not ask to record.
func NewCollector(ctx map[string]any) *Collector {
	return &Collector{context: ctx}
}

// Emit records an event.
//
// The event's Attrs map is copied, not retained. A Collector holds its events
// until the end of the run, so retaining the caller's map made the record
// mutable long after it was written: a producer that reuses one map across
// several emits — or that fills in a result field after emitting the event
// that announced the attempt — silently rewrote history, and what
// evidence.json finally said about a step was the caller's LAST word, not the
// word it recorded at the time. An audit record whose past can change is not
// one (principle 6: written as if an auditor will read it in five years).
//
// The copy is shallow, which is the right depth for this type: Attrs values
// are documented as JSON-encodable scalars and small slices, and a shallow
// copy fixes the aliasing that a reused map actually causes.
func (c *Collector) Emit(e Event) {
	if e.Schema == "" {
		e.Schema = SchemaVersion
	}
	if e.TS == "" {
		e.TS = time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
	}
	e.Attrs = copyAttrs(e.Attrs)
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

// copyAttrs returns a shallow copy of attrs, or nil when there is nothing to
// copy (so an event with no attributes still marshals without an "attrs" key).
func copyAttrs(attrs map[string]any) map[string]any {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]any, len(attrs))
	for k, v := range attrs {
		out[k] = v
	}
	return out
}

// Close is a no-op: the collector releases nothing.
func (c *Collector) Close() error { return nil }

// Events returns a copy of what has been recorded.
func (c *Collector) Events() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, len(c.events))
	copy(out, c.events)
	return out
}

// Document renders the collected events as the evidence.json document.
func (c *Collector) Document() Document {
	c.mu.Lock()
	defer c.mu.Unlock()
	events := make([]Event, len(c.events))
	copy(events, c.events)
	return Document{
		Schema:    SchemaVersion,
		CreatedAt: time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z"),
		Context:   c.context,
		Events:    events,
	}
}

// NewNDJSONSink streams events to w, one compact JSON object per line. It is
// what --json-events writes.
//
// Emit is mutex-guarded so concurrent producers (the fetcher runs several
// goroutines) serialise on the write, never on each other's state; a write
// error is dropped rather than propagated, because per the Sink contract "a
// slow or failing sink drops events rather than failing a build" — a reader
// that goes away (a closed pipe, `| head`, an SSH session that dropped) must
// never turn into a build failure. A malformed event (one whose Attrs cannot
// be marshalled) is dropped the same way rather than panicking the caller.
//
// Close never closes w. The sink did not open it, so it does not own its
// lifecycle — a caller who wired NewNDJSONSink(os.Stdout) still owns stdout
// afterwards, e.g. to print a final --json summary once the event stream
// ends. A caller that wants the underlying file closed does so itself.
func NewNDJSONSink(w io.Writer) Sink { return &ndjsonSink{w: w} }

type ndjsonSink struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *ndjsonSink) Emit(e Event) {
	if e.Schema == "" {
		e.Schema = SchemaVersion
	}
	if e.TS == "" {
		e.TS = time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
	}
	b, err := json.Marshal(e)
	if err != nil {
		// Drop: an event that cannot even be encoded must never fail the
		// build it is only supposed to be describing.
		return
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.w.Write(b) // tolerate write errors by dropping (Sink contract)
}

func (s *ndjsonSink) Close() error { return nil }

// NewFileSink collects events in memory like a Collector and, on Close,
// writes them to path as a single debark.events/v1 Document: a header
// (schema, created_at, ctx) plus every event recorded since construction.
// Unlike the NDJSON sink, a write failure here is real and reported — this is
// the one sink whose Close error the Sink contract says must not be
// swallowed, because it is usually the only durable copy of the run (an
// install's local evidence log, or the evidence.json a bundle assembler is
// about to fold into the manifest's file list).
//
// The document is written as indented JSON for a human reader; it is never
// hashed or re-derived from itself, so canonical (JCS) encoding is not
// required here the way it is for the manifest.
func NewFileSink(path string, ctx map[string]any) (Sink, error) {
	if path == "" {
		return nil, dferr.New(dferr.Usage, "evidence: NewFileSink: path is empty")
	}
	return &fileSink{Collector: NewCollector(ctx), path: path}, nil
}

type fileSink struct {
	*Collector
	path string

	// closeMu guards closed/closeErr so the first Close is the one that
	// decides, whatever order a deferred close and an explicit one run in.
	closeMu  sync.Mutex
	closed   bool
	closeErr error
}

// Emit records an event unless this sink has already been closed. A sink that
// kept accepting events after Close had declared the run's record final is a
// record that can still grow after it was published — and, because Close
// rewrote the whole file, a second Close would then quietly replace a file
// something else may already have read, hashed or (for a bundle's
// evidence.json) covered by a signed manifest.
func (s *fileSink) Emit(e Event) {
	s.closeMu.Lock()
	closed := s.closed
	s.closeMu.Unlock()
	if closed {
		return
	}
	s.Collector.Emit(e)
}

// Close writes the document. It is idempotent: the first call decides the
// file's contents and the result, and later calls report that same result
// without writing again.
func (s *fileSink) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.closeErr = s.write()
	return s.closeErr
}

func (s *fileSink) write() error {
	// s.Collector is spelled out rather than relying on promotion. fileSink
	// embeds *Collector and OVERRIDES some of what it promotes (Emit above
	// refuses events after Close), so "which layer am I calling?" is a real
	// question in this type. Naming the embedded field answers it at the
	// call site: this is the collector's own document, unfiltered.
	doc := s.Collector.Document() //nolint:staticcheck // QF1008: naming the embedded field is deliberate, see above
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		// One event that cannot be encoded must not cost the operator the
		// whole run's record. The NDJSON sink already drops just the bad
		// line and keeps streaming; this sink used to write nothing at all,
		// which is the worse failure for "usually the only durable copy of
		// the run". Retry with the offending events replaced by a marker,
		// and report the original problem only if even that fails.
		doc.Events = encodableEvents(doc.Events)
		b, err = json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return dferr.Wrap(dferr.Usage, err, "evidence: encode %s", s.path)
		}
	}
	b = append(b, '\n')
	if dir := filepath.Dir(s.path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return dferr.Wrap(dferr.Environment, err, "evidence: create %s", dir)
		}
	}
	if err := os.WriteFile(s.path, b, 0o644); err != nil {
		return dferr.Wrap(dferr.Environment, err, "evidence: write %s", s.path)
	}
	return nil
}

// encodableEvents replaces every event that cannot be JSON-encoded with one
// that says so, keeping its schema, timestamp and type — the fact that a
// record existed at that moment is itself evidence, and is worth more to a
// reader than the attributes that were lost.
func encodableEvents(events []Event) []Event {
	out := make([]Event, 0, len(events))
	for _, e := range events {
		if _, err := json.Marshal(e); err != nil {
			out = append(out, Event{
				Schema: e.Schema, TS: e.TS, Type: e.Type, Level: LevelError,
				Msg:   "event dropped: its attributes could not be encoded: " + err.Error(),
				Attrs: nil,
			})
			continue
		}
		out = append(out, e)
	}
	return out
}
