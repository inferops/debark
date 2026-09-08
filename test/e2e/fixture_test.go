package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests never touch Docker or a container and run under a plain
// `go test ./test/e2e/...` on any OS — no DEBARK_E2E needed. They exist so
// a broken fixture is caught the same way a broken Go file is: by CI, on
// every commit, not only on a nightly matrix run.

func TestStripJSONComments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "line comment stripped",
			in:   "{\n  // a comment\n  \"a\": 1\n}",
			want: `{"a":1}`,
		},
		{
			name: "block comment stripped",
			in:   "{\n  /* explanation\n     spanning lines */\n  \"a\": 1\n}",
			want: `{"a":1}`,
		},
		{
			name: "url with double slash inside a string survives",
			in:   `{"url": "https://example.com/x.deb"}`,
			want: `{"url":"https://example.com/x.deb"}`,
		},
		{
			name: "escaped quote inside a string does not end the string early",
			in:   `{"a": "she said \"//not a comment\""}`,
			want: `{"a":"she said \"//not a comment\""}`,
		},
		{
			name: "comment marker after a real string is still stripped",
			in:   "{\"a\": \"value\"} // trailing comment",
			want: `{"a":"value"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compactJSON(t, stripJSONComments([]byte(tc.in)))
			want := compactJSON(t, []byte(tc.want))
			if got != want {
				t.Errorf("stripJSONComments(%q) compacts to %q, want %q", tc.in, got, want)
			}
		})
	}
}

// compactJSON re-serialises through encoding/json so whitespace differences
// (including the newlines stripJSONComments intentionally preserves in
// place of a comment, to keep line numbers stable in parse errors) don't
// fail the comparison.
func compactJSON(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("invalid JSON after strip: %v\ninput: %s", err, b)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}

func TestLoadValidFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.json")
	writeFixtureFile(t, path, `{
		// this comment must not break parsing
		"schema_version": "debark.e2e.fixture/v1",
		"name": "sample",
		"protects": "unit test only",
		"target": {"distro": "debian", "version": "12", "arch": "amd64", "installed": {}},
		"request": {"packages": ["jq"]},
		"expect": {"build": {"exit_class": "success"}}
	}`)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Name != "sample" || f.Target.Distro != "debian" || f.Target.Version != "12" {
		t.Errorf("unexpected fixture: %+v", f)
	}
}

func TestLoadRejectsMissingProtects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	writeFixtureFile(t, path, `{
		"schema_version": "debark.e2e.fixture/v1",
		"name": "bad",
		"target": {"distro": "debian", "version": "12", "arch": "amd64", "installed": {}},
		"request": {},
		"expect": {"build": {"exit_class": "success"}}
	}`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "protects") {
		t.Fatalf("Load: want an error mentioning \"protects\", got %v", err)
	}
}

func TestLoadRejectsUnknownDistro(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	writeFixtureFile(t, path, `{
		"schema_version": "debark.e2e.fixture/v1",
		"name": "bad",
		"protects": "x",
		"target": {"distro": "arch-linux", "version": "1", "arch": "amd64", "installed": {}},
		"request": {},
		"expect": {"build": {"exit_class": "success"}}
	}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load: want an error for an unsupported distro id, got nil")
	}
}

func TestLoadRejectsTamperFixtureThatExpectsSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-tamper.json")
	writeFixtureFile(t, path, `{
		"schema_version": "debark.e2e.fixture/v1",
		"name": "bad-tamper",
		"protects": "x",
		"target": {"distro": "debian", "version": "12", "arch": "amd64", "installed": {}},
		"request": {"packages": ["jq"], "sign": true},
		"expect": {"build": {"exit_class": "success"}, "verify": {"exit_class": "success"}},
		"tamper": {"kind": "modified-deb"}
	}`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "verify.exit_class") {
		t.Fatalf("Load: want an error about verify.exit_class, got %v", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	writeFixtureFile(t, path, `{
		"schema_version": "debark.e2e.fixture/v1",
		"name": "bad",
		"protects": "x",
		"target": {"distro": "debian", "version": "12", "arch": "amd64", "installed": {}},
		"request": {},
		"expect": {"build": {"exit_class": "success"}},
		"totally_unknown_field": true
	}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load: want an error for an unknown top-level field (catches a typo'd field name), got nil")
	}
}

// TestRealFixturesLoad is the guard against fixture bit-rot: every checked-in
// fixture under fixtures/ must parse and validate on every commit, on every
// OS, with no container involved — a typo in a fixture file should fail CI
// immediately, not three container-minutes into a nightly matrix run.
func TestRealFixturesLoad(t *testing.T) {
	dir, err := FixturesDir()
	if err != nil {
		t.Fatalf("FixturesDir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("fixtures dir not present yet: %v", err)
	}
	fixtures, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll(%s): %v", dir, err)
	}
	if len(fixtures) == 0 {
		t.Fatalf("no fixtures found in %s", dir)
	}
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			if err := f.Validate(); err != nil {
				t.Errorf("%s: %v", f.SourcePath, err)
			}
		})
	}
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
