// Command matrix is the integration-matrix runner: it loads every fixture under test/e2e/fixtures, expands each into one
// or more (release x arch) rows per harness.TargetSpec.Matrix, runs them
// with a bounded worker pool, and writes a machine-readable JSON result plus
// a short human summary table — the artefact a nightly CI job publishes.
//
// Usage:
//
//	go run ./hack/matrix                          # every row
//	go run ./hack/matrix --fixture alt-deps        # one fixture, its own release
//	go run ./hack/matrix --release debian-12       # every fixture that targets/matrixes onto Debian 12
//	go run ./hack/matrix --workers 2 --out result.json
//	go run ./hack/matrix --skip-env-preflight      # run even if the host's apt looks broken
//
// It never needs DEBARK_E2E: that guard exists to keep `go test ./...`
// container-free, and this binary is never invoked by `go test` — running it
// at all is already an explicit, deliberate choice to start containers.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/test/e2e"
	"github.com/inferops/debark/test/e2e/harness"
)

// Process exit codes. 0 and 1 are what a nightly job has always keyed on; 3
// and 130 are new and exist so a host problem is never mistaken for a
// product problem — the exact confusion the 2026-09-03 invalid run caused
// (hack/matrix/results/README.md).
const (
	exitOK          = 0   // every attempted row passed or was cleanly skipped
	exitRowsFailed  = 1   // at least one row is fail or blocked: a product signal
	exitUsage       = 2   // bad flags, no fixtures, could not write the result
	exitEnvironment = 3   // no verdict: the environment preflight refused, or rows hit their timeout
	exitInterrupted = 130 // SIGINT/SIGTERM: rows cancelled, containers swept
)

// StatusTimeout, StatusAborted and StatusErrored are hack/matrix's own
// additions to the harness's row statuses, for the three outcomes where
// something other than the product ended the row.
//
//   - StatusTimeout: the row was still running when --row-timeout expired
//     and the harness killed it. Before this existed, such a row was
//     reported with whatever exit status the killed process happened to
//     leave behind — nine rows of the 2026-09-03 run said "exit 1 (usage),
//     want 0 (success)", sending a reader hunting for a command-line bug
//     that did not exist. A timeout is a statement about the host and the
//     budget, never about debark. "Still running" here is the harness's
//     own report (harness.RowResult.TimedOut), not an inference from when
//     the deadline fired: a row that reached a verdict and then spent the
//     rest of the budget removing its containers keeps that verdict.
//
//   - StatusAborted: the run was cancelled (SIGINT/SIGTERM) while the row
//     was in flight, for the same reason: an interrupted row has no verdict
//     and must not be filed as one.
//
//   - StatusErrored: a stage exited with a code debark cannot produce —
//     outside the frozen 0-7 table — so the container runtime or a signal
//     chose it. `docker exec` reports 125/126/127 for its own failures, a
//     killed process 128+signo, and a platform this host cannot execute 255
//     with "exec format error".
//
//     This is the same mistake StatusTimeout fixed, one exit code along.
//     Such a row used to be filed `blocked`, which this package's README
//     defines as "specifically looks like an unimplemented product
//     capability", and which makes the process exit 1, "a product signal".
//     The 2026-09-07 run spent a row and its exit code that way on QEMU
//     binfmt registration evaporating between two consecutive docker execs
//     into the same container, with the daemon up throughout — a fact about
//     the host reported as a fact about debark. The harness already said
//     so in the blocker text; it just had no way to say it in the status.
//     harness.RowResult.RuntimeError is that way.
//
// All three are additive to schema debark.e2e.matrixresult/v1: they are new
// values of an existing string field plus new keys inside "summary", so a
// reader that knows only pass/fail/blocked/skipped still parses the document
// and still sees a total that accounts for every row (see resultDoc).
const (
	StatusTimeout harness.Status = "timeout"
	StatusAborted harness.Status = "aborted"
	StatusErrored harness.Status = "errored"
)

const (
	// sweepBudget bounds the container cleanup sweep, on both the normal and
	// the signal path.
	sweepBudget = 90 * time.Second
	// signalGrace is how long an interrupted run waits for in-flight rows to
	// notice the cancellation and tear their own containers down before the
	// sweep runs anyway and the process exits. Rows abort in seconds; this
	// is the backstop, not the expected path.
	signalGrace = 30 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("matrix", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var fixtureFlag, releaseFlag stringList
	fs.Var(&fixtureFlag, "fixture", "run only fixtures whose name contains this (repeatable, or comma-separated)")
	fs.Var(&releaseFlag, "release", "run only releases matching this, e.g. debian-12 or noble (repeatable, or comma-separated)")
	workers := fs.Int("workers", defaultWorkers(), "bounded parallel worker count (each row is minutes of container time)")
	out := fs.String("out", defaultResultPath(), "path to write the JSON result file")
	rowTimeout := fs.Duration("row-timeout", 10*time.Minute, "per-row timeout")
	imageTimeout := fs.Duration("image-timeout", 5*time.Minute, "timeout for pulling one release image")
	workDir := fs.String("workdir", harness.DefaultWorkRoot(), "host scratch directory (never inside the repo)")
	quiet := fs.Bool("quiet", false, "suppress the human summary table (the JSON result is always written)")
	skipEnvPreflight := fs.Bool("skip-env-preflight", false, "start rows even if a stock container cannot apt-get update on this host")
	envPreflightTimeout := fs.Duration("env-preflight-timeout", 60*time.Second, "budget for the one-container apt-get update environment probe")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	fixtureFilter := splitList(fixtureFlag)
	releaseFilter := splitList(releaseFlag)

	fixturesDir, err := e2e.FixturesDir()
	if err != nil {
		fmt.Fprintf(stderr, "matrix: %v\n", err)
		return exitUsage
	}
	fixtures, err := e2e.LoadAll(fixturesDir)
	if err != nil {
		fmt.Fprintf(stderr, "matrix: %v\n", err)
		return exitUsage
	}
	if len(fixtures) == 0 {
		fmt.Fprintf(stderr, "matrix: no fixtures found in %s\n", fixturesDir)
		return exitUsage
	}

	jobs := planJobs(fixtures, fixtureFilter, releaseFilter)
	if len(jobs) == 0 {
		fmt.Fprintf(stderr, "matrix: no rows matched --fixture/--release\n")
		return exitUsage
	}

	// One cancellable context for the whole run, so a signal reaches every
	// in-flight row's docker calls instead of being noticed only after they
	// finish.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runID := harness.NewRunID()
	fmt.Fprintf(stderr, "matrix: run %s, %d row(s), %d worker(s), workdir=%s\n", runID, len(jobs), *workers, *workDir)

	// Cleanup is registered before anything can start a container, runs at
	// most once however it is reached (normal return, signal, or both), and
	// removes only containers carrying this run's own label — never a bare
	// prune, on a Docker host shared with unrelated long-running projects.
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() { sweepRun(runID, stderr) })
	}
	done := make(chan struct{})
	defer close(done) // LIFO: runs after cleanup, releasing the signal watcher
	defer cleanup()

	sigCh := make(chan os.Signal, 2)
	// SIGTERM is registered on every platform (it is simply never delivered
	// on Windows, where Ctrl+C arrives as os.Interrupt instead).
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	guard := &interruptGuard{
		stderr:  stderr,
		runID:   runID,
		cancel:  cancel,
		cleanup: cleanup,
		grace:   signalGrace,
		exit:    os.Exit,
	}
	go guard.watch(sigCh, done)

	dockerOK, dockerReason := harness.DockerAvailable(ctx)

	doc := resultDoc{
		MatrixResult: harness.MatrixResult{
			SchemaVersion: harness.ResultSchemaVersion,
			GeneratedAt:   time.Now().UTC(),
			Host: harness.HostInfo{
				GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(),
			},
			Config: harness.RunConfig{
				Workers:       *workers,
				FixtureFilter: strings.Join(fixtureFilter, ","),
				ReleaseFilter: strings.Join(releaseFilter, ","),
			},
		},
	}

	if !dockerOK {
		fmt.Fprintf(stderr, "matrix: docker unavailable (%s) — every row skipped\n", dockerReason)
		for _, j := range jobs {
			doc.Rows = append(doc.Rows, skippedRow(j, "docker unavailable: "+dockerReason))
		}
	} else {
		if *skipEnvPreflight {
			fmt.Fprintf(stderr, "matrix: --skip-env-preflight: not checking whether a container can reach its apt archive\n")
		} else if ok, detail := envPreflight(ctx, jobs, runID, *envPreflightTimeout, stderr); !ok {
			if ctx.Err() != nil {
				// The probe did not fail, it was interrupted — do not blame
				// the host for a signal the operator sent.
				fmt.Fprintf(stderr, "matrix: environment preflight interrupted before any row started\n")
				return exitInterrupted
			}
			fmt.Fprint(stderr, envPreflightRefusal(detail, len(jobs)))
			return exitEnvironment
		}
		doc.Rows = runJobs(ctx, jobs, runID, *workDir, *rowTimeout, *imageTimeout, *workers, stderr)
	}

	doc.Summary = summarize(doc.Rows)
	if err := writeResult(doc, *out); err != nil {
		fmt.Fprintf(stderr, "matrix: write result: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(stderr, "matrix: result written to %s\n", *out)

	if !*quiet {
		printSummaryTable(stdout, doc)
	}

	switch {
	case guard.tripped.Load():
		return exitInterrupted
	case doc.Summary.Fail > 0 || doc.Summary.Blocked > 0:
		return exitRowsFailed
	case doc.Summary.Timeout > 0 || doc.Summary.Aborted > 0 || doc.Summary.Errored > 0:
		// No product verdict was reached for at least one row, so this is
		// "check the host", not "the product failed". Ordered after the
		// fail/blocked case on purpose: a run carrying both a real product
		// failure and a host problem is still a product signal, and exit 1
		// is the louder of the two.
		return exitEnvironment
	}
	return exitOK
}

// ---------------------------------------------------------------------------
// Result document
// ---------------------------------------------------------------------------

// summaryCounts is the row-count breakdown written as "summary". It is
// harness.Summary plus the two counters only this runner can produce; the
// invariant a reader relies on is unchanged and still checkable:
// total == pass + fail + blocked + skipped + timeout + aborted.
type summaryCounts struct {
	Total   int `json:"total"`
	Pass    int `json:"pass"`
	Fail    int `json:"fail"`
	Blocked int `json:"blocked"`
	Skipped int `json:"skipped"`
	Timeout int `json:"timeout"`
	Aborted int `json:"aborted"`
	Errored int `json:"errored"`
}

// resultDoc is the JSON document this runner writes: schema
// debark.e2e.matrixresult/v1 exactly as before, with a "summary" object
// that carries two additional counters. The embedded MatrixResult supplies
// every other field verbatim (encoding/json flattens an embedded struct),
// and the outer Summary — being one level shallower — is the one that gets
// marshalled, so the output still has exactly one "summary" key.
type resultDoc struct {
	harness.MatrixResult
	Summary summaryCounts `json:"summary"`
}

func summarize(rows []harness.RowResult) summaryCounts {
	s := summaryCounts{}
	for _, r := range rows {
		s.Total++
		switch r.Status {
		case harness.StatusPass:
			s.Pass++
		case harness.StatusFail:
			s.Fail++
		case harness.StatusBlocked:
			s.Blocked++
		case harness.StatusSkipped:
			s.Skipped++
		case StatusTimeout:
			s.Timeout++
		case StatusAborted:
			s.Aborted++
		case StatusErrored:
			s.Errored++
		}
	}
	return s
}

// writeResult writes doc to path, creating parent directories as needed.
// (harness.MatrixResult.WriteJSON cannot be used: it would marshal the
// embedded value and drop the extended summary.)
func writeResult(doc resultDoc, path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ---------------------------------------------------------------------------
// Row classification
// ---------------------------------------------------------------------------

// classifyRow re-labels a row the harness itself ended, so a row that never
// reached a verdict is never filed as one.
//
// WHETHER the harness ended it is row.TimedOut, and that is the whole fix.
// RunFixture stamps that field the instant execute() returns and BEFORE it
// tears its containers down; teardown deliberately runs on its own fresh 90s
// context so containers are removed even after a deadline, which means the
// per-row context's error — read out here, after RunFixture has returned —
// stopped being evidence about the row the moment cleanup began. See
// harness.RowResult.TimedOut.
//
// Guessing from that error was this function's original design, and it had a
// false positive that is now gone: a row that reached a genuine fail or
// blocked verdict a few seconds before --row-timeout, and whose teardown
// then crossed the deadline, was relabelled timeout here — turning exit 1
// (a product signal, and the most valuable row in the file) into exit 3
// (check the host), and discarding the one row a reader most needed to see.
// The ambiguous window was as wide as teardown takes; it is now the few
// instructions between execute() returning and RunFixture reading the
// context, because the harness reports the fact instead of the runner
// inferring it.
//
// WHICH ending it was is the one thing a bool cannot carry, so rowCtxErr is
// still read for exactly that: Canceled means the whole run was cancelled by
// a signal (aborted), anything else means the deadline fired (timeout). The
// direction of that fallback is deliberate. Before row.TimedOut existed, an
// unrecognised error meant no evidence at all and the row was returned
// untouched; now the evidence is on the row itself and an unrecognised error
// is merely a missing label, so the row is still recorded as stopped rather
// than published as a verdict. Leaving it as fail is precisely the nine rows
// of the 2026-09-03 run that reported "exit 1 (usage), want 0 (success)" for
// a usage error nothing had returned, and sent a maintainer hunting a
// command-line bug that did not exist — four hours and a re-run.
//
// A row already reported as pass is never re-labelled, exactly as before:
// RunFixture reports pass only when every stage completed, so a deadline
// that expires afterwards is not a timeout of the row. That check stays
// first, ahead of row.TimedOut, so pass is exempt however the field is set.
//
// The original blocker text is appended rather than discarded — it is the
// last thing the row saw, still worth reading, just no longer the headline.
func classifyRow(row harness.RowResult, rowCtxErr error, rowTimeout time.Duration) harness.RowResult {
	if row.Status == harness.StatusPass {
		return row
	}
	// A runtime-chosen exit is checked BEFORE the timeout, and only when the
	// row was not killed: a row that was killed at --row-timeout is a
	// timeout whatever exit code the dying process left behind, which is the
	// exact confusion StatusTimeout exists to end. Order the other way and a
	// killed row whose process happened to leave 137 (SIGKILL, 128+9) would
	// be relabelled "errored" and the timeout would disappear from the
	// summary a reader uses to spot a sick host.
	if !row.TimedOut && row.RuntimeError {
		row.Status = StatusErrored
		return row
	}
	if !row.TimedOut {
		return row
	}
	stage := row.Stage
	if stage == "" {
		stage = "unknown"
	}
	ran := fmt.Sprintf("%.1fs", float64(row.TotalMS)/1000)
	prior := strings.TrimSpace(row.Blocker)

	if errors.Is(rowCtxErr, context.Canceled) {
		row.Status = StatusAborted
		row.Blocker = fmt.Sprintf(
			"run aborted: the matrix was cancelled (signal) and stopped this row after %s at stage %q — debark never reached a verdict",
			ran, stage)
	} else {
		row.Status = StatusTimeout
		row.Blocker = fmt.Sprintf(
			"row timeout: the harness killed this row at --row-timeout=%v (ran %s, furthest stage %q) — debark never reached a verdict",
			rowTimeout, ran, stage)
	}
	if prior != "" {
		row.Blocker += ". Last thing the row recorded: " + prior
	}
	return row
}

// ---------------------------------------------------------------------------
// Running rows
// ---------------------------------------------------------------------------

// preflight is what a release/arch pair needs checked once, shared by every
// job that uses it, so ten fixtures on Debian 12 do not each independently
// pull or probe it.
type preflight struct {
	ok     bool
	reason string
}

func runJobs(ctx context.Context, jobs []Job, runID, workDir string, rowTimeout, imageTimeout time.Duration, workers int, stderr io.Writer) []harness.RowResult {
	if workers < 1 {
		workers = 1
	}
	results := make([]harness.RowResult, len(jobs))

	var preflightMu sync.Mutex
	preflights := map[string]preflight{}
	checkPreflight := func(j Job) preflight {
		key := j.Release.DistroID + "/" + j.Release.VersionID + "/" + j.Arch
		preflightMu.Lock()
		if p, ok := preflights[key]; ok {
			preflightMu.Unlock()
			return p
		}
		preflightMu.Unlock()

		var p preflight
		if ok, reason := harness.EnsureImage(ctx, j.Release.Ref(), imageTimeout); !ok {
			p = preflight{ok: false, reason: "release image unavailable: " + reason}
		} else if platform, hasPlatform := distro.Platform(j.Arch); hasPlatform && platform != nativePlatform(runtime.GOARCH) {
			if ok, reason := harness.PlatformSupported(ctx, platform, j.Release.Ref(), 90*time.Second); !ok {
				p = preflight{ok: false, reason: reason}
			} else {
				p = preflight{ok: true}
			}
		} else {
			p = preflight{ok: true}
		}

		preflightMu.Lock()
		preflights[key] = p
		preflightMu.Unlock()
		return p
	}

	type indexedJob struct {
		idx int
		job Job
	}
	work := make(chan indexedJob)
	var wg sync.WaitGroup
	var logMu sync.Mutex
	logLine := func(format string, a ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(stderr, format+"\n", a...)
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ij := range work {
				j := ij.job
				if err := ctx.Err(); err != nil {
					// The run was cancelled before this row ever started a
					// container: never attempted is exactly "skipped".
					results[ij.idx] = skippedRow(j, "run cancelled before this row started: "+err.Error())
					continue
				}
				if j.SkipReason != "" {
					results[ij.idx] = skippedRow(j, j.SkipReason)
					logLine("SKIP  %s: %s", j.rowLabel(), j.SkipReason)
					continue
				}
				if p := checkPreflight(j); !p.ok {
					results[ij.idx] = skippedRow(j, p.reason)
					logLine("SKIP  %s: %s", j.rowLabel(), p.reason)
					continue
				}
				logLine("START %s", j.rowLabel())
				// j.Arch is the expanded architecture for this row (a
				// target.matrix:true fixture is expanded across every
				// matrix_arches entry); it must actually replace the
				// fixture's own default Target.Arch here, on the scenario
				// RunFixture uses to decide --platform, or every expanded
				// row silently runs as the fixture's original architecture
				// regardless of which one it claims to be — exactly the bug
				// found and fixed while building the integration matrix:
				// "arm64" rows were, underneath, running amd64 the
				// whole time. Scenario is a plain value type (TargetSpec is
				// embedded by value, not by pointer), so this copy-then-
				// mutate never touches j.Fixture.Scenario itself.
				scenario := j.Fixture.Scenario
				scenario.Target.Arch = j.Arch
				// The row's deadline is owned here as well as inside
				// RunFixture, so a cancellation reaches this row's docker
				// calls promptly and so the context latches WHICH ending it
				// was — DeadlineExceeded (row timeout) or Canceled (signal)
				// — for classifyRow to name.
				//
				// It is no longer asked WHETHER the row was killed. That
				// answer is row.TimedOut, stamped inside RunFixture before
				// teardown; read out here it would be read after teardown,
				// which is what used to relabel a real fail verdict as a
				// host timeout (see classifyRow).
				rowCtx, rowCancel := context.WithTimeout(ctx, rowTimeout)
				row := harness.RunFixture(rowCtx, scenario, harness.RunOpts{
					RunID:                runID,
					WorkDir:              workDir,
					Release:              j.Release,
					Timeout:              rowTimeout,
					KeepWorkDirOnFailure: true,
				})
				row.Arch = j.Arch
				row = classifyRow(row, rowCtx.Err(), rowTimeout)
				rowCancel()
				results[ij.idx] = row
				logLine("%-8s %s (%s, %.1fs)", strings.ToUpper(string(row.Status)), j.rowLabel(), row.Stage, float64(row.TotalMS)/1000)
			}
		}()
	}

	go func() {
		for i, j := range jobs {
			work <- indexedJob{idx: i, job: j}
		}
		close(work)
	}()
	wg.Wait()

	// results was written by index (ij.idx), so it is already in planJobs'
	// deterministic order — nothing further to sort.
	return results
}

func skippedRow(j Job, reason string) harness.RowResult {
	now := time.Now()
	return harness.RowResult{
		Fixture:    j.Fixture.Name,
		Protects:   j.Fixture.Protects,
		Distro:     j.Release.DistroID,
		Version:    j.Release.VersionID,
		Arch:       j.Arch,
		Status:     harness.StatusSkipped,
		Stage:      "preflight",
		Blocker:    reason,
		StartedAt:  now,
		FinishedAt: now,
	}
}

func defaultWorkers() int {
	// Container-minutes-bound, not CPU-bound work (task: "run rows in
	// parallel with a bounded worker count, since each row is minutes of
	// container time") — a modest fixed default regardless of host core
	// count, overridable with --workers.
	return 4
}

func defaultResultPath() string {
	root, err := harness.RepoRoot()
	if err != nil {
		return "matrix-result.json"
	}
	return root + "/hack/matrix/results/result.json"
}

// ---------------------------------------------------------------------------
// Cleanup and signals
// ---------------------------------------------------------------------------

// runFilter is the docker filter expression that selects this run's own
// containers and nothing else: the label harness.Sweep filters on, and the
// label harness.StartContainer (and aptProbeArgs) stamp on everything they
// create. An empty run id is refused rather than turned into a filter,
// because "label=debark.e2e.run=" is not scoped to anything this run owns.
func runFilter(runID string) (string, error) {
	if strings.TrimSpace(runID) == "" {
		return "", errors.New("empty run id: refusing to build an unscoped container filter")
	}
	return "label=" + harness.LabelRun + "=" + runID, nil
}

// runLabels are the labels this command stamps on the containers it starts
// itself (the environment probe); harness.StartContainer stamps the same
// pair on every row container.
func runLabels(runID string) []string {
	return []string{harness.LabelMarker + "=1", harness.LabelRun + "=" + runID}
}

// sweepRun force-removes every container labelled with this run id. It is
// idempotent (a second call finds nothing and says nothing) and narrow: it
// never prunes, never matches an unlabelled container, and never matches
// another run's — this host routinely carries dozens of containers belonging
// to unrelated projects. A transient failure (a row's own teardown removing
// the same container concurrently) is retried once, then reported with the
// exact filter to inspect by hand.
func sweepRun(runID string, stderr io.Writer) {
	filter, err := runFilter(runID)
	if err != nil {
		fmt.Fprintf(stderr, "matrix: sweep: %v\n", err)
		return
	}
	// A fresh context: the sweep must still run when the run's own context
	// has just been cancelled by a signal, which is precisely when it
	// matters most.
	ctx, cancel := context.WithTimeout(context.Background(), sweepBudget)
	defer cancel()
	for attempt := 1; ; attempt++ {
		n, serr := harness.Sweep(ctx, runID)
		if serr == nil {
			if n > 0 {
				fmt.Fprintf(stderr, "matrix: swept %d container(s) matching %s that a row's own cleanup did not remove\n", n, filter)
			}
			return
		}
		if attempt >= 2 || ctx.Err() != nil {
			fmt.Fprintf(stderr, "matrix: sweep: %v — check `docker ps -a --filter %s`\n", serr, filter)
			return
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
	}
}

// interruptGuard turns a SIGINT/SIGTERM into an orderly shutdown: cancel the
// run so in-flight rows stop and tear down their own containers, wait a
// bounded moment for that, sweep whatever is left under this run's label,
// and only then exit. Without it a signal kills the process outright, every
// deferred cleanup is skipped, and every container the run started is
// orphaned on a shared Docker host.
type interruptGuard struct {
	stderr  io.Writer
	runID   string
	cancel  context.CancelFunc
	cleanup func()
	grace   time.Duration
	exit    func(int)
	tripped atomic.Bool
}

// watch handles the first signal and returns. done is closed by run() once
// the normal path has finished its own cleanup, which is the case where this
// goroutine has nothing left to do.
func (g *interruptGuard) watch(sig <-chan os.Signal, done <-chan struct{}) {
	var s os.Signal
	select {
	case s = <-sig:
	case <-done:
		return
	}
	g.tripped.Store(true)
	fmt.Fprintf(g.stderr, "matrix: %v received — cancelling rows and removing run %s's containers (waiting up to %v for a clean stop; signal again to sweep now)\n", s, g.runID, g.grace)
	g.cancel()

	select {
	case <-done:
		// run() unwound normally: it has already swept and is returning its
		// own exit code.
		return
	case s2 := <-sig:
		fmt.Fprintf(g.stderr, "matrix: %v received again — sweeping now\n", s2)
	case <-time.After(g.grace):
		fmt.Fprintf(g.stderr, "matrix: rows did not stop within %v — sweeping now\n", g.grace)
	}
	g.cleanup()
	g.exit(exitInterrupted)
}

// ---------------------------------------------------------------------------
// Environment preflight
// ---------------------------------------------------------------------------

// envPreflight proves, in one short-lived container, that a stock target
// image can still reach its apt archive on this host, before the matrix
// spends hours discovering that it cannot.
//
// This exists because of a real, expensive run: on 2026-09-03 every one of
// nineteen rows burned its full 12-minute budget and reported a failure,
// because `apt-get update` in a bare Debian container was hanging on the
// host — debark was not involved at all (hack/matrix/results/README.md).
// A matrix that declines to start beats one that reports nineteen false
// failures four hours later.
//
// It probes with a release image the run already has cached, so the check
// costs one container and a few seconds and never turns a slow pull into a
// false refusal. On a cold host, where nothing is cached yet, there is
// nothing cheap to probe with: the per-row image preflight will prove the
// network by pulling, so this says so and lets the run continue.
func envPreflight(ctx context.Context, jobs []Job, runID string, timeout time.Duration, stderr io.Writer) (ok bool, detail string) {
	image := ""
	for _, ref := range plannedImages(jobs) {
		if harness.ImageExistsLocally(ctx, ref) {
			image = ref
			break
		}
	}
	if image == "" {
		fmt.Fprintf(stderr, "matrix: env preflight: no target image cached yet — skipping the apt probe (the first image pull proves the network instead)\n")
		return true, ""
	}

	fmt.Fprintf(stderr, "matrix: env preflight: %s apt-get update -qq (budget %v)\n", shortRef(image), timeout)
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	res, err := harness.Docker(pctx, aptProbeArgs(image, runID)...)
	ok, detail = classifyAptProbe(res, err, errors.Is(pctx.Err(), context.DeadlineExceeded), time.Since(start), timeout)
	if ok {
		fmt.Fprintf(stderr, "matrix: env preflight: ok — %s\n", detail)
	}
	return ok, detail
}

// aptProbeArgs is the probe container's exact command line. It carries this
// run's labels so that if the probe has to be killed at its deadline — the
// very case being tested — any container it leaves behind is swept by
// exactly the same scoped filter as every other container of the run.
func aptProbeArgs(image, runID string) []string {
	args := []string{"run", "--rm", "--name", "debark-e2e-preflight-" + runID}
	for _, l := range runLabels(runID) {
		args = append(args, "--label", l)
	}
	return append(args, image, "apt-get", "update", "-qq")
}

// classifyAptProbe turns the probe's outcome into a verdict plus one line of
// human detail. Split out from envPreflight so every branch is testable
// without a container.
func classifyAptProbe(res harness.CmdResult, err error, timedOut bool, elapsed, timeout time.Duration) (ok bool, detail string) {
	switch {
	case timedOut:
		return false, fmt.Sprintf("apt-get update did not finish within %v (a healthy host takes a few seconds) — the archive is unreachable, or Docker's networking has degraded", timeout)
	case err != nil:
		return false, fmt.Sprintf("could not run the probe container: %v", err)
	case res.ExitCode != 0:
		return false, fmt.Sprintf("apt-get update exited %d after %.1fs: %s", res.ExitCode, elapsed.Seconds(), lastLine(res.Combined()))
	}
	return true, fmt.Sprintf("apt-get update completed in %.1fs", elapsed.Seconds())
}

// envPreflightRefusal is the message printed instead of starting the run.
func envPreflightRefusal(detail string, rows int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nmatrix: ENVIRONMENT PREFLIGHT FAILED — refusing to start %d row(s).\n\n", rows)
	fmt.Fprintf(&b, "  %s\n\n", detail)
	b.WriteString("A stock target container cannot reach its apt archive on this host, so every\n")
	b.WriteString("row would spend its full --row-timeout doing nothing and then report a failure\n")
	b.WriteString("that says nothing about debark. That run has already happened once and cost\n")
	b.WriteString("four hours of container time — see hack/matrix/results/README.md.\n\n")
	b.WriteString("Check the host first:\n")
	b.WriteString("  docker run --rm debian:bookworm-slim apt-get update -qq   # should take a few seconds\n")
	b.WriteString("  docker ps -a                                              # Docker networking degrades under container load\n\n")
	b.WriteString("To run anyway, knowing the results may be meaningless:\n")
	b.WriteString("  go run ./hack/matrix --skip-env-preflight\n\n")
	b.WriteString("No result file was written: no row ran, so any result.json on disk is an older\n")
	b.WriteString("run and must not be read as this one.\n")
	return b.String()
}

// plannedImages is every distinct release image this run would use, in job
// order.
func plannedImages(jobs []Job) []string {
	var out []string
	seen := map[string]bool{}
	for _, j := range jobs {
		ref := j.Release.Ref()
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// shortRef abbreviates a digest-pinned image reference for log lines, where
// the full 71-character sha256 adds nothing but still has to be recognisable
// enough to match against `docker images --digests`.
func shortRef(ref string) string {
	i := strings.IndexByte(ref, '@')
	if i <= 0 {
		return ref
	}
	digest := ref[i+1:]
	if len(digest) > 19 {
		digest = digest[:19]
	}
	return ref[:i] + "@" + digest
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}
