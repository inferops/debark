package bundle

import (
	"fmt"
	"sort"
	"strings"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
)

// readmeText is the real implementation behind the package-level ReadmeText.
// It is a pure function of its three inputs so it can be golden-tested
// without a filesystem, and called a second time by a caller that signs the
// manifest after Assemble returns (Assemble itself can only ever render the
// unsigned form, because nothing is signed yet when it runs).
func readmeText(m *manifest.Manifest, l *lock.Lock, signed bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	header := func(title string) {
		line("")
		line(title)
		line(strings.Repeat("-", len(title)))
	}

	line("debark offline bundle")
	line(strings.Repeat("=", len("debark offline bundle")))

	header("Target")
	if l != nil {
		t := l.Target
		line("  Distro:       %s", strings.TrimSpace(t.DistroID+" "+t.VersionID+" ("+t.Codename+")"))
		line("  Architecture: %s", t.Arch)
		if len(t.ForeignArchs) > 0 {
			line("  Foreign arch: %s", strings.Join(t.ForeignArchs, ", "))
		}
	} else if m != nil {
		t := m.Target
		line("  Distro:       %s", strings.TrimSpace(t.DistroID+" "+t.VersionID+" ("+t.Codename+")"))
		line("  Architecture: %s", t.Arch)
	}

	header("Contents")
	install := installList(l)
	line("  %d package(s) will be installed (exact versions, from lock.json):", len(install))
	for _, entry := range install {
		line("    %s", entry)
	}
	if m != nil {
		line("")
		line("  %d file(s) in the repository pool, %s total.", m.Repository.PackageCount, humanBytes(m.Repository.PoolBytes))
	}

	if l != nil && len(l.Packages) > 0 {
		header("Provenance")
		for _, entry := range provenanceSummary(l.Packages) {
			line("  %s", entry)
		}
	}

	header("Trust")
	if signed {
		line("  This bundle is signed. Verify the signature before installing:")
		line("    debark verify <bundle-path> --key <operator-public-key>")
	} else {
		line("  *** THIS BUNDLE IS NOT SIGNED. ***")
		line("  Its authenticity and integrity cannot be checked against a key.")
		line("  Only install it if you already trust the channel it arrived through.")
		line("  Ask whoever built it to sign it (debark build --sign <key>), or pass")
		line("  --allow-unsigned to debark verify / debark install to proceed")
		line("  anyway. This is not recommended.")
	}
	if l != nil {
		switch l.ClosedWorld.Result {
		case lock.ClosedWorldOK:
			line("  Closed-world check: passed - this bundle installs with no network access.")
		case lock.ClosedWorldFailed:
			line("  Closed-world check: FAILED - %s", nonEmpty(l.ClosedWorld.Detail, "see lock.json for detail"))
		}
	}

	header("Warnings")
	if l == nil || len(l.Warnings) == 0 {
		line("  none")
	} else {
		for _, w := range l.Warnings {
			if len(w.Packages) > 0 {
				line("  - %s: %s (%s)", w.Code, w.Message, strings.Join(w.Packages, ", "))
			} else {
				line("  - %s: %s", w.Code, w.Message)
			}
		}
	}

	if l != nil && len(l.Unresolved) > 0 {
		header("Unresolved")
		line("  This bundle is INCOMPLETE: the following inputs could not be fully resolved.")
		for _, u := range l.Unresolved {
			line("  - %s (%s)%s", u.Input, u.Kind, detailSuffix(u.Detail))
		}
	}

	if m != nil {
		header("Built")
		line("  %s by %s %s (%s)", m.CreatedAt, m.Tool.Name, m.Tool.Version, m.Tool.Edition)
		line("  Bundle ID: %s", m.BundleID)
	}

	header("Verify")
	line("    debark verify <bundle-path>")

	header("Install")
	line("    debark install <bundle-path>")
	line("    debark install <bundle-path> --status      # report only, changes nothing")
	line("    debark install <bundle-path> --dry-run     # full simulation, changes nothing")
	line("")
	line("  This bundle is also a plain apt repository. To use it directly, point")
	line("  a source at its repo/ directory:")
	line("    Types: deb")
	line("    URIs: file:///path/to/bundle/repo")
	line("    Suites: ./")
	line("    Trusted: yes")
	line("  then: apt-get update && apt-get install <package>=<version>")

	return b.String()
}

func installList(l *lock.Lock) []string {
	if l == nil {
		return nil
	}
	out := append([]string(nil), l.Install...)
	sort.Strings(out)
	return out
}

// provenanceSummary counts lock.Packages by PublisherVerification and
// renders one line per non-empty category, strongest claim first, so the
// order never depends on map iteration.
func provenanceSummary(pkgs []lock.Package) []string {
	counts := map[lock.PublisherVerification]int{}
	for _, p := range pkgs {
		counts[p.PublisherVerification]++
	}
	labels := []struct {
		kind  lock.PublisherVerification
		label string
	}{
		{lock.VerifiedAPTSigned, "verified against a signed apt archive"},
		{lock.VerifiedUserSignature, "verified against an operator-supplied vendor signature"},
		{lock.VerifiedUserDigest, "verified against an operator-supplied digest"},
		{lock.VerifiedURLUnverified, "downloaded with no publisher signature or digest (unverified - HTTPS is not provenance)"},
	}
	var out []string
	for _, l := range labels {
		if n := counts[l.kind]; n > 0 {
			out = append(out, fmt.Sprintf("%d package(s) %s.", n, l.label))
		}
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}
