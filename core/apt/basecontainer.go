package apt

// This file is the container half of base-snapshot synthesis (ADR-014). It
// is deliberately NOT a Backend method, and iface.go records the reasoning
// where Backend declines to grow a third one; the short version is that a
// Windows or macOS host -- precisely the host that has no backend but the
// container -- has no Debian/Ubuntu archive keyring, so it can neither build
// the bootstrap root a base definition describes nor supply the keyrings the
// finished snapshot must carry for a later `build` to use it. There is
// therefore nothing for a container implementation of "resolve this closure"
// to receive, and nothing useful for it to hand back short of the whole
// snapshot. So the whole operation crosses the boundary: the container runs
// `debark snapshot from-base` end to end and one snapshot archive comes
// out.
//
// The consequence for this file is that it is much SMALLER than
// container.go's Resolve, and every deletion is load-bearing rather than a
// simplification:
//
//   - No envelope. What comes back is a snapshot archive, and the host
//     validates it by opening it with snapshot.Open -- per-file digests,
//     keyring fingerprint re-derivation, schema. That is a far stronger
//     check than any hand-written cross-check here could be, and it is code
//     that already exists and is already exercised by every captured
//     snapshot. Writing a weaker second validator here would be the "a fake
//     can make a test vacuous -- check whether your fake can express the
//     failure you are asserting against" trap this project has already paid
//     for repeatedly. This file therefore checks that a non-empty regular
//     file arrived and stops.
//   - No /archives mount and no store volume, because nothing is
//     downloaded: a base closure resolves a package list and fetches none of
//     it (closure.go's package comment).
//   - No /snapshot mount, because there is no input snapshot. That is the
//     whole point of the operation.

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/snapshot"
)

// containerBaseDefinitionTarget is the fixed path an operator-supplied base
// definition file is mounted at inside the container.
//
// Fixed rather than derived from the host filename for the same reason the
// snapshot mount is: the inner argv must not carry a host path. A host
// filename would leak the builder's directory layout onto a command line
// that is captured in evidence, and — worse — a name that is meaningful on
// the host may not be a legal single path element in the container's view.
// One constant, used by the mount and by the argv, so the two cannot drift.
//
// The ".yaml" suffix is not decoration: core/base.LoadFile parses these as
// YAML (docs/formats.md §3.7), and a definition arriving under a name with
// no extension would be an unnecessary difference from every other way that
// file is read.
const containerBaseDefinitionTarget = "/base/definition.yaml"

// BaseContainerInput is one base-snapshot synthesis run inside a container.
//
// It is a plain struct rather than ContainerOptions plus a second argument
// because this operation is not a Backend method and has no long-lived
// backend value behind it: core/base.Synthesize calls it once, with
// everything it knows, and gets a path back. The four container knobs at the
// bottom are the same four ContainerOptions carries, and are copied into one
// below so every helper in container_driver.go sees exactly the shape it
// already expects.
type BaseContainerInput struct {
	// BaseRef is the operator's --base argument: a builtin base id such as
	// "ubuntu:26.04/desktop", or a host path to a definition file.
	BaseRef string
	// BaseIsFile is true when BaseRef is a host path that must be mounted
	// into the container read-only; false when it is a builtin id, which the
	// inner binary already has compiled in.
	//
	// Passed in rather than re-derived here (by, say, calling
	// base.IsBuiltinID or stat-ing the path) so that ONE piece of code
	// decides what the operator's string meant. core/base.Resolve is that
	// code, and its rule — a builtin id wins outright, so a file lying about
	// in the working directory can never silently redefine a documented base
	// — is a decision this package must inherit, not re-take. Two
	// independent answers to "is this a file?" is exactly how a host that
	// resolved a builtin ends up mounting a file, or the reverse.
	BaseIsFile bool
	// Target is the identity the base describes; it selects the image
	// (core/distro.Resolve) and the --platform.
	Target snapshot.Target
	// WorkDir is a host directory mounted at /work; the archive is written
	// inside it.
	WorkDir string
	// OutName is the archive's base name inside WorkDir.
	OutName string

	Image    string // override, as ContainerOptions.Image
	Runtime  string // docker or podman; empty autodetects
	Platform string // override
	SelfPath string // the binary to mount and re-enter
	Events   evidence.Sink
}

// BaseSnapshotInContainer runs `debark snapshot from-base` inside a pinned
// image of the target release and returns the host path of the snapshot
// archive it wrote.
//
// It follows containerBackend.Resolve (container.go) step for step — the
// same order, the same guard rails, the same error classes — because the two
// are the same operation shape: validate what can be validated before
// anything is created, resolve the image and platform, prove the binary
// about to be mounted can actually execute under that platform, mount, run,
// classify the exit, read the result back. Where it differs from Resolve it
// differs deliberately, and this file's opening comment says where and why.
//
// The returned path is NOT a validated snapshot. It is a file that exists,
// is regular and is not empty. The caller opens it with snapshot.Open, and
// that is where the real check happens: this function has no business
// re-implementing a weaker copy of it.
func BaseSnapshotInContainer(ctx context.Context, in BaseContainerInput) (string, error) {
	if in.BaseRef == "" {
		return "", dferr.Usagef("container: BaseContainerInput.BaseRef is required")
	}
	if in.Target.Arch == "" {
		return "", dferr.Usagef("container: BaseContainerInput.Target has no architecture")
	}
	if in.WorkDir == "" {
		return "", dferr.Usagef("container: BaseContainerInput.WorkDir is required")
	}
	if err := containerCheckOutName(in.OutName); err != nil {
		return "", err
	}
	// Required is not enough: WorkDir becomes a mount source and is also
	// created below, and containerMountSource would only reject a relative
	// path several steps later — after os.MkdirAll had already made a
	// directory tree wherever the process happened to be running from.
	// Checking here means nothing is created before the path is known to be
	// usable. (Same guard, same reasoning, as Resolve's.)
	if err := containerCheckAbs(in.WorkDir, "BaseContainerInput.WorkDir"); err != nil {
		return "", err
	}

	opts := ContainerOptions{
		Runtime:  in.Runtime,
		Image:    in.Image,
		Platform: in.Platform,
		SelfPath: in.SelfPath,
		Events:   in.Events,
	}

	runtimeName, runtimePath, err := containerDetectRuntime(opts)
	if err != nil {
		return "", err
	}
	_, image, imageDigest, imageWarn, err := containerResolveImage(opts, in.Target.DistroID, in.Target.VersionID, in.Target.Codename)
	if err != nil {
		return "", err
	}
	platform, err := containerPlatformFor(opts, in.Target.Arch)
	if err != nil {
		return "", err
	}

	selfPath, err := containerSelfPath(opts, in.Target.Arch)
	if err != nil {
		return "", err
	}
	if err := containerCheckMountSourceFile(selfPath, "SelfPath binary"); err != nil {
		return "", err
	}
	if err := containerValidateBinary(selfPath, in.Target.Arch); err != nil {
		return "", err
	}

	if err := os.MkdirAll(in.WorkDir, 0o755); err != nil {
		return "", dferr.Wrap(dferr.Environment, err, "container: create work dir")
	}

	binSrc, err := containerMountSource(selfPath)
	if err != nil {
		return "", err
	}
	workSrc, err := containerMountSource(in.WorkDir)
	if err != nil {
		return "", err
	}

	// Two mounts, and for a builtin base that is all there is. Resolve needs
	// five or six; this needs the binary to re-enter and one writable
	// directory for the finished archive to appear in. There is deliberately
	// no /archives (nothing is downloaded), no named store volume (same
	// reason, which also keeps this path clear of both of the known
	// archives-cache defects recorded against Resolve's), and no
	// /snapshot (there is no input snapshot — producing one is the job).
	mounts := []containerMount{
		{Source: binSrc, Target: "/debark", RO: true},
		{Source: workSrc, Target: "/work", RO: false},
	}

	// innerBaseRef is what the INNER binary is told, which for a mounted
	// definition file is the mount target and never the host path. See
	// containerFromBaseArgs.BaseRef.
	innerBaseRef := in.BaseRef
	if in.BaseIsFile {
		if err := containerCheckMountSourceFile(in.BaseRef, "BaseContainerInput.BaseRef base definition"); err != nil {
			return "", err
		}
		defSrc, derr := containerMountSource(in.BaseRef)
		if derr != nil {
			return "", derr
		}
		// Appended last, the way Resolve appends its optional /external
		// mount, so the mandatory mounts keep a fixed position whether or
		// not this one is present.
		mounts = append(mounts, containerMount{Source: defSrc, Target: containerBaseDefinitionTarget, RO: true})
		innerBaseRef = containerBaseDefinitionTarget
	}

	// The same forwarding Resolve does, for the same reason: `snapshot
	// from-base` in a container runs a full apt closure resolution inside,
	// and until now the host had nothing to say about it either. Measured
	// before this change, a 19 s from-base run emitted two events, both
	// host-side. See containerevents.go.
	onEvent := containerForwardEventTo(in.Events)

	inner := containerBuildFromBaseArgv(containerFromBaseArgs{
		BaseRef:    innerBaseRef,
		Arch:       in.Target.Arch,
		OutName:    in.OutName,
		JSONEvents: onEvent != nil,
	})
	runArgv := containerBuildRunArgv(platform, mounts, nil, image, inner)

	attrs := map[string]any{
		"backend": "container", "runtime": runtimeName, "image": image,
		"image_digest": imageDigest, "platform": platform,
	}
	evidence.Emit(in.Events, evidence.TypeBackendSelected, "base snapshot synthesized in a container", attrs)
	if imageWarn != "" {
		// The unpinned-image warning has nowhere structured to go on this
		// path, and that is worth stating rather than dropping. Resolve
		// attaches it to the plan it returns, which carries it into the
		// lock; this function returns a path, and the snapshot inside was
		// written by a process that does not know which image it is running
		// in. The evidence stream is therefore the only record, and it is a
		// real one — it reaches evidence.json inside the bundle — but a
		// caller that wants this as a lock.Warning would need this function
		// to return warnings alongside the path. Recorded in this package's
		// report rather than decided here.
		evidence.Warn(in.Events, evidence.TypeWarning, imageWarn,
			map[string]any{"code": containerWarnUnpinnedImage})
	}

	outHost := filepath.Join(in.WorkDir, in.OutName)
	// The stale-output hole Resolve measured, in the one shape where the
	// check below cannot compensate for it. Resolve found that a previous
	// run's plan.json plus a container exiting 0 without writing one
	// returned the PREVIOUS run's selections as this run's result; here the
	// equivalent is returning a previous run's snapshot archive, which the
	// caller would then open successfully — snapshot.Open would validate it
	// perfectly, because it is a perfectly valid snapshot of the wrong base.
	// "Exists and is non-empty" is a deliberately weak acceptance test (see
	// this file's opening comment on why it is the right one), and it is
	// only safe BECAUSE of this removal: with it, "the file exists" can only
	// mean "this run wrote it".
	if rmErr := os.Remove(outHost); rmErr != nil && !os.IsNotExist(rmErr) {
		return "", dferr.Wrap(dferr.Environment, rmErr,
			"container: remove a stale base snapshot archive before running; a leftover %s would be returned as this run's result", outHost)
	}

	res, err := execContainerFn(ctx, runtimePath, runArgv, onEvent)
	if err != nil {
		return "", err
	}
	// Every non-zero exit is a failure here, with no equivalent of Resolve's
	// "3 means incomplete but a plan was still written". A snapshot is
	// all-or-nothing: there is no partial base whose package list is worth
	// reading back, and core/base's own synthesis refuses rather than
	// truncating. containerClassifyRunResult also catches the
	// docker/podman-level failures (missing qemu binfmt handler, unreachable
	// daemon, no image for --platform) that never reach the inner command.
	containerEmitForwardSummary(in.Events, res.Events)
	if res.ExitCode != 0 {
		return "", containerClassifyRunResult(res, platform)
	}

	fi, serr := os.Stat(outHost)
	if serr != nil {
		return "", dferr.Wrap(dferr.Verification, serr,
			"container: the inner `snapshot from-base` exited 0 but wrote no archive (%s)", containerLastLines(res.combined(), 6))
	}
	if fi.IsDir() {
		return "", dferr.New(dferr.Verification,
			"container: %s is a directory, want the snapshot archive the inner `snapshot from-base` was told to write", outHost)
	}
	if fi.Size() == 0 {
		// An empty file is what a truncated write, a full disk on the host
		// side of the /work mount, or a container killed mid-flush leaves
		// behind. snapshot.Open would report it as a malformed archive,
		// which is true but names the wrong culprit: the archive is not
		// malformed, it was never finished.
		return "", dferr.New(dferr.Verification,
			"container: the inner `snapshot from-base` exited 0 but %s is empty (%s)", outHost, containerLastLines(res.combined(), 6))
	}
	return outHost, nil
}

// containerCheckOutName refuses anything that is not a plain base name.
//
// OutName is joined onto /work on the inner argv and onto WorkDir on the
// host, so it is the one caller-supplied string in this operation that
// becomes a path on both sides of the boundary. The failure it prevents is
// the one containerCheckEnvelopeFilename already prevents for the resolve
// envelope's filenames, arriving from the other direction: a name carrying a
// separator or a parent reference writes, and then hands the caller, a path
// outside the directory that was meant to contain it. "../../.ssh/config" is
// not a hypothetical shape — it is the shape that was demonstrated end to
// end against the real core/store before that other guard was written.
//
// Both separators are rejected regardless of which OS this is running on,
// deliberately, and this is the reason a stdlib call is not enough:
// filepath.Base answers for the HOST's separator only, so on Linux it calls
// `a\b` a perfectly good single element — and this string is also joined
// onto a path inside a Linux container, where a host that let a backslash
// through has produced something the inner process reads as one filename and
// the host reads as two path elements. One definition, applied to both
// spellings, the same rule containerMountSource follows for the same reason.
//
// repository.ValidFilename, the project's existing answer to "is this string
// safe as a base name", is deliberately NOT reused here: it also insists on a
// .deb suffix, which this is not.
func containerCheckOutName(name string) error {
	bad := func(why string) error {
		return dferr.New(dferr.Usage,
			"container: BaseContainerInput.OutName %q is not a plain file name: %s; "+
				"it is joined onto the work directory on the host and onto /work inside the container, "+
				"so a name that can escape either is refused rather than resolved", name, why)
	}
	switch {
	case name == "":
		return dferr.Usagef("container: BaseContainerInput.OutName is required")
	case strings.ContainsAny(name, `/\`):
		return bad("it contains a path separator")
	case name == "." || name == "..":
		return bad("it is a parent or current directory reference")
	case strings.ContainsRune(name, 0):
		// Not reachable through the CLI, but this value can come from a
		// programmatic caller, and a NUL terminates the string for the
		// syscall while leaving the Go string's own comparison intact --
		// which is how a check that looked at the whole string can be made
		// to guard a path the kernel truncates.
		return bad("it contains a NUL byte")
	case strings.HasPrefix(name, "-"):
		// Defence in depth rather than a live hole: containerBuildFromBaseArgv
		// always joins this onto "/work/", so it can never reach the inner
		// argv as a bare token a flag parser would read as an option. The
		// refusal is here so that stays true if the argv is ever built a
		// different way -- the same reasoning validatePackageOperand gives
		// for refusing a leading '-' outright rather than relying on
		// position.
		return bad("it begins with '-', which a flag parser would read as an option")
	}
	return nil
}
