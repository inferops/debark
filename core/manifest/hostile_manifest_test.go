package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

// This file is the regression suite for the manifest as a hostile document.
// Load reads debark.manifest.json BEFORE any signature has been checked -
// it has to, because the signature is over its bytes - so every property
// api/schema/manifest.v1.schema.json publishes has to be enforced here, on
// bytes that arrived on the same removable medium as everything else.

const goodDigest = "b44139fd4606ea55923459efe34e565018eb649742d9194bbcfa2aa9e510edd9"

// manifestJSON renders a syntactically valid manifest with the given files
// array text spliced in, so a test can write a files entry Go's own types
// could not hold (a negative size, a non-hex digest).
func manifestJSON(filesArray string) []byte {
	return []byte(`{
  "schema_version": "` + SchemaVersion + `",
  "bundle_id": "0123456789abcdef",
  "created_at": "2026-01-01T00:00:00Z",
  "format_version": 1,
  "tool": {"name":"debark","version":"test","edition":"community"},
  "snapshot_digest": "` + goodDigest + `",
  "lock_digest": "` + goodDigest + `",
  "repository": {"packages_sha256":"` + goodDigest + `","release_sha256":"` + goodDigest + `","package_count":1,"pool_bytes":8},
  "files": ` + filesArray + `,
  "target": {"distro_id":"debian","version_id":"12","codename":"bookworm","arch":"amd64"}
}
`)
}

func writeManifest(t *testing.T, dir string, raw []byte) {
	t.Helper()
	writeFile(t, filepath.Join(dir, FileName), raw)
}

// TestLoad_AcceptsAWellFormedDocument is the over-blocking guard: everything
// below must refuse only what the schema refuses.
func TestLoad_AcceptsAWellFormedDocument(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, manifestJSON(`[{"path":"repo/Packages","size":0,"sha256":"`+goodDigest+`"},
    {"path":"repo/pool/d/demo/demo_1.0_amd64.deb","size":12,"sha256":"`+goodDigest+`"}]`))
	m, canon, err := Load(dir)
	if err != nil {
		t.Fatalf("Load rejected a well-formed manifest: %v", err)
	}
	if len(m.Files) != 2 || len(canon) == 0 {
		t.Fatalf("Load returned %d files and %d canonical bytes", len(m.Files), len(canon))
	}
}

// TestLoad_RejectsCaseVariantSchemaKey is the regression test for the schema
// gate being case-insensitive: encoding/json falls back to case-insensitive
// field matching, so {"SCHEMA_VERSION": ...} used to load as schema v1 and
// its canonical bytes then carried a key no other implementation of this
// format would recognise, vouched for by debark's own signature.
func TestLoad_RejectsCaseVariantSchemaKey(t *testing.T) {
	for _, key := range []string{"SCHEMA_VERSION", "Schema_Version", "schemaversion", "sChEmA_vErSiOn"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			raw := strings.Replace(string(manifestJSON("[]")), `"schema_version"`, `"`+key+`"`, 1)
			writeManifest(t, dir, []byte(raw))
			_, _, err := Load(dir)
			if err == nil {
				t.Fatalf("Load accepted a manifest whose only version key was %q", key)
			}
			if !errors.Is(err, ErrUnknownSchema) {
				t.Errorf("error does not wrap ErrUnknownSchema, so verify cannot report it as schema-unknown: %v", err)
			}
		})
	}
}

// TestLoad_RejectsAnOversizeManifest is the regression test for the missing
// size bound. A 120.9 MiB manifest was measured at 2,682 MiB total allocation
// and 1,088 MiB peak heap inside Load - an out-of-memory kill of "debark
// verify" on the target, from a file the target has not yet decided to trust,
// and core/bundle/tar.go admits an imported entry of up to 8 GiB.
func TestLoad_RejectsAnOversizeManifest(t *testing.T) {
	dir := t.TempDir()

	// Padding inside a JSON string, so the document is genuinely parseable if
	// anything ever gets that far: the refusal must come from the size, not
	// from a syntax error.
	pad := strings.Repeat("a", maxManifestSize)
	raw := []byte(`{"schema_version":"` + SchemaVersion + `","files":[],"padding":"` + pad + `"}`)
	if len(raw) <= maxManifestSize {
		t.Fatalf("test fixture is only %d bytes, not over the %d-byte limit", len(raw), maxManifestSize)
	}
	writeManifest(t, dir, raw)

	_, _, err := Load(dir)
	if err == nil {
		t.Fatalf("Load accepted a %d-byte manifest (limit is %d)", len(raw), maxManifestSize)
	}
	if !dferr.Is(err, dferr.Verification) {
		t.Errorf("error class = %s, want verification (hostile input): %v", dferr.ClassOf(err), err)
	}
	if strings.Contains(err.Error(), "unexpected end of JSON") {
		t.Errorf("Load truncated the document instead of refusing it: %v", err)
	}
}

// TestLoad_AcceptsAManifestJustUnderTheLimit proves the bound is a bound and
// not an off-by-one that rejects a legitimate large bundle.
func TestLoad_AcceptsAManifestJustUnderTheLimit(t *testing.T) {
	dir := t.TempDir()
	base := `{"schema_version":"` + SchemaVersion + `","files":[],"padding":""}`
	pad := strings.Repeat("a", maxManifestSize-len(base))
	raw := []byte(`{"schema_version":"` + SchemaVersion + `","files":[],"padding":"` + pad + `"}`)
	if len(raw) != maxManifestSize {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(raw), maxManifestSize)
	}
	writeManifest(t, dir, raw)
	if _, _, err := Load(dir); err != nil {
		t.Fatalf("Load rejected a manifest of exactly the limit (%d bytes): %v", maxManifestSize, err)
	}
}

// TestLoad_RejectsAMissingFilesKey: the schema lists files as required, and
// absent is not the same as empty. An empty list is a claim the bundle holds
// nothing, which verify can check; an absent key never made the claim.
func TestLoad_RejectsAMissingFilesKey(t *testing.T) {
	dir := t.TempDir()
	raw := strings.Replace(string(manifestJSON("[]")), `"files": [],`, "", 1)
	if strings.Contains(raw, `"files"`) {
		t.Fatalf("test fixture still has a files key:\n%s", raw)
	}
	writeManifest(t, dir, []byte(raw))
	_, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a manifest with no files key")
	}
	if !dferr.Is(err, dferr.Verification) {
		t.Errorf("error class = %s, want verification: %v", dferr.ClassOf(err), err)
	}
}

// TestLoad_RejectsStructurallyInvalidFileEntries walks the entries
// api/schema/manifest.v1.schema.json forbids and Load used to accept.
func TestLoad_RejectsStructurallyInvalidFileEntries(t *testing.T) {
	entry := func(path, size, sha string) string {
		return `[{"path":` + path + `,"size":` + size + `,"sha256":` + sha + `}]`
	}
	q := func(s string) string { return `"` + s + `"` }

	cases := []struct{ name, files string }{
		{"parent traversal", entry(q("../x"), "0", q(goodDigest))},
		{"deep traversal", entry(q("../../etc/passwd"), "0", q(goodDigest))},
		{"absolute posix path", entry(q("/etc/passwd"), "0", q(goodDigest))},
		{"windows drive letter", entry(q("C:/x"), "0", q(goodDigest))},
		{"backslash separator", entry(q(`a\\b`), "0", q(goodDigest))},
		{"unc path", entry(q(`\\\\server\\share\\x`), "0", q(goodDigest))},
		{"empty path", entry(q(""), "0", q(goodDigest))},
		{"dot", entry(q("."), "0", q(goodDigest))},
		{"dot-slash prefix", entry(q("./repo/Packages"), "0", q(goodDigest))},
		{"double slash", entry(q("repo//Packages"), "0", q(goodDigest))},
		{"embedded dot component", entry(q("repo/./Packages"), "0", q(goodDigest))},
		{"embedded parent component", entry(q("repo/sub/../Packages"), "0", q(goodDigest))},
		{"trailing slash", entry(q("repo/"), "0", q(goodDigest))},
		{"nul byte in path", entry(`"repo/Packages\u0000"`, "0", q(goodDigest))},
		{"escape byte in path", entry(`"repo/\u001bPackages"`, "0", q(goodDigest))},
		{"negative size", entry(q("repo/Packages"), "-1", q(goodDigest))},
		{"empty sha256", entry(q("repo/Packages"), "0", q(""))},
		{"short sha256", entry(q("repo/Packages"), "0", q("aabb"))},
		{"uppercase sha256", entry(q("repo/Packages"), "0", q(strings.ToUpper(goodDigest)))},
		{"non-hex sha256", entry(q("repo/Packages"), "0", q(strings.Repeat("z", 64)))},
		{"whitespace-padded sha256", entry(q("repo/Packages"), "0", q(" "+goodDigest+" "))},
		{"doubled sha256", entry(q("repo/Packages"), "0", q(goodDigest+goodDigest))},
		{
			"duplicate path",
			`[{"path":"repo/Packages","size":0,"sha256":"` + goodDigest + `"},` +
				`{"path":"repo/Packages","size":1,"sha256":"` + strings.Repeat("0", 64) + `"}]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeManifest(t, dir, manifestJSON(tc.files))
			_, _, err := Load(dir)
			if err == nil {
				t.Fatalf("Load accepted files: %s", tc.files)
			}
			if !dferr.Is(err, dferr.Verification) {
				t.Errorf("error class = %s, want verification (hostile input): %v", dferr.ClassOf(err), err)
			}
		})
	}
}

// TestLoad_RejectsAMillionEntriesBySize records how the "a million entries"
// case is answered: not by a separate count limit, but by the byte bound, so
// there is only one number to reason about.
func TestLoad_RejectsAMillionEntriesBySize(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 1_000_000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"path":"repo/f%d","size":0,"sha256":"%s"}`, i, goodDigest)
	}
	b.WriteString("]")

	dir := t.TempDir()
	writeManifest(t, dir, manifestJSON(b.String()))
	_, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a manifest with a million entries")
	}
	if !dferr.Is(err, dferr.Verification) {
		t.Errorf("error class = %s, want verification: %v", dferr.ClassOf(err), err)
	}
}

// TestCheckFilePath is the direct unit test for the path rule, because the
// invalid-UTF-8 case cannot be produced by creating a real file on Windows
// (the OS layer replaces an invalid byte with U+FFFD before it reaches the
// filesystem) and so cannot be reached through Build there.
func TestCheckFilePath(t *testing.T) {
	bad := []struct{ name, path string }{
		{"empty", ""},
		{"dot", "."},
		{"dotdot", ".."},
		{"parent prefix", "../x"},
		{"absolute", "/x"},
		{"drive letter", "C:/x"},
		{"backslash", `a\b`},
		{"nul", "a\x00b"},
		{"escape", "a\x1bb"},
		{"del", "a\x7fb"},
		{"invalid utf-8", "a\xffb"},
		{"uncleaned", "a//b"},
		{"trailing slash", "a/"},
		{"dot component", "a/./b"},
		{"parent component", "a/b/../c"},
	}
	for _, tc := range bad {
		if err := checkFilePath(tc.path); err == nil {
			t.Errorf("checkFilePath(%q) [%s] = nil, want an error", tc.path, tc.name)
		}
	}

	good := []string{
		"lock.json",
		"repo/Packages",
		"repo/pool/libj/libjq1/libjq1_1.6-2.1+deb12u2_amd64.deb",
		"repo/pool/g/g++/g++_4%3a12.2.0-3_amd64.deb",
		"a b/c d.txt",
		"ünïcodé/naïve.txt",
		"..hidden/x", // leading dots are fine as long as the component is not ".."
		"a/..b/c",    // likewise mid-path
		"x.tar.gz",   //
		"README.debark.md",
	}
	for _, p := range good {
		if err := checkFilePath(p); err != nil {
			t.Errorf("checkFilePath(%q) = %v, want nil", p, err)
		}
	}
}

// TestBuild_RefusesAPathItCouldNotSignUnambiguously proves Build applies the
// same rule Load enforces, so this package's own output is always input this
// package accepts. Creating a file whose name is invalid UTF-8 is only
// possible on a filesystem that permits arbitrary bytes, so the assertion
// that has to hold everywhere is the round trip.
func TestBuild_RefusesAPathItCouldNotSignUnambiguously(t *testing.T) {
	dir := sampleBundleDir(t)
	m, err := Build(t.Context(), buildInputFor(dir))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, f := range m.Files {
		if err := checkFilePath(f.Path); err != nil {
			t.Fatalf("Build produced path %q that Load would refuse: %v", f.Path, err)
		}
	}
	if err := Save(dir, m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, err := Load(dir); err != nil {
		t.Fatalf("Load refused the manifest Build produced: %v", err)
	}
}

// TestLoad_MissingManifestStillReportsNotExist pins the error identity verify
// depends on: readBounded wraps the os.Open error rather than replacing it,
// so errors.Is(err, fs.ErrNotExist) still tells "no manifest in this bundle"
// apart from "the manifest is unreadable".
func TestLoad_MissingManifestStillReportsNotExist(t *testing.T) {
	dir := t.TempDir()
	_, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load on a missing manifest returned no error")
	}
	if !os.IsNotExist(errors.Unwrap(err)) && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load's error no longer identifies a missing file: %v", err)
	}
}
