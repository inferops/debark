// Package bundle assembles the deterministic on-media tree, exports it as
// tar.zst, and reads one back.
//
// Determinism is the point: two builds of the same request produce
// byte-identical trees. Sorted entries, fixed modes, a fixed timestamp, no
// host paths anywhere in the output.
package bundle

import (
	"context"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/store"
)

// Layout names inside a bundle. Nothing else may write to a bundle root.
const (
	RepoDir      = "repo"
	PoolDir      = "repo/pool"
	SnapshotFile = "snapshot.json"
	ReadmeFile   = "README.txt"
	SBOMFile     = "sbom.cdx.json"
	AddedFile    = "last-run-added.txt"
	RemovedFile  = "last-run-removed.txt"
	// UnreferencedFile lists every pool file repo/Packages does not index:
	// on the media, covered by the manifest and the signature, invisible to
	// the target's apt. Unlike its two siblings it is a statement about the
	// bundle's state after the run rather than about what the run did, which
	// is exactly why it needs writing down - a file that was neither added
	// nor removed appears in neither of the other two reports. See
	// unreferenced.go.
	UnreferencedFile = "last-run-unreferenced.txt"
	BinDir           = "bin"
)

// WarnUnreferenced is the lock.Warning code raised when the pool holds files
// repo/Packages does not index. Exported so callers and tests can match on
// the code rather than on the message text.
const WarnUnreferenced = "pool.unreferenced"

// FixedModTime is the timestamp written into every archive entry, so an export
// is reproducible. It is the Unix epoch plus one day, which keeps tools that
// dislike a zero timestamp happy.
const FixedModTime = 86400

// Input is one assembly.
type Input struct {
	// Dir is the bundle root. It may already contain a previous bundle, which
	// is what makes runs incremental.
	Dir string
	// Plan is the resolved plan; Selections must already carry StagedPath or
	// be present in Store.
	Plan *resolve.Plan
	// Store supplies file bytes by digest.
	Store store.Store
	// Snapshot is written into the bundle as snapshot.json.
	Snapshot *snapshot.Snapshot
	// Lock is written as lock.json. The assembler fills its Packages,
	// Stats and file digests; the caller supplies everything else.
	Lock *lock.Lock
	// Release carries the fields for the generated Release file.
	Release repository.ReleaseFields
	// Prune removes superseded files. User-supplied files and files this
	// run's own Plan.Selections still names are never removed.
	Prune bool
	// EmbedBinary, when non-empty, is a path to a debark binary copied to
	// bin/debark-linux-<arch>.
	EmbedBinary string
	// SBOM writes sbom.cdx.json.
	SBOM []byte
	// Evidence writes evidence.json.
	Evidence []byte
	// CreatedAt is the canonical timestamp used everywhere in the bundle.
	CreatedAt string
}

// Result is what an assembly produced.
type Result struct {
	Dir      string
	Manifest *manifest.Manifest
	Lock     *lock.Lock
	Repo     *repository.Result
	Stats    lock.Stats
	// Added and Removed are the file names written to last-run-added.txt and
	// last-run-removed.txt.
	Added   []string
	Removed []string
	// Unreferenced is what was written to last-run-unreferenced.txt: the
	// pool files this run's repo/Packages does not index. Empty on a build
	// into a clean directory; non-empty means the media carries more than
	// the index admits to.
	Unreferenced []string
}

// Assemble materialises the pool, generates the repository indices, writes the
// lock, the snapshot copy, the README and the manifest, and prunes when asked.
// It does not sign: signing is the caller's step, so keys never reach this
// package.
func Assemble(ctx context.Context, in Input) (*Result, error) {
	return assemble(ctx, in)
}

// ExportTar writes dir as <name>.debark.tar.zst deterministically.
func ExportTar(ctx context.Context, dir, tarPath string) error {
	return exportTar(ctx, dir, tarPath)
}

// ImportTar extracts a bundle archive into dir.
func ImportTar(ctx context.Context, tarPath, dir string) error {
	return importTar(ctx, tarPath, dir)
}

// Bundle is an opened bundle on disk.
type Bundle struct {
	Dir       string
	Manifest  *manifest.Manifest
	Signature *manifest.SignatureFile
	Lock      *lock.Lock
	Snapshot  *snapshot.Snapshot

	// ManifestCanonical is the canonical byte form the signature covers.
	ManifestCanonical []byte
	// Extracted is true when Dir is a temporary directory holding an extracted
	// tar bundle; Close removes it.
	Extracted bool
}

// Close releases a temporary extraction directory, if any.
func (b *Bundle) Close() error { return closeBundle(b) }

// Open reads a bundle from a directory or a .debark.tar.zst file. It parses
// and nothing more: Open never verifies. verify.Verify does that, and every
// caller that acts on a bundle must call it first.
func Open(ctx context.Context, path string) (*Bundle, error) {
	return openBundle(ctx, path)
}

// Prune is the pure pruning decision, exported so it can be tested without a
// filesystem: given every file currently in the pool, it returns the ones to
// remove. Per (name, arch) it keeps the highest version by dpkg comparison;
// user-supplied files and files the caller's plan/lock still names
// (LockReferenced) participate in the comparison but are never returned for
// removal — see docs/experiments/E3-pin-fidelity.md's "Prune resolution"
// section for why the latter exists: a numeric-highest rule with no other
// input can otherwise discard the very version the current build's own lock
// is about to record, if an older run left a numerically higher sibling
// behind. This mirrors ADR-007's rule for the install/upgrade path (never let
// a free re-resolution override what the lock already decided) one stage
// earlier in the pipeline.
func Prune(files []PoolEntry) (remove []PoolEntry) { return pruneDecide(files) }

// PoolEntry is one file in the pool, as the prune decision sees it.
type PoolEntry struct {
	Name    string
	Arch    string
	Version string
	Path    string
	// UserSupplied marks a file the operator provided directly; it is
	// never returned for removal, however old it is.
	UserSupplied bool
	// LockReferenced marks a file that is the current build's own plan
	// selection for this (name, arch) — the version the lock this build
	// writes will name. It is never returned for removal, regardless of
	// version, for the same reason UserSupplied is never returned: the pool
	// exists to serve the lock, not the other way around. See
	// docs/experiments/E3-pin-fidelity.md.
	LockReferenced bool
}

// ReadmeText renders README.txt: what target the bundle is for, what is in it,
// how to verify it and how to install it. It is a pure function so its output
// can be golden-tested.
func ReadmeText(m *manifest.Manifest, l *lock.Lock, signed bool) string {
	return readmeText(m, l, signed)
}
