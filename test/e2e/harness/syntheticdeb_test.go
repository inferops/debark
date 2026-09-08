package harness

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"pault.ag/go/debian/deb"
)

// These tests never touch Docker; they exist so a bug in the hand-built
// .deb/.tar/.ar bytes (syntheticdeb.go) is caught by `go test` on any OS,
// long before it would otherwise surface as a confusing apt failure deep
// inside a container three minutes into a matrix row. They load the built
// bytes with the exact library debark itself uses to parse a real .deb
// (pault.ag/go/debian/deb — see docs/dev/contract-brief.md's reference
// material), which is a stronger guarantee than merely round-tripping
// through this package's own writer.

func TestBuildSyntheticDebBytesLoadsWithRealDebParser(t *testing.T) {
	spec := SyntheticDebSpec{
		Name:     "acme-demo",
		Version:  "1.2.3",
		Arch:     "amd64",
		Depends:  "jq, tree",
		Provides: "acme-virtual-service",
		Bin:      true,
		Postinst: "echo synthetic postinst ran",
	}
	raw, err := BuildSyntheticDebBytes(spec)
	if err != nil {
		t.Fatalf("BuildSyntheticDebBytes: %v", err)
	}

	d, err := deb.Load(bytes.NewReader(raw), "acme-demo_1.2.3_amd64.deb")
	if err != nil {
		t.Fatalf("pault.ag/go/debian/deb.Load rejected the synthetic .deb: %v", err)
	}
	defer d.Close()

	if d.Control.Package != spec.Name {
		t.Errorf("Control.Package = %q, want %q", d.Control.Package, spec.Name)
	}
	if d.Control.Version.String() != spec.Version {
		t.Errorf("Control.Version = %q, want %q", d.Control.Version.String(), spec.Version)
	}
	if d.Control.Architecture.String() != spec.Arch {
		t.Errorf("Control.Architecture = %q, want %q", d.Control.Architecture.String(), spec.Arch)
	}
	if got := d.Control.Values["Provides"]; got != spec.Provides {
		t.Errorf("control Provides = %q, want %q", got, spec.Provides)
	}
	if len(d.Control.Depends.Relations) == 0 {
		t.Errorf("control Depends parsed empty from %q", spec.Depends)
	}

	// The data member must contain the bin script at the expected path, with
	// the executable bit set — dpkg refuses to configure a non-executable
	// maintainer-installed binary in a way this fixture would notice, so this
	// mirrors what actually matters at install time.
	foundBin := false
	for {
		hdr, err := d.Data.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading data.tar: %v", err)
		}
		if hdr.Name == "./usr/bin/acme-demo" {
			foundBin = true
			if hdr.Mode&0o111 == 0 {
				t.Errorf("usr/bin/acme-demo mode = %o, want an executable bit set", hdr.Mode)
			}
		}
	}
	if !foundBin {
		t.Error("data.tar does not contain ./usr/bin/acme-demo")
	}
}

func TestBuildSyntheticDebBytesRequiresArch(t *testing.T) {
	_, err := BuildSyntheticDebBytes(SyntheticDebSpec{Name: "x", Version: "1"})
	if err == nil {
		t.Fatal("BuildSyntheticDebBytes: want an error when Arch is empty, got nil")
	}
}

func TestWriteSyntheticRepoProducesParseableDebs(t *testing.T) {
	dir := t.TempDir()
	repo := SyntheticRepo{
		Name: "test-repo",
		Packages: []SyntheticRepoPackage{
			{Spec: SyntheticDebSpec{Name: "acme-a", Version: "1.0", Bin: true}},
			{Spec: SyntheticDebSpec{Name: "acme-b", Version: "2.0", Depends: "acme-a"}},
		},
		Release: &SyntheticRelease{Origin: "AcmeTest", Label: "Acme", Suite: "bundle", Codename: "test"},
	}
	stanza, err := WriteSyntheticRepo(dir, "http://host.docker.internal:12345", repo, "amd64")
	if err != nil {
		t.Fatalf("WriteSyntheticRepo: %v", err)
	}
	for _, want := range []string{"Types: deb", "URIs: http://host.docker.internal:12345/test-repo", "Suites: ./", "Trusted: yes"} {
		if !strings.Contains(stanza, want) {
			t.Errorf("sources stanza %q does not contain %q", stanza, want)
		}
	}

	packagesText, err := os.ReadFile(dir + "/test-repo/Packages")
	if err != nil {
		t.Fatalf("read Packages: %v", err)
	}
	for _, want := range []string{"Package: acme-a", "Package: acme-b", "Depends: acme-a", "Filename: acme-a_1-0_amd64.deb"} {
		if !strings.Contains(string(packagesText), want) {
			t.Errorf("Packages file does not contain %q:\n%s", want, packagesText)
		}
	}

	releaseText, err := os.ReadFile(dir + "/test-repo/Release")
	if err != nil {
		t.Fatalf("read Release: %v", err)
	}
	for _, want := range []string{"Origin: AcmeTest", "Suite: bundle", "Codename: test", "MD5Sum:", "SHA256:"} {
		if !strings.Contains(string(releaseText), want) {
			t.Errorf("Release file does not contain %q:\n%s", want, releaseText)
		}
	}

	// Every .deb the Packages file names must itself parse.
	rawA, err := os.ReadFile(dir + "/test-repo/acme-a_1-0_amd64.deb")
	if err != nil {
		t.Fatalf("read acme-a deb: %v", err)
	}
	if _, err := deb.Load(bytes.NewReader(rawA), "acme-a.deb"); err != nil {
		t.Errorf("acme-a.deb does not parse: %v", err)
	}
}

// TestSyntheticDebInstallsWithRealDpkg is the check that matters more than
// the pure-Go parser round trip above: does the *real* dpkg — not just a
// permissive Go library — accept a hand-built .deb, unpack its data member
// with the executable bit intact, and run its postinst? A hand-rolled ar/tar
// format bug that a lenient parser tolerates would otherwise surface for the
// first time three container-minutes into an unrelated fixture's failure,
// which is exactly the kind of confusing, non-specific failure the task
// asks this harness to avoid. Guarded behind DEBARK_E2E=1 like every other
// container-touching test in this tree (docs/dev/contract-brief.md).
func TestSyntheticDebInstallsWithRealDpkg(t *testing.T) {
	RequireE2E(t)
	ctx := context.Background()
	runID := NewRunID()
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// A discarded sweep error leaves containers behind on a shared
		// Docker host with nothing said about it — the one thing this
		// harness promises never to do.
		if _, err := Sweep(cctx, runID); err != nil {
			t.Logf("sweep run %s: %v", runID, err)
		}
	})

	ok, reason := DockerAvailable(ctx)
	if !ok {
		t.Skipf("docker unavailable: %s", reason)
	}

	c, err := StartContainer(ctx, ContainerOpts{
		Image:  "docker.io/library/debian:bookworm-slim",
		Name:   "dfe2e-selftest-" + runID,
		Labels: map[string]string{LabelMarker: "1", LabelRun: runID},
	})
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	defer c.Remove(ctx)

	spec := SyntheticDebSpec{
		Name: "acme-selftest", Version: "1.0", Arch: "amd64",
		Bin: true, Postinst: "echo SELFTEST_POSTINST_RAN >/tmp/postinst-marker",
	}
	raw, err := BuildSyntheticDebBytes(spec)
	if err != nil {
		t.Fatalf("BuildSyntheticDebBytes: %v", err)
	}
	if err := c.WriteFile(ctx, "/tmp/acme-selftest.deb", raw); err != nil {
		t.Fatalf("write deb into container: %v", err)
	}

	if res, err := c.Exec(ctx, ExecOpts{}, "dpkg-deb", "--info", "/tmp/acme-selftest.deb"); err != nil || res.ExitCode != 0 {
		t.Fatalf("dpkg-deb --info: err=%v res=%s", err, res)
	}
	res, err := c.Exec(ctx, ExecOpts{}, "dpkg", "--install", "/tmp/acme-selftest.deb")
	if err != nil {
		t.Fatalf("dpkg --install: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("dpkg --install exit %d:\n%s", res.ExitCode, res.Combined())
	}

	run, err := c.Exec(ctx, ExecOpts{}, "/usr/bin/acme-selftest")
	if err != nil {
		t.Fatalf("exec installed binary: %v", err)
	}
	if run.ExitCode != 0 || !strings.Contains(string(run.Stdout), "acme-selftest 1.0") {
		t.Errorf("installed binary: exit=%d stdout=%q", run.ExitCode, run.Stdout)
	}

	marker, err := c.Exec(ctx, ExecOpts{}, "cat", "/tmp/postinst-marker")
	if err != nil || marker.ExitCode != 0 || !strings.Contains(string(marker.Stdout), "SELFTEST_POSTINST_RAN") {
		t.Errorf("postinst did not run as expected: err=%v res=%s", err, marker)
	}

	status, err := c.Exec(ctx, ExecOpts{}, "dpkg-query", "-W", "-f=${Status}", "acme-selftest")
	if err != nil || status.ExitCode != 0 || !strings.Contains(string(status.Stdout), "install ok installed") {
		t.Errorf("dpkg-query after install: err=%v res=%s", err, status)
	}
}
