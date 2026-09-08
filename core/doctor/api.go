// Package doctor reports what is likely to go wrong on the far side of the
// gap: maintainer scripts that want the network, snap shim packages, DKMS
// packages without matching headers, and redistribution terms.
//
// Every finding is a heuristic and says so. The wording is deliberate: doctor
// reports "no obvious network action found", never "proven safe".
//
// # This package runs before verification
//
// `debark doctor BUNDLE` opens the bundle through bundle.Open, which parses
// and never verifies (documented, by design — verify.Verify is the gate), and
// then, with ScanScripts, extracts and scans maintainer scripts out of the
// bundle's .deb files. So every byte this package parses is attacker-chosen
// and unauthenticated, and doctor is precisely the command an operator runs
// FIRST on media that just arrived, on a machine chosen for being air-gapped
// rather than for being fast. Two things follow, and they shape the code:
//
//   - Every parse and every decompression is bounded, and each bound carries
//     the number that was measured without it. See ar.go (per-member and
//     whole-archive byte budgets, member count), deb.go (scanByteLimit,
//     maxZstdWindow), deb822.go (linear folded-field accumulation, stanza
//     count) and network.go (maxScriptMatches).
//   - Nothing doctor reports is trusted as text. Everything that reaches a
//     terminal goes through display.go first.
//
// Running doctor is not a substitute for `debark verify`, and a clean
// doctor report says nothing about a bundle's provenance. Verify first
// whenever a signature is available; doctor exists to describe what a bundle
// will do on the target, not to establish that it is the bundle you were
// sent.
package doctor

import (
	"context"
	"errors"
	"io/fs"
	"sort"
	"time"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
)

// SchemaVersion is the doctor report schema, emitted by doctor --json.
const SchemaVersion = "debark.doctorreport/v1"

// Input is what doctor examines. A bundle, a snapshot, or both.
type Input struct {
	// BundleDir is a bundle root; its pool is scanned.
	BundleDir string
	// Lock is the bundle's lock when already parsed.
	Lock *lock.Lock
	// Snapshot is the target snapshot when available; it lets doctor check
	// DKMS packages against the installed kernel headers.
	Snapshot *snapshot.Snapshot
	// ScanScripts controls whether maintainer scripts are extracted and
	// scanned. Default true; it is the slow part.
	//
	// Note: as a plain bool, Run itself cannot distinguish "the caller wants
	// the default" from "the caller explicitly asked for false" — Go's zero
	// value for bool is false, not true. The documented default of true is
	// therefore the constructing caller's responsibility (the CLI sets it
	// unless the operator passes a flag such as --no-scan-scripts); Run takes
	// the field at face value. Flagged in that work report as a frozen-
	// contract wording that a plain bool cannot fully honour.
	ScanScripts bool
	// DpkgStatus is the target's captured /var/lib/dpkg/status content, when
	// available. It is not reachable from Snapshot alone: snapshot.Snapshot
	// (frozen) records only file metadata (path, digest, size) for
	// DpkgStatus, never the bytes, and reading them back requires an opened
	// snapshot.Archive that Input has no field for. This field is an
	// additive extension of the frozen Input struct (new field, no existing
	// field changed or removed) added so held-package and dkms-headers can
	// function at all; see that work report for the full reasoning. A
	// caller with no snapshot archive open (or one that used --no-keyrings /
	// omitted DpkgStatus) leaves this nil, and both checks degrade to a
	// softer finding or no finding rather than an error.
	DpkgStatus []byte
}

// Report is doctor's output.
type Report struct {
	SchemaVersion string    `json:"schema_version"`
	CheckedAt     string    `json:"checked_at"`
	Findings      []Finding `json:"findings"`
	// Scanned counts the packages actually examined.
	Scanned int `json:"scanned"`
	// Summary counts findings by severity.
	Summary map[string]int `json:"summary,omitempty"`
}

// Severity of a doctor finding. doctor never fails a build by itself; policy
// does that.
type Severity string

const (
	SeverityNote Severity = "note"
	SeverityWarn Severity = "warn"
)

// Check identifiers. Stable: scripts and policy files reference them.
const (
	CheckNetworkPostinst = "network-postinst"
	CheckSnapShim        = "snap-shim"
	CheckDKMSHeaders     = "dkms-headers"
	CheckRedistribution  = "redistribution"
	CheckUnverifiedURL   = "unverified-url"
	CheckHeldPackage     = "held-package"
	CheckEssentialChange = "essential-change"
)

// Finding is one observation.
type Finding struct {
	Check    string   `json:"check"`
	Severity Severity `json:"severity"`
	Package  string   `json:"package,omitempty"`
	Version  string   `json:"version,omitempty"`
	Message  string   `json:"message"`
	// Evidence is the concrete thing that triggered it: the matched line of a
	// maintainer script, the component name, the missing header package.
	Evidence string `json:"evidence,omitempty"`
	// Flag is the lock flag this finding corresponds to, when there is one.
	Flag string `json:"flag,omitempty"`
}

// Run examines the input and returns findings. Every check is a heuristic:
// silence from network-postinst means NoNetworkActionFound, never that a
// script was proven safe.
//
// Run needs Input.Lock: doctor's checks all examine a resolved plan, so a
// bare-snapshot invocation (no build has happened yet) returns an empty,
// non-nil Report rather than an error — there is nothing to check yet, which
// is not a failure.
func Run(ctx context.Context, in Input) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "doctor: run")
	}

	report := &Report{
		SchemaVersion: SchemaVersion,
		CheckedAt:     canonical.Time(time.Now()),
	}

	if in.Lock == nil {
		report.Summary = map[string]int{}
		return report, nil
	}
	report.Scanned = len(in.Lock.Packages)

	installed, held := parseDpkgStatus(in.DpkgStatus)
	kernelSuffixes := kernelHeaderSuffixes(installed)

	var findings []Finding
	for _, pkg := range in.Lock.Packages {
		if f := checkRedistribution(pkg); f != nil {
			findings = append(findings, *f)
		}
		if f := checkUnverifiedURL(pkg); f != nil {
			findings = append(findings, *f)
		}
		if f := checkHeldPackage(pkg, held); f != nil {
			findings = append(findings, *f)
		}

		var deb *debFile
		var unscanned string
		if in.ScanScripts && in.BundleDir != "" && pkg.Filename != "" {
			// poolPath, not a bare filepath.Join: Filename comes out of the
			// bundle's own lock.json, which nothing has verified at this
			// point, and Join happily resolves "../.." right out of the
			// bundle. See poolPath for the measured escape.
			debPath, perr := poolPath(in.BundleDir, pkg.Filename)
			switch {
			case perr != nil:
				unscanned = perr.Error()
			default:
				d, err := openDebFile(debPath)
				switch {
				case errors.Is(err, fs.ErrNotExist):
					// A .deb that is simply not there (mid-build, or a lock
					// naming a file this bundle does not carry) stays silent:
					// doctor is diagnostic, not a bundle integrity checker,
					// and core/verify is what fails a build over a missing
					// file. A .deb that IS there but cannot be read is a
					// different thing entirely — see below.
				case err != nil:
					unscanned = err.Error()
				default:
					deb = d
					if d.Unscannable != "" {
						unscanned = "control member compression " + d.Unscannable
					}
				}
			}
		}
		findings = append(findings, checkNetworkPostinst(pkg, deb, unscanned)...)
		if f := checkSnapShim(pkg, deb); f != nil {
			findings = append(findings, *f)
		}
		if f := checkDKMSHeaders(pkg, deb, installed, kernelSuffixes); f != nil {
			findings = append(findings, *f)
		}

		if err := ctx.Err(); err != nil {
			return nil, dferr.Wrap(dferr.Usage, err, "doctor: run")
		}
	}

	// Sanitise before sorting, so the report is ordered by what it actually
	// says. See display.go for the measured terminal-control injection this
	// closes.
	for i := range findings {
		sanitiseFinding(&findings[i])
	}
	sortFindings(findings)
	report.Findings = findings
	report.Summary = summariseFindings(findings)
	return report, nil
}

// sortFindings orders findings deterministically: by check, then package,
// then version, then evidence. Two runs over the same input produce the same
// Report.Findings order.
func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Check != b.Check {
			return a.Check < b.Check
		}
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Evidence < b.Evidence
	})
}

func summariseFindings(findings []Finding) map[string]int {
	out := map[string]int{}
	for _, f := range findings {
		out[string(f.Severity)]++
	}
	return out
}

// FlagsFor returns the lock flags a package earns from doctor's checks, so the
// build can record them without running a full report. The result is sorted
// and de-duplicated.
func FlagsFor(findings []Finding, pkg string) []string {
	// Run rewrites Finding.Package through displaySafe, so the name compared
	// against has to make the same trip or a package whose lock name carries
	// a control character would silently earn no flags. For every name a real
	// lock can carry, displaySafe is the identity.
	pkg = displaySafe(pkg, maxNameLen)
	seen := map[string]bool{}
	var out []string
	for _, f := range findings {
		if f.Package != pkg || f.Flag == "" || seen[f.Flag] {
			continue
		}
		seen[f.Flag] = true
		out = append(out, f.Flag)
	}
	sort.Strings(out)
	return out
}
