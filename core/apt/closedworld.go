package apt

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
)

// closedWorldRun is one apt-get invocation captured during a ClosedWorld
// check, kept so its argv and output both feed the final digests regardless
// of whether the check ultimately passed or failed.
type closedWorldRun struct {
	argv []string
	out  Output
}

// closedWorldPathMask is one host path that cannot be the same twice — or
// cannot be the same on two machines — paired with the fixed placeholder that
// stands in for it in everything ClosedWorld records. It is deliberately
// scoped to this file: local.go is growing its own masking helper for the
// resolve path, and the two have different inputs (a private root and an
// archives dir there, a work dir and a bundle repo here). Unifying them is a
// worthwhile follow-up, not a prerequisite.
type closedWorldPathMask struct{ path, placeholder string }

// closedWorldMasks is the complete set of run-varying paths this check hands
// to apt, in the order they must be replaced.
//
// Why this exists. Every value ClosedWorld returns is a lock.ClosedWorld
// field, so every one of them reaches lock.json -> LockDigest ->
// manifest.BundleID -> README.txt. Two of those values are built directly
// out of paths that are different on every single run:
//
//   - CommandDigest is canonical.Digest over the apt argv, and the argv
//     carries 18 "-o Dir::*=<path>" options, all of them derived from
//     RootSpec.Dir — which is in.WorkDir, a subdirectory of the engine's
//     mkdirTemp("", "debark-build-*") work root (core/engine/engine.go's
//     run, core/engine/finalize.go's runClosedWorld). Measured: two work
//     roots, same everything else, both "Result: ok", CommandDigest
//     0e8ebdf6… vs 1c5b713a….
//   - OutputDigest is the SHA-256 of apt's own captured output, and
//     `apt-get update` echoes the source URI it fetched from — here a file:
//     URI naming the finished bundle's repo/ directory, i.e. Output.Path.
//     Measured the same way: 985157f8… vs 606cccff… for two output
//     directories.
//
// This is the same defect, on the same kind of value, that
// sanitizeAPTOptionsForLock (local.go) already fixes for
// Resolver.APTOptions — that function was simply never applied on this path.
// The masking rule is deliberately identical to that one's: replace only the
// varying PREFIX, never the whole entry, so the Dir::* keys, the argv shape
// and apt's own sentence structure all survive intact and an auditor can
// still see exactly which paths within the private root apt was pointed at.
// What is left after masking is a genuine function of the request: change
// the install set, the architecture, the recommends policy or the source
// layout and both digests still move.
//
// WorkDir is listed before BundleRepoDir because the private root sits
// underneath it; masking the longer, more specific path first would leave
// the shorter one behind. SnapshotFilesDir is the snapshot archive's
// extraction directory (also a per-run temporary): nothing puts it in the
// argv today, but BuildPrivateRoot reads the target's dpkg status out of it
// and would name it in an error, and Detail carries error text.
func closedWorldMasks(in ClosedWorldInput, bundleRepoAbs string) []closedWorldPathMask {
	return []closedWorldPathMask{
		{in.WorkDir, "<CLOSED-WORLD-WORKDIR>"},
		{bundleRepoAbs, "<BUNDLE-REPO>"},
		{in.BundleRepoDir, "<BUNDLE-REPO>"},
		{in.SnapshotFilesDir, "<SNAPSHOT-FILES>"},
	}
}

// maskClosedWorldPaths replaces every masked path in s with its placeholder,
// in each of the FOUR spellings one path can appear in. Missing any of them
// leaves the build's own output directory in a value that becomes the bundle
// id, so this list is the whole of the fix and each entry earns its place:
//
//  1. the native form, as the -o Dir::* options carry it (host separator);
//  2. the forward-slashed form, which apt's own messages use;
//  3. the file: URI FileURI() generates for the ExtraSource line, which apt
//     echoes back in "Get:1 file:///work/... ./ InRelease". This is
//     PERCENT-ENCODED by net/url, so a path containing a space appears as
//     "/work/has%20space/repo" and matches neither 1 nor 2;
//  4. the name apt gives that URI's cached index under Dir::State::lists,
//     which it quotes in ordinary success-path warnings.
//
// Spelling 4 is why this function exists in its present form, and it is also
// where the first attempt at it was WRONG in a way its own test could not
// see. That attempt modelled the encoding as "replace every / with _". apt
// does more: libapt-pkg's URItoFileName percent-encodes a set of characters
// that includes '_' itself, so "/work/my_bundle/repo" is cached as
// "_work_my%5fbundle_repo_._Release", not "_work_my_bundle_repo_._Release".
// A build into an --out path containing an underscore, a space, '~', '=' or
// any of the rest therefore still leaked its directory into OutputDigest.
//
// Two things hid it. The test generated its own fixture with the same
// "replace / with _" rule it was checking, so the implementation was its own
// oracle; and the harness happens to compare "/work/bundle" against
// "/work/bundle-determinism-2", two paths with no character apt encodes, so
// the matrix went green. Had checkDeterminism used the natural
// "/work/bundle_2", the row would have stayed red.
//
// The encoding is therefore NOT reimplemented here. aptURItoFileName
// (sources.go) is this package's existing transcription of apt's own
// mangling, derived from real captured list-file names and asserted against
// them in hardening_test.go. One definition, already tested; a second would
// only drift from it, which is exactly what happened.
func maskClosedWorldPaths(s string, masks []closedWorldPathMask) string {
	for _, m := range masks {
		if m.path == "" {
			continue
		}
		slashed := filepath.ToSlash(m.path)
		// Most-encoded spellings first: each of these can contain characters
		// the plainer forms do not, so replacing a plainer form first would
		// leave them untouched rather than the other way round.
		uri := FileURI(m.path)
		s = strings.ReplaceAll(s, aptURItoFileName(uri), m.placeholder)
		s = strings.ReplaceAll(s, uri, m.placeholder)
		if slashed != m.path {
			s = strings.ReplaceAll(s, slashed, m.placeholder)
		}
		s = strings.ReplaceAll(s, m.path, m.placeholder)
	}
	return s
}

// ClosedWorld re-resolves the finished bundle against itself, offline
// (ADR-007): a fresh private root whose only source is the bundle's
// own repo/ directory, asked to simulate installing exactly the lock's
// install set — and, when the plan had upgrades, an exact-version simulated
// install of the lock's own upgrade set too, verified against the versions
// the lock recorded for it (never a bare, unpinned "-s full-upgrade": E3,
// docs/experiments/E3-pin-fidelity.md, found that a free full-upgrade
// against the bundle can silently select a different version than the
// online solve locked in and report it as ordinary success — exactly the
// class of divergence this check exists to catch). Because the only
// configured source is a file: URI, the check is network-free by
// construction; ClosedWorld additionally scans the captured output for a
// stray network URI as a defence against a future change accidentally
// widening the sources it builds.
func (b *localBackend) ClosedWorld(ctx context.Context, in ClosedWorldInput) (lock.ClosedWorld, error) {
	if in.BundleRepoDir == "" {
		return lock.ClosedWorld{Result: lock.ClosedWorldSkipped, Detail: "no bundle repository directory given"}, nil
	}
	if len(in.Install) == 0 {
		return lock.ClosedWorld{Result: lock.ClosedWorldSkipped, Detail: "install set is empty"}, nil
	}

	abs, err := filepath.Abs(in.BundleRepoDir)
	if err != nil {
		return lock.ClosedWorld{}, dferr.Wrap(dferr.Usage, err, "apt: closed-world: bundle repo dir")
	}

	arch, foreign := "", []string(nil)
	if in.Snapshot != nil {
		arch = in.Snapshot.Target.Arch
		foreign = in.Snapshot.Target.ForeignArchs
	}
	if arch == "" {
		return lock.ClosedWorld{}, dferr.New(dferr.Usage, "apt: closed-world: no target architecture")
	}

	// Fixed before the first apt-get call, so that every return below —
	// success, failure and the "check could not run" cases alike — records
	// digests and Detail text over the same masked view. See
	// closedWorldMasks for why this is not optional.
	masks := closedWorldMasks(in, abs)

	// An empty WorkDir would make this a RELATIVE path, so the private apt
	// root - sources.list, preferences, a dpkg status file - would be written
	// into whatever directory the process happens to be running from. That is
	// how a stray closedworld-aptroot/ tree appeared inside core/apt during a
	// test run. Refuse instead of guessing: every caller in the engine has a
	// work root, and one that does not has not decided where its scratch
	// space lives.
	if in.WorkDir == "" {
		return lock.ClosedWorld{}, dferr.New(dferr.Usage,
			"apt: closed-world check: WorkDir is empty; the private apt root has nowhere to live")
	}
	rootDir := filepath.Join(in.WorkDir, "closedworld-aptroot")
	spec := RootSpec{
		Dir:              rootDir,
		Arch:             arch,
		ForeignArchs:     foreign,
		Recommends:       true, // exact-version pins do not depend on this.
		PhasedPolicy:     snapshot.PhasedNeverInclude,
		Snapshot:         in.Snapshot,
		SnapshotFilesDir: in.SnapshotFilesDir,
		ExtraSources: []ExtraSource{{
			Name: "debark-bundle",
			Line: "deb [trusted=yes] " + FileURI(abs) + " ./",
		}},
	}
	root, err := BuildPrivateRoot(spec)
	if err != nil {
		return lock.ClosedWorld{}, err
	}

	var runs []closedWorldRun

	updOut, uerr := b.runnerFor(root).Run(ctx, root.Options, "update")
	runs = append(runs, closedWorldRun{argv: updOut.Argv, out: updOut})
	upd := ParseUpdate(updOut)
	if upd.Failed {
		msg := upd.Error()
		if msg == "" && uerr != nil {
			msg = uerr.Error()
		}
		return closedWorldResult(runs, masks, lock.ClosedWorldFailed, "update against the bundle repository failed: "+msg), nil
	} else if uerr != nil {
		return lock.ClosedWorld{}, uerr
	}

	installArgs := append([]string{"-s", "install"}, in.Install...)
	instOut, ierr := b.runnerFor(root).Run(ctx, root.Options, installArgs...)
	runs = append(runs, closedWorldRun{argv: instOut.Argv, out: instOut})
	if ierr != nil && dferr.ClassOf(ierr) != dferr.Resolution {
		return lock.ClosedWorld{}, ierr
	}
	sim := ParseSimulate(instOut)
	if sim.Failed {
		return closedWorldResult(runs, masks, lock.ClosedWorldFailed, "install simulation against the bundle repository failed: "+simulateFailureMessage(sim)), nil
	}
	// An action line this package cannot read is not a line it may ignore.
	// upgradeMismatches below iterates sim.Actions only, so a dropped Inst
	// line means that package is never compared against the lock at all - and
	// the check would then report ok for a package it never actually checked,
	// which is worse than reporting nothing. apt is the oracle; output whose
	// meaning we do not know cannot certify a closed world.
	if len(sim.UnparsedActions) > 0 {
		return closedWorldResult(runs, masks, lock.ClosedWorldFailed,
			"install simulation produced action lines this build could not read, so the closed-world check could not be completed: "+
				strings.Join(sim.UnparsedActions, "; ")), nil
	}

	if in.Upgrades {
		upgradeEntries, wantVersions, uerr3 := closedWorldUpgradeTargets(abs)
		if uerr3 != nil {
			return lock.ClosedWorld{}, uerr3
		}
		if len(upgradeEntries) > 0 {
			upgArgs := append([]string{"-s", "install"}, upgradeEntries...)
			upgOut, uerr2 := b.runnerFor(root).Run(ctx, root.Options, upgArgs...)
			runs = append(runs, closedWorldRun{argv: upgOut.Argv, out: upgOut})
			if uerr2 != nil && dferr.ClassOf(uerr2) != dferr.Resolution {
				return lock.ClosedWorld{}, uerr2
			}
			upgSim := ParseSimulate(upgOut)
			if upgSim.Failed {
				return closedWorldResult(runs, masks, lock.ClosedWorldFailed, "upgrade-set install simulation against the bundle repository failed: "+simulateFailureMessage(upgSim)), nil
			}
			// Same rule as the install pass above, and it bites harder here:
			// a dropped line is a package upgradeMismatches never sees, so the
			// E3 pin-fidelity divergence this check exists to catch would pass
			// silently.
			if len(upgSim.UnparsedActions) > 0 {
				return closedWorldResult(runs, masks, lock.ClosedWorldFailed,
					"upgrade-set simulation produced action lines this build could not read, so the closed-world check could not be completed: "+
						strings.Join(upgSim.UnparsedActions, "; ")), nil
			}
			if mismatches := upgradeMismatches(wantVersions, upgSim); len(mismatches) > 0 {
				return closedWorldResult(runs, masks, lock.ClosedWorldFailed,
					"closed-world upgrade check diverged from the lock's upgrade set (E3, docs/experiments/E3-pin-fidelity.md): "+strings.Join(mismatches, "; ")), nil
			}
		}
	}

	combined := combinedOutput(runs)
	lower := strings.ToLower(combined)
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") || strings.Contains(lower, "ftp://") {
		return closedWorldResult(runs, masks, lock.ClosedWorldFailed, "closed-world check referenced a network URI unexpectedly"), nil
	}

	return closedWorldResult(runs, masks, lock.ClosedWorldOK, ""), nil
}

func combinedOutput(runs []closedWorldRun) string {
	var b strings.Builder
	for _, r := range runs {
		b.Write(r.out.Combined())
	}
	return b.String()
}

// closedWorldResult builds the recorded result. Every string it records —
// each run's argv, the combined output, and the caller's one-line detail —
// goes through masks first, because all three are lock.json fields and all
// three otherwise carry a path that is different on every run. See
// closedWorldMasks.
//
// The masking happens HERE rather than at each call site so that there is
// exactly one place that can get it wrong, and so a future return path added
// to ClosedWorld cannot forget it: nothing else in this file constructs a
// lock.ClosedWorld carrying digests.
func closedWorldResult(runs []closedWorldRun, masks []closedWorldPathMask, result, detail string) lock.ClosedWorld {
	argvs := make([][]string, 0, len(runs))
	for _, r := range runs {
		argv := make([]string, 0, len(r.argv))
		for _, a := range r.argv {
			argv = append(argv, maskClosedWorldPaths(a, masks))
		}
		argvs = append(argvs, argv)
	}
	cmdDigest, _ := canonical.Digest(argvs)
	return lock.ClosedWorld{
		Result:        result,
		CommandDigest: cmdDigest,
		OutputDigest:  digest.Bytes([]byte(maskClosedWorldPaths(combinedOutput(runs), masks))),
		Detail:        maskClosedWorldPaths(detail, masks),
	}
}

// closedWorldUpgradeTargets reads the finished bundle's own lock.json — a
// sibling of bundleRepoDirAbs, per the frozen bundle layout
// (<bundle>/lock.json next to <bundle>/repo/) — and returns the exact
// architecture-qualified name:arch=version entries the online solve's
// full-upgrade pass chose (lock.Package.Reason == lock.ReasonUpgrade), plus
// the same information as a name:arch -> version map for the mismatch check
// below.
//
// ClosedWorldInput (frozen, core/apt/iface.go) carries no field for these:
// Install is deliberately only the requested/dependency set, never the whole
// pool (lock/types.go's own doc comment on Lock.Install), and Upgrades is a
// bare bool. The bundle's own lock.json is the one place left to get them
// from, and by the time ClosedWorld runs it is already guaranteed to exist:
// engine.finalizeBundle calls bundle.Assemble (which writes lock.json) before
// it calls runClosedWorld (core/engine/engine.go's run(), core/engine/
// finalize.go's assemble()/runClosedWorld() ordering) — the same
// already-verified-by-schema read core/install/bundle.go's loadLock performs
// from the installed side of this exact same layout.
func closedWorldUpgradeTargets(bundleRepoDirAbs string) (entries []string, want map[string]string, err error) {
	bundleDir := filepath.Dir(bundleRepoDirAbs)
	lk, lerr := lock.Load(bundleDir)
	if lerr != nil {
		return nil, nil, dferr.Wrap(dferr.Usage, lerr,
			"apt: closed-world: read the bundle's lock.json to determine the locked upgrade set")
	}

	want = make(map[string]string)
	for _, p := range lk.Packages {
		if p.Reason != lock.ReasonUpgrade {
			continue
		}
		key := p.Name + ":" + p.Arch
		want[key] = p.Version
		entries = append(entries, key+"="+p.Version)
	}
	sort.Strings(entries)
	return entries, want, nil
}

// upgradeMismatches compares a closed-world exact-version upgrade-set
// install simulation's Inst/Conf actions against the versions the lock
// recorded for that same set, and reports every package that would install
// differently. A package the simulation left untouched entirely is not a
// mismatch — apt only prints an Inst/Conf line for a package it would
// actually change, so no line at all means the target's dpkg status (copied
// into this same private root) already carries the locked version.
//
// This is defence 2 (ADR-007) made precise for the upgrade path: E3
// (docs/experiments/E3-pin-fidelity.md) found that a bare, unpinned
// full-upgrade against the bundle can silently select a different version
// than the one the online solve locked in, and report it as ordinary
// success ("Inst debark-e3-demo [1.0-1] (2.0-1 debark:bundle)" for a
// pin that had chosen 1.0-1 online). Asking for the exact locked version
// already makes that specific divergence structurally impossible for apt to
// produce without erroring outright (an exact "name:arch=version" request
// either succeeds at exactly that version or fails; apt does not silently
// substitute a different one) — this comparison is the explicit,
// independently-auditable proof of that, and the second line of defence
// against a bug elsewhere in this package (or a future change) that
// constructs an unpinned entry by mistake.
func upgradeMismatches(want map[string]string, sim SimulateResult) []string {
	var mismatches []string
	for _, act := range sim.Actions {
		if act.Kind != "Inst" && act.Kind != "Conf" {
			continue
		}
		key := act.Name + ":" + act.Arch
		wantVersion, ok := want[key]
		if !ok || act.Version == wantVersion {
			continue
		}
		mismatches = append(mismatches, fmt.Sprintf(
			"%s: the lock recorded %s as the upgrade target, but the simulated upgrade would install %s",
			key, wantVersion, act.Version))
	}
	sort.Strings(mismatches)
	return mismatches
}
