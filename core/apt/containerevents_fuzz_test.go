package apt

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/inferops/debark/core/evidence"
)

// FuzzContainerDecodeEventLine fuzzes the one function in this package whose
// entire input is bytes written by a process the host has decided not to
// trust.
//
// It asserts invariants rather than outputs, because there is no expected
// answer for an arbitrary line -- only a set of properties that must hold
// whatever comes back, since what comes back is put in front of a person and
// re-encoded as JSON.
func FuzzContainerDecodeEventLine(f *testing.F) {
	f.Add(`{"schema":"debark.events/v1","type":"apt.update","msg":"apt-get update","attrs":{"failed":false,"entries":16}}`)
	f.Add(`{"schema":"debark.events/v1","type":"apt.resolve","attrs":{"selections":3,"unresolved":0}}`)
	// The JSON spelling of ESC, not a raw byte: a raw control character is
	// not legal inside a JSON string, so this is how a producer that wanted
	// one would have to write it -- and it keeps a control character out of
	// this file's own source, which staticcheck ST1018 rightly objects to.
	f.Add(`{"schema":"debark.events/v1","type":"warning","level":"warn","msg":"\u001b[31mred\u001b[0m"}`)
	f.Add(`{"schema":"debark.events/v1","type":"apt.update","attrs":{"a":{"b":[1,2,{"c":3}]}}}`)
	f.Add(`{"schema":"debark.events/v1","type":"apt.update","attrs":{"forwarded_from":"lies"}}`)
	f.Add(`{"schema":"debark.events/v2","type":"apt.update"}`)
	f.Add(`E: Unable to locate package nosuchpkg`)
	f.Add(`{`)
	f.Add(``)
	f.Add("{\"schema\":\"debark.events/v1\",\"type\":\"warning\",\"msg\":\"\xff\xfe\"}")

	f.Fuzz(func(t *testing.T, line string) {
		ev, isEvent, ok := containerDecodeEventLine([]byte(line))
		if !ok {
			// Nothing else is promised about a refused line, except that a
			// refusal must not hand back a usable event.
			if ev.Type != "" || ev.Msg != "" || len(ev.Attrs) != 0 {
				t.Fatalf("a refused line produced a populated event (isEvent=%v): %+v", isEvent, ev)
			}
			return
		}
		if !isEvent {
			t.Fatalf("ok without isEvent, which no caller expects: %+v", ev)
		}

		if ev.Schema != evidence.SchemaVersion {
			t.Errorf("Schema = %q", ev.Schema)
		}
		if ev.TS != "" {
			t.Errorf("TS = %q, want empty: the inner clock is not checkable", ev.TS)
		}
		if !evidence.KnownType(ev.Type) {
			t.Errorf("Type = %q, which core/evidence does not declare", ev.Type)
		}
		switch ev.Level {
		case "", evidence.LevelInfo, evidence.LevelWarn, evidence.LevelError:
		default:
			t.Errorf("Level = %q", ev.Level)
		}

		// Msg: displayable. No control characters, valid UTF-8, bounded.
		if !utf8.ValidString(ev.Msg) {
			t.Errorf("Msg is not valid UTF-8: %q", ev.Msg)
		}
		for _, r := range ev.Msg {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				t.Fatalf("Msg carries control rune %U, which a terminal reads as a command: %q", r, ev.Msg)
			}
		}
		if n := len([]rune(ev.Msg)); n > containerEventMsgMax+3 {
			t.Errorf("Msg is %d runes, past the cap", n)
		}

		// Provenance: stamped by the host, never by the line.
		if ev.Attrs[evidence.AttrForwardedFrom] != evidence.ForwardedFromContainer {
			t.Fatalf("provenance is %v, not the host's stamp: core/engine drops events on exactly this key, so a "+
				"line that can change it can put itself into a signed document", ev.Attrs[evidence.AttrForwardedFrom])
		}

		// Attrs: bounded, scalar, with keys that are keys.
		if len(ev.Attrs) > containerEventAttrsMax+1 {
			t.Errorf("Attrs has %d entries, past the cap", len(ev.Attrs))
		}
		for k, v := range ev.Attrs {
			if k == "" || len(k) > containerEventKeyMax || !containerSafeAttrKey(k) {
				t.Errorf("attribute key %q survived", k)
			}
			switch val := v.(type) {
			case bool, float64, nil:
			case string:
				if !utf8.ValidString(val) {
					t.Errorf("attribute %q is not valid UTF-8", k)
				}
				if n := len([]rune(val)); n > containerEventValueMax+3 {
					t.Errorf("attribute %q is %d runes, past the cap", k, n)
				}
				for _, r := range val {
					if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
						t.Fatalf("attribute %q carries control rune %U: %q", k, r, val)
					}
				}
			default:
				t.Errorf("attribute %q has non-scalar type %T", k, v)
			}
		}

		// And the whole thing must survive the trip it is about to take: it
		// is marshalled by every sink downstream.
		if _, err := json.Marshal(ev); err != nil {
			t.Errorf("the forwarded event does not re-encode: %v", err)
		}
	})
}

// FuzzContainerEventStream fuzzes the splitter over arbitrary bytes arriving
// in arbitrary chunks, which is what os/exec actually hands it.
//
// The invariant that matters is conservation: every byte the container wrote
// must either be part of a line that became an event or still be in the
// diagnostic buffer. A splitter that silently swallowed output would lose the
// apt error message the driver's own failure paths quote.
func FuzzContainerEventStream(f *testing.F) {
	f.Add("Get:1 http://archive.ubuntu.com noble InRelease\n"+
		`{"schema":"debark.events/v1","type":"apt.update"}`+"\nReading package lists...\n", 20)
	f.Add("no newline at all", 3)
	f.Add("\n\n\n", 1)
	f.Add(`{"schema":"debark.events/v1","type":"apt.update"}`, 7)

	f.Fuzz(func(t *testing.T, stream string, chunk int) {
		if chunk <= 0 {
			chunk = 1
		}
		var diag bytes.Buffer
		var events []evidence.Event
		sink := &containerEventSink{emit: func(e evidence.Event) { events = append(events, e) }}
		w := &containerEventStream{diag: &diag, sink: sink}

		for i := 0; i < len(stream); i += chunk {
			end := i + chunk
			if end > len(stream) {
				end = len(stream)
			}
			n, err := w.Write([]byte(stream[i:end]))
			if err != nil {
				t.Fatalf("Write returned %v; os/exec's copier would report that as a failure of the run", err)
			}
			if n != end-i {
				t.Fatalf("Write reported %d of %d bytes; a short write is a copier error", n, end-i)
			}
			if len(w.pending) > containerEventLineMax {
				t.Fatalf("pending grew to %d bytes, past the %d cap: an unterminated stream must not be able to "+
					"grow this buffer without limit", len(w.pending), containerEventLineMax)
			}
		}
		w.Close()

		if len(w.pending) != 0 {
			t.Errorf("Close left %d bytes buffered: the last thing the container said was dropped", len(w.pending))
		}
		// Conservation. Every byte is either in the diagnostics or was part
		// of a line the decoder claimed. The counts say how many lines it
		// claimed; the diagnostics must hold everything else.
		consumed := sink.forwarded + sink.rejected + sink.dropped
		if consumed == 0 && diag.Len() != len(stream) {
			t.Errorf("no line was consumed as an event, but the diagnostics hold %d of %d bytes: output was lost",
				diag.Len(), len(stream))
		}
		if diag.Len() > len(stream) {
			t.Errorf("the diagnostics hold %d bytes for a %d-byte stream: output was duplicated", diag.Len(), len(stream))
		}
		if consumed > strings.Count(stream, "\n")+1 {
			t.Errorf("consumed %d event lines from a stream with %d newlines", consumed, strings.Count(stream, "\n"))
		}
	})
}
