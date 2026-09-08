package sbom

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/inferops/debark/core/lock"
)

var update = flag.Bool("update", false, "update golden files")

var serialPattern = regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// fixtureComponents exercises ordinary names, an epoch (':'), a Debian
// revision with '~' and '+' (backport/security-update conventions), and
// foreign architectures — the version characters purl must percent-encode.
func fixtureComponents() []Component {
	return []Component{
		{Name: "libc6", Version: "2.36-9+deb12u4", Arch: "amd64", Distro: "debian", SHA256: strings.Repeat("a1", 32), SourcePackage: "glibc"},
		{Name: "vlc", Version: "3.0.21-1build1", Arch: "amd64", Distro: "ubuntu", SHA256: strings.Repeat("b2", 32), SourcePackage: "vlc"},
		{Name: "zoom", Version: "1:5.17.11.4059-1", Arch: "amd64", Distro: "debian", SHA256: strings.Repeat("c3", 32), SourcePackage: ""},
		{Name: "openjdk-17-jre-headless", Version: "17.0.13+11~us1-0ubuntu1~22.04", Arch: "arm64", Distro: "ubuntu", SHA256: strings.Repeat("d4", 32), SourcePackage: "openjdk-17"},
	}
}

func fixtureOptions() Options {
	return Options{
		BundleID:      "bundle-20260903-0001",
		BundleVersion: "noble-amd64",
		Timestamp:     "2026-09-03T12:00:00Z",
		ToolName:      "debark",
		ToolVersion:   "1.0.0",
	}
}

func goldenPath() string { return filepath.Join("testdata", "golden", "sbom.cdx.json") }

func TestBuildGolden(t *testing.T) {
	doc, err := Build(fixtureOptions(), fixtureComponents())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got, err := doc.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}

	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath()), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath(), got, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	want, err := os.ReadFile(goldenPath())
	if err != nil {
		t.Fatalf("read golden (run go test -run TestBuildGolden -update to create it): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Build output does not match golden file %s.\n--- got ---\n%s\n--- want ---\n%s", goldenPath(), got, want)
	}
}

// TestBuildDeterministic proves two builds from the same inputs are
// byte-identical, independent of map iteration or any other hidden
// non-determinism.
func TestBuildDeterministic(t *testing.T) {
	var prev []byte
	for i := 0; i < 5; i++ {
		doc, err := Build(fixtureOptions(), fixtureComponents())
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		got, err := doc.JSON()
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && string(got) != string(prev) {
			t.Fatalf("run %d differs from run %d", i, i-1)
		}
		prev = got
	}
}

func TestSerialNumberDeterministic(t *testing.T) {
	a := deterministicSerialNumber("bundle-1")
	b := deterministicSerialNumber("bundle-1")
	c := deterministicSerialNumber("bundle-2")
	if a != b {
		t.Fatalf("same seed produced different serial numbers: %q vs %q", a, b)
	}
	if a == c {
		t.Fatalf("different seeds produced the same serial number")
	}
	if !serialPattern.MatchString(a) {
		t.Fatalf("serial number %q does not match the CycloneDX pattern", a)
	}
}

func TestPURLEscaping(t *testing.T) {
	cases := []struct{ distro, name, version, arch, want string }{
		{"debian", "vlc", "3.0.21-1build1", "amd64", "pkg:deb/debian/vlc@3.0.21-1build1?arch=amd64"},
		{"debian", "zoom", "1:5.17.11.4059-1", "amd64", "pkg:deb/debian/zoom@1%3A5.17.11.4059-1?arch=amd64"},
		{"ubuntu", "openjdk-17-jre-headless", "17.0.13+11~us1-0ubuntu1~22.04", "arm64",
			"pkg:deb/ubuntu/openjdk-17-jre-headless@17.0.13%2B11~us1-0ubuntu1~22.04?arch=arm64"},
	}
	for _, c := range cases {
		got, err := PURL(c.distro, c.name, c.version, c.arch)
		if err != nil {
			t.Fatalf("PURL(%q,%q,%q,%q): %v", c.distro, c.name, c.version, c.arch, err)
		}
		if got != c.want {
			t.Errorf("PURL(%q,%q,%q,%q) = %q, want %q", c.distro, c.name, c.version, c.arch, got, c.want)
		}
	}
}

func TestFromLock(t *testing.T) {
	l := &lock.Lock{
		Target: lock.Target{DistroID: "debian", Codename: "bookworm", Arch: "amd64"},
		Packages: []lock.Package{
			{Name: "zeta", Version: "1.0", Arch: "amd64", SHA256: strings.Repeat("1", 64)},
			{Name: "alpha", Version: "2.0", Arch: "amd64", SHA256: strings.Repeat("2", 64)},
		},
	}
	doc, err := FromLock(l, "bundle-x", "2026-09-03T00:00:00Z")
	if err != nil {
		t.Fatalf("FromLock: %v", err)
	}
	if len(doc.Components) != 2 {
		t.Fatalf("got %d components, want 2", len(doc.Components))
	}
	if doc.Components[0].Name != "alpha" || doc.Components[1].Name != "zeta" {
		t.Errorf("FromLock did not sort components by name: got %q, %q", doc.Components[0].Name, doc.Components[1].Name)
	}
}

func TestBuildRequiresTimestampAndBundleID(t *testing.T) {
	if _, err := Build(Options{}, nil); err == nil {
		t.Error("Build with no Timestamp/BundleID should error")
	}
	if _, err := Build(Options{Timestamp: "2026-09-03T00:00:00Z"}, nil); err == nil {
		t.Error("Build with no BundleID should error")
	}
}

// --- CycloneDX 1.6 schema validation --------------------------------------

const schemaDir = "testdata/cyclonedx"

// TestValidatesAgainstOfficialSchema checks Build's golden output against the
// real, published CycloneDX 1.6 JSON schema (embedded in testdata/cyclonedx —
// fetched once from https://cyclonedx.org/schema/bom-1.6.schema.json et al.,
// see that directory's provenance in that work report; this test needs no
// network). This is the "does it actually validate" deliverable, not an
// estimate: a failure here means real, actionable schema errors, printed
// below.
func TestValidatesAgainstOfficialSchema(t *testing.T) {
	if _, err := os.Stat(filepath.Join(schemaDir, "bom-1.6.schema.json")); err != nil {
		t.Skipf("schema not present at %s: %v", schemaDir, err)
	}
	v := loadSchemaValidator(t, schemaDir)

	doc, err := Build(fixtureOptions(), fixtureComponents())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	raw, err := doc.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		t.Fatalf("re-parse own output: %v", err)
	}

	errs := v.Validate(instance)
	if len(errs) != 0 {
		t.Errorf("document does NOT validate against CycloneDX 1.6 schema (%d error(s)):", len(errs))
		for _, e := range errs {
			t.Errorf("  %s", e)
		}
		t.Logf("document under test:\n%s", raw)
	} else {
		t.Logf("document validates cleanly against the official CycloneDX %s JSON schema (%d bytes checked, %d components)",
			SpecVersion, len(raw), len(doc.Components))
	}
}

// TestSelfValidatorSanity proves the hand-rolled validator actually rejects
// bad input — a validator that always returns "no errors" would make
// TestValidatesAgainstOfficialSchema meaningless.
func TestSelfValidatorSanity(t *testing.T) {
	if _, err := os.Stat(filepath.Join(schemaDir, "bom-1.6.schema.json")); err != nil {
		t.Skipf("schema not present at %s: %v", schemaDir, err)
	}
	v := loadSchemaValidator(t, schemaDir)

	bad := map[string]any{
		// missing required "specVersion"; bomFormat has a disallowed value;
		// components[0] is missing the required "name" and uses a "type" not
		// in the enum.
		"bomFormat": "NotCycloneDX",
		"components": []any{
			map[string]any{"type": "not-a-real-type"},
		},
	}
	errs := v.Validate(bad)
	if len(errs) == 0 {
		t.Fatal("validator reported zero errors for an intentionally invalid document")
	}
	t.Logf("validator correctly rejected a bad document with %d error(s), e.g. %q", len(errs), errs[0])
}

func TestSyftFallback(t *testing.T) {
	orig := runSyft
	defer func() { runSyft = orig }()

	t.Run("syft produces a valid document", func(t *testing.T) {
		runSyft = func(ctx context.Context, target string) ([]byte, error) {
			return []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[]}`), nil
		}
		// SyftAvailable() depends on the real PATH; BuildPreferSyft only
		// takes the syft branch when it is true, so this test's assertion
		// has to account for both possible environments.
		out, src, err := BuildPreferSyft(context.Background(), t.TempDir(), fixtureOptions(), fixtureComponents())
		if err != nil {
			t.Fatalf("BuildPreferSyft: %v", err)
		}
		if SyftAvailable() {
			if src != SourceSyft {
				t.Errorf("syft is on PATH and returned valid output, want Source=syft, got %s", src)
			}
		} else if src != SourceNative {
			t.Errorf("syft is not on PATH, want Source=native, got %s", src)
		}
		if len(out) == 0 {
			t.Error("empty output")
		}
	})

	t.Run("syft missing or failing falls back to native silently", func(t *testing.T) {
		// This exercises the fallback logic directly regardless of whether a
		// real syft binary happens to be on this machine's PATH: it fakes
		// SyftAvailable's effect by making runSyft itself fail, which is the
		// code path taken whenever syft errors out even if installed.
		runSyft = func(ctx context.Context, target string) ([]byte, error) {
			return nil, errFakeSyftMissing
		}
		out, src, err := BuildPreferSyft(context.Background(), t.TempDir(), fixtureOptions(), fixtureComponents())
		if err != nil {
			t.Fatalf("BuildPreferSyft should fall back, not error: %v", err)
		}
		if src != SourceNative {
			t.Errorf("want fallback Source=native, got %s", src)
		}
		var probe Document
		if err := json.Unmarshal(out, &probe); err != nil {
			t.Fatalf("fallback output is not valid JSON: %v", err)
		}
		if probe.BOMFormat != BOMFormat {
			t.Errorf("fallback output bomFormat = %q, want %q", probe.BOMFormat, BOMFormat)
		}
	})
}

var errFakeSyftMissing = &fakeErr{"syft: exec: \"syft\": executable file not found in $PATH"}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }
