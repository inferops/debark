package apt

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"pault.ag/go/debian/dependency"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
)

// localBackend runs apt-get on this machine.
type localBackend struct {
	aptGetPath string
	dpkgPath   string
	runner     Runner
	events     evidence.Sink
}

// newLocalBackend is api.go's NewLocalBackend body.
func newLocalBackend(opts LocalOptions) Backend {
	aptGetPath := opts.AptGetPath
	if aptGetPath == "" {
		aptGetPath = "apt-get"
	}
	dpkgPath := opts.DpkgPath
	if dpkgPath == "" {
		dpkgPath = "dpkg"
	}
	runner := opts.Runner
	if runner == nil {
		runner = newRunner(aptGetPath)
	}
	return &localBackend{aptGetPath: aptGetPath, dpkgPath: dpkgPath, runner: runner, events: opts.Events}
}

func (b *localBackend) Kind() lock.Backend { return lock.BackendLocal }

// runnerFor returns the Runner apt-get calls against root must use. When
// b.runner is this package's own exec-based implementation it returns a copy
// with APT_CONFIG pointed at root's generated loader file (see
// PrivateRoot.AptConfigPath and docs/experiments/E4-aptconfd-leakage.md) —
// without it, the private root's captured apt.conf/apt.conf.d is silently
// never read, even though every -o Dir::* option is otherwise honoured. A
// caller-supplied Runner (how this package's own tests inject a fake) is
// returned unchanged, since a fake does not exec anything and does not care
// about environment variables.
func (b *localBackend) runnerFor(root *PrivateRoot) Runner {
	if er, ok := b.runner.(*execRunner); ok {
		return er.withAptConfig(root.AptConfigPath)
	}
	return b.runner
}

// localOSReleasePath is a var so a test can point it at a fixture file.
var localOSReleasePath = "/etc/os-release"

// Probe reports whether apt-get runs at all here, purely by trying "apt-get
// -v" through the same Runner Resolve would use — no separate PATH lookup —
// which is what makes it fully exercisable through LocalOptions.Runner in a
// Windows unit test (there is no real apt-get to find there, so the fake
// Runner is the only thing standing in for one).
func (b *localBackend) Probe(ctx context.Context, target snapshot.Target) (Capabilities, error) {
	out, err := b.runner.Run(ctx, nil, "-v")
	if err != nil {
		if dferr.ClassOf(err) == dferr.Environment {
			return Capabilities{
				Available: false,
				Reason:    "apt-get is not available on this host",
				Hint:      "install apt-get (the apt package), or build with --backend=container",
			}, nil
		}
		return Capabilities{Available: false, Reason: err.Error()}, nil
	}
	aptVersion, _ := ParseAptGetVersion(out.Stdout)
	dpkgVersion := ""
	if dOut, dErr := runDpkgVersion(ctx, b.dpkgPath); dErr == nil {
		dpkgVersion, _ = ParseDpkgVersion(dOut)
	}
	distroID, versionID := readLocalOSRelease(localOSReleasePath)
	return Capabilities{
		Available:   true,
		APTVersion:  aptVersion,
		DpkgVersion: dpkgVersion,
		DistroID:    distroID,
		VersionID:   versionID,
	}, nil
}

func runDpkgVersion(ctx context.Context, dpkgPath string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, dpkgPath, "--version")
	cmd.Env = cleanEnv(os.Environ(), "")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func readLocalOSRelease(path string) (distroID, versionID string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch k {
		case "ID":
			distroID = v
		case "VERSION_ID":
			versionID = v
		}
	}
	return distroID, versionID
}

// pkgOperand is one apt package operand — the form every requested package
// and every external package name reaches this file in — split into the
// parts its bookkeeping compares on.
type pkgOperand struct {
	Name    string
	Arch    string // "" when the operand named no architecture
	Release string
	Version string
}

// splitPackageOperand splits "name[:arch][/release][=version]" into its
// parts, in the one order that is correct: the "=version" tail first, then
// "/release", and only then ":arch" in what is left.
//
// The order is the entire point, and getting it wrong fails silently. A
// Debian version carries a ':' of its own — the epoch — so splitting the
// whole operand on its FIRST colon reads "zlib1g=1:1.2.13.dfsg-1" as the
// package "zlib1g=1" at architecture "1.2.13.dfsg-1", neither of which is
// anything, and the operand then matches nothing at all. That is not a
// hypothetical shape: every zlib1g version in the two multiarch bundles of
// the 2026-09-05 matrix run is epoched (1:1.2.13.dfsg-1 on debian 12,
// 1:1.3.dfsg-3.1ubuntu2.2 on ubuntu 24.04). Peeling the separators off in
// this order is unambiguous for every operand apt accepts: a version may
// not contain '=' or '/' (Debian Policy §5.6.12 allows alphanumerics plus
// '.', '+', '-', ':' and '~'), and neither a package name nor an
// architecture may contain any of the three.
//
// This is the single parser for the grammar validatePackageOperand
// validates, and validatePackageOperand uses it too — one parser, so the
// validator and the bookkeeping can never disagree about which part of an
// operand is the package name.
func splitPackageOperand(operand string) pkgOperand {
	var o pkgOperand
	rest := operand
	if i := strings.IndexByte(rest, '='); i >= 0 {
		rest, o.Version = rest[:i], rest[i+1:]
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest, o.Release = rest[:i], rest[i+1:]
	}
	o.Name = rest
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		o.Name, o.Arch = rest[:i], rest[i+1:]
	}
	return o
}

// baseName is an operand's package name alone: "zlib1g:i386=1:1.2.13.dfsg-1"
// is "zlib1g". It is what every bare-name-keyed set in this file is keyed by
// — resolve.AttributeReasons's roots, and heldPackageVersions' hold set —
// because both of those are populated from apt's and dpkg's own records,
// which carry the architecture in a separate field and never in the name.
// It stripped "=version" but not ":arch" until the multiarch defect below;
// see splitPackageOperand and operandMatches.
func baseName(pkgArg string) string { return splitPackageOperand(pkgArg).Name }

// Resolve runs update, optional full-upgrade, requested packages and
// external packages, then builds the Plan.
func (b *localBackend) Resolve(ctx context.Context, in ResolveInput) (*resolve.Plan, error) {
	if in.Snapshot == nil {
		return nil, dferr.New(dferr.Usage, "apt: Resolve: Snapshot is required")
	}
	target := in.Snapshot.Target

	probeCap, err := b.Probe(ctx, target)
	if err != nil {
		return nil, err
	}
	if !probeCap.Available {
		return nil, (&dferr.Error{Class: dferr.Environment, Msg: "apt: local backend: " + probeCap.Reason}).WithHint("%s", probeCap.Hint)
	}

	arch := target.Arch
	if in.Options.ArchOverride != "" {
		arch = in.Options.ArchOverride
	}
	if arch == "" {
		return nil, dferr.New(dferr.Usage, "apt: Resolve: no target architecture")
	}

	// Every operand that will reach an apt-get argv is checked here, at the
	// edge, as well as at each call site (aptOperandArgs): this is where the
	// message can still name which input was rejected, which is the whole
	// difference between a usable error and "apt: bad operand" from four
	// frames down.
	if err := validatePackageOperands("requested package", in.Packages); err != nil {
		return nil, err
	}
	if err := validatePackageOperands("external package name (a supplied .deb's own Package: field)", in.ExternalNames); err != nil {
		return nil, err
	}

	rootDir := filepath.Join(in.WorkDir, "aptroot")
	spec := RootSpec{
		Dir:                 rootDir,
		ArchivesDir:         in.ArchivesDir,
		Arch:                arch,
		ForeignArchs:        target.ForeignArchs,
		Recommends:          in.Recommends,
		PhasedPolicy:        in.PhasedPolicy,
		MachineID:           target.MachineID,
		Snapshot:            in.Snapshot,
		SnapshotFilesDir:    in.SnapshotFilesDir,
		CopySnapshotSources: true,
		ApprovedKeys:        in.ApprovedKeys,
	}
	if in.ExternalRepoDir != "" {
		abs, err := filepath.Abs(in.ExternalRepoDir)
		if err != nil {
			return nil, dferr.Wrap(dferr.Usage, err, "apt: external repo dir")
		}
		spec.ExtraSources = append(spec.ExtraSources, ExtraSource{
			Name: "debark-external",
			Line: fmt.Sprintf("deb [trusted=yes] %s ./", FileURI(abs)),
		})
	}

	root, err := BuildPrivateRoot(spec)
	if err != nil {
		return nil, err
	}

	// heldVersions is the target's dpkg-hold set, each name mapped
	// to its own current version. record, below, substitutes that version
	// for whatever apt proposed for a held package before anything is
	// pinned and fetched with -y, so apt is never asked to actually change
	// one — see held.go and record's own comment for why doing it there,
	// rather than trimming each pass's own argument list, is where this is
	// enforced.
	heldVersions, herr := heldPackageVersions(in.Snapshot, in.SnapshotFilesDir)
	if herr != nil {
		return nil, dferr.Wrap(dferr.Usage, herr, "apt: reading held packages from dpkg status")
	}

	warnings := append([]lock.Warning(nil), root.Warnings...)
	if in.PhasedPolicy == snapshot.PhasedNeverInclude && in.Upgrades {
		// APT::Get::Never-Include-Phased-Updates (like APT::Machine-ID) only
		// changes apt's automatic upgrade-candidate selection, i.e. only the
		// full-upgrade pass below; an explicitly named "install <pkg>" always
		// bypasses phasing entirely regardless of this setting, so the
		// warning is scoped to in.Upgrades and must not claim to protect the
		// requested-packages pass too. Measured, not assumed:
		// docs/experiments/E1-phased-updates.md.
		warnings = append(warnings, lock.Warning{
			Code:    "phased-updates.never-include",
			Message: "snapshot carried no machine id; the full-upgrade pass only considered fully-phased versions. This does not extend to explicitly requested packages — apt resolves those to the newest candidate regardless of phasing, with or without a machine id.",
		})
	}
	// Solver-divergence recording (sharpened by experiment E2:
	// docs/experiments/E2-solver-divergence.md). Computed once here and
	// reused below for lock.Resolver.SolverDivergence, rather than
	// recomputed a second time, since it now does real I/O (a live
	// apt-config dump probe through this root's own configuration) instead
	// of a pure string comparison.
	solverDivergenceMsg, aptVersionNote := solverDivergenceForLock(ctx, in.Snapshot, in.SnapshotFilesDir, probeCap.APTVersion, root.AptConfigPath)
	if solverDivergenceMsg != "" {
		warnings = append(warnings, lock.Warning{
			Code:    "resolver.solver-divergence",
			Message: solverDivergenceMsg,
		})
	}
	if aptVersionNote != "" {
		warnings = append(warnings, lock.Warning{
			Code:    "resolver.apt-version-differs",
			Message: aptVersionNote,
		})
	}

	updateOut, uerr := b.runnerFor(root).Run(ctx, root.Options, "update")
	upd := ParseUpdate(updateOut)
	// sources_fetched and sources_failed, not just entries: apt exits 0 with
	// no network at all, so "failed" is false and "entries" counts acquire
	// ATTEMPTS, which retries inflate. With --network none Ubuntu 24.04
	// reported {"entries": 16, "failed": false} for four sources none of
	// which was reachable, and nothing on the wire said the network was
	// down. See SourceOutcomes.
	fetchedSources, failedSources := upd.SourceOutcomes()
	evidence.Emit(b.events, evidence.TypeAPTUpdate, "apt-get update", map[string]any{
		"failed":          upd.Failed,
		"entries":         len(upd.Entries),
		"sources_fetched": fetchedSources,
		"sources_failed":  failedSources,
	})
	if upd.Failed {
		msg := upd.Error()
		if msg == "" && uerr != nil {
			msg = uerr.Error()
		}
		return nil, dferr.New(dferr.Resolution, "apt: update failed: %s", msg)
	} else if uerr != nil {
		return nil, uerr
	}
	warnings = append(warnings, updateDiagnosticWarnings(upd)...)

	versions := map[pkgRef]string{}
	// roots is every root NAME (never an operand: see baseName), for
	// resolve.AttributeReasons and for the upgrade labelling below.
	// upgradeRoots is the subset the full-upgrade pass contributed, kept
	// apart because it is the only subset whose Reason is filled in AFTER
	// AttributeReasons rather than before it — which is exactly the
	// distinction attributionRoots has to make. See its doc.
	roots := map[string]bool{}
	upgradeRoots := map[string]bool{}
	var unresolved []lock.Unresolved
	heldExcluded := map[string]bool{}
	// heldPinned collects, separately from versions, every held package
	// record substitutes its own current version for. It stays out of
	// versions (rather than merged in like an ordinary pin) because it
	// cannot be fetched the same way: measured directly against a real
	// held package that "apt-get install --reinstall pkg=<its own current
	// version> --download-only -y" is refused with the identical "Held
	// packages were changed" error a real version bump gets — a hold
	// blocks any install-transaction action against the package, not
	// specifically a version change — so fetchAndBuildSelections fetches
	// this set through downloadHeldPackages instead, a wholly different
	// apt-get subcommand. See that function's doc for the full reasoning.
	heldPinned := map[pkgRef]string{}
	// record is the single point every Inst/Conf action from every pass
	// below (full-upgrade, requested packages, external) funnels through on
	// its way into versions, the exact set fetchAndBuildSelections later
	// pins and downloads with -y. Substituting a held package's own current
	// version here, rather than by pre-trimming each pass's own argument
	// list, is deliberately the one enforcement point: every apt-get call
	// above fetchAndBuildSelections is -s/--simulate and so never touches
	// anything regardless of what it proposes, so "apt is never asked to
	// change" a held package only actually has to hold for what
	// reaches the real fetch below — and catching it here covers a held
	// package however it entered the proposal (named explicitly, or pulled
	// in by full-upgrade) with one rule instead of three. The package still
	// ends up in the bundle — a hold pins its version, it does not remove
	// it from what an operator asked for — just never at anything other
	// than the version already on the target; when that current version
	// cannot be determined at all (no parseable Version field: malformed
	// input, never seen against a real dpkg status), it is excluded
	// entirely instead, since fetching an unknown version is not an option.
	record := func(act InstAction) {
		if curVer, isHeld := heldVersions[act.Name]; isHeld {
			heldExcluded[act.Name] = true
			if curVer == "" {
				return
			}
			a := act.Arch
			if a == "" {
				a = arch
			}
			heldPinned[pkgRef{Name: act.Name, Arch: a}] = curVer
			return
		}
		recordVersion(versions, act, arch)
	}

	if in.Upgrades {
		upOut, err := b.runnerFor(root).Run(ctx, root.Options, "-s", "full-upgrade")
		if err != nil && dferr.ClassOf(err) == dferr.Environment {
			return nil, err
		}
		sim := ParseSimulate(upOut)
		if perr := simulateParseError("apt-get -s full-upgrade", sim); perr != nil {
			return nil, perr
		}
		if sim.Failed {
			warnings = append(warnings, lock.Warning{
				Code:    "resolver.full-upgrade-failed",
				Message: "apt-get -s full-upgrade reported problems and was skipped: " + sim.Summary,
			})
		} else {
			for _, act := range sim.Actions {
				record(act)
				roots[act.Name] = true
				upgradeRoots[act.Name] = true
			}
		}
	}

	if len(in.Packages) > 0 {
		reqSim, err := b.simulateInstall(ctx, root, in.Packages)
		if err != nil {
			return nil, err
		}
		if reqSim.Failed {
			return nil, dferr.New(dferr.Resolution, "apt: could not satisfy requested packages: %s", simulateFailureMessage(reqSim))
		}
		for _, act := range reqSim.Actions {
			record(act)
		}
		for _, p := range in.Packages {
			roots[baseName(p)] = true
		}
	}

	externalConfirmed := map[string]bool{}
	if len(in.ExternalNames) > 0 {
		names := dedupSorted(in.ExternalNames)
		evidence.Emit(b.events, evidence.TypeInputExternal, "external packages", map[string]any{"names": names})
		combinedSim, err := b.simulateInstall(ctx, root, names)
		if err != nil {
			return nil, err
		}
		if !combinedSim.Failed {
			for _, act := range combinedSim.Actions {
				record(act)
			}
			for _, n := range names {
				externalConfirmed[n] = true
			}
		} else {
			for _, n := range names {
				oneSim, err := b.simulateInstall(ctx, root, []string{n})
				if err != nil {
					return nil, err
				}
				if oneSim.Failed {
					if len(oneSim.Unresolved) > 0 {
						unresolved = append(unresolved, oneSim.Unresolved...)
					} else {
						unresolved = append(unresolved, lock.Unresolved{Input: n, Kind: "package", Detail: simulateFailureMessage(oneSim)})
					}
					continue
				}
				for _, act := range oneSim.Actions {
					record(act)
				}
				externalConfirmed[n] = true
			}
		}
		for n := range externalConfirmed {
			roots[baseName(n)] = true
		}
	}

	if len(heldExcluded) > 0 {
		names := make([]string, 0, len(heldExcluded))
		for n := range heldExcluded {
			names = append(names, n)
		}
		sort.Strings(names)
		// Decision: a hold wins even over an explicit "debark build <pkg>"
		// naming it by name — honour the hold and warn, rather than refuse
		// the whole build or let the explicit name outrank it. "Holds ... are
		// respected" is unconditional, and buildjob.Options
		// carries no --allow-change-held-packages-equivalent override flag
		// to make an exception with, so there is no other reading available
		// without inventing a new frozen-contract field. Refusing outright
		// would turn one deliberately-pinned package into a build-wide
		// failure for everything else requested alongside it; silently
		// overriding the hold because it was named explicitly is exactly
		// the "surprising, hard-to-audit change" a hold exists to prevent.
		// So: the package is still included at whatever version is already
		// on the target — the request is honoured, just not the version
		// bump — the build still succeeds, and a stable-coded record is left
		// for an auditor to find.
		warnings = append(warnings, lock.Warning{
			Code:     "held-package.change-skipped",
			Message:  "on hold in the target's dpkg status (Status: hold ok installed); apt proposed changing its version but the hold was honoured instead. It is included in this bundle at its current version, unchanged, even though it was requested by name or an upgrade would otherwise have moved it. Remove the hold on the target and re-resolve to include the newer version.",
			Packages: names,
		})
	}

	var selections []resolve.Selection
	var install []string
	if len(versions) > 0 || len(heldPinned) > 0 {
		selections, err = b.fetchAndBuildSelections(ctx, root, versions, heldPinned)
		if err != nil {
			return nil, err
		}
		for _, s := range selections {
			install = append(install, qualifiedNameVersion(s))
		}
	}

	entries, perr := ReadPackagesIndex(filepath.Join(rootDir, "var", "lib", "apt", "lists"))
	if perr != nil {
		warnings = append(warnings, lock.Warning{Code: "resolver.packages-index-unreadable", Message: perr.Error()})
	}
	byNV := indexByNameVersion(entries)
	depGraph := buildDependencyGraph(selections, byNV)

	// The confirmed external names, as the operand list operandMatches
	// reads. An external name is normally a vendor .deb's own bare Package:
	// field, for which this is exactly the set-membership test it replaces —
	// but it is not always bare: core/apt's own container backend requests
	// every closed-world target as an arch-qualified "--external-name
	// name:arch" (see containerBackend.ClosedWorld's doc), and those names
	// were failing the same way an arch-qualified requested package was.
	externalOperands := make([]string, 0, len(externalConfirmed))
	for n := range externalConfirmed {
		externalOperands = append(externalOperands, n)
	}

	for i := range selections {
		s := &selections[i]
		// externalConfirmed is tested BEFORE reqContains, not after.
		// "debark build foo ./foo_1.0_amd64.deb" -- a package both named
		// on the command line and supplied as a vendor .deb -- is an
		// ordinary operator action, and with the requested case first it
		// took that branch: Reason became "requested", so the
		// external-provenance downgrade below never ran (the file kept the
		// apt-signed claim it was never entitled to) and neither did the
		// ReasonExternal-gated StagedPath correction further down, leaving
		// StagedPath pointing into the archives directory at a file apt's
		// file: method never copied there. Where an input is BOTH, what it
		// is beats what it was asked for: the bytes came from the operator,
		// and that is what the lock has to say.
		switch {
		case reqContains(externalOperands, s.Name, s.Arch):
			s.Reason = lock.ReasonExternal
			s.UserSupplied = true
			s.Flags = append(s.Flags, lock.FlagUserSupplied)
			// fetchAndBuildSelections stamps every selection
			// VerifiedAPTSigned, right for the overwhelming common case (a
			// file apt fetched from an archive whose InRelease it verified)
			// but wrong here: this file came from the external staging repo,
			// added with "deb [trusted=yes] file://... ./" specifically so
			// apt would resolve its dependencies without demanding a
			// signature a vendor .deb never carries — trusted=yes means
			// nothing was actually verified. Recording apt-signed anyway
			// would claim, in the one record an auditor reads for
			// provenance, a check that never ran. core/fetch's
			// FromLocalFile faced the identical question for its own
			// local-file inputs and chose VerifiedURLUnverified (its "HTTPS
			// is not provenance" reasoning applies just the same to an
			// un-digested local file: possessing the bytes proves nothing
			// about who produced them); matching that choice here means the
			// two code paths never disagree about the provenance of what is,
			// from an auditor's chair, the same kind of input.
			s.PublisherVerification = lock.VerifiedURLUnverified
		case reqContains(in.Packages, s.Name, s.Arch):
			s.Reason = lock.ReasonRequested
		}
	}

	// fetchAndBuildSelections (above) stages every selection under
	// ArchivesDir uniformly, because that is where apt places anything it
	// actually downloads over http(s). A selection resolved from the
	// external staging repo (ExternalRepoDir, added above as a
	// "deb [trusted=yes] file://... ./" source) is different: apt's file
	// acquire method uses a local file:// package in place rather than
	// copying it into its download cache, so it never lands in ArchivesDir
	// at all — only in ExternalRepoDir, where core/engine originally staged
	// it (core/engine/inputs.go's stageExternals). Left as
	// fetchAndBuildSelections set it, every operator-supplied .deb's
	// StagedPath pointed at a file that does not exist, which
	// core/engine's ingestPlan then failed to open ("no such file or
	// directory") for every single external/vendor input — confirmed
	// against a real apt, in a real container, while diagnosing this. Now
	// that Reason has just been assigned above, every external selection is
	// identifiable, so its StagedPath is corrected here to the directory
	// the file actually lives in — the same fix core/apt's container
	// backend already applies for the identical reason; see
	// containerRewriteStagedPaths in container_driver.go, which this
	// mirrors for the local backend.
	if in.ExternalRepoDir != "" {
		for i := range selections {
			s := &selections[i]
			if s.Reason == lock.ReasonExternal && s.Filename != "" {
				s.StagedPath = filepath.Join(in.ExternalRepoDir, s.Filename)
			}
		}
	}

	resolve.AttributeReasons(attributionRoots(roots, upgradeRoots, selections), selections, depGraph)
	for i := range selections {
		if selections[i].Reason == "" && roots[selections[i].Name] {
			selections[i].Reason = lock.ReasonUpgrade
		}
	}

	resolve.SortSelections(selections)
	sort.Strings(install)

	if unsatisfiedNonHeldRequest(in.Packages, selections, heldExcluded) {
		// Some requested name produced no selection for a reason other than
		// a hold (which already got its own, more specific warning above):
		// it was already exactly satisfied on the target, so there was
		// nothing to fetch.
		warnings = append(warnings, lock.Warning{
			Code:    "resolver.already-satisfied",
			Message: "one or more requested packages were already at the exact version on the target; nothing was added for them",
		})
	}

	// Fixed once, and applied to everything free-text on its way into the
	// Plan. See lockSafeWarnings for why this is not optional.
	masks := resolveHostPathMasks(rootDir, in)

	plan := &resolve.Plan{
		Target: lock.Target{
			DistroID:     target.DistroID,
			VersionID:    target.VersionID,
			Codename:     target.Codename,
			Arch:         arch,
			ForeignArchs: target.ForeignArchs,
			APTVersion:   target.APTVersion,
			DpkgVersion:  target.DpkgVersion,
		},
		Resolver: lock.Resolver{
			Backend:           lock.BackendLocal,
			APTVersion:        probeCap.APTVersion,
			DpkgVersion:       probeCap.DpkgVersion,
			APTOptions:        sanitizeAPTOptionsForLock(root.SortedOptions(), rootDir, in.ArchivesDir),
			PhasedUpdates:     string(root.PhasedPolicy),
			InstallRecommends: in.Recommends,
			SolverDivergence:  solverDivergenceMsg,
		},
		Selections: selections,
		Install:    install,
		Warnings:   lockSafeWarnings(warnings, masks),
		Unresolved: lockSafeUnresolved(unresolved, masks),
	}
	evidence.Emit(b.events, evidence.TypeAPTResolve, "apt resolve complete", map[string]any{
		"selections": len(selections),
		"unresolved": len(unresolved),
	})
	return plan, nil
}

// sanitizeAPTOptionsForLock returns opts with every occurrence of roots
// (the private root's directory and the archives directory, both a fresh
// mkdirTemp("", "debark-build-*") subdirectory every single run — see
// core/engine/resolve.go's workDir/archivesDir) replaced by a fixed
// placeholder, so a value that can never be the same twice never reaches
// lock.json. Two builds of the identical request must produce byte-
// identical lock.json (the determinism claim, and the contract
// brief's rule 2: "Never write a host path... into an artefact"); the
// Dir::* options are exactly where that happened, since every one of them
// is built from one of these two directories (buildOptions in root.go).
//
// Only the varying path PREFIX is replaced, never the whole entry: the
// Dir::* KEYS are kept, because their presence is what actually tells an
// auditor a private root was genuinely used and which paths within it apt
// was pointed at — dropping them outright would be simpler but would throw
// that signal away for no reason, since only the value, never the key,
// differs run to run. Every other option this package ever sets
// (APT::Architecture, APT::Architectures::, APT::Install-Recommends,
// APT::Machine-ID, APT::Get::Never-Include-Phased-Updates, and any future
// APT::Solver override) carries an architecture name, a boolean or a target
// identity value, never a path under either root, so ReplaceAll leaves
// every one of them untouched rather than needing an explicit allow-list of
// "safe" keys that would have to be kept in sync with root.go by hand.
//
// This does make Resolver.APTOptions's own doc comment ("every -o option
// passed, sorted, so the run is reproducible") subtly imprecise as written:
// it is now every -o option passed, with its two known-run-varying
// directory prefixes masked. The field is not frozen-file-owned by this package (core/lock/types.go), so the doc itself is not editable here; this
// comment is the record of the deliberate gap between what it says and what
// it now does.
func sanitizeAPTOptionsForLock(opts []string, roots ...string) []string {
	out := append([]string(nil), opts...)
	for _, root := range roots {
		if root == "" {
			continue
		}
		for i, o := range out {
			out[i] = strings.ReplaceAll(o, root, "<PRIVATE-ROOT>")
		}
	}
	return out
}

// operandMatches reports whether one operand asks for the selection
// (name, arch). It is the single answer to "did the operator ask for this
// package?" that this file's requested- and external-package bookkeeping is
// built on.
//
// Debian identifies a package by (name, architecture), and an apt operand
// may say so: "zlib1g:i386" is a different ask from "zlib1g:amd64", while a
// bare "jq" means "jq, at whatever architecture resolves". So the match is
// asymmetric on purpose — an UNQUALIFIED operand matches a selection of any
// architecture, a QUALIFIED one matches only its own — which is both apt's
// own reading of an install operand and the case that must never regress:
// nearly every request ever typed is unqualified, and each has to keep
// matching the single native-arch selection it produced.
//
// Comparing the raw operand against Selection.Name (what this did before)
// meant an arch-qualified operand matched NOTHING: "zlib1g:i386" was tested
// against a selection whose Name is "zlib1g" and whose Arch is "i386". Both
// consequences reached real lock.json files in the 2026-09-05 matrix run,
// in both multiarch bundles, with the bundles themselves built correctly —
// this was never about resolution, only about what the lock records:
//
//   - Resolve's resolver.already-satisfied warning fired on every
//     arch-qualified request, telling the operator their package "was
//     already at the exact version on the target; nothing was added for it"
//     in the same lock whose packages array records it fetched, at a
//     version, with a digest.
//   - Reason could never be "requested" for one. foreign-arch-i386, whose
//     whole request is "zlib1g:i386", recorded that package as
//     "dependency-of:zlib1g:i386" — the package as a dependency of itself,
//     naming a parent that is not even a package name — and
//     multiarch-coexist recorded it as "dependency-of:jq".
func operandMatches(operand, name, arch string) bool {
	o := splitPackageOperand(operand)
	if o.Name != name {
		return false
	}
	return o.Arch == "" || o.Arch == arch
}

// reqContains reports whether any operand in pkgs asks for the selection
// (name, arch).
func reqContains(pkgs []string, name, arch string) bool {
	for _, p := range pkgs {
		if operandMatches(p, name, arch) {
			return true
		}
	}
	return false
}

// requestSatisfied reports whether some selection answers this one operand.
func requestSatisfied(operand string, selections []resolve.Selection) bool {
	for _, s := range selections {
		if operandMatches(operand, s.Name, s.Arch) {
			return true
		}
	}
	return false
}

// unsatisfiedNonHeldRequest reports whether some requested operand produced
// no selection for a reason other than being held. heldExcluded names already
// get their own, more specific held-package.change-skipped warning; counting
// them again here would print a second warning that explains the same
// absence in different, slightly contradictory language ("already at the
// exact version" is not what happened to a package a hold kept back).
//
// The question is asked per OPERAND, not per package name. It was a count of
// distinct satisfied names against a count of distinct requested names,
// which cannot see a multiarch request at all: "zlib1g:i386 zlib1g:amd64" is
// two asks that are satisfied independently, and a name-keyed count called
// the pair satisfied as soon as either one of them resolved.
func unsatisfiedNonHeldRequest(pkgs []string, selections []resolve.Selection, heldExcluded map[string]bool) bool {
	for _, p := range pkgs {
		if p == "" || heldExcluded[baseName(p)] {
			continue
		}
		if !requestSatisfied(p, selections) {
			return true
		}
	}
	return false
}

// attributionRoots is the root list resolve.AttributeReasons walks outward
// from: every root name Resolve knows about, minus any name it cannot itself
// give a Reason to.
//
// AttributeReasons deliberately leaves a seeded root's own Reason blank — a
// root is not a dependency of anything, least of all of itself, and only the
// caller knows which kind of root it is (see its doc). So a name may only be
// seeded once every selection carrying that name is one Resolve labels
// itself: requested/external immediately above, or upgrade in the loop
// immediately below, which is why upgradeRoots is passed in separately
// rather than folded into roots.
//
// Multiarch is where that stops being automatic, and it is the reason this
// function exists at all. Now that "zlib1g:i386" is understood, it makes
// "zlib1g" a root NAME — correctly: the packages it pulled in really are
// dependencies of it, and before this they were recorded as
// "dependency-of:zlib1g:i386", naming a parent that is not a package name at
// all (foreign-arch-i386's lock, 2026-09-05 matrix run). But a request for
// one architecture says nothing about the OTHER architecture's selection of
// the same name, and that sibling gets a Reason from nobody: seeding the
// name would leave it blank, and a blank reason is not merely untidy, it is
// an invalid lock (core/lock/validate.go's validReason rejects "", so the
// build fails at the write). Measured, not imagined: the same matrix run's
// multiarch-coexist bundle carries exactly that pair — zlib1g:i386, which
// was requested, and zlib1g:amd64, which jq's own closure pulled in.
// Leaving such a name unseeded costs nothing and keeps the sibling the
// honest dependency edge the graph gives it (dependency-of:jq there, which
// is where apt actually got it from).
//
// In a single-architecture build the two sets are identical, so nothing here
// changes what any non-multiarch lock records: an unqualified request
// matches its selection whatever architecture it resolved at, so no root
// name is ever dropped.
func attributionRoots(roots, upgradeRoots map[string]bool, selections []resolve.Selection) []string {
	unnamed := map[string]bool{}
	for _, s := range selections {
		if s.Reason == "" && !upgradeRoots[s.Name] {
			unnamed[s.Name] = true
		}
	}
	out := make([]string, 0, len(roots))
	for n := range roots {
		if !unnamed[n] {
			out = append(out, n)
		}
	}
	return out
}

func dedupSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// simulateInstall runs "apt-get -s install <names>" and parses it. A
// non-Resolution error (exec/environment failure) is returned directly; a
// Resolution-class failure is absorbed into the returned SimulateResult so
// the caller decides what a failed simulate means for its case (hard error
// for requested packages, retry-one-at-a-time for external ones).
func (b *localBackend) simulateInstall(ctx context.Context, root *PrivateRoot, names []string) (SimulateResult, error) {
	operands, err := aptOperandArgs("apt-get -s install", names)
	if err != nil {
		return SimulateResult{}, err
	}
	out, err := b.runnerFor(root).Run(ctx, root.Options, append([]string{"-s", "install"}, operands...)...)
	if err != nil && dferr.ClassOf(err) != dferr.Resolution {
		return SimulateResult{}, err
	}
	sim := ParseSimulate(out)
	if perr := simulateParseError("apt-get -s install", sim); perr != nil {
		return SimulateResult{}, perr
	}
	return sim, nil
}

func simulateFailureMessage(sim SimulateResult) string {
	var parts []string
	for _, u := range sim.Unresolved {
		if len(u.Missing) > 0 {
			parts = append(parts, fmt.Sprintf("%s (missing: %s)", u.Input, strings.Join(u.Missing, ", ")))
		} else {
			parts = append(parts, fmt.Sprintf("%s (%s)", u.Input, u.Detail))
		}
	}
	if sim.Summary != "" {
		parts = append(parts, sim.Summary)
	}
	if len(parts) == 0 {
		return "apt reported a failure with no further detail"
	}
	return strings.Join(parts, "; ")
}

// fetchAndBuildSelections pins every (name, version) in versions and runs
// --print-uris then the real --download-only install, then joins the
// result against the fetched Packages indices and the files that actually
// landed in ArchivesDir, hashing each one itself rather than trusting any
// printed hash. heldVersions (a disjoint set of (name, arch) keys from
// versions) is fetched separately, via downloadHeldPackages — see its own
// doc for why a held package's own current version cannot go through the
// same "install --download-only" call the rest of versions does.
func (b *localBackend) fetchAndBuildSelections(ctx context.Context, root *PrivateRoot, versions map[pkgRef]string, heldVersions map[pkgRef]string) ([]resolve.Selection, error) {
	pinned := make([]string, 0, len(versions))
	for ref, ver := range versions {
		pinned = append(pinned, ref.Name+":"+ref.Arch+"="+ver)
	}
	sort.Strings(pinned)

	byFilename := map[string]URIEntry{}
	if len(pinned) > 0 {
		// pinned is built from apt's own Inst-line output, which a hostile
		// archive controls end to end: a stanza named "-y" produced the
		// operand "-y:amd64=1.0", which apt read as an option, not a
		// package. aptOperandArgs is what stops that.
		operands, err := aptOperandArgs("apt-get install --download-only", pinned)
		if err != nil {
			return nil, err
		}
		uriOut, err := b.runnerFor(root).Run(ctx, root.Options, append([]string{"install", "--print-uris", "-y"}, operands...)...)
		if err != nil {
			return nil, err
		}
		for _, u := range ParsePrintURIs(uriOut) {
			byFilename[u.Filename] = u
		}

		dlArgs := append([]string{"--download-only", "-y", "install"}, operands...)
		dlOut, err := b.runnerFor(root).Run(ctx, root.Options, dlArgs...)
		if err != nil {
			return nil, err
		}
		if strings.Contains(strings.ToLower(string(dlOut.Combined())), "failed to fetch") {
			return nil, dferr.New(dferr.Incomplete, "apt: download failed: %s", lastLines(dlOut.Combined(), 6))
		}
	}

	staged, ok := b.archivesDirOf(root)
	if !ok {
		return nil, dferr.New(dferr.Resolution, "apt: internal: private root was built without Dir::Cache::archives")
	}
	if err := b.downloadHeldPackages(ctx, root, staged, heldVersions); err != nil {
		return nil, err
	}

	entries, err := ReadPackagesIndex(filepath.Join(root.Dir, "var", "lib", "apt", "lists"))
	if err != nil {
		return nil, dferr.Wrap(dferr.Resolution, err, "apt: reading fetched package indices")
	}
	byNV := indexByNameVersion(entries)
	// Provenance is decided over every index that offered the version, not
	// just the one byNV kept — see verificationForIndexes.
	indexFilesByNV := indexFilesByNameVersion(entries)

	// stagedEntries backs findStagedFile's fallback below: read once, used
	// per selection, rather than re-listing the directory in every
	// iteration.
	stagedEntries, direrr := os.ReadDir(staged)
	if direrr != nil && !os.IsNotExist(direrr) {
		return nil, dferr.Wrap(dferr.Resolution, direrr, "apt: reading fetched archives directory")
	}

	// Which of this root's sources declared themselves trusted=yes, i.e.
	// asked apt to accept their packages with no signature check at all.
	// See verificationForIndex.
	trustedPrefixes := trustedIndexPrefixes(root.Dir)

	all := make(map[pkgRef]string, len(versions)+len(heldVersions))
	for ref, ver := range versions {
		all[ref] = ver
	}
	for ref, ver := range heldVersions {
		all[ref] = ver
	}

	var selections []resolve.Selection
	for ref, ver := range all {
		key := nvKey(ref.Name, ref.Arch, ver)
		entry, ok := byNV[key]
		if !ok {
			// Present in the Inst-line output but not found in any fetched
			// index stanza: should not happen against a real archive; skip
			// rather than fabricate a Selection with unknown provenance.
			continue
		}
		name := ref.Name
		arch := entry.Architecture.String()
		indexFilename := filepath.Base(entry.Filename)
		filename := indexFilename
		// The archive's own "Filename:" index field usually matches the
		// name apt gives the file once it lands in Dir::Cache::archives,
		// but not always — measured against a real apt: a repository whose
		// Filename field is not already exactly "<name>_<version>_<arch>.deb"
		// (e.g. a version rendered with a different separator than the
		// control Version field, which the held-package e2e fixture's
		// synthetic repo does deliberately) is fetched under apt's own
		// computed name instead, not the index's. Trust the index's
		// prediction only while a file by that name is actually present;
		// otherwise fall back to what the directory itself says, which is
		// always right. A package name and architecture never contain '_'
		// (Debian Policy §5.6.7), so matching "<name>_..._<arch>.deb" is
		// exact regardless of how the version segment in between is spelled.
		if _, statErr := os.Stat(filepath.Join(staged, filename)); statErr != nil {
			if actual, found := findStagedFile(stagedEntries, name, arch, ver); found {
				filename = actual
			}
		}
		stagedPath := filepath.Join(staged, filename)
		var size int64
		var sha256 string
		if fi, statErr := os.Stat(stagedPath); statErr == nil {
			size = fi.Size()
			if h, _, hErr := digest.SHA256File(stagedPath); hErr == nil {
				sha256 = h
			}
		}
		// The --print-uris response is keyed by the index-predicted name
		// (both come from the same archive metadata), independently of
		// whichever name the fetch actually landed under above.
		u, haveURI := byFilename[indexFilename]
		originURI := ""
		if haveURI {
			originURI = u.URI
		}
		if size == 0 {
			size = int64(entry.Size)
		}
		if sha256 == "" {
			sha256 = strings.ToLower(entry.SHA256)
		}

		sel := resolve.Selection{
			Name:                  name,
			Arch:                  arch,
			Version:               ver,
			SourcePackage:         entry.SourcePackage(),
			Section:               entry.Section,
			Filename:              filename,
			Size:                  size,
			SHA256:                sha256,
			URI:                   originURI,
			Origin:                originFor(root, entry),
			PublisherVerification: verificationForIndexes(indexFilesByNV[key], trustedPrefixes),
			Essential:             entry.Essential(),
			StagedPath:            stagedPath,
		}
		if flag, ok := distro.ComponentFlag(entry.Component); ok {
			sel.Flags = append(sel.Flags, flag)
		}
		selections = append(selections, sel)
	}
	return selections, nil
}

// downloadHeldPackages fetches heldVersions' .deb files into dir via
// "apt-get download", not "apt-get install ... --download-only".
//
// Every form of "install" (with or without --reinstall, at a version bump
// or at the package's own already-installed version) is refused by apt for
// a held package once -y is in effect and --allow-change-held-packages is
// not: measured directly, against a real apt-mark hold-ed package, that
// "apt-get install --reinstall pkg=<its own current version>
// --download-only -y" hits the identical "E: Held packages were changed
// and -y was used without --allow-change-held-packages" a genuine version
// bump gets. A hold blocks apt from computing ANY install-transaction
// action against the package, not specifically a version change — so no
// amount of choosing what version to pin at, inside an "install" call,
// gets a held package's bytes fetched at all.
//
// "apt-get download <name>=<version>" is a different apt-get subcommand:
// it performs no dependency resolution and computes no install transaction
// at all, only "give me this exact archive file", so it never consults
// dpkg's hold state in the first place — confirmed directly, against the
// same held package, that it succeeds with no held-package complaint
// whatsoever.
//
// download's one limitation this works around: it always writes into the
// current working directory and, measured directly, ignores a
// "-o Dir::Cache::Archives=..." override entirely. Rather than redirect
// that (the frozen Runner interface has no notion of a working directory,
// and a process-global os.Chdir for the duration of one apt-get call is
// worse), this records what dir's future sibling — the process's actual
// cwd — held before the call, runs it, and moves whatever new file matches
// each requested (name, arch) into dir itself, exactly where
// fetchAndBuildSelections expects every fetched file to already be.
func (b *localBackend) downloadHeldPackages(ctx context.Context, root *PrivateRoot, dir string, heldVersions map[pkgRef]string) error {
	if len(heldVersions) == 0 {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "apt: held package download: getwd")
	}
	before, err := os.ReadDir(cwd)
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "apt: held package download: read %s", cwd)
	}
	beforeNames := make(map[string]bool, len(before))
	for _, e := range before {
		beforeNames[e.Name()] = true
	}

	names := make([]string, 0, len(heldVersions))
	for ref, ver := range heldVersions {
		names = append(names, ref.Name+":"+ref.Arch+"="+ver)
	}
	sort.Strings(names)

	operands, err := aptOperandArgs("apt-get download", names)
	if err != nil {
		return err
	}
	out, err := b.runnerFor(root).Run(ctx, root.Options, append([]string{"download"}, operands...)...)
	if err != nil {
		return err
	}
	if strings.Contains(strings.ToLower(string(out.Combined())), "failed to fetch") {
		return dferr.New(dferr.Incomplete, "apt: held package download failed: %s", lastLines(out.Combined(), 6))
	}

	after, err := os.ReadDir(cwd)
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "apt: held package download: read %s after fetch", cwd)
	}
	var newEntries []os.DirEntry
	for _, e := range after {
		if !beforeNames[e.Name()] {
			newEntries = append(newEntries, e)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dferr.Wrap(dferr.Environment, err, "apt: held package download: create %s", dir)
	}
	for ref, ver := range heldVersions {
		name, found := findStagedFile(newEntries, ref.Name, ref.Arch, ver)
		if !found {
			// fetchAndBuildSelections's own byNV/StagedPath handling
			// already tolerates a ref with no file behind it (skips
			// rather than fabricates a Selection); nothing more to do
			// here than leave it unfetched.
			continue
		}
		if err := os.Rename(filepath.Join(cwd, name), filepath.Join(dir, name)); err != nil {
			return dferr.Wrap(dferr.Environment, err, "apt: held package download: move %s", name)
		}
	}
	return nil
}

// findStagedFile looks for the one .deb in entries (a directory listing of
// the archives directory) belonging to (name, arch, version): a file named
// exactly "<name>_<version>_<arch>.deb". A package name and an architecture
// token never contain '_' (Debian Policy §5.6.7), so the two outer segments
// are unambiguous, and the middle one is the version apt itself renders the
// file under when it does not simply reuse the archive's own "Filename:"
// field — which is the case this fallback exists for.
//
// The version must match, not merely be present. Matching "<name>_..._<arch>
// .deb" on the outer segments alone let a repository that controls both its
// Package: and its Filename: fields have the WRONG file attributed to a
// selection: publish foo 1.0 as "foo_666_amd64.deb" and foo 2.0 with a
// Filename: that resolves to nothing on disk, and foo 2.0's Selection picked
// up foo 1.0's bytes. The SHA256 is then computed over those substituted
// bytes, so the lock is perfectly self-consistent while naming a package it
// does not contain — the one failure mode a digest cannot catch.
//
// A ':' in an epoch is percent-encoded in a file name ("1%3a2.0-1"), so that
// spelling of the version is accepted as well as the literal one.
func findStagedFile(entries []os.DirEntry, name, arch, version string) (string, bool) {
	prefix := name + "_"
	suffix := "_" + arch + ".deb"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, suffix) {
			continue
		}
		if !stagedVersionMatches(n[len(prefix):len(n)-len(suffix)], version) {
			continue
		}
		return n, true
	}
	return "", false
}

// stagedVersionMatches compares a file name's version segment against the
// resolved version, tolerating the percent-encoding apt applies to the ':'
// of an epoch (and only that: nothing else here is decoded, so a segment
// cannot smuggle a separator back in).
func stagedVersionMatches(segment, version string) bool {
	if version == "" {
		return false
	}
	if segment == version {
		return true
	}
	return strings.EqualFold(segment, strings.ReplaceAll(version, ":", "%3a"))
}

// archivesDirOf recovers the archives directory from root's own option list,
// so fetchAndBuildSelections never has to be told it twice.
func (b *localBackend) archivesDirOf(root *PrivateRoot) (string, bool) {
	const prefix = "Dir::Cache::archives="
	for _, o := range root.Options {
		if strings.HasPrefix(o, prefix) {
			return strings.TrimPrefix(o, prefix), true
		}
	}
	return "", false
}

// pkgRef identifies one resolved package by name and architecture. A plain
// name is not enough to key a set of resolved files: a Multi-Arch: same
// library can legitimately be selected at two architectures under the same
// name (and even the same version), which is exactly why the lock's Install
// entries are architecture-qualified (name:arch=version, never bare
// name=version) — see resolve.Plan.Install's doc.
type pkgRef struct{ Name, Arch string }

// recordVersion adds one Inst-line result to versions, keyed by (name, arch).
// nativeArch is used when the Inst line's own architecture could not be
// determined (every real one seen empirically always carries a "[arch]"
// bracket, so this is a defensive fallback, not the common path).
func recordVersion(versions map[pkgRef]string, act InstAction, nativeArch string) {
	arch := act.Arch
	if arch == "" {
		arch = nativeArch
	}
	versions[pkgRef{Name: act.Name, Arch: arch}] = act.Version
}

// qualifiedNameVersion renders a Selection in the lock's required
// name:arch=version form.
func qualifiedNameVersion(s resolve.Selection) string {
	return s.Name + ":" + s.Arch + "=" + s.Version
}

func nvKey(name, arch, version string) string { return name + "\x00" + arch + "\x00" + version }

func indexByNameVersion(entries []PackagesIndexEntry) map[string]PackagesIndexEntry {
	out := make(map[string]PackagesIndexEntry, len(entries))
	for _, e := range entries {
		out[nvKey(e.Package, e.Architecture.String(), e.Version.String())] = e
	}
	return out
}

// indexFilesByNameVersion records the name of every fetched index that
// offered a given (name, arch, version), in ReadPackagesIndex's own sorted
// order.
//
// indexByNameVersion above is last-write-wins, which is harmless for the
// metadata it is read for — two archives carrying the same name and version
// describe the same package — but not for provenance, where WHICH archive
// offered it is the entire question. See verificationForIndexes.
func indexFilesByNameVersion(entries []PackagesIndexEntry) map[string][]string {
	out := make(map[string][]string, len(entries))
	for _, e := range entries {
		k := nvKey(e.Package, e.Architecture.String(), e.Version.String())
		out[k] = append(out[k], e.IndexFile)
	}
	return out
}

// releaseFileRE recovers the "<mangled-uri>_dists_<suite>" prefix shared by a
// Packages index and its InRelease/Release file — see ReadPackagesIndex.
var releaseFileRE = regexp.MustCompile(`^(.*_dists_.+?)_[^_]+_binary-[^_]+_Packages$`)

func originFor(root *PrivateRoot, entry PackagesIndexEntry) lock.Origin {
	o := lock.Origin{Suite: entry.Suite, Component: entry.Component}
	if base, ok := releaseBaseFor(entry.IndexFile); ok {
		listsDir := filepath.Join(root.Dir, "var", "lib", "apt", "lists")
		for _, suf := range []string{"_InRelease", "_Release"} {
			p := filepath.Join(listsDir, base+suf)
			if h, _, err := digest.SHA256File(p); err == nil {
				o.ReleaseDigest = h
				break
			}
		}
	}
	if fp, ok := singleSourceFingerprint(root); ok {
		o.KeyFingerprint = fp
	}
	return o
}

// singleSourceFingerprint returns the one archive key fingerprint used
// across the private root when there is exactly one distinct one, which
// covers the overwhelming common case (a snapshot with a single archive key).
// A snapshot with several distinctly-keyed sources needs per-suite
// attribution this package does not attempt (documented limitation): a
// selection's KeyFingerprint is left empty rather than guessed.
func singleSourceFingerprint(root *PrivateRoot) (string, bool) {
	seen := map[string]bool{}
	for _, rec := range root.SignedBy {
		for _, fp := range rec.Fingerprints {
			seen[fp] = true
		}
	}
	if len(seen) != 1 {
		return "", false
	}
	for fp := range seen {
		return fp, true
	}
	return "", false
}

func releaseBaseFor(indexFile string) (string, bool) {
	m := releaseFileRE.FindStringSubmatch(indexFile)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// buildDependencyGraph builds a name -> []name adjacency restricted to known
// (resolved) package names, from each selection's own Depends, Pre-Depends
// and Recommends fields. For each relation (an AND-joined requirement), the
// first OR-alternative whose name is itself a resolved selection becomes the
// edge; alternatives satisfied by something outside this bundle (already on
// the target, a virtual package, ...) contribute no edge, exactly as
// principle 1 requires — apt decided the closure, this only reads back which
// resolved package's own control data explains which other resolved package.
func buildDependencyGraph(selections []resolve.Selection, byNV map[string]PackagesIndexEntry) map[string][]string {
	known := make(map[string]bool, len(selections))
	for _, s := range selections {
		known[s.Name] = true
	}
	graph := make(map[string][]string, len(selections))
	for _, s := range selections {
		entry, ok := byNV[nvKey(s.Name, s.Arch, s.Version)]
		if !ok {
			continue
		}
		var deps []dependency.Dependency
		deps = append(deps, entry.GetPreDepends(), entry.GetDepends())
		if raw, ok := entry.Values["Recommends"]; ok && strings.TrimSpace(raw) != "" {
			if d, err := dependency.Parse(raw); err == nil && d != nil {
				deps = append(deps, *d)
			}
		}
		seen := map[string]bool{}
		for _, d := range deps {
			for _, rel := range d.Relations {
				for _, poss := range rel.Possibilities {
					if known[poss.Name] && !seen[poss.Name] {
						seen[poss.Name] = true
						graph[s.Name] = append(graph[s.Name], poss.Name)
					}
				}
			}
		}
	}
	return graph
}

// Solver-divergence recording used to live here as a pure major.minor
// comparison (solverDivergence). Experiment E2
// (docs/experiments/E2-solver-divergence.md) found that check gated on the
// wrong signal; it has been replaced by solverDivergenceForLock in
// solver.go, called above in Resolve, which compares the effective
// APT::Solver instead and additionally records the old major.minor
// comparison as a separate, non-blocking note (aptVersionDivergenceNote).

// --- apt argv hygiene ------------------------------------------------------

// aptOperandArgs renders the operand half of an apt-get command line: the
// "--" end-of-options terminator, then the validated operands.
//
// SECURITY. ADR-001 makes exec.Command, never a shell, the only way this
// package starts apt, so the risk here is not shell metacharacters — there is
// no shell anywhere in the path, verified. The risk is apt's own argv
// parsing: apt-get accepts options and operands freely interleaved, so an
// operand beginning with '-' is not a package name at all, it is an OPTION.
// Confirmed by printing the argv actually produced before this existed:
//
//	apt-get -o ... -s install --allow-unauthenticated \
//	    --allow-insecure-repositories --print-uris -y
//
// — six "package names" of which not one was read as a package, two of which
// switch off the archive authentication that makes apt a trustworthy oracle
// in the first place. Three unvalidated sources reach this point: a --list
// file line or bare CLI argument (both classified as "an apt package name" by
// falling through every other case upstream), a vendor .deb's own Package:
// field, and apt's own Inst-line output, which a hostile archive writes.
//
// "--" closes the option half: everything after it is a file/operand to apt's
// CommandLine parser regardless of how it starts. Validation closes the
// other half, which "--" does not touch — an operand that is not a package
// name is not something to hand apt and hope, because whatever apt then
// resolves it to is what ends up in the bundle and in the lock.
func aptOperandArgs(what string, operands []string) ([]string, error) {
	if err := validatePackageOperands(what, operands); err != nil {
		return nil, err
	}
	if len(operands) == 0 {
		return nil, nil
	}
	return append([]string{"--"}, operands...), nil
}

func validatePackageOperands(what string, operands []string) error {
	for _, o := range operands {
		if err := validatePackageOperand(what, o); err != nil {
			return err
		}
	}
	return nil
}

// validatePackageOperand checks one apt operand of the form
// "name[:arch][/release|=version]".
//
// The name is checked with repository.ValidPackageName — the same validator
// core/repository already applies before interpolating a package name into a
// pool path, for the same reason (it is attacker-controlled for a vendor
// .deb) and with the same grammar (Debian Policy §5.6.1). It was simply never
// applied on this side. The qualifier parts get their own, narrower character
// sets: they are not names, but they do travel into file names, deb822
// records and evidence strings alongside one.
func validatePackageOperand(what, operand string) error {
	bad := func(why string) error {
		return dferr.New(dferr.Usage, "apt: %s %q is not a valid package operand: %s", what, operand, why).
			WithHint("expected an apt package name, optionally name=version, name:arch or name/release (Debian Policy §5.6.1: lowercase letters, digits, '+', '-' and '.', starting with a letter or digit)")
	}
	if operand == "" {
		return bad("it is empty")
	}
	if strings.HasPrefix(operand, "-") {
		// Caught explicitly, ahead of the name check, so the message says
		// what actually happens rather than "invalid character".
		return bad("it begins with '-', which apt reads as an option, not as a package")
	}
	// The same splitter the requested-package bookkeeping matches with, so
	// that what this function calls the package name and what operandMatches
	// compares against a Selection can never be two different substrings of
	// one operand.
	o := splitPackageOperand(operand)
	name, arch, release, version := o.Name, o.Arch, o.Release, o.Version
	if !repository.ValidPackageName(name) {
		return bad("the package name part is not a legal Debian binary package name")
	}
	// Debian Policy §5.6.12: a version is alphanumerics plus '.', '+', '-',
	// ':' (the epoch separator) and '~'.
	if version != "" && !validOperandToken(version, ".+-:~") {
		return bad("the version part contains a character a Debian version never has")
	}
	// An architecture (dpkg-architecture) and a release/suite name are both
	// lowercase alphanumerics with '-' (and, for a suite, '.').
	if arch != "" && !validOperandToken(arch, "-.") {
		return bad("the architecture qualifier contains a character an architecture name never has")
	}
	if release != "" && !validOperandToken(release, "-.") {
		return bad("the release qualifier contains a character a suite name never has")
	}
	if strings.Contains(operand, "=") && version == "" {
		return bad("it ends in '=' with no version after it")
	}
	return nil
}

func validOperandToken(s, extra string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte(extra, c) >= 0:
		default:
			return false
		}
	}
	return true
}

// simulateParseError turns a simulate whose own action lines this package
// could not read into a hard error.
//
// apt is the oracle; output this package cannot read is output whose meaning
// it does not know, and the honest response is to stop. The alternative was
// measured, not imagined: instConfRE silently dropped the line, so the
// package never entered the version set, was never fetched, and never made it
// into the closure — and its absence then reached the operator as
// unsatisfiedNonHeldRequest's reassuring "already at the exact version on the
// target" warning, with the build exiting 0. On the install-time target that
// is an unmet dependency in a bundle whose whole promise is that there are
// none.
func simulateParseError(what string, sim SimulateResult) error {
	if len(sim.UnparsedActions) == 0 {
		return nil
	}
	return dferr.New(dferr.Resolution,
		"apt: %s printed %d package-action line(s) this parser could not read: %s",
		what, len(sim.UnparsedActions), strings.Join(sim.UnparsedActions, " | ")).
		WithHint("this is apt's own output for a package action, so it cannot be skipped without silently dropping that package from the closure; report the line above with the apt version from the lock's resolver block")
}

// updateDiagnosticWarnings records, as lock warnings, the per-item failures
// apt-get update reported while still exiting 0.
//
// ParseUpdate deliberately does not treat an individual Err:/GPG-error line
// as a run failure (apt retries internally, and a retry that succeeded is not
// a failure — see its doc), and UpdateResult.Error() is only ever called on
// the already-failed path. The consequence was that a GPG error apt tolerated
// vanished entirely: with insecure-repository options in play apt reports a
// bad signature as "W:" and exits 0, so debark resolved against an
// unauthenticated repository, recorded nothing at all, and stamped every
// selection apt-signed. These warnings are the record that the check was
// tried and complained about, whatever apt then decided to do.
func updateDiagnosticWarnings(upd UpdateResult) []lock.Warning {
	var out []lock.Warning
	// The whole-machine case, stated once and first, because the per-source
	// warnings below read as four separate hiccups when they are one cause.
	// apt exits 0 when every index fails to download -- it prints "W: Some
	// index files failed to download. They have been ignored, or old ones
	// used instead." and carries on with whatever it already had, which in a
	// fresh container is nothing. The resolution that follows then fails on
	// the base definition's own seed packages, and the operator is told that
	// "apt" and "init" cannot be located when what actually happened is that
	// the network is unreachable.
	//
	// This does not fail the run. apt is the oracle for whether an update
	// failed (ADR-001, and ParseUpdate's own doc), and overriding its exit
	// code here would break every legitimate run against a source that is
	// meant to be partially unavailable. It records what apt said, in a
	// sentence that names the cause.
	if fetched, failed := upd.SourceOutcomes(); fetched == 0 && failed > 0 {
		out = append(out, lock.Warning{
			Code: "apt-update.no-index-fetched",
			Message: fmt.Sprintf("apt-get update could not fetch an index from any of the %d configured source(s), and apt still exited 0 (it falls back to whatever indexes were already on disk). "+
				"If anything below failed to resolve, this is why: the archive was unreachable, not the package missing. Check network, DNS and proxy settings for this machine.", failed),
		})
	}
	for _, ge := range upd.GPGErrors {
		msg := "apt-get update reported a GPG error for " + ge.Repository
		if len(ge.NoPubkeys) > 0 {
			msg += ": missing key(s) " + strings.Join(ge.NoPubkeys, ", ")
		} else if ge.Detail != "" {
			msg += ": " + ge.Detail
		}
		out = append(out, lock.Warning{
			Code:    "apt-update.gpg-error",
			Message: msg + ". apt did not fail the update, so the resolution below went ahead against this repository; treat anything sourced from it as unauthenticated.",
		})
	}
	for _, e := range upd.Entries {
		if e.Status != "err" {
			continue
		}
		out = append(out, lock.Warning{
			Code:    "apt-update.fetch-error",
			Message: "apt-get update could not fetch " + e.Detail + ". apt did not fail the update (it retries internally), so this may have succeeded on a later attempt; it is recorded because an index that never arrived silently narrows what apt had to resolve against.",
		})
	}
	return out
}

// --- lock.json hygiene -----------------------------------------------------

// hostPathMask is one per-run directory paired with the fixed placeholder
// that replaces it. Both fields are plain strings because apt's own output
// is not a path grammar this package can parse - the only reliable thing to
// do with it is replace the directories debark itself created, which is
// exactly what sanitizeAPTOptionsForLock already does for the options.
type hostPathMask struct {
	Path        string
	Placeholder string
}

// maskHostPaths replaces every occurrence of each mask's directory with its
// placeholder, in the order given: a containing directory must be masked
// after the directories inside it, or masking WorkDir first would leave the
// tail of a rootDir that sits under it behind. resolveHostPathMasks
// guarantees that ordering by construction rather than by hand.
//
// Each directory is masked in both separator forms. On Windows debark
// hands apt a backslash path, but apt's messages and the list filenames it
// reports come back with forward slashes, so masking only the native form
// would leave the run-varying path in lock.json on the very platform the
// private root is most used from - which is the determinism break this
// masking exists to close.
func maskHostPaths(s string, masks []hostPathMask) string {
	if s == "" {
		return s
	}
	for _, m := range masks {
		if m.Path == "" {
			continue
		}
		s = strings.ReplaceAll(s, m.Path, m.Placeholder)
		if alt := filepath.ToSlash(m.Path); alt != m.Path {
			s = strings.ReplaceAll(s, alt, m.Placeholder)
		}
	}
	return s
}

// resolveHostPathMasks is the set of per-run directories that must never
// appear in anything Resolve writes into the Plan. The placeholders match the
// two conventions already in this package (sanitizeAPTOptionsForLock's
// <PRIVATE-ROOT>, and the named per-directory placeholders the closed-world
// check uses) rather than inventing a third.
//
// The list is sorted longest path first, not hand-ordered, because
// maskHostPaths applies the masks in order and a containing directory masked
// first leaves the tail of the one inside it behind ("<WORK-DIR>/aptroot/..."
// still varies run to run). rootDir is always WorkDir/aptroot, and
// core/engine's ArchivesDir sits under the same per-build temporary
// directory, so today the hand-written order below would happen to be right —
// but the next field added to ResolveInput would have to get that reasoning
// right again, and getting it wrong fails silently, by leaking exactly what
// this exists to mask. Sorting makes the order a property of the data, not of
// whoever edits this list next: a longer path can never be a prefix of a
// shorter one, so longest-first is correct for any nesting.
func resolveHostPathMasks(rootDir string, in ResolveInput) []hostPathMask {
	masks := []hostPathMask{
		{rootDir, "<PRIVATE-ROOT>"},
		{in.ArchivesDir, "<PRIVATE-ROOT>"},
		{in.ExternalRepoDir, "<EXTERNAL-REPO>"},
		{in.SnapshotFilesDir, "<SNAPSHOT-FILES>"},
		{in.WorkDir, "<WORK-DIR>"},
	}
	sort.SliceStable(masks, func(i, j int) bool {
		return len(masks[i].Path) > len(masks[j].Path)
	})
	return masks
}

// lockSafeWarnings scrubs every warning on its way into the Plan, because
// lock.json ships inside the bundle and crosses the air gap.
//
// Two different problems, one place to fix them, which is why this is done
// here rather than at each of the dozen append sites:
//
//   - CREDENTIALS. A warning is very often apt's own text, verbatim, and apt
//     prints the URI it was given: "E: Failed to fetch
//     https://ci-bot:s3cr3t@artifactory.corp/deb/dists/stable/InRelease 401"
//     put a working token into a file that is then shipped to everyone who
//     receives the bundle. core/fetch.RedactURL is the single place this
//     project decides how a possibly-credentialed URL may reach a persisted
//     artefact; every URI embedded in warning text now goes through it.
//   - DETERMINISM. The same warnings carry host paths — a parse failure names
//     the file, and every one of those files lives under a
//     mkdirTemp("", "debark-build-*") directory that is different on every
//     single run. Two builds of one identical request must produce
//     byte-identical lock.json. This is the same defect already fixed
//     for Resolver.APTOptions (sanitizeAPTOptionsForLock), in the same run,
//     through a different field.
func lockSafeWarnings(ws []lock.Warning, masks []hostPathMask) []lock.Warning {
	if len(ws) == 0 {
		return nil
	}
	out := make([]lock.Warning, len(ws))
	copy(out, ws)
	for i := range out {
		out[i].Message = lockSafeText(out[i].Message, masks)
	}
	return out
}

// lockSafeUnresolved applies the identical treatment to Unresolved.Detail,
// which is likewise apt's own text copied verbatim into lock.json.
func lockSafeUnresolved(us []lock.Unresolved, masks []hostPathMask) []lock.Unresolved {
	if len(us) == 0 {
		return nil
	}
	out := make([]lock.Unresolved, len(us))
	copy(out, us)
	for i := range out {
		out[i].Detail = lockSafeText(out[i].Detail, masks)
	}
	return out
}

func lockSafeText(s string, masks []hostPathMask) string {
	if s == "" {
		return s
	}
	return maskHostPaths(redactURIsInText(s), masks)
}

// embeddedURIRE finds an absolute URI inside free text. The character class
// stops at whitespace and at the quoting characters apt puts around a URI,
// which is what keeps "…InRelease  401  Unauthorized" from swallowing the
// status text into the URI.
var embeddedURIRE = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"<>]+`)

// redactURIsInText runs every URI embedded in s through fetch.RedactURL.
// Trailing sentence punctuation is peeled off first and put back after: apt's
// own lines end a URI at whitespace, but this package's own messages quote
// one mid-sentence, and "https://host/x)." must not be handed to a URL parser
// with the ")." still attached.
func redactURIsInText(s string) string {
	return embeddedURIRE.ReplaceAllStringFunc(s, func(m string) string {
		trail := ""
		for len(m) > 0 && strings.IndexByte(".,;:!?)]}>", m[len(m)-1]) >= 0 {
			trail = m[len(m)-1:] + trail
			m = m[:len(m)-1]
		}
		return fetch.RedactURL(m) + trail
	})
}

// verificationForIndexes decides what a selection may claim about who
// authenticated it, from the sources its index stanza actually came from.
//
// fetchAndBuildSelections stamped every selection lock.VerifiedAPTSigned
// unconditionally, which is right for the overwhelming common case and flatly
// wrong for a source that carries trusted=yes: that option tells apt to skip
// signature verification entirely, and apt then resolves, downloads and exits
// 0 exactly as it would for a properly signed archive. A snapshot's
// "deb [trusted=yes] http://vendor.example/ ..." survives verbatim into the
// private root, so a package from it landed in lock.json as "apt-signed" —
// the lock asserting a check that provably did not run, in the one record an
// auditor reads for provenance.
//
// lock.VerifiedURLUnverified is the same answer the external staging repo
// already gets (Resolve's ReasonExternal branch) and the same one
// core/fetch's FromLocalFile chose for its own unsigned local inputs, so the
// three paths agree about what is, from an auditor's chair, the same claim:
// we have the bytes, and nothing verified who produced them.
//
// It is handed EVERY fetched index that offered this (name, arch, version),
// not just the one indexByNameVersion kept, and answers with the weakest
// claim any of them supports. Half the fix was to notice trusted=yes at all;
// the other half is that a single index file is not enough to decide from.
// indexByNameVersion is last-write-wins over ReadPackagesIndex's name-sorted
// glob, so when a signed archive and a captured trusted=yes source both offer
// the identical version, which one decided the recorded provenance was
// nothing but the alphabetical order of apt's own list-file names —
// "attacker.example" sorts before "deb.debian.org", so the signed stanza won
// and the selection was stamped apt-signed even though the bytes could
// equally have come from the source apt never checked. Nothing in the fetched
// metadata distinguishes the two (same name, same version, same digest field
// if the mirror is honest about it), so the answer is the weaker one: the
// same "fail in the safe direction" rule aptURItoFileName's own doc states —
// downgrade a provenance claim rather than inflate one. A false
// url-unverified costs a policy warning; a false apt-signed is a lie in the
// audit record.
func verificationForIndexes(indexFiles []string, trustedPrefixes []string) lock.PublisherVerification {
	for _, f := range indexFiles {
		if indexFileBelongsTo(f, trustedPrefixes) {
			return lock.VerifiedURLUnverified
		}
	}
	return lock.VerifiedAPTSigned
}
