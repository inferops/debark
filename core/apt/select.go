package apt

import (
	"context"
	"fmt"
	"strings"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
)

// selectBackend is api.go's SelectBackend body.
func selectBackend(ctx context.Context, sel Selection) (Backend, Capabilities, error) {
	return selectBackendWithRunner(ctx, sel, nil)
}

// selectBackendWithRunner is selectBackend with the local backend's Runner
// overridable, which is what lets this package's own tests exercise every
// selection outcome (host has no apt, distro mismatch, apt version
// mismatch, ...) without a real apt-get on the test machine. runner == nil
// means "use the real exec-based one", exactly what selectBackend gets.
func selectBackendWithRunner(ctx context.Context, sel Selection, runner Runner) (Backend, Capabilities, error) {
	requested := sel.Requested
	if requested == "" {
		requested = lock.BackendAuto
	}

	local := newLocalBackend(LocalOptions{Events: sel.Events, Runner: runner})
	localCap, err := local.Probe(ctx, sel.Target)
	if err != nil {
		return nil, Capabilities{}, err
	}
	localReason, localMismatched := localMismatchReason(ctx, localCap, sel.Target)

	switch requested {
	case lock.BackendLocal:
		if localMismatched {
			return nil, localCap, environmentUnavailable("local", localReason)
		}
		emitSelected(sel.Events, lock.BackendLocal, localSelectedReason("explicitly requested", localCap, sel.Target))
		return local, localCap, nil

	case lock.BackendContainer:
		cb, ccap := probeContainerFunc(ctx, sel)
		if !ccap.Available {
			return nil, ccap, environmentUnavailableHint("container", ccap.Reason, ccap.Hint)
		}
		emitSelected(sel.Events, lock.BackendContainer, "explicitly requested")
		return cb, ccap, nil

	case lock.BackendAuto:
		if !localMismatched {
			emitSelected(sel.Events, lock.BackendLocal, localSelectedReason("host apt matches the target: "+localSummary(localCap), localCap, sel.Target))
			return local, localCap, nil
		}
		evidence.Emit(sel.Events, evidence.TypeBackendSelected, "local backend not used", map[string]any{
			"backend": string(lock.BackendLocal), "reason": localReason,
		})
		cb, ccap := probeContainerFunc(ctx, sel)
		if !ccap.Available {
			return nil, ccap, environmentUnavailableHint("auto",
				fmt.Sprintf("local: %s; container: %s", localReason, ccap.Reason), ccap.Hint)
		}
		emitSelected(sel.Events, lock.BackendContainer, "auto-selected because "+localReason)
		return cb, ccap, nil

	default:
		return nil, Capabilities{}, dferr.New(dferr.Usage, "apt: unknown backend %q (want auto, local or container)", requested)
	}
}

// localMismatchReason reports why the local backend cannot be used for
// target, or ("", false) when it can.
//
// This gate originally keyed purely on "host apt major.minor == target apt
// major.minor". Experiment E2 (docs/experiments/E2-solver-divergence.md)
// measured that this is the wrong granularity: across seven fixtures and
// three real apt releases, every genuine selection divergence tracked the
// *effective solver* (APT::Solver) specifically -- two apt versions running
// the same solver never diverged, and the same apt binary diverges from
// itself under nothing more than -o APT::Solver=internal. So the primary
// gate here is distro id AND effective solver (solver.go); apt major.minor
// is now only a secondary, non-blocking signal recorded for the audit trail
// (localSelectedReason below, and -- with the real captured value, not just
// the release-default inference this function is limited to -- Resolve time,
// via solverDivergenceForLock/aptVersionDivergenceNote in local.go, which is
// also where a mismatch becomes a lock.Warning: select.go has no lock to
// write one into yet).
//
// The target's effective solver is inferred from target.APTVersion's release
// default (targetGateSolver, solver.go) rather than read from the target's
// actual captured apt.conf.d: Selection (api.go, frozen) carries only
// snapshot.Target, never the full snapshot or its APT.Conf files, so an
// explicit target-side override cannot be seen this early -- only Resolve,
// which does receive the full snapshot in ResolveInput, can. A target that
// hand-edited apt.conf.d away from its release's default therefore is not
// caught here, only by the Resolve-time recheck that populates
// lock.Resolver.SolverDivergence. This is a known asymmetry the frozen
// Selection contract forces, not an oversight -- see this package's final
// report.
//
// The host's effective solver is probed live (hostSolverProbeFunc, backed by
// `apt-config dump`, cached for the process lifetime): unlike the target
// side, nothing here prevents asking the actual resolving apt what it would
// do, so this side is not an inference.
//
// That asymmetry is exactly why the two sides are not simply compared. An
// inference and a measurement of the SAME apt are not two opinions to
// reconcile -- when they disagree, the inference is wrong. Refusing the local
// backend on that disagreement is what made every Debian 13 target
// unresolvable even on a Debian 13 host (apt 3.0.3: table says "3.0", apt
// itself reports no APT::Solver at all and so resolves with "internal";
// hack/matrix/results/run-2026-09-05.json). solverGateReason (solver.go)
// holds that decision, with the measurement given precedence over the table
// only where it is genuinely a measurement of the same thing.
func localMismatchReason(ctx context.Context, cap Capabilities, target snapshot.Target) (string, bool) {
	if !cap.Available {
		reason := cap.Reason
		if reason == "" {
			reason = "apt-get is not available on this host"
		}
		return reason, true
	}
	if target.DistroID != "" && cap.DistroID != "" && !strings.EqualFold(cap.DistroID, target.DistroID) {
		return fmt.Sprintf("host distro %q does not match target distro %q", cap.DistroID, target.DistroID), true
	}

	return solverGateReason(hostSolverProbeFunc(ctx), cap.APTVersion, target)
}

func localSummary(cap Capabilities) string {
	return fmt.Sprintf("%s %s, apt %s", cap.DistroID, cap.VersionID, cap.APTVersion)
}

// localSelectedReason appends the secondary, non-blocking apt-version note
// (aptVersionDivergenceNote, solver.go) to base when the host and target apt
// major.minor differ, so it is visible in the backend.selected evidence
// event even on a build where local was, correctly, still selected (the
// solver matched). This is an observability nicety on top of the
// authoritative record: the real, always-recorded copy of this same note
// lives in lock.Warnings, written at Resolve time (local.go), because
// select.go itself has no lock to write into yet.
func localSelectedReason(base string, cap Capabilities, target snapshot.Target) string {
	if note := aptVersionDivergenceNote(cap.APTVersion, target.APTVersion); note != "" {
		return base + "; " + note
	}
	return base
}

// probeContainerFunc is a package-level indirection purely so this package's
// own tests can substitute a fake container-availability outcome. Selection
// logic must be verifiable without depending on whether a real docker/podman
// happens to be installed and reachable on the machine running the tests —
// that is the own container backend's concern to test, not
// selectBackend's. Production code always uses the default, probeContainer.
var probeContainerFunc = probeContainer

// probeContainer constructs the container backend and probes it, treating a
// nil Backend (possible if a future build omits container support) the same
// as any other "not available" outcome rather than a Go error, so callers
// handle every unavailability the same way.
func probeContainer(ctx context.Context, sel Selection) (Backend, Capabilities) {
	cb := NewContainerBackend(ContainerOptions{Image: sel.Image, SelfPath: sel.SelfPath, Events: sel.Events})
	if cb == nil {
		return nil, Capabilities{
			Available: false,
			Reason:    "container backend unavailable in this build",
			Hint:      "install docker or podman and use a debark build with container support, or run on a host matching the target so --backend=local applies",
		}
	}
	ccap, err := cb.Probe(ctx, sel.Target)
	if err != nil {
		return nil, Capabilities{Available: false, Reason: err.Error()}
	}
	return cb, ccap
}

func environmentUnavailable(backend, reason string) error {
	return environmentUnavailableHint(backend, reason, "")
}

func environmentUnavailableHint(backend, reason, hint string) error {
	e := &dferr.Error{Class: dferr.Environment, Msg: fmt.Sprintf("apt: %s backend unavailable: %s", backend, reason)}
	if hint == "" {
		hint = "check `debark doctor` for the exact fix"
	}
	return e.WithHint("%s", hint)
}

func emitSelected(sink evidence.Sink, backend lock.Backend, reason string) {
	evidence.Emit(sink, evidence.TypeBackendSelected, "backend selected", map[string]any{
		"backend": string(backend),
		"reason":  reason,
	})
}
