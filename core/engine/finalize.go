package engine

import (
	"context"
	"os"
	"path/filepath"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/bundle"
	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/doctor"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/version"
)

// assemble calls bundle.Assemble to materialise the pool, generate the repo
// indices, and write the first cut of lock.json, snapshot.json and
// README.txt. Its output (res.Lock) becomes authoritative from here on:
// it carries the Packages/Stats/file-digest fills the assembler's own
// contract promises on top of what buildInitialLock already supplied.
func (b *build) assemble(ctx context.Context) error {
	b.bundleDir = b.assemblyDir()
	if err := os.MkdirAll(filepath.Dir(b.bundleDir), 0o755); err != nil {
		return dferr.Wrap(dferr.Environment, err, "engine: prepare output directory")
	}

	// --update (refresh) prunes superseded pool files unless the request says
	// not to; plain additive runs never prune, regardless of Options.Prune,
	// per Options.Prune's own doc ("removes superseded files in refresh
	// mode"). User-supplied files are never pruned either way — that
	// exclusion is bundle.Prune's job, run internally by Assemble.
	shouldPrune := b.req.Options.UpdateMode == buildjob.UpdateRefresh && b.req.Options.Prune

	res, err := assembleBundle(ctx, bundle.Input{
		Dir:      b.bundleDir,
		Plan:     b.plan,
		Store:    b.st,
		Snapshot: b.snap.Snapshot,
		Lock:     b.lockDoc,
		Release: repository.ReleaseFields{
			Origin:        repository.DefaultOrigin,
			Label:         repository.DefaultLabel,
			Suite:         repository.DefaultSuite,
			Codename:      b.snap.Snapshot.Target.Codename,
			Architectures: b.repoArches(),
			Components:    []string{repository.DefaultComponent},
			Description:   repository.DefaultDescription,
			Date:          b.createdAt,
		},
		Prune:       shouldPrune,
		EmbedBinary: b.req.Options.EmbedBinary,
		CreatedAt:   b.createdAt,
	})
	if err != nil {
		return classify(err, dferr.Environment, "engine: assemble bundle")
	}
	b.assembled = res
	if res.Lock != nil {
		b.lockDoc = res.Lock
	}

	// unreferenced joins added/removed as a third count on the same event.
	//
	// It is the one of the three that describes the bundle's state rather
	// than what this run did, and it is the only one an operator cannot
	// derive from the other two: a pool file that was neither added nor
	// removed this run appears in neither of their reports, yet it is on the
	// media, inside the manifest and therefore inside the operator's
	// signature, while repo/Packages does not index it (core/bundle's
	// unreferenced.go). core/bundle already writes the full list to
	// last-run-unreferenced.txt and raises pool.unreferenced into lock.json;
	// evidence.json is the third surface an auditor reads, and it was the
	// only one of the three that stayed silent. A count, not the list: this
	// event's shape is counts, and the complete list already has two homes
	// inside the same bundle.
	b.emit(evidence.TypeBundleAssembled, "bundle assembled", map[string]any{
		"package_count": len(b.lockDoc.Packages), "added": len(res.Added), "removed": len(res.Removed),
		"unreferenced": len(res.Unreferenced),
	})
	if len(res.Removed) > 0 {
		b.emit(evidence.TypePruned, "pruned superseded files", map[string]any{"count": len(res.Removed)})
	}
	if res.Repo != nil {
		b.emit(evidence.TypeRepoIndexed, "repository indexed", map[string]any{
			"package_count": res.Repo.PackageCount, "pool_bytes": res.Repo.PoolBytes,
		})
	}
	return nil
}

// runDoctor folds doctor's findings into the in-memory lock's package flags.
// doctor never fails a build by itself (its own package doc says so
// explicitly), so any error here is recorded as a warning event and
// otherwise ignored — "run doctor when cheap" reads as best-effort, not
// load-bearing.
func (b *build) runDoctor(ctx context.Context) {
	report, err := doctorRun(ctx, doctor.Input{
		BundleDir: b.bundleDir, Lock: b.lockDoc, Snapshot: b.snap.Snapshot, ScanScripts: true,
		// DpkgStatus is the target's captured dpkg status, read out of the
		// still-open snapshot archive the same way installRecommends
		// (snapshot.go) already reads other captured files from it. Without
		// this, doctor's held-package and dkms-headers checks have no
		// installed-package list to check against and can never fire — see
		// doctor.Input.DpkgStatus's own doc comment, and defect 6 in the implementation notes.
		DpkgStatus: dpkgStatusBytes(b.snap),
	})
	if err != nil {
		b.warn(evidence.TypeWarning, "doctor: "+err.Error(), nil)
		return
	}
	for _, f := range report.Findings {
		b.emit(evidence.TypeDoctorFinding, f.Message, map[string]any{
			"check": f.Check, "severity": string(f.Severity), "package": f.Package, "flag": f.Flag,
		})
	}
	for i := range b.lockDoc.Packages {
		pkg := &b.lockDoc.Packages[i]
		add := doctorFlagsFor(report.Findings, pkg.Name)
		if len(add) == 0 {
			continue
		}
		flags := pkg.Flags
		for _, fl := range add {
			if !containsString(flags, fl) {
				flags = append(flags, fl)
			}
		}
		pkg.Flags = sortedStrings(flags)
	}
}

// runClosedWorld re-resolves the finished bundle against itself with the
// bundle as the only source (ADR-007) and folds the result into the
// lock. A failure here — the check ran but the bundle would not actually
// install offline — is deliberately classified as dferr.Resolution (exit 5),
// not dferr.Incomplete (exit 3): Incomplete means a specific, enumerable
// piece is missing (a URL failed, an external package's dependencies could
// not be resolved), which is exactly what Unresolved/FetchFailed already
// report. A closed-world failure is a different kind of problem — apt,
// asked a second time under the exact conditions the target will use, could
// not satisfy the same install set apt itself just chose — which is
// "apt could not satisfy the request" by definition, just caught one step
// later than usual. buildResult applies the priority between the two.
func (b *build) runClosedWorld(ctx context.Context) {
	if b.req.Options.ClosedWorldCheck != nil && !*b.req.Options.ClosedWorldCheck {
		b.lockDoc.ClosedWorld = lock.ClosedWorld{Result: lock.ClosedWorldSkipped, Detail: "disabled by request options"}
		b.emit(evidence.TypeClosedWorld, "closed-world check skipped", map[string]any{"result": lock.ClosedWorldSkipped})
		return
	}

	workDir := filepath.Join(b.workRoot, "closed-world")
	_ = os.MkdirAll(workDir, 0o755)

	cw, err := b.backend.ClosedWorld(ctx, apt.ClosedWorldInput{
		Snapshot:         b.snap.Snapshot,
		SnapshotFilesDir: b.snap.FilesDir(),
		BundleRepoDir:    b.bundleRepoPath(),
		Install:          b.lockDoc.Install,
		Upgrades:         b.req.Options.Upgrades,
		WorkDir:          workDir,
	})
	if err != nil {
		// A closed-world check that could not even run is reported the same
		// way as one that ran and failed: never a silent pass.
		cw = lock.ClosedWorld{Result: lock.ClosedWorldFailed, Detail: err.Error()}
	}
	b.lockDoc.ClosedWorld = cw

	if cw.Result == lock.ClosedWorldOK {
		b.emit(evidence.TypeClosedWorld, "closed-world check passed", map[string]any{"result": cw.Result})
		return
	}
	if cw.Result == lock.ClosedWorldSkipped {
		// Skipped is not failed. The check is skipped when there is nothing
		// for it to check - most importantly when every input was
		// unresolvable, so the install set is empty. Treating that as a
		// failure masked the more specific outcome: a build whose vendor
		// .deb could not be satisfied must exit 3 (incomplete, with the
		// unresolved inputs named), not exit 5 (resolution failed), because
		// those tell an operator different things about what they now have.
		// The skip is still recorded and warned about; it just does not
		// claim the bundle failed a check that never ran.
		b.warn(evidence.TypeClosedWorld, "closed-world check skipped", map[string]any{"result": cw.Result, "detail": cw.Detail})
		b.lockDoc.Warnings = append(b.lockDoc.Warnings, lock.Warning{
			Code:    "closed-world.skipped",
			Message: "closed-world check skipped: " + cw.Detail,
		})
		return
	}
	b.closedWorldFailed = true
	b.warn(evidence.TypeClosedWorld, "closed-world check "+cw.Result, map[string]any{"result": cw.Result, "detail": cw.Detail})
	b.lockDoc.Warnings = append(b.lockDoc.Warnings, lock.Warning{
		Code:    "closed-world." + cw.Result,
		Message: "closed-world check " + cw.Result + ": this bundle may not install cleanly with no network. " + cw.Detail,
	})
}

// foldUnsignedWarning records that this bundle will ship unsigned. It runs
// after backend selection resolved b.signer (in resolveDeps, before anything
// was written), so the fact is known well before this point; it is folded in
// here, alongside the doctor and closed-world folds, so all three land in the
// same single lock rewrite in finalizeBundle.
func (b *build) foldUnsignedWarning() {
	if b.signer != nil {
		return
	}
	b.lockDoc.Warnings = append(b.lockDoc.Warnings, lock.Warning{
		Code:    "unsigned",
		Message: "this bundle is unsigned; verify will refuse it unless --allow-unsigned is given. Pass --sign or configure a signer to produce a verifiable bundle.",
	})
}

// finalizeBundle is where the ordering rule that matters lives.
//
// bundle.Assemble already wrote a first cut of lock.json and README.txt, but
// neither can be final at that point: lock.json is missing the doctor flags
// and the closed-world result (both only knowable after assembly), and
// README.txt is a pure function of the lock plus a "signed" bool that
// Assemble has no way to know (Deps.Signer is engine-only state; signing
// itself can only happen once the manifest exists, which needs the tree to
// be finished — including the README). So:
//
//  1. lock.json is validated (lock.Validate — see below) and rewritten ONCE
//     here, after doctor and closed-world and the unsigned-warning fold have
//     all landed in b.lockDoc (run() calls those three before calling this
//     method) — one rewrite, not three.
//  2. README.txt is re-rendered from that final lock and the intent to sign
//     (b.signer != nil) and overwrites Assemble's first draft.
//  3. sbom.cdx.json is written when Options.SBOM asked for one (writeSBOM,
//     in sbom.go), from the same final lock and bundle id.
//  4. When this bundle will be signed, the manifest.signed evidence event is
//     recorded now — BEFORE evidence.json is written — not after the real
//     Sign call far below. See "Ordering the signing event" below for why.
//  5. evidence.json is built by hand (not via collector.Document(), which
//     stamps the real clock — see evidence.go) from b.collector's events,
//     which by now includes every event emitted above and, when this bundle
//     will be signed, step 4's manifest.signed — the fix for defect 3 in the implementation notes: finalizeBundle's own promise, right here, is that this
//     file "holds every event emitted above", and until this fix that was
//     false for exactly one event, the one an auditor would care about most.
//  6. manifest.Build walks the bundle tree — which now includes the final
//     lock, the final README, the SBOM (if any) and evidence.json —
//     producing the one manifest that actually gets saved and signed.
//  7. Only if a signer is configured is the real Sign called, over that
//     manifest's canonical bytes, and only on success is
//     debark.manifest.sig written.
//
// Nothing after this method runs may write into the bundle directory: the
// manifest must be the last thing written, and it must cover every other
// file, which is only true if every other file is already final when
// manifest.Build walks the tree.
//
// # Ordering the signing event (defect 3)
//
// The manifest must cover evidence.json (step 6 needs it on disk already),
// and the signature covers the manifest (step 7 signs the exact bytes
// step 6 produced) — so evidence.json has to be in its FINAL form before the
// one real Sign call this method makes. That is a hard, unavoidable
// ordering: there is no way to call Sign first and then fold its result into
// a file the manifest it just signed is supposed to already cover.
//
// The two facts this event ever carried are signer_kind and key_id (see the
// map below — never the signature bytes or the signature's own CreatedAt),
// and both are available from the Signer interface itself, before Sign is
// ever called: Kind() and KeyID() are documented as "the signer_kind
// recorded in the signature block" and "identifies the signing key"
// (core/sign/iface.go) — the stable identity of a configured signer, not a
// fact only Sign's return value can reveal. Every built-in Signer echoes
// exactly that identity back in the Signature Sign returns (ed25519.go and
// gpg.go both set SignerKind/KeyID from their own Kind()/KeyID()), so
// recording it here is not a guess about what Sign will report — for a
// plugin signer specifically, KeyID() can still be empty here if the
// plugin's best-effort key_info exchange at construction time did not
// complete (see core/sign/plugin.go); this is judged an acceptable, narrow
// gap rather than a reason to pay for a second real signing operation on
// every signed build (a second gpg-agent/pinentry round trip, or a second
// billed HSM/KMS call, merely to re-derive two facts that are already
// static properties of the configured signer).
//
// This is safe for what actually gets read later: "signed" only ever
// becomes durably true on disk. Every error path from here to the end of
// this method — an unwritable README, SBOM or evidence file, a manifest
// build failure, and, the one that matters most for this event, Sign()
// itself failing below — either returns before evidence.json exists at all,
// or (Sign failing) removes the whole half-built bundle directory before
// returning. Nobody ever reads a persisted bundle whose evidence.json
// claims a signature the bundle does not carry. A caller streaming events
// live (--json-events) could in principle observe this event moments before
// a later failure aborts the build — exactly as README.txt already commits
// to rendering "this bundle is signed" from this same willSign value before
// Sign is called, a few lines below — but Build's own contract is clear
// that such a run produces no bundle and returns an error, so nothing that
// reads the actual result (the returned error, the process exit code, or a
// finished bundle on disk) is ever misled.
func (b *build) finalizeBundle(ctx context.Context) error {
	// Everything that can add to the lock has now run (assemble, doctor,
	// runClosedWorld, foldUnsignedWarning). Before the one rewrite below,
	// strip the values that describe this machine rather than this request:
	// the per-run temporary work root and the output path, wherever a
	// collaborator's text carried them, and the store-cache-dependent
	// download counter. See reproducible.go for the full argument — this is
	// the single place the engine makes the lock reproducible, exactly as
	// lock.Validate below is the single place it checks it for correctness.
	//
	// It is NOT the only writer of lock.json. core/bundle/assemble.go writes
	// one too, and finalize.go:64 then adopts that object, so the engine's
	// write is the second of two. An earlier version of this comment claimed
	// sole ownership, and that claim is exactly why a second writer went
	// unexamined until it was found shipping a vendor URL's credential inside
	// the signed bundle (c1b397c). Redaction is now a gate on the write in
	// core/bundle rather than a habit of whichever function built the
	// document.
	b.makeLockReproducible()

	// install.Plan/Apply already
	// enforce this same contract (core/install/bundle.go) and would reject a
	// malformed lock, so this is the last and only line of defence against
	// ever shipping one — see defect 4 in the implementation notes. Failing the
	// build now, with a precise diagnostic, beats a mysterious rejection at
	// install time, possibly on the far side of the air gap from anyone who
	// could easily debug it.
	if err := lock.Validate(b.lockDoc); err != nil {
		return classify(err, dferr.Usage, "engine: validate lock")
	}

	lockDigest, err := saveLock(b.bundleDir, b.lockDoc)
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "engine: write lock.json")
	}

	willSign := b.signer != nil
	target := manifest.Target{
		DistroID: b.snap.Snapshot.Target.DistroID, VersionID: b.snap.Snapshot.Target.VersionID,
		Codename: b.snap.Snapshot.Target.Codename, Arch: b.effectiveArch(),
	}
	repoDigests := b.repositoryDigests()
	tool := version.Tool()
	bundleID := manifest.NewBundleID(lockDigest, b.createdAt)

	readmeManifest := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion, BundleID: bundleID, CreatedAt: b.createdAt,
		FormatVersion: manifest.CurrentFormatVersion, Tool: tool, SnapshotDigest: b.snap.Digest,
		LockDigest: lockDigest, Repository: repoDigests, Target: target, EvidenceRef: evidence.FileName,
	}
	readme := readmeText(readmeManifest, b.lockDoc, willSign)
	if err := os.WriteFile(filepath.Join(b.bundleDir, bundle.ReadmeFile), []byte(readme), 0o644); err != nil {
		return dferr.Wrap(dferr.Environment, err, "engine: write README.txt")
	}

	// sbom.cdx.json (defect 5): generated from the FINAL lock and the same
	// bundle id the README above and the real manifest below both use, only
	// when the request asked for one. See writeSBOM's own doc comment
	// (sbom.go) for why this is written directly here rather than through
	// bundle.Input.SBOM.
	var sbomRef string
	if b.req.Options.SBOM {
		sbomRef, err = b.writeSBOM(bundleID)
		if err != nil {
			return err
		}
	}

	// See "Ordering the signing event" above for why this happens here,
	// before evidence.json is written, rather than after the real Sign call
	// near the end of this method.
	if willSign {
		b.emit(evidence.TypeManifestSigned, "manifest signed", map[string]any{
			"signer_kind": b.signer.Kind(), "key_id": b.signer.KeyID(),
		})
	}

	// Built by hand, not via b.collector.Document() (which stamps
	// time.Now() for CreatedAt): see evidence.go's doc comment on
	// emit/warn for why every event this package sends already carries
	// b.createdAt as its TS, and why the document header gets the same
	// treatment here instead of asking the collector for its own idea of
	// "now".
	//
	// Nor via evidence.NewFileSink, whose doc comment names this exact file
	// as one of its two intended uses ("the evidence.json a bundle assembler
	// is about to fold into the manifest's file list"). That was reviewed
	// and deliberately not taken, for three reasons, each of which is on its
	// own sufficient:
	//
	//  1. fileSink.write calls Collector.Document(), which stamps
	//     time.Now() into CreatedAt — precisely the leak this package
	//     already avoids one paragraph above. A bundle built twice under a
	//     fixed SOURCE_DATE_EPOCH would get two different evidence.json
	//     headers, hence two different manifests and two different
	//     signatures. core/evidence's api.go is frozen and has no
	//     clock-injection seam, so this is not fixable from the caller.
	//  2. It encodes with encoding/json, not core/canonical. Everything in a
	//     bundle whose bytes are hashed goes through canonical (JCS)
	//     encoding, and evidence.json is hashed: manifest.Build covers it.
	//     NewFileSink's own doc says canonical encoding "is not required
	//     here the way it is for the manifest" — true of an install's local
	//     log, not of a file inside the signed tree.
	//  3. A sink writes on Close, which is the caller's lifecycle, not this
	//     method's ordering rule. evidence.json must land after the last
	//     event and before manifest.Build walks the tree — a window a few
	//     lines wide, enforced right here. Handing that timing to a Close
	//     somewhere else is how it silently stops holding.
	//
	// So NewFileSink stays unused by the engine on purpose. Its contract
	// about an unswallowed Close error is honoured here in the only form
	// that matters: the os.WriteFile below returns its error and fails the
	// build.
	evBytes, err := canonical.MarshalIndent(evidence.Document{
		Schema:    evidence.SchemaVersion,
		CreatedAt: b.createdAt,
		Context:   b.evidenceCtx,
		Events:    b.collector.Events(),
	})
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "engine: encode evidence")
	}
	if err := os.WriteFile(filepath.Join(b.bundleDir, evidence.FileName), evBytes, 0o644); err != nil {
		return dferr.Wrap(dferr.Environment, err, "engine: write evidence.json")
	}

	m, err := buildManifest(ctx, manifest.BuildInput{
		Dir: b.bundleDir, SnapshotDigest: b.snap.Digest, LockDigest: lockDigest, Repository: repoDigests,
		Target: target, Tool: tool, CreatedAt: b.createdAt, EvidenceRef: evidence.FileName,
		SBOMRef: sbomRef,
	})
	if err != nil {
		return classify(err, dferr.Environment, "engine: build manifest")
	}
	if err := saveManifest(b.bundleDir, m); err != nil {
		return dferr.Wrap(dferr.Environment, err, "engine: save manifest")
	}
	b.manifestDoc = m

	if !willSign {
		return nil
	}

	canon, err := canonicalManifest(m)
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "engine: canonicalise manifest")
	}
	sig, err := b.signer.Sign(ctx, manifest.SignPurpose, canon)
	if err != nil {
		// The README and the manifest just written both committed to "this
		// bundle is signed" (willSign was true when they were rendered), and
		// evidence.json above already recorded the manifest.signed event on
		// that same assumption. A signer that was successfully constructed
		// but fails at the moment of signing is rare, and shipping a bundle
		// whose README and evidence both claim a signature that does not
		// exist is worse — so this always fails the whole build and removes
		// the bundle directory, regardless of Required. That is stricter
		// than the frozen contract technically requires (it only mandates
		// failure for Required-with-no-signer).
		_ = os.RemoveAll(b.bundleDir)
		// classify, not Wrap: core/sign classifies its own failures, and
		// dferr.ClassOf/HintOf both resolve the OUTERMOST *dferr.Error, so an
		// unconditional Wrap here silently overrode every one of them. An
		// operator pressing Esc at the gpg pinentry prompt returns Usage
		// (core/sign/gpg.go's "operation cancelled at the pinentry prompt"),
		// and the plugin signer's protocol errors are Usage too
		// (core/sign/plugin.go) — reporting either as Environment tells the
		// operator "this machine cannot do the job" when the truth is "you
		// cancelled" or "your plugin is misbehaving", and it threw away the
		// remedy hints core/sign attaches ("import or generate the signing
		// key…", "start gpg-agent…") along with the class. Environment
		// remains the default for a signer that returns a bare, unclassified
		// error. The neighbouring Wrap calls in this method are correct as
		// they stand: they wrap os/json errors, which carry no class of their
		// own.
		return classify(err, dferr.Environment, "engine: sign manifest")
	}
	// The signature block's own timestamp joins the single-clock chain here.
	//
	// Every Signer stamps CreatedAt with core/sign's nowStamp(), which is
	// canonical.Time(time.Now()) — the real wall clock, unconditionally, in
	// all three implementations (ed25519.go, gpg.go, plugin.go). core/sign
	// is a frozen-API package this package does not own and it has no
	// clock-injection seam, so a build under SOURCE_DATE_EPOCH=1700000000
	// produced a signature whose created_at was the actual time of day.
	// Measured: identical manifest bytes signed twice with the same key gave
	// byte-identical ed25519 signature bytes but two different
	// debark.manifest.sig files, which means a signed bundle was never
	// byte-identical across two runs no matter what else was fixed.
	//
	// Overwriting is safe, and is not a way of forging when the signing
	// happened: CreatedAt is not part of what was signed. The signed message
	// is sign.SigningInput(purpose, canonicalManifest) — purpose, a NUL, then
	// the manifest bytes — and no verifier reads Signature.CreatedAt at all
	// (core/verify never touches it). What it records is "when the bundle
	// this signature covers was built", and this build has exactly one answer
	// to that question, b.createdAt: the same value already in
	// manifest.CreatedAt, lock.CreatedAt, repo/Release's Date and every
	// evidence event's TS (see clock.go's effectiveCreatedAt — "the ONLY
	// function that decides what now means"). Writing anything else here made
	// the signature file disagree with the manifest it signs about the same
	// instant.
	//
	// The engine-side fix is deliberate rather than a workaround: core/sign
	// would otherwise need a per-call timestamp on the frozen Signer.Sign
	// signature, or a package-level clock var, to say the same thing. See
	// the implementation notes for what a core/sign-side fix would look like.
	sig.CreatedAt = b.createdAt
	if err := saveManifestSignature(b.bundleDir, &manifest.SignatureFile{
		Schema: manifest.SignatureSchemaVersion, ManifestSHA256: canonical.DigestBytes(canon),
		Signatures: []manifest.Signature{sig},
	}); err != nil {
		_ = os.RemoveAll(b.bundleDir)
		return dferr.Wrap(dferr.Environment, err, "engine: save signature")
	}
	b.signed = true
	return nil
}
