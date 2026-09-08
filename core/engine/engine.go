package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/bundle"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/policy"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/store"
)

// engineImpl is the concrete Engine. It holds only the caller's Deps: nothing
// about one in-flight Build call is stored here, so one Engine is safe to use
// for several Build calls, sequentially or concurrently.
type engineImpl struct {
	deps Deps
}

func (e *engineImpl) Build(ctx context.Context, req buildjob.BuildRequest) (*buildjob.BuildResult, error) {
	// startedAt is always the real wall clock: it is used for exactly one
	// thing, BuildResult.Stats.DurationSeconds (result.go), which reports
	// how long this process actually took and is never written into a
	// hashed or signed artefact — SOURCE_DATE_EPOCH has no business
	// changing it. createdAt is the reproducibility-sensitive timestamp;
	// effectiveCreatedAt (clock.go) is the single place that decides its
	// value, honouring SOURCE_DATE_EPOCH when set. Computing the two
	// separately, right here, is what keeps them from ever being confused
	// for each other later.
	startedAt := timeNow()
	createdAt, err := effectiveCreatedAt()
	if err != nil {
		return nil, err
	}
	b := &build{eng: e, req: req, startedAt: startedAt, createdAt: createdAt}
	return b.run(ctx)
}

// build holds everything specific to one Build call. A fresh one is created
// per call, which is what makes concurrent Build calls on the same Engine
// safe despite the amount of mutable state a build accumulates.
type build struct {
	eng *engineImpl
	req buildjob.BuildRequest

	startedAt time.Time
	// createdAt is the single canonical timestamp threaded through every
	// artefact in the bundle (lock, manifest, Release, the evidence
	// document and every event's TS — see evidence.go's emit/warn), so that
	// fixing the clock (SOURCE_DATE_EPOCH in production, a fixed timeNow in
	// a test; see clock.go's effectiveCreatedAt) makes the whole run
	// reproducible (principle 2). It is computed exactly once, in
	// engineImpl.Build, and never reassigned. Nothing below this struct
	// definition calls time.Now, timeNow or effectiveCreatedAt directly for
	// anything that ends up in an artefact — every artefact-facing
	// timestamp in this package reads b.createdAt instead.
	createdAt string

	// Resolved collaborators: Deps values when set, defaults otherwise. See
	// resolveDeps in defaults.go.
	st     store.Store
	repoWr repository.Writer
	pol    policy.Evaluator
	signer sign.Signer
	// signerOwned is true when this build constructed signer itself (from
	// Output.Sign.SignerRef) rather than receiving it via Deps. Only an
	// owned signer is Close()d: Deps.Signer is caller-owned and may be reused
	// across many Build calls, so closing it here would break the next call.
	signerOwned bool
	selfPath    string

	// sink is what every pipeline stage emits to: the caller's Events (or
	// evidence.Discard{}) fanned out together with collector, an in-memory
	// copy this build keeps so it can write evidence.json itself once the
	// bundle directory exists (see the ordering-rule comment on run).
	collector *evidence.Collector
	sink      evidence.Sink
	// evidenceCtx is exactly the map handed to evidence.NewCollector when
	// collector was built (resolveDeps, in defaults.go). finalizeBundle
	// reuses it to construct evidence.json's Document by hand instead of
	// calling collector.Document() (which stamps time.Now() for the
	// document's own CreatedAt — see evidence.go's doc comment for why that
	// call is avoided everywhere in this package).
	evidenceCtx map[string]any

	approvedKeys []string

	// workRoot is a temporary directory for everything that must not end up
	// in the bundle: apt's archives dir, its private-root work dir, the
	// external-input staging repo, the closed-world work dir. Always removed
	// before Build returns.
	workRoot string

	snap *snapshot.Archive

	// Expanded, deduplicated inputs (packages.txt / --list expansion and
	// --local-dir scanning already folded in). See inputs.go.
	packages []string
	urls     []buildjob.URLInput
	files    []string

	externalRepoDir string
	externalNames   []string
	// externalOrigins records where each staged external .deb came from,
	// keyed by the base name it was staged under — the same name the
	// external staging repo indexes it by, and therefore the same name the
	// backend reports back in Selection.Filename. Written by stageExternals,
	// read once by attributeExternalOrigins after the backend has resolved.
	// See attributeExternalOrigins (inputs.go) for why the engine has to be
	// the one to remember this.
	externalOrigins map[string]externalOrigin
	// fetchFailures collects both URLs that failed to download and local
	// files that could not be read, each with the reason it failed. The
	// validated prototype reports both kinds in one bucket
	// (download-packages.sh's FETCH_FAILED) and BuildResult has no second
	// field for the local-file case; this follows the prototype rather than
	// inventing a place to put local failures.
	//
	// BuildResult's fetch_failed[] and fetch_failures[] are BOTH derived
	// from this one slice, in buildResult. That is what makes their
	// one-to-one, same-order correspondence a property of the code instead
	// of a promise in a doc comment: two independently appended lists would
	// drift the first time a branch remembered one and forgot the other.
	fetchFailures []buildjob.FetchFailure

	backend             apt.Backend
	backendCaps         apt.Capabilities
	effectiveRecommends bool

	plan *resolve.Plan

	downloadedBytes int64
	totalBytes      int64

	// policyWarnings holds non-deny findings, folded into the lock's
	// Warnings when the initial lock is built. A deny finding aborts the
	// build before that point (see evaluatePolicy) and never reaches here.
	policyWarnings []lock.Warning

	lockDoc *lock.Lock

	bundleDir string
	assembled *bundle.Result

	closedWorldFailed bool

	manifestDoc *manifest.Manifest
	signed      bool

	resultBundlePath string
}

// run is the whole pipeline. Steps are ordered exactly as written; see the
// block comment above finalizeBundle in finalize.go for the one ordering
// rule that is load-bearing (the manifest must be the last thing written, and
// must cover the lock's final rewrite, the final README and evidence.json).
func (b *build) run(ctx context.Context) (*buildjob.BuildResult, error) {
	if err := b.resolveDeps(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if b.signerOwned && b.signer != nil {
			_ = b.signer.Close()
		}
	}()

	workRoot, err := mkdirTemp("", "debark-build-*")
	if err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "engine: create work directory")
	}
	b.workRoot = workRoot
	// Best-effort teardown of the scratch tree. The build result is already
	// decided by the time this runs, and a failure to remove a temp directory
	// is not a build failure -- turning it into one would change a green build
	// into a red one over a stale file handle. Discarded explicitly.
	defer func() { _ = os.RemoveAll(b.workRoot) }()

	if err := b.loadSnapshot(ctx); err != nil {
		return nil, err
	}
	// Archive.Close only releases the temp directory the snapshot was extracted
	// into (core/snapshot/api.go); nothing is written through it, so there is
	// no data to lose and nothing the build can do about a failure here.
	defer func() { _ = b.snap.Close() }()

	if err := b.expandAndStageInputs(ctx); err != nil {
		return nil, err
	}

	if err := b.selectBackend(ctx); err != nil {
		return nil, err
	}

	if err := b.resolveWithBackend(ctx); err != nil {
		return nil, err
	}

	if err := b.ingestPlan(ctx); err != nil {
		return nil, err
	}

	// A policy deny stops the build before anything is written to
	// Output.Path: "no bundle was produced at all" (Engine.Build's own
	// contract) is still literally true at this point.
	if err := b.evaluatePolicy(ctx); err != nil {
		return nil, err
	}

	b.buildInitialLock()

	if err := b.assemble(ctx); err != nil {
		return nil, err
	}

	b.runDoctor(ctx)
	b.runClosedWorld(ctx)
	b.foldUnsignedWarning()

	if err := b.finalizeBundle(ctx); err != nil {
		return nil, err
	}

	if b.req.Output.Format == buildjob.FormatTar {
		if err := exportTar(ctx, b.bundleDir, b.req.Output.Path); err != nil {
			return nil, classify(err, dferr.Environment, "engine: export tar")
		}
		b.resultBundlePath = b.req.Output.Path
	} else {
		b.resultBundlePath = b.bundleDir
	}

	return b.buildResult(), nil
}

// classify returns err unchanged when it already carries a dferr class
// (every real collaborator is expected to return one), and wraps it with def
// otherwise. It exists because the fakes used in this package's own tests,
// and any collaborator that has not been fully implemented yet, may return a
// plain error.
func classify(err error, def dferr.Class, format string, args ...any) error {
	if err == nil {
		return nil
	}
	var e *dferr.Error
	if errors.As(err, &e) {
		return err
	}
	return dferr.Wrap(def, err, format, args...)
}

// effectiveArch is the snapshot's target architecture unless the request
// overrides it.
func (b *build) effectiveArch() string {
	if b.req.Options.ArchOverride != "" {
		return b.req.Options.ArchOverride
	}
	return b.snap.Snapshot.Target.Arch
}

// assemblyDir is where the bundle is actually assembled on disk. For
// directory output it is Output.Path itself. For tar output it is a sibling
// working directory: bundle.Assemble needs a real, persistent directory to
// compare against on the next incremental run (its own doc: "Dir ... may
// already contain a previous bundle, which is what makes runs incremental"),
// and a single tar file cannot serve as that directory, so re-running
// `--tar` against the same Output.Path stays incremental against this
// side-directory exactly the way the prototype's plain directory output
// does, and ExportTar produces the requested tar file from it as a final,
// derived step. This is a design choice the frozen bundle contract leaves
// open, not a value it dictates.
func (b *build) assemblyDir() string {
	if b.req.Output.Format == buildjob.FormatTar {
		return b.req.Output.Path + ".d"
	}
	return b.req.Output.Path
}

// repoArches is the sorted, deduplicated architecture list written into
// Release.Architectures: the effective target arch plus every foreign arch
// the snapshot records.
func (b *build) repoArches() []string {
	seen := map[string]bool{b.effectiveArch(): true}
	archs := []string{b.effectiveArch()}
	for _, fa := range b.snap.Snapshot.Target.ForeignArchs {
		if !seen[fa] {
			seen[fa] = true
			archs = append(archs, fa)
		}
	}
	sortedStrings(archs)
	return archs
}

// repositoryDigests copies the assembler's repository.Result into the
// manifest.Repository shape once bundle.Assemble has run.
func (b *build) repositoryDigests() manifest.Repository {
	if b.assembled == nil || b.assembled.Repo == nil {
		return manifest.Repository{}
	}
	r := b.assembled.Repo
	return manifest.Repository{
		PackagesSHA256:   r.PackagesSHA256,
		PackagesGzSHA256: r.PackagesGzSHA256,
		ReleaseSHA256:    r.ReleaseSHA256,
		PackageCount:     r.PackageCount,
		PoolBytes:        r.PoolBytes,
	}
}

// bundleRepoPath is the finished bundle's repo/ directory, as an absolute
// path under bundleDir. filepath.Join keeps this correct on Windows dev
// machines even though bundle.RepoDir is always forward-slashed.
//
// It said "absolute" and was not: bundleDir comes from --out, whose DEFAULT
// is the relative "bundle", and filepath.Join of a relative path is relative.
// Its one consumer is ClosedWorldInput.BundleRepoDir, which the container
// backend turns into a bind-mount source and therefore requires to be
// absolute -- so every `build --backend container` that did not spell --out
// absolutely failed its closed-world check with "is not an absolute path",
// exit 5, on a bundle that was otherwise complete and correct. The local
// backend hid the same defect by calling filepath.Abs itself.
//
// Absolute only here, never in what the build reports: BuildResult.BundlePath
// stays exactly as the operator wrote it.
func (b *build) bundleRepoPath() string {
	p := filepath.Join(b.bundleDir, filepath.FromSlash(bundle.RepoDir))
	abs, err := filepath.Abs(p)
	if err != nil {
		// Abs only fails when the working directory cannot be determined,
		// which is not a reason to lose the path: the backends' own
		// diagnosis of a relative path is better than a swallowed error.
		return p
	}
	return abs
}
