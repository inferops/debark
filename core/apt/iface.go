// Package apt is the adapter around the distribution's own apt-get. apt is the
// oracle: every dependency decision in debark is made by the target
// release's apt running in a private root, never by Go code (ADR-001).
//
// This file is the frozen contract. Implementations live beside it:
// the private-root builder, the exec runner and its output parsers, the local
// backend, and the container backend.
package apt

import (
	"context"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
)

// Backend resolves a request against a snapshot and stages the chosen .deb
// files somewhere the caller can ingest them.
//
// Two implementations exist in v1: local, which runs apt-get on this machine,
// and container, which runs this same binary with --backend=local inside a
// pinned image of the target release. Both return the same Plan, so nothing
// downstream knows or cares which ran. Signing never happens inside a backend.
type Backend interface {
	// Kind is local or container.
	Kind() lock.Backend

	// Probe reports whether this backend can run here, and what it would use.
	// It performs no resolution and must be cheap: it is called during
	// backend auto-selection.
	Probe(ctx context.Context, target snapshot.Target) (Capabilities, error)

	// Resolve runs the full sequence — update, optional full-upgrade,
	// install --download-only for requested and external packages — and
	// returns the plan plus the staged files.
	Resolve(ctx context.Context, in ResolveInput) (*resolve.Plan, error)

	// ClosedWorld re-resolves the finished bundle against itself with the
	// network unavailable and the bundle as the only source (ADR-007).
	ClosedWorld(ctx context.Context, in ClosedWorldInput) (lock.ClosedWorld, error)
}

// Base-closure resolution (ADR-014) deliberately does NOT appear on Backend,
// and the reason is worth recording because "add a third method" was the
// obvious shape and it is wrong.
//
// Backend exists so that one question — what does apt say — has one answer
// whichever machine apt runs on. A base closure cannot be split that way. Its
// input is not a snapshot but a base definition, and building the bootstrap
// root that definition describes needs the target distribution's own archive
// keyring; a Windows or macOS host, which is precisely the host that has no
// backend but the container, does not have one and cannot get one. So there
// is nothing for a container implementation of "resolve this closure" to
// receive. What crosses that boundary instead is the whole operation: the
// container runs `snapshot from-base` and one finished snapshot archive comes
// back, which the host then validates with snapshot.Open like any other.
//
// The two halves therefore live apart, both in this package:
//
//	LocalClosure            (closure.go)       apt on this machine
//	BaseSnapshotInContainer (basecontainer.go) the whole command, in the image
//
// core/base.Synthesize is the single caller that chooses between them.

// Capabilities is what a Probe found.
type Capabilities struct {
	// Available is false when this backend cannot run here at all; Reason then
	// explains why in one line and Hint gives the exact command that fixes it.
	Available bool
	Reason    string
	Hint      string

	// APTVersion and DpkgVersion are what this backend would resolve with,
	// e.g. "2.8.3" and "1.22.6". For the container backend they are the
	// image's, discovered on first use and cached.
	APTVersion  string
	DpkgVersion string

	// DistroID and VersionID identify the resolving system.
	DistroID  string
	VersionID string

	// Image and ImageDigest are set by the container backend.
	Image       string
	ImageDigest string

	// Runtime is docker or podman for the container backend.
	Runtime string
}

// ResolveInput is everything a backend needs for one resolution.
type ResolveInput struct {
	// Snapshot is the target's captured state, already loaded and validated.
	Snapshot *snapshot.Snapshot
	// SnapshotFilesDir is the directory holding the snapshot's captured files
	// (the extracted files/ tree). The backend materialises the private root
	// from it.
	SnapshotFilesDir string

	// Packages are apt package names, optionally name=version.
	Packages []string
	// ExternalRepoDir, when non-empty, is a directory of vendor .deb files
	// already indexed as a staging repository. The backend adds it to the
	// private root with Trusted: yes and a file: URI so apt resolves their
	// dependencies against the archive.
	ExternalRepoDir string
	// ExternalNames are the package names the external .deb files provide;
	// they are installed alongside Packages.
	ExternalNames []string

	// ArchivesDir is where apt must leave downloaded .deb files
	// (Dir::Cache::archives). The caller ingests them into the store.
	ArchivesDir string
	// WorkDir is a scratch directory for the private root and captured output.
	WorkDir string

	// Options carries the request's resolution knobs.
	Options buildjob.Options
	// Recommends is the effective Install-Recommends value, already resolved
	// from the target's apt.conf and any override.
	Recommends bool
	// Upgrades adds a full-upgrade --download-only pass before install.
	Upgrades bool
	// PhasedPolicy is the phased-update policy for this snapshot.
	PhasedPolicy snapshot.PhasedPolicy

	// ApprovedKeys, when non-empty, is the set of archive key fingerprints
	// resolution is allowed to trust (uppercase hex). A source authenticated
	// by anything else is a policy failure (exit 6).
	ApprovedKeys []string

	// DownloadOnly is always true in v1: debark never installs on the
	// builder. The field exists so the contract states it.
	DownloadOnly bool
}

// ClosedWorldInput describes the offline self-check.
type ClosedWorldInput struct {
	Snapshot         *snapshot.Snapshot
	SnapshotFilesDir string
	// BundleRepoDir is the finished bundle's repo/ directory, which becomes
	// the only source. It must be ABSOLUTE: the container backend makes it a
	// bind-mount source and refuses a relative path, where the local backend
	// happens to absolutise it itself, so a relative path is a check that
	// passes on one backend and fails on the other.
	BundleRepoDir string
	// Install is the exact name=version set the lock will tell install to
	// request.
	Install []string
	// Upgrades repeats the full-upgrade simulation when the plan had one.
	Upgrades bool
	WorkDir  string
}

// Runner executes apt-get. It exists so every parser can be tested against
// recorded output and so the container backend can substitute an exec that
// crosses a container boundary.
type Runner interface {
	// Run executes apt-get with the given options and arguments and returns
	// its combined output. A non-zero exit is returned as an error carrying
	// dferr.Resolution, with the output preserved for the caller to parse.
	Run(ctx context.Context, opts []string, args ...string) (Output, error)
}

// Output is one apt-get invocation's result.
type Output struct {
	Argv     []string
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Combined returns stdout followed by stderr, which is what most parsers want.
func (o Output) Combined() []byte { return append(append([]byte{}, o.Stdout...), o.Stderr...) }
