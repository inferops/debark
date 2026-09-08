package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/bundle"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
)

// Real .deb fixtures, built in Go.
//
// The test below deliberately runs the REAL bundle.Assemble rather than this
// package's fake assembler, and Assemble's own pipeline hands the pool to the
// real repository writer, which parses each file's control data through
// pault.ag/go/debian/deb. Synthetic "fake .deb bytes" (what fixturePlan in
// fakes_test.go supplies, and all this package's other tests need) would be
// rejected there, and there is no dpkg-deb on the Windows machines these
// tests must also pass on. Same construction as core/bundle's and
// core/repository's own testdeb_test.go.

func arMember(buf *bytes.Buffer, name string, data []byte) {
	h := make([]byte, 60)
	for i := range h {
		h[i] = ' '
	}
	copy(h[0:16], name)
	copy(h[16:28], "0")
	copy(h[28:34], "0")
	copy(h[34:40], "0")
	copy(h[40:48], "100644")
	copy(h[48:58], strconv.Itoa(len(data)))
	h[58] = 0x60
	h[59] = 0x0A
	buf.Write(h)
	buf.Write(data)
	if len(data)%2 == 1 {
		buf.WriteByte('\n')
	}
}

func gzipTarOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(tarBuf.Bytes()); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return gzBuf.Bytes()
}

// realDebBytes is a minimal but genuinely parseable .deb for pkg/version/arch.
func realDebBytes(t *testing.T, pkg, version, arch string) []byte {
	t.Helper()
	control := fmt.Sprintf(
		"Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Test <test@example.com>\nDescription: a test package\n",
		pkg, version, arch)
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	arMember(&buf, "debian-binary", []byte("2.0\n"))
	arMember(&buf, "control.tar.gz", gzipTarOf(t, map[string][]byte{"./control": []byte(control)}))
	arMember(&buf, "data.tar.gz", gzipTarOf(t, map[string][]byte{}))
	return buf.Bytes()
}

// realDebPlan is fixturePlan's counterpart for tests that run the real
// assembler: the same single-selection shape, but backed by a .deb the
// repository writer can actually read.
func realDebPlan(t *testing.T, pkg, version, arch string) *resolve.Plan {
	t.Helper()
	filename := fmt.Sprintf("%s_%s_%s.deb", pkg, version, arch)
	staged := filepath.Join(t.TempDir(), filename)
	if err := os.WriteFile(staged, realDebBytes(t, pkg, version, arch), 0o644); err != nil {
		t.Fatalf("write staged %s: %v", filename, err)
	}
	return &resolve.Plan{
		Target: lock.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: arch},
		Resolver: lock.Resolver{
			Backend: lock.BackendLocal, APTVersion: "2.6.1", DpkgVersion: "1.21.22",
			PhasedUpdates: string(snapshot.PhasedNeverInclude), InstallRecommends: true,
		},
		Selections: []resolve.Selection{{
			Name: pkg, Arch: arch, Version: version, SourcePackage: pkg,
			Filename: filename, StagedPath: staged,
			Reason: lock.ReasonRequested, PublisherVerification: lock.VerifiedAPTSigned,
		}},
		Install: []string{fmt.Sprintf("%s:%s=%s", pkg, arch, version)},
	}
}

// eventOfType returns the single event of typ the collector saw, failing the
// test when there is not exactly one.
func eventOfType(t *testing.T, evs []evidence.Event, typ string) evidence.Event {
	t.Helper()
	var found []evidence.Event
	for _, e := range evs {
		if e.Type == typ {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s event, got %d (all events: %+v)", typ, len(found), evs)
	}
	return found[0]
}

func intAttr(t *testing.T, e evidence.Event, key string) int {
	t.Helper()
	v, ok := e.Attrs[key]
	if !ok {
		t.Fatalf("event %s has no %q attribute; attrs = %+v", e.Type, key, e.Attrs)
	}
	n, ok := v.(int)
	if !ok {
		t.Fatalf("event %s attribute %q = %#v, want an int", e.Type, key, v)
	}
	return n
}

func readReportLines(t *testing.T, dir, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// TestBuild_UnreferencedPoolFilesReachEvidence is the engine's half of the
// unreferenced-pool report.
//
// core/bundle computes, for every build, the pool files this run's
// repo/Packages does not index: on the media, covered by the manifest and so
// by the operator's signature, invisible to the target's apt. It writes them
// to last-run-unreferenced.txt, raises pool.unreferenced into lock.json, and
// returns them on bundle.Result.Unreferenced. evidence.json is the third
// surface an auditor reads and it was the only one of the three that said
// nothing: assemble() emitted added and removed on bundle.assembled and
// dropped the count that is not derivable from either of them.
//
// It runs the REAL bundle.Assemble, not this package's fake assembler,
// because a fake told to report one unreferenced file proves only that the
// engine can copy a number the test itself invented. Here the leftover is a
// real .deb, really left in the pool by a previous run's shape, and the count
// on the event has to survive the whole real computation — the pool walk, the
// repository writer's index, and the engine's own wiring — to come out right.
// The premise assertions below (Packages does not index the leftover, and
// last-run-removed.txt is empty) are what make the number mean "unreferenced"
// rather than "some file we happened to find".
func TestBuild_UnreferencedPoolFilesReachEvidence(t *testing.T) {
	h := newHarness(t)
	// saveVars (fakes_test.go) already captured the original, so this is
	// restored on cleanup like every other injected var.
	assembleBundle = bundle.Assemble

	const arch = "amd64"
	h.backend.resolveFn = func(ctx context.Context, in apt.ResolveInput) (*resolve.Plan, error) {
		return realDebPlan(t, "vlc", "3.0.21-1build1", arch), nil
	}

	// An earlier run's request named a package this one does not. Nothing
	// supersedes it, so an additive run leaves it in the pool — one of the
	// two ordinary sequences core/bundle's unreferenced.go names.
	leftoverRel := repository.PoolPath("tree", "tree_2.1.0-1_amd64.deb")
	if leftoverRel == "" {
		t.Fatal("repository.PoolPath rejected the leftover fixture name")
	}
	leftoverAbs := filepath.Join(h.outDir, "repo", filepath.FromSlash(leftoverRel))
	if err := os.MkdirAll(filepath.Dir(leftoverAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leftoverAbs, realDebBytes(t, "tree", "2.1.0-1", arch), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Build(context.Background(), h.req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Premise. Without these the count below proves nothing.
	if _, err := os.Stat(leftoverAbs); err != nil {
		t.Fatalf("premise broken: the leftover is not in the finished pool: %v", err)
	}
	pkgs, err := os.ReadFile(filepath.Join(h.outDir, "repo", "Packages"))
	if err != nil {
		t.Fatalf("read repo/Packages: %v", err)
	}
	if strings.Contains(string(pkgs), "Package: tree") {
		t.Fatal("premise broken: repo/Packages indexes the leftover, so it is not unreferenced")
	}
	if got := readReportLines(t, h.outDir, bundle.RemovedFile); len(got) != 0 {
		t.Fatalf("premise broken: %s = %v, want empty (an additive run removes nothing)", bundle.RemovedFile, got)
	}
	if got := readReportLines(t, h.outDir, bundle.UnreferencedFile); len(got) != 1 || got[0] != leftoverRel {
		t.Fatalf("premise broken: %s = %v, want [%s]", bundle.UnreferencedFile, got, leftoverRel)
	}

	// The fix itself: the count reaches evidence.json's bundle.assembled
	// event, alongside the two counts that were already there.
	ev := eventOfType(t, h.events.Events(), evidence.TypeBundleAssembled)
	if got := intAttr(t, ev, "unreferenced"); got != 1 {
		t.Errorf("bundle.assembled attrs[unreferenced] = %d, want 1", got)
	}
	if got := intAttr(t, ev, "added"); got != 1 {
		t.Errorf("bundle.assembled attrs[added] = %d, want 1", got)
	}
	if got := intAttr(t, ev, "removed"); got != 0 {
		t.Errorf("bundle.assembled attrs[removed] = %d, want 0", got)
	}

	// evidence.json is what actually crosses the air gap; the collector copy
	// above is only the engine's own mirror of it. Assert on the file.
	evJSON, err := os.ReadFile(filepath.Join(h.outDir, evidence.FileName))
	if err != nil {
		t.Fatalf("read %s: %v", evidence.FileName, err)
	}
	if !strings.Contains(string(evJSON), `"unreferenced": 1`) &&
		!strings.Contains(string(evJSON), `"unreferenced":1`) {
		t.Errorf("%s carries no unreferenced count on bundle.assembled:\n%s", evidence.FileName, evJSON)
	}
}

// TestBuild_CleanBuildReportsNoUnreferencedFiles is the other half: the
// attribute is emitted on every build, not only on the interesting one, so an
// auditor reading evidence.json can tell "nothing was unreferenced" from
// "this build predates the report". Also runs the real assembler.
func TestBuild_CleanBuildReportsNoUnreferencedFiles(t *testing.T) {
	h := newHarness(t)
	assembleBundle = bundle.Assemble
	h.backend.resolveFn = func(ctx context.Context, in apt.ResolveInput) (*resolve.Plan, error) {
		return realDebPlan(t, "vlc", "3.0.21-1build1", "amd64"), nil
	}

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Build(context.Background(), h.req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	if got := readReportLines(t, h.outDir, bundle.UnreferencedFile); len(got) != 0 {
		t.Fatalf("premise broken: %s = %v on a clean build, want empty", bundle.UnreferencedFile, got)
	}
	ev := eventOfType(t, h.events.Events(), evidence.TypeBundleAssembled)
	if got := intAttr(t, ev, "unreferenced"); got != 0 {
		t.Errorf("bundle.assembled attrs[unreferenced] = %d on a clean build, want 0", got)
	}
}
