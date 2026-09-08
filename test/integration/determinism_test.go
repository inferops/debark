package integration

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/engine"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/store"
)

// fixedSourceDateEpoch is an arbitrary, valid Unix timestamp
// (2023-11-14T22:13:20Z) used to fix every build's clock in this file. Its
// exact value does not matter — only that it is the same for both builds and
// that it is a real instant a human reading lock.json's created_at later
// would recognise, unlike the Unix epoch itself.
const fixedSourceDateEpoch = "1700000000"

// vendorDeb is one operator-supplied .deb, byte-identical between the two
// builds but living at a DIFFERENT absolute path in each of them. It is what
// gives this test teeth over "the same request, from a CI checkout and from a
// laptop": --local-dir is the headline vendor-.deb path, and the directory it
// names is a host location, not part of what was asked for. Its bytes are
// produced by fetch.BuildFixtureDeb, which is deterministic (fixed mtimes,
// sorted control fields), so the only difference between build A's copy and
// build B's copy is where it sits on disk.
var vendorDeb = fetch.FixtureDeb{
	Package:      "acme-agent",
	Version:      "2.1.0",
	Architecture: "amd64",
	Description:  "debark integration test vendor package",
}

// writeVendorDeb writes vendorDeb into dir and returns dir, ready to be
// passed as an Inputs.LocalDirs entry.
func writeVendorDeb(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("%s_%s_%s.deb", vendorDeb.Package, vendorDeb.Version, vendorDeb.Architecture))
	if err := fetch.WriteFixtureDeb(path, vendorDeb); err != nil {
		t.Fatalf("fetch.WriteFixtureDeb: %v", err)
	}
	return dir
}

// ---- a backend that actually exercises the two host-path seams -------------

// determinismBackend is fakeAptBackend with the two seams that genuinely
// carry a host path into the finished bundle wired up for real, because a
// fake that quietly leaves them empty is exactly how a determinism test
// passes against provably non-identical builds:
//
//   - Resolve fills plan.Resolver.APTOptions — a recorded lock.json field —
//     from a REAL private apt root materialised under the engine's own
//     mkdirTemp("", "debark-build-*") work root, and fills it RAW, with no
//     masking of its own. A backend is an interface with more than one
//     implementation (local, container, and whatever comes next); making the
//     fake mask its own paths would only prove the fake can mask, so this one
//     deliberately hands the engine the argv apt was really invoked with and
//     leaves it to the engine — the sole writer of lock.json — to refuse to
//     record a value that can never be the same twice.
//   - ClosedWorld is not faked at all: it is the REAL local backend
//     (apt.NewLocalBackend) driven by a fake apt-get Runner, so
//     BuildPrivateRoot, the argv assembly and the CommandDigest/OutputDigest
//     computation in core/apt/closedworld.go all run for real, against the
//     real per-run work directory and the real bundle repo path. Only the
//     process that would have been exec'd is fake — the same seam
//     core/apt's own unit tests use (LocalOptions.Runner), which is what
//     lets the closed-world check be exercised honestly on a machine with no
//     apt-get, including Windows.
//
// The old shape of this test could not see either defect: its backend never
// set Resolver.APTOptions at all, and every build set
// Options.ClosedWorldCheck=false so the closed-world digests were never
// computed.
type determinismBackend struct {
	*fakeAptBackend
	closedWorld apt.Backend
}

func newDeterminismBackend() *determinismBackend {
	return &determinismBackend{
		fakeAptBackend: newFakeAptBackend(),
		closedWorld:    apt.NewLocalBackend(apt.LocalOptions{Runner: fakeAptGet{}}),
	}
}

func (b *determinismBackend) Resolve(ctx context.Context, in apt.ResolveInput) (*resolve.Plan, error) {
	plan, err := b.fakeAptBackend.Resolve(ctx, in)
	if err != nil {
		return nil, err
	}
	arch := "amd64"
	if in.Snapshot != nil && in.Snapshot.Target.Arch != "" {
		arch = in.Snapshot.Target.Arch
	}
	root, err := apt.BuildPrivateRoot(apt.RootSpec{
		Dir:          filepath.Join(in.WorkDir, "resolve-aptroot"),
		ArchivesDir:  in.ArchivesDir,
		Arch:         arch,
		ForeignArchs: in.Snapshot.Target.ForeignArchs,
		Recommends:   in.Recommends,
		PhasedPolicy: in.PhasedPolicy,
	})
	if err != nil {
		return nil, err
	}
	// Raw and unmasked, on purpose: see the type's doc comment.
	plan.Resolver.APTOptions = root.SortedOptions()
	return plan, nil
}

func (b *determinismBackend) ClosedWorld(ctx context.Context, in apt.ClosedWorldInput) (lock.ClosedWorld, error) {
	return b.closedWorld.ClosedWorld(ctx, in)
}

// fakeAptGet is an apt.Runner that never execs anything but answers in the
// exact output shapes core/apt's parsers expect, including the one detail
// that matters here: `apt-get update` echoes back the source URI it fetched
// from, which for the closed-world check is a file: URI naming the finished
// bundle's own repo directory — a host path derived from Output.Path. It
// reads that URI out of the private root's real sources.list.d rather than
// being told it, so it can only ever echo the path the production code
// actually pointed apt at.
type fakeAptGet struct{}

func (fakeAptGet) Run(_ context.Context, opts []string, args ...string) (apt.Output, error) {
	// Mirrors execRunner.Run's argv assembly exactly (core/apt/runner.go):
	// this is the argv that becomes ClosedWorld.CommandDigest.
	full := make([]string, 0, len(opts)*2+len(args))
	for _, o := range opts {
		full = append(full, "-o", o)
	}
	full = append(full, args...)
	out := apt.Output{Argv: append([]string{"apt-get"}, full...)}

	switch {
	case len(args) > 0 && args[0] == "update":
		out.Stdout = []byte(fakeAptUpdateOutput(opts))
	case len(args) > 1 && args[0] == "-s" && args[1] == "install":
		out.Stdout = []byte(fakeAptSimulateOutput(args[2:]))
	}
	return out, nil
}

// fakeAptUpdateOutput renders what apt-get update prints for a file: source:
// one Get: line per index file, naming the URI. The URI is read back out of
// the sources.list.d directory the -o Dir::Etc::sourceparts option points at,
// which is the private root BuildPrivateRoot just materialised.
func fakeAptUpdateOutput(opts []string) string {
	var uris []string
	for _, o := range opts {
		dir, ok := strings.CutPrefix(o, "Dir::Etc::sourceparts=")
		if !ok {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			for _, field := range strings.Fields(string(data)) {
				if strings.HasPrefix(field, "file:") {
					uris = append(uris, field)
				}
			}
		}
	}
	sort.Strings(uris)

	var b strings.Builder
	for i, uri := range uris {
		fmt.Fprintf(&b, "Get:%d %s ./ InRelease [1234 B]\n", 2*i+1, uri)
		fmt.Fprintf(&b, "Get:%d %s ./ Packages [456 B]\n", 2*i+2, uri)
	}
	b.WriteString("Reading package lists...\n")
	return b.String()
}

// fakeAptSimulateOutput renders `apt-get -s install name:arch=version ...`
// output in which every requested entry installs at exactly the version it
// was pinned to — the closed-world success case.
func fakeAptSimulateOutput(entries []string) string {
	var b strings.Builder
	b.WriteString("Reading package lists...\nBuilding dependency tree...\n")
	for _, e := range entries {
		nameArch, version, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		name, arch, ok := strings.Cut(nameArch, ":")
		if !ok {
			arch = "amd64"
		}
		fmt.Fprintf(&b, "Inst %s (%s debark:bundle [%s])\n", name, version, arch)
		fmt.Fprintf(&b, "Conf %s (%s debark:bundle [%s])\n", name, version, arch)
	}
	fmt.Fprintf(&b, "0 upgraded, %d newly installed, 0 to remove and 0 not upgraded.\n", len(entries))
	return b.String()
}

// ---- the two builds ---------------------------------------------------------

// buildDeterministic runs one full build of the SAME snapshot archive and the
// SAME request into <outRoot>/out.
//
// st and signer are supplied by the caller and SHARED by both builds, which
// is deliberate and is what two of this test's four assertions rest on:
//
//   - one store, so the second build finds every file already held. The store
//     is a persistent, machine-level content cache; whether it happens to
//     already hold a byte is a property of the machine, never of the request,
//     so nothing in the bundle may vary with it. Giving each build its own
//     private store — what this test used to do — hides every such
//     dependency, because then both builds always download everything.
//   - one signing key, so debark.manifest.sig can be compared like any
//     other file. An auditor rebuilding a bundle to check it byte for byte
//     rebuilds it with the same key; two throwaway keys would make the
//     signature (and evidence.json, which records the key id) differ for a
//     reason that has nothing to do with reproducibility, and the old version
//     of this test paid for that by excluding both from comparison.
//
// What DOES differ between the two calls: the output directory, and the
// directory the identical vendor .deb is read from. Both are host locations,
// neither is part of what was asked for.
func buildDeterministic(t *testing.T, archivePath, outRoot string, st store.Store, signer sign.Signer) {
	t.Helper()
	ctx := context.Background()

	deps := engine.Deps{
		Backend: newDeterminismBackend(),
		Store:   st,
		Repo:    repository.NewWriter(),
		Signer:  signer,
		Events:  evidence.NewCollector(nil),
	}
	eng, err := engine.New(deps)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	req := buildjob.BuildRequest{
		SchemaVersion: buildjob.SchemaVersion,
		SnapshotRef:   archivePath,
		Inputs: buildjob.Inputs{
			Packages:  []string{"aaa-base"},
			LocalDirs: []string{writeVendorDeb(t, filepath.Join(outRoot, "vendor"))},
		},
		Options: buildjob.Options{
			UpdateMode: buildjob.UpdateAdditive,
			// Left at its default (nil = on). The closed-world check runs on
			// every real build and its result is recorded in lock.json, so a
			// determinism test that disables it is not testing the shape of
			// build an operator actually gets.
		},
		Output: buildjob.Output{
			Path:   filepath.Join(outRoot, "out"),
			Format: buildjob.FormatDir,
			Sign:   buildjob.SignOptions{Required: true},
		},
	}

	if _, err := eng.Build(ctx, req); err != nil {
		t.Fatalf("engine.Build: %v", err)
	}
}

// bundleFiles reads every regular file under <outRoot>/out, keyed by its
// slash-separated path relative to the bundle root.
func bundleFiles(t *testing.T, outRoot string) map[string][]byte {
	t.Helper()
	root := filepath.Join(outRoot, "out")
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("no files found under %s", root)
	}
	return out
}

func loadLockOrFatal(t *testing.T, outRoot string) *lock.Lock {
	t.Helper()
	l, err := lock.Load(filepath.Join(outRoot, "out"))
	if err != nil {
		t.Fatalf("lock.Load(%s): %v", outRoot, err)
	}
	return l
}

func loadManifestOrFatal(t *testing.T, outRoot string) *manifest.Manifest {
	t.Helper()
	m, _, err := manifest.Load(filepath.Join(outRoot, "out"))
	if err != nil {
		t.Fatalf("manifest.Load(%s): %v", outRoot, err)
	}
	return m
}

// assertEqualJSON compares a and b via canonical JSON (so field order never
// matters) and reports a readable diff on mismatch.
func assertEqualJSON(t *testing.T, label string, a, b any) {
	t.Helper()
	ab, err := canonical.Marshal(a)
	if err != nil {
		t.Fatalf("%s: marshal A: %v", label, err)
	}
	bb, err := canonical.Marshal(b)
	if err != nil {
		t.Fatalf("%s: marshal B: %v", label, err)
	}
	if string(ab) != string(bb) {
		t.Errorf("%s differs between the two builds:\n  A=%s\n  B=%s", label, ab, bb)
	}
}

// TestDeterminism builds the same request twice and requires the two finished
// bundles to be byte-identical, file for file, with NO exclusions —
// debark.manifest.sig included. Design section 3.13's claim ("build the
// same request twice -> byte-identical manifest and repository metadata") is
// the product's whole provenance story: an auditor who cannot rebuild a
// bundle and get the same bytes cannot check anything debark says about it.
//
// The two builds differ in every way a rebuild legitimately can, and in no
// way it may not:
//
//	SAME: the request's content (packages, the vendor .deb's bytes, options),
//	      the snapshot archive, the clock (SOURCE_DATE_EPOCH), the signing
//	      key, the content store.
//	DIFFERENT: the output directory, the directory the vendor .deb is read
//	      from, the engine's per-run temporary work root (mkdirTemp, a fresh
//	      random path every run, not controllable by the caller at all), and
//	      what the shared store already holds when each build starts.
//
// Every one of those "DIFFERENT" values is a host location or a machine cache
// state. None of them is part of what was asked for, so none of them may
// change a single byte of the result.
func TestDeterminism(t *testing.T) {
	// The one seam this whole test relies on: fixed once, structurally
	// impossible for either build below to observe a different value (see
	// core/engine/clock.go's effectiveCreatedAt — every artefact timestamp in
	// the bundle, every evidence event's TS, and the manifest signature's own
	// created_at derive from this single value). t.Setenv restores the
	// previous value on cleanup.
	t.Setenv("SOURCE_DATE_EPOCH", fixedSourceDateEpoch)

	root := t.TempDir()
	archivePath, _ := buildFixtureSnapshotArchive(t, filepath.Join(root, "snap"))

	// One store and one key for both builds — see buildDeterministic's doc
	// comment for why each of those two is load-bearing.
	st, err := store.Open(filepath.Join(root, "store"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	privKey := filepath.Join(root, "keys", "operator.key")
	if _, err := sign.GenerateKey(privKey, "debark integration test key"); err != nil {
		t.Fatalf("sign.GenerateKey: %v", err)
	}
	signer, err := sign.SignerFor(context.Background(), privKey)
	if err != nil {
		t.Fatalf("sign.SignerFor: %v", err)
	}
	t.Cleanup(func() { _ = signer.Close() })

	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	buildDeterministic(t, archivePath, dirA, st, signer)
	buildDeterministic(t, archivePath, dirB, st, signer)

	lA, lB := loadLockOrFatal(t, dirA), loadLockOrFatal(t, dirB)
	mA, mB := loadManifestOrFatal(t, dirA), loadManifestOrFatal(t, dirB)

	// ---- the closed-world check must actually have RUN ---------------------
	// Everything this test asserts about ClosedWorld's two digests is
	// vacuous if the check was skipped (both digests are then empty, and
	// empty equals empty). Assert the outcome first, so the digest
	// comparison below can never silently pass for the wrong reason.
	if lA.ClosedWorld.Result != lock.ClosedWorldOK {
		t.Fatalf("closed-world check did not run and pass: result=%q detail=%q", lA.ClosedWorld.Result, lA.ClosedWorld.Detail)
	}
	if lA.ClosedWorld.CommandDigest == "" || lA.ClosedWorld.OutputDigest == "" {
		t.Fatalf("closed-world check recorded no digests: %+v", lA.ClosedWorld)
	}
	// Same for the resolver's recorded apt options: a backend that records
	// none would make the APTOptions comparison meaningless.
	if len(lA.Resolver.APTOptions) == 0 {
		t.Fatal("lock.Resolver.APTOptions is empty; this test cannot see a path leaking through it")
	}

	// ---- the whole bundle, byte for byte, no exclusions ---------------------
	filesA, filesB := bundleFiles(t, dirA), bundleFiles(t, dirB)

	var paths []string
	seen := map[string]bool{}
	for p := range filesA {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for p := range filesB {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	for _, p := range paths {
		a, okA := filesA[p]
		b, okB := filesB[p]
		switch {
		case !okA || !okB:
			t.Errorf("%s exists in one bundle but not the other (A=%v B=%v)", p, okA, okB)
		case !bytes.Equal(a, b):
			t.Errorf("%s differs between two builds of the same request (%d vs %d bytes)", p, len(a), len(b))
		}
	}

	// The signature file specifically: it is the last thing written and the
	// only file no earlier version of this test ever looked at, so name it
	// explicitly rather than trusting it to have been in the walk.
	if _, ok := filesA[manifest.SigFileName]; !ok {
		t.Errorf("%s missing from build A; a signed bundle's signature is part of its bytes", manifest.SigFileName)
	}
	if !bytes.Equal(filesA[manifest.SigFileName], filesB[manifest.SigFileName]) {
		t.Errorf("%s differs between the two builds: the same manifest bytes, signed with the same key under a "+
			"fixed SOURCE_DATE_EPOCH, must produce the same signature file", manifest.SigFileName)
	}

	// ---- the signature's own clock is the build's clock ---------------------
	// Comparing A against B cannot see this defect on its own, and it is worth
	// being explicit about why, because it is exactly the shape of hole this
	// whole test exists to close. Every Signer stamps Signature.CreatedAt with
	// core/sign's nowStamp(), which is canonical.Time(time.Now()) — the real
	// wall clock, in all three implementations. canonical.Time truncates to
	// the second, and these two builds finish a few hundred milliseconds
	// apart, so both stamp the SAME wrong second and the byte comparison above
	// sails straight past a signature file that honours no SOURCE_DATE_EPOCH
	// at all. (Measured: with the engine's overwrite removed, the byte
	// comparison passed three runs out of three.) A test that only ever
	// compared two builds started in the same second would report a
	// reproducible bundle to an auditor who rebuilds it tomorrow and gets
	// different bytes.
	//
	// So assert the invariant directly instead of hoping to catch it by
	// racing: the timestamp inside the signature file must be the build's one
	// createdAt — the same value in manifest.created_at, lock.created_at and
	// every evidence event's ts (core/engine/clock.go's effectiveCreatedAt,
	// "the ONLY function that decides what now means"). Pinned against the
	// fixed epoch as well as against the manifest, so this fails loudly if
	// SOURCE_DATE_EPOCH ever stops being honoured anywhere in that chain.
	epochSec, err := strconv.ParseInt(fixedSourceDateEpoch, 10, 64)
	if err != nil {
		t.Fatalf("fixedSourceDateEpoch %q is not a Unix timestamp: %v", fixedSourceDateEpoch, err)
	}
	wantCreatedAt := canonical.Time(time.Unix(epochSec, 0))
	if mA.CreatedAt != wantCreatedAt {
		t.Errorf("manifest.CreatedAt = %q, want %q: SOURCE_DATE_EPOCH=%s was not honoured",
			mA.CreatedAt, wantCreatedAt, fixedSourceDateEpoch)
	}
	for _, dir := range []string{dirA, dirB} {
		sigFile, err := manifest.LoadSignature(filepath.Join(dir, "out"))
		if err != nil {
			t.Fatalf("manifest.LoadSignature(%s): %v", dir, err)
		}
		if len(sigFile.Signatures) == 0 {
			t.Fatalf("%s: signature file carries no signature block", dir)
		}
		for i, sig := range sigFile.Signatures {
			if sig.CreatedAt != wantCreatedAt {
				t.Errorf("%s: signatures[%d].created_at = %q, want %q — the signature file must record the "+
					"build's single timestamp, not the wall clock at the moment the signer happened to run",
					dir, i, sig.CreatedAt, wantCreatedAt)
			}
		}
	}

	// ---- field-level assertions, for a readable failure ---------------------
	// The byte comparison above is the real assertion; these say WHICH field
	// moved when it fails, which a 6 KB JSON diff would not.
	assertEqualJSON(t, "lock.Packages", lA.Packages, lB.Packages)
	assertEqualJSON(t, "lock.Install", lA.Install, lB.Install)
	assertEqualJSON(t, "lock.Target", lA.Target, lB.Target)
	assertEqualJSON(t, "lock.Resolver", lA.Resolver, lB.Resolver)
	assertEqualJSON(t, "lock.Resolver.APTOptions", lA.Resolver.APTOptions, lB.Resolver.APTOptions)
	assertEqualJSON(t, "lock.ClosedWorld", lA.ClosedWorld, lB.ClosedWorld)
	assertEqualJSON(t, "lock.Stats", lA.Stats, lB.Stats)
	assertEqualJSON(t, "lock.Warnings", lA.Warnings, lB.Warnings)
	assertEqualJSON(t, "lock.CreatedAt", lA.CreatedAt, lB.CreatedAt)
	assertEqualJSON(t, "lock.SnapshotDigest", lA.SnapshotDigest, lB.SnapshotDigest)
	assertEqualJSON(t, "lock.RequestDigest", lA.RequestDigest, lB.RequestDigest)

	assertEqualJSON(t, "manifest.BundleID", mA.BundleID, mB.BundleID)
	assertEqualJSON(t, "manifest.LockDigest", mA.LockDigest, mB.LockDigest)
	assertEqualJSON(t, "manifest.SnapshotDigest", mA.SnapshotDigest, mB.SnapshotDigest)
	assertEqualJSON(t, "manifest.Repository", mA.Repository, mB.Repository)
	assertEqualJSON(t, "manifest.Target", mA.Target, mB.Target)
	assertEqualJSON(t, "manifest.CreatedAt", mA.CreatedAt, mB.CreatedAt)
	assertEqualJSON(t, "manifest.Tool", mA.Tool, mB.Tool)
	assertEqualJSON(t, "manifest.Files", mA.Files, mB.Files)

	// ---- no host path may appear anywhere in the bundle ---------------------
	// The byte comparison catches a path that VARIES; this catches one that
	// is merely present. Both builds could in principle agree on a leaked
	// path if the leak came from something they share, and a recorded host
	// path is a defect in its own right (contract brief rule 2) even when it
	// happens not to break this particular pair of builds.
	for _, p := range paths {
		for label, host := range map[string]string{
			"the build's output directory": filepath.Join(dirA, "out"),
			"the vendor input directory":   filepath.Join(dirA, "vendor"),
		} {
			for _, form := range []string{host, filepath.ToSlash(host)} {
				if bytes.Contains(filesA[p], []byte(form)) {
					t.Errorf("%s records %s (%q); no host path may reach an artefact", p, label, form)
				}
			}
		}
	}
}
