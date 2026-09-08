// Frozen public API of the install package.

package install

import (
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/verify"
)

// Deps are the install runner's collaborators, injected so the target-side
// code is testable without apt and without a bundle.
type Deps struct {
	// Verifier runs before anything touches the system. Required.
	Verifier verify.Verifier
	// Events receives install.plan and install.result. Optional.
	Events evidence.Sink
	// AptPath overrides the apt-get binary, for tests.
	AptPath string
	// DpkgPath overrides the dpkg binary, for tests.
	DpkgPath string
	// DpkgQueryPath overrides the dpkg-query binary, for tests.
	//
	// Separate from DpkgPath, and not derived from it, because dpkg does not
	// forward the query basedivergence.go needs. Measured on Ubuntu 24.04
	// (dpkg 1.22.6), 2026-09-06: `dpkg -W --showformat=...` answers "dpkg:
	// error: unknown option -W" — dpkg forwards only -l/-s/-S/-L/-p to
	// dpkg-query. Deriving the path from DpkgPath's directory would be a
	// guess about a layout debark does not control, so it is configured
	// like every other binary this package runs.
	//
	// It is invoked only for a bundle built from a synthesized snapshot, and
	// a failure to run it is a warning rather than a refusal, so unlike
	// AptPath and DpkgPath it is never checked by a precondition.
	DpkgQueryPath string
	// Root overrides the filesystem root the preconditions read, for tests:
	// where /etc/os-release is read from and where --keep-source writes
	// /etc/apt/sources.list.d/. Empty means the real filesystem root.
	Root string

	// OnAptOutput, when set, is called once per apt-get/dpkg invocation this
	// run makes, with the full argv and its captured combined output.
	//
	// This is the seam contract-brief rule 7 asks every core/ package to
	// design: core never writes to stdout/stderr or reads a terminal itself
	// (Progress and human output are the CLI's job), so apt's own output is
	// always captured rather than inherited. Report.AptOutputDigest carries
	// only a digest of everything captured this run, because the report is a
	// stable, compact, --json-able object and raw apt chatter does not belong
	// in it. A caller that wants the real bytes — to show --verbose output on
	// a terminal, or to keep its own copy for debugging a failure — sets this
	// callback and receives them directly, without core/install ever having
	// printed anything itself. It is optional and may be called from the
	// goroutine running Plan/Apply only (never concurrently).
	OnAptOutput func(argv []string, output []byte)
}

// New returns the standard target-side install runner.
func New(deps Deps) Runner {
	if deps.AptPath == "" {
		deps.AptPath = "apt-get"
	}
	if deps.DpkgPath == "" {
		deps.DpkgPath = "dpkg"
	}
	if deps.DpkgQueryPath == "" {
		deps.DpkgQueryPath = "dpkg-query"
	}
	return &runner{deps: deps}
}
