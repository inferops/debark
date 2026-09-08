package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
)

// runner is the standard Runner (New's return value). It holds nothing but
// its Deps: every call is independent and safe for concurrent use as long
// as the caller does not share a bundle directory across concurrent Applies.
type runner struct {
	deps Deps
}

func (r *runner) Plan(ctx context.Context, bundlePath string, opts Options) (*Report, error) {
	return r.execute(ctx, bundlePath, opts, false)
}

func (r *runner) Apply(ctx context.Context, bundlePath string, opts Options) (*Report, error) {
	return r.execute(ctx, bundlePath, opts, true)
}

// execute is Plan and Apply's shared body: verify, preconditions, compute
// the plan from the lock, and — only when apply is true — perform the
// installation. This is deliberately one function rather than Apply calling
// the public Plan and then a separate mutate step, so "Apply runs Plan and
// then performs the installation" (iface.go) is true of the actual control
// flow, not just true in spirit, and so a bundle is never re-verified or
// re-simulated partway through applying it.
func (r *runner) execute(ctx context.Context, bundlePath string, opts Options, apply bool) (*Report, error) {
	started := time.Now()
	report := &Report{
		SchemaVersion: SchemaVersion,
		StartedAt:     canonical.Time(started),
		BundlePath:    bundlePath,
	}
	finish := func(err error) (*Report, error) {
		report.FinishedAt = canonical.Time(time.Now())
		return report, err
	}

	// apt file: URIs break on a path containing a space; refuse early with a
	// clear message rather than a confusing apt parse error later
	// (install-offline.sh line ~81-83).
	if strings.ContainsAny(bundlePath, " \t\n") {
		return finish(dferr.New(dferr.Usage, "install: bundle path must not contain whitespace (apt file: URIs break): %s", bundlePath))
	}

	bundleDir, cleanupBundle, err := resolveBundleDir(ctx, bundlePath)
	if err != nil {
		return finish(dferr.Wrap(dferr.Usage, err, "install: open bundle"))
	}
	defer cleanupBundle()
	if abs, absErr := filepath.Abs(bundleDir); absErr == nil {
		bundleDir = abs
	}

	var outputBuf bytes.Buffer
	record := func(argv []string, out []byte) {
		outputBuf.Write(out)
		if r.deps.OnAptOutput != nil {
			r.deps.OnAptOutput(argv, out)
		}
	}
	digestSoFar := func() {
		if outputBuf.Len() > 0 {
			report.AptOutputDigest = digest.Bytes(outputBuf.Bytes())
		}
	}

	// verify.Options.SkipFileDigests stops the pool's .deb contents being
	// hashed at all (only their size is compared), and it is documented in
	// core/verify as "reserved for inspect on very large bundles; verify
	// itself never sets it". Options.Verify is whatever install's caller
	// handed over, so nothing but this line stops that flag reaching the one
	// caller for which it is not a display convenience: install is the point
	// at which those exact bytes are unpacked as root. Skipping them would
	// leave a same-size substitution undetected on the one path where the
	// substitution matters, which is a way around verify-before-apt, not a
	// speed option. Refused explicitly rather than quietly forced off, so a
	// caller that asked for it learns that it did not happen.
	if opts.Verify.SkipFileDigests {
		return finish(dferr.New(dferr.Usage,
			"install: refusing to install with file-digest checking disabled").
			WithHint("verify.Options.SkipFileDigests is for inspecting a bundle, never for installing one"))
	}

	// 1. Verify first, always. There is no code path below this point that
	// runs against an unverified bundle (ADR-008).
	vrep, verr := r.deps.Verifier.Verify(ctx, bundleDir, opts.Verify)
	report.Verify = vrep
	if vrep != nil {
		report.BundleID = vrep.BundleID
	}
	if verr != nil {
		// Only an *unclassified* verifier error becomes Verification. The
		// verifier classifies the failures it can tell apart itself, and
		// deliberately returns "gpg is not installed" as Environment ("that
		// is an environment problem, not evidence about the bundle" —
		// verify.checkSignatures); since dferr.ClassOf resolves the
		// OUTERMOST *Error, wrapping such a cause in Verification here would
		// report a missing binary with the exit code ADR-012 gives the
		// operator as "the media is compromised, escalate" — and would drop
		// the cause's "install gnupg" hint with it. Same shape as
		// core/engine's classify(): preserve an already-classified cause,
		// classify only one that arrived with no class at all.
		var classified *dferr.Error
		if errors.As(verr, &classified) {
			return finish(verr)
		}
		return finish(dferr.Wrap(dferr.Verification, verr, "install: bundle verification failed"))
	}
	if vrep == nil || !vrep.OK {
		return finish(dferr.New(dferr.Verification, "install: bundle failed verification").
			WithHint("run 'debark verify' for details"))
	}

	// The plan comes from the lock, never from apt's own opinion (ADR-007):
	// load it now that verification has proven it trustworthy.
	lk, err := loadLock(bundleDir)
	if err != nil {
		return finish(dferr.Wrap(dferr.Usage, err, "install: read lock.json"))
	}
	report.TargetExpected = lk.Target

	// 2. Preconditions.
	if err := lookExecutable(r.deps.DpkgPath); err != nil {
		return finish(dferr.New(dferr.Environment, "install: dpkg not usable: %v", err).
			WithHint("install dpkg, or set Deps.DpkgPath"))
	}
	if !opts.Dpkg {
		if err := lookExecutable(r.deps.AptPath); err != nil {
			return finish(dferr.New(dferr.Environment, "install: apt-get not usable: %v", err).
				WithHint("install apt-get, or set Deps.AptPath, or pass --dpkg"))
		}
	}

	archOut, archErr := runProcess(ctx, r.deps.DpkgPath, []string{"--print-architecture"}, buildEnv(false))
	record(archOut.Argv, archOut.Combined)
	if archErr != nil {
		return finish(dferr.New(dferr.Environment, "install: dpkg --print-architecture failed: %v", archErr).
			WithHint("check Deps.DpkgPath"))
	}
	actualArch := strings.TrimSpace(string(archOut.Combined))

	// The machine's foreign architectures are asked for unconditionally, not
	// only when the bundle needs one, so Report.TargetActual describes the
	// machine as fully as TargetExpected describes the bundle: "which
	// architectures does this dpkg accept" is the fact the refusal below
	// turns on, and a report that carried it only in the failing case would
	// be missing it from exactly the runs an operator later compares against.
	// A failure to answer is NOT fatal here - see the severity split below.
	foreignOut, foreignErr := runProcess(ctx, r.deps.DpkgPath, []string{"--print-foreign-architectures"}, buildEnv(false))
	record(foreignOut.Argv, foreignOut.Combined)
	var actualForeign []string
	if foreignErr == nil {
		actualForeign = parseForeignArchs(foreignOut.Combined)
	}

	root := effectiveRoot(r.deps)
	rel := readOSRelease(root)
	report.TargetActual = lock.Target{
		DistroID:     rel.ID,
		VersionID:    rel.VersionID,
		Codename:     rel.VersionCodename,
		Arch:         actualArch,
		ForeignArchs: actualForeign,
	}

	if lk.Target.Arch != "" && actualArch != "" && actualArch != lk.Target.Arch {
		report.Problems = append(report.Problems, fmt.Sprintf(
			"architecture mismatch: bundle is %s, this machine is %s", lk.Target.Arch, actualArch))
		return finish(dferr.New(dferr.TargetMismatch,
			"install: architecture mismatch: bundle is %s, this machine is %s", lk.Target.Arch, actualArch))
	}

	// Foreign architectures: the second half of "is this bundle for this
	// machine", and until 2026-09-06 the half nothing checked. See
	// missingForeignArchs (precondition.go) for the measurement: a foreign-arch
	// bundle on a target without `dpkg --add-architecture i386` fails rc=100
	// with dpkg refusing each archive, the same bundle after that one command
	// succeeds rc=0, and `apt-get -s install` succeeds in BOTH cases, so the
	// simulate-based plan check below cannot see the difference.
	//
	// Both signed sources for the list are consulted, and their union is what
	// is required. lock.Target.ForeignArchs is the one the existing
	// architecture check's sibling field lives in and is covered by the
	// signature (verify ties lock.json to the signed manifest before install
	// ever reads it); verify.ReportTarget.ForeignArchs is the manifest's own
	// copy, which core/manifest and core/verify both document as existing
	// specifically so "install's architecture precondition check" has a signed
	// source for it. Taking the union rather than picking one means a bundle
	// whose two signed copies disagree - only possible through a builder bug,
	// since both derive from one snapshot - refuses on the stricter reading
	// instead of installing on the more permissive one.
	requiredForeign := lk.Target.ForeignArchs
	if vrep != nil && len(vrep.Target.ForeignArchs) > 0 {
		requiredForeign = mergeSortedUnique(requiredForeign, vrep.Target.ForeignArchs)
	}
	if len(requiredForeign) > 0 && foreignErr != nil {
		// Severity is deliberately conditional. A dpkg that cannot answer
		// --print-foreign-architectures is only an install-stopping problem
		// when the bundle actually needs a foreign architecture; failing every
		// ordinary single-arch install because of it would trade a real
		// regression for no safety at all. When the bundle DOES need one,
		// falling through with an empty list would silently disable this
		// check - the same reasoning that makes a failed --print-architecture
		// query fatal just above.
		return finish(dferr.New(dferr.Environment,
			"install: dpkg --print-foreign-architectures failed: %v", foreignErr).
			WithHint("this bundle needs foreign architecture support; check that dpkg is usable on this machine"))
	}
	if missing := missingForeignArchs(requiredForeign, actualForeign, actualArch); len(missing) > 0 {
		have := "none"
		if len(actualForeign) > 0 {
			have = strings.Join(actualForeign, ", ")
		}
		problem := fmt.Sprintf(
			"foreign architecture not enabled: bundle needs %s, this machine has %s",
			strings.Join(missing, ", "), have)
		report.Problems = append(report.Problems, problem)
		return finish(dferr.New(dferr.TargetMismatch, "install: %s", problem).
			WithHint("run (as root) and then install again: %s", addArchitectureCommand(missing)))
	}
	if lk.Target.Codename != "" && rel.VersionCodename != "" && !strings.EqualFold(lk.Target.Codename, rel.VersionCodename) {
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"bundle was built for %q but this machine is %q - continuing anyway", lk.Target.Codename, rel.VersionCodename))
	}

	// Was this bundle built against an assumed base rather than a snapshot of
	// this machine, and if so, how far has the assumption drifted from what
	// is actually here (ADR-014)? See basedivergence.go, which also explains
	// why the answer is always a warning and never a refusal.
	//
	// Its position in this function is load-bearing in two ways. It is the
	// last of the "is this bundle for this machine" checks and sits with
	// them, because that is what it is - the same question the architecture,
	// foreign-architecture and codename checks above ask, asked of the
	// installed set instead of the identity. And it runs BEFORE the private
	// apt root is built and before apt-get is invoked at all, so the operator
	// reads it above apt's own output rather than underneath a screen of it,
	// and so a run that dies at `apt-get update` still carries the finding.
	// Being here rather than in an apply-only branch is also what makes it
	// appear for Plan, which is to say for --status and --dry-run: an
	// operator asking "what will this do?" before touching the machine is
	// exactly who most needs to know the bundle rests on an assumption.
	r.noteBaseDivergence(ctx, bundleDir, report, record)

	// --keep-source is checked HERE, before the private root is built and
	// long before anything is installed, rather than at PersistSource time -
	// which runs only after a successful install, when refusing would mean
	// telling the operator their packages are in but the thing they asked
	// for is not, with no way back. Plan is gated identically so --dry-run
	// and --status answer the question the operator actually asked ("what
	// will this do?") instead of discovering it during the real run. See
	// keepsource.go for what the flag grants and why some directories must
	// not be granted it.
	repoDir := filepath.Join(bundleDir, repoDirName)
	if opts.KeepSource {
		if opts.Dpkg {
			// --dpkg never builds a private root, so there is no .sources
			// file to persist and PersistSource is never reached. Saying so
			// beats letting a flag the operator typed do nothing in silence.
			report.Warnings = append(report.Warnings,
				"--keep-source does nothing with --dpkg: the raw dpkg path never builds an apt source to keep")
		} else if problem := keepSourceTrustProblem(repoDir); problem != "" {
			return finish(dferr.New(dferr.Usage,
				"install: refusing --keep-source: %s", problem).
				WithHint("copy the bundle to a directory only root can write and run --keep-source from there, or drop --keep-source and let the source stay temporary"))
		}
	}

	// The plan comes from the lock (ADR-007): select exactly what the online
	// solve chose, never "whatever is newest in the bundle". --dpkg ignores
	// --all/--only entirely and always targets every file in the pool,
	// matching install-offline.sh's --dpkg branch exactly.
	var names []string
	var pkgs []lock.Package
	if opts.Dpkg {
		names = make([]string, 0, len(lk.Packages))
		for _, p := range lk.Packages {
			names = append(names, nameVersion(p.Name, p.Version))
		}
		sort.Strings(names)
		pkgs = lk.Packages
		report.Warnings = append(report.Warnings,
			"using the raw dpkg path - apt's ordering and conflict checks are skipped")
	} else {
		names, pkgs, err = selectInstallSet(lk, opts)
		if err != nil {
			return finish(dferr.Wrap(dferr.Usage, err, "install: selecting packages"))
		}
		// --upgrade: fold the lock's own upgrade set into the exact-version
		// install request, rather than asking apt to freely re-derive an
		// upgrade against the flat bundle (E3, ADR-007 — see
		// selectUpgradeSet's doc comment). From here on names/pkgs already
		// carry everything to install, so the rest of this function needs no
		// separate "is this an upgrade" branch at all: the single
		// "-s install"/"install" call below already covers it, and apt's own
		// Inst-line shape ("[old] (new)" vs plain "(new)") tells ToInstall
		// from ToUpgrade apart exactly as it always has.
		if opts.Upgrade {
			upNames, upPkgs, total := selectUpgradeSet(lk, pkgs)
			if total == 0 {
				report.Warnings = append(report.Warnings,
					"install: --upgrade was requested but the lock records no packages with reason \"upgrade\" - nothing beyond the requested set will be installed")
			}
			names = mergeSortedUnique(names, upNames)
			pkgs = append(pkgs, upPkgs...)
			sort.Slice(pkgs, func(i, j int) bool {
				if pkgs[i].Name != pkgs[j].Name {
					return pkgs[i].Name < pkgs[j].Name
				}
				return pkgs[i].Arch < pkgs[j].Arch
			})
		}
	}

	// Every file the plan names must actually be in the pool, and it is
	// install that has to say so.
	//
	// core/verify proves the bundle is the one that was signed - and, since
	// checkLockPackages landed there, that lock.json's per-package digests
	// are the ones the signed manifest records for those same paths. Neither
	// of those is the question asked here, which is narrower and is about
	// this run: are the files THIS install is about to hand to apt or dpkg
	// on the medium at all. It is one stat per selected package, no hashing,
	// and it closes a real false success rather than duplicating a check:
	// --dpkg installs whatever .deb files the pool walk finds and reports
	// ToInstall from the LOCK, so a lock entry with no file behind it used
	// to come back as a clean, applied, OK install of a package that was
	// never unpacked. On the apt path it turns a failure that would
	// otherwise land mid-install, with some packages already unpacked, into
	// a refusal that --dry-run can see.
	//
	// The filename is checked for shape before it is used, rather than
	// trusted because core/lock validates it on load: a gate that is only
	// correct because another package sanitised its input has no property of
	// its own, and this one is turning a document into a filesystem path.
	poolProblems := missingPoolFiles(repoDir, pkgs)
	report.Problems = append(report.Problems, poolProblems...)

	var required int64
	for _, p := range pkgs {
		required += p.Size
	}
	var diskProblem bool
	if free, ok := diskFree(root); ok {
		if shortfall, short := diskShortfall(free, required); short {
			diskProblem = true
			report.Problems = append(report.Problems, fmt.Sprintf(
				"insufficient disk space: short by %d bytes", shortfall))
		}
	}

	// 3-4. Private apt view + plan from the lock (skipped for --dpkg, which
	// never touches apt at all).
	var pr *privateRoot
	if !opts.Dpkg {
		pr, err = newPrivateRoot(repoDir, opts.Fast)
		if err != nil {
			return finish(dferr.Wrap(dferr.Environment, err, "install: build private apt view"))
		}
		// Deliberately ignored, not forgotten: Close only removes the
		// temporary private root, and a failure to remove a scratch
		// directory must not change what install reports about the
		// installation itself.
		defer func() { _ = pr.Close() }()

		out, uerr := runProcess(ctx, r.deps.AptPath, append(pr.AptArgs(false), "update"), buildEnv(opts.Fast))
		record(out.Argv, out.Combined)
		if uerr != nil {
			return finish(dferr.New(dferr.Environment, "install: apt-get update failed: %v", uerr))
		}

		if len(names) > 0 {
			simArgs := append(pr.AptArgs(false), append([]string{"-s", "install"}, names...)...)
			simOut, simErr := runProcess(ctx, r.deps.AptPath, simArgs, buildEnv(opts.Fast))
			record(simOut.Argv, simOut.Combined)
			toInstall, toUpgrade, toRemove, problems := parseAptSim(simOut.Combined)
			report.ToInstall = toInstall
			report.ToUpgrade = toUpgrade
			report.ToRemove = toRemove
			report.Problems = append(report.Problems, problems...)
			if simErr != nil && len(problems) == 0 {
				report.Problems = append(report.Problems, "apt-get -s install: "+simErr.Error())
			}
			already := len(names) - len(toInstall) - len(toUpgrade)
			if already > 0 {
				report.AlreadyCurrent = already
			}
		}
		// opts.Upgrade needs no separate simulate call here any more: names
		// already carries the lock's upgrade set (folded in above, before
		// this block), so the "-s install" simulation just above already
		// simulated it, exactly as it does for the plain install set — apt
		// itself reports an already-installed package's exact-version request
		// as an "Inst pkg [old] (new)" line, which parseAptSim already buckets
		// into ToUpgrade. This is the fix for E3/ADR-007: the target's apt is
		// never asked an open "full-upgrade" question against the flat
		// bundle, only ever the same closed "can you install exactly these
		// versions" question defence 1 already relies on.
	} else {
		report.ToInstall = names
	}

	report.OK = len(report.Problems) == 0
	digestSoFar()
	evidence.Emit(r.deps.Events, evidence.TypeInstallPlan, "install plan computed", map[string]any{
		"bundle_id":       report.BundleID,
		"bundle_path":     bundleDir,
		"to_install":      len(report.ToInstall),
		"to_upgrade":      len(report.ToUpgrade),
		"to_remove":       len(report.ToRemove),
		"already_current": report.AlreadyCurrent,
		"ok":              report.OK,
		"dpkg_fallback":   opts.Dpkg,
	})

	if !apply {
		return finish(nil)
	}

	// --- Apply: perform the installation. ---
	if !report.OK {
		// Disk space is bucketed under Environment in the exit-code table
		// ("no apt, no container runtime, no disk, no permission") — a more
		// fundamental blocker than apt's own Resolution failures, and
		// classified as such even when an apt-detected problem is also
		// present.
		class := dferr.Resolution
		switch {
		case len(poolProblems) > 0:
			// A bundle that does not hold the files its own signed lock
			// names is a damaged or incomplete medium, and that is what
			// exit 4 means - the same answer `debark verify` gives for
			// the same bundle, since a pool file the manifest lists and the
			// medium lacks is a verify Problem too. Ranked above disk space
			// because it is a statement about the artifact, not about this
			// machine.
			class = dferr.Verification
		case diskProblem:
			class = dferr.Environment
		}
		return finish(dferr.New(class, "install: refusing to apply — the plan has problems: %s",
			strings.Join(report.Problems, "; ")))
	}
	if err := requireConfirmation(opts); err != nil {
		return finish(err)
	}

	var applyErr error
	if opts.Dpkg {
		applyErr = r.applyDpkg(ctx, repoDir, opts, record)
	} else {
		applyErr = r.applyApt(ctx, pr, names, opts, record)
		if applyErr == nil && opts.KeepSource {
			// The failure branch has always warned. The SUCCESS branch is
			// the one that changes the machine: it is the only outcome of
			// any install debark performs that leaves a standing,
			// unverified, root-level package-installation channel behind,
			// and a report that mentioned it only when it went wrong would
			// be silent in exactly the case worth reporting.
			if dest, perr := pr.PersistSource(root); perr != nil {
				report.Warnings = append(report.Warnings, "could not persist source: "+perr.Error())
			} else {
				report.Warnings = append(report.Warnings, keepSourceStandingTrustWarning(dest, pr.repoDir))
			}
		}
	}

	report.Applied = true
	report.OK = applyErr == nil
	if applyErr != nil {
		report.Problems = append(report.Problems, applyErr.Error())
	}
	digestSoFar()

	evidence.Emit(r.deps.Events, evidence.TypeInstallResult, "install finished", map[string]any{
		"bundle_id":     report.BundleID,
		"bundle_path":   bundleDir,
		"ok":            report.OK,
		"dpkg_fallback": opts.Dpkg,
	})
	r.recordEvidence(report, opts)

	if applyErr != nil {
		return finish(dferr.Wrap(dferr.Resolution, applyErr, "install: apply failed"))
	}
	return finish(nil)
}

// applyApt performs the real, mutating apt-get call: install of the exact
// locked set (step 4). names already includes the lock's upgrade set
// when opts.Upgrade was requested (selectUpgradeSet, folded in by execute()
// before this runs) — there is deliberately no separate "apt-get
// full-upgrade" call here: E3 found that asking the target's apt to
// re-derive an upgrade freely against the flat bundle can silently reselect
// a version the target's own pin was written to reject (ADR-007). An
// already-installed package named at its exact locked version is still an
// upgrade (or downgrade) as far as apt-get install is concerned — it reads
// the target's real dpkg status and acts accordingly — so one exact-version
// install call already does everything full-upgrade used to do here, without
// ever handing apt an open question.
func (r *runner) applyApt(ctx context.Context, pr *privateRoot, names []string, opts Options, record func([]string, []byte)) error {
	env := buildEnv(opts.Fast)

	if len(names) == 0 {
		return nil
	}
	args := append(pr.AptArgs(opts.Yes), append([]string{"install"}, names...)...)
	out, err := runProcess(ctx, r.deps.AptPath, args, env)
	record(out.Argv, out.Combined)
	if err != nil {
		return fmt.Errorf("apt-get install: %w", err)
	}
	return nil
}

// applyDpkg is the fallback path: dpkg --unpack every pool file, then
// dpkg --configure -a. It skips apt's ordering and conflict checks
// entirely, which is why Plan/Apply always attach a loud warning when it is
// used (install-offline.sh's --dpkg branch, ported faithfully).
func (r *runner) applyDpkg(ctx context.Context, repoDir string, opts Options, record func([]string, []byte)) error {
	files, err := poolDebFiles(repoDir)
	if err != nil {
		return fmt.Errorf("list pool files: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no .deb files found under %s", filepath.Join(repoDir, "pool"))
	}

	var fastArgs []string
	if opts.Fast {
		fastArgs = []string{"--force-unsafe-io"}
	}
	env := buildEnv(opts.Fast)

	// Chunked so a bundle with many thousands of packages never risks the
	// platform's argv length limit; install-offline.sh gets away with a
	// single glob because typical bundles are small, but nothing about the
	// design caps bundle size.
	const chunkSize = 200
	for i := 0; i < len(files); i += chunkSize {
		end := i + chunkSize
		if end > len(files) {
			end = len(files)
		}
		args := append(append([]string{}, fastArgs...), "--unpack")
		args = append(args, files[i:end]...)
		out, err := runProcess(ctx, r.deps.DpkgPath, args, env)
		record(out.Argv, out.Combined)
		if err != nil {
			return fmt.Errorf("dpkg --unpack: %w", err)
		}
	}

	args := append(append([]string{}, fastArgs...), "--configure", "-a")
	out, err := runProcess(ctx, r.deps.DpkgPath, args, env)
	record(out.Argv, out.Combined)
	if err != nil {
		return fmt.Errorf("dpkg --configure -a: %w", err)
	}
	return nil
}

// recordEvidence appends the durable, local install.result record
// (step 6). A failure to write it is reported as a warning, not a
// failure of the install itself: by the time this runs the installation
// (successful or not) has already happened.
func (r *runner) recordEvidence(report *Report, opts Options) {
	path := opts.EvidencePath
	if path == "" {
		path = DefaultEvidencePath()
	}
	ev := evidence.New(evidence.TypeInstallResult, "debark install", map[string]any{
		"bundle_path":       report.BundlePath,
		"bundle_id":         report.BundleID,
		"applied":           report.Applied,
		"ok":                report.OK,
		"to_install":        report.ToInstall,
		"to_upgrade":        report.ToUpgrade,
		"to_remove":         report.ToRemove,
		"already_current":   report.AlreadyCurrent,
		"warnings":          report.Warnings,
		"problems":          report.Problems,
		"apt_output_digest": report.AptOutputDigest,
		"dpkg_fallback":     opts.Dpkg,
	})
	if err := appendEvidenceLog(path, ev); err != nil {
		report.Warnings = append(report.Warnings, "could not write local evidence log: "+err.Error())
	}
}

// poolDebFiles lists every .deb under repoDir/pool, sorted, for the dpkg
// fallback.
func poolDebFiles(repoDir string) ([]string, error) {
	root := filepath.Join(repoDir, "pool")
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".deb") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// mergeSortedUnique merges a and b, deduplicated and sorted.
func mergeSortedUnique(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
