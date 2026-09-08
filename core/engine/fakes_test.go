package engine

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/bundle"
	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/doctor"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/policy"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/store"
)

// callLog records, in order, which collaborator method ran. Every fake in
// this file appends its own name to a shared log, which is how the tests in
// engine_test.go assert the pipeline calls things in the right order rather
// than merely calling them at all.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (c *callLog) add(name string) {
	c.mu.Lock()
	c.calls = append(c.calls, name)
	c.mu.Unlock()
}

func (c *callLog) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// ---- fake store.Store ------------------------------------------------------

type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	entries map[string]store.Entry
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, entries: map[string]store.Entry{}}
}

func (s *fakeStore) Root() string { return "fake-store-root" }

func (s *fakeStore) Has(d string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[d]
	return ok
}

func (s *fakeStore) Path(d string) string { return filepath.Join("fake-store-root", d) }

func (s *fakeStore) Put(ctx context.Context, r io.Reader) (string, int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	sum := digest.Bytes(data)
	s.mu.Lock()
	s.objects[sum] = data
	s.mu.Unlock()
	return sum, int64(len(data)), nil
}

func (s *fakeStore) PutFile(ctx context.Context, path string, moveOK bool) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	sum := digest.Bytes(data)
	s.mu.Lock()
	s.objects[sum] = data
	s.mu.Unlock()
	if moveOK {
		_ = os.Remove(path)
	}
	return sum, int64(len(data)), nil
}

func (s *fakeStore) Open(d string) (io.ReadCloser, error) {
	s.mu.Lock()
	data, ok := s.objects[d]
	s.mu.Unlock()
	if !ok {
		return nil, dferr.New(dferr.Environment, "fakeStore: no object %s", d)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *fakeStore) Materialise(d, dest string) error {
	s.mu.Lock()
	data, ok := s.objects[d]
	s.mu.Unlock()
	if !ok {
		return dferr.New(dferr.Environment, "fakeStore: no object %s", d)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o644)
}

func (s *fakeStore) Index() (store.Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := store.Index{SchemaVersion: store.IndexSchemaVersion}
	for _, e := range s.entries {
		idx.Entries = append(idx.Entries, e)
	}
	return idx, nil
}

func (s *fakeStore) Record(e store.Entry) error {
	s.mu.Lock()
	s.entries[e.Digest] = e
	s.mu.Unlock()
	return nil
}

func (s *fakeStore) GC(ctx context.Context, keep func(string) bool) (store.GCStats, error) {
	return store.GCStats{}, nil
}

// ---- fake apt.Backend -------------------------------------------------------

type fakeBackend struct {
	kind lock.Backend
	log  *callLog

	resolveFn     func(ctx context.Context, in apt.ResolveInput) (*resolve.Plan, error)
	closedWorldFn func(ctx context.Context, in apt.ClosedWorldInput) (lock.ClosedWorld, error)
}

func (f *fakeBackend) Kind() lock.Backend { return f.kind }

func (f *fakeBackend) Probe(ctx context.Context, t snapshot.Target) (apt.Capabilities, error) {
	return apt.Capabilities{Available: true, DistroID: t.DistroID, VersionID: t.VersionID}, nil
}

func (f *fakeBackend) Resolve(ctx context.Context, in apt.ResolveInput) (*resolve.Plan, error) {
	f.log.add("apt.Resolve")
	return f.resolveFn(ctx, in)
}

func (f *fakeBackend) ClosedWorld(ctx context.Context, in apt.ClosedWorldInput) (lock.ClosedWorld, error) {
	f.log.add("apt.ClosedWorld")
	if f.closedWorldFn != nil {
		return f.closedWorldFn(ctx, in)
	}
	return lock.ClosedWorld{Result: lock.ClosedWorldOK}, nil
}

// ---- fake repository.Writer -------------------------------------------------

type fakeRepoWriter struct {
	t     *testing.T
	log   *callLog
	calls []repository.Input
}

// Write records the call and does a minimal real check: every file it was
// asked to index must actually exist under in.Dir at the moment it is
// called, which is before the caller's temp work root is cleaned up. A test
// that wants to confirm a staged file was really on disk should check that
// here, at call time, rather than after Build has returned and workRoot is
// already gone.
func (f *fakeRepoWriter) Write(ctx context.Context, in repository.Input) (*repository.Result, error) {
	f.log.add("repository.Write")
	f.calls = append(f.calls, in)
	_ = os.MkdirAll(in.Dir, 0o755)
	if f.t != nil {
		for _, pf := range in.Files {
			if _, err := os.Stat(filepath.Join(in.Dir, filepath.FromSlash(pf.Path))); err != nil {
				f.t.Errorf("fakeRepoWriter.Write: file to index does not exist: %v", err)
			}
		}
	}
	_ = os.WriteFile(filepath.Join(in.Dir, "Packages"), []byte("Package: fake\n\n"), 0o644)
	return &repository.Result{PackagesPath: "Packages", PackageCount: len(in.Files)}, nil
}

// ---- fake sign.Signer --------------------------------------------------------

type fakeSigner struct {
	kind, keyID string
	log         *callLog
	signErr     error
	closed      bool
}

func (f *fakeSigner) Kind() string  { return f.kind }
func (f *fakeSigner) KeyID() string { return f.keyID }

func (f *fakeSigner) Sign(ctx context.Context, purpose string, canon []byte) (manifest.Signature, error) {
	f.log.add("sign.Sign")
	if f.signErr != nil {
		return manifest.Signature{}, f.signErr
	}
	return manifest.Signature{
		SignerKind: f.kind, KeyID: f.keyID, Algorithm: "ed25519",
		CreatedAt: "2026-01-01T00:00:00Z", Signature: "ZmFrZS1zaWduYXR1cmU=",
	}, nil
}

func (f *fakeSigner) Close() error { f.closed = true; return nil }

// ---- fake policy.Evaluator ---------------------------------------------------

type fakePolicy struct {
	log      *callLog
	findings []policy.Finding
	err      error
	// gotInput is the last Input the evaluator was handed. A policy sees the
	// plan exactly as the pipeline finished assembling it, which makes it the
	// cheapest true observation point for anything the engine attributes to a
	// selection after the backend has answered (see attributeExternalOrigins
	// in inputs.go).
	gotInput policy.Input
}

func (f *fakePolicy) Evaluate(ctx context.Context, in policy.Input) ([]policy.Finding, error) {
	f.log.add("policy.Evaluate")
	f.gotInput = in
	return f.findings, f.err
}

// ---- fake fetch.Fetcher -------------------------------------------------------

type fakeFetcher struct {
	results []fetch.Result
}

func (f *fakeFetcher) Fetch(ctx context.Context, in buildjob.URLInput) (*fetch.Fetched, error) {
	for _, r := range f.results {
		if r.Input.URL == in.URL {
			return r.Fetched, r.Err
		}
	}
	return nil, dferr.New(dferr.Usage, "fakeFetcher: no result configured for %s", in.URL)
}

func (f *fakeFetcher) FetchAll(ctx context.Context, in []buildjob.URLInput) []fetch.Result {
	return f.results
}

// ---- fixtures -----------------------------------------------------------------

func fixtureSnapshot() *snapshot.Snapshot {
	return &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		CreatedAt:     "2026-01-01T00:00:00Z",
		Target: snapshot.Target{
			DistroID: "debian", VersionID: "12", Codename: "bookworm",
			Arch: "amd64", APTVersion: "2.6.1", DpkgVersion: "1.21.22",
		},
		DpkgStatus: snapshot.File{Path: "/var/lib/dpkg/status", ArchivePath: "var/lib/dpkg/status"},
	}
}

// fixtureArchive builds a *snapshot.Archive by struct literal. Snapshot,
// Digest and Path are exported and settable this way; filesDir and cleanup
// are not (snapshot.Open is the only real constructor, and it is still a
// stub), so FilesDir() reads back "" and Close() is a safe no-op here. The
// engine's own code (loadFileSet, in particular) is written to tolerate a
// missing files directory for exactly this reason — see snapshot.go.
func fixtureArchive() *snapshot.Archive {
	return &snapshot.Archive{
		Snapshot: fixtureSnapshot(),
		Digest:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Path:     "fixture-snapshot",
	}
}

// fixturePlan returns a plan with one selection backed by a real temporary
// file, so store.PutFile has real bytes to ingest.
func fixturePlan(t *testing.T) *resolve.Plan {
	t.Helper()
	dir := t.TempDir()
	staged := filepath.Join(dir, "vlc_3.0.21-1build1_amd64.deb")
	if err := os.WriteFile(staged, []byte("fake .deb bytes for vlc"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &resolve.Plan{
		Target: lock.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		Resolver: lock.Resolver{
			Backend: lock.BackendLocal, APTVersion: "2.6.1", DpkgVersion: "1.21.22",
			PhasedUpdates: string(snapshot.PhasedNeverInclude), InstallRecommends: true,
		},
		Selections: []resolve.Selection{{
			Name: "vlc", Arch: "amd64", Version: "3.0.21-1build1", SourcePackage: "vlc",
			Filename: "vlc_3.0.21-1build1_amd64.deb", StagedPath: staged,
			Reason: lock.ReasonRequested, PublisherVerification: lock.VerifiedAPTSigned,
		}},
		// Install entries are frozen as the architecture-qualified
		// name:arch=version form (lock.Lock.Install's own doc comment);
		// lock.Validate rejects the unqualified name=version form. This
		// fixture used to write the unqualified form and only got away with
		// it because nothing in this package's own test suite called
		// Validate — see defect 4 in the implementation notes, and
		// finalizeBundle's new lock.Validate call in finalize.go, which
		// would now reject this fixture if it stayed wrong.
		Install: []string{"vlc:amd64=3.0.21-1build1"},
	}
}

// ---- harness --------------------------------------------------------------

// harness wires every injection point (Deps and the package-level vars) to
// fakes with sane defaults, and restores the real package vars via
// t.Cleanup so tests never leak state into one another.
type harness struct {
	t     *testing.T
	calls *callLog

	backend    *fakeBackend
	repoWriter *fakeRepoWriter
	policyEval *fakePolicy
	signer     *fakeSigner
	store      *fakeStore
	events     *evidence.Collector

	deps Deps
	req  buildjob.BuildRequest

	outDir string

	assembleInputs []bundleInputSnapshot
}

// bundleInputSnapshot records just the fields the prune test needs out of
// each bundle.Input the fake assembler received, rather than keeping the
// whole (much larger) bundle.Input value around.
type bundleInputSnapshot struct {
	Prune bool
	Dir   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	calls := &callLog{}
	saveVars(t)

	openSnapshot = func(ctx context.Context, ref string) (*snapshot.Archive, error) {
		calls.add("snapshot.Open")
		return fixtureArchive(), nil
	}

	saveLock = func(dir string, l *lock.Lock) (string, error) {
		calls.add("lock.Save")
		d, err := canonical.Digest(l)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, "lock.json"), []byte("{}"), 0o644); err != nil {
			return "", err
		}
		return d, nil
	}

	buildManifest = func(ctx context.Context, in manifest.BuildInput) (*manifest.Manifest, error) {
		calls.add("manifest.Build")
		return &manifest.Manifest{
			SchemaVersion: manifest.SchemaVersion, BundleID: "test-bundle-id", CreatedAt: in.CreatedAt,
			FormatVersion: manifest.CurrentFormatVersion, Tool: in.Tool, SnapshotDigest: in.SnapshotDigest,
			LockDigest: in.LockDigest, Repository: in.Repository, Target: in.Target, EvidenceRef: in.EvidenceRef,
		}, nil
	}
	saveManifest = func(dir string, m *manifest.Manifest) error {
		calls.add("manifest.Save")
		return os.WriteFile(filepath.Join(dir, manifest.FileName), []byte("{}"), 0o644)
	}
	canonicalManifest = func(m *manifest.Manifest) ([]byte, error) {
		calls.add("manifest.Canonical")
		return canonical.Marshal(m)
	}
	saveManifestSignature = func(dir string, sig *manifest.SignatureFile) error {
		calls.add("manifest.SaveSignature")
		return os.WriteFile(filepath.Join(dir, manifest.SigFileName), []byte("{}"), 0o644)
	}
	doctorRun = func(ctx context.Context, in doctor.Input) (*doctor.Report, error) {
		calls.add("doctor.Run")
		return &doctor.Report{SchemaVersion: doctor.SchemaVersion}, nil
	}
	readmeText = func(m *manifest.Manifest, l *lock.Lock, signed bool) string {
		calls.add("bundle.ReadmeText")
		return "fake readme\n"
	}

	h := &harness{t: t, calls: calls}

	assembleBundle = func(ctx context.Context, in bundle.Input) (*bundle.Result, error) {
		calls.add("bundle.Assemble")
		if err := os.MkdirAll(filepath.Join(in.Dir, "repo", "pool"), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(in.Dir, "repo", "Release"), []byte("Origin: debark\n"), 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(in.Dir, "snapshot.json"), []byte("{}"), 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(in.Dir, "README.txt"), []byte("draft\n"), 0o644); err != nil {
			return nil, err
		}
		h.assembleInputs = append(h.assembleInputs, bundleInputSnapshot{Prune: in.Prune, Dir: in.Dir})
		l := in.Lock
		return &bundle.Result{
			Dir: in.Dir, Lock: l,
			Repo:  &repository.Result{PackagesPath: "Packages", PackageCount: len(in.Plan.Selections)},
			Stats: l.Stats,
		}, nil
	}

	tmp := t.TempDir()
	h.outDir = filepath.Join(tmp, "out")

	st := newFakeStore()
	backend := &fakeBackend{kind: lock.BackendLocal, log: calls, resolveFn: func(ctx context.Context, in apt.ResolveInput) (*resolve.Plan, error) {
		return fixturePlan(t), nil
	}}
	repoWriter := &fakeRepoWriter{t: t, log: calls}
	pol := &fakePolicy{log: calls}
	signer := &fakeSigner{kind: "ed25519-file", keyID: "test-key", log: calls}
	events := evidence.NewCollector(nil)

	h.backend, h.repoWriter, h.policyEval, h.signer, h.store, h.events = backend, repoWriter, pol, signer, st, events
	h.deps = Deps{Backend: backend, Store: st, Repo: repoWriter, Policy: pol, Signer: signer, Events: events}
	h.req = buildjob.BuildRequest{
		SchemaVersion: buildjob.SchemaVersion,
		SnapshotRef:   "fixture-snapshot",
		Inputs:        buildjob.Inputs{Packages: []string{"vlc"}},
		Output:        buildjob.Output{Path: h.outDir, Format: buildjob.FormatDir},
	}

	fixedTime, err := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	timeNow = func() time.Time { return fixedTime }

	return h
}

// saveVars snapshots every package-level injection var this test file
// overrides and restores the originals on cleanup, so tests never leak
// fakes into one another or into a later, unrelated test binary run.
func saveVars(t *testing.T) {
	t.Helper()
	origOpenSnapshot := openSnapshot
	origSaveLock := saveLock
	origBuildManifest := buildManifest
	origSaveManifest := saveManifest
	origCanonicalManifest := canonicalManifest
	origSaveManifestSignature := saveManifestSignature
	origDoctorRun := doctorRun
	origReadmeText := readmeText
	origAssembleBundle := assembleBundle
	origExportTar := exportTar
	origParseListFile := parseListFile
	origScanDir := scanDir
	origFromLocalFile := fromLocalFile
	origNewFetcher := newFetcher
	origDebPackageName := debPackageName
	origSignerFor := signerFor
	origLoadPolicy := loadPolicy
	origLoadApprovedKeys := loadApprovedKeys
	origOpenStore := openStore
	origSelectBackendFn := selectBackendFn
	origTimeNow := timeNow

	t.Cleanup(func() {
		openSnapshot = origOpenSnapshot
		saveLock = origSaveLock
		buildManifest = origBuildManifest
		saveManifest = origSaveManifest
		canonicalManifest = origCanonicalManifest
		saveManifestSignature = origSaveManifestSignature
		doctorRun = origDoctorRun
		readmeText = origReadmeText
		assembleBundle = origAssembleBundle
		exportTar = origExportTar
		parseListFile = origParseListFile
		scanDir = origScanDir
		fromLocalFile = origFromLocalFile
		newFetcher = origNewFetcher
		debPackageName = origDebPackageName
		signerFor = origSignerFor
		loadPolicy = origLoadPolicy
		loadApprovedKeys = origLoadApprovedKeys
		openStore = origOpenStore
		selectBackendFn = origSelectBackendFn
		timeNow = origTimeNow
	})
}
