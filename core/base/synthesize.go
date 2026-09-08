package base

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/version"
)

// Paths the synthesized snapshot records its files at. They are the paths
// those files would have on a real machine, because that is what a snapshot
// document means: `snapshot create` records where it read each file from, and
// core/apt's private-root builder reads them back expecting target-absolute
// paths (it rewrites each Signed-By to the in-root copy by matching them).
// A synthesized snapshot that used made-up paths would be a different format
// wearing the same schema version.
const (
	sourcesDir     = "/etc/apt/sources.list.d"
	dpkgStatusPath = "/var/lib/dpkg/status"
)

// Options is one synthesis.
type Options struct {
	// Base is the resolved definition, its source and its digest.
	Base Resolved
	// BaseRef is the operator's original --base argument, needed verbatim by
	// the container path so the inner run resolves the same thing this one
	// did. Empty means Base.Definition.ID.
	BaseRef string

	// Out is where the snapshot archive is written.
	Out string
	// WorkDir is a scratch directory. The caller creates and removes it.
	WorkDir string

	// Backend is auto, local or container, exactly as `build --backend`.
	Backend string
	// Image overrides the container image.
	Image string
	// SelfPath is this executable, which the container backend mounts and
	// re-enters.
	SelfPath string

	// Events receives the backend and closure events. Optional.
	Events evidence.Sink

	// CreatedAt overrides the document timestamp. Empty means now, truncated
	// to a second. A caller that wants a byte-identical archive across runs
	// (a test, or a fleet committing one base to git) sets it.
	CreatedAt string
}

// Result is a written, validated synthesized snapshot.
type Result struct {
	// Path is Options.Out.
	Path string
	// Snapshot is the document as read back from Path — not as built, so
	// what a caller inspects is what the file actually says.
	Snapshot *snapshot.Snapshot
	// Digest is the snapshot digest, the same value a lock will record.
	Digest string
	// Backend is which one resolved the seeds.
	Backend lock.Backend
	// Warnings are the resolution's, for the caller to show or fold into a
	// lock.
	Warnings []lock.Warning
}

// Synthesize turns a base definition into a snapshot archive.
//
// The result is an ordinary debark.snapshot/v1 archive: `build --snapshot`
// takes it without knowing where it came from, `snapshot inspect` reads it,
// and it can be committed to git so a fleet provably builds against one
// assumed baseline. The single thing that distinguishes it is origin.kind,
// and that field is required precisely so nothing downstream can fail to
// notice (ADR-014).
//
// Whichever backend runs, the archive is written and then re-opened with
// snapshot.Open before this function returns. That is not belt and braces.
// snapshot.Open is the full reader `build` itself uses — schema validation,
// per-file digest verification, and the keyring-fingerprint re-derivation
// that holds a document to the strictest rule in the format. Running it here
// means a synthesized snapshot has passed exactly the same gate a captured
// one does before anyone relies on it, and on the container path it is also
// the only thing standing between the host and a file a container wrote.
func Synthesize(ctx context.Context, opts Options) (*Result, error) {
	if opts.Out == "" {
		return nil, dferr.New(dferr.Usage, "base: Synthesize: Out is required")
	}
	if opts.WorkDir == "" {
		return nil, dferr.New(dferr.Usage, "base: Synthesize: WorkDir is required")
	}
	if err := Validate(opts.Base.Definition); err != nil {
		return nil, err
	}
	if opts.Base.Digest == "" {
		return nil, dferr.New(dferr.Usage, "base: Synthesize: the base definition has no digest")
	}

	kind, err := chooseBackend(ctx, opts)
	if err != nil {
		return nil, err
	}

	var warnings []lock.Warning
	switch kind {
	case lock.BackendContainer:
		if err := synthesizeInContainer(ctx, opts); err != nil {
			return nil, err
		}
	default:
		w, err := synthesizeLocally(ctx, opts)
		if err != nil {
			return nil, err
		}
		warnings = w
	}

	archive, err := snapshot.Open(ctx, opts.Out)
	if err != nil {
		return nil, err
	}
	defer func() { _ = archive.Close() }()

	if !archive.Snapshot.Synthesized() {
		// Only reachable if a future edit writes the wrong origin, or — on
		// the container path — if the archive that came back was produced by
		// something other than the from-base run that was asked for. Either
		// way the document would be claiming to be a measurement of a real
		// machine, which is the one claim this whole feature exists to stop
		// anything making by accident.
		return nil, dferr.New(dferr.Verification,
			"base: the snapshot written at %s does not record origin.kind = %q", opts.Out, snapshot.OriginSynthesized)
	}

	return &Result{
		Path:     opts.Out,
		Snapshot: archive.Snapshot,
		Digest:   archive.Digest,
		Backend:  kind,
		Warnings: warnings,
	}, nil
}

// chooseBackend applies the rule to the base's target, so from-base and
// build agree about where apt runs. It goes through apt.SelectBackend rather
// than re-deriving the rule, which is what keeps the two from drifting: the
// solver-divergence gate E2 established, the container-runtime hint, and the
// refusal to silently degrade to a second-class resolver all come along.
func chooseBackend(ctx context.Context, opts Options) (lock.Backend, error) {
	requested := lock.Backend(strings.TrimSpace(opts.Backend))
	if requested == "auto" {
		requested = ""
	}
	backend, _, err := apt.SelectBackend(ctx, apt.Selection{
		Target:    gateTarget(ctx, opts.Base.Definition),
		Requested: requested,
		Image:     opts.Image,
		Events:    opts.Events,
		SelfPath:  opts.SelfPath,
	})
	if err != nil {
		return "", err
	}
	return backend.Kind(), nil
}

// gateTarget is targetFor, plus the apt version — but only when this host is
// demonstrably the release the base describes.
//
// the backend gate compares the host's *measured* effective solver
// against the target's *expected* one, and refuses the local backend when it
// cannot confirm they match (experiment E2: divergence tracks the solver, not
// the apt version). It infers the target's expected solver from the target's
// recorded apt version — and a base has none, because nobody has run apt on
// a machine that does not exist. So the gate refuses local for every base,
// always, and falls back to the container.
//
// That refusal is correct in general and must stay. It is wrong in exactly
// one case: when the host's own distro id AND version id equal the base's,
// the host's apt is not a proxy for the release's apt, it IS the release's
// apt — the same package, from the same archive. Probing it and recording
// what it answered is a measurement, not the guess the gate exists to refuse.
// Anything else (an Ubuntu 24.04 host asked for an ubuntu:22.04 base) leaves
// the version empty and correctly goes to the container, which runs the
// release's own apt for real.
//
// Found by running it: `snapshot from-base ubuntu:24.04/minimal --backend
// local` on an Ubuntu 24.04 host was refused with "target solver known=false"
// until this existed.
func gateTarget(ctx context.Context, d Definition) snapshot.Target {
	target := targetFor(d)
	probe, err := apt.NewLocalBackend(apt.LocalOptions{}).Probe(ctx, target)
	if err != nil || !probe.Available {
		return target
	}
	if !strings.EqualFold(probe.DistroID, d.DistroID) || probe.VersionID != d.VersionID {
		return target
	}
	target.APTVersion = probe.APTVersion
	target.DpkgVersion = probe.DpkgVersion
	return target
}

// targetFor is the snapshot.Target a base describes. APTVersion and
// DpkgVersion are deliberately absent here: nobody has run apt yet, and a
// guess at them would be a fact about a machine invented by this function.
// synthesizeLocally fills them in from the resolver that actually answered.
func targetFor(d Definition) snapshot.Target {
	return snapshot.Target{
		DistroID:  d.DistroID,
		VersionID: d.VersionID,
		Codename:  d.Codename,
		Arch:      d.Arch,
	}
}

// synthesizeInContainer runs the whole from-base command inside a pinned
// image of the target release and moves the archive it wrote to Out.
//
// The container produces a finished snapshot rather than a package list
// because a host that needs the container backend at all — Windows, macOS, a
// mismatched Linux — has no copy of the target distribution's archive keyring
// and so can neither build the bootstrap root the seeds resolve in nor supply
// the key material the finished snapshot has to carry. Inside the image both
// are simply present.
func synthesizeInContainer(ctx context.Context, opts Options) error {
	baseRef := opts.BaseRef
	if baseRef == "" {
		baseRef = opts.Base.Definition.ID
	}
	// Resolved.Builtin, not a comparison against Source: see its doc comment
	// for why a file called "builtin" made the string form wrong.
	isFile := !opts.Base.Builtin

	produced, err := apt.BaseSnapshotInContainer(ctx, apt.BaseContainerInput{
		BaseRef:    baseRef,
		BaseIsFile: isFile,
		Target:     targetFor(opts.Base.Definition),
		WorkDir:    filepath.Join(opts.WorkDir, "container"),
		OutName:    "base.snapshot.tar.zst",
		Image:      opts.Image,
		SelfPath:   opts.SelfPath,
		Events:     opts.Events,
	})
	if err != nil {
		return err
	}
	return moveFile(produced, opts.Out)
}

// synthesizeLocally builds the bootstrap root's inputs here, asks this
// machine's apt what the seeds resolve to, and writes the archive.
func synthesizeLocally(ctx context.Context, opts Options) ([]lock.Warning, error) {
	d := opts.Base.Definition

	bootstrap, files, err := bootstrapSnapshot(d)
	if err != nil {
		return nil, err
	}
	filesDir := filepath.Join(opts.WorkDir, "bootstrap", snapshot.FilesDir)
	if err := writeFileSet(filesDir, files); err != nil {
		return nil, err
	}

	closure, err := apt.LocalClosure(ctx, apt.LocalOptions{Events: opts.Events}, apt.ClosureInput{
		Snapshot:         bootstrap,
		SnapshotFilesDir: filesDir,
		Seeds:            d.Seeds,
		Recommends:       d.Recommends,
		WorkDir:          filepath.Join(opts.WorkDir, "closure"),
	})
	if err != nil {
		return nil, err
	}
	// Applied here, once, before anything reads the closure — so the
	// synthesized dpkg status and origin.assumed_installed cannot disagree
	// about what the base claims. They are the same set written twice, and
	// a filter applied to only one of them would be worse than no filter.
	closure.Packages = applyExcludes(closure.Packages, d.Excludes)
	if len(closure.Packages) == 0 {
		return nil, dferr.New(dferr.Resolution,
			"base %s: every package in the seed closure was excluded", d.ID)
	}

	snap, err := finalSnapshot(opts, closure, files)
	if err != nil {
		return nil, err
	}
	if err := snapshot.WriteArchive(opts.Out, snap, files); err != nil {
		return nil, err
	}
	return closure.Warnings, nil
}

// bootstrapSnapshot is the input to the seed resolution: identity, sources,
// keyrings — and no dpkg status at all, which is what makes apt answer "what
// does a stock install contain" rather than "what is missing here".
func bootstrapSnapshot(d Definition) (*snapshot.Snapshot, *snapshot.FileSet, error) {
	files := &snapshot.FileSet{Bytes: map[string][]byte{}}

	sourcesFile := record(sourcesDir+"/"+d.DistroID+".sources", []byte(d.Sources), files)

	keyrings := make([]snapshot.File, 0, len(d.Keyrings))
	for _, kp := range d.Keyrings {
		data, err := os.ReadFile(kp)
		if err != nil {
			return nil, nil, dferr.Wrap(dferr.Environment, err,
				"base %s: reading archive keyring %s", d.ID, kp).(*dferr.Error).
				WithHint("a base is resolved either on a host of the same distribution, which has this file, or inside the pinned image of the target release, which also has it — try --backend container")
		}
		keyrings = append(keyrings, record(kp, data, files))
	}

	fingerprints, err := snapshot.KeyMaterial(keyrings, files)
	if err != nil {
		return nil, nil, err
	}

	return &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		CreatedAt:     canonical.Time(time.Now()),
		Tool:          toolInfo(),
		Target:        targetFor(d),
		APT: snapshot.APT{
			Sources:  []snapshot.File{sourcesFile},
			Keyrings: keyrings,
		},
		KeyringFingerprints: fingerprints,
		Origin:              originFor(Resolved{Definition: d}),
		// DpkgStatus stays zero. core/apt's validateClosureInput refuses a
		// bootstrap that carries one, so this is checked, not merely
		// intended.
	}, files, nil
}

// finalSnapshot is the document that gets written: the bootstrap's identity
// and configuration, plus the dpkg status the closure produced.
func finalSnapshot(opts Options, closure *apt.Closure, files *snapshot.FileSet) (*snapshot.Snapshot, error) {
	d := opts.Base.Definition

	status := apt.DpkgStatusFor(closure.Packages)
	statusFile := record(dpkgStatusPath, status, files)

	keyrings := make([]snapshot.File, 0, len(d.Keyrings))
	for _, kp := range d.Keyrings {
		ap := archivePath(kp)
		data, ok := files.Bytes[ap]
		if !ok {
			return nil, dferr.New(dferr.Environment, "base %s: keyring %s was not staged", d.ID, kp)
		}
		keyrings = append(keyrings, fileRecord(kp, data))
	}
	fingerprints, err := snapshot.KeyMaterial(keyrings, files)
	if err != nil {
		return nil, err
	}

	target := targetFor(d)
	// The resolver that actually answered is the honest source for these:
	// they are what a later build's solver-divergence check compares against,
	// and on the local path this is the same apt the target release ships.
	target.APTVersion = closure.Resolver.APTVersion
	target.DpkgVersion = closure.Resolver.DpkgVersion

	createdAt := opts.CreatedAt
	if createdAt == "" {
		// SOURCE_DATE_EPOCH is honoured here for the same reason the engine
		// honours it: this document's digest becomes lock.SnapshotDigest and
		// then part of what the manifest signature covers, so a wall clock
		// here would make two otherwise identical `build --base` runs produce
		// different bundles. canonical.EffectiveTime is the one implementation
		// of that rule.
		t, err := canonical.EffectiveTime(time.Now)
		if err != nil {
			return nil, err
		}
		createdAt = t
	}

	sourcesFile := fileRecord(sourcesDir+"/"+d.DistroID+".sources", []byte(d.Sources))

	return &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		CreatedAt:     createdAt,
		Tool:          toolInfo(),
		Target:        target,
		APT: snapshot.APT{
			Sources:  []snapshot.File{sourcesFile},
			Keyrings: keyrings,
		},
		DpkgStatus:          statusFile,
		Origin:              originWithClosure(opts.Base, closure.Packages),
		InstalledCount:      len(closure.Packages),
		KeyringFingerprints: fingerprints,
		Warnings:            synthesisWarnings(closure),
		// No MachineID, and therefore PhasedNeverInclude at resolve time
		// (ADR-006). A base is a claim about a release; a machine id is the
		// most machine-specific fact there is, and inventing one would make
		// every build from this base reproduce a phasing decision belonging
		// to no machine at all.
		//
		// No Labels either, and no OSRelease: a captured snapshot's
		// os-release is a file that was read off a real disk, and a
		// plausible-looking synthesized one would be the same kind of
		// invention.
	}, nil
}

// synthesisWarnings is what an operator holding this snapshot should be told,
// carried in the document itself rather than only printed once at synthesis
// time — because the snapshot is the thing that gets committed to git, copied
// between machines and read months later by someone who did not run the
// command.
func synthesisWarnings(closure *apt.Closure) []string {
	out := []string{
		"this snapshot was synthesized from a base definition, not captured from a real machine: " +
			"the bundle built from it is complete only if the target really is a stock install of this release",
	}
	for _, w := range closure.Warnings {
		out = append(out, w.Code+": "+w.Message)
	}
	return out
}

func originFor(r Resolved) snapshot.Origin {
	return snapshot.Origin{
		Kind:         snapshot.OriginSynthesized,
		BaseID:       r.Definition.ID,
		Source:       r.Source,
		SourceDigest: r.Digest,
	}
}

// originWithClosure is originFor plus the assumed installed set, which only
// the finished document carries: the bootstrap has no closure yet.
//
// The list is name:arch, matching how dpkg identifies a package and how
// install queries for one, so the comparison on the target is a set
// membership test and not a parse. Versions are deliberately left out. They
// would roughly double the field for no gain: a target running a different
// version of an assumed package still HAS it, so the bundle is not short, and
// reporting that as divergence would bury the one direction that matters
// under noise from ordinary patching.
func originWithClosure(r Resolved, packages []apt.ClosurePackage) snapshot.Origin {
	o := originFor(r)
	o.AssumedInstalled = make([]string, 0, len(packages))
	for _, p := range packages {
		o.AssumedInstalled = append(o.AssumedInstalled, p.Qualified())
	}
	sort.Strings(o.AssumedInstalled)
	return o
}

// applyExcludes removes the definition's excluded packages from the closure,
// across every architecture. See Definition.Excludes for why removing is
// always the safe direction, and why an exclusion is a bare name.
func applyExcludes(packages []apt.ClosurePackage, excludes []string) []apt.ClosurePackage {
	if len(excludes) == 0 {
		return packages
	}
	drop := make(map[string]bool, len(excludes))
	for _, e := range excludes {
		drop[e] = true
	}
	out := packages[:0:0]
	for _, p := range packages {
		if drop[p.Name] {
			continue
		}
		out = append(out, p)
	}
	return out
}

func toolInfo() snapshot.Tool {
	return snapshot.Tool{Name: version.Name, Version: version.Version, Edition: version.Edition}
}

// archivePath is core/snapshot's own target-path-to-member-name rule, applied
// here because this package builds File records without going through
// Capture. It must agree exactly with snapshot's toArchivePath, and
// synthesize_test.go pins that by round-tripping through snapshot.Validate,
// which enforces the same safeArchivePath rule from the other side.
func archivePath(targetPath string) string {
	return strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(targetPath, "\\", "/")), "/")
}

func fileRecord(targetPath string, data []byte) snapshot.File {
	return snapshot.File{
		Path:        targetPath,
		ArchivePath: archivePath(targetPath),
		Size:        int64(len(data)),
		SHA256:      digest.Bytes(data),
		Mode:        "0644",
	}
}

func record(targetPath string, data []byte, files *snapshot.FileSet) snapshot.File {
	f := fileRecord(targetPath, data)
	files.Bytes[f.ArchivePath] = data
	return f
}

// writeFileSet materialises a FileSet as the files/ tree core/apt's private
// root reads from. Each member name has already been through archivePath, and
// is re-checked here against escaping the directory rather than trusted —
// the same rule core/snapshot's extractors apply, for the same reason.
func writeFileSet(dir string, files *snapshot.FileSet) error {
	for name, data := range files.Bytes {
		rel := filepath.FromSlash(name)
		if !filepath.IsLocal(rel) {
			return dferr.New(dferr.Verification, "base: refusing to stage %q, which is not a path inside the files directory", name)
		}
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return dferr.Wrap(dferr.Environment, err, "base: staging %s", name)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return dferr.Wrap(dferr.Environment, err, "base: staging %s", name)
		}
	}
	return nil
}

// moveFile moves src to dst, falling back to copy-then-remove across
// filesystems (the container path's work directory and the operator's --out
// are routinely on different ones).
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return dferr.Wrap(dferr.Environment, err, "base: creating output directory")
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Clean(src))
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "base: reading the synthesized snapshot back")
	}
	// dst is the operator's own --out, so there is no boundary here to
	// escape from -- writing where they asked is the point of the flag, and
	// unlike a snapshot's archive_path (which arrives from the far side of an
	// air gap and IS confined) nothing untrusted contributes to it. Cleaned
	// anyway: it came off a command line, and a normalised path is what the
	// error message below should name.
	//nolint:gosec // G703: dst is the operator's own --out. gosec's taint
	// analysis follows it back to a command-line string and cannot see that
	// there is no boundary here to escape from -- writing where the operator
	// asked is the flag's entire purpose. The genuinely confined path in this
	// feature is a snapshot's archive_path, which arrives from the far side
	// of an air gap and IS checked, by snapshot.safeArchivePath on the way in
	// and by writeFileSet's filepath.IsLocal on the way out.
	if err := os.WriteFile(filepath.Clean(dst), data, 0o644); err != nil {
		return dferr.Wrap(dferr.Environment, err, "base: writing %s", dst)
	}
	_ = os.Remove(src)
	return nil
}
