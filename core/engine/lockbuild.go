package engine

import (
	"path/filepath"
	"sort"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/repository"
)

// requestDigestInput is the exact, deliberately narrow shape
// lock.RequestDigest is computed over: WHAT the operator asked for, and
// nothing about WHERE any of it lives on the machine that ran the build.
//
// # What "the same request" means
//
// RequestDigest answers exactly one question: "were these two builds asked
// for the same thing?" Two operators ask for the same thing when they name
// the same packages, the same vendor URLs, the same .deb files, and set the
// same knobs — regardless of which machine they are sitting at. A vendor
// .deb checked out at /builds/acme/repo/vendor-debs on a CI runner and at
// /home/alice/vendor-debs on a laptop is the SAME input; the directory it
// happens to sit in is an accident of the machine, in exactly the way
// Output.Path and SnapshotRef already were. So a host path may not reach
// this digest, in either direction: not written into it verbatim (contract
// brief rule 2 — a host path must never reach an artefact) and not allowed
// to change it (design section 3.13 — the same request built twice must
// produce byte-identical artefacts, and RequestDigest is a lock.json field,
// so it cascades into LockDigest, manifest.BundleID and the bundle id
// README.txt prints).
//
// Measured before this fix, with everything else held identical: setting
// any one of Options.StoreDir, Options.PolicyRef, Options.ApprovedKeysRef,
// Options.EmbedBinary, Inputs.Files, Inputs.LocalDirs or Inputs.ListFiles
// moved RequestDigest off its baseline f62557483d7eafba…; the same build
// with --local-dir /home/alice/vendor-debs and with --local-dir
// /builds/acme/repo/vendor-debs produced f440c1e5d08cdbd8… and
// f285e80ac6f4599b…. Every one of those seven is a host location.
//
// # How each kind of input is represented
//
//   - Package names and URL inputs are hashed VERBATIM: they are names, not
//     locations. A URL is the vendor's global identifier for the file, the
//     same string on every machine, and URLInput.SHA256 is the operator's
//     own content assertion about it — both are the request itself.
//   - File and directory inputs (Inputs.Files, Inputs.LocalDirs,
//     Inputs.ListFiles, and the three path-valued Options below) are
//     represented by BASENAME only. The basename is the part an operator
//     would say out loud ("the acme-agent .deb", "policy.yaml"); the
//     directories above it are the machine. Their CONTENT is not lost by
//     this: every local .deb that actually resolves is recorded, by name,
//     version and SHA-256, in this same lock's Packages array, which is a
//     far stronger content assertion than hashing the path string ever was.
//   - Every list is sorted before hashing, which Inputs' own doc comment
//     already promises ("the engine sorts before hashing so a reordering
//     does not change the request digest") and which nothing implemented
//     until now.
//
// # Deliberately excluded, and why
//
// This is the field's contract, so document it precisely — it is a
// published lock field someone will read years from now:
//
//   - BuildRequest.Output (Path, Format, Sign): where the bundle is written
//     and how it is delivered, never what was requested. This was defect 1
//     in earlier implementation notes.
//   - BuildRequest.SnapshotRef: also a host path (or a caller-side locator)
//     for the snapshot to resolve against, not part of the request's
//     content — see BuildRequest.SnapshotRef's own doc comment ("resolved by
//     the caller, not by the engine"). Two copies of the same snapshot
//     content at two different paths must be recognisable as the same
//     request. The snapshot's actual identity is already recorded
//     separately and content-addressed, in Lock.SnapshotDigest, which sits
//     right next to RequestDigest on Lock; a caller that wants to know
//     "is this byte-for-byte the same request against byte-for-byte the same
//     snapshot" checks both fields together rather than relying on one field
//     to encode both questions.
//   - Options.StoreDir: the location of the content-addressed store, which
//     is a persistent machine-level cache shared by every build on the host.
//     It is not an input at all — nothing about the requested bundle changes
//     with it — so unlike the file inputs above it is dropped outright
//     rather than reduced to a basename. It belongs in exactly the same
//     category as Output.Path.
//   - BuildRequest.SchemaVersion: wire-format metadata, not something the
//     operator asked for.
//
// This is its own named type, not `buildjob.BuildRequest` with a field
// zeroed out, so that a field added to BuildRequest in the future lands
// outside RequestDigest's scope by default and has to be deliberately added
// here — never silently starts affecting the digest, or silently starts
// leaking a new host path into it, the moment someone adds it upstream.
// Options is embedded whole for that same reason inverted: it is a
// dense list of behavioural knobs where the default for a NEW one should be
// "yes, it changes the request", so the four path-valued ones are blanked
// out by name in requestDigestOptions below instead.
type requestDigestInput struct {
	// Packages and URLs are the request's content-addressed inputs, sorted.
	Packages []string            `json:"packages,omitempty"`
	URLs     []buildjob.URLInput `json:"urls,omitempty"`
	// FileNames, LocalDirNames and ListFileNames are the basenames of
	// Inputs.Files, Inputs.LocalDirs and Inputs.ListFiles, sorted.
	FileNames     []string `json:"file_names,omitempty"`
	LocalDirNames []string `json:"local_dir_names,omitempty"`
	ListFileNames []string `json:"list_file_names,omitempty"`

	Options requestDigestOptions `json:"options"`
}

// requestDigestOptions is buildjob.Options with every path-valued field
// replaced by its basename (or, for StoreDir, removed entirely — see
// requestDigestInput's doc comment). Options itself is embedded rather than
// re-listed field by field so that a knob added to buildjob.Options in the
// future is covered by RequestDigest automatically, which is the right
// default for that type: its fields change what apt is asked and what is
// written. Only the path-valued ones need the override, and they are named
// here explicitly so that adding a fifth path-valued option is a visible,
// deliberate act.
type requestDigestOptions struct {
	buildjob.Options
	// These four shadow the embedded Options fields of the same name. JSON
	// field shadowing follows Go's own embedding rules: the outer field wins,
	// and the embedded one is not emitted, so each key appears exactly once.
	StoreDir        string `json:"store_dir,omitempty"`
	PolicyRef       string `json:"policy_ref,omitempty"`
	ApprovedKeysRef string `json:"approved_keys_ref,omitempty"`
	EmbedBinary     string `json:"embed_binary,omitempty"`
}

// newRequestDigestInput reduces a request to the shape above. Note that it
// reads only req, never any expanded/staged engine state: RequestDigest must
// describe what was ASKED for, which is knowable before a single byte is
// fetched, not what the resolution happened to produce.
func newRequestDigestInput(req buildjob.BuildRequest) requestDigestInput {
	opts := requestDigestOptions{
		Options: req.Options,
		// StoreDir stays empty: dropped, not reduced.
		PolicyRef:       baseNameOf(req.Options.PolicyRef),
		ApprovedKeysRef: baseNameOf(req.Options.ApprovedKeysRef),
		EmbedBinary:     baseNameOf(req.Options.EmbedBinary),
	}
	urls := append([]buildjob.URLInput(nil), req.Inputs.URLs...)
	sort.Slice(urls, func(i, j int) bool {
		if urls[i].URL != urls[j].URL {
			return urls[i].URL < urls[j].URL
		}
		return urls[i].SHA256 < urls[j].SHA256
	})
	return requestDigestInput{
		Packages:      sortedStrings(append([]string(nil), req.Inputs.Packages...)),
		URLs:          urls,
		FileNames:     sortedBaseNames(req.Inputs.Files),
		LocalDirNames: sortedBaseNames(req.Inputs.LocalDirs),
		ListFileNames: sortedBaseNames(req.Inputs.ListFiles),
		Options:       opts,
	}
}

// baseNameOf is filepath.Base with the empty string preserved: an unset
// option must stay unset rather than becoming ".".
func baseNameOf(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}

func sortedBaseNames(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, baseNameOf(p))
	}
	return sortedStrings(out)
}

// buildInitialLock maps the resolved, ingested plan into the lock.Lock that
// is handed to bundle.Assemble. Per Input.Lock's own contract ("The
// assembler fills its Packages, Stats and file digests; the caller supplies
// everything else"), Packages is filled here anyway — from the same plan the
// assembler already receives — because the plan carries provenance
// (Origin, Reason, PublisherVerification, redistribution Flags) that only
// the engine and the backend know about; the assembler's job is to
// reconcile that against what actually landed in the pool (digests, stats
// against the previous run), not to invent package identity from nothing.
// ClosedWorld and the doctor-derived flags are folded in later, after
// assembly and the closed-world check (see finalize.go); this is the
// pre-assembly shape.
func (b *build) buildInitialLock() {
	// best-effort: a digest failure here is not fatal to the build. See
	// requestDigestInput's doc comment above for exactly what is and is not
	// covered.
	reqDigest, _ := canonical.Digest(newRequestDigestInput(b.req))

	packages := make([]lock.Package, 0, len(b.plan.Selections))
	for _, sel := range b.plan.Selections {
		flags := append([]string(nil), sel.Flags...)
		if sel.UserSupplied && !containsString(flags, lock.FlagUserSupplied) {
			flags = append(flags, lock.FlagUserSupplied)
		}
		packages = append(packages, lock.Package{
			Name:                  sel.Name,
			Arch:                  sel.Arch,
			Version:               sel.Version,
			SourcePackage:         sel.SourcePackage,
			Filename:              repository.PoolPath(sel.Name, sel.Filename),
			Size:                  sel.Size,
			SHA256:                sel.SHA256,
			Origin:                redactOrigin(sel.Origin),
			Reason:                sel.Reason,
			PublisherVerification: sel.PublisherVerification,
			Flags:                 sortedStrings(flags),
			Essential:             sel.Essential,
		})
	}

	install := append([]string(nil), b.plan.Install...)
	sort.Strings(install)

	warnings := append([]lock.Warning(nil), b.plan.Warnings...)
	warnings = append(warnings, b.policyWarnings...)

	b.lockDoc = &lock.Lock{
		SchemaVersion:  lock.SchemaVersion,
		CreatedAt:      b.createdAt,
		SnapshotDigest: b.snap.Digest,
		RequestDigest:  reqDigest,
		Target: lock.Target{
			DistroID:     b.snap.Snapshot.Target.DistroID,
			VersionID:    b.snap.Snapshot.Target.VersionID,
			Codename:     b.snap.Snapshot.Target.Codename,
			Arch:         b.effectiveArch(),
			ForeignArchs: b.snap.Snapshot.Target.ForeignArchs,
			APTVersion:   b.snap.Snapshot.Target.APTVersion,
			DpkgVersion:  b.snap.Snapshot.Target.DpkgVersion,
		},
		Resolver:   b.plan.Resolver,
		Packages:   packages,
		Install:    install,
		Unresolved: append([]lock.Unresolved(nil), b.plan.Unresolved...),
		Warnings:   warnings,
		// DownloadedBytes is deliberately not set: it counts what the
		// machine's persistent store did not already hold, which is not a
		// property of the request. It is reported in BuildResult instead.
		// See stripCacheDependentStats (reproducible.go), which also clears
		// the value bundle.Assemble recomputes over this same template.
		Stats: lock.Stats{Bytes: b.totalBytes},
	}
}

// redactOrigin returns a copy of o with any credential embedded in URI
// stripped (fetch.RedactURL) before the value is written into lock.json,
// which ships inside the bundle and crosses the air gap (F3,
// docs/security/review-findings.md).
//
// Origin.URI IS populated with a live vendor URL, credential and all: this
// package's own attributeExternalOrigins (inputs.go) writes the operator's
// literal URL there, and has since it landed. This comment used to say the
// opposite — "no backend populates Origin.URI with a live vendor URL as of
// this writing", true when core/apt/local.go's originFor was the only
// producer and it fills only Suite/Component/ReleaseDigest/KeyFingerprint —
// and that stale sentence is a large part of why nobody checked the second
// writer. A comment asserting that a field is never populated is read as
// permission not to handle it. It is corrected here rather than deleted so
// the next reader can see that the assertion was made, and was wrong.
//
// This call site is NOT what keeps a credential out of the shipped
// lock.json, and must not be relied on as if it were. The document this
// function's caller builds is handed to bundle.Assemble as a template, and
// buildLock (core/bundle/assemble.go) rebuilds Packages from the same
// selections, discarding this redacted copy; the assembler's copy is the one
// that reaches disk. The gate that actually holds is redactLockOrigins in
// core/bundle, on the write itself — see its doc comment for why the fix
// belongs there and not in an enumeration of writers.
//
// What this retains is defence in depth at the producer, which is worth
// keeping precisely because the assembler is injectable in this package's
// tests (vars.go) and because Origin.URI's own contract promises "the
// archive URI or the vendor URL" to whoever reads the record years later:
// nothing reaching this call site is trusted to already be safe, whichever
// backend or future code path populated it. fetch.RedactURL is idempotent,
// so redacting in both places costs nothing and changes no bytes.
func redactOrigin(o lock.Origin) lock.Origin {
	o.URI = fetch.RedactURL(o.URI)
	return o
}
