package apt

// Reading the event stream of the debark process that runs INSIDE the
// container, and deciding what may be believed of it.
//
// The problem this solves. On the container backend a complete build emitted
// eleven events and not one of them came from the process that did the work:
// no apt.update, no apt.resolve, nothing between "backend selected" and
// "finished". Measured from Windows against a real Docker daemon before this
// file existed, a 61 s `build --backend container` emitted four events, all
// host-side (one snapshot.loaded, three backend.selected). That is why build
// progress on the container backend is honestly indeterminate: the host has
// nothing to report between starting `docker run` and it exiting.
//
// The route is short, and shorter than it looks. resolveEvidenceSink
// (internal/cli/cmd_resolve.go) already puts `--json-events -` on STDERR
// rather than stdout, because stdout is reserved for the plan envelope
// (docs/dev/resolve-contract.md: "diagnostics go to stderr, and --json-events
// NDJSON (when requested) also goes to stderr so it can never corrupt the
// envelope"), and execContainer already captures both streams. So this is one
// flag on the inner argv plus a line-by-line scan where there was a
// whole-buffer read. No file in /work, no tailing, no stdout multiplexing.
//
// What this file exists for is the other half: those bytes are UNTRUSTED.
// This whole driver is written on the premise that the envelope is produced
// by a process the host cannot vouch for -- containerCrossCheckEnvelope,
// containerCheckEnvelopeFilename and containerReverifyFiles all exist for
// that reason -- and an event stream is the same bytes from the same process,
// with two extra properties that make it worse:
//
//   - It is displayed. An event's Msg reaches a terminal renderer and a GUI,
//     so a control sequence in it is a terminal injection and a very long one
//     is a denial of service against whatever draws it.
//   - It is unbounded. A cooperating process emits a handful of events; a
//     hostile or broken one emits as many as it likes, as large as it likes,
//     and the host is holding them in memory.
//
// So nothing crosses without being checked against a shape this build
// defines: the schema string exactly, a type from core/evidence's own list,
// one of the three levels, a message with control characters removed and a
// length cap, and attributes limited to a bounded number of scalar values
// with bounded keys. Anything else is dropped and counted, and the count is
// reported once, by the host, in its own voice.
//
// And what is deliberately NOT here: any path from these bytes into
// evidence.json. See containerForwardEventTo's doc comment.

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/inferops/debark/core/evidence"
)

// Bounds. Every one of these is a limit on what ONE untrusted process can
// make this one hold in memory or put on a terminal; none of them is reached
// by a cooperating inner debark, whose whole resolve emits four events with
// four small attributes each.
const (
	// containerEventLineMax is the longest line this will buffer while
	// looking for a newline. Past it the accumulated bytes are handed to the
	// diagnostic buffer and the rest of the line is passed through as
	// diagnostics too: an event that large is not one this build wrote.
	containerEventLineMax = 64 << 10
	// containerEventsMax is the most events one container run may forward.
	// A real resolve emits four.
	containerEventsMax = 5000
	// containerEventMsgMax is the longest human message, in runes.
	containerEventMsgMax = 512
	// containerEventAttrsMax is the most attributes one event may carry.
	containerEventAttrsMax = 32
	// containerEventKeyMax and containerEventValueMax bound one attribute.
	containerEventKeyMax   = 64
	containerEventValueMax = 512
)

// containerEventSink is the shared, concurrency-safe destination the two
// line splitters (stdout and stderr) hand accepted events to.
//
// Two splitters and one sink because os/exec copies each stream on its own
// goroutine, and because the two inner commands do not agree on which stream
// carries the events: `resolve` sends them to stderr (resolveEvidenceSink
// reserves stdout for the envelope), while `snapshot from-base` uses the
// ordinary sink, which sends "-" to stdout. Watching both means this file
// does not have to know which command it is reading, and it does not have to
// be changed when a third one appears.
type containerEventSink struct {
	mu sync.Mutex
	// emit is the host's own sink. Never nil while a splitter is attached.
	emit func(evidence.Event)
	// forwarded, rejected and dropped are counted for the one summary the
	// host emits at the end. rejected is a line that claimed to be a
	// debark.events/v1 event and did not survive validation; dropped is a
	// valid one past containerEventsMax.
	forwarded, rejected, dropped int
}

func (s *containerEventSink) accept(e evidence.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forwarded >= containerEventsMax {
		s.dropped++
		return
	}
	s.forwarded++
	s.emit(e)
}

func (s *containerEventSink) reject() {
	s.mu.Lock()
	s.rejected++
	s.mu.Unlock()
}

// containerEventCounts is what one container run's stream produced, carried
// out on containerRunResult so the caller can report it without reaching
// into execContainer's internals.
type containerEventCounts struct {
	Forwarded int
	Rejected  int
	Dropped   int
}

// counts returns what this run produced, for the host's summary.
func (s *containerEventSink) counts() containerEventCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return containerEventCounts{Forwarded: s.forwarded, Rejected: s.rejected, Dropped: s.dropped}
}

// containerEventStream is the io.Writer execContainer attaches to one of the
// container's output streams in place of a plain bytes.Buffer. It splits the
// stream into lines, hands the ones that are debark events to the sink, and
// appends everything else to diag byte for byte.
//
// Separating the two is not a nicety. Without it, `--json-events -` would
// bury the inner process's real diagnostics under NDJSON: containerLastLines
// (used in four error paths) shows the LAST six lines of the captured output,
// and apt's actual failure message would be six event objects further up.
// With it, the diagnostic buffer contains exactly what it contained before
// this change -- everything that is not an event.
type containerEventStream struct {
	diag *bytes.Buffer
	sink *containerEventSink

	// pending is the partial line carried between Writes. os/exec copies in
	// arbitrary chunks, so a line arrives split at any byte, including the
	// middle of a UTF-8 sequence.
	pending []byte
	// overlong marks that the current line already exceeded
	// containerEventLineMax and has been flushed to diag, so the remainder
	// is passed through rather than buffered.
	overlong bool
}

func (w *containerEventStream) Write(p []byte) (int, error) {
	rest := p
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		w.consume(rest[:i+1])
		rest = rest[i+1:]
	}
	if len(rest) > 0 {
		if w.overlong {
			w.diag.Write(rest)
		} else {
			w.pending = append(w.pending, rest...)
			if len(w.pending) > containerEventLineMax {
				// Not an event this build wrote. Give up on it as an event,
				// keep it as diagnostics, and stop buffering until the line
				// ends -- an unterminated stream must not be able to grow
				// this buffer without limit.
				w.diag.Write(w.pending)
				w.pending = nil
				w.overlong = true
			}
		}
	}
	// Always the full length: this is a sink, and a short write would make
	// os/exec's copier report an error for output it was only observing.
	return len(p), nil
}

// consume handles one complete line, newline included.
func (w *containerEventStream) consume(line []byte) {
	if w.overlong {
		w.diag.Write(line)
		w.overlong = false
		w.pending = nil
		return
	}
	if len(w.pending) > 0 {
		line = append(w.pending, line...)
		w.pending = nil
	}
	ev, isEvent, ok := containerDecodeEventLine(line)
	switch {
	case !isEvent:
		w.diag.Write(line)
	case ok:
		w.sink.accept(ev)
	default:
		// It said it was a debark event and it was not one this build can
		// believe. It is not passed through to the diagnostic buffer: doing
		// that would let a hostile inner process write chosen text into an
		// error message simply by prefixing it with a schema field. It is
		// counted, and the count is reported by the host.
		w.sink.reject()
	}
}

// Close flushes a trailing partial line. A process killed mid-write leaves
// one, and it belongs in the diagnostics -- it is exactly the kind of
// truncated output that says what went wrong.
func (w *containerEventStream) Close() {
	if len(w.pending) > 0 {
		w.diag.Write(w.pending)
		w.pending = nil
	}
	w.overlong = false
}

// containerDecodeEventLine decides what one line of an inner container
// process's output is.
//
// isEvent says the line claims to be a debark.events/v1 record: it is JSON,
// it is an object, and its "schema" is exactly this build's. ok says it also
// survived validation and may be shown to a person. A line that is not an
// event at all is ordinary diagnostics and is passed through untouched --
// apt's own messages, docker's, and the `--json` envelope `snapshot
// from-base` prints, whose pretty-printed lines are not objects on their own.
func containerDecodeEventLine(line []byte) (ev evidence.Event, isEvent, ok bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return evidence.Event{}, false, false
	}
	// Decoded into a private shape rather than straight into evidence.Event:
	// Attrs must arrive as map[string]any so the composite values a hostile
	// producer can nest are visible here as []any and map[string]any and can
	// be refused. Unmarshalling into the real type would accept them.
	var raw struct {
		Schema string         `json:"schema"`
		Type   string         `json:"type"`
		Level  string         `json:"level"`
		Msg    string         `json:"msg"`
		Attrs  map[string]any `json:"attrs"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return evidence.Event{}, false, false
	}
	if raw.Schema != evidence.SchemaVersion {
		return evidence.Event{}, false, false
	}
	// From here the line is an event line whatever happens next, so a
	// failure below is a rejection and never diagnostics.

	// The type must be one core/evidence itself declares. An unknown type is
	// refused rather than passed along: the type is what every consumer
	// switches on, and inventing one is how a stream gets a renderer to do
	// something its author never considered.
	if !evidence.KnownType(raw.Type) {
		return evidence.Event{}, true, false
	}
	// Exactly the three levels, or absent. An unrecognised level is refused
	// rather than defaulted, because defaulting silently turns an unreadable
	// "error" into an "info".
	switch raw.Level {
	case "", evidence.LevelInfo, evidence.LevelWarn, evidence.LevelError:
	default:
		return evidence.Event{}, true, false
	}

	out := evidence.Event{
		Schema: evidence.SchemaVersion,
		// TS is deliberately left empty, discarding whatever the inner
		// process wrote. It is an untrusted value with no way to check it,
		// and leaving it empty means the host's own sink stamps it (the
		// Event contract: sinks "set Schema and TS when empty"), so a
		// forwarded event is timestamped when this process saw it.
		Type:  raw.Type,
		Level: raw.Level,
		Msg:   containerSafeText(raw.Msg, containerEventMsgMax),
		Attrs: containerSafeAttrs(raw.Attrs),
	}
	// Stamped last and unconditionally, overwriting anything the inner
	// process put in this key. It is the host saying where these bytes came
	// from, not the container claiming anything about itself -- and
	// core/engine's buildClockSink reads it to keep forwarded events out of
	// the bundle's own record, so it must not be forgeable in either
	// direction.
	out.Attrs[evidence.AttrForwardedFrom] = evidence.ForwardedFromContainer
	return out, true, true
}

// containerSafeAttrs returns a bounded map of scalar attributes.
//
// Scalars only. json.Unmarshal into `any` produces string, float64, bool, nil
// and -- for anything nested -- []any or map[string]any, so refusing the last
// two is the whole check. Nothing debark emits nests, and a nested value is
// unbounded in depth and size in a structure the host is about to hold and
// re-encode.
func containerSafeAttrs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	if len(in) == 0 {
		return out
	}
	// Sorted, so which attributes survive a map at the cap is a property of
	// the input rather than of Go's map iteration order.
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	// sort.Strings, not a hand-rolled loop: the key count is attacker-
	// controlled up to whatever fits in containerEventLineMax, and an O(n^2)
	// sort over it would be a denial of service reachable from one line of
	// container output.
	sort.Strings(keys)
	for _, k := range keys {
		if len(out) >= containerEventAttrsMax {
			break
		}
		if k == "" || len(k) > containerEventKeyMax || !containerSafeAttrKey(k) {
			continue
		}
		switch v := in[k].(type) {
		case string:
			out[k] = containerSafeText(v, containerEventValueMax)
		case bool, float64, nil:
			out[k] = v
		default:
			// []any, map[string]any, and anything a future encoding adds.
		}
	}
	return out
}

// containerSafeAttrKey accepts the shape every attribute key in this project
// actually has: lowercase words joined by underscores, dots or dashes. A key
// is a JSON object key that reaches a UI and a log, so it is allow-listed
// rather than sanitised -- there is no useful key this refuses.
func containerSafeAttrKey(k string) bool {
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// containerSafeText makes one untrusted string fit to put in front of a
// person: valid UTF-8, no control characters, and no longer than maxRunes
// runes.
//
// Control characters are replaced by a space rather than removed, so a string
// built to hide something ("done\r\nERROR: nothing to see") cannot close up
// into a shorter, cleaner-looking sentence than it really was. The point of
// removing them is that an event message is rendered by a terminal and by a
// WebView, and an ESC there is a cursor-control sequence, not a character.
func containerSafeText(s string, maxRunes int) string {
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for _, r := range s {
		if n >= maxRunes {
			b.WriteString("...")
			break
		}
		switch {
		case r == utf8.RuneError:
			continue
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
		n++
	}
	return b.String()
}

// containerForwardEventTo builds the hook execContainer calls for each
// accepted event, or nil when there is nowhere to send one.
//
// nil matters: execContainer with a nil hook behaves exactly as it did before
// this change -- a plain bytes.Buffer on each stream, no line splitting, no
// flag on the inner argv -- so a caller that does not forward cannot have its
// captured output changed by this file. ClosedWorld is that caller, and it is
// not an oversight: its captured stderr is digested into lock.ClosedWorld,
// which reaches lock.json, the lock digest, the bundle id and the signature
// (see the CommandDigest note in container.go). Nothing that varies with what
// a second process chose to narrate belongs in that path, and a closed-world
// check has no progress to report anyway.
//
// # What forwarded events are, and are not
//
// They go to the operator's LIVE stream and they do not go into the bundle.
// core/engine's buildClockSink drops every event carrying
// evidence.AttrForwardedFrom before it reaches the collector that becomes
// evidence.json, which the manifest hashes and the signature covers. Two
// separate reasons, either of which would be enough:
//
//   - Determinism. 5d26dac had just found that core/fetch's 250 ms wall-clock
//     progress events made any build with a vendor URL unreproducible: same
//     bytes, 2 events when the link was fast and 9 when it was slow,
//     evidence.json 3450 bytes against 5220, two bundle ids for one output.
//     A live stream from a SECOND process is the same trap one layer out, and
//     worse: its event count, wording and attributes are properties of
//     whichever debark binary happened to be mounted, not of the request.
//   - Provenance. evidence.json is the record an auditor reads in five years.
//     A claim made by a process the host has spent this entire file refusing
//     to trust does not belong in it under the same schema, and beside the
//     host's own observations, with nothing but an attribute to tell them
//     apart.
//
// Nothing is lost that was there before: no inner event has ever reached
// evidence.json, because none has ever crossed the boundary at all. What the
// bundle records about the container run is unchanged -- the plan, the lock,
// the resolver identity and the image digest, all host-side conclusions.
func containerForwardEventTo(sink evidence.Sink) func(evidence.Event) {
	if sink == nil {
		return nil
	}
	return sink.Emit
}

// containerEmitForwardSummary reports, once and in the host's own voice, what
// the inner process's stream did -- including the parts of it that were
// refused.
//
// Silently dropping is how a validator becomes invisible. If a change to the
// inner binary starts emitting a type this one does not know, or an operator
// is looking at a stream that is being truncated at the cap, the only place
// that can say so is here: the events themselves never arrived.
func containerEmitForwardSummary(sink evidence.Sink, c containerEventCounts) {
	if sink == nil || (c.Rejected == 0 && c.Dropped == 0) {
		return
	}
	evidence.Warn(sink, evidence.TypeWarning,
		"some events from the debark process inside the container were not forwarded",
		map[string]any{
			"code":               "container-events-refused",
			"forwarded":          c.Forwarded,
			"rejected":           c.Rejected,
			"dropped_over_limit": c.Dropped,
			"limit":              containerEventsMax,
			// Marked like the events it is about, so it shares their fate:
			// this is a statement about a stream that never enters the
			// bundle, and it must not enter it either.
			evidence.AttrForwardedFrom: evidence.ForwardedFromContainer,
		})
}
