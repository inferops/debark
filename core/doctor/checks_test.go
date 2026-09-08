package doctor

import (
	"strings"
	"testing"

	"github.com/inferops/debark/core/lock"
)

func TestCheckSnapShim(t *testing.T) {
	cases := []struct {
		name string
		pkg  lock.Package
		deb  *debFile
		want bool
	}{
		{
			name: "tiny package calling snap install",
			pkg:  lock.Package{Name: "firefox", Version: "1:1-1", Size: 5000},
			deb: &debFile{
				Scripts: map[string]string{"postinst": "#!/bin/sh\nsnap install firefox\n"},
				Control: map[string]string{"Description": "transitional dummy package"},
			},
			want: true,
		},
		{
			name: "description says transitional package for a snap",
			pkg:  lock.Package{Name: "chromium-browser", Version: "1:1", Size: 999999999},
			deb: &debFile{
				Scripts: map[string]string{},
				Control: map[string]string{"Description": "transitional package\n This is a transitional package for the chromium snap."},
			},
			want: true,
		},
		{
			name: "large package calling snap install does not fire on the script signal alone",
			pkg:  lock.Package{Name: "bigtool", Version: "1.0", Size: 50 * 1024 * 1024},
			deb: &debFile{
				Scripts: map[string]string{"postinst": "#!/bin/sh\nsnap install bigtool\n"},
				Control: map[string]string{"Description": "a large tool"},
			},
			want: false,
		},
		{
			name: "ordinary package does not fire",
			pkg:  lock.Package{Name: "bash", Version: "5.0", Size: 1000},
			deb: &debFile{
				Scripts: map[string]string{"postinst": "#!/bin/sh\nupdate-alternatives --install /bin/sh sh /bin/bash 10\n"},
				Control: map[string]string{"Description": "a shell"},
			},
			want: false,
		},
		{
			name: "nil debFile does not fire",
			pkg:  lock.Package{Name: "bash", Version: "5.0", Size: 1000},
			deb:  nil,
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkSnapShim(c.pkg, c.deb)
			if (got != nil) != c.want {
				t.Fatalf("checkSnapShim() = %v, want finding=%v", got, c.want)
			}
			if got != nil {
				if got.Check != CheckSnapShim || got.Flag != lock.FlagSnapShim {
					t.Errorf("finding has wrong Check/Flag: %+v", got)
				}
			}
		})
	}
}

func TestCheckDKMSHeaders(t *testing.T) {
	installedWithHeaders := map[string]bool{
		"linux-image-6.1.0-13-amd64":   true,
		"linux-headers-6.1.0-13-amd64": true,
	}
	installedNoHeaders := map[string]bool{
		"linux-image-6.1.0-13-amd64": true,
	}

	t.Run("non-dkms package never fires", func(t *testing.T) {
		pkg := lock.Package{Name: "bash", Version: "5.0"}
		if got := checkDKMSHeaders(pkg, nil, installedNoHeaders, kernelHeaderSuffixes(installedNoHeaders)); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})

	t.Run("dkms package by name with matching headers installed: no finding", func(t *testing.T) {
		pkg := lock.Package{Name: "nvidia-dkms", Version: "1.0"}
		suf := kernelHeaderSuffixes(installedWithHeaders)
		if got := checkDKMSHeaders(pkg, nil, installedWithHeaders, suf); got != nil {
			t.Fatalf("want nil (headers present), got %+v", got)
		}
	})

	t.Run("dkms package by name with headers missing: finding names the missing package", func(t *testing.T) {
		pkg := lock.Package{Name: "nvidia-dkms", Version: "1.0"}
		suf := kernelHeaderSuffixes(installedNoHeaders)
		got := checkDKMSHeaders(pkg, nil, installedNoHeaders, suf)
		if got == nil {
			t.Fatal("want a finding, got nil")
		}
		if got.Check != CheckDKMSHeaders || got.Flag != lock.FlagDKMS {
			t.Errorf("finding has wrong Check/Flag: %+v", got)
		}
		if !strings.Contains(got.Evidence, "linux-headers-6.1.0-13-amd64") {
			t.Errorf("Evidence = %q, want it to name the missing header package", got.Evidence)
		}
	})

	t.Run("dkms package detected via data.tar secondary signal", func(t *testing.T) {
		pkg := lock.Package{Name: "acme-module", Version: "1.0"} // name has no "dkms"
		deb := &debFile{HasDKMSConf: true}
		suf := kernelHeaderSuffixes(installedNoHeaders)
		got := checkDKMSHeaders(pkg, deb, installedNoHeaders, suf)
		if got == nil {
			t.Fatal("want a finding via the data.tar dkms.conf signal, got nil")
		}
	})

	t.Run("no dpkg status available at all: soft finding, not silence", func(t *testing.T) {
		pkg := lock.Package{Name: "nvidia-dkms", Version: "1.0"}
		got := checkDKMSHeaders(pkg, nil, map[string]bool{}, nil)
		if got == nil {
			t.Fatal("want a finding noting no snapshot data was available")
		}
	})

	t.Run("dpkg status available but no linux-image found: soft finding", func(t *testing.T) {
		pkg := lock.Package{Name: "nvidia-dkms", Version: "1.0"}
		installed := map[string]bool{"bash": true}
		got := checkDKMSHeaders(pkg, nil, installed, kernelHeaderSuffixes(installed))
		if got == nil {
			t.Fatal("want a finding noting the kernel could not be determined")
		}
	})
}

func TestCheckRedistribution(t *testing.T) {
	cases := []struct {
		name     string
		pkg      lock.Package
		wantFlag string
		wantNil  bool
	}{
		{
			name:     "multiverse component",
			pkg:      lock.Package{Name: "vlc", Origin: lock.Origin{Component: "multiverse"}},
			wantFlag: lock.FlagMultiverse,
		},
		{
			name:     "restricted component",
			pkg:      lock.Package{Name: "nvidia-driver", Origin: lock.Origin{Component: "restricted"}},
			wantFlag: lock.FlagRestricted,
		},
		{
			name:    "main component is not flagged",
			pkg:     lock.Package{Name: "bash", Origin: lock.Origin{Component: "main"}},
			wantNil: true,
		},
		{
			name:     "external vendor deb is flagged user-supplied",
			pkg:      lock.Package{Name: "vendor-tool", Reason: lock.ReasonExternal, Origin: lock.Origin{URI: "https://vendor.example/tool.deb"}, SourcePackage: "vendor-tool"},
			wantFlag: lock.FlagUserSupplied,
		},
		{
			name:    "requested apt package from main is clean",
			pkg:     lock.Package{Name: "bash", Reason: lock.ReasonRequested, Origin: lock.Origin{Component: "main"}},
			wantNil: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkRedistribution(c.pkg)
			if c.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("want a finding, got nil")
			}
			if got.Flag != c.wantFlag {
				t.Errorf("Flag = %q, want %q", got.Flag, c.wantFlag)
			}
			if got.Check != CheckRedistribution {
				t.Errorf("Check = %q, want %q", got.Check, CheckRedistribution)
			}
		})
	}
}

func TestCheckUnverifiedURL(t *testing.T) {
	if got := checkUnverifiedURL(lock.Package{Name: "x", PublisherVerification: lock.VerifiedAPTSigned}); got != nil {
		t.Errorf("apt-signed should not fire, got %+v", got)
	}
	if got := checkUnverifiedURL(lock.Package{Name: "x", PublisherVerification: lock.VerifiedUserDigest}); got != nil {
		t.Errorf("user-digest should not fire, got %+v", got)
	}
	got := checkUnverifiedURL(lock.Package{Name: "x", PublisherVerification: lock.VerifiedURLUnverified, Origin: lock.Origin{URI: "https://vendor.example/x.deb"}})
	if got == nil {
		t.Fatal("url-unverified should fire")
	}
	if got.Check != CheckUnverifiedURL {
		t.Errorf("Check = %q", got.Check)
	}
}

func TestCheckHeldPackage(t *testing.T) {
	held := map[string]bool{"bash": true}
	if got := checkHeldPackage(lock.Package{Name: "vlc"}, held); got != nil {
		t.Errorf("non-held package should not fire, got %+v", got)
	}
	got := checkHeldPackage(lock.Package{Name: "bash", Version: "5.0"}, held)
	if got == nil {
		t.Fatal("held package should fire")
	}
	if got.Check != CheckHeldPackage {
		t.Errorf("Check = %q", got.Check)
	}
}

func TestParseDpkgStatus(t *testing.T) {
	status := "" +
		"Package: bash\n" +
		"Status: install ok installed\n" +
		"Version: 5.2.15-2\n" +
		"\n" +
		"Package: cowsay\n" +
		"Status: hold ok installed\n" +
		"Version: 3.7\n" +
		"\n" +
		"Package: half-removed-thing\n" +
		"Status: deinstall ok config-files\n" +
		"Version: 1.0\n" +
		"\n" +
		"Package: linux-image-6.1.0-13-amd64\n" +
		"Status: install ok installed\n" +
		"Version: 6.1.55-1\n" +
		"\n" +
		"Package: linux-headers-6.1.0-13-amd64\n" +
		"Status: install ok installed\n" +
		"Version: 6.1.55-1\n"

	installed, held := parseDpkgStatus([]byte(status))

	for _, want := range []string{"bash", "cowsay", "linux-image-6.1.0-13-amd64", "linux-headers-6.1.0-13-amd64"} {
		if !installed[want] {
			t.Errorf("installed[%q] should be true", want)
		}
	}
	if installed["half-removed-thing"] {
		t.Error("a deinstall/config-files package should not count as installed")
	}
	if !held["cowsay"] {
		t.Error("held[cowsay] should be true")
	}
	if held["bash"] {
		t.Error("held[bash] should be false")
	}

	suf := kernelHeaderSuffixes(installed)
	if len(suf) != 1 || suf[0] != "6.1.0-13-amd64" {
		t.Errorf("kernelHeaderSuffixes = %v, want [6.1.0-13-amd64]", suf)
	}
}

func TestParseDpkgStatus_Empty(t *testing.T) {
	installed, held := parseDpkgStatus(nil)
	if len(installed) != 0 || len(held) != 0 {
		t.Errorf("empty input should produce empty maps, got installed=%v held=%v", installed, held)
	}
}

func TestFlagsFor(t *testing.T) {
	findings := []Finding{
		{Check: CheckNetworkPostinst, Package: "a", Flag: lock.FlagNetworkPostinst},
		{Check: CheckRedistribution, Package: "a", Flag: lock.FlagMultiverse},
		{Check: CheckNetworkPostinst, Package: "a", Flag: lock.FlagNetworkPostinst}, // duplicate flag, same package
		{Check: CheckUnverifiedURL, Package: "a", Flag: ""},                         // no flag
		{Check: CheckRedistribution, Package: "b", Flag: lock.FlagUserSupplied},
	}
	got := FlagsFor(findings, "a")
	want := []string{lock.FlagMultiverse, lock.FlagNetworkPostinst} // sorted, deduped
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if got := FlagsFor(findings, "nonexistent"); got != nil {
		t.Errorf("unknown package should get nil flags, got %v", got)
	}
}
