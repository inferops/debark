package bundle

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pault.ag/go/debian/deb"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/store"
	"github.com/inferops/debark/core/version"
)

// assemble is the real implementation behind the package-level Assemble.
//
// It calls three packages this package does not own - snapshot.Digest,
// lock.Save and manifest.Build - through their frozen api.go signatures.
// Everything before those calls (pool materialisation, incremental
// accounting, pruning, repository generation) is exercised directly by this
// package's own tests without needing any of the three to be implemented;
// see assemble_test.go.
func assemble(ctx context.Context, in Input) (*Result, error) {
	if err := validateAssembleInput(in); err != nil {
		return nil, err
	}

	repoDir := filepath.Join(in.Dir, RepoDir)
	poolDir := filepath.Join(in.Dir, PoolDir)
	if err := os.MkdirAll(poolDir, 0o755); err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "bundle: create %s", poolDir)
	}

	selections := append([]resolve.Selection(nil), in.Plan.Selections...)
	resolve.SortSelections(selections)

	outcome, err := materialiseSelections(ctx, in.Store, repoDir, selections)
	if err != nil {
		return nil, err
	}

	var removed []string
	if in.Prune {
		removed, err = pruneExistingPool(in.Store, repoDir, selections)
		if err != nil {
			return nil, err
		}
	}

	// After prune, so nothing already reported in last-run-removed.txt is
	// counted twice, and unconditionally, because the gap this reports is not
	// a pruning artefact: a package an earlier request named and this one
	// does not is left in the pool by every additive run too. See
	// unreferenced.go.
	unreferenced, err := unreferencedPoolFiles(repoDir, outcome.poolFiles)
	if err != nil {
		return nil, err
	}

	releaseFields := in.Release
	if releaseFields.Date == "" {
		d, err := formatReleaseDate(in.CreatedAt)
		if err != nil {
			return nil, dferr.New(dferr.Usage, "bundle: Assemble: Input.CreatedAt: %v", err)
		}
		releaseFields.Date = d
	}

	repoResult, err := repository.NewWriter().Write(ctx, repository.Input{
		Dir:      repoDir,
		Files:    outcome.poolFiles,
		Release:  releaseFields,
		Compress: true,
	})
	if err != nil {
		return nil, err
	}

	if err := writeSnapshotJSON(in.Dir, in.Snapshot); err != nil {
		return nil, err
	}

	// Written HERE, before manifest.Build below, not down with its
	// last-run-added.txt / last-run-removed.txt siblings, which are written
	// after it. Those two get away with it only because the engine builds a
	// second, final manifest over the finished tree (core/engine/finalize.go);
	// a bundle produced by Assemble alone has them on disk and absent from
	// its own manifest, which is precisely what verify reports as
	// file-unexpected. A report about what the signature covers must itself
	// be covered by it, so this one is not allowed to inherit that wrinkle.
	if err := writeLines(filepath.Join(in.Dir, UnreferencedFile), unreferenced); err != nil {
		return nil, err
	}

	l := buildLock(in.Lock, selections, in.Plan.Install, outcome.stats, unreferencedWarning(unreferenced, selections))
	// The last statement between "a lock document exists in memory" and "a
	// lock document exists on disk, hashed into the manifest and covered by
	// the signature". See redactLockOrigins for why the redaction is here and
	// not in buildLock.
	redactLockOrigins(l)
	lockDigest, err := lock.Save(in.Dir, l)
	if err != nil {
		return nil, err
	}

	var sbomRef, evidenceRef string
	if len(in.SBOM) > 0 {
		if err := os.WriteFile(filepath.Join(in.Dir, SBOMFile), in.SBOM, 0o644); err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "bundle: write %s", SBOMFile)
		}
		sbomRef = SBOMFile
	}
	if len(in.Evidence) > 0 {
		if err := os.WriteFile(filepath.Join(in.Dir, evidence.FileName), in.Evidence, 0o644); err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "bundle: write %s", evidence.FileName)
		}
		evidenceRef = evidence.FileName
	}
	if in.EmbedBinary != "" {
		if err := embedBinary(in.Dir, in.EmbedBinary, l.Target.Arch); err != nil {
			return nil, err
		}
	}

	var snapshotDigest string
	if in.Snapshot != nil {
		snapshotDigest, err = snapshot.Digest(in.Snapshot)
		if err != nil {
			return nil, err
		}
	}

	m, err := manifest.Build(ctx, manifest.BuildInput{
		Dir:            in.Dir,
		SnapshotDigest: snapshotDigest,
		LockDigest:     lockDigest,
		Repository: manifest.Repository{
			PackagesSHA256:   repoResult.PackagesSHA256,
			PackagesGzSHA256: repoResult.PackagesGzSHA256,
			ReleaseSHA256:    repoResult.ReleaseSHA256,
			PackageCount:     repoResult.PackageCount,
			PoolBytes:        repoResult.PoolBytes,
		},
		Target:      manifest.Target{DistroID: l.Target.DistroID, VersionID: l.Target.VersionID, Codename: l.Target.Codename, Arch: l.Target.Arch},
		Tool:        version.Tool(),
		CreatedAt:   in.CreatedAt,
		SBOMRef:     sbomRef,
		EvidenceRef: evidenceRef,
	})
	if err != nil {
		return nil, err
	}
	if err := manifest.Save(in.Dir, m); err != nil {
		return nil, err
	}

	// Nothing is signed yet - signing is the caller's step, after Assemble
	// returns - so this can only ever render the unsigned form. A caller
	// that signs the manifest afterwards should call bundle.ReadmeText again
	// with signed=true and overwrite README.txt itself.
	readme := ReadmeText(m, l, false)
	if err := os.WriteFile(filepath.Join(in.Dir, ReadmeFile), []byte(readme), 0o644); err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "bundle: write %s", ReadmeFile)
	}

	if err := writeLines(filepath.Join(in.Dir, AddedFile), outcome.added); err != nil {
		return nil, err
	}
	if err := writeLines(filepath.Join(in.Dir, RemovedFile), removed); err != nil {
		return nil, err
	}

	return &Result{
		Dir:          in.Dir,
		Manifest:     m,
		Lock:         l,
		Repo:         repoResult,
		Stats:        l.Stats,
		Added:        outcome.added,
		Removed:      removed,
		Unreferenced: unreferenced,
	}, nil
}

func validateAssembleInput(in Input) error {
	if in.Dir == "" {
		return dferr.New(dferr.Usage, "bundle: Assemble: Input.Dir is required")
	}
	if in.Plan == nil {
		return dferr.New(dferr.Usage, "bundle: Assemble: Input.Plan is required")
	}
	if in.Store == nil {
		return dferr.New(dferr.Usage, "bundle: Assemble: Input.Store is required")
	}
	if in.Lock == nil {
		return dferr.New(dferr.Usage, "bundle: Assemble: Input.Lock is required")
	}
	if _, err := canonical.ParseTime(in.CreatedAt); err != nil {
		return dferr.New(dferr.Usage, "bundle: Assemble: Input.CreatedAt: %v", err)
	}
	return nil
}

func formatReleaseDate(createdAt string) (string, error) {
	t, err := canonical.ParseTime(createdAt)
	if err != nil {
		return "", err
	}
	return t.UTC().Format("Mon, 02 Jan 2006 15:04:05 UTC"), nil
}

// materialiseOutcome is what one pass over the plan's selections produced.
type materialiseOutcome struct {
	poolFiles []repository.PoolFile
	stats     lock.Stats
	added     []string // pool-relative paths materialised (fresh or refreshed) this run
	unchanged []string // pool-relative paths already correct, left untouched
}

// materialiseSelections places every selection's file at its pool path,
// leaving a file already present with the right digest untouched (the
// "unchanged" case). A selection not yet in the store is ingested from its
// StagedPath first; store.Materialise is what actually places the bytes,
// hardlinking when possible.
func materialiseSelections(ctx context.Context, st store.Store, repoDir string, selections []resolve.Selection) (materialiseOutcome, error) {
	var out materialiseOutcome
	claimed := make(map[string]resolve.Selection, len(selections))
	for _, sel := range selections {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if sel.Name == "" || sel.Filename == "" {
			return out, dferr.New(dferr.Usage, "bundle: selection %q has no name or filename", sel.Name)
		}
		// Both fields come from the .deb's own control data, which is
		// attacker-controlled for a vendor .deb. PoolPath returns "" rather
		// than an escaping path; refuse loudly instead of writing anywhere.
		relPath := repository.PoolPath(sel.Name, sel.Filename)
		if relPath == "" {
			return out, dferr.New(dferr.Usage,
				"bundle: refusing package with an unsafe name or filename: name %q, filename %q",
				sel.Name, sel.Filename)
		}
		dest := filepath.Join(repoDir, filepath.FromSlash(relPath))
		// Defence in depth: even with a validated relPath, confirm the
		// resolved destination is still inside the pool before writing.
		if !isInside(filepath.Join(repoDir, "pool"), dest) {
			return out, dferr.New(dferr.Usage,
				"bundle: refusing to write %s outside the pool", relPath)
		}
		// Two selections mapping to one pool path is silent corruption, not a
		// harmless collision: the later one overwrites the earlier, and the
		// lock entry for the earlier then describes bytes that are not on
		// disk anywhere. lock.Validate cannot see it (its uniqueness key is
		// name/arch/version, which differ) and verify cannot either, because
		// the manifest is built from the pool after the overwrite.
		if prior, dup := claimed[relPath]; dup {
			return out, dferr.New(dferr.Usage,
				"bundle: %s %s/%s and %s %s/%s both claim pool path %s",
				prior.Name, prior.Arch, prior.Version, sel.Name, sel.Arch, sel.Version, relPath)
		}
		claimed[relPath] = sel

		out.stats.Bytes += sel.Size
		out.poolFiles = append(out.poolFiles, repository.PoolFile{Path: relPath})

		if fileDigestMatches(dest, sel.SHA256) {
			out.stats.Unchanged++
			out.unchanged = append(out.unchanged, relPath)
			continue
		}

		alreadyInStore := sel.SHA256 != "" && st.Has(sel.SHA256)
		useDigest, err := ensureInStore(ctx, st, sel)
		if err != nil {
			return out, err
		}
		if err := st.Materialise(useDigest, dest); err != nil {
			// Keep the store's own class: a content mismatch is a
			// Verification failure, not an environment one.
			return out, dferr.Wrap(dferr.ClassOf(err), err, "bundle: materialise %s", relPath)
		}
		// Check what actually landed, independently of whatever Store
		// implementation placed it. This is the last point at which a
		// substitution is still visible: the Packages index, the manifest and
		// the operator's signature are all re-derived from this file, so from
		// here on a wrong .deb produces a bundle that agrees with itself and
		// verifies clean on the target.
		if !fileDigestMatches(dest, useDigest) {
			return out, dferr.New(dferr.Verification,
				"bundle: %s: pool file does not match %s after materialising", relPath, useDigest)
		}
		out.stats.Added++
		out.added = append(out.added, relPath)
		if !alreadyInStore {
			// Best-effort accounting: bytes that were not already
			// content-addressed in the store had to come from somewhere
			// outside it this run. True network-vs-cache accounting belongs
			// to the fetch layer, which is upstream of this package.
			out.stats.DownloadedBytes += sel.Size
		}
	}
	sort.Strings(out.added)
	sort.Strings(out.unchanged)
	return out, nil
}

func fileDigestMatches(path, wantSHA256 string) bool {
	if wantSHA256 == "" {
		return false
	}
	got, _, err := digest.SHA256File(path)
	if err != nil {
		return false
	}
	return digest.Equal(got, wantSHA256)
}

// ensureInStore makes sure sel's content is in the store and returns the
// digest to materialise from. A selection may arrive already in the store
// (sel.SHA256 set and present) or only staged on disk (sel.StagedPath); the
// Input contract requires one or the other.
//
// What it deliberately does not do is treat an object's presence at an
// address as evidence about its content. The store is a content-addressed
// tree of ordinary files, so anything able to write one file into it can park
// its own bytes behind a digest apt vouched for; taking the Has shortcut
// would then hardlink those bytes into the pool without ever opening them. So
// a staged copy is always re-ingested - PutFile hashes what it stores - and
// an object with no staged copy behind it is read back in full first.
func ensureInStore(ctx context.Context, st store.Store, sel resolve.Selection) (string, error) {
	if sel.StagedPath == "" {
		if sel.SHA256 == "" {
			return "", dferr.New(dferr.Usage, "bundle: %s: neither a digest nor a staged path", sel.Name)
		}
		if !st.Has(sel.SHA256) {
			return "", dferr.New(dferr.Environment, "bundle: %s (%s): not in the store and no staged path", sel.Name, sel.SHA256)
		}
		if err := verifyStoreObject(st, sel.SHA256); err != nil {
			return "", dferr.Wrap(dferr.Verification, err, "bundle: %s", sel.Name)
		}
		return sel.SHA256, nil
	}
	got, _, err := st.PutFile(ctx, sel.StagedPath, false)
	if err != nil {
		return "", dferr.Wrap(dferr.ClassOf(err), err, "bundle: ingest %s into the store", sel.Name)
	}
	if sel.SHA256 != "" && !digest.Equal(got, sel.SHA256) {
		return "", dferr.New(dferr.Verification, "bundle: %s: staged file digest %s does not match plan digest %s", sel.Name, got, sel.SHA256)
	}
	return got, nil
}

// verifyStoreObject reads an object back out of the store and checks it
// against the address it is held under. Store.Open streams, so this is one
// pass and constant memory. The comparison is repeated here rather than left
// to the Store: Assemble takes the interface, and a security property may not
// depend on which implementation was passed in.
func verifyStoreObject(st store.Store, want string) error {
	rc, err := st.Open(want)
	if err != nil {
		return err
	}
	defer rc.Close()
	got, _, err := digest.SHA256Reader(rc)
	if err != nil {
		return err
	}
	if !digest.Equal(got, want) {
		return dferr.New(dferr.Verification,
			"store object %s holds content that hashes to %s", want, got)
	}
	return nil
}

// pruneExistingPool runs the pure Prune decision over everything currently
// in the pool - this run's selections plus whatever an earlier run left
// behind - and deletes what it returns.
func pruneExistingPool(st store.Store, repoDir string, selections []resolve.Selection) ([]string, error) {
	entries, err := collectPoolEntries(st, repoDir, selections)
	if err != nil {
		return nil, err
	}
	toRemove := Prune(entries)
	sort.Slice(toRemove, func(i, j int) bool { return toRemove[i].Path < toRemove[j].Path })

	removed := make([]string, 0, len(toRemove))
	for _, e := range toRemove {
		abs := filepath.Join(repoDir, filepath.FromSlash(e.Path))
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return nil, dferr.Wrap(dferr.Environment, err, "bundle: prune: remove %s", e.Path)
		}
		removed = append(removed, e.Path)
	}
	return removed, nil
}

// collectPoolEntries builds the pure Prune input from the pool directory as
// it exists on disk right now. Files that belong to the current plan are
// identified without reopening them, and marked LockReferenced: true - they
// are what the lock this run writes will name (buildLock derives
// lock.Packages from this same selections slice), so Prune must never
// remove them even if a numerically higher sibling is sitting in the pool
// from an earlier, non-pruning run (docs/experiments/E3-pin-fidelity.md).
// Anything else is a leftover from an earlier run and is inspected directly
// so pruning still works across runs. UserSupplied for a leftover is
// recovered from the store index recorded by whoever added it
// (store.Record); a leftover this store has no record of is treated as not
// user-supplied, since there is nothing else to go on.
func collectPoolEntries(st store.Store, repoDir string, selections []resolve.Selection) ([]PoolEntry, error) {
	bySelPath := make(map[string]resolve.Selection, len(selections))
	for _, sel := range selections {
		bySelPath[repository.PoolPath(sel.Name, sel.Filename)] = sel
	}

	userSuppliedByDigest := map[string]bool{}
	if idx, err := st.Index(); err == nil {
		for _, e := range idx.Entries {
			if e.UserSupplied {
				userSuppliedByDigest[e.Digest] = true
			}
		}
	}

	poolRoot := filepath.Join(repoDir, "pool")
	if _, err := os.Stat(poolRoot); os.IsNotExist(err) {
		return nil, nil
	}

	var entries []PoolEntry
	walkErr := filepath.WalkDir(poolRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".deb") {
			return nil
		}
		rel, rerr := filepath.Rel(repoDir, p)
		if rerr != nil {
			return rerr
		}
		relSlash := filepath.ToSlash(rel)

		if sel, ok := bySelPath[relSlash]; ok {
			entries = append(entries, PoolEntry{
				Name: sel.Name, Arch: sel.Arch, Version: sel.Version,
				Path: relSlash, UserSupplied: sel.UserSupplied, LockReferenced: true,
			})
			return nil
		}

		name, arch, ver, err := readDebIdentity(p)
		if err != nil {
			return dferr.Wrap(dferr.Usage, err, "bundle: prune: read control data of %s", relSlash)
		}
		userSupplied := false
		if sha, _, herr := digest.SHA256File(p); herr == nil {
			userSupplied = userSuppliedByDigest[sha]
		}
		entries = append(entries, PoolEntry{Name: name, Arch: arch, Version: ver, Path: relSlash, UserSupplied: userSupplied})
		return nil
	})
	if walkErr != nil {
		return nil, dferr.Wrap(dferr.Environment, walkErr, "bundle: prune: scan %s", poolRoot)
	}
	return entries, nil
}

func readDebIdentity(path string) (name, arch, version string, err error) {
	d, closer, err := deb.LoadFile(path)
	if err != nil {
		return "", "", "", err
	}
	// Read-only: the control data is already in memory by the time this runs,
	// so a failure to close the .deb cannot corrupt anything we return. The
	// discard is explicit rather than implicit.
	defer func() { _ = closer() }()
	p := d.Control.Paragraph
	return p.Values["Package"], p.Values["Architecture"], p.Values["Version"], nil
}

// buildLock fills in the parts of the lock Assemble owns (Packages, Install,
// Stats) onto the caller-supplied template, which already carries
// SchemaVersion, CreatedAt, SnapshotDigest, RequestDigest, Target, Resolver,
// ClosedWorld, Warnings and Unresolved.
//
// extra carries the warnings only assembly can know about - today just the
// unreferenced-pool one (unreferenced.go), which needs both the final pool
// contents and this run's selections, neither of which exists before
// Assemble runs. They are appended onto a COPY of the template's slice: the
// template belongs to the caller, and the engine reuses the same *lock.Lock
// across its own later folds, so appending in place could write through the
// shared backing array into a value this package does not own.
func buildLock(tmpl *lock.Lock, selections []resolve.Selection, planInstall []string, stats lock.Stats, extra ...*lock.Warning) *lock.Lock {
	l := *tmpl

	// Copied, not appended in place: l is a shallow copy, so l.Warnings still
	// shares tmpl's backing array, and tmpl belongs to the caller - the
	// engine hands in the same *lock.Lock it goes on folding its own warnings
	// into after Assemble returns. Untouched when there is nothing to add, so
	// a template with no warnings still marshals the field away (omitempty).
	for _, w := range extra {
		if w == nil {
			continue
		}
		warnings := make([]lock.Warning, 0, len(l.Warnings)+1)
		l.Warnings = append(append(warnings, l.Warnings...), *w)
	}

	l.Packages = make([]lock.Package, 0, len(selections))
	for _, sel := range selections {
		l.Packages = append(l.Packages, lock.Package{
			Name:                  sel.Name,
			Arch:                  sel.Arch,
			Version:               sel.Version,
			SourcePackage:         sel.SourcePackage,
			Filename:              repository.PoolPath(sel.Name, sel.Filename),
			Size:                  sel.Size,
			SHA256:                sel.SHA256,
			Origin:                sel.Origin,
			Reason:                sel.Reason,
			PublisherVerification: sel.PublisherVerification,
			Flags:                 append([]string(nil), sel.Flags...),
			Essential:             sel.Essential,
		})
	}
	sort.Slice(l.Packages, func(i, j int) bool {
		a, b := l.Packages[i], l.Packages[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Arch != b.Arch {
			return a.Arch < b.Arch
		}
		return a.Version < b.Version
	})

	install := append([]string(nil), planInstall...)
	sort.Strings(install)
	l.Install = install
	l.Stats = stats
	return &l
}

// redactLockOrigins strips any embedded credential from every
// packages[].origin.uri in l, in place, immediately before the document is
// written. It is the gate on this package's write path, not a step in
// buildLock's construction, and the placement is the whole point.
//
// # What it is fixing
//
// lock.Origin.URI had no writer at all until attributeExternalOrigins
// (core/engine/inputs.go) started recording the operator's vendor URL there,
// credential and all. core/engine/lockbuild.go redacts on the way into its
// own lock document (redactOrigin), but buildLock above then rebuilds
// l.Packages from the same selections and the template's redacted copy is
// discarded — so the assembler's copy is the one that reached disk, was
// hashed into debark.manifest.json, was covered by the signature, and
// crossed the air gap. Measured, not theorised: an e2e fixture found it and
// a by-hand build against a real binary reproduced it, with lock.json the
// only file in the whole bundle that carried the token.
//
// # Why here rather than in buildLock
//
// Making buildLock call redactOrigin too would restore an ENUMERATION of
// writers, and this wave has now watched that shape go stale three times —
// 034f709 (a fake that could not produce production's error shape), 0853f19
// (a fourth argv call site an enumeration of three had missed), and this.
// An enumeration is only correct until someone adds an entry to it, and
// nothing makes them notice.
//
// A gate on the write is not an enumeration. There is exactly one statement
// in this package that turns a *lock.Lock into lock.json, and everything in
// the document passes through it — packages this function built, packages a
// future revision builds differently, and packages a caller supplied on the
// template. The invariant it states is a property of the package ("nothing
// core/bundle writes carries a credential") rather than a habit of one
// function, so a second producer cannot silently opt out of it.
//
// It also covers a second on-disk writer this package does not own, for
// free. core/engine adopts the very object returned here as its own lock
// document (finalize.go's b.lockDoc = res.Lock) and saves it AGAIN after
// folding in the closed-world result — the engine's own comment calls itself
// "the only writer of lock.json", which is not true. Because the redaction
// mutates the returned document rather than a copy made for writing, the
// engine's second write, its in-process doctor run and its SBOM all see the
// redacted value without core/bundle reaching into another package's file.
//
// # The two fixes deliberately NOT taken here
//
// Redacting at the source — attributeExternalOrigins never putting a
// credential into Selection.Origin.URI in the first place — is one line and
// is genuinely narrower, and it is recommended as a follow-up. It is not
// taken here because that file belongs to another package, and because it is
// still an enumeration, just of producers rather than writers: the moment
// core/apt's originFor starts recording an archive URI (a private archive's
// sources.list line can carry userinfo), the source-side fix is one producer
// short and this gate is not.
//
// Making the field unable to hold an unredacted URL at all — a type in
// core/lock constructed only through the redactor — is the strongest form
// and is currently impossible without a wider refactor: core/fetch imports
// core/lock, so core/lock cannot import fetch.RedactURL back, and the only
// honest way to have the type is to move RedactURL into a leaf package that
// both can import. That is a change across several packages this package does
// not own, and duplicating the redaction rule into core/lock instead is the
// opposite of what commit 61b3e7b just did for the file-URI escaping rule
// ("one copy of the rule, not two"). Recorded as the right shape, not done
// by stealth.
//
// # Why the loss is acceptable
//
// fetch.RedactURL drops the whole query string, not just the credentialed
// parameter, because no vendor agrees on what the secret parameter is
// called. lock.Origin.URI's contract is "the archive URI or the vendor URL"
// — a provenance record answering "which host, which file", which scheme,
// host and path still answer. Suite, Component, ReleaseDigest and
// KeyFingerprint are untouched, and so is LocalPath, which is a path and not
// a URL. The redaction is idempotent, which determinism depends on: the
// engine marshals this same document a second time and must produce the same
// bytes.
func redactLockOrigins(l *lock.Lock) {
	if l == nil {
		return
	}
	for i := range l.Packages {
		l.Packages[i].Origin.URI = fetch.RedactURL(l.Packages[i].Origin.URI)
	}
}

func writeSnapshotJSON(bundleDir string, s *snapshot.Snapshot) error {
	if s == nil {
		return nil
	}
	b, err := canonical.MarshalIndent(s)
	if err != nil {
		return dferr.Wrap(dferr.Usage, err, "bundle: marshal snapshot")
	}
	if err := os.WriteFile(filepath.Join(bundleDir, SnapshotFile), b, 0o644); err != nil {
		return dferr.Wrap(dferr.Environment, err, "bundle: write %s", SnapshotFile)
	}
	return nil
}

func writeLines(path string, lines []string) error {
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	var b strings.Builder
	for _, l := range sorted {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return dferr.Wrap(dferr.Environment, err, "bundle: write %s", filepath.Base(path))
	}
	return nil
}

func embedBinary(bundleDir, srcPath, arch string) error {
	if arch == "" {
		arch = "unknown"
	}
	// arch is the lock's Target.Arch, which comes from the snapshot or from
	// --arch (Options.ArchOverride, not validated upstream). It is about to
	// become part of a file name, so anything that is not an architecture
	// tuple is a path fragment: "../../../../PWNED" writes a 0755 executable
	// four directories above the bundle root.
	if !plausibleArch(arch) {
		return dferr.New(dferr.Usage, "bundle: embed binary: %q is not an architecture", arch)
	}
	destDir := filepath.Join(bundleDir, BinDir)
	dest := filepath.Join(destDir, "debark-linux-"+arch)
	// Defence in depth, matching materialiseSelections: a validated arch
	// should already be containment-safe, so confirm it rather than assume it.
	if !isInside(destDir, dest) {
		return dferr.New(dferr.Usage, "bundle: embed binary: refusing to write outside %s", BinDir)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return dferr.Wrap(dferr.Environment, err, "bundle: embed binary: create %s", destDir)
	}
	if err := copyExecutable(srcPath, dest); err != nil {
		return dferr.Wrap(dferr.Environment, err, "bundle: embed binary")
	}
	return nil
}

// plausibleArch is a format check, not a support check: it accepts anything
// shaped like a dpkg architecture tuple. It is deliberately the same grammar
// as core/snapshot's plausibleArch (validate.go), duplicated rather than
// exported so the two packages stay independent - if one is ever loosened,
// loosen both, because a string that is acceptable in a snapshot and refused
// here (or the reverse) is a bug in itself.
func plausibleArch(a string) bool {
	if a == "" || len(a) > 32 {
		return false
	}
	for i, r := range a {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		case r == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

func copyExecutable(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// isInside reports whether path is within root after both are cleaned. It is
// the last check standing between attacker-controlled control-file fields and
// an arbitrary write on the builder host.
func isInside(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
