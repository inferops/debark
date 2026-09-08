// Package e2e is the integration matrix: the fixture model and the
// `go test` driver that proves debark's central claim end to end — "a
// bundle that resolves online installs offline, every time, on every
// supported release" (the // correctness kill criterion).
//
// A fixture is a declarative JSON file under fixtures/: a target release and
// installed state, a request, and an expected outcome. Adding a row to the
// matrix means adding a file here, not writing Go. See README.md for the
// field reference and for how to debug a failing fixture.
//
// The whole suite is guarded behind DEBARK_E2E=1 (see e2e_test.go) so a
// plain `go test ./...` never starts a container — this file and
// fixture_test.go are the only things in this package that run without it.
//
// Fixtures are JSON, not YAML: go.mod is frozen (docs/dev/contract-brief.md
// rule 4, "No new dependencies... report it, do not change it") and carries
// no YAML library. To still let every fixture carry the human explanation
// the task requires ("each as its own fixture file with a comment
// explaining what would break if the behaviour regressed"), fixture files
// are JSON with `//` and `/* */` comments (stripped by stripJSONComments
// below) — real comments, not a policy of cramming prose into a JSON string.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/test/e2e/harness"
)

// FixtureSchemaVersion is the schema every file under fixtures/ declares.
const FixtureSchemaVersion = "debark.e2e.fixture/v1"

// Fixture is one fixture file's top-level shape: an envelope (this package)
// around a harness.Scenario (the harness package, so both `go test` and
// hack/matrix share one definition with no import cycle between them).
type Fixture struct {
	SchemaVersion string `json:"schema_version"`
	harness.Scenario
	// SourcePath is set by Load, not read from the file.
	SourcePath string `json:"-"`
}

// FixturesDir returns the directory this package's fixture files live in,
// relative to the package's own source location so it resolves correctly
// regardless of the caller's working directory.
func FixturesDir() (string, error) {
	root, err := harness.RepoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "test", "e2e", "fixtures"), nil
}

// LoadAll reads and validates every *.json file under dir, sorted by
// filename so a run is reproducible and a diff between two result files is
// meaningful.
func LoadAll(dir string) ([]Fixture, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read fixtures dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var out []Fixture
	seen := map[string]string{}
	for _, name := range names {
		path := filepath.Join(dir, name)
		f, err := Load(path)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[f.Name]; dup {
			return nil, fmt.Errorf("fixture name %q used by both %s and %s", f.Name, prev, path)
		}
		seen[f.Name] = path
		out = append(out, f)
	}
	return out, nil
}

// Load reads, decomments, parses and validates one fixture file.
func Load(path string) (Fixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, fmt.Errorf("read %s: %w", path, err)
	}
	stripped := stripJSONComments(raw)
	var f Fixture
	dec := json.NewDecoder(strings.NewReader(string(stripped)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Fixture{}, fmt.Errorf("parse %s: %w", path, err)
	}
	f.SourcePath = path
	if err := f.Validate(); err != nil {
		return Fixture{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Validate checks the fixture is internally consistent enough to run, so a
// mistake in a fixture file is reported once, clearly, at load time — never
// as a confusing failure three container-minutes into a matrix row.
func (f Fixture) Validate() error {
	var errs []string
	if f.SchemaVersion != FixtureSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version = %q, want %q", f.SchemaVersion, FixtureSchemaVersion))
	}
	if f.Name == "" {
		errs = append(errs, "name is required")
	}
	if strings.TrimSpace(f.Protects) == "" {
		errs = append(errs, "protects is required (which design section or failure mode this fixture guards)")
	}
	if f.Target.Distro != distro.Debian && f.Target.Distro != distro.Ubuntu {
		errs = append(errs, fmt.Sprintf("target.distro = %q, want %q or %q", f.Target.Distro, distro.Debian, distro.Ubuntu))
	}
	if f.Target.Version == "" {
		errs = append(errs, "target.version is required")
	} else if _, err := distro.Resolve(f.Target.Distro, f.Target.Version, ""); err != nil {
		errs = append(errs, err.Error())
	}
	if f.Target.Arch == "" {
		errs = append(errs, "target.arch is required")
	}
	if f.Expect.Build.ExitClass == "" {
		errs = append(errs, "expect.build.exit_class is required")
	} else if _, ok := validExitClasses[f.Expect.Build.ExitClass]; !ok {
		errs = append(errs, fmt.Sprintf("expect.build.exit_class = %q is not a known dferr class", f.Expect.Build.ExitClass))
	}
	for _, se := range []*harness.StageExpect{f.Expect.Verify, f.Expect.Install} {
		if se == nil {
			continue
		}
		if se.ExitClass != "skip" {
			if _, ok := validExitClasses[se.ExitClass]; !ok {
				errs = append(errs, fmt.Sprintf("exit_class %q is not a known dferr class or \"skip\"", se.ExitClass))
			}
		}
	}
	if f.Tamper != nil {
		switch f.Tamper.Kind {
		case harness.TamperModifiedDeb, harness.TamperAddedFile, harness.TamperRemovedFile,
			harness.TamperEditedManifest, harness.TamperSwappedSignature, harness.TamperWrongKey,
			harness.TamperSymlinkedFile:
		default:
			errs = append(errs, fmt.Sprintf("tamper.kind %q is not a known tamper kind", f.Tamper.Kind))
		}
		if !f.Request.Sign {
			errs = append(errs, "tamper fixtures must set request.sign = true (an unsigned bundle can't test a signature tamper)")
		}
		if f.Expect.Verify == nil || f.Expect.Verify.ExitClass == "success" || f.Expect.Verify.ExitClass == "skip" {
			errs = append(errs, "tamper fixtures must set expect.verify.exit_class to a failure class — the whole point is that verify refuses it")
		}
	}
	errs = append(errs, validateContainerPaths(f.Expect.ContainerPaths)...)
	errs = append(errs, validateBundleFiles(f.Expect.BundleFiles)...)
	if pb := f.Request.PriorBuild; pb != nil && len(pb.Packages) == 0 {
		errs = append(errs, "request.prior_build names no packages, so it would build the same empty request twice and establish nothing")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid fixture:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// validateContainerPaths catches the two mistakes that would make a
// container-path assertion pass for the wrong reason, at load time rather
// than three container-minutes into a matrix row.
//
// A misspelt role would leave the harness with no container to look in;
// that is reported as plumbing at run time, so it blocks rather than lies,
// but it should never get that far. An absolute path is required because a
// relative one is resolved against whatever working directory the probe
// happens to run in, which is not a thing a fixture can reason about.
func validateContainerPaths(checks []harness.ContainerPathCheck) []string {
	var errs []string
	for i, chk := range checks {
		where := fmt.Sprintf("expect.container_paths[%d]", i)
		switch chk.Container {
		case harness.RoleState, harness.RoleBuilder:
		default:
			errs = append(errs, fmt.Sprintf("%s.container = %q, want %q or %q",
				where, chk.Container, harness.RoleState, harness.RoleBuilder))
		}
		if !strings.HasPrefix(chk.Path, "/") {
			errs = append(errs, fmt.Sprintf("%s.path = %q must be absolute inside the container", where, chk.Path))
		}
	}
	return errs
}

// validateBundleFiles rejects a check that asserts nothing (no substrings)
// and a path that is not bundle-relative. A check with no Contains would
// assert only that the file exists, which every file in a bundle already
// does by way of the manifest — so it is far more likely to be an unfinished
// fixture than a deliberate one.
func validateBundleFiles(checks []harness.BundleFileCheck) []string {
	var errs []string
	for i, chk := range checks {
		where := fmt.Sprintf("expect.bundle_files[%d]", i)
		switch {
		case chk.Path == "":
			errs = append(errs, where+".path is required")
		case strings.HasPrefix(chk.Path, "/") || strings.Contains(chk.Path, "\\") || strings.Contains(chk.Path, ".."):
			errs = append(errs, fmt.Sprintf("%s.path = %q must be bundle-relative with forward slashes", where, chk.Path))
		}
		if len(chk.Contains) == 0 {
			errs = append(errs, where+".contains is required: a check that only asserts the file exists asserts nothing the manifest does not already")
		}
	}
	return errs
}

var validExitClasses = map[string]bool{
	"success": true, "usage": true, "environment": true, "incomplete": true,
	"verification": true, "resolution": true, "policy": true, "target-mismatch": true,
}

// stripJSONComments removes `//` line comments and `/* */` block comments
// from JSON source, leaving anything inside a string literal untouched (so
// a URL like "https://example.com" is not corrupted by its own "//"). This
// is the totality of what fixture files need beyond encoding/json — see the
// package doc for why a YAML library is not an option here.
func stripJSONComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inString := false
	escaped := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				if src[i] == '\n' {
					out = append(out, '\n')
				}
				i++
			}
			i++ // land on the '/' of '*/'; outer loop's i++ steps past it
		default:
			out = append(out, c)
		}
	}
	return out
}
