package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNDJSONSinkWritesOneCompactObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	s := NewNDJSONSink(&buf)

	Emit(s, TypeFetchFile, "fetched libfoo", map[string]any{"name": "libfoo"})
	Emit(s, TypeAPTUpdate, "apt update", nil)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}
	for _, line := range lines {
		if strings.Contains(line, "\n") {
			t.Fatalf("line contains embedded newline: %q", line)
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line is not valid JSON: %v: %q", err, line)
		}
		if e.Schema != SchemaVersion {
			t.Errorf("schema = %q, want %q", e.Schema, SchemaVersion)
		}
		if e.TS == "" {
			t.Error("ts is empty")
		}
		// Compact: no indentation inserted.
		if strings.Contains(line, "  ") {
			t.Errorf("line looks indented, want compact: %q", line)
		}
	}
	if !strings.Contains(lines[0], `"fetch.file"`) {
		t.Errorf("first line missing type: %q", lines[0])
	}
}

func TestNDJSONSinkConcurrentEmitIsSafe(t *testing.T) {
	var buf syncBuffer
	s := NewNDJSONSink(&buf)

	var wg sync.WaitGroup
	const n = 200
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			Emit(s, TypeProgress, "", map[string]any{"i": i})
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("got %d lines, want %d (a torn write would merge or split lines)", len(lines), n)
	}
	for _, line := range lines {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("torn write produced invalid JSON line: %v: %q", err, line)
		}
	}
}

// syncBuffer serialises writes so the test isolates ndjsonSink's own locking
// from a data race in the buffer itself.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// erroringWriter always fails, to prove the NDJSON sink drops write errors
// instead of surfacing them.
type erroringWriter struct{}

func (erroringWriter) Write(p []byte) (int, error) { return 0, errors.New("boom") }

func TestNDJSONSinkDropsWriteErrors(t *testing.T) {
	s := NewNDJSONSink(erroringWriter{})
	// Must not panic and Emit returns nothing to check; the point is that a
	// broken pipe never fails the caller.
	Emit(s, TypeWarning, "irrelevant", nil)
	if err := s.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

func TestNDJSONSinkCloseDoesNotCloseWriter(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "ndjson-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	s := NewNDJSONSink(f)
	Emit(s, TypeWarning, "hello", nil)
	if err := s.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	// The sink must not have closed f: writing again must still work.
	if _, err := f.WriteString("still open\n"); err != nil {
		t.Fatalf("file was closed by sink.Close(): %v", err)
	}
}

func TestFileSinkWritesDocumentOnClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "evidence.json")

	ctx := map[string]any{"tool_version": "test"}
	s, err := NewFileSink(path, ctx)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}

	Emit(s, TypeBuildStarted, "build started", map[string]any{"n": 1})
	Emit(s, TypeBuildFinished, "build finished", map[string]any{"n": 2})

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("evidence file was not written: %v", err)
	}
	var doc Document
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("evidence file is not valid JSON: %v", err)
	}
	if doc.Schema != SchemaVersion {
		t.Errorf("schema = %q, want %q", doc.Schema, SchemaVersion)
	}
	if doc.CreatedAt == "" {
		t.Error("created_at is empty")
	}
	if len(doc.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(doc.Events))
	}
	if doc.Events[0].Type != TypeBuildStarted || doc.Events[1].Type != TypeBuildFinished {
		t.Errorf("events out of order or wrong: %+v", doc.Events)
	}
	if v, _ := doc.Context["tool_version"].(string); v != "test" {
		t.Errorf("context not preserved: %+v", doc.Context)
	}
}

func TestFileSinkReportsWriteFailure(t *testing.T) {
	// A path with a nonexistent, uncreatable parent (using a file as a
	// "directory") makes the final write fail, and that failure must come
	// back from Close rather than being swallowed.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "evidence.json")

	s, err := NewFileSink(path, nil)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	Emit(s, TypeWarning, "x", nil)
	if err := s.Close(); err == nil {
		t.Fatal("Close() = nil, want an error because the parent directory could not be created")
	}
}

func TestNewFileSinkRejectsEmptyPath(t *testing.T) {
	if _, err := NewFileSink("", nil); err == nil {
		t.Fatal("NewFileSink(\"\", nil) = nil error, want an error")
	}
}

func TestEmitToleratesNilSink(t *testing.T) {
	// Must not panic.
	Emit(nil, TypeWarning, "x", nil)
	Warn(nil, TypeWarning, "x", nil)
}

// closeErrSink is a Sink whose Close fails with a given error.
type closeErrSink struct{ err error }

func (closeErrSink) Emit(Event)     {}
func (s closeErrSink) Close() error { return s.err }

// TestMultiSinkCloseReportsEveryFailure. MultiSink.Close used to keep only
// the FIRST error and drop the rest, which inverts the one asymmetry this
// package's contract states out loud: a slow or failing sink drops events
// "except for the file sink's final Close, whose error is reported", and
// NewFileSink adds that its Close error "must not be swallowed, because it is
// usually the only durable copy of the run".
//
// A MultiSink is precisely where that copy sits next to a terminal renderer
// and an NDJSON stream, so first-wins meant a cosmetic failure earlier in the
// slice silently replaced "could not write evidence.json" — the run's record
// lost, and the operator told about a progress line instead.
func TestMultiSinkCloseReportsEveryFailure(t *testing.T) {
	cosmetic := errors.New("progress: could not finish the line")
	durable := errors.New("evidence: write evidence.json: no space left on device")

	// The durable failure is deliberately LAST, which is the order that used
	// to lose it.
	m := MultiSink{closeErrSink{cosmetic}, Discard{}, closeErrSink{durable}}
	err := m.Close()
	if err == nil {
		t.Fatal("MultiSink.Close reported success while two sinks failed")
	}
	if !errors.Is(err, durable) {
		t.Errorf("the durable-copy failure was swallowed: Close() = %v", err)
	}
	if !errors.Is(err, cosmetic) {
		t.Errorf("the first failure was dropped: Close() = %v", err)
	}

	if err := (MultiSink{Discard{}, nil, Discard{}}).Close(); err != nil {
		t.Errorf("Close of sinks that all succeed returned %v, want nil", err)
	}
}

// TestMultiSinkCloseClosesEverySink: reporting more errors must not stop the
// loop early — every sink still gets its Close, or a file is left unwritten
// because something before it failed.
func TestMultiSinkCloseClosesEverySink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "late.json")
	fs, err := NewFileSink(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	fs.Emit(New("warning", "recorded", nil))

	m := MultiSink{closeErrSink{errors.New("boom")}, fs}
	if err := m.Close(); err == nil {
		t.Fatal("expected the failing sink's error")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file sink after a failing one was never closed: %v", err)
	}
}
