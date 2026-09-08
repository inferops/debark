// Package apt: this file is the contribution -- the container
// backend, the thing that makes debark work on macOS and Windows at all,
// and that makes a cross-release or cross-architecture build correct rather
// than approximate.
//
// containerBackend implements Backend (core/apt/iface.go, frozen) by
// shelling out to the `docker` or `podman` CLI -- never a daemon SDK
// -- to re-run *this same debark binary*, built for the
// target's architecture, as `debark resolve --backend local` inside a
// pinned image of the target release (ADR-013). That inner invocation's
// command line, mount layout, JSON envelope and exit codes are the frozen
// contract in docs/dev/resolve-contract.md; container_driver.go builds and
// parses exactly that. Only the resolve step runs in a container: indexing,
// manifest generation and signing stay on the host, because signing keys
// must never enter a container and because one host-side code path is what
// keeps bundles byte-identical (resolve-contract.md's opening paragraph).
// This backend never mounts a key, a key path or a keyring into a
// container, and never passes one on its argv -- there is nothing in
// ResolveInput or ClosedWorldInput that names one.
//
// Ownership: this file and every other core/apt/container*.go file belong
// to the container backend; the rest of the package is the local one -- this
// file calls nothing defined there, only the frozen declarations in
// iface.go and api.go, so it keeps compiling regardless of its in-progress
// state. api.go's NewContainerBackend stub still needs a one-line change
// (`return newContainerBackend(opts)`) to wire this in; that edit belongs to
// whoever owns api.go, not to this package.
package apt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
)

// containerWarnUnpinnedImage is the lock.Warning code used when the image
// resolution ran with carries no known digest (resolve-contract.md: "A
// tag-only image produces a warning recorded in the lock's resolver block").
const containerWarnUnpinnedImage = "container.image-unpinned"

// containerBackend is the concrete Backend implementation. It holds nothing
// but its configuration: the process-lifetime image-version cache lives at
// package level (containerImageProbeCache in container_driver.go) precisely
// so it is shared no matter how many containerBackend values
// NewContainerBackend hands out.
type containerBackend struct {
	opts ContainerOptions
}

// Compile-time proof that *containerBackend satisfies Backend exactly.
// Nothing in the tree assigns one to the other yet -- api.go's
// NewContainerBackend stub still returns a bare nil, so without this line a
// signature slip here would compile silently today and surface only once
// the one-line hand-off below is made. This line makes go build itself the
// check.
var _ Backend = (*containerBackend)(nil)

// newContainerBackend is the real constructor behind the frozen
// NewContainerBackend(opts) in api.go. See this file's package doc for the
// one-line hand-off api.go still needs.
func newContainerBackend(opts ContainerOptions) *containerBackend {
	return &containerBackend{opts: opts}
}

func (b *containerBackend) Kind() lock.Backend { return lock.BackendContainer }

// emit sends one evidence event if b.opts.Events is set, filling in Schema
// and TS via the Sink implementation's own Emit (evidence.Event doc: sinks
// "set Schema and TS when empty").
func (b *containerBackend) emit(typ string, attrs map[string]any) {
	if b.opts.Events == nil {
		return
	}
	b.opts.Events.Emit(evidence.Event{Type: typ, Attrs: attrs})
}

// --- Probe -----------------------------------------------------------

// Probe reports whether this backend can run here, and what it would use.
// It must stay cheap: the one potentially slow step (discovering an image's
// apt/dpkg versions, which requires actually running the image) is cached
// for the process lifetime in containerImageProbeCache, so only the first
// Probe of a given (runtime, image, platform) ever pays for it.
func (b *containerBackend) Probe(ctx context.Context, target snapshot.Target) (Capabilities, error) {
	name, execPath, err := containerDetectRuntime(b.opts)
	if err != nil {
		// A missing runtime is a fact Probe reports, not a Go-level failure:
		// SelectBackend turns Capabilities.Available==false plus
		// Reason/Hint into the dferr.Environment error the operator sees
		// when they asked for the container backend specifically. Resolve
		// and ClosedWorld, which cannot proceed at all without a runtime,
		// return this same classified error directly instead.
		return Capabilities{Available: false, Reason: err.Error(), Hint: dferr.HintOf(err)}, nil
	}

	release, image, imageDigest, _, ierr := containerResolveImage(b.opts, target.DistroID, target.VersionID, target.Codename)
	if ierr != nil {
		return Capabilities{}, ierr
	}
	platform, perr := containerPlatformFor(b.opts, target.Arch)
	if perr != nil {
		return Capabilities{Available: true, Runtime: name, Image: image, Reason: perr.Error()}, nil
	}

	probe, verr := containerCachedProbeImageVersions(ctx, execPath, name, image, platform)
	if verr != nil {
		// The runtime itself works, but the image could not be probed (no
		// network, bad tag, emulation unavailable, ...): still report
		// Available so the caller sees a runtime exists, but explain why the
		// version discovery failed.
		return Capabilities{
			Available: true, Runtime: name, Image: image,
			Reason: verr.Error(), Hint: dferr.HintOf(verr),
		}, nil
	}
	return Capabilities{
		Available:   true,
		Runtime:     name,
		Image:       image,
		ImageDigest: imageDigest, // never release.ImageDigest: see containerResolveImage
		APTVersion:  probe.APTVersion,
		DpkgVersion: probe.DpkgVersion,
		DistroID:    containerFirstNonEmpty(probe.DistroID, release.DistroID),
		VersionID:   containerFirstNonEmpty(probe.VersionID, release.VersionID),
	}, nil
}

func containerFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// containerResolveImage resolves the release row and the image reference to
// run for a target identity, honouring ContainerOptions.Image as an
// override. warn is non-empty when the resolved image has no known digest
// pin (an operator-supplied override that is not already "name@sha256:...",
// or a distro-table row hack/pin-images.go has not pinned yet).
//
// imageDigest is returned SEPARATELY from release.ImageDigest, and callers
// must record this one, because the two are not the same value under an
// override and recording the wrong one is a provenance lie.
//
// Measured: with ContainerOptions.Image = "myregistry/debian:custom" for a
// debian/12 target, distro.Resolve still returns the bookworm-slim row, so
// every caller that reached for release.ImageDigest wrote
// Resolver.Image = "myregistry/debian:custom" alongside
// Resolver.ImageDigest = "sha256:88200866..." -- bookworm-slim's digest, for
// an image that never ran. The lock then simultaneously carried the
// containerWarnUnpinnedImage warning ("not pinned by digest") and a pin, and
// the same wrong pair went into the backend.selected evidence event, which
// is inside the signed bundle. An auditor pulling that digest gets a
// different image than the one that resolved the bundle, with nothing in the
// record saying so.
//
// So: an override contributes a digest only when it carries one itself
// ("name@sha256:..."), taken from the override string, and otherwise
// contributes none. Empty is the honest answer -- the warning is already
// there to say why -- and it is what keeps "ImageDigest names the image that
// ran" true on every path.
func containerResolveImage(opts ContainerOptions, distroID, versionID, codename string) (release distro.Release, image, imageDigest, warn string, err error) {
	release, rerr := distro.Resolve(distroID, versionID, codename)
	if rerr != nil {
		return distro.Release{}, "", "", "", dferr.Wrap(dferr.Usage, rerr, "container: resolve target release")
	}
	if opts.Image != "" {
		image = opts.Image
		if _, digestPart, found := strings.Cut(image, "@"); found && strings.HasPrefix(digestPart, "sha256:") {
			imageDigest = digestPart
		} else {
			warn = fmt.Sprintf(
				"container image %q (ContainerOptions.Image override) is not pinned by digest; resolution is not reproducible across a registry re-tag", image)
		}
		return release, image, imageDigest, warn, nil
	}
	image = release.Ref()
	imageDigest = release.ImageDigest
	if release.ImageDigest == "" {
		warn = fmt.Sprintf(
			"container image %s has no pinned digest for %s %s (%s); run hack/pin-images.go and update core/distro/distro.go, or resolution is not byte-for-byte reproducible across a registry re-tag",
			release.Image, release.DistroID, release.VersionID, release.Codename)
	}
	return release, image, imageDigest, warn, nil
}

// containerSelfPath returns the debark binary to mount into the container,
// for a container that will run wantArch.
//
// The process actually running this code is, on macOS and Windows, never
// itself a Linux ELF binary, so there is no usable default there at all: a
// linux/<arch> build must come from somewhere else. Three places, in order:
//
//  1. ContainerOptions.SelfPath, which the CLI fills from --self-binary, the
//     DEBARK_SELF_BINARY environment variable, or the self_binary config
//     key. An explicit answer always wins.
//  2. bin/debark-linux-<arch> (or debark-linux-<arch>) beside the
//     running executable. This is the same naming convention the bundle
//     layout already uses for --embed-binary (design 3.4.4) and the same one
//     containerSelfPathHint tells an operator to build into, so an operator
//     who has followed either instruction once needs no flag at all -- which
//     is the whole point of looking here, since on Windows and macOS the
//     alternative is that every single build command carries a path.
//  3. os.Executable() itself. On a Linux host targeting its own architecture
//     with the container backend explicitly requested (parity testing, or an
//     explicit operator override of an otherwise-local decision), the
//     running binary can coincidentally already be a valid static
//     linux/<arch> build.
//
// This function never decides whether the file it names actually works:
// containerValidateBinary does that, and its error is what names the flag
// and the exact build command when it does not.
func containerSelfPath(opts ContainerOptions, wantArch string) (string, error) {
	if opts.SelfPath != "" {
		return opts.SelfPath, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", dferr.New(dferr.Environment,
			"container: determine the running executable's path: %v", err).
			WithHint("%s", containerSelfPathHint(wantArch))
	}
	if sibling, ok := containerSiblingSelfPath(p, wantArch); ok {
		return sibling, nil
	}
	return p, nil
}

// containerSiblingSelfPath looks for a linux/<arch> debark beside exePath,
// under either of the two spellings the project already documents, and
// reports whether one is there. It only ever answers "there is a regular
// file at this path": whether that file is the right kind of binary is
// containerValidateBinary's question, deliberately, so that a wrongly-built
// sibling produces the same specific ELF diagnosis an explicit
// --self-binary would rather than being silently skipped over.
func containerSiblingSelfPath(exePath, wantArch string) (string, bool) {
	if exePath == "" || wantArch == "" {
		return "", false
	}
	dir := filepath.Dir(exePath)
	for _, cand := range []string{
		filepath.Join(dir, "bin", "debark-linux-"+wantArch),
		filepath.Join(dir, "debark-linux-"+wantArch),
	} {
		if fi, err := os.Stat(cand); err == nil && fi.Mode().IsRegular() {
			return cand, true
		}
	}
	return "", false
}

// containerStageSnapshotDir materialises an "extracted snapshot directory"
// (snapshot.DocumentName + snapshot.FilesDir side by side, exactly the
// layout snapshot.Open documents it accepts) that the inner `debark
// resolve --snapshot DIR` can load, and returns the two host paths to mount.
//
// Why this exists: docs/dev/resolve-contract.md's mount table names a
// single ro mount of "the snapshot archive" at /snapshot.tar.zst. But
// Backend.Resolve (iface.go, frozen) is not given a path to that archive --
// ResolveInput carries an already-parsed *snapshot.Snapshot plus
// SnapshotFilesDir (the extracted files/ tree), never the original
// snapshot.tar.zst's path. There is no field to bind-mount an archive file
// from. This bridges the gap the only way the fields on hand allow: it
// re-serialises the in-memory Snapshot document to JSON (plain
// encoding/json, not core/canonical -- this file is never hashed or signed,
// it only has to round-trip through the inner process's snapshot.Open) next
// to a mount of the already-extracted files/ tree (no copy of potentially
// large file contents), and the driver passes `--snapshot /snapshot`, a
// directory, which the contract's flag grammar explicitly allows ("Path to
// a snapshot.tar.zst *or an extracted snapshot directory*"). See the implementation notes: this is a considered bridging decision, not something the
// frozen contract states outright, because ResolveInput's shape and the
// mount table's illustrative example do not quite line up.
func containerStageSnapshotDir(workDir string, snap *snapshot.Snapshot, snapshotFilesDir string) (jsonHostPath, filesHostPath string, err error) {
	stageDir := filepath.Join(workDir, "container-snapshot")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return "", "", dferr.Wrap(dferr.Environment, err, "container: stage snapshot dir")
	}
	jsonHostPath = filepath.Join(stageDir, snapshot.DocumentName)
	sj, merr := json.Marshal(snap)
	if merr != nil {
		return "", "", dferr.Wrap(dferr.Usage, merr, "container: marshal snapshot for staging")
	}
	if werr := os.WriteFile(jsonHostPath, sj, 0o644); werr != nil {
		return "", "", dferr.Wrap(dferr.Environment, werr, "container: write staged snapshot.json")
	}
	if err := containerCheckMountSourceDir(snapshotFilesDir, "SnapshotFilesDir"); err != nil {
		return "", "", err
	}
	return jsonHostPath, snapshotFilesDir, nil
}

// containerDefaultStoreVolume is the stable, well-known volume name used
// when Windows or macOS defaults to a named volume without an
// operator-supplied ContainerOptions.StoreVolume (see storeVolumeChoice).
// Deliberately shared across builds/targets on this machine: it is meant to
// behave like a persistent Dir::Cache::archives-style cache, exactly as a
// bind-mounted ArchivesDir already does today across repeated runs -- just
// on faster storage.
//
// This comment used to end by claiming that "unrelated bytes a previous,
// different build left behind in this same volume are inert clutter, not a
// correctness risk", on the grounds that containerReverifyFiles only ever
// trusts the filenames named in the envelope's files[]. That is true of
// containerReverifyFiles and false of the build. files[] is not a list the
// inner resolve chooses: scanArchivesDir (internal/cli/cmd_resolve.go) puts
// EVERY .deb sitting in /archives into it, and containerCrossCheckEnvelope
// then requires files[] and the plan to agree in both directions. So a .deb
// left here by an earlier build of a different target fails every later build
// on this machine -- any target, any architecture, any package list.
// Reproduced with seven .deb files from two distributions in this volume: a
// fresh ubuntu:24.04/minimal build asking only for jq failed on hello_2.10-3.
//
// The clutter is not inert and there is no flag, config key or environment
// variable that points a build at a different volume. Until there is, the
// remedy is `docker volume rm debark-archives-cache`, which
// containerStaleArchivesVolumeHint now says at the point of failure.
const containerDefaultStoreVolume = "debark-archives-cache"

// storeVolumeChoice reports whether /archives should be backed by a named
// volume instead of a bind mount, and which volume to use.
//
// FLIP POINT (E5), now flipped: docs/experiments/E5-container-io.md measured
// Docker Desktop for Windows with a 342 MB / 300-file store and found a
// named volume roughly 61x faster to write and 8x faster to read than a
// bind mount to a native Windows path (mean 6.9 MB/s -> 419 MB/s write,
// 119 MB/s -> 939 MB/s read) -- a multi-minute wait versus sub-second, not a
// rounding error. So Windows now defaults to a named volume.
//
// macOS was explicitly NOT measured by E5 (no macOS hardware available when
// it ran). It defaults to a named volume here too, on the same architectural
// reasoning E5's own "what this means for the design" section gives (Docker
// Desktop for Mac also crosses a host<->VM boundary for a bind mount, via
// virtiofs rather than WSL2) -- but that choice is INFERRED, not measured,
// and E5 explicitly cautions the *magnitude* on macOS should not be assumed
// to match Windows's. Re-run hack/experiments/e5-container-io.sh on macOS
// hardware before treating this default as settled there.
//
// Linux keeps the bind-mount default: a native Linux docker/podman engine
// has no host<->VM boundary at all, so a bind mount costs nothing extra
// there (E5 did not need to measure this to know it).
//
// An explicit ContainerOptions.StoreVolume always wins over this default, on
// every OS -- an operator's own choice of a persistent, reusable volume (or,
// via a bind-mount escape hatch built on StoreDir upstream of this backend,
// their own choice to keep the store visible on the host filesystem, which
// E5 notes a named volume gives up).
func (b *containerBackend) storeVolumeChoice() (volume string, useVolume bool) {
	if b.opts.StoreVolume != "" {
		return b.opts.StoreVolume, true
	}
	switch runtime.GOOS {
	case "windows", "darwin":
		return containerDefaultStoreVolume, true
	default:
		return "", false
	}
}

// --- Resolve -----------------------------------------------------------

// Resolve mounts the snapshot, work and archives directories (plus the
// external staging repo, when present) into a pinned image of the target
// release and re-enters this same binary as `debark resolve --backend
// local`, exactly per docs/dev/resolve-contract.md. It cross-checks the
// returned envelope's plan against its files list, re-verifies every staged
// file's digest against the bytes actually sitting on the host, and returns
// the resulting Plan with Resolver.Backend, Image and ImageDigest filled in
// from the container side (the inner process, believing itself to be the
// local backend, cannot know these).
func (b *containerBackend) Resolve(ctx context.Context, in ResolveInput) (*resolve.Plan, error) {
	if in.Snapshot == nil {
		return nil, dferr.Usagef("container: ResolveInput.Snapshot is nil")
	}
	if in.ArchivesDir == "" || in.WorkDir == "" {
		return nil, dferr.Usagef("container: ResolveInput.ArchivesDir and WorkDir are required")
	}
	// Required is not enough: both become mount sources, and a mount source
	// must be absolute. containerMountSource does reject a relative path, but
	// it runs several steps later -- after os.MkdirAll and after
	// containerStageSnapshotDir has already written snapshot.json -- so a
	// relative --work landed a directory tree in whatever directory the
	// process happened to be running from before failing. And with a named
	// store volume (Windows/macOS default) ArchivesDir never reaches
	// containerMountSource at all on the way in. Checking here means nothing
	// is created before the path is known to be usable.
	if err := containerCheckAbs(in.WorkDir, "ResolveInput.WorkDir"); err != nil {
		return nil, err
	}
	if err := containerCheckAbs(in.ArchivesDir, "ResolveInput.ArchivesDir"); err != nil {
		return nil, err
	}
	target := in.Snapshot.Target

	runtimeName, runtimePath, err := containerDetectRuntime(b.opts)
	if err != nil {
		return nil, err
	}
	_, image, imageDigest, imageWarn, err := containerResolveImage(b.opts, target.DistroID, target.VersionID, target.Codename)
	if err != nil {
		return nil, err
	}

	targetArch := in.Options.ArchOverride
	if targetArch == "" {
		targetArch = target.Arch
	}
	platform, err := containerPlatformFor(b.opts, targetArch)
	if err != nil {
		return nil, err
	}

	selfPath, err := containerSelfPath(b.opts, targetArch)
	if err != nil {
		return nil, err
	}
	if err := containerCheckMountSourceFile(selfPath, "SelfPath binary"); err != nil {
		return nil, err
	}
	if err := containerValidateBinary(selfPath, targetArch); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(in.WorkDir, 0o755); err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "container: create work dir")
	}
	if err := os.MkdirAll(in.ArchivesDir, 0o755); err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "container: create archives dir")
	}

	snapJSONHost, snapFilesHost, err := containerStageSnapshotDir(in.WorkDir, in.Snapshot, in.SnapshotFilesDir)
	if err != nil {
		return nil, err
	}

	binSrc, err := containerMountSource(selfPath)
	if err != nil {
		return nil, err
	}
	snapJSONSrc, err := containerMountSource(snapJSONHost)
	if err != nil {
		return nil, err
	}
	filesSrc, err := containerMountSource(snapFilesHost)
	if err != nil {
		return nil, err
	}
	workSrc, err := containerMountSource(in.WorkDir)
	if err != nil {
		return nil, err
	}

	mounts := []containerMount{
		{Source: binSrc, Target: "/debark", RO: true},
		{Source: snapJSONSrc, Target: "/snapshot/" + snapshot.DocumentName, RO: true},
		{Source: filesSrc, Target: "/snapshot/" + snapshot.FilesDir, RO: true},
		{Source: workSrc, Target: "/work", RO: false},
	}

	volumeName, useVolume := b.storeVolumeChoice()
	if useVolume {
		mounts = append(mounts, containerMount{Source: volumeName, Target: "/archives", RO: false})
	} else {
		archSrc, aerr := containerMountSource(in.ArchivesDir)
		if aerr != nil {
			return nil, aerr
		}
		mounts = append(mounts, containerMount{Source: archSrc, Target: "/archives", RO: false})
	}

	var externalTarget string
	if in.ExternalRepoDir != "" {
		if err := containerCheckMountSourceDir(in.ExternalRepoDir, "ExternalRepoDir"); err != nil {
			return nil, err
		}
		extSrc, eerr := containerMountSource(in.ExternalRepoDir)
		if eerr != nil {
			return nil, eerr
		}
		externalTarget = "/external"
		mounts = append(mounts, containerMount{Source: extSrc, Target: externalTarget, RO: true})
	}

	// The inner process's own event stream, forwarded to the operator's live
	// stream as it arrives. nil when this backend has nowhere to send one,
	// which also leaves the inner argv and the captured output exactly as
	// they were before forwarding existed. See containerevents.go, and
	// containerForwardEventTo for why these events reach the live stream and
	// never the bundle's evidence.json.
	//
	// ContainerOptions.Events has documented itself as receiving "apt events
	// proxied from inside" since it was written. This is the commit that
	// makes that true.
	onEvent := containerForwardEventTo(b.opts.Events)

	inner := containerBuildInnerArgv(containerResolveArgs{
		Packages:     in.Packages,
		ExternalRepo: externalTarget,
		JSONEvents:   onEvent != nil,
		// ExternalNames is BOTH what the inner resolve is asked to install
		// from the external repo AND, below, the only thing that entitles a
		// returned selection to call itself reason:external. See
		// containerCrossCheckEnvelope.
		ExternalNames: in.ExternalNames,
		// ResolveInput.Recommends is the effective, already-resolved value
		// (iface.go), so the flag is genuinely wanted here -- unlike
		// ClosedWorld, which has no such field. RecommendsSet says so
		// explicitly rather than letting a zero value ship as
		// "--recommends=false"; see containerBuildInnerArgv.
		Recommends:    in.Recommends,
		RecommendsSet: true,
		Upgrades:      in.Upgrades,
		Arch:          targetArch,
		ApprovedKeys:  in.ApprovedKeys,
	})
	runArgv := containerBuildRunArgv(platform, mounts, nil, image, inner)

	b.emit(evidence.TypeBackendSelected, map[string]any{
		"backend": "container", "runtime": runtimeName, "image": image,
		"image_digest": imageDigest, "platform": platform,
	})

	// The envelope is read back from a fixed path under WorkDir, and a file
	// already sitting there is indistinguishable from one this run wrote.
	// Measured: with a previous run's plan.json in place and a container that
	// exits 0 writing nothing, Resolve returned the PREVIOUS run's selections
	// as if they were this one's. The engine's own WorkDir is a fresh
	// os.MkdirTemp today, but WorkDir is a public field on a frozen input
	// struct with no such contract attached, and the cost of being wrong is a
	// bundle built from another request's plan. Remove it first so "the file
	// exists" can only mean "this run wrote it".
	planOutHost := filepath.Join(in.WorkDir, "plan.json")
	if rmErr := os.Remove(planOutHost); rmErr != nil && !os.IsNotExist(rmErr) {
		return nil, dferr.Wrap(dferr.Environment, rmErr,
			"container: remove a stale plan envelope before running; a leftover %s would be read back as this run's result", planOutHost)
	}

	res, err := execContainerFn(ctx, runtimePath, runArgv, onEvent)
	if err != nil {
		return nil, err
	}
	// Reported before the exit code is classified: a run that failed is
	// exactly the one whose refused events matter, and returning first would
	// mean the only place that can say so never runs.
	containerEmitForwardSummary(b.opts.Events, res.Events)
	if res.ExitCode != 0 && res.ExitCode != int(dferr.Incomplete) {
		return nil, containerClassifyRunResult(res, platform)
	}

	envBytes, rerr := os.ReadFile(planOutHost)
	if rerr != nil {
		return nil, dferr.Wrap(dferr.Verification, rerr,
			"container: read plan envelope (inner exit %d, %s)", res.ExitCode, containerLastLines(res.combined(), 6))
	}
	env, perr := containerParseEnvelope(envBytes)
	if perr != nil {
		return nil, perr
	}
	// in.ExternalNames, not the envelope, decides what may claim external
	// provenance. See containerCrossCheckEnvelope's doc comment for the
	// measurement.
	//
	// volumeName is passed as well, and is "" whenever useVolume is false
	// (storeVolumeChoice returns the two together). It changes no check: it
	// tells the cross-check whether /archives was a directory this run
	// created and owns or the shared cache every build on this machine writes
	// into, which is the difference between "a file is here that nobody asked
	// for" and "the cache is stale". See containerUnselectedFileError.
	if cerr := containerCrossCheckEnvelope(env, in.ExternalNames, volumeName); cerr != nil {
		return nil, cerr
	}

	if useVolume {
		if verr := containerCopyVolumeToHost(ctx, runtimePath, image, volumeName, in.ArchivesDir); verr != nil {
			return nil, verr
		}
	}
	if rverr := containerReverifyFiles(env, in.ArchivesDir); rverr != nil {
		return nil, rverr
	}

	plan := env.Plan
	// External selections are staged from ExternalRepoDir, never from
	// --archives, so containerReverifyFiles (which iterates the envelope's
	// files[], scoped to --archives) cannot see them at all. This is their
	// host-side re-verification.
	if rverr := containerReverifyExternalFiles(&plan, in.ExternalRepoDir); rverr != nil {
		return nil, rverr
	}
	if rwerr := containerRewriteStagedPaths(&plan, in.ArchivesDir, in.ExternalRepoDir); rwerr != nil {
		return nil, rwerr
	}
	plan.Resolver.Backend = lock.BackendContainer
	plan.Resolver.Image = image
	// imageDigest, never release.ImageDigest: under an --image override the
	// distro row's digest belongs to an image that never ran. See
	// containerResolveImage.
	plan.Resolver.ImageDigest = imageDigest
	if imageWarn != "" {
		plan.Warnings = append(plan.Warnings, lock.Warning{Code: containerWarnUnpinnedImage, Message: imageWarn})
	}
	if res.ExitCode == int(dferr.Incomplete) && len(plan.Unresolved) == 0 {
		// Contract: "The caller must read the envelope on 3 as well as 0."
		// The inner process classified this run 3 (incomplete) against the
		// frozen exit table, and a backend does not get to downgrade a code
		// that table defines.
		//
		// This used to append a warning and return (plan, nil), which is
		// exactly that downgrade: the engine derives its own exit class from
		// lockDoc.Unresolved (core/engine), the envelope's unresolved list is
		// empty by construction on this branch, so nothing downstream had any
		// remaining trace of the 3 and the operator was told exit 0 -- a
		// clean build -- for a run the process that did the resolving called
		// incomplete. Neither a warning in the lock nor a line on stderr
		// changes the exit code a script reads.
		//
		// A synthesised lock.Unresolved entry was the alternative, and is
		// worse: it would have to name an input as unresolved, and this
		// branch exists precisely because the envelope names none, so it
		// would put a guess into the signed record. An error carrying the
		// class the inner process itself chose says only what is known.
		return nil, dferr.New(dferr.Incomplete,
			"container: inner debark resolve exited %d (incomplete) but the envelope's plan.unresolved is empty, "+
				"so this build cannot say which inputs are missing; treating the run as incomplete rather than reporting success: %s",
			res.ExitCode, containerLastLines(res.combined(), 6))
	}
	return &plan, nil
}

// --- ClosedWorld ---------------------------------------------------------

// ClosedWorld implements the ADR-013 fallback named in the contract brief: when
// the host has no usable apt at all (macOS, Windows, or any host where
// SelectBackend chose the container backend in the first place), the
// pre-export closed-world proof (ADR-007) runs inside a
// container of the target release with --network none, instead of on the
// host. Running it directly on the host, with the host's own matching apt,
// is the LOCAL backend's job -- by construction, if this backend
// is the one being asked, host apt was already judged unsuitable, so this
// method always uses a container; it does not re-check host apt itself.
//
// Bridging note (reported, not improvised silently -- see this package's
// report): docs/dev/resolve-contract.md defines `debark resolve`'s flags
// precisely, but names no separate closed-world command or flag. Its
// --external-repo/--external-name ADD a repo to the private root's normal,
// still-online sources rather than REPLACING them, unlike core/apt's own
// RootSpec.CopySnapshotSources=false + a single ExtraSource, which is how
// the local backend's closed-world path gets true source isolation. This
// method reuses `debark resolve --backend local` with --external-repo
// pointed at the finished bundle's repo/ directory and --network none on
// the *outer* `docker run`, as the closest contract-legal approximation:
// with the network cut, the still-configured online sources cannot be
// reached regardless of whether they are present, so only the local
// file://-repo entries can resolve anything -- closed-world in effect, if
// not in the private root's literal source list. A dedicated flag on
// `debark resolve` (e.g. --no-target-sources) would close this gap
// cleanly; recommended in the implementation notes.
//
// E3 fix (docs/experiments/E3-pin-fidelity.md, mirroring closedworld.go's
// localBackend.ClosedWorld -- see that method's own doc comment for the
// full finding): this method never passes --upgrades to the inner resolve.
// --upgrades would ask the inner process to run a real, unpinned
// full-upgrade, which is exactly the free, highest-version-wins resolution
// E3 found can silently pick a version the target's own pin was written to
// reject. Instead this method builds one wantVersions map -- name:arch ->
// the version the lock recorded -- covering BOTH in.Install (already
// name:arch=version, the exact set the production install command will
// request) and, when in.Upgrades is set, the finished bundle's own
// lock.json Reason: "upgrade" packages (closedWorldUpgradeTargets, shared
// with the local backend -- same package, same function, so the two
// backends can never drift apart on what "the locked set" means). Every
// wantVersions key is requested from the inner resolve via bare
// --external-name "name:arch" (not --package name:arch=version:
// --external-name's contract grammar carries no version anyway, and
// local.go's own requested-vs-external bookkeeping, reqContains/roots keyed
// by bare Name, mislabels -- though does not break -- an arch-qualified
// --package token, so this method deliberately does not attempt that; see
// the implementation notes), forcing the inner resolve to find a candidate for
// every one of them from the bundle alone.
//
// That bare request is not itself exact-version, so after the inner resolve
// returns this method verifies every wantVersions entry explicitly
// (containerVersionMismatches, below) in two layers:
//
//  1. For every package the resolve actually proposed touching (a Selection
//     exists for it), containerUpgradeMismatches adapts resolve.Plan's
//     Selections into the shape closedworld.go's own upgradeMismatches
//     already compares against a want map, and calls it directly -- one
//     implementation of "does this version equal the locked one", shared
//     with the local backend, not a second that could drift.
//  2. For every wantVersions entry the resolve did NOT touch at all (no
//     Inst/Conf action, because apt's bare-name candidate already matched
//     whatever the private root's copy of the target's own dpkg status
//     said was installed): this is the case a bare-name request can hide a
//     divergence in that upgradeMismatches structurally cannot see, since
//     it only judges packages present in a Selection. If the target simply
//     already has the exact locked version, silence is correct and this
//     method makes no complaint (matching the local backend's own "no
//     action is not a mismatch" reading of an exact-version simulate that
//     needed nothing done). But if the target's dpkg status shows some
//     OTHER version installed -- meaning the bare-name resolve's silence
//     reflects "the target already has *something*", not "the target
//     already has what the lock wants" -- that is a divergence local's
//     exact-version request could never produce (an exact "name=version"
//     request either changes the target to that version or fails outright)
//     but that this method's own request shape structurally can, so it is
//     verified here by cross-checking the SAME snapshot dpkg status the
//     private root itself was seeded from (containerInstalledVersions,
//     below) -- entirely host-side, no second container round trip.
//
// Both layers name the package and both versions on a mismatch, and say
// which layer caught it -- "the bundle-only resolve would install a
// different version" versus "the bundle-only resolve proposed no change,
// but the target's own recorded state does not already match" -- because,
// as the task that added this second layer put it, those mean different
// things to an operator even though both are closed-world failures.
//
// Net effect, stated plainly because it is the one sentence an operator
// needs and a clean claim here would not survive scrutiny: after both
// layers, this method verifies the exact locked version of everything in
// wantVersions (the whole install set, and the whole upgrade set when
// requested) -- either from what the bundle-only resolve proposed, or from
// the target's own captured current state when it proposed nothing. Nothing
// in wantVersions is accepted on bare-name resolvability alone any more.
// What is still genuinely different from the local backend is the
// MECHANISM, not the coverage: local gets its guarantee structurally, by
// construction of the request it sends apt (an exact-version ask that
// cannot silently return a different version); this method gets the same
// guarantee by verifying the response instead, against two different
// sources of truth depending on what the response contains. The two are
// equivalent for every case this package's tests construct, including the
// real container round trip in container_e2e_test.go, but they are not the
// same code path, and a future change to either should not assume they are.
func (b *containerBackend) ClosedWorld(ctx context.Context, in ClosedWorldInput) (lock.ClosedWorld, error) {
	if in.Snapshot == nil {
		return lock.ClosedWorld{}, dferr.Usagef("container: ClosedWorldInput.Snapshot is nil")
	}
	if in.WorkDir == "" || in.BundleRepoDir == "" {
		return lock.ClosedWorld{}, dferr.Usagef("container: ClosedWorldInput.WorkDir and BundleRepoDir are required")
	}
	// Same reasoning as Resolve's guard above: WorkDir is a mount source and
	// also the parent of the staged snapshot and the closed-world archives
	// directory, both of which are created before any mount source is
	// converted.
	if err := containerCheckAbs(in.WorkDir, "ClosedWorldInput.WorkDir"); err != nil {
		return lock.ClosedWorld{}, err
	}
	if err := containerCheckAbs(in.BundleRepoDir, "ClosedWorldInput.BundleRepoDir"); err != nil {
		return lock.ClosedWorld{}, err
	}
	// Mirrors localBackend.ClosedWorld's own guard (closedworld.go), and for
	// the same reason: with nothing to install there is nothing this check
	// can prove, and "ok" is a claim about a check that ran. The engine reads
	// the two outcomes very differently -- core/engine/finalize.go warns and
	// records a closed-world.skipped warning for skipped, and deliberately
	// does NOT let it stand as a passed check -- so a container build
	// returning ok here was the one path that could put "closed-world: ok"
	// into a signed lock for an install set nothing was ever asked about.
	// Placed after the WorkDir/BundleRepoDir shape checks so a malformed path
	// is still a Usage error rather than a silent skip.
	if len(in.Install) == 0 {
		return lock.ClosedWorld{Result: lock.ClosedWorldSkipped, Detail: "install set is empty"}, nil
	}
	target := in.Snapshot.Target

	// Fixed before anything is recorded, so every return below -- ok, failed
	// and the "could not run" cases alike -- digests the same masked view.
	// See closedWorldMasks (closedworld.go) for the full finding; the two
	// container-only entries below are this backend's own additions to it.
	bundleRepoAbs, aerr := filepath.Abs(in.BundleRepoDir)
	if aerr != nil {
		return lock.ClosedWorld{}, dferr.Wrap(dferr.Usage, aerr, "container: closed-world: bundle repo dir")
	}
	masks := closedWorldMasks(in, bundleRepoAbs)

	_, runtimePath, err := containerDetectRuntime(b.opts)
	if err != nil {
		return lock.ClosedWorld{}, err
	}
	_, image, imageDigest, _, err := containerResolveImage(b.opts, target.DistroID, target.VersionID, target.Codename)
	if err != nil {
		return lock.ClosedWorld{}, err
	}
	platform, err := containerPlatformFor(b.opts, target.Arch)
	if err != nil {
		return lock.ClosedWorld{}, err
	}
	selfPath, err := containerSelfPath(b.opts, target.Arch)
	if err != nil {
		return lock.ClosedWorld{}, err
	}
	if err := containerCheckMountSourceFile(selfPath, "SelfPath binary"); err != nil {
		return lock.ClosedWorld{}, err
	}
	if err := containerValidateBinary(selfPath, target.Arch); err != nil {
		return lock.ClosedWorld{}, err
	}
	// The two run-varying paths closedWorldMasks does not know about,
	// because only this backend has them. Both are in res.Argv verbatim:
	// containerBuildRunArgv puts the mount source for SelfPath in a -v spec,
	// and execContainer prepends the runtime's own absolute exec path
	// (C:\Program Files\Docker\...\docker.exe here, /usr/bin/docker there).
	// Neither says anything about what was requested, and SelfPath in
	// particular is not scrubbed by the engine's own scrubHostPaths on the
	// way out -- and scrubbing could not have helped anyway, because by the
	// time the lock exists these paths are already inside a SHA-256.
	//
	// Appended after the shared masks rather than before them so that a
	// SelfPath or a runtime that happens to live under WorkDir still has its
	// WorkDir prefix replaced first -- same "longest varying prefix wins"
	// ordering rule closedWorldMasks documents for WorkDir vs BundleRepoDir.
	masks = append(masks,
		closedWorldPathMask{selfPath, "<SELF-BINARY>"},
		closedWorldPathMask{runtimePath, "<CONTAINER-RUNTIME>"},
	)
	// And one more spelling, which is this backend's alone and which
	// maskClosedWorldPaths therefore cannot be expected to know about
	// (its own doc enumerates the four spellings APT produces): the
	// docker/podman bind-mount source. containerMountSource rewrites a
	// Windows drive-letter path into the form docker itself demands --
	// forward slashes AND a LOWERCASED drive letter -- so
	// "C:\Users\...\work" enters the argv as "c:/Users/.../work", matching
	// neither the native form (backslashes) nor the forward-slashed one
	// (capital C). Measured while writing the digest test for this change:
	// with all four shared spellings masked, two runs of the identical
	// request STILL produced different CommandDigests, and the surviving
	// difference was exactly these -v specs. On a Linux host the conversion
	// is a no-op and every entry here is skipped.
	//
	// Derived from the list above rather than hand-written, and in the same
	// order, so a path added to closedWorldMasks upstream gets its
	// mount-source spelling for free instead of quietly going unmasked.
	mountForms := make([]closedWorldPathMask, 0, len(masks))
	for _, m := range masks {
		if m.path == "" {
			continue
		}
		src, serr := containerMountSource(m.path)
		if serr != nil || src == m.path || src == filepath.ToSlash(m.path) {
			continue
		}
		mountForms = append(mountForms, closedWorldPathMask{src, m.placeholder})
	}
	masks = append(masks, mountForms...)
	if err := os.MkdirAll(in.WorkDir, 0o755); err != nil {
		return lock.ClosedWorld{}, dferr.Wrap(dferr.Environment, err, "container: create work dir")
	}
	if err := containerCheckMountSourceDir(in.BundleRepoDir, "BundleRepoDir"); err != nil {
		return lock.ClosedWorld{}, err
	}

	// wantVersions is name:arch -> the version the lock recorded, for
	// EVERY package this check must confirm resolves to that exact
	// version: the install set (in.Install, already name:arch=version --
	// the exact set the production install command will request) plus,
	// when in.Upgrades, the bundle's own lock.json Reason: "upgrade"
	// packages (closedWorldUpgradeTargets, shared with the local backend --
	// see the method doc comment above). Both are checked the same way,
	// below, because both need the identical guarantee: the production
	// install command asks for in.Install exactly as much as it asks for
	// the upgrade set.
	//
	// An entry that is not "name=version" is refused, not dropped. The
	// previous `if ok && key != ""` skipped it silently, and silently is the
	// whole problem: the package then never reached externalNames, so the
	// inner resolve was never asked for it, so nothing ever checked it -- and
	// the method still returned ClosedWorldOK, certifying a bundle for an
	// install entry it had not looked at. (localBackend.ClosedWorld cannot
	// have this defect: it passes in.Install verbatim to `apt-get -s
	// install`, so a malformed entry is apt's error, loudly.) Every real
	// caller builds Install from the lock's own name:arch=version packages,
	// so an entry without "=" means a caller bug or a hand-edited input, and
	// Usage is the class for both.
	wantVersions := make(map[string]string, len(in.Install))
	for _, nv := range in.Install {
		key, version, ok := strings.Cut(nv, "=")
		if !ok || key == "" || version == "" {
			return lock.ClosedWorld{}, dferr.New(dferr.Usage,
				"container: closed-world: install entry %q is not name[:arch]=version; "+
					"this check verifies the exact locked version of every install entry and cannot do that for one that names none", nv)
		}
		wantVersions[key] = version
	}
	if in.Upgrades {
		// closedWorldUpgradeTargets' first return is "name:arch=version"
		// entries, shaped for local backend's exact-version "apt-get -s
		// install" argv; this backend only needs its second return, the
		// name:arch -> version map, to merge into wantVersions.
		_, upgradeWant, uerr := closedWorldUpgradeTargets(bundleRepoAbs)
		if uerr != nil {
			return lock.ClosedWorld{}, uerr
		}
		for key, version := range upgradeWant {
			wantVersions[key] = version
		}
	}

	// externalNames: every wantVersions key, by bare "name:arch" -- see the
	// method doc comment above for why this is --external-name and not an
	// exact-version --package entry.
	externalNames := make([]string, 0, len(wantVersions))
	for key := range wantVersions {
		externalNames = append(externalNames, key)
	}
	sort.Strings(externalNames)

	// installedVersions closes the gap a bare-name request leaves open (see
	// the method doc comment's second verification layer): the SAME
	// snapshot dpkg status the private root itself is seeded from, read
	// host-side, no container involved. Only fetched when there is
	// something in wantVersions to possibly need it for.
	var installedVersions map[string]string
	if len(wantVersions) > 0 {
		installedVersions, err = containerInstalledVersions(in.Snapshot, in.SnapshotFilesDir)
		if err != nil {
			return lock.ClosedWorld{}, err
		}
	}

	snapJSONHost, snapFilesHost, err := containerStageSnapshotDir(in.WorkDir, in.Snapshot, in.SnapshotFilesDir)
	if err != nil {
		return lock.ClosedWorld{}, err
	}
	archivesDir := filepath.Join(in.WorkDir, "closedworld-archives")
	if err := os.MkdirAll(archivesDir, 0o755); err != nil {
		return lock.ClosedWorld{}, dferr.Wrap(dferr.Environment, err, "container: create closed-world archives dir")
	}

	binSrc, err := containerMountSource(selfPath)
	if err != nil {
		return lock.ClosedWorld{}, err
	}
	snapJSONSrc, err := containerMountSource(snapJSONHost)
	if err != nil {
		return lock.ClosedWorld{}, err
	}
	filesSrc, err := containerMountSource(snapFilesHost)
	if err != nil {
		return lock.ClosedWorld{}, err
	}
	workSrc, err := containerMountSource(in.WorkDir)
	if err != nil {
		return lock.ClosedWorld{}, err
	}
	archSrc, err := containerMountSource(archivesDir)
	if err != nil {
		return lock.ClosedWorld{}, err
	}
	repoSrc, err := containerMountSource(in.BundleRepoDir)
	if err != nil {
		return lock.ClosedWorld{}, err
	}

	mounts := []containerMount{
		{Source: binSrc, Target: "/debark", RO: true},
		{Source: snapJSONSrc, Target: "/snapshot/" + snapshot.DocumentName, RO: true},
		{Source: filesSrc, Target: "/snapshot/" + snapshot.FilesDir, RO: true},
		{Source: workSrc, Target: "/work", RO: false},
		{Source: archSrc, Target: "/archives", RO: false},
		{Source: repoSrc, Target: "/bundlerepo", RO: true},
	}

	// ClosedWorldInput carries no Recommends field, so this method has to
	// choose one, and it chooses the same value localBackend.ClosedWorld's
	// RootSpec does: Recommends: true (closedworld.go). The two backends run
	// the same check against the same bundle and must not disagree about the
	// dependency closure they resolve within it; a recommends policy that
	// differs between them would make one backend's "ok" mean something the
	// other's does not. As closedworld.go's own note says, exact-version pins
	// do not depend on this either way, which is why the choice is safe to
	// fix rather than plumb through the frozen input struct.
	//
	// This is deliberate and explicit, which is the change: the comment here
	// used to claim --recommends was "left unset" and that the inner process
	// fell back to its documented default -- but containerBuildInnerArgv
	// appended fmt.Sprintf("--recommends=%t", a.Recommends) unconditionally,
	// so the zero value shipped "--recommends=false" on every container
	// closed-world run. The container check was resolving a narrower closure
	// than the local one, silently, while the comment said otherwise.
	// RecommendsSet now makes "unset" expressible; this call sets it.
	//
	// Upgrades is deliberately never passed here: it would ask the inner
	// resolve for an unpinned full-upgrade, exactly the free resolution E3
	// found unsafe against a flat bundle.
	inner := containerBuildInnerArgv(containerResolveArgs{
		ExternalRepo:  "/bundlerepo",
		ExternalNames: externalNames,
		Recommends:    true,
		RecommendsSet: true,
		Arch:          target.Arch,
	})
	runArgv := containerBuildRunArgv(platform, mounts, []string{"--network", "none"}, image, inner)

	b.emit(evidence.TypeClosedWorld, map[string]any{
		"backend": "container", "image": image, "image_digest": imageDigest, "network": "none",
	})

	// Same stale-envelope reasoning as Resolve's: this method reads its
	// result back from a fixed <WorkDir>/plan.json, and a leftover file there
	// is indistinguishable from one this run wrote. Measured on the Resolve
	// path (a pre-existing plan.json plus a container exiting 0 without
	// writing one returned the previous run's selections); the same hole is
	// here, and here it would certify a bundle against another run's plan.
	planOutHost := filepath.Join(in.WorkDir, "plan.json")
	if rmErr := os.Remove(planOutHost); rmErr != nil && !os.IsNotExist(rmErr) {
		return lock.ClosedWorld{}, dferr.Wrap(dferr.Environment, rmErr,
			"container: closed-world: remove a stale plan envelope before running; a leftover %s would be read back as this run's result", planOutHost)
	}

	// nil, deliberately: this run's captured output is digested into
	// lock.ClosedWorld and reaches the bundle id and the signature. See
	// containerForwardEventTo.
	res, err := execContainerFn(ctx, runtimePath, runArgv, nil)
	if err != nil {
		return lock.ClosedWorld{}, err
	}

	// Every recorded value from here on goes through closedWorldResult --
	// the SAME function the local backend records with, over the same
	// closedWorldRun shape -- rather than hashing res.Argv and res.combined()
	// directly.
	//
	// Why. The direct version was `digest.Bytes([]byte(strings.Join(res.Argv,
	// "\x00")))` over the raw `docker run` argv, and that argv is nothing but
	// host paths: the absolute path to this machine's docker/podman binary,
	// plus -v mount specs naming WorkDir (a fresh os.MkdirTemp
	// "debark-build-*" on every single run), BundleRepoDir, SnapshotFilesDir
	// and SelfPath. Measured: two ClosedWorld runs with identical inputs in
	// two temp directories produced CommandDigest abf7d38d... and 8debae9a...
	// -- and CommandDigest reaches lock.ClosedWorld -> lock.json -> LockDigest
	// -> manifest.NewBundleID -> the signature, so the byte-identical
	// rebuild claim was false for EVERY container build, which is every macOS
	// build, every Windows build and every cross-release build. The engine's
	// scrubHostPaths cannot reach this: it rewrites strings in the lock, and
	// by then these are hashes.
	//
	// This is the identical defect closedworld.go found and fixed for the
	// local backend, so it is fixed by calling that fix rather than by
	// writing a second masking pass here that could drift from it -- most
	// concretely, maskClosedWorldPaths also knows apt's list-file spelling of
	// a path (every "/" replaced by "_"), which a hand-rolled version here
	// would have missed and which the local backend needed a separate
	// measurement to discover. Detail is masked for the same reason and is
	// not a formality: docker's own mount errors quote the host paths back at
	// you, and SelfPath is not scrubbed by the engine on the way out.
	//
	// closedWorldResult also digests the argv with canonical.Digest over a
	// [][]string, so a CommandDigest from this backend and one from the local
	// backend are computed the same way over the same shape and mean the same
	// thing.
	runs := []closedWorldRun{{
		argv: res.Argv,
		out:  Output{Argv: res.Argv, Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode},
	}}

	switch {
	case res.ExitCode == 0:
		envBytes, rerr := os.ReadFile(planOutHost)
		if rerr != nil {
			return closedWorldResult(runs, masks, lock.ClosedWorldFailed,
				fmt.Sprintf("exit 0 but no plan envelope written: %v", rerr)), nil
		}
		env, perr := containerParseEnvelope(envBytes)
		if perr != nil {
			return closedWorldResult(runs, masks, lock.ClosedWorldFailed, perr.Error()), nil
		}
		if len(env.Plan.Unresolved) > 0 {
			return closedWorldResult(runs, masks, lock.ClosedWorldFailed,
				fmt.Sprintf("%d input(s) unresolved against the bundle alone", len(env.Plan.Unresolved))), nil
		}
		if len(wantVersions) > 0 {
			// The inner resolve was only ever asked for these packages by
			// bare name (see the method doc comment above), so nothing
			// upstream of this point already enforced their exact version --
			// this comparison is the check, not a redundant second line of
			// defence the way it is for the local backend.
			if mismatches := containerVersionMismatches(wantVersions, env.Plan.Selections, installedVersions); len(mismatches) > 0 {
				return closedWorldResult(runs, masks, lock.ClosedWorldFailed,
					"closed-world check diverged from the lock's exact versions (E3, docs/experiments/E3-pin-fidelity.md): "+strings.Join(mismatches, "; ")), nil
			}
		}
		// The stray-network-URI scan localBackend.ClosedWorld has run for as
		// long as it has had a closed world to check (closedworld.go: "a
		// defence against a future change accidentally widening the
		// sources"), and it belongs here MORE than it does there, not less.
		// The local check is closed by construction -- its private root's
		// only configured source is a file: URI. This one is not: the inner
		// `debark resolve --external-repo /bundlerepo` ADDS the bundle to
		// the target's normal, still-online sources rather than replacing
		// them (see this method's bridging note above), so the single thing
		// standing between it and the network is the outer `docker run
		// --network none`. If that flag is ever dropped, reordered out of
		// containerBuildRunArgv's extraFlags, or defeated by a runtime that
		// ignores it, this check would keep passing while quietly resolving
		// against the internet -- and would certify the bundle as
		// self-contained on the strength of it.
		//
		// Scanned over the masked output, so what is scanned is exactly what
		// OutputDigest records; masking only ever replaces host paths, which
		// cannot contain a URI scheme.
		combined := maskClosedWorldPaths(string(res.combined()), masks)
		lower := strings.ToLower(combined)
		if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") || strings.Contains(lower, "ftp://") {
			return closedWorldResult(runs, masks, lock.ClosedWorldFailed,
				"closed-world check referenced a network URI unexpectedly"), nil
		}
		return closedWorldResult(runs, masks, lock.ClosedWorldOK, ""), nil
	case res.ExitCode == int(dferr.Incomplete):
		return closedWorldResult(runs, masks, lock.ClosedWorldFailed,
			fmt.Sprintf("incomplete: %s", containerLastLines(res.combined(), 4))), nil
	default:
		return closedWorldResult(runs, masks, lock.ClosedWorldFailed,
			containerLastLines(res.combined(), 4)), nil
	}
}

// containerUpgradeMismatches adapts a resolve.Plan's Selections (what the
// inner, container-run `debark resolve` actually chose from the bundle
// alone) into the InstAction/SimulateResult shape closedworld.go's
// upgradeMismatches already knows how to compare against a want map of
// locked versions -- so this backend shares that exact comparison (message
// text included) with the local backend rather than re-implementing it a
// second time that could drift from the first (the contract brief). want is no
// longer only the upgrade set (see ClosedWorld's doc comment above); the
// comparison logic itself never cared -- it only judges keys present in
// want, so a Selection for anything else is harmlessly ignored exactly as
// it would be for local's simulated apt-get output. One accepted wording
// wrinkle from this reuse: upgradeMismatches' own message says "as the
// upgrade target" unconditionally, which is imprecise for an ordinary
// in.Install entry that was never an upgrade at all; that message's text
// belongs to closedworld.go, not this file, so it is reused verbatim rather
// than patched here -- the package name and both versions it names are
// always correct regardless.
func containerUpgradeMismatches(want map[string]string, sels []resolve.Selection) []string {
	actions := make([]InstAction, 0, len(sels))
	for _, s := range sels {
		actions = append(actions, InstAction{Kind: "Inst", Name: s.Name, Arch: s.Arch, Version: s.Version})
	}
	return upgradeMismatches(want, SimulateResult{Actions: actions})
}

// containerVersionMismatches is ClosedWorld's full version verification: it
// layers a second check on top of containerUpgradeMismatches to close the
// gap a bare-name request leaves that an exact-version request structurally
// cannot have (see ClosedWorld's doc comment above for the full reasoning).
//
//  1. containerUpgradeMismatches catches every want key the inner resolve
//     actually proposed touching (a Selection exists) at the wrong version.
//  2. For every want key NOT touched at all -- no Selection, so
//     containerUpgradeMismatches cannot see it either way -- this checks
//     installed (the SAME snapshot dpkg status the private root itself was
//     seeded from, read host-side by containerInstalledVersions). Silence
//     from the bare-name resolve is only trustworthy when the target's own
//     recorded state already matches what the lock wants; if installed
//     shows a different version, or shows the package not installed at all,
//     that is a real divergence a bare-name request can hide but an
//     exact-version one never could, so it is reported here, worded
//     differently from #1 on purpose: an operator needs to know whether the
//     bundle offered the wrong version, or offered no confirmed candidate
//     for the right one at all.
//
// installed may be nil (nothing to cross-check against, e.g. the snapshot
// carried no captured dpkg status): every want key without a Selection then
// falls through to the "not installed" wording, which is correct -- a nil
// map has no entries to match.
func containerVersionMismatches(want map[string]string, sels []resolve.Selection, installed map[string]string) []string {
	mismatches := containerUpgradeMismatches(want, sels)

	touched := make(map[string]bool, len(sels))
	for _, s := range sels {
		touched[s.Key()] = true
	}
	keys := make([]string, 0, len(want))
	for key := range want {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if touched[key] {
			continue // already judged above by containerUpgradeMismatches
		}
		wantVersion := want[key]
		switch instVersion, ok := installed[key]; {
		case ok && instVersion == wantVersion:
			// Confirmed independently of the bundle-only resolve: the
			// target's own captured dpkg status already has exactly the
			// locked version, which is why the bare-name resolve proposed
			// nothing for it. Not a mismatch.
		case ok:
			mismatches = append(mismatches, fmt.Sprintf(
				"%s: the lock recorded %s, but the bundle-only resolve proposed no change for it and the target's captured dpkg status shows %s installed instead",
				key, wantVersion, instVersion))
		default:
			mismatches = append(mismatches, fmt.Sprintf(
				"%s: the lock recorded %s, but the bundle-only resolve found no candidate for it and the target's captured dpkg status does not show it installed",
				key, wantVersion))
		}
	}
	sort.Strings(mismatches)
	return mismatches
}

// containerInstalledVersions reads the snapshot's own captured dpkg status
// -- the same file BuildPrivateRoot copies into every private root this
// package builds, local or container -- and returns every currently
// installed package's version, keyed name:arch. Entirely host-side: no
// container, no second inner resolve. Deliberately not scoped to held
// packages the way held.go's heldPackageVersions is (a different, narrower
// question); this wants every installed package's actual version,
// regardless of hold state, as the ground truth containerVersionMismatches
// cross-checks a silent bare-name resolve against.
//
// A snapshot with no captured dpkg status at all (DpkgStatus.ArchivePath
// empty) returns an empty map and no error, matching heldPackageVersions'
// own reading of that case: BuildPrivateRoot already treats it as "resolve
// as if nothing were installed", so every want key would legitimately need
// an explicit fetch and show up as a Selection instead of falling through
// to this lookup at all. A status file that IS captured but cannot be read
// back is a hard error, not a silent empty map -- this backend must not
// silently report "verified" for something it could not actually check.
func containerInstalledVersions(snap *snapshot.Snapshot, filesDir string) (map[string]string, error) {
	installed := map[string]string{}
	if snap == nil || snap.DpkgStatus.ArchivePath == "" {
		return installed, nil
	}
	data, err := readSnapshotFile(filesDir, snap.DpkgStatus.ArchivePath)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "container: closed-world: read the snapshot's dpkg status")
	}
	for _, stanza := range splitDpkgStatusStanzas(data) {
		name := stanza["Package"]
		arch := stanza["Architecture"]
		if name == "" || arch == "" {
			continue
		}
		// dpkg's Status field is "<want> <flag> <status>". Any want (not
		// just "install") counts here as long as dpkg still considers the
		// package installed -- unlike heldPackageVersions, this is not
		// scoped to holds.
		fields := strings.Fields(stanza["Status"])
		if len(fields) == 3 && fields[2] == "installed" {
			installed[name+":"+arch] = strings.TrimSpace(stanza["Version"])
		}
	}
	return installed, nil
}
