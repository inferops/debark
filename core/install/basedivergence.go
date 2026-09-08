package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/inferops/debark/core/dferr"
)

// Base divergence: checking, on the target, the one claim a synthesized
// snapshot makes that nothing upstream of here is able to test.
//
// A captured snapshot is a MEASUREMENT of one real machine, so a bundle built
// from it is proven complete for that machine. A synthesized snapshot
// (ADR-014, core/base) is an ASSUMPTION about a machine that may not exist
// yet: "a stock ubuntu:26.04/desktop already has these 1,847 packages". The
// online solve then fetches only what such a machine would still be missing.
// The two errors that assumption can make are not symmetric, which is the
// whole reason origin.assumed_installed travels inside the bundle at all
// (core/snapshot/types.go states it at length):
//
//   - the real machine has MORE than the base assumed — the bundle is merely
//     larger than it needed to be, which costs media and nothing else;
//   - the real machine has FEWER — the bundle is SHORT, and a short bundle
//     fails at the far side of the air gap, which is the exact failure
//     debark exists to prevent.
//
// install runs on the target, with the target's real dpkg in front of it, so
// it is the one place in debark where the assumption can finally be checked
// against reality. Nothing on the build side can do this: the builder never
// sees the machine, and verify checks that the bundle is the one that was
// signed, not that the machine is the one it was assumed to be.
//
// It WARNS; it never refuses. Three reasons, all of them the project's
// existing rules rather than a judgement made here. Doctor-style findings
// warn and policy fails (docs/formats.md §3.10: "doctor never fails a build
// by itself; policy does"), and this is a finding. A missing assumed package
// is evidence that the bundle MIGHT be short, not proof that it is — apt is
// the oracle, and apt answers that exact question a few lines further down,
// from the same run, with the real dependency graph in hand; refusing here
// would substitute this file's guess for apt's answer. And the operator
// holding the media at the far side of an air gap is the last person who
// should be told "no" by something that cannot say what is actually wrong.

// --- reading snapshot.json without importing core/snapshot -----------------

// bundleSnapshotDoc is the only part of debark.snapshot/v1 this package
// reads, declared locally instead of by importing core/snapshot.
//
// SOURCE OF TRUTH: core/snapshot/types.go — Snapshot.Origin and the Origin
// type, whose doc comments carry the rationale for every field below. The
// names, the JSON tags and the "synthesized" constant are copied from there
// and must not be changed here without being changed there first.
//
// WHY a copy rather than the real type. core/install is the only part of
// debark that runs on the air-gapped machine, and it must stay small enough
// to audit and to package in a distribution — the long note on
// resolveBundleDir in bundle.go is the same boundary, stated for the same
// reason, and it is why this package does not reach for core/bundle's
// loadSnapshotJSON, which already reads exactly this file. core/snapshot is
// the capture, validation, redaction and archive-reading surface for a
// document install needs three fields of; importing it would put all of that
// in the target-side binary's dependency graph, and would make a build break
// anywhere in it a build break here. This is the same trade loadLock did NOT
// have to make: core/lock is a document type and its own validator, small
// enough to depend on outright.
//
// The copy is a duplicate, and duplicates drift. snapshotdoc_guard_test.go is
// the tripwire: it imports core/snapshot, marshals a real snapshot.Snapshot
// carrying a synthesized Origin, and asserts this type decodes it to the same
// values. That import is TEST-ONLY and deliberately so — a _test.go import
// does not appear in the shipped binary's dependency graph, which is exactly
// the property the boundary above protects, so the guard costs the boundary
// nothing. Do not "fix" it by deleting it, and do not "fix" this type by
// replacing it with snapshot.Snapshot.
type bundleSnapshotDoc struct {
	// Origin is the provenance of the snapshot this bundle was built from.
	// Everything else in the document (target identity, apt configuration,
	// keyring fingerprints) is either checked elsewhere or is no business of
	// this check, so it is deliberately not declared: encoding/json ignores
	// unknown members, and a field this package does not read is a field
	// that cannot drift.
	Origin bundleOrigin `json:"origin"`
}

// bundleOrigin mirrors snapshot.Origin. Source of truth:
// core/snapshot/types.go.
type bundleOrigin struct {
	// Kind is "captured" or "synthesized" (snapshot.OriginCaptured /
	// snapshot.OriginSynthesized). Required in the document, and deliberately
	// not defaulted there: absent-means-captured would make the dangerous one
	// the silent default. This package inherits that reading — anything that
	// is not exactly "synthesized" produces no report.
	Kind string `json:"kind"`
	// BaseID identifies the base definition a synthesized snapshot came from,
	// e.g. "ubuntu:26.04/desktop". Empty when Kind is "captured".
	BaseID string `json:"base_id,omitempty"`
	// AssumedInstalled is every package the base claims a stock install
	// already has, as name:arch, sorted. Empty when Kind is "captured": a
	// real machine's installed-package list is the sensitive inventory D8
	// keeps out of artefacts, and core/snapshot's Validate refuses a captured
	// snapshot that carries anything in this field.
	AssumedInstalled []string `json:"assumed_installed,omitempty"`
}

// originSynthesized mirrors snapshot.OriginSynthesized. Compared as a string
// here only because the constant itself is what may not be imported; the
// guard test asserts the two are equal.
const originSynthesized = "synthesized"

// snapshotDocName mirrors snapshot.DocumentName: the bundle member this
// reads, at the bundle root. docs/formats.md §2 lists it as a member of every
// bundle, alongside lock.json and the manifest, and not as an optional one.
const snapshotDocName = "snapshot.json"

// readBundleSnapshotDoc reads and decodes the bundle's snapshot.json.
//
// It only ever runs after verification has passed, so the bytes are already
// proven to be the ones the signed manifest names — this is not a trust
// boundary, it is a decode. What can still go wrong is a bundle assembled
// without a snapshot at all (core/bundle writes the member only when one was
// supplied) or a document this reduced type cannot decode. Neither is fatal;
// see noteBaseDivergence for that decision and its reasoning.
func readBundleSnapshotDoc(bundleDir string) (*bundleSnapshotDoc, error) {
	raw, err := os.ReadFile(filepath.Join(bundleDir, snapshotDocName))
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "install: read %s", snapshotDocName)
	}
	var doc bundleSnapshotDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "install: parse %s", snapshotDocName)
	}
	return &doc, nil
}

// --- what this machine actually has ----------------------------------------

// dpkgInstalledQueryArgs is the argv for the one extra query this file adds
// to the two dpkg invocations install already makes
// (--print-architecture and --print-foreign-architectures, runner.go).
//
// It names dpkg-query rather than going through Deps.DpkgPath, and that is
// measured, not stylistic. On Ubuntu 24.04 (dpkg 1.22.6), 2026-09-06:
//
//	$ dpkg -W --showformat='${Package}:${Architecture} ${Status}\n'
//	dpkg: error: unknown option -W
//
// dpkg forwards -l/-s/-S/-L/-p to dpkg-query and nothing else, so the query
// has to name the real binary. Hence Deps.DpkgQueryPath, which follows
// AptPath/DpkgPath exactly and exists for the same reason they do.
//
// ${Package}:${Architecture}, never ${binary:Package}. The latter is dpkg's
// DISPLAY spelling: it abbreviates a native-architecture package to its bare
// name and qualifies only foreign ones, so "libfoo" and "libfoo:i386" would
// come back from one machine and the comparison below would have to guess
// which arch the unqualified half meant. That guess is the arch-blindness
// this project has already been bitten by twice. origin.assumed_installed is
// always name:arch (core/base's originWithClosure, which takes the arch from
// apt's own bracketed answer, so an Architecture: all package is "all" on
// both sides), and the two spellings have to be the same one for every
// package or "libfoo:i386 was assumed" is satisfied by "libfoo:amd64 is
// installed".
//
// ${Status} arrives as a single field with three words in it and is split by
// dpkgStatusInstalled below; --showformat's \n is dpkg-query's own escape,
// which is why it is a literal backslash-n in the Go string and not a newline.
func dpkgInstalledQueryArgs() []string {
	return []string{"-W", `--showformat=${Package}:${Architecture} ${Status}\n`}
}

// parseDpkgInstalledSet turns dpkgInstalledQueryArgs' output into the set of
// name:arch entries this machine actually has, one line per package dpkg
// knows about:
//
//	bash:amd64 install ok installed
//	zlib1g:i386 hold ok installed
//	nano:amd64 deinstall ok config-files
//
// A line that does not have the expected four fields is skipped rather than
// guessed at: dpkg has no reason to print one, and inventing a meaning for it
// could only ever move a package into or out of the "present" set on no
// evidence.
func parseDpkgInstalledSet(out []byte) map[string]bool {
	installed := map[string]bool{}
	text := strings.ReplaceAll(string(out), "\r\n", "\n")
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		if dpkgStatusInstalled(fields[1], fields[2], fields[3]) {
			installed[fields[0]] = true
		}
	}
	return installed
}

// dpkgStatusInstalled reports whether dpkg's ${Status} means "this package is
// on the machine".
//
// ${Status} is THREE fields — desired action, error flag, current status —
// and only the last two answer that question. This project has already paid
// for treating it as a string: both of the e2e harness's package assertions
// matched the literal "install ok installed", which folded the DESIRED field
// into the test, and a package the operator has HELD reports "hold ok
// installed" — installed, configured, usable, and differing only because
// someone asked apt not to change it.
//
// The negation is the dangerous direction, and here it is the only direction:
// this predicate's false answers are what become "missing", so a naive
// strings.Contains(status, "install ok installed") would report every held
// package on the machine as absent from it and turn an ordinary, correctly
// pinned target into a report claiming the bundle is short. That is worse
// than no report at all, because it is a wrong one about the exact thing this
// file exists to get right.
//
// Deliberately narrow in the other direction too. "deinstall ok installed"
// and "purge ok installed" describe a machine mid-transaction and are NOT
// accepted; neither is anything whose error flag is not "ok" (reinstreq) or
// whose state is not "installed" (unpacked, half-configured, config-files).
// Erring toward "not installed" is the same rule core/base states for a base
// definition: over-reporting divergence produces a noisy warning, while
// under-reporting it produces silence about a bundle that may be short.
func dpkgStatusInstalled(desired, errFlag, state string) bool {
	return (desired == "install" || desired == "hold") && errFlag == "ok" && state == "installed"
}

// --- the comparison --------------------------------------------------------

// compareAssumedInstalled is the whole finding: which of the packages the
// base assumed a stock install already has are not on this machine.
//
// Only that direction is computed. A machine with MORE than the base assumed
// is not divergence worth reporting — the bundle is bigger than it needed to
// be and installs perfectly — and listing those extras would bury the one
// direction that can fail under a list that is, on any real desktop, far
// longer.
//
// Versions are not compared, because origin.assumed_installed does not carry
// them, and that omission is deliberate on the producing side too
// (core/base's originWithClosure): a machine running a different version of
// an assumed package still HAS it, so the bundle is not short on its account,
// and reporting ordinary patching as divergence would bury the real finding.
//
// Duplicate entries are counted once. The document is input; a base that
// somehow listed a package twice must not make both the total and the
// missing count larger than the set they describe.
//
// unusable counts entries that do not look like a package name at all - see
// printableEntry. They are excluded from both numbers rather than reported as
// missing, and the caller says how many there were without quoting any of
// them.
func compareAssumedInstalled(origin bundleOrigin, installed map[string]bool) (d *BaseDivergence, unusable int) {
	d = &BaseDivergence{BaseID: origin.BaseID}
	seen := make(map[string]bool, len(origin.AssumedInstalled))
	for _, entry := range origin.AssumedInstalled {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !printableEntry(entry) {
			unusable++
			continue
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		d.Assumed++
		if !installed[entry] {
			d.Missing = append(d.Missing, entry)
		}
	}
	// Sorted here rather than trusted from the document: origin.assumed_
	// installed is documented as sorted, but a report an operator reads and a
	// --json consumer diffs must be ordered because THIS code ordered it.
	sort.Strings(d.Missing)
	return d, unusable
}

// maxEntryLen bounds one assumed-installed entry. The longest real Debian
// package name is well under half this; core/snapshot applies the same 256
// (checkAssumedInstalled), which is where the number comes from.
const maxEntryLen = 256

// printableEntry reports whether an assumed-installed entry is safe to put in
// front of a human: bounded, and printable ASCII throughout.
//
// core/snapshot's Validate already refuses a document whose
// origin.assumed_installed carries control characters, and it refuses it for
// exactly this reason - install prints a sample of these names, and a name
// carrying ESC[1A ESC[2K can scroll back over the digest line a reviewer was
// told to compare by hand (docs/threat-model.md §6). This function does
// not take that on trust, on the same principle missingPoolFiles states one
// file over: a gate that is only correct because another package sanitised
// its input has no property of its own. Validate ran on the machine that
// BUILT the bundle; install runs on the target, reading a document that
// arrived across an air gap, and the signature proves only that the bytes are
// the ones that were signed - not that whoever signed them meant this package
// well.
//
// Every legitimate entry is ASCII graphic: dpkg package names are drawn from
// [a-z0-9+-.] and architectures from [a-z0-9-]. Anything else is not a
// package this comparison could ever match, so rejecting it costs nothing
// real and is not a guess about intent.
func printableEntry(s string) bool {
	if len(s) > maxEntryLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// maxNamedMissing bounds how many missing packages the human sentence names.
// An operator on a serial console does not want four hundred names, and
// Report.Warnings is also joined into the durable local evidence log. Ten is
// enough to recognise a pattern ("all of them are fonts-*") and short enough
// to read at a glance; BaseDivergence.Missing carries the complete list, and
// --json is where anyone who wants all four hundred goes.
const maxNamedMissing = 10

// noteBaseDivergence performs the check and records it on the report. It
// never returns an error: every failure below degrades to a warning, for the
// reasons given at each one.
func (r *runner) noteBaseDivergence(ctx context.Context, bundleDir string, report *Report, record func([]string, []byte)) {
	doc, err := readBundleSnapshotDoc(bundleDir)
	if err != nil {
		// A bundle whose snapshot.json is missing or unparseable must not
		// fail the install, and this is a deliberate choice rather than
		// leniency for its own sake. Verification has already proved these
		// bytes are the ones the signed manifest names, so nothing here is a
		// trust question; this is a REPORTING feature, and refusing to
		// install a cryptographically sound bundle because a report about it
		// could not be produced would be debark choosing its own paperwork
		// over the operator's outage.
		//
		// It does warn, including when the file is simply absent, because
		// silence would be ambiguous in precisely the wrong direction: an
		// operator who sees nothing cannot tell "this bundle was captured
		// from your machine, there is nothing to say" from "nobody checked".
		// Saying which one it is costs one line.
		report.Warnings = append(report.Warnings,
			"could not tell whether this bundle was built against an assumed base, so no divergence report was produced: "+err.Error())
		return
	}
	if doc.Origin.Kind != originSynthesized {
		// A captured snapshot is a measurement of this machine; there is no
		// assumption to check, so there is no warning, no report field and
		// no dpkg-query call. The cost of this feature on the overwhelmingly
		// common path is one file read.
		return
	}

	// Emitted before the query, so an operator still learns the bundle rests
	// on an assumption even if the query below cannot run.
	if doc.Origin.BaseID != "" {
		report.Warnings = append(report.Warnings,
			"this bundle was built against an assumed base ("+doc.Origin.BaseID+"), not a snapshot of this machine")
	} else {
		report.Warnings = append(report.Warnings,
			"this bundle was built against an assumed base, not a snapshot of this machine")
	}

	out, qerr := runProcess(ctx, r.deps.DpkgQueryPath, dpkgInstalledQueryArgs(), buildEnv(false))
	// Recorded like every other apt-get/dpkg invocation install makes, even
	// though it is by far the largest: Deps.OnAptOutput promises the caller
	// one callback per invocation, and an invocation quietly left out of that
	// stream is one an operator debugging a wrong divergence report cannot
	// see. It is only ever made for a synthesized bundle.
	record(out.Argv, out.Combined)
	if qerr != nil {
		// Same degradation, same reason: this is a report, not a gate. A
		// dpkg-query that cannot answer is also not evidence about the
		// bundle, so it must not change the bundle's fate.
		report.Warnings = append(report.Warnings,
			"could not list this machine's installed packages, so the assumed base was not checked against it: "+qerr.Error())
		return
	}

	div, unusable := compareAssumedInstalled(doc.Origin, parseDpkgInstalledSet(out.Combined))
	report.BaseDivergence = div
	report.Warnings = append(report.Warnings, divergenceSentence(div))
	if unusable > 0 {
		// Counted, never quoted: the whole reason these entries were dropped
		// is that they are not safe to put in front of a terminal, so echoing
		// one back in the warning about them would be self-defeating.
		report.Warnings = append(report.Warnings,
			groupThousands(unusable)+" entries in this bundle's assumed base package list are not package names and were ignored"+
				" - the divergence figures above do not count them")
	}
}

// divergenceSentence renders the finding as the one line an operator reads.
//
// The zero case gets a sentence of its own rather than saying nothing, and
// that is the deliberate choice the wording turns on. "Nothing was missing"
// and "the check did not run" are different facts with the same silence, and
// the count is also the only place the SIZE of the assumption appears: "all
// 1,847 packages the base assumes are present" tells the operator both that
// the check ran and how much of their machine it just accounted for. It is
// phrased as a statement of fact and not as reassurance, because it is not
// one: origin.assumed_installed carries no versions, so a clean result says
// every assumed package is here, not that every assumed package is the
// version the solve resolved against.
func divergenceSentence(d *BaseDivergence) string {
	total := groupThousands(d.Assumed)
	if d.Assumed == 0 {
		// core/base refuses to produce a closure with nothing in it ("a base
		// whose closure is empty would claim a stock install contains no
		// packages at all"), so a synthesized snapshot that reaches here with
		// an empty assumed set is a document debark did not write, or one
		// every entry of which was discarded as unusable. Either way the
		// honest report is that nothing was compared. "all 0 packages the
		// base assumes are present" would be literally true and would read
		// as a clean bill of health for a check that had nothing to check.
		return "this bundle's assumed base lists no packages, so there was nothing to compare against this machine"
	}
	if len(d.Missing) == 0 {
		return "all " + total + " packages the base assumes are present on this machine - nothing it assumed is missing"
	}

	named := d.Missing
	suffix := ""
	if len(named) > maxNamedMissing {
		named = named[:maxNamedMissing]
		suffix = " and " + groupThousands(len(d.Missing)-maxNamedMissing) + " more"
	}
	return groupThousands(len(d.Missing)) + " of the " + total +
		" assumed base packages are not present on this machine: " +
		strings.Join(named, ", ") + suffix +
		" - the bundle was resolved as if this machine already had them, so it may be short"
}

// groupThousands renders n with thousands separators ("1847" -> "1,847").
// A base closure is routinely four figures, and an unseparated 1847 next to a
// 12 is exactly the pair a tired operator misreads.
func groupThousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
