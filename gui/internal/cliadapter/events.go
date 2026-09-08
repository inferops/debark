package cliadapter

// events.go is the implementing package: Adapter.Build, and the NDJSON event stream behind
// it. It owns exactly one interface method and the machinery that method
// needs; invoke.go owns process execution for every other command and
// errors.go owns classify.
//
// The problem this file exists to solve is C2 in docs/dev/cli-surface.md.
// `--json-events -` writes NDJSON to stdout, which `--json` is also using,
// and the result object is MarshalIndent'ed and multi-line while the events
// are compact and one per line — so line-by-line parsing of a shared stdout
// mangles the one payload the operator most needs. The decision, frozen in
// EventsDestination, is to give the event stream its own destination: a
// temporary file the adapter reserves, passes as --json-events, tails while
// the build runs, drains after it exits, and removes. stdout stays a clean
// json.Decoder input.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
)

// eventsPollInterval is how often the tailer looks for new bytes in the
// events file while the build is running.
//
// 100 ms is chosen against the only thing on the wire that has a rate:
// core/fetch throttles byte progress to once per 250 ms per file, so this
// reads more than twice as often as the fastest producer, and the most a poll
// can add to an event's age is one interval. The cost is a handful of reads a
// second against a file the writer just touched, which the page cache serves
// — invisible next to a build that downloads packages. Never a busy loop.
//
// A var rather than a const so a test can slow it down to a value no build
// would ever reach, and prove that the drain after the process exits is what
// delivers the last events rather than a lucky tick.
var eventsPollInterval = 100 * time.Millisecond

const (
	// eventsReadChunkBytes is one tail read. Comfortably larger than any
	// plausible event line, so a burst is usually a single syscall.
	eventsReadChunkBytes = 64 << 10

	// eventsMaxLineBytes bounds the carry-over buffer for an incomplete line.
	// A truncated or corrupt file can contain megabytes with no newline in
	// them, and this buffer lives in the GUI's own address space; past the
	// ceiling the line is counted as malformed and skipped to the next
	// newline, which is what every other unreadable line gets.
	eventsMaxLineBytes = 4 << 20

	// eventsWaitDelay bounds how long Wait may block on the output pipes
	// AFTER the process itself has exited. The container backend can leave a
	// runtime child holding them, and without a delay a killed build would
	// hang the caller on a descendant of a process that is already dead.
	eventsWaitDelay = 5 * time.Second

	// eventsStderrLimit is how much stderr Build keeps. Deliberately above
	// MaxStderrBytes so that Error's own truncation marker still fires on
	// anything that overflowed: an operator reading a message that stops
	// mid-sentence should be told it was cut.
	eventsStderrLimit = MaxStderrBytes + 4<<10

	// eventsNoExitStatus is the code used when the process never produced
	// one. Outside 0..7, so NewError maps it to dferr.Environment: a failure
	// that never reached a debark exit path is a fact about the machine.
	eventsNoExitStatus = -1
)

// eventsTailersLive counts tail goroutines that have started and not yet
// returned. Build's contract is that it never outlives its own goroutine, and
// a counter makes that checkable instead of merely stated: a test asserts it
// is zero once Build has returned, whatever path it took.
var eventsTailersLive atomic.Int64

// Build runs a build to completion, delivering every NDJSON event to sink as
// it arrives.
//
// The result and the error are NOT mutually exclusive, and this is the method
// where that matters: `build` prints its --json document and THEN exits with
// a non-zero class, so an incomplete build (exit 3) hands back a fully
// populated *buildjob.BuildResult alongside a *Error. Unresolved and
// FetchFailed are the only place that detail exists. The same is true of
// exits 5 and 6, so the rule here is uniform: whenever stdout carried a
// result, the caller gets it.
//
// sink is called from one goroutine and one only, so events reach it in
// emission order. It MUST NOT BLOCK — no lock another goroutine holds, no
// unbuffered channel, no I/O. Nothing here holds a lock across the call, so a
// sink that stalls stalls only the tail; but it stalls it for the whole
// build, and a stalled tail is a build screen that has stopped moving.
func (a *adapter) Build(ctx context.Context, spec BuildSpec, sink EventSink) (*buildjob.BuildResult, error) {
	// argv is what the operator would type, and what every error carries. It
	// is BuildArgv's output verbatim, with no --json-events in it: the real
	// command line adds one flag naming a temporary file, that path is this
	// adapter's own business, and Error.Argv is an exported accessor with a
	// "copy the command" button behind it. The temp path never leaves this
	// function.
	argv := BuildArgv(spec)

	// Validate first: the UI gets a sentence a person can act on instead of
	// cobra's usage block rendered into a details drawer.
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	// Through binary(), never a.bin directly: discovery is lazy and mutex
	// guarded, and a machine with no debark gets invoke.go's sentence about
	// installing it rather than an exec error from this file.
	//
	// Located BEFORE anything else is set up, because everything below —
	// the capability question, the temp file, the tail goroutine — is work on
	// behalf of a binary that has to exist first.
	bin, locErr := a.binary()
	if locErr != nil {
		return nil, locErr
	}

	// THE CAPABILITY CHECK, and the reason it is not an unconditional flag.
	// cobra does not shrug at an unknown flag: a debark that predates
	// --json-events answers it with an error and its usage block, exit 1. So
	// passing it unconditionally made every build against such a binary die on
	// a usage error, instead of running with no progress stream — which is the
	// degraded mode Capabilities.JSONEvents exists to describe. When the flag
	// is absent there is nothing to tail, so the temp file and the goroutine
	// are not created either; the caller's sink is simply never called, and the
	// build screen shows the indeterminate spinner it is told to show through
	// AppInfo.progress_events.
	stopTail := func() {}
	runArgv := argv
	if a.invokeJSONEventsSupported(ctx) {
		evPath, removeEvents, err := eventsNewTempFile()
		if err != nil {
			return nil, classify(argv, eventsNoExitStatus, "").
				WithCause(err).
				WithSummary("could not reserve a file for debark's event stream: %v", err)
		}
		// Registered FIRST so it runs LAST. Deferred cleanup is LIFO, so the
		// tailer's read handle is closed before the file is removed — which
		// matters on Windows, and reads better than either ordering by accident.
		defer removeEvents()

		tail := &eventsTailer{path: evPath, sink: sink}
		stopTail = tail.start()
		// Both deferred and called on the normal path below. stopTail is
		// idempotent; deferring it as well means a panic between here and there
		// cannot leave a goroutine reading a file nobody will ever remove.
		defer stopTail()

		runArgv = buildArgvWithEvents(argv, evPath)
	}

	// Spawned and managed here rather than through invoke.go's runner: a
	// streaming build needs its own process handling — a kill that a tail can
	// survive, a bounded wait for the pipes, and a drain that happens after
	// the wait.
	cmd := exec.CommandContext(ctx, bin, runArgv[1:]...)
	var stdout bytes.Buffer
	stderr := &eventsStderrBuffer{limit: eventsStderrLimit}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	// A nil Stdin gives the child the null device. --json already guarantees
	// debark never prompts; a child that reads anyway gets EOF rather than
	// waiting on a terminal that is not there.
	cmd.Stdin = nil
	// CommandContext kills the process when ctx ends; WaitDelay bounds what
	// happens next. See eventsWaitDelay.
	cmd.WaitDelay = eventsWaitDelay

	if err := cmd.Start(); err != nil {
		return nil, classify(argv, eventsNoExitStatus, stderr.String()).
			WithCause(err).
			WithSummary("could not run %s: %v", bin, err)
	}

	// Wait returns once the process has exited — including when ctx ended and
	// CommandContext killed it — so by the time it returns, every byte
	// debark will ever write to the events file is on disk.
	waitErr := cmd.Wait()

	// THE DRAIN, and the reason it is on its own line with its own comment.
	// stopTail signals the tailer and blocks until it has read the events
	// file to EOF one final time. A build that emits its last events and
	// exits inside one poll interval is the normal case, not the exotic one,
	// and those last events — bundle.assembled, the closing fetch.file
	// records — are exactly the ones that say what the build produced.
	// Returning before this would silently lose them.
	stopTail()

	result, decodeErr := buildDecodeResult(stdout.Bytes())

	if ctx.Err() != nil {
		// The operator pressed Stop, or the app is shutting down. Whatever
		// status the killed process returned is a consequence of the kill and
		// not a diagnosis, so reporting it as a failure would be a lie. The
		// events already delivered stand, and so does a result if debark
		// managed to print one before it died.
		return result, NewCanceledError(argv)
	}

	if waitErr == nil {
		if decodeErr != nil {
			// debark succeeded and the adapter could not use its output.
			// NewError documents exit code 0 as exactly this case.
			return nil, classify(argv, int(dferr.Success), stderr.String()).
				WithCause(decodeErr).
				WithSummary("debark finished but its --json result could not be read: %v", decodeErr)
		}
		return result, nil
	}

	// A failure. classify owns the message, including the one caveat C1 warns
	// about: exits 3, 5 and 6 out of `build` print the result and then return
	// a SILENT error, so stderr is empty and the exit code is all there is.
	// errors.go carries a rule for exactly that shape, so nothing here
	// rewrites the summary from the result — the lists in Unresolved and
	// FetchFailed are the caller's to render, and two packages competing over
	// one sentence would be a worse error message, not a better one.
	failure := classify(argv, buildExitStatus(waitErr), stderr.String())

	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		// No exit status at all: WaitDelay expired with a descendant still
		// holding the pipes, or the wait itself failed. classify sees only a
		// number, and for a process that DID exit, "debark was killed" —
		// which is what the out-of-table floor says — would be wrong.
		failure = failure.WithCause(waitErr).
			WithSummary("debark did not finish cleanly: %v", waitErr)
	}

	return result, failure
}

// buildArgvWithEvents returns argv with "--json-events PATH" inserted: the one
// difference between the command the operator is shown and the command that
// actually runs.
//
// The insertion point is load-bearing. BuildArgv emits --json last among the
// flags and then, when the spec has positional inputs, a "--" separator
// followed by those inputs. Appending would put the flag AFTER the separator,
// where cobra reads it as a package name called "--json-events" — a bug that
// shows up only on specs that have positional arguments, which is every real
// build. So the flag goes immediately after the last "--json" in the vector.
// That element is always BuildArgv's own: every flag value that could
// coincidentally equal "--json" is emitted before it, and no positional can
// equal it, because they all carry an apt:, url: or file: prefix.
func buildArgvWithEvents(argv []string, path string) []string {
	at := len(argv)
	for i := len(argv) - 1; i >= 0; i-- {
		if argv[i] == "--json" {
			at = i + 1
			break
		}
	}
	out := make([]string, 0, len(argv)+2)
	out = append(out, argv[:at]...)
	out = append(out, "--json-events", path)
	return append(out, argv[at:]...)
}

// eventsNewTempFile reserves the path --json-events will be pointed at and
// returns the function that removes it.
//
// It creates the file and immediately closes it, rather than only inventing a
// name: os.CreateTemp is what makes the name unique against every other build
// on the machine, and debark opens the path itself with os.Create, which
// truncates whatever is there. An empty file this process no longer holds
// open is exactly the handover the CLI expects, on both platforms.
//
// A var rather than a plain func for the same reason debark's own
// openEventsFile is one: it is the seam a test needs in order to know which
// file a build is about to use, and to prove the file is gone afterwards.
var eventsNewTempFile = func() (path string, remove func(), err error) {
	f, err := os.CreateTemp("", "debark-events-*.ndjson")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(name)
		return "", nil, cerr
	}
	return name, func() { _ = os.Remove(name) }, nil
}

// eventsStderrBuffer collects stderr with a hard ceiling.
//
// A failing `build` writes almost nothing to stderr and a usage error writes
// cobra's entire help text, but neither is a bound: a backend that loops can
// print without limit, into the GUI's own address space.
type eventsStderrBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *eventsStderrBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	// Always report the whole length. A short write is an I/O error to the
	// child, and losing the tail of a help screen must not look like one.
	return n, nil
}

func (b *eventsStderrBuffer) String() string { return b.buf.String() }

// buildDecodeResult reads the --json document off stdout.
//
// A plain json.Decoder, which is the entire point of not passing
// --json-events "-": stdout carries one MarshalIndent'ed buildjob.BuildResult
// and nothing else, so there is no framing question here to get wrong.
func buildDecodeResult(stdout []byte) (*buildjob.BuildResult, error) {
	if len(bytes.TrimSpace(stdout)) == 0 {
		return nil, errors.New("debark printed no JSON on stdout")
	}
	var r buildjob.BuildResult
	if err := json.NewDecoder(bytes.NewReader(stdout)).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// buildExitStatus is the number classify branches on.
//
// An *exec.ExitError knows it, including the -1 os/exec reports for a process
// killed by a signal; anything else means the process never produced a status
// at all. Both land outside 0..7, which NewError maps to dferr.Environment.
func buildExitStatus(err error) int {
	if err == nil {
		return int(dferr.Success)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return eventsNoExitStatus
}

// ---------------------------------------------------------------------------
// The tail
// ---------------------------------------------------------------------------

// eventsTailer reads the NDJSON file debark is writing, one line at a time,
// and hands each decoded event to a sink.
//
// It is deliberately incurious about content, because every robustness rule
// in cli-surface.md §5 is "accept it and carry on": NDJSON writes are best
// effort and debark silently drops an event it cannot encode, four declared
// event types are never emitted at all, and a crashed run leaves a file whose
// last line is half written. A gap is normal and is never an error. A line
// that will not decode is counted and skipped.
type eventsTailer struct {
	path string
	// sink may be nil, in which case events are decoded and discarded — the
	// interface documents that, and decoding anyway keeps the malformed-line
	// count honest.
	sink EventSink

	f     *os.File
	chunk []byte
	// line holds the bytes of an incomplete final line, carried to the next
	// read. The writer is another process: a read lands wherever it lands,
	// and half a JSON object must never be decoded.
	line []byte
	// skipping is set when one line grew past eventsMaxLineBytes; the rest of
	// it is discarded up to the next newline.
	skipping bool

	// Counters, written only by the tail goroutine and read only after stop
	// has returned — whose channel receive is the happens-before that makes
	// reading them safe.
	delivered int
	malformed int
}

// start launches the tail and returns the function that ends it.
//
// The returned function closes the tailer down and BLOCKS until the goroutine
// has read the file to EOF one final time. That is the entire contract: call
// it once the process has exited, and every event debark wrote has reached
// the sink by the time it returns. It is idempotent, so a caller can both
// defer it and call it on the normal path.
func (t *eventsTailer) start() func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	eventsTailersLive.Add(1)
	go func() {
		// LIFO: the live counter drops before finished closes, so a test that
		// reads the counter after stop returns cannot race the goroutine.
		defer close(finished)
		defer eventsTailersLive.Add(-1)
		t.run(done)
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-finished
	}
}

func (t *eventsTailer) run(done <-chan struct{}) {
	defer t.closeFile()
	tick := time.NewTicker(eventsPollInterval)
	defer tick.Stop()
	for {
		// done is checked before the ticker rather than alongside it, so a
		// stop is never deferred by a random select. The last iteration is
		// always a drain followed by a return.
		stopped := false
		select {
		case <-done:
			stopped = true
		default:
			select {
			case <-done:
				stopped = true
			case <-tick.C:
			}
		}
		t.drain()
		if stopped {
			return
		}
	}
}

// drain reads everything available right now, delivering every complete line
// it finds. On the final call — after the writer has exited — "everything
// available" is the whole rest of the file.
func (t *eventsTailer) drain() {
	if t.f == nil {
		f, err := os.Open(t.path)
		if err != nil {
			// debark has not opened the path yet, or never will: a build
			// that fails before it builds its evidence sink never creates the
			// file at all. Neither is an error, and neither is worth a log
			// line every poll.
			return
		}
		t.f = f
	}
	if t.chunk == nil {
		t.chunk = make([]byte, eventsReadChunkBytes)
	}
	for {
		n, err := t.f.Read(t.chunk)
		if n > 0 {
			t.consume(t.chunk[:n])
		}
		if err != nil || n == 0 {
			// io.EOF means "nothing more right now", not "end of the stream":
			// the file grows under us and the next read continues from the
			// same offset.
			return
		}
	}
}

// consume splits a freshly read chunk into lines, keeping any incomplete tail
// for the next read.
func (t *eventsTailer) consume(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			t.stash(b)
			return
		}
		line := b[:i]
		b = b[i+1:]
		if t.skipping {
			// The tail of a line that already blew the ceiling.
			t.skipping = false
			t.line = t.line[:0]
			continue
		}
		if len(t.line) > 0 {
			t.line = append(t.line, line...)
			line = t.line
		}
		t.deliver(line)
		t.line = t.line[:0]
	}
}

// stash keeps an incomplete line for the next read, or gives up on it.
func (t *eventsTailer) stash(b []byte) {
	if t.skipping {
		return
	}
	if len(t.line)+len(b) > eventsMaxLineBytes {
		// Not an event: a truncated write, a corrupt file, or something that
		// is not NDJSON at all. Count it like any other unreadable line and
		// resynchronise at the next newline.
		t.malformed++
		t.skipping = true
		t.line = t.line[:0]
		return
	}
	t.line = append(t.line, b...)
}

// deliver decodes one complete line and hands it to the sink.
func (t *eventsTailer) deliver(line []byte) {
	trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r"))
	if len(trimmed) == 0 {
		// A blank line is not a dropped event; NDJSON writers produce them at
		// the end of a file and nowhere else.
		return
	}
	if trimmed[0] != '{' {
		// One line is one JSON object. A scalar, an array or a "null" would
		// unmarshal into a zero Event and be delivered as a real one, which
		// is worse than being skipped.
		t.malformed++
		return
	}
	var e Event
	if err := json.Unmarshal(trimmed, &e); err != nil {
		t.malformed++
		return
	}
	t.delivered++
	// The sink is called from this goroutine and this goroutine only, so
	// events reach it in emission order; and no lock is held across the call,
	// because EventSink's contract says it must not block and the tail is
	// what stalls when it does. Unknown and never-emitted types go through
	// untouched: cli-surface.md §5.1 says accept them and never wait for one.
	if t.sink != nil {
		t.sink(e)
	}
}

func (t *eventsTailer) closeFile() {
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
}
