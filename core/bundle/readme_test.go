package bundle

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
)

var updateGolden = flag.Bool("update", false, "update golden files")

func sampleManifest() *manifest.Manifest {
	return &manifest.Manifest{
		SchemaVersion:  manifest.SchemaVersion,
		BundleID:       "abcdef0123456789",
		CreatedAt:      "2026-09-03T00:00:00Z",
		FormatVersion:  manifest.CurrentFormatVersion,
		Tool:           manifest.Tool{Name: "debark", Version: "1.0.0", Edition: manifest.EditionCommunity},
		SnapshotDigest: "snap-digest",
		LockDigest:     "lock-digest",
		Repository: manifest.Repository{
			PackagesSHA256:   "pkgs-sha",
			PackagesGzSHA256: "pkgsgz-sha",
			ReleaseSHA256:    "release-sha",
			PackageCount:     4,
			PoolBytes:        458752, // 448 KiB
		},
		Target: manifest.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
	}
}

func sampleLock() *lock.Lock {
	return &lock.Lock{
		SchemaVersion:  lock.SchemaVersion,
		CreatedAt:      "2026-09-03T00:00:00Z",
		SnapshotDigest: "snap-digest",
		RequestDigest:  "req-digest",
		Target: lock.Target{
			DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64",
		},
		Resolver: lock.Resolver{
			Backend: lock.BackendContainer, Image: "docker.io/library/debian:bookworm-slim",
			APTVersion: "2.6.1", DpkgVersion: "1.21.22", PhasedUpdates: "never-include",
			InstallRecommends: true,
		},
		Packages: []lock.Package{
			{Name: "jq", Arch: "amd64", Version: "1.6-2.1+deb12u2", Filename: "pool/j/jq/jq_1.6-2.1+deb12u2_amd64.deb", Size: 63984, SHA256: "jqsha", Reason: lock.ReasonRequested, PublisherVerification: lock.VerifiedAPTSigned},
			{Name: "libjq1", Arch: "amd64", Version: "1.6-2.1+deb12u2", Filename: "pool/libj/libjq1/libjq1_1.6-2.1+deb12u2_amd64.deb", Size: 135656, SHA256: "libjqsha", Reason: "dependency-of:jq", PublisherVerification: lock.VerifiedAPTSigned},
			{Name: "libonig5", Arch: "amd64", Version: "6.9.8-1", Filename: "pool/libo/libonig5/libonig5_6.9.8-1_amd64.deb", Size: 187828, SHA256: "libonigsha", Reason: "dependency-of:libjq1", PublisherVerification: lock.VerifiedAPTSigned},
			{Name: "vendor-tool", Arch: "amd64", Version: "9.0", Filename: "pool/v/vendor-tool/vendor-tool_9.0_amd64.deb", Size: 71168, SHA256: "vendorsha", Reason: lock.ReasonExternal, PublisherVerification: lock.VerifiedURLUnverified},
		},
		Install: []string{"vendor-tool=9.0", "jq=1.6-2.1+deb12u2"},
		ClosedWorld: lock.ClosedWorld{
			Result: lock.ClosedWorldOK,
		},
		Warnings: []lock.Warning{
			{Code: "publisher.url-unverified", Message: "downloaded over HTTPS with no publisher signature", Packages: []string{"vendor-tool"}},
		},
		Stats: lock.Stats{Added: 4, Removed: 0, Unchanged: 0, Bytes: 458752, DownloadedBytes: 458752},
	}
}

func TestReadmeTextGolden(t *testing.T) {
	m := sampleManifest()
	l := sampleLock()

	cases := []struct {
		name   string
		signed bool
	}{
		{"signed", true},
		{"unsigned", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReadmeText(m, l, tc.signed)
			goldenPath := filepath.Join("testdata", "readme", tc.name+".golden")

			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatalf("mkdir golden dir: %v", err)
				}
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with -update to create it)", goldenPath, err)
			}
			if got != string(want) {
				t.Errorf("ReadmeText(signed=%v) does not match %s.\n--- got ---\n%s\n--- want ---\n%s", tc.signed, goldenPath, got, string(want))
			}
		})
	}
}

func TestReadmeTextUnsignedIsLoud(t *testing.T) {
	got := ReadmeText(sampleManifest(), sampleLock(), false)
	if !containsAll(got, "NOT SIGNED", "not recommended") {
		t.Errorf("unsigned README does not carry a loud warning:\n%s", got)
	}
}

func TestReadmeTextSignedHasNoUnsignedWarning(t *testing.T) {
	got := ReadmeText(sampleManifest(), sampleLock(), true)
	if containsAll(got, "NOT SIGNED") {
		t.Errorf("signed README still carries the unsigned warning:\n%s", got)
	}
	if !containsAll(got, "This bundle is signed") {
		t.Errorf("signed README does not say so:\n%s", got)
	}
}

func TestReadmeTextAnswersTheFourQuestions(t *testing.T) {
	got := ReadmeText(sampleManifest(), sampleLock(), true)
	// what target
	if !containsAll(got, "debian", "bookworm", "amd64") {
		t.Errorf("README does not answer 'what target':\n%s", got)
	}
	// what will be installed
	if !containsAll(got, "will be installed", "jq=1.6-2.1+deb12u2") {
		t.Errorf("README does not answer 'what will be installed':\n%s", got)
	}
	// how much
	if !containsAll(got, "KB total") && !containsAll(got, "MB total") {
		t.Errorf("README does not answer 'how much':\n%s", got)
	}
	// can this be trusted
	if !containsAll(got, "Verify", "debark verify") {
		t.Errorf("README does not answer 'can this be trusted':\n%s", got)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
