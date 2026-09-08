package engine

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/version"
)

// expandAndStageInputs turns the request's raw Inputs into the deduplicated
// package/URL/file lists apt resolution needs, expanding list files and
// local-dir scans first, then stages every URL and file input: fetch
// each into the store, materialise a copy into a flat staging directory, and
// index that directory as a private repository so the apt backend can
// resolve the externals' own dependencies against it. ExternalNames — the
// package names apt needs to `install` — come from each staged .deb's own
// Package: control field, read via debPackageName.
func (b *build) expandAndStageInputs(ctx context.Context) error {
	in := b.req.Inputs

	packages := append([]string(nil), in.Packages...)
	urls := append([]buildjob.URLInput(nil), in.URLs...)
	files := append([]string(nil), in.Files...)
	localDirs := append([]string(nil), in.LocalDirs...)

	for _, lf := range in.ListFiles {
		expanded, err := parseListFile(lf)
		if err != nil {
			return classify(err, dferr.Usage, "engine: parse list file %s", lf)
		}
		packages = append(packages, expanded.Packages...)
		urls = append(urls, expanded.URLs...)
		files = append(files, expanded.Files...)
		localDirs = append(localDirs, expanded.LocalDirs...)
	}

	for _, dir := range dedupeStrings(localDirs) {
		found, err := scanDir(dir)
		if err != nil {
			return classify(err, dferr.Usage, "engine: scan local dir %s", dir)
		}
		files = append(files, found...)
	}

	b.packages = dedupeStrings(packages)
	b.urls = dedupeURLs(urls)
	b.files = dedupeStrings(files)

	return b.stageExternals(ctx)
}

// stagedExternal is one .deb that made it into the staging directory,
// whichever of the two paths (local file, URL) it came from.
type stagedExternal struct {
	stagedPath string
	filename   string
	origin     externalOrigin
}

// externalOrigin is the provenance of one staged external input, as the
// ENGINE knows it — which is the only place it is ever known.
//
// url is the operator's literal URL input, and is empty for a local --file
// or --local-dir input. That emptiness is the whole point: it is the one
// fact that tells a URL input apart from a local file input, and by the time
// apt has resolved the staging repository it is gone. See
// attributeExternalOrigins.
//
// digest is the SHA-256 of the bytes actually staged, used to confirm that
// the file the backend selected under this base name really is the one this
// engine put there before the URL is attributed to it.
type externalOrigin struct {
	url    string
	digest string
}

func (b *build) stageExternals(ctx context.Context) error {
	if len(b.urls) == 0 && len(b.files) == 0 {
		return nil
	}

	stagingDir := filepath.Join(b.workRoot, "external")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return dferr.Wrap(dferr.Environment, err, "engine: create staging directory")
	}

	var staged []stagedExternal

	for _, path := range b.files {
		fetched, err := fromLocalFile(ctx, b.st, path)
		if err != nil {
			// Basenames only, for the same reason as the success path
			// below — an incomplete build still writes a bundle, and this
			// event still travels inside it. err.Error() names the path
			// too, so the path is masked out of the message rather than the
			// message dropped: the syscall-level reason ("permission
			// denied", "is a directory") is what makes this warning useful.
			base := filepath.Base(path)
			detail := strings.ReplaceAll(err.Error(), path, base)
			b.warn(evidence.TypeInputExternal, "local .deb not found or unreadable: "+base,
				map[string]any{"path": base, "error": detail})
			// reason comes from core/fetch, which tagged the return
			// statement it took, so "this file is missing" and "this file
			// is not a .deb" stay apart. The path-masked detail is reused
			// rather than the raw message: FetchFailure.Input already
			// carries the operator's full path, so nothing is lost, and one
			// string cannot disagree with itself.
			reason := fetch.ReasonOf(err)
			if reason == buildjob.ReasonOther {
				reason = buildjob.ReasonUnreadable
			}
			b.fetchFailures = append(b.fetchFailures, buildjob.FetchFailure{
				Input:  path,
				Reason: reason,
				Detail: detail,
			})
			continue
		}
		dest := filepath.Join(stagingDir, fetched.Filename)
		if err := b.st.Materialise(fetched.Digest, dest); err != nil {
			return dferr.Wrap(dferr.Environment, err, "engine: stage %s", fetched.Filename)
		}
		// No url: this input came off the operator's own disk. Recording
		// the emptiness is as load-bearing as recording a URL would be —
		// see externalOrigin's doc comment.
		staged = append(staged, stagedExternal{
			stagedPath: dest, filename: fetched.Filename,
			origin: externalOrigin{digest: fetched.Digest},
		})
		// "source" is the input's BASENAME, not the operator's full path.
		// evidence.json is written inside the bundle and is covered by the
		// manifest the signature is made over, so a host path recorded here
		// is a host path shipped across the air gap (contract brief rule 2)
		// AND a reproducibility break: the same vendor .deb, byte-identical,
		// read from /home/alice/vendor-debs on a laptop and from
		// /builds/acme/repo/vendor-debs on a CI runner would otherwise give
		// two different evidence.json files, hence two different manifests
		// and two different signatures. The same reasoning that scoped
		// lock.RequestDigest to basenames applies here (see
		// requestDigestInput in lockbuild.go); nothing identifying is lost,
		// because the file's real identity — its SHA-256 — is in this same
		// event, and its resolved package identity is in lock.Packages.
		// The operator's literal path is still returned to the caller
		// verbatim in BuildResult.FetchFailed when the read FAILS: that is
		// operator-facing summary data, not a persisted artefact.
		b.emit(evidence.TypeInputExternal, "staged local .deb: "+fetched.Filename, map[string]any{
			"source": filepath.Base(path), "filename": fetched.Filename, "sha256": fetched.Digest, "size": fetched.Size,
			"publisher_verification": string(fetched.Verification),
		})
	}

	if len(b.urls) > 0 {
		fetcher := newFetcher(fetch.Options{Store: b.st, Events: b.sink, UserAgent: userAgent()})
		for _, res := range fetcher.FetchAll(ctx, b.urls) {
			// safeURL strips any userinfo/query credential from the
			// operator's literal input string before it can reach an
			// evidence event (core/evidence's contract: a value there "must
			// never contain a secret") — res.Input.URL itself is left
			// untouched for FetchFailed, which is operator-facing summary
			// data returned from Build, not a persisted artefact.
			safeURL := fetch.RedactURL(res.Input.URL)
			if res.Err != nil {
				detail := fetch.RedactMessage(res.Err.Error())
				// fetch.RedactMessage, not res.Err.Error() raw: this string
				// is written into evidence.json, which the manifest covers
				// and the bundle carries across the air gap, and a
				// transport failure arrives wrapped in a *url.Error whose
				// Error() prints the request URL verbatim — password and
				// query token included. core/fetch now redacts on the way
				// out, so in production this is the second of two passes;
				// it is here as well because this is the line that writes
				// the artefact, and a Fetcher is an injectable interface
				// (newFetcher is a package var, and every test replaces
				// it). The package that WRITES the secret is the one that
				// has to be unable to.
				b.warn(evidence.TypeInputExternal, "download failed: "+safeURL,
					map[string]any{"url": safeURL, "error": detail})
				// Input is the operator's LITERAL url, matching FetchFailed
				// and for the same reason (they typed it and have to
				// recognise it to retry). Detail is the redacted form,
				// because unlike Input it can carry a cause this process
				// did not compose.
				b.fetchFailures = append(b.fetchFailures, buildjob.FetchFailure{
					Input:  res.Input.URL,
					Reason: fetch.ReasonOf(res.Err),
					Detail: detail,
				})
				continue
			}
			dest := filepath.Join(stagingDir, res.Fetched.Filename)
			if err := b.st.Materialise(res.Fetched.Digest, dest); err != nil {
				return dferr.Wrap(dferr.Environment, err, "engine: stage %s", res.Fetched.Filename)
			}
			// The operator's LITERAL url is carried, not safeURL: this value
			// is the engine's private record of provenance, and every
			// consumer of it redacts for itself at the point it becomes
			// visible (redactOrigin on the way into lock.json — see
			// lockbuild.go — and core/policy's own fetch.RedactURL on the way
			// into a Finding's Detail). Redacting here as well would leave
			// nothing able to answer "was this the same URL?" and would
			// duplicate a scrubber this project deliberately keeps single.
			staged = append(staged, stagedExternal{
				stagedPath: dest, filename: res.Fetched.Filename,
				origin: externalOrigin{url: res.Input.URL, digest: res.Fetched.Digest},
			})
			b.emit(evidence.TypeInputExternal, "downloaded: "+res.Fetched.Filename, map[string]any{
				"url": safeURL, "filename": res.Fetched.Filename, "sha256": res.Fetched.Digest, "size": res.Fetched.Size,
				"publisher_verification": string(res.Fetched.Verification),
			})
		}
	}

	if len(staged) == 0 {
		return nil
	}

	var names []string
	var poolFiles []repository.PoolFile
	origins := make(map[string]externalOrigin, len(staged))
	for _, s := range staged {
		name, err := debPackageName(ctx, s.stagedPath)
		if err != nil || name == "" {
			b.warn(evidence.TypeInputExternal, "not a valid .deb, ignoring: "+s.filename,
				map[string]any{"filename": s.filename})
			_ = os.Remove(s.stagedPath)
			continue
		}
		names = append(names, name)
		poolFiles = append(poolFiles, repository.PoolFile{Path: s.filename})
		// Last write wins, which is what is actually on disk: two inputs
		// staging the same base name means the second Materialise above
		// overwrote the first, and the staging repo indexes the survivor.
		origins[s.filename] = s.origin
	}
	if len(poolFiles) == 0 {
		return nil
	}
	sort.Strings(names)
	b.externalNames = dedupeStrings(names)

	if _, err := b.repoWr.Write(ctx, repository.Input{
		Dir:   stagingDir,
		Files: poolFiles,
		Release: repository.ReleaseFields{
			Origin:        "debark-external",
			Label:         "debark external inputs",
			Suite:         "./",
			Architectures: []string{b.effectiveArch()},
			Components:    []string{repository.DefaultComponent},
			Date:          b.createdAt,
		},
		Compress: true,
	}); err != nil {
		return classify(err, dferr.Environment, "engine: index external .deb files")
	}
	b.externalRepoDir = stagingDir
	b.externalOrigins = origins
	return nil
}

// attributeExternalOrigins puts back the one fact resolution destroys: which
// external selections came from a vendor URL, and which came off the
// operator's own disk.
//
// # Why the engine has to do this
//
// An external input reaches apt as a file in the staging repository
// (stageExternals above), added as "deb [trusted=yes] file://... ./". So
// every URI apt can report for it — Selection.URI, filled by core/apt from
// apt's own --print-uris (core/apt/local.go) — is a file: URI naming a
// directory under this build's temporary work root. The operator's URL is
// known only here, before the fetch, and was previously dropped the moment
// the bytes landed in the staging directory.
//
// That silently disabled a documented security control. A policy's
// allow_url_inputs: false is evaluated as "Reason is external AND URI is an
// http(s) URL" (core/policy's allow-url-inputs rule); with URI always a
// file: URI in production, the rule matched nothing and an operator who
// switched it on to refuse network-sourced packages got a clean build and no
// finding. The rule's own test set URI by hand, so it passed. Fixing the
// RULE instead would mean denying every ReasonExternal selection, which
// denies local --file inputs too — inputs examples/policy.yaml explicitly
// expects to keep working — turning a silent allow into false exit-6
// denials. So the data is what was wrong, and this is where the data lives.
//
// # What it writes, and what it deliberately does not
//
//   - Selection.URI becomes the operator's URL for a URL input, and is
//     CLEARED for a local file input. Both halves matter: the field's own
//     contract is "the archive URI apt used, or the vendor URL. Empty for a
//     local file input" (core/resolve/types.go), and until now it was a
//     work-root file: URI for both, which is neither. Clearing it also keeps
//     a per-run temporary path out of the value core/policy hands to a
//     commercial edition in Finding.Detail.
//   - Origin.URI gets the same value, because that is the field that reaches
//     lock.json and doctor's external-provenance check. What keeps a
//     credential out of the shipped lock.json is redactLockOrigins, a gate on
//     the write in core/bundle; redactOrigin in lockbuild.go is defence in
//     depth at one producer and does not cover the other (c1b397c). lock.Origin.URI is
//     documented as "the archive URI or the vendor URL; empty for local file
//     inputs" and has had no writer at all until now.
//   - Origin.LocalPath is deliberately left empty. Its doc says "the
//     operator-supplied path for file inputs, recorded as given", and
//     recording it as given would put a host path into lock.json — which
//     both the contract rule against host paths in artefacts and this
//     package's byte-identical-rebuild claim forbid, since the same vendor
//     .deb read from two directories would then produce two different
//     LockDigests and two different bundle ids. The distinction the security
//     control needs is carried entirely by URI being empty or not, so
//     nothing is lost by leaving it alone; making LocalPath truthful is a
//     core/lock decision, not one to smuggle in here.
//
// # Matching
//
// By staged base name, which is the name the staging repo indexes and the
// name the backend reports back in Selection.Filename (core/apt/local.go
// rewrites external StagedPaths using exactly this equality). The digest is
// then checked when both sides know one: if apt resolved this base name to
// different bytes than the engine staged, it is not the operator's download
// and must not be attributed to their URL — leaving URI as the backend set
// it, so the failure mode is "no claim" rather than "a false claim".
func (b *build) attributeExternalOrigins() {
	if b.plan == nil || len(b.externalOrigins) == 0 {
		return
	}
	for i := range b.plan.Selections {
		sel := &b.plan.Selections[i]
		if sel.Reason != lock.ReasonExternal {
			continue
		}
		org, ok := b.externalOrigins[sel.Filename]
		if !ok {
			continue
		}
		if sel.SHA256 != "" && org.digest != "" && !strings.EqualFold(sel.SHA256, org.digest) {
			b.warn(evidence.TypeInputExternal,
				"staged input and resolved package disagree about "+sel.Filename+"; provenance not attributed",
				map[string]any{"filename": sel.Filename, "name": sel.Name})
			continue
		}
		sel.URI = org.url
		sel.Origin.URI = org.url
	}
}

func userAgent() string {
	return "debark/" + version.Get().Version
}
