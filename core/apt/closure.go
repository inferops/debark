package apt

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
)

// Closure answers a different question from Resolve, and the difference is
// worth stating plainly.
//
// Resolve asks "what is MISSING on this machine", against a snapshot that
// records what the machine already has, and it fetches every answer because
// every answer has to cross the gap. Closure asks "what would a stock install
// of this release CONTAIN" — the seed metapackages of a base definition
// (ADR-014), resolved in a private root where nothing is installed. Its answer
// is a package list that becomes a synthesized snapshot's dpkg status; not one
// byte of it ends up in a bundle, so it must never download anything. A
// desktop seed closure is a couple of gigabytes of .deb files that would be
// fetched, hashed and thrown away.
//
// That is the whole reason this is a separate operation rather than a flag on
// Resolve. It is still the same oracle (ADR-001): apt in a private root
// decides, this package only reads back what it said.

// ClosureInput is one base-closure resolution.
type ClosureInput struct {
	// Snapshot is a BOOTSTRAP snapshot: the target identity, the archive
	// sources and the keyrings those sources are signed by, with DpkgStatus
	// deliberately left zero so BuildPrivateRoot writes an empty dpkg status
	// and apt resolves as if nothing were installed. Required.
	Snapshot *snapshot.Snapshot
	// SnapshotFilesDir holds the bootstrap snapshot's files (sources,
	// keyrings), exactly as for ResolveInput.
	SnapshotFilesDir string

	// Seeds are the base definition's seed packages, e.g.
	// ["ubuntu-desktop-minimal"]. At least one is required.
	Seeds []string
	// Recommends is APT::Install-Recommends for this resolution. A base
	// definition should normally set it false: a recommended package the
	// real installer did not take would be claimed as installed when it is
	// not, which is the one direction that produces a short bundle
	// (docs/experiments/E8-base-fidelity.md).
	Recommends bool

	// WorkDir is a scratch directory for the private root.
	WorkDir string
}

// ClosurePackage is one package apt said a stock install of the release
// contains. Presence, not provenance: nothing here is fetched, so there is no
// file, no digest and no origin to record.
type ClosurePackage struct {
	Name    string `json:"name"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
}

// Qualified renders the package as dpkg identifies it, name:arch.
func (p ClosurePackage) Qualified() string { return p.Name + ":" + p.Arch }

// ClosureSchemaVersion is the schema this document would carry if it ever
// left the process. Today it does not: Closure is produced here and consumed
// by core/base's finalSnapshot and synthesisWarnings, in the same run.
//
// This comment used to describe the schema as the document a hidden
// "snapshot closure" subcommand printed, and called that the way the
// container backend carried a Closure across the container boundary. There
// is no such subcommand, and there is no such crossing:
// core/apt/basecontainer.go runs `debark snapshot from-base` end to end
// inside the container and brings back one snapshot archive, precisely so
// that nothing has to carry a Closure across. The version stays because the
// field is serialised and a document without one is not a debark document
// -- but it is not evidence that a command exists.
const ClosureSchemaVersion = "debark.baseclosure/v1"

// Closure is what a base's seeds resolve to.
type Closure struct {
	SchemaVersion string `json:"schema_version"`
	// Packages is the resolved closure, sorted by (name, arch).
	Packages []ClosurePackage `json:"packages"`
	// Resolver records which apt actually answered, so a synthesized
	// snapshot can carry the same apt/dpkg versions a captured one would.
	Resolver lock.Resolver `json:"resolver"`
	// Warnings are anything the operator should know about this resolution.
	Warnings []lock.Warning `json:"warnings,omitempty"`
}

// sortClosurePackages orders a closure deterministically. Two runs of the
// same seeds against the same archive must produce a byte-identical
// synthesized snapshot, and this is the only ordering step between apt's
// output (which follows apt's own install order) and that document.
func sortClosurePackages(p []ClosurePackage) {
	sort.SliceStable(p, func(i, j int) bool {
		if p[i].Name != p[j].Name {
			return p[i].Name < p[j].Name
		}
		if p[i].Arch != p[j].Arch {
			return p[i].Arch < p[j].Arch
		}
		return p[i].Version < p[j].Version
	})
}

// validateClosureInput is the shared edge check both backends run before
// doing anything expensive, so a bad seed is named here rather than four
// frames down inside a container.
func validateClosureInput(in ClosureInput) (snapshot.Target, string, error) {
	if in.Snapshot == nil {
		return snapshot.Target{}, "", dferr.New(dferr.Usage, "apt: Closure: Snapshot is required")
	}
	target := in.Snapshot.Target
	if target.Arch == "" {
		return target, "", dferr.New(dferr.Usage, "apt: Closure: no target architecture")
	}
	if len(in.Seeds) == 0 {
		return target, "", dferr.New(dferr.Usage, "apt: Closure: no seed packages")
	}
	if in.Snapshot.DpkgStatus.ArchivePath != "" {
		// Not a formatting nicety. A bootstrap snapshot carrying a real dpkg
		// status would make apt answer "what is missing relative to THAT",
		// and the caller would write the answer out as though it were the
		// full contents of a stock install — a base claiming far fewer
		// packages than it has, which is the direction that produces a short
		// bundle. Refusing is the only safe reading of the mistake.
		return target, "", dferr.New(dferr.Usage,
			"apt: Closure: the bootstrap snapshot carries a dpkg status (%s); a base closure must resolve against an empty one",
			in.Snapshot.DpkgStatus.Path)
	}
	if err := validatePackageOperands("base seed package", in.Seeds); err != nil {
		return target, "", err
	}
	if in.WorkDir == "" {
		return target, "", dferr.New(dferr.Usage, "apt: Closure: WorkDir is required")
	}
	return target, target.Arch, nil
}

// closureFromSimulate turns apt's simulate output into the closure, refusing
// rather than dropping anything it cannot read.
//
// Only Inst actions count. A Conf line is a configure step for a package an
// Inst line already named, so counting both would double-count every package
// in the closure.
func closureFromSimulate(sim SimulateResult, defaultArch string) ([]ClosurePackage, error) {
	if perr := simulateParseError("apt-get -s install", sim); perr != nil {
		return nil, perr
	}
	if sim.Failed {
		return nil, dferr.New(dferr.Resolution,
			"apt: base seeds do not resolve against this release's archive: %s", simulateFailureMessage(sim))
	}
	seen := map[string]bool{}
	out := make([]ClosurePackage, 0, len(sim.Actions))
	for _, a := range sim.Actions {
		if a.Kind != "Inst" {
			continue
		}
		arch := a.Arch
		if arch == "" {
			arch = defaultArch
		}
		p := ClosurePackage{Name: a.Name, Arch: arch, Version: a.Version}
		if seen[p.Qualified()] {
			continue
		}
		seen[p.Qualified()] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, dferr.New(dferr.Resolution,
			"apt: base seeds resolved to nothing; a base whose closure is empty would claim a stock install contains no packages at all")
	}
	sortClosurePackages(out)
	return out, nil
}

// LocalClosure resolves a base definition's seeds with apt-get on this
// machine.
//
// It is a function rather than a Backend method for the reason iface.go
// records: a base closure has no container counterpart to be dispatched to,
// because the container runs the whole from-base command instead. It still
// goes through localBackend so that it inherits the one thing an outside
// caller could not do for itself — runnerFor, which is what actually puts
// APT_CONFIG in the child's environment and therefore what makes the private
// root's apt.conf take effect at all (docs/experiments/E4-aptconfd-leakage.md).
func LocalClosure(ctx context.Context, opts LocalOptions, in ClosureInput) (*Closure, error) {
	b, ok := newLocalBackend(opts).(*localBackend)
	if !ok {
		// newLocalBackend's own return type; unreachable unless this package
		// is edited into inconsistency, which is exactly when a clear message
		// beats a nil dereference.
		return nil, dferr.New(dferr.Environment, "apt: LocalClosure: local backend has an unexpected type")
	}
	return b.closure(ctx, in)
}

// closure runs exactly two apt-get invocations — update, then `-s install` — and
// no more. There is no --print-uris pass and no --download-only pass: see the
// package-level comment above on why a closure must never fetch.
func (b *localBackend) closure(ctx context.Context, in ClosureInput) (*Closure, error) {
	target, arch, err := validateClosureInput(in)
	if err != nil {
		return nil, err
	}

	probeCap, err := b.Probe(ctx, target)
	if err != nil {
		return nil, err
	}
	if !probeCap.Available {
		return nil, (&dferr.Error{Class: dferr.Environment, Msg: "apt: local backend: " + probeCap.Reason}).WithHint("%s", probeCap.Hint)
	}

	root, err := BuildPrivateRoot(RootSpec{
		Dir:          filepath.Join(in.WorkDir, "aptroot"),
		Arch:         arch,
		ForeignArchs: target.ForeignArchs,
		Recommends:   in.Recommends,
		// No MachineID and no phased policy of its own: a base is a claim
		// about a release, not about one machine, so PhasedNeverInclude is
		// the only defensible reading. It changes nothing here in practice —
		// phasing governs the automatic upgrade pass, never an explicitly
		// named install (docs/experiments/E1-phased-updates.md) — and there
		// is no upgrade pass in a closure.
		PhasedPolicy:        snapshot.PhasedNeverInclude,
		Snapshot:            in.Snapshot,
		SnapshotFilesDir:    in.SnapshotFilesDir,
		CopySnapshotSources: true,
	})
	if err != nil {
		return nil, err
	}

	warnings := filterBootstrapWarnings(root.Warnings)

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

	sim, err := b.simulateInstall(ctx, root, in.Seeds)
	if err != nil {
		return nil, err
	}
	packages, err := closureFromSimulate(sim, arch)
	if err != nil {
		return nil, err
	}

	evidence.Emit(b.events, evidence.TypeAPTResolve, "base closure resolved", map[string]any{
		"seeds":    len(in.Seeds),
		"packages": len(packages),
	})

	return &Closure{
		SchemaVersion: ClosureSchemaVersion,
		Packages:      packages,
		Resolver: lock.Resolver{
			Backend:           lock.BackendLocal,
			APTVersion:        probeCap.APTVersion,
			DpkgVersion:       probeCap.DpkgVersion,
			APTOptions:        root.SortedOptions(),
			PhasedUpdates:     string(snapshot.PhasedNeverInclude),
			InstallRecommends: in.Recommends,
		},
		Warnings: warnings,
	}, nil
}

// filterBootstrapWarnings drops the one private-root warning that is this
// operation's premise rather than a problem with it.
//
// BuildPrivateRoot warns "no dpkg status available; resolving as if nothing
// were installed on the target" whenever it has to write an empty status,
// because for a normal Resolve that means a snapshot arrived without the one
// file the whole design turns on. For a base closure it means the caller did
// exactly what validateClosureInput above insists on. Passing it through
// would put a line in front of the operator that reads as a degradation when
// it is the specification.
func filterBootstrapWarnings(in []lock.Warning) []lock.Warning {
	out := make([]lock.Warning, 0, len(in))
	for _, w := range in {
		if w.Code == warnNoDpkgStatus {
			continue
		}
		out = append(out, w)
	}
	return out
}

// DpkgStatusFor renders a closure as a /var/lib/dpkg/status file: one deb822
// stanza per package, marked installed.
//
// This is the whole point of a closure — it is what makes a synthesized
// snapshot indistinguishable, to every later stage, from a captured one. The
// stanzas carry only the four fields anything in debark reads back out of a
// status file (core/apt/held.go's splitDpkgStatusStanzas, core/snapshot's
// countInstalled, core/doctor): Package, Status, Architecture, Version.
//
// It is deliberately NOT a full dpkg database. A real status file carries
// Depends, Description, Conffiles and much more, and writing plausible-looking
// versions of those would be inventing facts about a machine nobody measured.
// apt needs none of them to answer "what is missing": the installed set and
// its versions is the entire input.
func DpkgStatusFor(packages []ClosurePackage) []byte {
	sorted := append([]ClosurePackage(nil), packages...)
	sortClosurePackages(sorted)
	var buf []byte
	for _, p := range sorted {
		buf = append(buf, fmt.Sprintf("Package: %s\nStatus: install ok installed\nArchitecture: %s\nVersion: %s\n\n",
			p.Name, p.Arch, p.Version)...)
	}
	return buf
}
