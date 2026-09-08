package doctor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inferops/debark/core/lock"
)

// TestPoolPathRefusesEscapes covers every shape of hostile Package.Filename
// against poolPath directly. Filename comes out of the bundle's own
// lock.json, which nothing has verified when `debark doctor BUNDLE` reads
// it, so each of these is a string an attacker chooses freely.
//
// The Windows-specific shapes are refused on every platform on purpose: a
// hostile bundle is not obliged to know what will read it, and a rule that
// only fires on the platform the payload happens to name is not a rule.
func TestPoolPathRefusesEscapes(t *testing.T) {
	for _, tc := range []struct{ name, filename string }{
		{"parent traversal", "../../outside/secret.deb"},
		{"deep traversal", "../../../../../../../../etc/passwd"},
		{"traversal after a real prefix", "pool/./../../outside/secret.deb"},
		{"bare parent", ".."},
		{"absolute unix path", "/etc/passwd"},
		{"windows drive letter", "C:/Windows/win.ini"},
		{"unc path", `\\server\share\x.deb`},
		{"backslash separator", `pool\..\..\outside\secret.deb`},
		{"NUL byte", "pool/a\x00b.deb"},
		{"empty", ""},
		{"dot", "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := poolPath(filepath.FromSlash("/bundle"), tc.filename)
			if err == nil {
				t.Fatalf("poolPath accepted %q and returned %q", tc.filename, got)
			}
		})
	}
}

// TestPoolPathAcceptsRealPoolNames is the other side of the rule: the pool
// paths a real lock actually carries must still resolve, or the traversal
// defence has simply broken doctor. These are the shape core/repository
// writes — pool/<component>/<initial>/<source>/<file>.deb.
func TestPoolPathAcceptsRealPoolNames(t *testing.T) {
	root := filepath.FromSlash("/bundle")
	for _, filename := range []string{
		"pool/main/j/jq/jq_1.6-2.1_amd64.deb",
		"pool/main/libo/libonig5/libonig5_6.9.8-1_amd64.deb",
		"pool/main/l/linux-headers-6.1.0-13-common/linux-headers_6.1.0-13_all.deb",
		"jq_1.6-2.1_amd64.deb",
		"pool/main/./j/jq/jq.deb", // a "." component is tidied, not refused
	} {
		got, err := poolPath(root, filename)
		if err != nil {
			t.Fatalf("poolPath refused a real pool path %q: %v", filename, err)
		}
		want := filepath.Join(root, "repo")
		if !strings.HasPrefix(got, want+string(filepath.Separator)) {
			t.Errorf("poolPath(%q) = %q, which is not under %q", filename, got, want)
		}
	}
}

// TestRunDoesNotReadOutsideBundle is the end-to-end regression for the
// traversal, run against Run itself rather than the helper.
//
// Measured on the unfixed code: a lock naming "../../outside/secret.deb"
// made doctor open that file, parse it as a .deb, and report its
// maintainer-script text back in Finding.Evidence — an arbitrary-file read
// whose CONTENTS reach the report, not merely a stat. The payload here stays
// inside the test's own temp tree, so a regression is observable without the
// escape reaching anything real.
func TestRunDoesNotReadOutsideBundle(t *testing.T) {
	root := t.TempDir()
	bundleDir := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundleDir, "repo", "pool"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	// A perfectly valid .deb that simply is not in the bundle. Its postinst
	// carries a marker that can only appear in the report if doctor really
	// read a file outside the bundle root.
	const marker = "MARKER-READ-FROM-OUTSIDE-THE-BUNDLE"
	writeFixtureDeb(t, outside, "secret.deb", fixtureDeb{
		Scripts: map[string]string{"postinst": "#!/bin/sh\ncurl http://x.invalid/" + marker + "\n"},
	})

	l := &lock.Lock{Packages: []lock.Package{
		{Name: "victim", Version: "1.0", Filename: "../../outside/secret.deb"},
	}}
	report, err := Run(context.Background(), Input{BundleDir: bundleDir, Lock: l, ScanScripts: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, f := range report.Findings {
		if strings.Contains(f.Evidence, marker) {
			t.Fatalf("doctor read a .deb outside the bundle and reported its contents: %+v", f)
		}
	}
	// The escape must be reported, not passed over in silence. doctor's
	// silence on the network check is rendered as NoNetworkActionFound, so a
	// lock entry that steers doctor out of the bundle and thereby gets its
	// package skipped would be buying itself a clean bill of health; see
	// checkNetworkPostinst. Exactly one finding: the "could not be read" one.
	if len(report.Findings) != 1 {
		t.Fatalf("expected exactly one finding for an unresolvable pool path, got %d: %+v", len(report.Findings), report.Findings)
	}
	f := report.Findings[0]
	if f.Check != CheckNetworkPostinst || f.Severity != SeverityWarn {
		t.Errorf("finding = %+v, want a %s warn", f, CheckNetworkPostinst)
	}
	if f.Flag != "" {
		t.Errorf("an unreadable package must earn no lock flag, got %q", f.Flag)
	}
	if !strings.Contains(f.Evidence, "escapes the bundle") {
		t.Errorf("evidence should name the escape, got %q", f.Evidence)
	}
}

// TestOpenDebFileRefusesNonRegularFile protects the Lstat guard. A bundle can
// be a plain directory the operator points doctor at (bundle.Open accepts
// one), so nothing has filtered what lives under repo/ the way tar import
// does. A symlink there would otherwise redirect the read anywhere the
// process can reach — poolPath's lexical containment cannot see through one —
// and on Unix a FIFO would make os.Open block until a writer appeared, which
// is a hang rather than an error.
func TestOpenDebFileRefusesNonRegularFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("directory", func(t *testing.T) {
		sub := filepath.Join(dir, "notafile.deb")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := openDebFile(sub); err == nil {
			t.Fatal("openDebFile accepted a directory")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		target := writeFixtureDeb(t, dir, "real.deb", fixtureDeb{})
		link := filepath.Join(dir, "link.deb")
		if err := os.Symlink(target, link); err != nil {
			// Creating a symlink on Windows needs Developer Mode or
			// SeCreateSymbolicLinkPrivilege; skipping is honest here, the
			// directory case above still exercises the same guard.
			t.Skipf("cannot create a symlink on %s: %v", runtime.GOOS, err)
		}
		if _, err := openDebFile(link); err == nil {
			t.Fatal("openDebFile followed a symlink")
		}
	})
}
