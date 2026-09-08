package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/bundle"
	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/engine"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/install"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/store"
	"github.com/inferops/debark/core/verify"
)

// builtFixture is the outcome of one full engine.Build run, shared by every
// subtest in TestFullPipeline so the pipeline runs exactly once and every
// join is checked against the same real bundle.
type builtFixture struct {
	WorkDir string
	OutDir  string
	PubKey  string
	Result  *buildjob.BuildResult
	Events  *evidence.Collector
}

// runFixtureBuild drives engine.Build with a fake apt.Backend (this
// package's own; core/apt is a different, in-flight package this test does
// not depend on) and real everything else: a real content store, a real
// repository.Writer, a real ed25519 signer from sign.GenerateKey, and a
// real evidence collector (task requirement 3).
func runFixtureBuild(t *testing.T, workDir string) builtFixture {
	t.Helper()
	ctx := context.Background()

	archivePath, _ := buildFixtureSnapshotArchive(t, filepath.Join(workDir, "snapshot-src"))

	privKey := filepath.Join(workDir, "keys", "operator.key")
	if _, err := sign.GenerateKey(privKey, "debark integration test key"); err != nil {
		t.Fatalf("sign.GenerateKey: %v", err)
	}
	pubKey := strings.TrimSuffix(privKey, sign.PrivateKeyFileSuffix) + sign.PublicKeyFileSuffix

	signer, err := sign.SignerFor(ctx, privKey)
	if err != nil {
		t.Fatalf("sign.SignerFor: %v", err)
	}
	t.Cleanup(func() { _ = signer.Close() })

	st, err := store.Open(filepath.Join(workDir, "store"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	events := evidence.NewCollector(map[string]any{"harness": "test/integration"})

	deps := engine.Deps{
		Backend: newFakeAptBackend(),
		Store:   st,
		Repo:    repository.NewWriter(),
		Signer:  signer,
		Events:  events,
	}
	eng, err := engine.New(deps)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	outDir := filepath.Join(workDir, "bundle")
	req := buildjob.BuildRequest{
		SchemaVersion: buildjob.SchemaVersion,
		SnapshotRef:   archivePath,
		Inputs:        buildjob.Inputs{Packages: []string{"aaa-base"}},
		Options: buildjob.Options{
			UpdateMode: buildjob.UpdateAdditive,
			// Honesty over convenience: there is no real apt backend in
			// this process to answer "does this bundle install with the
			// network gone", so the check is explicitly turned off rather
			// than answered by the fake backend. See
			// assertClosedWorldHonest and the fakeAptBackend.ClosedWorld
			// doc comment.
			ClosedWorldCheck: boolPtr(false),
		},
		Output: buildjob.Output{
			Path:   outDir,
			Format: buildjob.FormatDir,
			Sign:   buildjob.SignOptions{Required: true},
		},
	}

	res, err := eng.Build(ctx, req)
	if err != nil {
		t.Fatalf("engine.Build: %v", err)
	}

	return builtFixture{WorkDir: workDir, OutDir: outDir, PubKey: pubKey, Result: res, Events: events}
}

// TestFullPipeline runs the entire pipeline once and asserts at every join
// named in the contract brief: bundle tree shape, pool-path agreement across
// lock/bundle/repository, manifest file coverage, architecture-qualified
// install entries, snapshot digest agreement, an honest closed-world state,
// a real verify.Verify pass, an export/import round trip through media,
// install.Plan against a fake apt, and the negative tamper case.
func TestFullPipeline(t *testing.T) {
	workDir := t.TempDir()
	built := runFixtureBuild(t, workDir)
	res := built.Result

	if res.ExitClass != buildjob.ExitSuccess {
		t.Fatalf("BuildResult.ExitClass = %q, want %q (warnings=%v unresolved=%v fetch_failed=%v)",
			res.ExitClass, buildjob.ExitSuccess, res.Warnings, res.Unresolved, res.FetchFailed)
	}
	if !res.Signed {
		t.Error("BuildResult.Signed = false, want true (a real signer was configured)")
	}
	if res.Stats.PackageCount != len(fixturePlanSpecs()) {
		t.Errorf("Stats.PackageCount = %d, want %d", res.Stats.PackageCount, len(fixturePlanSpecs()))
	}
	if len(res.Warnings) != 0 {
		t.Errorf("BuildResult.Warnings = %v, want none", res.Warnings)
	}

	t.Run("bundle tree shape matches design section 3.4.4", func(t *testing.T) {
		assertBundleTreeShape(t, built.OutDir)
	})

	t.Run("pool paths agree: lock vs bundle vs repository", func(t *testing.T) {
		assertPoolPathsAgree(t, built.OutDir)
	})

	t.Run("manifest Files exactly covers the bundle, no extras, none missing", func(t *testing.T) {
		assertManifestFilesExact(t, built.OutDir)
	})

	t.Run("lock install entries are architecture-qualified end to end", func(t *testing.T) {
		assertLockInstallQualified(t, built.OutDir)
	})

	t.Run("snapshot digest: manifest agrees with the bundle copy", func(t *testing.T) {
		assertSnapshotDigestAgreement(t, built.OutDir)
	})

	t.Run("closed-world check is honestly skipped, never a claimed ok", func(t *testing.T) {
		assertClosedWorldHonest(t, built.OutDir)
	})

	t.Run("evidence collector matches evidence.json", func(t *testing.T) {
		assertEvidenceMatches(t, built.OutDir, built.Events)
	})

	// Task requirement 5: the single most valuable assertion in the file.
	t.Run("verify.Verify passes with the operator key supplied out of band", func(t *testing.T) {
		report := runVerify(t, built.OutDir, built.PubKey, false)
		if !report.OK {
			t.Fatalf("verify.Verify: OK=false, problems=%+v", report.Problems)
		}
		if !report.Signed {
			t.Error("verify report Signed=false, want true")
		}
		if len(report.Problems) != 0 {
			t.Errorf("verify report Problems = %+v, want none", report.Problems)
		}
	})

	// Task requirement 6.
	var importedDir string
	t.Run("export, reimport through media, verify again", func(t *testing.T) {
		importedDir = exportReimportAndVerify(t, workDir, built.OutDir, built.PubKey)
	})

	// Task requirement 7: run against the round-tripped copy when we have
	// one, so this also exercises install.Plan reading a bundle that has
	// actually survived the tar export/import boundary.
	t.Run("install.Plan matches the lock's install set exactly", func(t *testing.T) {
		dir := built.OutDir
		if importedDir != "" {
			dir = importedDir
		}
		assertInstallPlanMatchesLock(t, dir, built.PubKey)
	})

	// Task requirement 9: the negative case, on a fresh copy so it never
	// disturbs the bundle other subtests read.
	t.Run("corrupted pool byte fails verify with ProblemFileDigest", func(t *testing.T) {
		assertCorruptionDetected(t, workDir, built.OutDir, built.PubKey)
	})
}

// ---- individual assertions --------------------------------------------------

func mustExistFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Errorf("expected %s to exist: %v", path, err)
		return
	}
	if info.IsDir() {
		t.Errorf("expected %s to be a file, found a directory", path)
	}
}

func assertBundleTreeShape(t *testing.T, dir string) {
	t.Helper()
	for _, rel := range []string{
		manifest.FileName,
		manifest.SigFileName,
		lock.FileName,
		bundle.SnapshotFile,
		evidence.FileName,
		bundle.ReadmeFile,
		filepath.Join("repo", "Release"),
		filepath.Join("repo", "Packages"),
		filepath.Join("repo", "Packages.gz"),
	} {
		mustExistFile(t, filepath.Join(dir, rel))
	}

	for _, spec := range fixturePlanSpecs() {
		want := repository.PoolPath(spec.Name, spec.filename())
		if want == "" {
			t.Fatalf("repository.PoolPath(%q, %q) returned empty", spec.Name, spec.filename())
		}
		mustExistFile(t, filepath.Join(dir, "repo", filepath.FromSlash(want)))
	}

	// bundle.Assemble also writes these two run-accounting files
	// (core/bundle/api.go's AddedFile/RemovedFile). They are not part of
	// the bundle layout design section 3.4.4 documents, but manifest.Build
	// faithfully walks the whole tree and picks them up regardless (see
	// assertManifestFilesExact) — asserted explicitly here so a future
	// change to this is a deliberate, visible edit to this test rather
	// than a silent tree-shape drift. See the implementation notes "extra files"
	// observation.
	mustExistFile(t, filepath.Join(dir, bundle.AddedFile))
	mustExistFile(t, filepath.Join(dir, bundle.RemovedFile))
}

func assertPoolPathsAgree(t *testing.T, dir string) {
	t.Helper()
	l, err := lock.Load(dir)
	if err != nil {
		t.Fatalf("lock.Load: %v", err)
	}
	packagesText, err := os.ReadFile(filepath.Join(dir, "repo", "Packages"))
	if err != nil {
		t.Fatalf("read repo/Packages: %v", err)
	}

	for _, spec := range fixturePlanSpecs() {
		want := repository.PoolPath(spec.Name, spec.filename())

		pkg, ok := l.Find(spec.Name, spec.Arch)
		if !ok {
			t.Errorf("lock.json has no package entry for %s/%s", spec.Name, spec.Arch)
			continue
		}
		if pkg.Filename != want {
			t.Errorf("lock.Package(%s).Filename = %q, want %q (repository.PoolPath)", spec.Name, pkg.Filename, want)
		}
		if _, err := os.Stat(filepath.Join(dir, "repo", filepath.FromSlash(want))); err != nil {
			t.Errorf("bundle did not materialise a pool file at %s, the path lock.json claims: %v", want, err)
		}
		if !strings.Contains(string(packagesText), "Filename: "+want+"\n") {
			t.Errorf("repo/Packages has no stanza with \"Filename: %s\" (what lock.json and the pool layout both expect)", want)
		}
	}
}

func assertManifestFilesExact(t *testing.T, dir string) {
	t.Helper()
	m, _, err := manifest.Load(dir)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}

	inManifest := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		if inManifest[f.Path] {
			t.Errorf("manifest.Files lists %s more than once", f.Path)
		}
		inManifest[f.Path] = true
	}

	onDisk := map[string]bool{}
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == manifest.FileName || rel == manifest.SigFileName {
			return nil
		}
		onDisk[rel] = true
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk bundle dir: %v", walkErr)
	}

	for path := range onDisk {
		if !inManifest[path] {
			t.Errorf("%s exists in the bundle but has no manifest.Files entry (verify reports this as %s)",
				path, verify.ProblemFileUnexpected)
		}
	}
	for path := range inManifest {
		if !onDisk[path] {
			t.Errorf("manifest.Files lists %s but it is not present in the bundle (verify reports this as %s)",
				path, verify.ProblemFileMissing)
		}
	}

	// Called out explicitly because the contract brief calls them out
	// explicitly: both are written by engine.finalizeBundle AFTER
	// bundle.Assemble's own first, discarded manifest build, so only the
	// FINAL manifest.Build call (also in finalizeBundle, once both files
	// exist) could possibly have picked them up.
	for _, must := range []string{evidence.FileName, bundle.ReadmeFile} {
		if !inManifest[must] {
			t.Errorf("manifest.Files does not include %s", must)
		}
	}
}

// splitInstallEntryForTest parses the lock's required "name:arch=version"
// install-entry form. This is a third, independent implementation of the
// same grammar core/lock's unexported parseInstallEntry and
// core/install/select.go's splitInstallEntry each already implement —
// deliberately: this test should not just trust lock.Validate (or
// install's own parser) to grade its own homework when checking that the
// format actually made it end to end.
func splitInstallEntryForTest(s string) (name, arch, version string, ok bool) {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 || eq == len(s)-1 {
		return "", "", "", false
	}
	left, ver := s[:eq], s[eq+1:]
	colon := strings.IndexByte(left, ':')
	if colon <= 0 || colon == len(left)-1 {
		return "", "", "", false
	}
	return left[:colon], left[colon+1:], ver, true
}

func assertLockInstallQualified(t *testing.T, dir string) {
	t.Helper()
	l, err := lock.Load(dir)
	if err != nil {
		t.Fatalf("lock.Load: %v", err)
	}
	if err := lock.Validate(l); err != nil {
		t.Fatalf("lock.Validate: %v", err)
	}
	if len(l.Install) == 0 {
		t.Fatal("lock.Install is empty, want at least the requested package")
	}
	for _, entry := range l.Install {
		name, arch, version, ok := splitInstallEntryForTest(entry)
		if !ok {
			t.Errorf("lock.Install entry %q is not in the required name:arch=version form", entry)
			continue
		}
		pkg, found := l.Find(name, arch)
		if !found || pkg.Version != version {
			t.Errorf("lock.Install entry %q does not name a package present in lock.Packages at that exact version", entry)
		}
	}
}

func assertSnapshotDigestAgreement(t *testing.T, dir string) {
	t.Helper()
	m, _, err := manifest.Load(dir)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}

	// This is the check that actually matters in production, and the one
	// verify.Verify itself performs (core/verify/verify.go's
	// checkLockAndSnapshot): the digest is recomputed from the raw bytes of
	// the snapshot.json COPY inside the bundle, canonicalised — never by
	// re-running snapshot.Open against it.
	raw, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err != nil {
		t.Fatalf("read snapshot.json: %v", err)
	}
	canon, err := canonical.Transform(raw)
	if err != nil {
		t.Fatalf("canonical.Transform(snapshot.json): %v", err)
	}
	got := canonical.DigestBytes(canon)
	if got != m.SnapshotDigest {
		t.Errorf("manifest.SnapshotDigest = %s, but the canonical digest of the snapshot.json copy in the bundle is %s",
			m.SnapshotDigest, got)
	}

	// FINDING: snapshot.Open cannot be pointed at
	// the bundle's snapshot.json copy to independently re-derive this
	// digest, even though that is the obvious thing a caller might try.
	// core/bundle only ever copies the snapshot DOCUMENT into a bundle
	// (assemble.go's writeSnapshotJSON) — never the files/ tree the
	// document's own File entries reference — but snapshot.Open's
	// directory-mode contract (archive.go's doOpen -> verifyExtractedFiles)
	// unconditionally requires every referenced captured file to exist on
	// disk and match its digest, and fails with dferr.Verification
	// otherwise. core/bundle.Open itself knows this and deliberately does
	// NOT call snapshot.Open for this exact reason (see bundle/open.go's
	// loadSnapshotJSON doc comment) — this assertion demonstrates that the
	// trap that comment warns about is real, so the gap is documented
	// rather than merely asserted in prose. Owner: core/snapshot and/or
	// core/bundle — either snapshot.Open should grow a document-only mode,
	// or this should be called out loudly on Open itself.
	_, openErr := snapshot.Open(context.Background(), dir)
	switch {
	case openErr == nil:
		t.Error("snapshot.Open(bundleDir) unexpectedly succeeded — if bundle now carries a files/ tree, or " +
			"snapshot.Open grew a document-only mode, this finding is resolved: update this test and the implementation notes")
	case dferr.ClassOf(openErr) != dferr.Verification:
		t.Errorf("snapshot.Open(bundleDir) failed as expected but with class %v, want %v: %v",
			dferr.ClassOf(openErr), dferr.Verification, openErr)
	default:
		t.Logf("confirmed finding: snapshot.Open(bundleDir) fails (class=%v): %v", dferr.ClassOf(openErr), openErr)
	}
}

func assertClosedWorldHonest(t *testing.T, dir string) {
	t.Helper()
	l, err := lock.Load(dir)
	if err != nil {
		t.Fatalf("lock.Load: %v", err)
	}
	if l.ClosedWorld.Result != lock.ClosedWorldSkipped {
		t.Errorf("lock.ClosedWorld.Result = %q, want %q: a build with no real apt backend must never claim a check that never ran",
			l.ClosedWorld.Result, lock.ClosedWorldSkipped)
	}
	if l.ClosedWorld.Detail == "" {
		t.Error("lock.ClosedWorld.Detail is empty; an honest skip should say why")
	}
}

func hasEventType(events []evidence.Event, typ string) bool {
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func assertEvidenceMatches(t *testing.T, dir string, external *evidence.Collector) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, evidence.FileName))
	if err != nil {
		t.Fatalf("read evidence.json: %v", err)
	}
	var doc evidence.Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse evidence.json: %v", err)
	}

	externalEvents := external.Events()
	if len(doc.Events) == 0 {
		t.Error("evidence.json has no events")
	}
	if len(externalEvents) == 0 {
		t.Error("the externally supplied evidence.Collector (Deps.Events) recorded no events")
	}

	for _, typ := range []string{
		evidence.TypeSnapshotLoaded, evidence.TypeBackendSelected, evidence.TypeBundleAssembled,
		evidence.TypeClosedWorld,
	} {
		if !hasEventType(doc.Events, typ) {
			t.Errorf("evidence.json has no %s event", typ)
		}
		if !hasEventType(externalEvents, typ) {
			t.Errorf("externally supplied evidence collector recorded no %s event", typ)
		}
	}

	// FINDING: evidence.json is written to disk in
	// engine/finalize.go's finalizeBundle BEFORE the manifest is signed, so
	// the manifest.signed event — emitted only after signing succeeds, and
	// arguably the single most security-relevant event in the whole build —
	// can never appear in the persisted file. finalizeBundle's own doc
	// comment claims the file holds "every event emitted above, including
	// doctor.finding and closed_world.checked", which is true for those two
	// but not for manifest.signed. The externally supplied Deps.Events
	// collector (fed live via evidence.MultiSink) DOES receive it, which is
	// the only reason this is visible at all: nothing else in the tree
	// checks evidence.json's completeness against the live event stream.
	// This is asserted precisely (exact expected count, exact missing type)
	// rather than just logged, so a future reordering fix in finalize.go
	// flips these two checks constructively instead of silently going
	// unnoticed. Owner: core/engine.
	const signedType = evidence.TypeManifestSigned
	if hasEventType(doc.Events, signedType) {
		t.Logf("evidence.json now includes a %s event — if finalizeBundle's write order changed, this finding is resolved: update this test", signedType)
		if len(doc.Events) != len(externalEvents) {
			t.Errorf("evidence.json has %d events, live collector has %d, and both now agree %s is present: investigate a NEW discrepancy",
				len(doc.Events), len(externalEvents), signedType)
		}
	} else {
		if !hasEventType(externalEvents, signedType) {
			t.Errorf("even the live Deps.Events collector never saw a %s event — the finding has changed shape, investigate", signedType)
		}
		if len(doc.Events) != len(externalEvents)-1 {
			t.Errorf("evidence.json has %d events, live Deps.Events collector has %d — expected exactly one fewer "+
				"(the missing %s event; see finalize.go's write-before-sign ordering)",
				len(doc.Events), len(externalEvents), signedType)
		}
	}
}

func runVerify(t *testing.T, bundleDir, pubKeyPath string, allowUnsigned bool) *verify.Report {
	t.Helper()
	v := verify.New()
	report, err := v.Verify(context.Background(), bundleDir, verify.Options{
		Keys:          sign.KeySource{Files: []string{pubKeyPath}},
		AllowUnsigned: allowUnsigned,
	})
	if err != nil {
		t.Fatalf("verify.Verify: %v", err)
	}
	return report
}

func exportReimportAndVerify(t *testing.T, workDir, bundleDir, pubKeyPath string) string {
	t.Helper()
	ctx := context.Background()

	tarPath := filepath.Join(workDir, "exported.debark.tar.zst")
	if err := bundle.ExportTar(ctx, bundleDir, tarPath); err != nil {
		t.Fatalf("bundle.ExportTar: %v", err)
	}
	if _, err := os.Stat(tarPath); err != nil {
		t.Fatalf("exported tar does not exist: %v", err)
	}

	importDir := filepath.Join(workDir, "imported")
	if err := bundle.ImportTar(ctx, tarPath, importDir); err != nil {
		t.Fatalf("bundle.ImportTar: %v", err)
	}

	report := runVerify(t, importDir, pubKeyPath, false)
	if !report.OK {
		t.Fatalf("verify.Verify on the re-imported bundle: OK=false, problems=%+v", report.Problems)
	}
	return importDir
}

func writeInstallRootFixture(t *testing.T, root string, target lock.Target) {
	t.Helper()
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", etcDir, err)
	}
	content := "ID=" + target.DistroID + "\nVERSION_ID=\"" + target.VersionID + "\"\nVERSION_CODENAME=" + target.Codename + "\n"
	if err := os.WriteFile(filepath.Join(etcDir, "os-release"), []byte(content), 0o644); err != nil {
		t.Fatalf("write os-release: %v", err)
	}
}

func installNameVersionsFromLock(t *testing.T, l *lock.Lock) []string {
	t.Helper()
	out := make([]string, 0, len(l.Install))
	for _, entry := range l.Install {
		name, _, version, ok := splitInstallEntryForTest(entry)
		if !ok {
			t.Fatalf("lock.Install entry %q not in name:arch=version form", entry)
		}
		out = append(out, name+"="+version)
	}
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// assertInstallPlanMatchesLock drives install.Runner.Plan (never Apply, per
// task requirement 7) against the bundle, with a real verify.Verifier and a
// fake apt-get/dpkg (see main_test.go — install.Deps has no Go-interface
// seam for this, only path strings), and checks the resulting plan matches
// the lock's install set exactly.
func assertInstallPlanMatchesLock(t *testing.T, bundleDir, pubKeyPath string) {
	t.Helper()
	l, err := lock.Load(bundleDir)
	if err != nil {
		t.Fatalf("lock.Load: %v", err)
	}

	simOutput := installSimOutput(fixturePlanSpecs())
	aptPath, dpkgPath := installFakeAptDpkg(t, l.Target.Arch, simOutput)

	rootDir := t.TempDir()
	writeInstallRootFixture(t, rootDir, l.Target)

	runner := install.New(install.Deps{
		Verifier: verify.New(),
		AptPath:  aptPath,
		DpkgPath: dpkgPath,
		Root:     rootDir,
	})

	report, err := runner.Plan(context.Background(), bundleDir, install.Options{
		Verify: verify.Options{Keys: sign.KeySource{Files: []string{pubKeyPath}}},
	})
	if err != nil {
		problems := []string{}
		if report != nil {
			problems = report.Problems
		}
		t.Fatalf("install.Plan: %v (problems=%v)", err, problems)
	}
	if !report.OK {
		t.Fatalf("install.Plan report OK=false, problems=%v", report.Problems)
	}
	if report.Verify == nil || !report.Verify.OK {
		t.Fatalf("install.Plan did not run a passing verification first: %+v", report.Verify)
	}

	wantToInstall := installNameVersionsFromLock(t, l)
	sort.Strings(wantToInstall)
	gotToInstall := append([]string(nil), report.ToInstall...)
	sort.Strings(gotToInstall)

	if !equalStringSlices(wantToInstall, gotToInstall) {
		t.Errorf("install.Plan report.ToInstall = %v, want %v (derived from lock.Install)", gotToInstall, wantToInstall)
	}
	if report.AlreadyCurrent != 0 {
		t.Errorf("report.AlreadyCurrent = %d, want 0", report.AlreadyCurrent)
	}
	if len(report.Problems) != 0 {
		t.Errorf("report.Problems = %v, want none", report.Problems)
	}
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func findFirstPoolDeb(t *testing.T, bundleDir string) string {
	t.Helper()
	var found string
	poolRoot := filepath.Join(bundleDir, "repo", "pool")
	err := filepath.WalkDir(poolRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if found != "" {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".deb") {
			found = p
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk pool: %v", err)
	}
	if found == "" {
		t.Fatalf("no .deb file found under %s", poolRoot)
	}
	return found
}

func flipOneByte(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) == 0 {
		t.Fatalf("%s is empty, cannot corrupt", path)
	}
	data[len(data)/2] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// assertCorruptionDetected is task requirement 9: corrupt one byte of one
// pool .deb in the FINISHED, assembled bundle (not a hand-built tamper
// fixture) and require verify to fail with ProblemFileDigest.
func assertCorruptionDetected(t *testing.T, workDir, bundleDir, pubKeyPath string) {
	t.Helper()
	corruptDir := filepath.Join(workDir, "corrupt-copy")
	if err := copyDir(bundleDir, corruptDir); err != nil {
		t.Fatalf("copy bundle for corruption test: %v", err)
	}

	target := findFirstPoolDeb(t, corruptDir)
	flipOneByte(t, target)

	report := runVerify(t, corruptDir, pubKeyPath, false)
	if report.OK {
		t.Fatal("verify.Verify reported OK=true against a bundle with one corrupted pool byte")
	}

	rel, err := filepath.Rel(corruptDir, target)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	rel = filepath.ToSlash(rel)

	found := false
	for _, p := range report.Problems {
		if p.Kind == verify.ProblemFileDigest && p.Path == rel {
			found = true
			if p.Expected == "" || p.Got == "" || p.Expected == p.Got {
				t.Errorf("ProblemFileDigest for %s has a suspicious Expected/Got pair: %+v", rel, p)
			}
		}
	}
	if !found {
		t.Errorf("verify.Verify problems do not include a %s for %s; got %+v", verify.ProblemFileDigest, rel, report.Problems)
	}
}
