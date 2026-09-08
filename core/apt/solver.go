package apt

// This file is the fix experiment E2 (docs/experiments/E2-solver-divergence.md)
// motivated: backend auto-selection and the solver-divergence record
// must key on the *effective APT::Solver*, not on apt major.minor.
// E2 measured, directly and repeatedly, that every genuine selection
// divergence tracked the solver algorithm specifically -- two different apt
// versions running the same solver never diverged, and the same apt binary
// diverges from itself under nothing more than -o APT::Solver=internal.
//
// Two independent ways of learning "the effective solver" live here:
//   - the TARGET's, from its captured apt.conf.d (an explicit override) with
//     a per-apt-version default table as the fallback (defaultSolverFor
//     APTVersion) -- used both by select.go's selection-time gate (which,
//     per Selection's frozen shape, only ever has the fallback available --
//     see targetGateSolver) and by local.go's Resolve, which does receive
//     the full snapshot and so can read the real captured value
//     (solverDivergenceForLock).
//   - the RESOLVING apt's, by asking apt itself: `apt-config dump` exposes
//     the effective configuration (E2's own recommendation), parsed by
//     solverFromDump. probeHostSolverCached asks the bare host
//     (select.go's gate, cached for the process lifetime like the existing
//     apt/dpkg version probe); probeSolverForRoot asks through a specific
//     private root's APT_CONFIG (local.go's Resolve, so a target-side
//     override that got copied into the private root -- filterAptConf keeps
//     APT::Solver, it only drops Dir::* and proxies -- is correctly
//     reflected, not just the bare host default).
//
// Those two are NOT the same kind of measurement, and treating them as
// interchangeable is what broke every Debian 13 build. Measured 2026-09-05 on
// a stock debian:trixie-slim container -- both sides of the failing matrix
// row were that same image, i.e. the builder WAS the target:
//
//	$ apt-config dump | grep -i '^APT::Solver'   -> no output: no key at all
//	$ apt-get --version | head -1                -> apt 3.0.3 (amd64)
//
// so trixie ships an apt 3.x whose effective solver is "internal", while
// defaultSolverForAPTVersion's "major >= 3 -> 3.0" rule -- an explicitly
// INFERRED generalisation of E2's three measured Ubuntu points, which E2
// itself flags as unverified for exactly "a Debian build whose compile-time
// default differs from Ubuntu's" -- says "3.0" for that same apt. select.go's
// gate then compared the inference (target side) against the probe (host
// side), read the disagreement as solver divergence, and refused the local
// backend for every Debian 13 target on a Debian 13 host
// (hack/matrix/results/run-2026-09-05.json: basic-install-matrix,
// debian-13/amd64 and debian-13/arm64, exit 2 environment).
//
// A guess disagreeing with a measurement OF THE SAME APT is a defect in the
// guess, not evidence of divergence. What makes that distinction decidable at
// runtime is the provenance the dump carries alongside the value -- see
// solverProbe and probeMeasuresTargetDefault below, which is the rule both
// the selection-time gate (solverGateReason) and the Resolve-time record
// (solverDivergenceForLock) now apply.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/inferops/debark/core/snapshot"
)

// solverInternal and solver3 are apt's own effective-solver values -- see
// core/snapshot/aptconf.go's identical constants for the full citation of
// E2's evidence. Duplicated here (rather than exported from core/snapshot)
// because core/apt already imports core/snapshot (iface.go), so the reverse
// import needed to share one definition is not available, and because this
// package's own constants are already unexported package-local vocabulary
// (aptMajorMinor et al. follow the same pattern).
const (
	solverInternal = "internal"
	solver3        = "3.0"
)

// defaultSolverForAPTVersion is core/apt's copy of core/snapshot's
// identically-named, identically-behaved function; see that copy's doc
// comment (core/snapshot/aptconf.go) for the full measured-vs-inferred
// evidence citation. Both copies exist for the same layering reason as
// solverInternal/solver3 above.
//
// It is known to be WRONG for at least one shipped release, and is
// deliberately left that way: apt 3.0.3 on Debian 13/trixie has no
// APT::Solver key in `apt-config dump` at all and therefore resolves with
// "internal", while the "major >= 3" rule below answers "3.0" (measured
// 2026-09-05; see this file's header). The rule is not narrowed to
// "major.minor >= 3.1 -> 3.0" because that would swap one unmeasured
// generalisation for another -- nobody here has measured Ubuntu 25.04's apt
// 3.0.x, or any Debian apt 3.1+, and E2's own verdict is that a version
// proxy which happens to be right is not the same thing as a rule that is
// right for the right reason. Two callers already have something strictly
// better available and must prefer it:
//
//   - an explicit APT::Solver captured from the target's apt.conf.d
//     (snapshot.EffectiveSolver) -- a recorded fact about the target;
//   - a live `apt-config dump` of an apt of the same major.minor that shows
//     no APT::Solver configured at all (probeMeasuresTargetDefault) -- a
//     direct measurement of the very compiled-in default this table guesses.
//
// This function is the last resort behind both, never a value to compare a
// measurement against.
func defaultSolverForAPTVersion(aptVersion string) (solver string, ok bool) {
	major, _, mok := aptMajorMinor(aptVersion)
	if !mok {
		return "", false
	}
	n, err := strconv.Atoi(major)
	if err != nil {
		return "", false
	}
	if n >= 3 {
		return solver3, true
	}
	return solverInternal, true
}

// targetGateSolver returns the solver target.APTVersion's apt would use if
// its apt.conf.d sets no override. It is an inference from a hand-maintained
// version table, and solverGateReason treats it as one: it is consulted only
// when nothing better is available, never compared against a live probe of
// the very same apt (see probeMeasuresTargetDefault).
//
// This is necessarily an inference, not a read of the target's actual
// captured apt.conf.d: Selection (api.go, frozen per the contract brief) carries
// only snapshot.Target, never the snapshot's APT.Conf files, so an explicit
// target-side APT::Solver override cannot be seen this early -- only
// core/snapshot.EffectiveSolver, called from local.go's Resolve once the
// full snapshot is available (ResolveInput.Snapshot), sees the real value.
// A target that hand-edits apt.conf.d away from its release's default will
// therefore not be caught by select.go's gate, only by the Resolve-time
// recheck that populates lock.Resolver.SolverDivergence -- a known asymmetry
// the frozen Selection contract forces.
func targetGateSolver(target snapshot.Target) (solver string, known bool) {
	return defaultSolverForAPTVersion(target.APTVersion)
}

// --- apt-config dump: execution ---------------------------------------

// runAptConfigDump runs `apt-config dump` (optionally with -o overrides,
// exactly the lever E2 used to force a solver) and returns its combined
// output. It execs "apt-config" directly via os/exec rather than through the
// Runner interface (which is scoped to running "apt-get") -- the same
// pattern local.go's runDpkgVersion already uses for "dpkg --version", a
// different sibling apt-package binary.
//
// aptConfigPath, when non-empty, is exported as APT_CONFIG so the dump
// reflects a specific private root's merged configuration instead of the
// bare host's (see PrivateRoot.AptConfigPath and
// docs/experiments/E4-aptconfd-leakage.md: APT_CONFIG, not "-c", is the
// mechanism that actually redirects apt's apt.conf/apt.conf.d scan, and it
// is honoured by every libapt-pkg-based frontend, apt-config included, not
// just apt-get).
func runAptConfigDump(ctx context.Context, aptConfigPath string, extraOpts ...string) ([]byte, error) {
	args := make([]string, 0, len(extraOpts)*2+1)
	for _, o := range extraOpts {
		args = append(args, "-o", o)
	}
	args = append(args, "dump")
	cmd := exec.CommandContext(ctx, "apt-config", args...)
	cmd.Env = cleanEnv(os.Environ(), aptConfigPath)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --- apt-config dump: parsing -------------------------------------------

// aptConfigDumpLineRE matches one `apt-config dump` assignment line:
// dump's own output is already flat, fully-qualified `Dotted::Key "value";`
// statements, one per line, with no nested { } blocks and no comments -- it
// exists specifically to be script-parseable -- so a line scan is
// sufficient. This deliberately does not reuse core/snapshot's fuller
// apt.conf.d parser (a different package, built for the richer syntax real
// apt.conf.d fragments use, which dump's own output never contains).
var aptConfigDumpLineRE = regexp.MustCompile(`^([A-Za-z0-9_.:-]+)\s+"((?:[^"\\]|\\.)*)"\s*;\s*$`)

func unescapeAptConfigValue(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseAptConfigDumpMap parses every assignment out of an `apt-config dump`
// (or `apt-config dump -o ...`) output into a lower-cased-key -> value map.
// A line that does not match the expected shape is silently skipped rather
// than treated as an error: dump's output is apt's own, not third-party
// input that needs strict validation, and a caller only ever looks up the
// handful of keys it actually cares about.
func parseAptConfigDumpMap(dump []byte) map[string]string {
	kv := make(map[string]string)
	for _, line := range strings.Split(string(dump), "\n") {
		m := aptConfigDumpLineRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		kv[strings.ToLower(m[1])] = unescapeAptConfigValue(m[2])
	}
	return kv
}

// solverFromDump extracts the effective solver from a successful
// `apt-config dump`'s output, preferring the apt-get-scoped key over the
// bare one -- E2's own recommendation ("specifically the
// binary::apt-get::APT::Solver scoped key, since that's the front-end
// debark actually shells out to"), and confirmed necessary by the raw
// capture itself: on 25.10 the scoped key carries the real "3.0" value while
// the bare one is blank (`APT::Solver ""`); on 26.04 there is no scoped key
// at all and only the bare one carries "3.0" -- so a caller that checked
// only one of the two would misread one of the two solver3 releases E2
// tested.
//
// Absence of BOTH keys is itself a confident answer, not an unknown:
// hack/experiments/out/e2/e2-run.log shows apt 2.8.3 (the one pre-solver3
// release tested) with neither key present at all, while both solver3
// releases always showed one of them holding "3.0" -- so "dump ran, found
// neither key" means "nothing in this apt's merged configuration selects a
// solver", and apt's own fallback for an unset APT::Solver is the classic
// resolver, i.e. internal (E2 §2: `-o APT::Solver=internal` "works
// identically to no override at all, on all three releases"). That is a
// different case from the probe itself failing to run at all (apt-config
// missing, non-zero exit, ...), which the caller (probeHostSolverUncached /
// probeSolverForRoot) reports as ok=false before this function is reached.
//
// configured reports which of those two shapes produced the value, and is
// the whole reason this function returns two things rather than one. See
// solverProbe for why the difference is load-bearing rather than cosmetic.
// A key that is present but EMPTY counts as unconfigured, deliberately:
// apt treats an empty APT::Solver as unset (E2 recorded questing carrying
// `APT::Solver ""` while the value that actually applied lived in the scoped
// key), so an empty assignment is not this machine choosing a solver.
func solverFromDump(dump []byte) (solver string, configured bool) {
	kv := parseAptConfigDumpMap(dump)
	if v := kv["binary::apt-get::apt::solver"]; v != "" {
		return v, true
	}
	if v := kv["apt::solver"]; v != "" {
		return v, true
	}
	return solverInternal, false
}

// effectiveSolverFromDump is solverFromDump for the callers that only need
// the value (aptconf.go's private-root round-trip check, solver_e2e_test.go's
// -o APT::Solver=internal lever). Kept as a separate one-result function so
// those call sites are not forced to name and discard a provenance flag they
// have no use for.
func effectiveSolverFromDump(dump []byte) string {
	solver, _ := solverFromDump(dump)
	return solver
}

// solverProbe is one live `apt-config dump` observation of an apt's effective
// solver. It carries the value's provenance as well as the value because
// provenance is what separates a measurement from a configuration choice:
//
//   - Configured == true: a non-empty APT::Solver assignment (bare or
//     apt-get-scoped) was present in the merged configuration. That could be apt's
//     own shipped default -- E2 recorded stock Ubuntu 26.04 carrying a
//     blanket `APT::Solver "3.0"` and stock 25.10 carrying
//     `binary::apt-get::APT::Solver "3.0"` -- or an admin's apt.conf.d edit,
//     which is E2's own `-o APT::Solver=internal` lever and shows up in the
//     dump as exactly the same shape. The dump cannot tell those two apart,
//     so a configured value states only "this is what THIS machine resolves
//     with" and says nothing about any other machine's apt.
//
//   - Configured == false: nothing anywhere in the merged configuration
//     mentions a solver, so the value is apt's own fallback for an unset
//     APT::Solver -- a property of the apt BINARY, not of this machine.
//     E2 recorded exactly this shape on Ubuntu 24.04 / apt 2.8.3, and the
//     2026-09-05 Debian 13 / apt 3.0.3 measurement in this file's header
//     shows it again. This is the case in which the probe is measuring the
//     same quantity defaultSolverForAPTVersion is guessing at.
type solverProbe struct {
	// Solver is the effective solver value, always one of apt's own
	// spellings; never empty when OK is true.
	Solver string
	// Configured is true when a non-empty APT::Solver key was present.
	Configured bool
	// OK is false when the probe could not be run at all (no apt-config,
	// non-zero exit, cancelled context) -- distinct from a probe that ran
	// and found no key, which is a confident answer.
	OK bool
}

// sameAPTMajorMinor reports whether two apt version strings agree on
// major.minor, both parsing. Used only to decide whether a compiled-in
// default measured on one apt describes the other (probeMeasuresTargetDefault
// below); never as a divergence signal in its own right, which is precisely
// the use E2 demoted it from.
func sameAPTMajorMinor(a, b string) bool {
	aMaj, aMin, aOK := aptMajorMinor(a)
	bMaj, bMin, bOK := aptMajorMinor(b)
	return aOK && bOK && aMaj == bMaj && aMin == bMin
}

// probeMeasuresTargetDefault reports whether a live probe of the resolving
// apt is, in fact, a MEASUREMENT of the very quantity
// defaultSolverForAPTVersion was guessing at for the target. When it is, the
// two values are not two independent signals whose disagreement means
// divergence -- they are one guess and one measurement of the same number,
// and the measurement is simply the better value. Comparing them and
// reporting "the solvers differ" is how a Debian 13 host came to refuse
// resolving for a Debian 13 target (this file's header).
//
// All three conditions are necessary:
//
//  1. !targetExplicit -- the target's expected solver is itself the version
//     table's inference (nothing in its captured apt.conf.d sets
//     APT::Solver). If the target genuinely configured a solver, that is a
//     recorded fact about the target and no measurement taken elsewhere can
//     stand in for it; compare, and report a real divergence.
//
//  2. !probe.Configured -- nothing in the probed apt's merged configuration
//     sets APT::Solver, so the probed value is that binary's own fallback
//     rather than a local policy choice. A CONFIGURED value may well be an
//     override this machine has and the target does not: that is E2's
//     `-o APT::Solver=internal` case, the one where the same apt binary
//     really does diverge from itself, and the gate must keep catching it.
//
//  3. Same apt major.minor on both sides -- a compiled-in default is a
//     property of the binary, so measuring it on one apt only describes the
//     other when they are the same apt. E2's Limitations record that two
//     point releases of one major.minor were never compared against each
//     other, so this does assume they ship the same compiled-in default.
//     That is a far weaker assumption than the one it replaces (that every
//     apt 3.x everywhere defaults to solver3, which the Debian 13
//     measurement falsifies outright), and it is strictly finer-grained:
//     the table it is correcting keys on the major number alone.
func probeMeasuresTargetDefault(probe solverProbe, probeAPTVersion, targetAPTVersion string, targetExplicit bool) bool {
	return probe.OK &&
		!probe.Configured &&
		!targetExplicit &&
		sameAPTMajorMinor(probeAPTVersion, targetAPTVersion)
}

// --- host probe: bare host, cached for the process lifetime -------------

// hostSolverProbeFunc probes this host's effective apt solver via
// `apt-config dump`, the live equivalent of core/snapshot.EffectiveSolver's
// captured-file read. A package-level var, exactly like probeContainerFunc
// in select.go, so this package's own tests substitute a deterministic
// outcome without a real apt-config on the test machine (there is none on
// Windows, and a test must not depend on the state of whatever Linux box
// happens to run it either). Production code always uses the default,
// probeHostSolver.
//
// It hands back the whole solverProbe, not just a value and an ok flag: the
// gate cannot tell a wrong version-table guess from a genuine host-side
// override without knowing whether apt reported an APT::Solver key at all.
var hostSolverProbeFunc = probeHostSolver

// hostSolverCacheKey is the sync.Map key. There is only one distinct value
// in practice -- this process has exactly one bare host to probe, unlike
// containerImageProbeCache's real (runtime, image, platform) key space in
// container_driver.go -- but the same sync.Map "store only on success"
// pattern is reused for consistency with that existing probe cache and
// because it already handles the concurrent-first-call case correctly.
type hostSolverCacheKey struct{}

type hostSolverCacheEntry struct {
	Probe solverProbe
}

// hostSolverCache caches the bare host's effective solver for the process
// lifetime, the same guarantee Capabilities.APTVersion documents for the
// container image probe ("discovered on first use and cached"). Only a
// successful probe is cached: a transient failure (apt-config momentarily
// unreadable, a cancelled context) is retried on the next call rather than
// remembered as permanent for the rest of the process -- see
// containerImageProbeCache's identical policy in container_driver.go.
var hostSolverCache sync.Map // hostSolverCacheKey{} -> hostSolverCacheEntry

// probeHostSolver is hostSolverProbeFunc's production implementation: the
// cached bare-host probe, provenance included.
func probeHostSolver(ctx context.Context) solverProbe {
	if v, found := hostSolverCache.Load(hostSolverCacheKey{}); found {
		return v.(hostSolverCacheEntry).Probe
	}
	p := probeHostSolverUncachedProbe(ctx)
	if !p.OK {
		return p
	}
	hostSolverCache.Store(hostSolverCacheKey{}, hostSolverCacheEntry{Probe: p})
	return p
}

func probeHostSolverUncachedProbe(ctx context.Context) solverProbe {
	dump, err := runAptConfigDump(ctx, "")
	if err != nil {
		return solverProbe{}
	}
	solver, configured := solverFromDump(dump)
	return solverProbe{Solver: solver, Configured: configured, OK: true}
}

// probeHostSolverCached and probeHostSolverUncached are the value-only views
// of the two functions above, for callers that care about the effective
// solver and not about how apt came to report it (solver_e2e_test.go, which
// exercises the real apt-config plumbing on Linux).
func probeHostSolverCached(ctx context.Context) (solver string, ok bool) {
	p := probeHostSolver(ctx)
	return p.Solver, p.OK
}

func probeHostSolverUncached(ctx context.Context) (solver string, ok bool) {
	p := probeHostSolverUncachedProbe(ctx)
	return p.Solver, p.OK
}

// --- resolver probe: root-scoped, uncached (correctness-critical per build) --

// probeSolverForRoot asks apt-config for the effective solver AS SEEN
// THROUGH one private root's APT_CONFIG, i.e. what actually governed this
// specific resolution -- not merely the bare host default, which can differ
// when the target's captured apt.conf.d set an explicit APT::Solver that
// filterAptConf copied into the root (it only strips Dir::* and proxy
// entries, never APT::Solver). Deliberately uncached: unlike the bare host
// (probeHostSolverCached, one stable answer for the whole process), a
// private root is rebuilt fresh per resolution and its APT_CONFIG loader
// path is not a value it would ever be correct to remember across builds.
func probeSolverForRoot(ctx context.Context, aptConfigPath string) solverProbe {
	dump, err := runAptConfigDump(ctx, aptConfigPath)
	if err != nil {
		return solverProbe{}
	}
	solver, configured := solverFromDump(dump)
	return solverProbe{Solver: solver, Configured: configured, OK: true}
}

// --- selection-time gate (select.go's localMismatchReason) --------------

// solverGateReason is the solver half of select.go's localMismatchReason,
// kept here (pure, no I/O) so the whole decision is unit-testable on any OS
// with plain inputs, and so it sits next to the evidence its comments cite.
// It returns ("", false) when the local backend may resolve for this target,
// or a one-line reason and true when it may not.
//
// The ordering of the checks is the fix. Before it, the first thing this
// logic did was compare hostSolver (a live measurement) against
// targetGateSolver (a version-table guess) and treat any difference as
// divergence -- which is why a Debian 13 host was refused for a Debian 13
// target: the guess said "3.0", trixie's own apt said "internal", and the
// disagreement was reported as though the two apts would resolve
// differently, when they are the same apt (this file's header for the raw
// measurement). So the measurement is consulted for what it can actually
// settle BEFORE the guess is allowed to contradict it.
//
// What this deliberately does NOT do is weaken the case E2 built the gate
// for. probeMeasuresTargetDefault only displaces the guess when the host's
// apt reports no APT::Solver key whatsoever; a host that carries an explicit
// APT::Solver -- E2's `-o APT::Solver=internal` lever, or an apt.conf.d edit
// -- still has its value compared against the target's expected one and
// still loses the local backend when they differ, because a configured value
// is a fact about this machine only and cannot speak for the target's.
func solverGateReason(host solverProbe, hostAPTVersion string, target snapshot.Target) (reason string, mismatched bool) {
	targetSolver, targetKnown := targetGateSolver(target)

	// The host's apt is the target's apt, and it was asked directly: there
	// is no second, independent value here to disagree with, only a version
	// table that has no business overruling a measurement of the very apt it
	// was trying to describe. targetExplicit is hard-false because Selection
	// (api.go, frozen) never carries the target's APT.Conf files -- see
	// targetGateSolver -- so at this point "the target's expected solver" can
	// only ever BE the table's guess.
	if probeMeasuresTargetDefault(host, hostAPTVersion, target.APTVersion, false) {
		return "", false
	}

	switch {
	case !host.OK, !targetKnown:
		return fmt.Sprintf(
			"could not confirm the host apt's effective solver matches the target's expected one (host solver probed=%v, target solver known=%v); preferring the container rather than risking an unverified match",
			host.OK, targetKnown), true

	case host.Solver == targetSolver:
		return "", false

	default:
		// Name how each side was determined. The Debian 13 failure was
		// legible only after someone went and read two functions to find
		// out that one of these numbers was measured and the other guessed;
		// the next person to hit a false positive here should be able to
		// see it in the error text.
		return fmt.Sprintf(
			"host apt's effective solver %q (%s) differs from the target's expected solver %q (inferred from the target's apt %s; %s) -- experiment E2: divergence tracks the solver, not the apt version (docs/experiments/E2-solver-divergence.md)",
			host.Solver, describeHostSolverProvenance(host), targetSolver,
			orUnknownVersion(target.APTVersion),
			"Selection carries no apt.conf.d, so an explicit target-side override cannot be seen until Resolve"), true
	}
}

// describeHostSolverProvenance renders where the host's probed solver value
// came from, for solverGateReason's message. The distinction is not
// decoration: "configured" is the case in which the gate is comparing two
// genuinely independent facts, and "apt's own default, no APT::Solver
// configured" is the case in which a disagreement with the version table
// means the table is wrong for this apt -- and, if the versions had matched,
// would not have been a mismatch at all.
func describeHostSolverProvenance(host solverProbe) string {
	if host.Configured {
		return "probed live via apt-config dump; explicitly configured in this host's apt configuration"
	}
	return "probed live via apt-config dump; apt's own default, no APT::Solver configured anywhere on this host"
}

// --- Resolve-time divergence recording (§ requirement 4/5) --------------

// buildFileSetFromDir reads exactly the files core/snapshot.EffectiveSolver
// needs (snap.APT.Conf) from the extracted snapshot files directory into an
// in-memory *snapshot.FileSet, mirroring core/engine/snapshot.go's
// loadFileSet -- that function's own doc explains why this bridging exists
// at all: EffectiveSolver (like InstallRecommends before it) wants a
// *FileSet, but ResolveInput/snapshot.Open hand back a files directory, not
// one. A file that cannot be read (redacted, or simply absent in a fixture)
// is skipped rather than treated as an error, matching EffectiveSolver's own
// fallback-to-the-version-default behaviour for a value it cannot read.
func buildFileSetFromDir(snap *snapshot.Snapshot, filesDir string) *snapshot.FileSet {
	fs := &snapshot.FileSet{Bytes: map[string][]byte{}}
	if snap == nil || filesDir == "" {
		return fs
	}
	for _, f := range snap.APT.Conf {
		if f.ArchivePath == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(filesDir, filepath.FromSlash(f.ArchivePath)))
		if err != nil {
			continue
		}
		fs.Bytes[f.ArchivePath] = data
	}
	return fs
}

// describeTargetSolver renders how the target's expected solver was
// determined, for buildSolverDivergenceMessage.
func describeTargetSolver(explicit bool, version string) string {
	if explicit {
		return "an explicit APT::Solver in the target's captured apt.conf.d"
	}
	return fmt.Sprintf("apt %s's own default; no explicit APT::Solver in the captured apt.conf.d", orUnknownVersion(version))
}

// describeResolverSolver renders how the resolving apt's actual solver was
// determined, for buildSolverDivergenceMessage.
func describeResolverSolver(solver string, known bool, version string) string {
	if !known {
		return "unknown (apt-config dump probe failed)"
	}
	return fmt.Sprintf("%q, probed live via apt-config dump on the resolving apt %s", solver, orUnknownVersion(version))
}

func orUnknownVersion(v string) string {
	if v == "" {
		return "(unknown version)"
	}
	return v
}

// buildSolverDivergenceMessage renders the audit string for
// lock.Resolver.SolverDivergence: which solver the target expected, which
// one actually resolved, and how each was determined -- deliberately pure
// (plain string/bool in, string out) and separated from the I/O in
// solverDivergenceForLock below so it is unit-testable on any OS with plain
// inputs, without a real apt-config anywhere. Returns "" only when both
// sides are confidently known and equal -- an unconfirmed match (either side
// unknown) is reported, not silently treated as a match, per the task's
// "populate it with something an auditor can act on five years later."
func buildSolverDivergenceMessage(targetSolver string, targetExplicit bool, targetVersion string, resolverSolver string, resolverKnown bool, resolverVersion string) string {
	targetKnown := targetSolver != ""
	switch {
	case !targetKnown:
		return fmt.Sprintf(
			"target's expected solver could not be determined (apt version %s is not a recognised apt version and its captured apt.conf.d sets no explicit APT::Solver); resolution ran with solver %s",
			orUnknownVersion(targetVersion), describeResolverSolver(resolverSolver, resolverKnown, resolverVersion))

	case !resolverKnown:
		return fmt.Sprintf(
			"target expected solver %q (%s); the resolving apt's actual solver could not be confirmed (apt-config dump probe failed on the resolving apt %s)",
			targetSolver, describeTargetSolver(targetExplicit, targetVersion), orUnknownVersion(resolverVersion))

	case targetSolver == resolverSolver:
		return ""

	default:
		return fmt.Sprintf(
			"target expected solver %q (%s); resolution actually ran with solver %s -- experiment E2 found this exact divergence changes which packages apt selects (docs/experiments/E2-solver-divergence.md)",
			targetSolver, describeTargetSolver(targetExplicit, targetVersion),
			describeResolverSolver(resolverSolver, resolverKnown, resolverVersion))
	}
}

// aptVersionDivergenceNote is the secondary, non-blocking signal E2
// demotes apt major.minor to: recorded whenever it differs, but -- unlike
// before this fix -- never by itself a reason to prefer the container (that
// gate is now the effective solver; see buildSolverDivergenceMessage above
// and select.go's localMismatchReason). E2 found zero genuine selection
// divergence across seven fixtures when only major.minor varied and the
// solver was held fixed (25.10-default vs 26.04-default), which is why this
// no longer blocks -- but E2's own Limitations section notes only one apt
// build per major.minor was available, so two different point releases of
// the same major.minor were never actually compared against each other;
// recording every major.minor difference keeps that residual uncertainty
// visible in the audit trail even though it no longer gates selection.
func aptVersionDivergenceNote(resolverVersion, targetVersion string) string {
	rMaj, rMin, rOK := aptMajorMinor(resolverVersion)
	tMaj, tMin, tOK := aptMajorMinor(targetVersion)
	if !rOK || !tOK || (rMaj == tMaj && rMin == tMin) {
		return ""
	}
	return fmt.Sprintf("resolving apt %s differs from the target's apt %s (major.minor %s.%s vs %s.%s); not blocking by itself -- experiment E2 found no selection divergence from apt version alone once the solver matched -- but two point releases of the exact same major.minor were never independently compared, so this is still recorded (docs/experiments/E2-solver-divergence.md)",
		resolverVersion, targetVersion, rMaj, rMin, tMaj, tMin)
}

// solverDivergenceForLock is the Resolve-time counterpart to select.go's
// selection-time gate (localMismatchReason / targetGateSolver): unlike that
// gate, this runs after ResolveInput has handed Resolve the full snapshot,
// so it consults the target's actual captured apt.conf.d
// (snapshot.EffectiveSolver) rather than only the release-default table, and
// it probes the resolving apt's solver through the same private-root
// configuration (rootAptConfigPath) that actually governed this resolution.
//
// It returns the string for lock.Resolver.SolverDivergence and, separately,
// the apt-major.minor note that is still recorded (as a warning) but no
// longer gates backend selection. Called once per Resolve and the results
// reused for both the warnings list and the Resolver record, rather than
// (as the pre-fix code did) computing the same comparison twice.
func solverDivergenceForLock(ctx context.Context, snap *snapshot.Snapshot, snapshotFilesDir, resolverAPTVersion, rootAptConfigPath string) (solverDivergence, versionNote string) {
	var targetSolver string
	var targetExplicit bool
	var targetVersion string
	if snap != nil {
		fs := buildFileSetFromDir(snap, snapshotFilesDir)
		targetSolver, targetExplicit = snapshot.EffectiveSolver(snap, fs)
		targetVersion = snap.Target.APTVersion
	}
	resolver := probeSolverForRoot(ctx, rootAptConfigPath)

	// Same rule as the selection-time gate (solverGateReason), for the same
	// reason and against the same failure: when the target set no explicit
	// APT::Solver, targetSolver is core/snapshot's copy of the version-table
	// guess -- which answers "3.0" for Debian 13's apt 3.0.3, whose real
	// effective solver is "internal" (this file's header). Comparing that
	// guess against a live probe of an apt of the same major.minor that has
	// no APT::Solver configured at all would stamp
	// lock.Resolver.SolverDivergence with a divergence that did not happen,
	// on every single Debian 13 build -- an audit record that cries wolf is
	// worse than none, because the next real divergence is read as more of
	// the same noise. The measurement replaces the guess it is a measurement
	// of; nothing else about the comparison changes.
	if probeMeasuresTargetDefault(resolver, resolverAPTVersion, targetVersion, targetExplicit) {
		targetSolver = resolver.Solver
	}

	solverDivergence = buildSolverDivergenceMessage(targetSolver, targetExplicit, targetVersion, resolver.Solver, resolver.OK, resolverAPTVersion)
	versionNote = aptVersionDivergenceNote(resolverAPTVersion, targetVersion)
	return solverDivergence, versionNote
}
