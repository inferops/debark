package harness

import "time"

// ResultSchemaVersion is the schema of the JSON file hack/matrix writes —
// "a machine-readable result file ... this is what the nightly CI job
// publishes". It follows the same
// "debark.<thing>/v1" convention as the product's own formats, even though
// it lives outside api/schema/ (this is the harness's own format, not a
// product artefact).
const ResultSchemaVersion = "debark.e2e.matrixresult/v1"

// Status values for one row. Three buckets, deliberately distinct (task:
// "the real current pass/fail/blocked count"):
//
//   - StatusPass: every stage reached its expected exit class and every
//     post-hoc assertion (packages present, binaries run, doctor findings,
//     determinism) matched.
//   - StatusFail: every stage ran — nothing errored unexpectedly — but an
//     expectation did not hold (wrong exit class, a binary exited non-zero,
//     a package missing). This is a real behavioural bug once the product is
//     far enough along to reach it.
//   - StatusBlocked: a stage could not even be attempted or errored in a way
//     that is recognisably "this part of the product does not exist yet"
//     (the debark binary would not build at all, or a stub explicitly
//     said so — stub packages consistently return errors containing the
//     literal string "not implemented", e.g. core/engine/iface.go's
//     `New` stub). This is the expected status for nearly every row today.
//   - StatusSkipped: the row was never attempted — a release image or an
//     emulator was unavailable, or `--fixture`/`--release` filtered it out.
type Status string

const (
	StatusPass    Status = "pass"
	StatusFail    Status = "fail"
	StatusBlocked Status = "blocked"
	StatusSkipped Status = "skipped"
)

// notImplementedMarker is the literal substring stub bodies use
// throughout this tree (see core/engine/iface.go's New, and the identical
// pattern in other core/* packages). Finding
// it in a stage's combined output is how RunFixture tells "blocked" (the
// product does not do this yet) from "fail" (the product did it, wrongly).
const notImplementedMarker = "not implemented"

// RowResult is one fixture x release x arch outcome.
type RowResult struct {
	Fixture  string `json:"fixture"`
	Protects string `json:"protects"`
	Distro   string `json:"distro"`
	Version  string `json:"version"`
	Arch     string `json:"arch"`

	Status Status `json:"status"`
	// Stage is the furthest pipeline stage reached, e.g. "build-binary",
	// "target-setup", "snapshot-create", "target-commit", "build", "verify",
	// "install", "assert-binaries", "determinism", "done".
	//
	// "target-commit" is where the state container is frozen into the image
	// the fresh target is started from (scenario.go's commitTargetImage). A
	// row blocked there produced no evidence about debark at all: the
	// harness could not even construct the machine the bundle was built for.
	Stage string `json:"stage"`
	// Blocker is the exact error/mismatch text, when Status is not pass.
	Blocker string `json:"blocker,omitempty"`

	// ExitClasses records the observed dferr exit class (by name) at each
	// CLI stage that ran, e.g. {"build":"success","verify":"verification"}.
	ExitClasses map[string]string `json:"exit_classes,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	TotalMS    int64     `json:"total_ms"`
	// StageMS is wall-clock time per stage in milliseconds — the
	// performance baseline.
	StageMS map[string]int64 `json:"stage_ms,omitempty"`

	// BundleSizeBytes and PackageCount are the other half of the
	// performance baseline: "record wall-clock time and bundle size per
	// fixture" — recorded whenever a bundle was produced, independent of
	// whether the row ultimately passed.
	BundleSizeBytes int64 `json:"bundle_size_bytes,omitempty"`
	PackageCount    int   `json:"package_count,omitempty"`

	// DeterminismOK is set only for fixtures with determinism:true.
	DeterminismOK *bool `json:"determinism_ok,omitempty"`

	// LogPath is a host path to the full stdout/stderr transcript of every
	// stage attempted, for "how to debug a failing one".
	LogPath string `json:"log_path,omitempty"`

	// TimedOut records that the row's deadline — or a cancellation of the
	// whole run — arrived while the row was still EXECUTING, rather than
	// after it had already reached a verdict.
	//
	// It exists because a caller cannot work this out for itself. RunFixture
	// tears its containers down INSIDE the call, on a deliberately fresh
	// context, so that cleanup still happens after a timeout; only then does
	// it return. A caller that reads the row context's error afterwards
	// therefore cannot tell "killed mid-stage" from "reached a real verdict
	// and then spent longer than the remaining budget removing containers",
	// and the second one is a genuine product signal. Relabelling it as a
	// host problem is the same class of mistake as the reverse — a result
	// file that says something nothing produced — just pointed the other
	// way, and it discards the one row a reader most needs to see.
	//
	// Nothing else in RowResult separates the two: a killed row lands as
	// fail or blocked like any other, and TotalMS/StageMS both include
	// teardown, so any threshold on them starts relabelling genuinely killed
	// rows as product failures instead.
	//
	// It is read the instant execute() returns, which narrows the ambiguous
	// window from "as long as teardown takes" to the few instructions
	// between those two statements. That is a narrowing, not an elimination:
	// a deadline that fires in exactly that gap still sets this on a row
	// that had just concluded. omitempty keeps the field additive, so a
	// schema-v1 reader sees no new key on any row that finished normally.
	TimedOut bool `json:"timed_out,omitempty"`

	// RuntimeError records that a stage exited with a code debark cannot
	// produce — anything outside the frozen 0-7 exit table — so the value
	// was chosen by the container runtime or by a signal, not by the
	// product. `docker exec` reports 125/126/127 for its own failures, a
	// killed process reports 128+signo, and an image whose platform this
	// host cannot execute reports 255 with "exec format error".
	//
	// It is the same separation TimedOut makes, for the same reason. A row
	// that never got a verdict must not be filed as one, and until this
	// existed such a row landed as `blocked` — which hack/matrix/README.md
	// defines as "a stage errored in a way that specifically looks like an
	// unimplemented product capability", and which drives process exit 1,
	// "at least one row is fail or blocked: a product signal". The
	// 2026-09-07 run spent a row and an exit code that way on QEMU binfmt
	// registration evaporating mid-run, with the daemon up throughout.
	//
	// reportStageFailure already knew: it writes "exit %d is not one of
	// debark's own exit classes (0-%d), so the product did not choose it
	// — the container runtime or a signal did" into the blocker. The
	// knowledge simply never reached the status. This carries it there.
	//
	// omitempty keeps it additive, exactly as TimedOut is: no row that
	// finished normally gains a key.
	RuntimeError bool `json:"runtime_error,omitempty"`
}

// Blocked reports whether text (a stage's combined stdout+stderr, or a Go
// error's message) looks like an unimplemented stub rather than a real
// failure.
func Blocked(text string) bool {
	return containsFold(text, notImplementedMarker)
}

func containsFold(s, substr string) bool {
	// Small local case-fold contains, so this file has no extra import for
	// one call site; stub messages are consistently lowercase in this tree
	// today, but folding costs nothing.
	sl, sub := []rune(s), []rune(substr)
	if len(sub) == 0 {
		return true
	}
	toLower := func(rs []rune) []rune {
		out := make([]rune, len(rs))
		for i, r := range rs {
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			out[i] = r
		}
		return out
	}
	sl, sub = toLower(sl), toLower(sub)
	for i := 0; i+len(sub) <= len(sl); i++ {
		match := true
		for j := range sub {
			if sl[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// HostInfo records the environment the matrix ran on.
type HostInfo struct {
	GOOS                string `json:"goos"`
	GOARCH              string `json:"goarch"`
	GoVersion           string `json:"go_version"`
	DockerServerVersion string `json:"docker_server_version,omitempty"`
}

// RunConfig records how the matrix was invoked.
type RunConfig struct {
	Workers       int    `json:"workers"`
	FixtureFilter string `json:"fixture_filter,omitempty"`
	ReleaseFilter string `json:"release_filter,omitempty"`
}

// Summary is the row-count breakdown.
type Summary struct {
	Total   int `json:"total"`
	Pass    int `json:"pass"`
	Fail    int `json:"fail"`
	Blocked int `json:"blocked"`
	Skipped int `json:"skipped"`
}

// MatrixResult is the top-level JSON document hack/matrix writes.
//
// It is data only, deliberately: hack/matrix computes the summary
// (summarize) and writes the file (writeResult) itself, because its own
// document embeds this struct and adds two status buckets — timeout and
// aborted — that this package has no notion of.
//
// This type therefore carries no Summarize/WriteJSON helpers, and must not
// grow them back. Both existed here, both lost their last caller when
// hack/matrix took the job over, and neither could ever be safely used
// again: resultDoc embeds MatrixResult and shadows Summary with its own
// wider counters, so a promoted Summarize() would fill the INNER, invisible
// field and leave the one that gets marshalled stale, while a promoted
// WriteJSON would marshal the embedded value and silently drop the outer
// summary entirely. Being exported, neither would ever be reported by the
// unused linter. A stale or truncated summary in a result file is the exact
// failure hack/matrix/results/README.md is about — a run whose table says
// something nothing produced.
type MatrixResult struct {
	SchemaVersion string      `json:"schema_version"`
	GeneratedAt   time.Time   `json:"generated_at"`
	Host          HostInfo    `json:"host"`
	Config        RunConfig   `json:"config"`
	Rows          []RowResult `json:"rows"`
	// Summary is part of the v1 document shape and is kept so this type
	// remains a complete Go form of it — a reader unmarshalling a result
	// file needs somewhere for the summary to land. Nothing in this tree
	// writes it: hack/matrix's own document embeds MatrixResult and shadows
	// this field with wider counters (timeout, aborted), and the shallower
	// field is the one encoding/json marshals.
	//
	// The consequence is worth stating, because it is not obvious: marshal a
	// bare MatrixResult without setting this and the document carries a
	// summary of all zeros, which reads exactly like a run in which nothing
	// happened. Anything that writes a result file must compute it, as
	// hack/matrix does.
	Summary Summary `json:"summary"`
}
