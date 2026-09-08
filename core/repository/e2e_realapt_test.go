package repository

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// requireLinuxAPT skips the test unless it is running on Linux with a real
// apt and DEBARK_E2E=1 (see docs/dev/contract-brief.md "Testing"). Run with:
//
//	DEBARK_E2E=1 bash hack/linux-test.sh ./core/repository/...
func requireLinuxAPT(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Getenv("DEBARK_E2E") == "" {
		t.Skip("needs Linux with apt; set DEBARK_E2E=1")
	}
	if _, err := exec.LookPath("apt-get"); err != nil {
		t.Skip("apt-get not on PATH")
	}
}

// runCmd runs a command and fails the test with its combined output on
// error; it also returns the output on success so callers can assert on it.
func runCmd(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestE2ERealAptAcceptsGeneratedRepository is the proof the design brief asks
// for: a real apt, pointed at a repo/ built entirely by this package's pure-Go
// Writer (no apt-ftparchive, no dpkg-scanpackages - see
// docs/dev/prototype-baseline.md, which found the Bash prototype cannot rely
// on either being present), successfully runs `apt-get update` and
// `apt-get -s install` against it.
//
// The .deb files are fetched with `apt-get download` inside the container
// this test runs in, never checked into the repository, per the contract brief.
func TestE2ERealAptAcceptsGeneratedRepository(t *testing.T) {
	requireLinuxAPT(t)
	ctx := context.Background()

	work := t.TempDir()
	debsDir := filepath.Join(work, "downloaded")
	repoDir := filepath.Join(work, "repo")
	if err := os.MkdirAll(debsDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", debsDir, err)
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", repoDir, err)
	}

	// Same closure as the recorded prototype baseline
	// (docs/dev/prototype-baseline.md, testdata/baseline/bundle-metadata/):
	// jq and tree, plus jq's own library dependencies, so the repository
	// carries more than one package and a real Depends relationship.
	pkgNames := []string{"jq", "libjq1", "libonig5", "tree"}

	runCmd(t, work, "apt-get", "update")
	runCmd(t, debsDir, "apt-get", append([]string{"download"}, pkgNames...)...)

	entries, err := os.ReadDir(debsDir)
	if err != nil {
		t.Fatalf("read %s: %v", debsDir, err)
	}
	var files []PoolFile
	var gotNames []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".deb") {
			continue
		}
		pkg := e.Name()
		if i := strings.Index(pkg, "_"); i > 0 {
			pkg = pkg[:i]
		}
		rel := PoolPath(pkg, e.Name())
		dest := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := copyFileForTest(filepath.Join(debsDir, e.Name()), dest); err != nil {
			t.Fatalf("copy %s into pool: %v", e.Name(), err)
		}
		files = append(files, PoolFile{Path: rel})
		gotNames = append(gotNames, pkg)
	}
	if len(files) != len(pkgNames) {
		t.Fatalf("downloaded %d .deb file(s) %v, want %d matching %v", len(files), gotNames, len(pkgNames), pkgNames)
	}

	release := ReleaseFields{
		Origin:        DefaultOrigin,
		Label:         DefaultLabel,
		Suite:         DefaultSuite,
		Codename:      "bookworm",
		Architectures: []string{"amd64"},
		Components:    []string{DefaultComponent},
		Description:   DefaultDescription,
		Date:          time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 UTC"),
	}
	res, err := NewWriter().Write(ctx, Input{Dir: repoDir, Files: files, Release: release, Compress: true})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.PackageCount != len(pkgNames) {
		t.Fatalf("PackageCount = %d, want %d", res.PackageCount, len(pkgNames))
	}
	t.Logf("wrote repository: %d packages, %d pool bytes", res.PackageCount, res.PoolBytes)

	// A structural cross-check against the recorded prototype baseline
	// (testdata/baseline/bundle-metadata/Packages): jq's stanza must still
	// start with Package: and end with the SHA256 field, in the same
	// relative order the real dpkg-deb-produced control data implies.
	packagesText, err := os.ReadFile(filepath.Join(repoDir, "Packages"))
	if err != nil {
		t.Fatalf("read Packages: %v", err)
	}
	assertBaselineFieldShape(t, string(packagesText))

	// The real proof: point a private apt view at repo/ with a file: source,
	// exactly as the target-side installer will, and run apt-get update then
	// a simulated install.
	sourcesDir := filepath.Join(work, "sources.list.d")
	if err := os.MkdirAll(sourcesDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", sourcesDir, err)
	}
	absRepoDir, err := filepath.Abs(repoDir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	// Flat repository form: Packages/Release live directly
	// under the given URI, so the source stanza names no component and ends
	// its Suite in "/" - the sources.list(5) "exact path" form.
	sourceStanza := fmt.Sprintf("Types: deb\nURIs: file://%s\nSuites: ./\nTrusted: yes\n", filepath.ToSlash(absRepoDir))
	if err := os.WriteFile(filepath.Join(sourcesDir, "debark-e2e.sources"), []byte(sourceStanza), 0o644); err != nil {
		t.Fatalf("write sources file: %v", err)
	}

	listsDir := filepath.Join(work, "lists")
	prefsDir := filepath.Join(work, "preferences.d")
	os.MkdirAll(listsDir, 0o755)
	os.MkdirAll(prefsDir, 0o755)

	aptOpts := []string{
		"-o", "Dir::Etc::sourcelist=/dev/null",
		"-o", "Dir::Etc::sourceparts=" + sourcesDir,
		"-o", "Dir::Etc::preferences=/dev/null",
		"-o", "Dir::Etc::preferencesparts=" + prefsDir,
		"-o", "Dir::State::lists=" + listsDir,
		"-o", "Dir::Cache::pkgcache=",
		"-o", "Dir::Cache::srcpkgcache=",
		"-o", "Acquire::Languages=none",
		"-o", "APT::Sandbox::User=root",
	}

	updateOut := runCmd(t, work, "apt-get", append(append([]string{}, aptOpts...), "update")...)
	t.Logf("apt-get update output:\n%s", updateOut)
	if strings.Contains(updateOut, "NO_PUBKEY") || strings.Contains(strings.ToLower(updateOut), "failed") {
		t.Fatalf("apt-get update reported a failure:\n%s", updateOut)
	}

	installArgs := append(append([]string{}, aptOpts...), "-s", "install")
	installArgs = append(installArgs, pkgNames...)
	installOut := runCmd(t, work, "apt-get", installArgs...)
	t.Logf("apt-get -s install output:\n%s", installOut)

	for _, pkg := range pkgNames {
		if !strings.Contains(installOut, "Inst "+pkg+" ") {
			t.Errorf("apt-get -s install output does not mention installing %s:\n%s", pkg, installOut)
		}
	}
	lower := strings.ToLower(installOut)
	for _, bad := range []string{"unable to locate", "unmet dependencies", "unable to correct problems"} {
		if strings.Contains(lower, bad) {
			t.Errorf("apt-get -s install reported %q:\n%s", bad, installOut)
		}
	}
}

// assertBaselineFieldShape checks the jq stanza in text has the same
// relative field shape as the recorded prototype baseline
// (testdata/baseline/bundle-metadata/Packages): Package first, Description
// present, and the appended file fields (Filename/Size/MD5sum/SHA1/SHA256)
// last, in that order.
func assertBaselineFieldShape(t *testing.T, text string) {
	t.Helper()
	stanzas := strings.Split(strings.TrimSuffix(text, "\n"), "\n\n")
	var jqStanza string
	for _, s := range stanzas {
		if strings.HasPrefix(s, "Package: jq\n") {
			jqStanza = s
			break
		}
	}
	if jqStanza == "" {
		t.Fatalf("no jq stanza found in generated Packages:\n%s", text)
	}
	var fields []string
	for _, line := range strings.Split(jqStanza, "\n") {
		if line == "" || strings.HasPrefix(line, " ") {
			continue // continuation line of a multi-line field
		}
		if i := strings.Index(line, ":"); i > 0 {
			fields = append(fields, line[:i])
		}
	}
	if len(fields) == 0 || fields[0] != "Package" {
		t.Errorf("jq stanza does not start with Package:; fields = %v", fields)
	}
	wantTail := []string{"Filename", "Size", "MD5sum", "SHA1", "SHA256"}
	if len(fields) < len(wantTail) {
		t.Fatalf("jq stanza has too few fields %v, want at least the tail %v", fields, wantTail)
	}
	gotTail := fields[len(fields)-len(wantTail):]
	for i, w := range wantTail {
		if gotTail[i] != w {
			t.Errorf("jq stanza field tail = %v, want %v", gotTail, wantTail)
			break
		}
	}
	hasDescription := false
	for _, f := range fields {
		if f == "Description" {
			hasDescription = true
		}
	}
	if !hasDescription {
		t.Errorf("jq stanza has no Description field; fields = %v", fields)
	}
}

func copyFileForTest(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
