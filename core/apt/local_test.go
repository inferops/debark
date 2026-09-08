package apt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
)

// This file tests local.go's Resolve, none of which had a unit test before:
//
//  1. held.go's dpkg-status hold extraction, and Resolve's substitution of a
//     held package's own current version for whatever apt proposed, fetched
//     via apt-get download rather than install --download-only.
//  2. the external-.deb "resolve together, fall back to one at a time"
//     isolation, including that a surviving selection's
//     Unresolved siblings carry apt's own missing-dependency names via
//     parse.go's existing parser.
//  3. that an external/user-supplied selection is never recorded as
//     apt-signed, since it was never actually verified by anything.
//  4. that lock.Resolver.APTOptions never carries the run's own randomly
//     named temp directory, which would otherwise break byte-identical
//     lock.json across two builds of the same request.
//  5. that fetchAndBuildSelections finds the real fetched file even when an
//     archive's own "Filename:" index field does not predict apt's actual
//     on-disk cache name.
//
// Most of it runs through a fake Runner (routedRunner, below), so none of it
// needs a real apt-get anywhere — including on Windows, the same technique
// select_test.go's fakeVersionRunner and closedworld_test.go's fakeCWRunner
// already use.

// --- fake Runner --------------------------------------------------------

// routedRunner answers each apt-get invocation Resolve makes by matching its
// args against a list of registered predicates, in registration order. An
// unrouted call fails the test immediately (via Fatalf) instead of silently
// returning a zero Output, so a test that forgets to route a call fails
// loudly at the call site rather than via a confusing downstream assertion.
type routedRunner struct {
	t      *testing.T
	calls  [][]string
	routes []routedResponse
}

type routedResponse struct {
	match func(args []string) bool
	out   Output
	// fn, when set, is called instead of returning out directly — for a
	// route whose response needs a side effect first (downloadHeldPackages
	// expects "apt-get download" to have actually left a file behind by the
	// time it returns).
	fn func(args []string) Output
}

func (r *routedRunner) on(match func(args []string) bool, out Output) {
	r.routes = append(r.routes, routedResponse{match: match, out: out})
}

func (r *routedRunner) onFunc(match func(args []string) bool, fn func(args []string) Output) {
	r.routes = append(r.routes, routedResponse{match: match, fn: fn})
}

func (r *routedRunner) Run(_ context.Context, _ []string, args ...string) (Output, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	for _, rt := range r.routes {
		if rt.match(args) {
			if rt.fn != nil {
				return rt.fn(args), nil
			}
			return rt.out, nil
		}
	}
	r.t.Fatalf("routedRunner: unrouted apt-get call: %v", args)
	return Output{}, nil
}

// argsEqual matches an apt-get call whose args are exactly want, in order —
// for calls whose argument order this package guarantees (a sorted pinned
// list, or a single-name call).
func argsEqual(want ...string) func([]string) bool {
	return func(args []string) bool { return reflect.DeepEqual(args, want) }
}

// argsAreSet matches a call of exactly prefix followed by set, in any
// order — for "-s install <names...>" calls whose trailing name order
// mirrors whatever order the caller (e.g. in.Packages) supplied, which a
// test should not have to predict.
func argsAreSet(prefix []string, set ...string) func([]string) bool {
	want := map[string]int{}
	for _, s := range set {
		want[s]++
	}
	return func(args []string) bool {
		if len(args) != len(prefix)+len(set) {
			return false
		}
		for i, p := range prefix {
			if args[i] != p {
				return false
			}
		}
		got := map[string]int{}
		for _, a := range args[len(prefix):] {
			got[a]++
		}
		return reflect.DeepEqual(got, want)
	}
}

// --- fixture builders ----------------------------------------------------

// minimalSnapshotWithHolds returns a bare-bones *snapshot.Snapshot (just
// enough Target identity for Resolve to run) plus a files directory. When
// held is non-empty, it also captures a dpkg status listing each name as
// "hold ok installed" at version 1.0, so heldPackageVersions has a real
// captured file to read exactly the way it would from a real snapshot.
func minimalSnapshotWithHolds(t *testing.T, held ...string) (*snapshot.Snapshot, string) {
	t.Helper()
	filesDir := t.TempDir()
	snap := &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Target: snapshot.Target{
			DistroID: "debian", VersionID: "12", Codename: "bookworm",
			Arch: "amd64", APTVersion: "2.6.1", DpkgVersion: "1.21.23",
		},
	}
	if len(held) == 0 {
		return snap, filesDir
	}
	var sb strings.Builder
	for _, n := range held {
		fmt.Fprintf(&sb, "Package: %s\nStatus: hold ok installed\nVersion: 1.0\nArchitecture: amd64\n\n", n)
	}
	const archivePath = "var/lib/dpkg/status"
	dst := filepath.Join(filesDir, filepath.FromSlash(archivePath))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(sb.String())
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	snap.DpkgStatus = snapshot.File{Path: "/var/lib/dpkg/status", ArchivePath: archivePath, Size: int64(len(data))}
	return snap, filesDir
}

// fakeArchivePkg is one stanza writeFakePackagesIndex writes.
type fakeArchivePkg struct {
	Name, Version, Arch string
	Size                int64
	// Depends, when set, is written as the stanza's Depends: field. It is
	// what buildDependencyGraph reads to explain one selection by another,
	// so a test that asserts on a "dependency-of:<pkg>" Reason has to supply
	// it — debark never invents an edge the packages' own control data did
	// not declare (principle 1), so without this every selection falls back
	// to the representative root instead.
	Depends string
}

// writeFakePackagesIndex pre-places a real deb822 Packages stanza directly
// where ReadPackagesIndex (parse.go) will look for it once Resolve runs:
// <WorkDir>/aptroot/var/lib/apt/lists/*_Packages. A fake Runner never
// actually execs "apt-get update", so nothing else would ever put a file
// there; this stands in for what a real archive fetch would have left
// behind, in the exact format control.ParseBinaryIndex expects (the same
// shape parse_test.go's golden fixtures use).
func writeFakePackagesIndex(t *testing.T, listsDir string, pkgs ...fakeArchivePkg) {
	t.Helper()
	if err := os.MkdirAll(listsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, p := range pkgs {
		fmt.Fprintf(&sb, "Package: %s\nVersion: %s\nArchitecture: %s\nFilename: pool/x/%s/%s_%s_%s.deb\nSize: %d\nSHA256: %s\nSection: misc\n",
			p.Name, p.Version, p.Arch, p.Name, p.Name, p.Version, p.Arch, p.Size, strings.Repeat("a", 64))
		if p.Depends != "" {
			fmt.Fprintf(&sb, "Depends: %s\n", p.Depends)
		}
		sb.WriteString("\n")
	}
	path := filepath.Join(listsDir, "debark-test_Packages")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeCwdFile writes name into the test process's current directory —
// standing in, inside a fake "apt-get download" route, for the file a real
// apt-get download would have left there (see downloadHeldPackages's doc).
// Callers must have isolated the process cwd first (t.Chdir(t.TempDir())),
// never call this against the real source tree.
func writeCwdFile(t *testing.T, name string, content []byte) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, name), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- held.go: pure parsing -------------------------------------------------

func TestSplitDpkgStatusStanzas(t *testing.T) {
	data := []byte("Package: foo\nStatus: install ok installed\nVersion: 1.0\nDescription: multi\n line\n .\n more\n\n" +
		"Package: bar\nStatus: hold ok installed\nVersion: 2.0\n\n")
	stanzas := splitDpkgStatusStanzas(data)
	if len(stanzas) != 2 {
		t.Fatalf("got %d stanzas, want 2: %+v", len(stanzas), stanzas)
	}
	if stanzas[0]["Package"] != "foo" || stanzas[0]["Status"] != "install ok installed" {
		t.Errorf("stanza 0 = %+v", stanzas[0])
	}
	if stanzas[1]["Package"] != "bar" || stanzas[1]["Status"] != "hold ok installed" {
		t.Errorf("stanza 1 = %+v", stanzas[1])
	}
}

func TestHeldPackageVersions_Basic(t *testing.T) {
	data := []byte(
		"Package: held-pkg\nStatus: hold ok installed\nVersion: 1.0\n\n" +
			"Package: normal-pkg\nStatus: install ok installed\nVersion: 1.0\n\n" +
			"Package: hold-not-installed\nStatus: hold ok not-installed\nVersion: 1.0\n\n" +
			"Package: deinstalled-pkg\nStatus: deinstall ok config-files\nVersion: 1.0\n\n" +
			"Package: mentions-hold-elsewhere\nStatus: install ok installed\nMaintainer: Hold Ok Installed <x@example.com>\nVersion: 1.0\n\n",
	)
	filesDir := t.TempDir()
	dst := filepath.Join(filesDir, "var", "lib", "dpkg", "status")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	snap := &snapshot.Snapshot{DpkgStatus: snapshot.File{ArchivePath: "var/lib/dpkg/status"}}

	held, err := heldPackageVersions(snap, filesDir)
	if err != nil {
		t.Fatalf("heldPackageVersions: %v", err)
	}
	if len(held) != 1 || held["held-pkg"] != "1.0" {
		t.Fatalf("held = %v, want exactly {held-pkg: 1.0}", held)
	}
	for _, notHeld := range []string{"normal-pkg", "hold-not-installed", "deinstalled-pkg", "mentions-hold-elsewhere"} {
		if _, ok := held[notHeld]; ok {
			t.Errorf("%q must not be reported as held: %v", notHeld, held)
		}
	}
}

func TestHeldPackageVersions_NoDpkgStatus(t *testing.T) {
	held, err := heldPackageVersions(&snapshot.Snapshot{}, t.TempDir())
	if err != nil || len(held) != 0 {
		t.Errorf("empty snapshot: held=%v err=%v, want empty set and no error", held, err)
	}
	held, err = heldPackageVersions(nil, t.TempDir())
	if err != nil || len(held) != 0 {
		t.Errorf("nil snapshot: held=%v err=%v, want empty set and no error", held, err)
	}
}

// TestHeldPackageVersions_RealFixture reuses root_test.go's debian12Fixture
// technique (the real, checked-in 88-package dpkg status captured from a
// live debian:bookworm-slim container) rather than a hand-built one, per the
// the contract brief instruction to build fixtures from it the same way
// root_test.go does.
func TestHeldPackageVersions_RealFixture(t *testing.T) {
	snap, filesDir := debian12Fixture(t)

	// Baseline: the real, pristine fixture has nothing on hold.
	held, err := heldPackageVersions(snap, filesDir)
	if err != nil {
		t.Fatalf("heldPackageVersions on real fixture: %v", err)
	}
	if len(held) != 0 {
		t.Errorf("real fixture should have no held packages, got %v", held)
	}

	// Inject a hold into the same real, messy status file (not a hand-built
	// one) and confirm extraction finds exactly that package at its recorded
	// version, and does not false-positive on a real installed package that
	// is not held (adduser, which root_test.go already asserts is present
	// in this fixture).
	statusPath := filepath.Join(filesDir, filepath.FromSlash(snap.DpkgStatus.ArchivePath))
	orig, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	augmented := append(append([]byte{}, orig...),
		[]byte("\nPackage: acme-injected-hold\nStatus: hold ok installed\nVersion: 9.9\nArchitecture: amd64\n\n")...)
	if err := os.WriteFile(statusPath, augmented, 0o644); err != nil {
		t.Fatal(err)
	}

	held, err = heldPackageVersions(snap, filesDir)
	if err != nil {
		t.Fatalf("heldPackageVersions after injecting a hold: %v", err)
	}
	if len(held) != 1 || held["acme-injected-hold"] != "9.9" {
		t.Fatalf("held = %v, want exactly {acme-injected-hold: 9.9}", held)
	}
	if _, ok := held["adduser"]; ok {
		t.Error("adduser (a real, non-held, installed package in this fixture) must not be reported as held")
	}
}

// --- defect 1: Resolve pins held packages at their current version --------

// TestResolve_HeldPackage_PinnedAtCurrentVersion mirrors the held-package
// e2e fixture's own shape: two packages start at 1.0, both are named
// explicitly in the request, --upgrades is on, and only one of the two is
// held. acme-other-demo must move to 2.0; acme-held-demo must stay at 1.0 —
// but it must still end up IN the bundle at 1.0 (a fresh install-time
// target has never had it installed at all, unlike the machine the
// snapshot came from — dropping a held package from the bundle entirely
// would leave the fresh target without it, not "unchanged"). A stable-coded
// warning must record that a hold shaped the plan.
//
// The full-upgrade response deliberately includes an Inst line for the held
// package too (real apt would not propose one there — a plain full-upgrade
// already skips held packages on its own), so this also exercises record's
// defensive path, not only the named-request path the real bug was in.
func TestResolve_HeldPackage_PinnedAtCurrentVersion(t *testing.T) {
	// downloadHeldPackages uses the process's real working directory (apt-get
	// download's own limitation — see its doc); isolate it to a scratch dir
	// for this test so nothing is ever written into the shared source tree,
	// and so the fake "download" route below has somewhere safe to drop the
	// file it simulates apt producing. t.Chdir restores it automatically.
	t.Chdir(t.TempDir())

	snap, filesDir := minimalSnapshotWithHolds(t, "acme-held-demo")
	workDir := t.TempDir()
	listsDir := filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists")
	writeFakePackagesIndex(t, listsDir,
		fakeArchivePkg{Name: "acme-held-demo", Version: "1.0", Arch: "amd64", Size: 660},
		fakeArchivePkg{Name: "acme-other-demo", Version: "2.0", Arch: "amd64", Size: 1234},
	)

	bothInst := "Inst acme-held-demo [1.0] (2.0 acme-versions:12 [amd64])\n" +
		"Inst acme-other-demo [1.0] (2.0 acme-versions:12 [amd64])\n"

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	runner.on(argsEqual("-s", "full-upgrade"), Output{Stdout: []byte(bothInst)})
	// The actual reported bug: apt-get -s install <held-pkg> <other-pkg>
	// (naming the held package explicitly) proposes changing it anyway —
	// the -y-gated hold-changed refusal only fires without -y, in the real
	// fetch pass below, which is exactly where the fixture crashed.
	runner.on(argsAreSet([]string{"-s", "install", "--"}, "acme-held-demo", "acme-other-demo"), Output{Stdout: []byte(bothInst)})
	// Only acme-other-demo goes through the normal install/--download-only
	// pass: record kept acme-held-demo out of versions entirely (see
	// downloadHeldPackages's doc for why "install", with or without
	// --reinstall, can never be the mechanism for a held package).
	runner.on(argsEqual("install", "--print-uris", "-y", "--", "acme-other-demo:amd64=2.0"), Output{Stdout: []byte(
		"'file:///nowhere/acme-other-demo_2.0_amd64.deb' acme-other-demo_2.0_amd64.deb 1234 MD5Sum:00000000000000000000000000000000\n",
	)})
	runner.on(argsEqual("--download-only", "-y", "install", "--", "acme-other-demo:amd64=2.0"), Output{})
	// acme-held-demo is fetched separately via "apt-get download", which
	// (like the real apt-get download subcommand) writes into the process's
	// current directory rather than anywhere this fake controls directly —
	// simulated here by actually writing the file there when this route
	// fires, exactly as apt would have.
	runner.onFunc(argsEqual("download", "--", "acme-held-demo:amd64=1.0"), func(args []string) Output {
		writeCwdFile(t, "acme-held-demo_1.0_amd64.deb", []byte("held package bytes"))
		return Output{Stdout: []byte("Get:1 http://example.invalid acme-held-demo 1.0 [660 B]\nFetched 660 B in 0s\n")}
	})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Packages:         []string{"acme-held-demo", "acme-other-demo"},
		Upgrades:         true,
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(plan.Selections) != 2 {
		t.Fatalf("Selections = %+v, want exactly 2", plan.Selections)
	}
	byName := map[string]string{}
	for _, s := range plan.Selections {
		byName[s.Name] = s.Version
	}
	if byName["acme-held-demo"] != "1.0" {
		t.Errorf("acme-held-demo version = %q, want 1.0 (unchanged, still included)", byName["acme-held-demo"])
	}
	if byName["acme-other-demo"] != "2.0" {
		t.Errorf("acme-other-demo version = %q, want 2.0 (the upgrade proceeded)", byName["acme-other-demo"])
	}
	if want := []string{"acme-held-demo:amd64=1.0", "acme-other-demo:amd64=2.0"}; !reflect.DeepEqual(plan.Install, want) {
		t.Errorf("Install = %v, want %v", plan.Install, want)
	}
	// The held selection's StagedPath must point at a real file — proof
	// downloadHeldPackages actually moved what "apt-get download" produced
	// into the archives directory, not merely that a Selection was
	// fabricated from index metadata with nothing behind it (which is
	// exactly the class of bug that made engine's later store step fail
	// with "no such file or directory" for a different reason earlier).
	for _, s := range plan.Selections {
		if s.Name != "acme-held-demo" {
			continue
		}
		if s.StagedPath == "" {
			t.Fatal("acme-held-demo StagedPath is empty")
		}
		if _, statErr := os.Stat(s.StagedPath); statErr != nil {
			t.Errorf("acme-held-demo StagedPath %q does not exist: %v", s.StagedPath, statErr)
		}
		archivesDir := filepath.Join(workDir, "archives")
		if filepath.Dir(s.StagedPath) != archivesDir {
			t.Errorf("acme-held-demo StagedPath = %q, want it inside %q (the archives dir), not left in the process's own cwd", s.StagedPath, archivesDir)
		}
	}

	var held *lock.Warning
	for i := range plan.Warnings {
		switch plan.Warnings[i].Code {
		case "held-package.change-skipped":
			held = &plan.Warnings[i]
		case "resolver.already-satisfied":
			t.Errorf("resolver.already-satisfied must not also fire for a held package that still got its own selection: %+v", plan.Warnings[i])
		}
	}
	if held == nil {
		t.Fatalf("expected a held-package.change-skipped warning, got %+v", plan.Warnings)
	}
	if want := []string{"acme-held-demo"}; !reflect.DeepEqual(held.Packages, want) {
		t.Errorf("held-package.change-skipped Packages = %v, want %v", held.Packages, want)
	}
}

// TestResolve_HeldPackage_NotRequested proves the same current-version
// pinning applies to a held package apt proposes on its own (full-upgrade),
// when the operator never named it at all — the plain "holds are respected"
// half of the hold rule, as opposed to the explicit-request decision the test
// above covers.
func TestResolve_HeldPackage_NotRequested(t *testing.T) {
	t.Chdir(t.TempDir()) // see downloadHeldPackages's doc; isolate its cwd use

	snap, filesDir := minimalSnapshotWithHolds(t, "acme-held-demo")
	workDir := t.TempDir()
	listsDir := filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists")
	writeFakePackagesIndex(t, listsDir, fakeArchivePkg{Name: "acme-held-demo", Version: "1.0", Arch: "amd64", Size: 660})

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	runner.on(argsEqual("-s", "full-upgrade"), Output{Stdout: []byte(
		"Inst acme-held-demo [1.0] (2.0 acme-versions:12 [amd64])\n",
	)})
	runner.onFunc(argsEqual("download", "--", "acme-held-demo:amd64=1.0"), func(args []string) Output {
		writeCwdFile(t, "acme-held-demo_1.0_amd64.deb", []byte("held package bytes"))
		return Output{Stdout: []byte("Get:1 http://example.invalid acme-held-demo 1.0 [660 B]\nFetched 660 B in 0s\n")}
	})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Upgrades:         true,
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Selections) != 1 || plan.Selections[0].Name != "acme-held-demo" || plan.Selections[0].Version != "1.0" {
		t.Fatalf("Selections = %+v, want exactly acme-held-demo=1.0", plan.Selections)
	}
	found := false
	for _, w := range plan.Warnings {
		if w.Code == "held-package.change-skipped" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected held-package.change-skipped, got %+v", plan.Warnings)
	}
}

// --- defect 2 + 3: external .deb isolation and provenance ------------------

const unmetDepsOutput = "The following packages have unmet dependencies:\n" +
	" acme-broken-tool : Depends: nonexistent-package-xyz-does-not-exist-9999 but it is not installable\n" +
	"E: Unable to correct problems, you have held broken packages.\n"

// TestResolve_ExternalOneAtATime_SingleFailure mirrors the
// external-deb-missing-deps e2e fixture exactly: one external input whose
// dependency cannot exist anywhere. Resolve must return a Plan (not an
// error) with the failure recorded in Unresolved, including the missing
// dependency name parse.go's ParseSimulate already extracts — this is what
// lets core/engine report exit 3 ("bundle built but incomplete") instead of
// exit 5 ("resolution failed"): see buildResult in core/engine/result.go,
// which sets ExitIncomplete whenever len(Unresolved) > 0.
func TestResolve_ExternalOneAtATime_SingleFailure(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	workDir := t.TempDir()

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	// Only one external name: the "resolve together" attempt and the
	// one-at-a-time fallback both issue the identical "-s install
	// acme-broken-tool" call, so one route legitimately serves both.
	runner.on(argsEqual("-s", "install", "--", "acme-broken-tool"), Output{Stdout: []byte(unmetDepsOutput)})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		ExternalRepoDir:  filepath.Join(workDir, "external"),
		ExternalNames:    []string{"acme-broken-tool"},
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve returned an error instead of an incomplete Plan (this is exactly the exit-5-instead-of-3 bug): %v", err)
	}
	if len(plan.Selections) != 0 {
		t.Errorf("Selections = %+v, want none", plan.Selections)
	}
	if len(plan.Install) != 0 {
		t.Errorf("Install = %v, want none", plan.Install)
	}
	if len(plan.Unresolved) != 1 {
		t.Fatalf("Unresolved = %+v, want exactly 1 entry", plan.Unresolved)
	}
	u := plan.Unresolved[0]
	if u.Input != "acme-broken-tool" {
		t.Errorf("Unresolved[0].Input = %q, want acme-broken-tool", u.Input)
	}
	if u.Kind != "package" {
		t.Errorf("Unresolved[0].Kind = %q, want package", u.Kind)
	}
	if want := []string{"nonexistent-package-xyz-does-not-exist-9999"}; !reflect.DeepEqual(u.Missing, want) {
		t.Errorf("Unresolved[0].Missing = %v, want %v", u.Missing, want)
	}
	if !strings.Contains(u.Detail, "Depends") {
		t.Errorf("Unresolved[0].Detail = %q, want apt's own explanation", u.Detail)
	}
}

// TestResolve_ExternalOneAtATime_KeepsTheGoodOne is the isolation claim
// the external-input rule actually makes: two external inputs, one bad, one good. A
// collective failure must not lose the good one — it must still resolve,
// still get fetched, and be recorded as an external selection (never
// apt-signed: this also covers defect 3 for the "fallback succeeded"
// branch, as opposed to the "combined succeeded outright" branch the next
// test covers).
func TestResolve_ExternalOneAtATime_KeepsTheGoodOne(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	workDir := t.TempDir()
	listsDir := filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists")
	writeFakePackagesIndex(t, listsDir, fakeArchivePkg{Name: "acme-good-tool", Version: "1.0", Arch: "amd64", Size: 555})

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	// dedupSorted alphabetises the combined attempt's argv.
	runner.on(argsEqual("-s", "install", "--", "acme-broken-tool", "acme-good-tool"), Output{Stdout: []byte(unmetDepsOutput)})
	runner.on(argsEqual("-s", "install", "--", "acme-broken-tool"), Output{Stdout: []byte(unmetDepsOutput)})
	runner.on(argsEqual("-s", "install", "--", "acme-good-tool"), Output{Stdout: []byte(
		"Inst acme-good-tool (1.0 debark-external [amd64])\n",
	)})
	runner.on(argsEqual("install", "--print-uris", "-y", "--", "acme-good-tool:amd64=1.0"), Output{Stdout: []byte(
		"'file:///external/acme-good-tool_1.0_amd64.deb' acme-good-tool_1.0_amd64.deb 555 MD5Sum:11111111111111111111111111111111\n",
	)})
	runner.on(argsEqual("--download-only", "-y", "install", "--", "acme-good-tool:amd64=1.0"), Output{})

	externalRepoDir := filepath.Join(workDir, "external")
	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		ExternalRepoDir:  externalRepoDir,
		ExternalNames:    []string{"acme-broken-tool", "acme-good-tool"},
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(plan.Unresolved) != 1 || plan.Unresolved[0].Input != "acme-broken-tool" {
		t.Fatalf("Unresolved = %+v, want exactly acme-broken-tool", plan.Unresolved)
	}
	if len(plan.Selections) != 1 {
		t.Fatalf("Selections = %+v, want exactly one (acme-good-tool)", plan.Selections)
	}
	sel := plan.Selections[0]
	if sel.Name != "acme-good-tool" || sel.Version != "1.0" {
		t.Fatalf("selection = %+v, want acme-good-tool=1.0", sel)
	}
	if sel.Reason != lock.ReasonExternal {
		t.Errorf("Reason = %q, want %q", sel.Reason, lock.ReasonExternal)
	}
	if !sel.UserSupplied {
		t.Error("UserSupplied = false, want true for an external selection")
	}
	if sel.PublisherVerification == lock.VerifiedAPTSigned {
		t.Errorf("PublisherVerification = %q: an external/vendor .deb staged into a [trusted=yes] repo was never actually verified, so it must never claim apt-signed provenance", sel.PublisherVerification)
	}
	if sel.PublisherVerification != lock.VerifiedURLUnverified {
		t.Errorf("PublisherVerification = %q, want %q (matching core/fetch's FromLocalFile precedent)", sel.PublisherVerification, lock.VerifiedURLUnverified)
	}
}

// TestResolve_ExternalCombinedSuccess_NeverAPTSigned covers the other
// branch: the combined "resolve together" attempt succeeds outright (no
// one-at-a-time fallback triggered at all), which must still record the
// weaker provenance, not apt-signed — proving the defect 3 fix is not an
// artefact of only the fallback code path.
func TestResolve_ExternalCombinedSuccess_NeverAPTSigned(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	workDir := t.TempDir()
	listsDir := filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists")
	writeFakePackagesIndex(t, listsDir,
		fakeArchivePkg{Name: "acme-good-tool", Version: "1.0", Arch: "amd64", Size: 555},
		fakeArchivePkg{Name: "acme-great-tool", Version: "3.0", Arch: "amd64", Size: 777},
	)

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	runner.on(argsEqual("-s", "install", "--", "acme-good-tool", "acme-great-tool"), Output{Stdout: []byte(
		"Inst acme-good-tool (1.0 debark-external [amd64])\n" +
			"Inst acme-great-tool (3.0 debark-external [amd64])\n",
	)})
	runner.on(argsEqual("install", "--print-uris", "-y", "--", "acme-good-tool:amd64=1.0", "acme-great-tool:amd64=3.0"), Output{Stdout: []byte(
		"'file:///external/acme-good-tool_1.0_amd64.deb' acme-good-tool_1.0_amd64.deb 555 MD5Sum:11111111111111111111111111111111\n" +
			"'file:///external/acme-great-tool_3.0_amd64.deb' acme-great-tool_3.0_amd64.deb 777 MD5Sum:22222222222222222222222222222222\n",
	)})
	runner.on(argsEqual("--download-only", "-y", "install", "--", "acme-good-tool:amd64=1.0", "acme-great-tool:amd64=3.0"), Output{})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		ExternalRepoDir:  filepath.Join(workDir, "external"),
		ExternalNames:    []string{"acme-good-tool", "acme-great-tool"},
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Unresolved) != 0 {
		t.Errorf("Unresolved = %+v, want none", plan.Unresolved)
	}
	if len(plan.Selections) != 2 {
		t.Fatalf("Selections = %+v, want 2", plan.Selections)
	}
	for _, sel := range plan.Selections {
		if sel.PublisherVerification != lock.VerifiedURLUnverified {
			t.Errorf("%s: PublisherVerification = %q, want %q", sel.Name, sel.PublisherVerification, lock.VerifiedURLUnverified)
		}
		if sel.Reason != lock.ReasonExternal {
			t.Errorf("%s: Reason = %q, want %q", sel.Name, sel.Reason, lock.ReasonExternal)
		}
	}
}

// --- defect 4: APTOptions must not leak the run's own temp root -----------

// TestResolve_APTOptions_NoTempRootLeak is deliberately driven through the
// real localBackend.Resolve (not a fake apt.Backend, the way
// test/integration's TestDeterminism is), because that is exactly what let
// this bug go unnoticed: a fake backend never populates APTOptions at all,
// so nothing exercised root.SortedOptions()'s actual content. WorkDir and
// ArchivesDir here stand in for core/engine's own mkdirTemp("",
// "debark-build-*")-per-run directories, and are unique to this test run
// (t.TempDir()) the same way a real build's are unique to that build — so an
// APTOptions entry that still contained either would prove the exact defect
// reported: two builds of one identical request producing different
// lock.json bytes purely because of where their private roots happened to
// land on disk.
func TestResolve_APTOptions_NoTempRootLeak(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	workDir := t.TempDir()
	archivesDir := filepath.Join(t.TempDir(), "archives")

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		ArchivesDir:      archivesDir,
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Resolver.APTOptions) == 0 {
		t.Fatal("APTOptions is empty; this test cannot prove anything")
	}
	for _, o := range plan.Resolver.APTOptions {
		if strings.Contains(o, workDir) {
			t.Errorf("APTOptions entry leaks this run's own temp WorkDir (randomly named every real build, so this breaks byte-identical lock.json across two builds of the same request): %q", o)
		}
		if strings.Contains(o, archivesDir) {
			t.Errorf("APTOptions entry leaks this run's own temp ArchivesDir: %q", o)
		}
	}
	// This project chose to normalise rather than drop: the Dir::* keys
	// below must still be present (with a masked value) so an auditor can
	// still see a private root was genuinely used and which paths within it
	// apt was pointed at. The options that actually explain the resolution
	// decision must survive completely unchanged.
	mustHavePrefix := []string{
		"Dir::Cache::archives=<PRIVATE-ROOT>",
		"Dir::State=<PRIVATE-ROOT>",
		"APT::Architecture=" + snap.Target.Arch,
		"APT::Install-Recommends=",
	}
	for _, want := range mustHavePrefix {
		found := false
		for _, o := range plan.Resolver.APTOptions {
			if strings.HasPrefix(o, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected an APTOptions entry with prefix %q, got %v", want, plan.Resolver.APTOptions)
		}
	}
}

// --- fetchAndBuildSelections: real on-disk filename can differ from the
// archive's own index metadata -----------------------------------------

// TestResolve_FetchedFilename_FallsBackToRealDirectoryListing reproduces,
// with a real file on disk, a mismatch a real e2e run of the held-package
// fixture hit: a repository's Packages index "Filename:" field
// ("acme-mismatch-demo_1-0_amd64.deb") does not match the name apt's own
// download cache actually used ("acme-mismatch-demo_1.0_amd64.deb", built
// from the control Version field directly, dot preserved) — exactly what
// the e2e harness's synthetic repo generator produces (its Filename field
// hyphenates the version; its control Version field does not). Before this
// fix, fetchAndBuildSelections trusted the index's prediction unconditionally
// and pointed StagedPath at a file that was never there; downstream, engine's
// store step failed with "no such file or directory" for acme-other-demo — a
// package with nothing to do with the hold logic itself, which is why this
// gets its own test independent of the held-package tests above.
func TestResolve_FetchedFilename_FallsBackToRealDirectoryListing(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	workDir := t.TempDir()
	listsDir := filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists")
	if err := os.MkdirAll(listsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stanza := "Package: acme-mismatch-demo\nVersion: 1.0\nArchitecture: amd64\n" +
		"Filename: pool/a/acme-mismatch-demo_1-0_amd64.deb\nSize: 42\nSHA256: " + strings.Repeat("b", 64) + "\nSection: misc\n\n"
	if err := os.WriteFile(filepath.Join(listsDir, "debark-test_Packages"), []byte(stanza), 0o644); err != nil {
		t.Fatal(err)
	}

	// Dir::Cache::archives: what apt actually fetched, named apt's own way
	// (dot preserved), not the index's (hyphenated).
	archivesDir := filepath.Join(workDir, "archives")
	if err := os.MkdirAll(archivesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realContent := []byte("not a real deb, just needs stable, known bytes to hash")
	if err := os.WriteFile(filepath.Join(archivesDir, "acme-mismatch-demo_1.0_amd64.deb"), realContent, 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	runner.on(argsEqual("-s", "install", "--", "acme-mismatch-demo"), Output{Stdout: []byte(
		"Inst acme-mismatch-demo (1.0 acme-versions:12 [amd64])\n",
	)})
	runner.on(argsEqual("install", "--print-uris", "-y", "--", "acme-mismatch-demo:amd64=1.0"), Output{Stdout: []byte(
		"'http://example.invalid/acme-mismatch-demo_1-0_amd64.deb' acme-mismatch-demo_1-0_amd64.deb 42 MD5Sum:00000000000000000000000000000000\n",
	)})
	runner.on(argsEqual("--download-only", "-y", "install", "--", "acme-mismatch-demo:amd64=1.0"), Output{})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Packages:         []string{"acme-mismatch-demo"},
		ArchivesDir:      archivesDir,
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Selections) != 1 {
		t.Fatalf("Selections = %+v, want exactly 1", plan.Selections)
	}
	sel := plan.Selections[0]
	if sel.Filename != "acme-mismatch-demo_1.0_amd64.deb" {
		t.Errorf("Filename = %q, want the real on-disk name acme-mismatch-demo_1.0_amd64.deb (the index's own acme-mismatch-demo_1-0_amd64.deb does not exist on disk)", sel.Filename)
	}
	wantStagedPath := filepath.Join(archivesDir, "acme-mismatch-demo_1.0_amd64.deb")
	if sel.StagedPath != wantStagedPath {
		t.Errorf("StagedPath = %q, want %q", sel.StagedPath, wantStagedPath)
	}
	if _, statErr := os.Stat(sel.StagedPath); statErr != nil {
		t.Errorf("StagedPath does not point at a real file: %v", statErr)
	}
	// SHA256 must be computed from the real file's actual bytes, not left at
	// the index's placeholder digest — proof the fallback, not just the
	// filename correction, is what ran.
	sum := sha256.Sum256(realContent)
	wantSHA := hex.EncodeToString(sum[:])
	if sel.SHA256 != wantSHA {
		t.Errorf("SHA256 = %q, want %q (hash of the real staged file's bytes)", sel.SHA256, wantSHA)
	}
}

func TestFindStagedFile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"acme-tool_1.0_amd64.deb",
		"acme-tool-extra_1.0_amd64.deb", // must not match "acme-tool" by a loose prefix
		"other-thing_2.0_arm64.deb",
		"epoch-pkg_1%3a2.0-1_amd64.deb", // apt percent-encodes an epoch's ':'
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := findStagedFile(entries, "acme-tool", "amd64", "1.0"); !ok || got != "acme-tool_1.0_amd64.deb" {
		t.Errorf("findStagedFile(acme-tool, amd64, 1.0) = %q, %v", got, ok)
	}
	if got, ok := findStagedFile(entries, "other-thing", "arm64", "2.0"); !ok || got != "other-thing_2.0_arm64.deb" {
		t.Errorf("findStagedFile(other-thing, arm64, 2.0) = %q, %v", got, ok)
	}
	if _, ok := findStagedFile(entries, "acme-tool", "arm64", "1.0"); ok {
		t.Error("findStagedFile(acme-tool, arm64) should not match (wrong arch)")
	}
	if _, ok := findStagedFile(entries, "nope", "amd64", "1.0"); ok {
		t.Error("findStagedFile(nope, amd64) should not match (no such package)")
	}
	// The version segment must match too. A repository controls both its
	// Package: and its Filename: fields, so matching on the outer segments
	// alone let it have the wrong file's bytes attributed to a selection —
	// and the SHA256 is then computed over those substituted bytes, so the
	// lock is self-consistent while naming a package it does not contain.
	if got, ok := findStagedFile(entries, "acme-tool", "amd64", "2.0"); ok {
		t.Errorf("findStagedFile(acme-tool, amd64, 2.0) = %q, want no match: 2.0 is not the version on disk (1.0)", got)
	}
	if got, ok := findStagedFile(entries, "epoch-pkg", "amd64", "1:2.0-1"); !ok || got != "epoch-pkg_1%3a2.0-1_amd64.deb" {
		t.Errorf("findStagedFile(epoch-pkg, amd64, 1:2.0-1) = %q, %v, want the percent-encoded file", got, ok)
	}
	if _, ok := findStagedFile(entries, "acme-tool", "amd64", ""); ok {
		t.Error("findStagedFile with an empty version should not match anything")
	}
}

// TestResolve_SameVersionInSignedAndTrustedSource_IsNotAPTSigned is the
// residual half of the trusted=yes provenance defect.
//
// verificationForIndex is fed one index file name — the one
// indexByNameVersion happened to keep. That map is last-write-wins over the
// name-sorted list of every fetched index (ReadPackagesIndex sorts its glob),
// so when the same (name, arch, version) is offered by BOTH a signed archive
// and a captured "trusted=yes" source, which of the two decides the recorded
// provenance is nothing but the alphabetical order of apt's own list-file
// names. Here the trusted source is attacker.example and the signed one is
// deb.debian.org, so the signed stanza sorts last, wins the map, and the
// selection was stamped apt-signed — even though the bytes could equally have
// come from the source where apt performed no signature check at all.
//
// Nothing in the fetched metadata distinguishes the two, so the honest answer
// is the weaker claim: this is the same "fail in the safe direction" rule
// aptURItoFileName's own doc already states — downgrade a provenance claim
// rather than inflate one.
func TestResolve_SameVersionInSignedAndTrustedSource_IsNotAPTSigned(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	writeSnapshotSource(t, snap, filesDir, "/etc/apt/sources.list",
		"deb http://deb.debian.org/debian bookworm main\n")
	writeSnapshotSource(t, snap, filesDir, "/etc/apt/sources.list.d/vendor.list",
		"deb [trusted=yes] http://attacker.example/repo stable main\n")

	workDir := t.TempDir()
	listsDir := filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists")
	pkg := fakeArchivePkg{Name: "acme-tool", Version: "1.0", Arch: "amd64", Size: 12}
	// Both archives offer the identical (name, arch, version). "a" sorts
	// before "d", so the signed index is the one that wins the map.
	writeNamedPackagesIndex(t, listsDir, "attacker.example_repo_dists_stable_main_binary-amd64_Packages", pkg)
	writeNamedPackagesIndex(t, listsDir, "deb.debian.org_debian_dists_bookworm_main_binary-amd64_Packages", pkg)

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	runner.on(argsEqual("-s", "install", "--", "acme-tool"), Output{Stdout: []byte(
		"Inst acme-tool (1.0 acme:12 [amd64])\n",
	)})
	runner.on(argsEqual("install", "--print-uris", "-y", "--", "acme-tool:amd64=1.0"), Output{})
	runner.on(argsEqual("--download-only", "-y", "install", "--", "acme-tool:amd64=1.0"), Output{})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Packages:         []string{"acme-tool"},
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Selections) != 1 {
		t.Fatalf("Selections = %+v, want exactly 1", plan.Selections)
	}
	if got := plan.Selections[0].PublisherVerification; got != lock.VerifiedURLUnverified {
		t.Errorf("PublisherVerification = %q, want %q: this (name, version) is also offered by a [trusted=yes] source, so nothing here proves the bytes are the signed archive's — recording apt-signed makes lock.json assert a check that may never have run",
			got, lock.VerifiedURLUnverified)
	}
}

// --- defect 6: an architecture-qualified request ("name:arch") ------------
//
// Debian identifies a package by (name, architecture), and a request may say
// so. Everything built on baseName compared a whole operand against a
// Selection's bare Name, so "zlib1g:i386" matched nothing it had itself just
// resolved. Both consequences reached real lock.json files in the 2026-09-05
// matrix run — in both multiarch bundles, both of which built and installed
// correctly, so this was only ever about what the lock RECORDS:
//
//  1. resolver.already-satisfied ("one or more requested packages were
//     already at the exact version on the target; nothing was added for
//     them") fired on every arch-qualified request, four lines above the
//     lock's own record of the package being fetched.
//  2. Reason was never "requested" for one. foreign-arch-i386, whose entire
//     request is "zlib1g:i386", recorded that package as
//     "dependency-of:zlib1g:i386" — the package as a dependency of itself,
//     naming a parent that is not even a package name — and its five real
//     dependencies got the same non-existent parent. multiarch-coexist
//     recorded the requested zlib1g:i386 as "dependency-of:jq".

func TestSplitPackageOperand_VersionBeforeArch(t *testing.T) {
	// The epoch is the whole reason this splits "=version" off first. A
	// Debian version carries a ':' of its own, so a parser that looks for the
	// architecture in the WHOLE operand reads the epoch as one: the last two
	// rows below are the ones an "obvious" first-colon split gets wrong, and
	// they are not exotic — every zlib1g version in the two multiarch bundles
	// of the 2026-09-05 matrix run is epoched.
	for _, tc := range []struct {
		operand                      string
		name, arch, release, version string
	}{
		{"jq", "jq", "", "", ""},
		{"jq=1.7.1-3", "jq", "", "", "1.7.1-3"},
		{"zlib1g:i386", "zlib1g", "i386", "", ""},
		{"jq/bookworm", "jq", "", "bookworm", ""},
		{"zlib1g:i386/bookworm", "zlib1g", "i386", "bookworm", ""},
		{"zlib1g=1:1.2.13.dfsg-1", "zlib1g", "", "", "1:1.2.13.dfsg-1"},
		{"zlib1g:i386=1:1.2.13.dfsg-1", "zlib1g", "i386", "", "1:1.2.13.dfsg-1"},
	} {
		got := splitPackageOperand(tc.operand)
		if got.Name != tc.name || got.Arch != tc.arch || got.Release != tc.release || got.Version != tc.version {
			t.Errorf("splitPackageOperand(%q) = %+v, want name=%q arch=%q release=%q version=%q",
				tc.operand, got, tc.name, tc.arch, tc.release, tc.version)
		}
	}
}

func TestOperandMatches_UnqualifiedMatchesAnyArch(t *testing.T) {
	for _, tc := range []struct {
		operand, name, arch string
		want                bool
	}{
		// The overwhelmingly common case, and the one that must never
		// regress: nearly every request ever typed is unqualified, and each
		// one has to keep matching the single native-arch selection it
		// produced.
		{"jq", "jq", "amd64", true},
		{"jq", "jq", "i386", true},
		{"jq", "jq", "all", true},
		{"jq=1.7.1-3", "jq", "amd64", true},
		{"zlib1g=1:1.2.13.dfsg-1", "zlib1g", "amd64", true},
		// Qualified: that architecture and no other.
		{"zlib1g:i386", "zlib1g", "i386", true},
		{"zlib1g:i386", "zlib1g", "amd64", false},
		{"zlib1g:amd64", "zlib1g", "i386", false},
		{"zlib1g:i386=1:1.2.13.dfsg-1", "zlib1g", "i386", true},
		{"zlib1g:i386=1:1.2.13.dfsg-1", "zlib1g", "amd64", false},
		// Never a name it did not name.
		{"zlib1g:i386", "zlib1g-dev", "i386", false},
		{"jq", "libjq1", "amd64", false},
	} {
		if got := operandMatches(tc.operand, tc.name, tc.arch); got != tc.want {
			t.Errorf("operandMatches(%q, %q, %q) = %v, want %v", tc.operand, tc.name, tc.arch, got, tc.want)
		}
	}
}

// TestResolve_MultiarchRequest_SiblingArchIsNotRequested is the
// multiarch-coexist fixture's exact shape (test/e2e/fixtures/
// multiarch-coexist.json, ubuntu 24.04): one unqualified amd64 request and
// one i386-qualified request in a single build, resolving to a bundle that
// carries zlib1g at BOTH architectures — one because it was asked for, one
// because jq's own closure needs it. The versions are the real ones that run
// recorded, epoch included.
//
// The two architectures must be told apart in both directions. zlib1g:i386 is
// what was requested; zlib1g:amd64 was not, and must keep the honest
// dependency edge apt's own control data explains it by, rather than being
// promoted to "requested" (claiming the operator asked for a package they did
// not) or to "upgrade" (claiming a full-upgrade pass this build never ran).
func TestResolve_MultiarchRequest_SiblingArchIsNotRequested(t *testing.T) {
	const zver = "1:1.3.dfsg-3.1ubuntu2.2"
	const jqver = "1.7.1-3ubuntu0.24.04.2"

	snap, filesDir := minimalSnapshotWithHolds(t)
	snap.Target.ForeignArchs = []string{"i386"}
	workDir := t.TempDir()
	writeFakePackagesIndex(t, filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists"),
		fakeArchivePkg{Name: "jq", Version: jqver, Arch: "amd64", Size: 100, Depends: "libjq1"},
		fakeArchivePkg{Name: "libjq1", Version: jqver, Arch: "amd64", Size: 200, Depends: "zlib1g"},
		fakeArchivePkg{Name: "zlib1g", Version: zver, Arch: "amd64", Size: 300},
		fakeArchivePkg{Name: "zlib1g", Version: zver, Arch: "i386", Size: 310},
	)

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.7.14 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	runner.on(argsAreSet([]string{"-s", "install", "--"}, "jq", "zlib1g:i386"), Output{Stdout: []byte(
		"Inst jq (" + jqver + " Ubuntu:24.04/noble [amd64])\n" +
			"Inst libjq1 (" + jqver + " Ubuntu:24.04/noble [amd64])\n" +
			"Inst zlib1g (" + zver + " Ubuntu:24.04/noble [amd64])\n" +
			"Inst zlib1g:i386 (" + zver + " Ubuntu:24.04/noble [i386])\n",
	)})
	pins := []string{"jq:amd64=" + jqver, "libjq1:amd64=" + jqver, "zlib1g:amd64=" + zver, "zlib1g:i386=" + zver}
	runner.on(argsEqual(append([]string{"install", "--print-uris", "-y", "--"}, pins...)...), Output{})
	runner.on(argsEqual(append([]string{"--download-only", "-y", "install", "--"}, pins...)...), Output{})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Packages:         []string{"jq", "zlib1g:i386"},
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got := map[string]string{}
	for _, s := range plan.Selections {
		got[s.Name+":"+s.Arch] = s.Reason
	}
	want := map[string]string{
		"jq:amd64":     lock.ReasonRequested,
		"libjq1:amd64": lock.ReasonDependencyOfPrefix + "jq",
		"zlib1g:amd64": lock.ReasonDependencyOfPrefix + "jq",
		"zlib1g:i386":  lock.ReasonRequested,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("selection reasons = %v, want %v", got, want)
	}
	if w, ok := warningByCode(plan.Warnings, "resolver.already-satisfied"); ok {
		t.Errorf("resolver.already-satisfied fired for a request this very plan satisfied: %+v", w)
	}
}

// TestResolve_ForeignArchRequest_DependenciesNameTheRequestedPackage is the
// foreign-arch-i386 fixture's shape (test/e2e/fixtures/
// foreign-arch-i386.json, debian 12): the whole request is one foreign-arch
// package, and every other selection is part of its closure.
//
// The operand carries BOTH qualifiers — "zlib1g:i386=1:1.2.13.dfsg-1", an
// operator pinning an exact foreign-arch version — because that is the
// combination this is easiest to get wrong a second time: split the whole
// operand on its first colon and the architecture reads as "i386=1", which
// matches nothing, exactly the failure being replaced.
//
// The dependency Reasons are as much the point as the requested one. Before
// this, all six packages in that fixture's real bundle were recorded as
// "dependency-of:zlib1g:i386" — including zlib1g itself, a package recorded
// as a dependency of itself, and every one of them naming a parent that
// appears nowhere in the lock, "zlib1g:i386" being an operand and not a
// package name.
func TestResolve_ForeignArchRequest_DependenciesNameTheRequestedPackage(t *testing.T) {
	const zver = "1:1.2.13.dfsg-1"
	const libcver = "2.36-9+deb12u14"

	snap, filesDir := minimalSnapshotWithHolds(t)
	snap.Target.ForeignArchs = []string{"i386"}
	workDir := t.TempDir()
	writeFakePackagesIndex(t, filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists"),
		fakeArchivePkg{Name: "zlib1g", Version: zver, Arch: "i386", Size: 90, Depends: "libc6 (>= 2.14)"},
		fakeArchivePkg{Name: "libc6", Version: libcver, Arch: "i386", Size: 2600},
	)

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	runner.on(argsEqual("-s", "install", "--", "zlib1g:i386="+zver), Output{Stdout: []byte(
		"Inst libc6:i386 (" + libcver + " Debian:12/stable [i386])\n" +
			"Inst zlib1g:i386 (" + zver + " Debian:12/stable [i386])\n",
	)})
	pins := []string{"libc6:i386=" + libcver, "zlib1g:i386=" + zver}
	runner.on(argsEqual(append([]string{"install", "--print-uris", "-y", "--"}, pins...)...), Output{})
	runner.on(argsEqual(append([]string{"--download-only", "-y", "install", "--"}, pins...)...), Output{})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Packages:         []string{"zlib1g:i386=" + zver},
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got := map[string]string{}
	for _, s := range plan.Selections {
		got[s.Name+":"+s.Arch] = s.Reason
	}
	want := map[string]string{
		"zlib1g:i386": lock.ReasonRequested,
		"libc6:i386":  lock.ReasonDependencyOfPrefix + "zlib1g",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("selection reasons = %v, want %v", got, want)
	}
	if w, ok := warningByCode(plan.Warnings, "resolver.already-satisfied"); ok {
		t.Errorf("resolver.already-satisfied fired for the one package this plan was built to fetch: %+v", w)
	}
}
