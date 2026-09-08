package engine

import (
	"context"
	"os"
	"path/filepath"

	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/store"
)

// selectBackend honours a caller-supplied Deps.Backend outright; otherwise it
// calls Deps.SelectBackend, defaulting that to apt.SelectBackend, exactly as
// Deps documents. Either way it emits backend.selected itself rather than
// trusting the selection path to have done so, since Deps.Backend skips
// selection entirely and a fake SelectBackend in a test has no obligation to
// emit anything — this way every build has exactly one such event.
func (b *build) selectBackend(ctx context.Context) error {
	if b.eng.deps.Backend != nil {
		b.backend = b.eng.deps.Backend
		b.emit(evidence.TypeBackendSelected, "backend supplied by caller",
			map[string]any{"backend": string(b.backend.Kind())})
		return nil
	}

	fn := b.eng.deps.SelectBackend
	if fn == nil {
		fn = selectBackendFn
	}
	backend, caps, err := fn(ctx, apt.Selection{
		Target:    b.snap.Snapshot.Target,
		Requested: lock.Backend(b.req.Options.Backend),
		Image:     b.req.Options.Image,
		Events:    b.sink,
		SelfPath:  b.selfPath,
	})
	if err != nil {
		return classify(err, dferr.Environment, "engine: select backend")
	}
	b.backend = backend
	b.backendCaps = caps
	b.emit(evidence.TypeBackendSelected, "backend selected", map[string]any{
		"backend": string(backend.Kind()), "apt_version": caps.APTVersion, "dpkg_version": caps.DpkgVersion,
		"image": caps.Image, "image_digest": caps.ImageDigest, "runtime": caps.Runtime,
	})
	return nil
}

// resolveWithBackend runs the actual resolution: update, optional
// full-upgrade, install --download-only for requested and external packages
// , all inside the backend's own private root. --approved-keys is
// loaded earlier (resolveDeps) so it can be threaded in here, honouring the
// the contract brief ordering note that it must reach ResolveInput.
func (b *build) resolveWithBackend(ctx context.Context) error {
	archivesDir := filepath.Join(b.workRoot, "archives")
	workDir := filepath.Join(b.workRoot, "resolve-work")
	if err := os.MkdirAll(archivesDir, 0o755); err != nil {
		return dferr.Wrap(dferr.Environment, err, "engine: create archives directory")
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return dferr.Wrap(dferr.Environment, err, "engine: create resolve work directory")
	}

	recommends := installRecommends(b.snap)
	if b.req.Options.Recommends != nil {
		recommends = *b.req.Options.Recommends
	}
	b.effectiveRecommends = recommends

	plan, err := b.backend.Resolve(ctx, apt.ResolveInput{
		Snapshot:         b.snap.Snapshot,
		SnapshotFilesDir: b.snap.FilesDir(),
		Packages:         b.packages,
		ExternalRepoDir:  b.externalRepoDir,
		ExternalNames:    b.externalNames,
		ArchivesDir:      archivesDir,
		WorkDir:          workDir,
		Options:          b.req.Options,
		Recommends:       recommends,
		Upgrades:         b.req.Options.Upgrades,
		PhasedPolicy:     b.snap.Snapshot.PhasedPolicyFor(),
		ApprovedKeys:     b.approvedKeys,
		DownloadOnly:     true,
	})
	if err != nil {
		return classify(err, dferr.Resolution, "engine: resolve")
	}
	resolve.SortSelections(plan.Selections)
	b.plan = plan
	// The backend answers with what apt chose; only the engine knows which
	// of those files it put in front of apt, and where each came from. Put
	// that back before anything reads the plan — evaluatePolicy is the first
	// reader, and allow_url_inputs cannot work without it. See
	// attributeExternalOrigins (inputs.go).
	b.attributeExternalOrigins()
	return nil
}

// ingestPlan copies every selected file into the content-addressed store,
// records its package identity, and tracks how many of the plan's bytes were
// actually downloaded this run versus already held — the two numbers
// Stats.Bytes and Stats.DownloadedBytes need. "Already held" is only checked
// when the backend already told us the file's digest (Selection.SHA256):
// apt-resolved and staged-repo selections should always carry one (apt
// verifies against Packages/InRelease hashes as it downloads), so this is
// exact in the real pipeline; a selection with no pre-known digest is simply
// counted as downloaded, which only affects a reporting number, never
// correctness.
func (b *build) ingestPlan(ctx context.Context) error {
	var downloaded, total int64
	for i := range b.plan.Selections {
		sel := &b.plan.Selections[i]
		if sel.StagedPath == "" {
			b.warn(evidence.TypeWarning, "selection with no staged file: "+sel.Name,
				map[string]any{"name": sel.Name, "arch": sel.Arch, "version": sel.Version})
			continue
		}
		alreadyHeld := sel.SHA256 != "" && b.st.Has(sel.SHA256)

		sum, size, err := b.st.PutFile(ctx, sel.StagedPath, true)
		if err != nil {
			return dferr.Wrap(dferr.Environment, err, "engine: store %s", sel.Filename)
		}
		sel.SHA256 = sum
		sel.Size = size
		// The staged copy is GONE: PutFile was called with moveOK=true, and
		// every one of its success paths consumes the source (rename into
		// the store on the fast path, copy-then-remove on the fallback). The
		// selection must stop advertising a file that no longer exists.
		//
		// This is a contract, not tidiness. bundle.Input.Plan documents that
		// "Selections must already carry StagedPath or be present in Store",
		// and core/bundle's ensureInStore now takes the caller at its word:
		// it re-ingests StagedPath whenever one is set, rather than
		// short-circuiting on st.Has(sel.SHA256) — deliberately, because
		// presence at an address is not evidence about content. So a
		// selection left pointing at the file this method just moved away
		// makes Assemble open a path that cannot be there, and the build
		// dies with "ingest <pkg> into the store: open <workroot>/archives/
		// <file>: no such file or directory". The bytes ARE in the store,
		// under sel.SHA256, which is exactly the other half of that "or".
		sel.StagedPath = ""

		if err := b.st.Record(store.Entry{
			Digest: sum, Name: sel.Name, Version: sel.Version, Arch: sel.Arch,
			Size: size, Filename: sel.Filename, AddedAt: b.createdAt, UserSupplied: sel.UserSupplied,
		}); err != nil {
			return dferr.Wrap(dferr.Environment, err, "engine: record %s in store index", sel.Filename)
		}

		total += size
		if !alreadyHeld {
			downloaded += size
		}
		// already_held is deliberately NOT recorded here, for exactly the
		// reason Stats.DownloadedBytes is no longer recorded in the lock
		// (see stripCacheDependentStats in reproducible.go): it is a fact
		// about this machine's persistent content store, not about the
		// request, and evidence.json is written INSIDE the bundle and
		// covered by the manifest the signature is made over. Recording it
		// made the same request, built twice on one machine, produce two
		// different evidence.json files, two different manifests and two
		// different signatures — the whole bundle non-reproducible, for
		// nothing but "we had built it before". The value is still used
		// above, where it belongs: BuildResult.Stats.DownloadedBytes, which
		// the CLI prints and no artefact records.
		b.emit(evidence.TypeStoreHit, "ingested "+sel.Filename, map[string]any{
			"name": sel.Name, "version": sel.Version, "arch": sel.Arch,
			"sha256": sum, "size": size,
		})
	}
	b.downloadedBytes = downloaded
	b.totalBytes = total
	return nil
}
